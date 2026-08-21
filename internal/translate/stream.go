package translate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxStreamLine = 8 << 20

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

	started       bool
	blockOpen     bool
	blockKind     string
	blockIndex    int
	openToolIndex int
	stopReason    string
	usage         AnthropicUsage
}

func NewStreamWriter(writer io.Writer, flush func(), model string, includeThinking bool) *StreamWriter {
	return &StreamWriter{writer: writer, flush: flush, model: model, includeThinking: includeThinking, openToolIndex: -1}
}

// Consume reads the upstream SSE stream to completion, emitting Anthropic
// events as it goes.
func (s *StreamWriter) Consume(body io.Reader) error {
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
			s.emit("error", map[string]any{"type": "error", "error": json.RawMessage(chunk.Error)})
			return nil
		}
		s.consumeChunk(chunk)
	}
	s.Finish()
	return scanner.Err()
}

func (s *StreamWriter) consumeChunk(chunk openAIChunk) {
	s.ensureStarted(chunk.ID, chunk.Model)
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
		s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": ""}, -1)
		s.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": choice.Delta.ReasoningContent},
		})
	}
	if choice.Delta.Content != "" {
		s.openBlock("text", map[string]any{"type": "text", "text": ""}, -1)
		s.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": choice.Delta.Content},
		})
	}
	for _, call := range choice.Delta.ToolCalls {
		s.consumeToolCall(call)
	}
	if choice.FinishReason != "" {
		s.stopReason = stopReason(choice.FinishReason)
	}
}

// consumeToolCall opens a tool_use block the first time a call index is seen
// and streams its arguments as input_json_delta fragments.
func (s *StreamWriter) consumeToolCall(call openAIToolCall) {
	if s.blockKind != "tool_use" || s.openToolIndex != call.Index {
		s.openBlock("tool_use", map[string]any{
			"type":  "tool_use",
			"id":    toolUseID(call.ID),
			"name":  call.Function.Name,
			"input": map[string]any{},
		}, call.Index)
	}
	if call.Function.Arguments == "" {
		return
	}
	s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments},
	})
}

func (s *StreamWriter) ensureStarted(id, model string) {
	if s.started {
		return
	}
	s.started = true
	if model != "" {
		s.model = model
	}
	s.emit("message_start", map[string]any{
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
func (s *StreamWriter) openBlock(kind string, block map[string]any, toolIndex int) {
	if s.blockOpen && s.blockKind == kind && (kind != "tool_use" || s.openToolIndex == toolIndex) {
		return
	}
	s.closeBlock()
	s.blockOpen = true
	s.blockKind = kind
	s.openToolIndex = toolIndex
	s.emit("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.blockIndex,
		"content_block": block,
	})
}

func (s *StreamWriter) closeBlock() {
	if !s.blockOpen {
		return
	}
	if s.blockKind == "thinking" {
		s.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": thinkingSignature},
		})
	}
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIndex})
	s.blockOpen = false
	s.blockKind = ""
	s.openToolIndex = -1
	s.blockIndex++
}

// Finish closes the message. It is safe to call more than once.
func (s *StreamWriter) Finish() {
	if !s.started {
		return
	}
	s.closeBlock()
	reason := s.stopReason
	if reason == "" {
		reason = "end_turn"
	}
	s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": s.usage,
	})
	s.emit("message_stop", map[string]any{"type": "message_stop"})
	s.started = false
}

func (s *StreamWriter) emit(event string, payload any) {
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
