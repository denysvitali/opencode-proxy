package translate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const maxStreamLine = 8 << 20

// keepaliveInterval spaces out ping events on translated streams. Long
// thinking pauses can leave the upstream silent for minutes; without
// traffic, idle-timeout middleboxes kill the connection mid-turn.
var keepaliveInterval = 15 * time.Second

type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *openAIUsage    `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// StreamWriter turns an OpenAI chat-completions SSE stream into the Anthropic
// Messages event stream clients expect.
type StreamWriter struct {
	writer          io.Writer
	flush           func()
	model           string
	includeThinking bool
	keepalive       time.Duration

	mu            sync.Mutex
	started       bool
	closed        bool
	blockOpen     bool
	blockKind     string
	blockIndex    int
	openToolIndex int
	stopReason    string
	usage         AnthropicUsage
}

func NewStreamWriter(writer io.Writer, flush func(), model string, includeThinking bool) *StreamWriter {
	return &StreamWriter{writer: writer, flush: flush, model: model, includeThinking: includeThinking, keepalive: keepaliveInterval, openToolIndex: -1}
}

// Consume reads the upstream SSE stream to completion, emitting Anthropic
// events as it goes. Ping events are emitted during upstream silences so
// intermediaries do not reap the connection mid-turn.
func (s *StreamWriter) Consume(body io.Reader) error {
	stopKeepalive := s.startKeepalive()
	defer stopKeepalive()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			s.Finish()
			return nil
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				s.emit("error", map[string]any{"type": "error", "error": json.RawMessage(chunk.Error)})
				s.Finish()
			}
			return nil
		}
		s.consumeChunk(chunk)
	}
	if err := scanner.Err(); err != nil {
		// The upstream broke mid-stream; leave the message unfinished so the
		// caller can end it with an explicit error instead of a completion
		// that hides the truncation.
		return err
	}
	if s.started {
		// The upstream hung up without [DONE]. Ending the message here would
		// dress a truncated answer up as a completed turn, which is exactly
		// the silent-stop bug this guards against.
		return errors.New("upstream stream ended before the completion marker")
	}
	return errors.New("upstream stream ended without emitting any events")
}

// startKeepalive emits ping events until the returned stop func runs. The
// stop func waits for the goroutine to exit so no ping is ever in flight
// after Consume returns.
func (s *StreamWriter) startKeepalive() func() {
	interval := s.keepalive
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.mu.Lock()
				finished := s.closed
				s.mu.Unlock()
				if !finished {
					s.emit("ping", map[string]any{"type": "ping"})
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

func (s *StreamWriter) consumeChunk(chunk openAIChunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureStartedLocked(chunk.ID, chunk.Model)
	if chunk.Usage != nil {
		s.usage.InputTokens = chunk.Usage.PromptTokens
		s.usage.OutputTokens = chunk.Usage.CompletionTokens
		s.usage.CacheReadInputTokens = chunk.Usage.PromptTokensDetails.CachedTokens
	}
	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]

	if choice.Delta.ReasoningContent != "" && s.includeThinking {
		s.openBlockLocked("thinking", map[string]any{"type": "thinking", "thinking": ""}, -1)
		s.writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": choice.Delta.ReasoningContent},
		})
	}
	if choice.Delta.Content != "" {
		s.openBlockLocked("text", map[string]any{"type": "text", "text": ""}, -1)
		s.writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": choice.Delta.Content},
		})
	}
	for _, call := range choice.Delta.ToolCalls {
		s.consumeToolCallLocked(call)
	}
	if choice.FinishReason != "" {
		s.stopReason = stopReason(choice.FinishReason)
	}
}

// consumeToolCall opens a tool_use block the first time a call index is seen
// and streams its arguments as input_json_delta fragments.
func (s *StreamWriter) consumeToolCallLocked(call openAIToolCall) {
	if s.blockKind != "tool_use" || s.openToolIndex != call.Index {
		s.openBlockLocked("tool_use", map[string]any{
			"type":  "tool_use",
			"id":    toolUseID(call.ID),
			"name":  call.Function.Name,
			"input": map[string]any{},
		}, call.Index)
	}
	if call.Function.Arguments == "" {
		return
	}
	s.writeEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments},
	})
}

func (s *StreamWriter) ensureStartedLocked(id, model string) {
	if s.started {
		return
	}
	s.started = true
	if model != "" {
		s.model = model
	}
	s.writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": AnthropicMessageOut{
			ID:      messageID(id),
			Type:    "message",
			Role:    "assistant",
			Model:   s.model,
			Content: []map[string]any{},
		},
	})
}

// openBlock closes the current content block, if any, and starts a new one.
func (s *StreamWriter) openBlockLocked(kind string, block map[string]any, toolIndex int) {
	if s.blockOpen && s.blockKind == kind && (kind != "tool_use" || s.openToolIndex == toolIndex) {
		return
	}
	s.closeBlockLocked()
	s.blockOpen = true
	s.blockKind = kind
	s.openToolIndex = toolIndex
	s.writeEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.blockIndex,
		"content_block": block,
	})
}

func (s *StreamWriter) closeBlockLocked() {
	if !s.blockOpen {
		return
	}
	if s.blockKind == "thinking" {
		s.writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": thinkingSignature},
		})
	}
	s.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIndex})
	s.blockOpen = false
	s.blockKind = ""
	s.openToolIndex = -1
	s.blockIndex++
}

// Finish closes the message. It is safe to call more than once. When no
// chunk ever arrived it still emits a complete empty message so the client
// ends its turn instead of waiting on a silent stream forever.
func (s *StreamWriter) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if !s.started {
		s.ensureStartedLocked("", "")
		s.openBlockLocked("text", map[string]any{"type": "text", "text": ""}, -1)
		s.closeBlockLocked()
	}
	s.closeBlockLocked()
	reason := s.stopReason
	if reason == "" {
		reason = "end_turn"
	}
	s.writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": s.usage,
	})
	s.writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

// Fail ends the stream with an explicit Anthropic error event instead of a
// normal completion. The client sees a real failure it can retry rather than
// a truncated answer that looks finished.
func (s *StreamWriter) Fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if !s.started {
		return
	}
	s.closeBlockLocked()
	s.writeEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
}

// emit serializes one SSE frame; callers that already hold the mutex use
// writeEvent directly.
func (s *StreamWriter) emit(event string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEvent(event, payload)
}

func (s *StreamWriter) writeEvent(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(s.writer, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return
	}
	if s.flush != nil {
		s.flush()
	}
}
