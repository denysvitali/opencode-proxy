package translate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// This file converts chat-completions output back into the OpenAI Responses
// API shape: a non-streaming JSON converter and an SSE stream writer that
// emits the event sequence Codex CLI expects.

// responsesUsage is the usage block of a Responses API response.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

// responsesObject is the response envelope shared by non-streaming bodies
// and the "response" field of streaming events.
type responsesObject struct {
	ID                string           `json:"id"`
	Object            string           `json:"object"`
	CreatedAt         int64            `json:"created_at"`
	Status            string           `json:"status"`
	Model             string           `json:"model"`
	Output            []map[string]any `json:"output"`
	Error             any              `json:"error"`
	IncompleteDetails map[string]any   `json:"incomplete_details,omitempty"`
	Usage             *responsesUsage  `json:"usage,omitempty"`
}

func newResponsesObject(id, model string) *responsesObject {
	return &responsesObject{
		ID:        id,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "in_progress",
		Model:     model,
		Output:    []map[string]any{},
	}
}

// FromOpenAIToResponses converts a non-streaming chat-completions response
// into a Responses API response object.
func FromOpenAIToResponses(body []byte, model string, kinds *ResponsesToolKinds) ([]byte, error) {
	var upstream openAIResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, err
	}
	if upstream.Model != "" {
		model = upstream.Model
	}

	out := newResponsesObject(responseID(upstream.ID), model)
	out.Status = "completed"

	if len(upstream.Choices) > 0 {
		choice := upstream.Choices[0]
		appendResponsesOutput(&out.Output, choice.Message.ReasoningContent, choice.Message.Content, choice.Message.ToolCalls, kinds)
		switch choice.FinishReason {
		case "length":
			out.Status = "incomplete"
			out.IncompleteDetails = map[string]any{"reason": "max_output_tokens"}
		case "content_filter":
			out.Status = "incomplete"
			out.IncompleteDetails = map[string]any{"reason": "content_filter"}
		}
	}
	usage := responsesUsageFromOpenAI(upstream.Usage)
	if usage != nil {
		out.Usage = usage
	}
	return json.Marshal(out)
}

// appendResponsesOutput turns one assistant turn into Responses output items:
// reasoning first (if present), then the message, then tool calls.
func appendResponsesOutput(output *[]map[string]any, reasoning, content string, calls []openAIToolCall, kinds *ResponsesToolKinds) {
	if strings.TrimSpace(reasoning) != "" {
		*output = append(*output, reasoningItem(reasoning))
	}
	if content != "" {
		*output = append(*output, messageItem(content))
	}
	for _, call := range calls {
		*output = append(*output, toolCallItem(call, kinds))
	}
}

func reasoningItem(text string) map[string]any {
	return map[string]any{
		"type": "reasoning",
		"id":   reasoningID(""),
		"summary": []map[string]any{{
			"type": "summary_text",
			"text": text,
		}},
	}
}

func messageItem(text string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     messageID(""),
		"status": "completed",
		"role":   "assistant",
		"content": []map[string]any{{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
}

// toolCallItem converts one chat-completions tool call back into the
// Responses item type the client declared: local_shell_call for the shell
// tool, custom_tool_call for freeform tools, function_call otherwise.
func toolCallItem(call openAIToolCall, kinds *ResponsesToolKinds) map[string]any {
	callID := call.ID
	if callID == "" {
		callID = "call_opencode_proxy"
	}
	switch {
	case kinds != nil && kinds.ShellTool && call.Function.Name == shellToolName:
		action := decodeShellAction(call.Function.Arguments)
		return map[string]any{
			"type":    "local_shell_call",
			"id":      shellCallID(call.ID),
			"call_id": callID,
			"status":  "completed",
			"action":  action,
		}
	case kinds != nil && kinds.CustomTools[call.Function.Name]:
		input := unwrapFreeformInput(call.Function.Arguments)
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      customCallID(call.ID),
			"call_id": callID,
			"name":    call.Function.Name,
			"input":   input,
			"status":  "completed",
		}
	default:
		return map[string]any{
			"type":      "function_call",
			"id":        functionCallID(call.ID),
			"call_id":   callID,
			"name":      call.Function.Name,
			"arguments": call.Function.Arguments,
			"status":    "completed",
		}
	}
}

// decodeShellAction parses model-produced shell arguments into a
// local_shell exec action, tolerating missing or malformed fields.
func decodeShellAction(arguments string) map[string]any {
	action := map[string]any{"type": "exec", "command": []string{}}
	var parsed struct {
		Command          []string `json:"command"`
		TimeoutMS        *int64   `json:"timeout_ms"`
		WorkingDirectory string   `json:"working_directory"`
	}
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
		if len(parsed.Command) > 0 {
			action["command"] = parsed.Command
		}
		if parsed.TimeoutMS != nil {
			action["timeout_ms"] = *parsed.TimeoutMS
		}
		if parsed.WorkingDirectory != "" {
			action["working_directory"] = parsed.WorkingDirectory
		}
	}
	return action
}

// unwrapFreeformInput extracts the freeform payload from the synthetic
// {"input": ...} wrapper used to carry custom tools over function calling.
func unwrapFreeformInput(arguments string) string {
	var parsed struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil && parsed.Input != "" {
		return parsed.Input
	}
	return arguments
}

func responsesUsageFromOpenAI(usage openAIUsage) *responsesUsage {
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return nil
	}
	out := &responsesUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.PromptTokens + usage.CompletionTokens,
	}
	out.InputTokensDetails.CachedTokens = usage.PromptTokensDetails.CachedTokens
	return out
}

// --- ID namespaces -------------------------------------------------------

func responseID(id string) string {
	if id == "" {
		id = fmt.Sprint(time.Now().UnixNano())
	}
	return "resp_" + id
}

func reasoningID(id string) string {
	if id == "" {
		id = fmt.Sprint(time.Now().UnixNano())
	}
	return "rs_" + id
}

func functionCallID(id string) string {
	if id == "" {
		id = fmt.Sprint(time.Now().UnixNano())
	}
	return "fc_" + id
}

func shellCallID(id string) string {
	if id == "" {
		id = fmt.Sprint(time.Now().UnixNano())
	}
	return "lsh_" + id
}

func customCallID(id string) string {
	if id == "" {
		id = fmt.Sprint(time.Now().UnixNano())
	}
	return "ctc_" + id
}

// --- Streaming ------------------------------------------------------------

const (
	responsesKeepaliveInterval = 15 * time.Second
)

// ResponsesStreamWriter consumes an OpenAI chat-completions SSE stream and
// re-emits it as Responses API events.
//
// Text deltas are forwarded live. Tool-call arguments are buffered until the
// call completes because the final item type depends on the tool name (a
// "shell" call becomes a local_shell_call only once we know the request
// declared that tool), and shell payloads are small enough that buffering
// costs nothing perceptible.
type ResponsesStreamWriter struct {
	writer io.Writer
	flush  func()
	model  string
	kinds  *ResponsesToolKinds

	sequence   int
	responseID string
	started    bool
	finished   bool

	outputIndex int

	textOpen bool
	textID   string
	text     strings.Builder

	reasoningOpen bool
	reasoningID   string
	reasoning     strings.Builder

	calls     map[int]*streamedToolCall
	callOrder []int

	output     []map[string]any
	usage      *responsesUsage
	stopReason string
}

type streamedToolCall struct {
	itemID string
	callID string
	name   string
	args   strings.Builder
}

// NewResponsesStreamWriter creates a writer converting chat-completions SSE
// into Responses API events. kinds may be nil when the request declared no
// special tools.
func NewResponsesStreamWriter(writer io.Writer, flush func(), model string, kinds *ResponsesToolKinds) *ResponsesStreamWriter {
	return &ResponsesStreamWriter{
		writer: writer,
		flush:  flush,
		model:  model,
		kinds:  kinds,
		calls:  map[int]*streamedToolCall{},
	}
}

// Consume reads the upstream stream to completion, emitting events as it
// goes. It always terminates the response envelope exactly once.
func (s *ResponsesStreamWriter) Consume(body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			s.Fail(chunk.Error)
			return nil
		}
		s.consumeChunk(chunk)
	}
	err := scanner.Err()
	s.Complete()
	return err
}

func (s *ResponsesStreamWriter) consumeChunk(chunk openAIChunk) {
	s.ensureStarted(chunk.Model)
	if chunk.Usage != nil {
		s.usage = responsesUsageFromOpenAI(*chunk.Usage)
	}
	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]

	if delta := choice.Delta.ReasoningContent; delta != "" {
		s.openReasoning()
		s.reasoning.WriteString(delta)
		s.emit("response.reasoning_summary_text.delta", map[string]any{
			"type":          "response.reasoning_summary_text.delta",
			"item_id":       s.reasoningID,
			"output_index":  s.outputIndex - 1,
			"summary_index": 0,
			"delta":         delta,
		})
	}
	if delta := choice.Delta.Content; delta != "" {
		s.openText()
		s.text.WriteString(delta)
		s.emit("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       s.textID,
			"output_index":  s.outputIndex - 1,
			"content_index": 0,
			"delta":         delta,
		})
	}
	for _, call := range choice.Delta.ToolCalls {
		s.consumeToolCallDelta(call)
	}
	if choice.FinishReason != "" {
		s.stopReason = choice.FinishReason
	}
}

func (s *ResponsesStreamWriter) consumeToolCallDelta(call openAIToolCall) {
	state, ok := s.calls[call.Index]
	if !ok {
		state = &streamedToolCall{
			itemID: functionCallID(call.ID),
			callID: call.ID,
			name:   call.Function.Name,
		}
		if state.callID == "" {
			state.callID = "call_" + state.itemID
		}
		s.calls[call.Index] = state
		s.callOrder = append(s.callOrder, call.Index)
	}
	if call.Function.Name != "" {
		state.name = call.Function.Name
	}
	if call.Function.Arguments != "" {
		state.args.WriteString(call.Function.Arguments)
	}
}

// ensureStarted emits the opening events of the response envelope.
func (s *ResponsesStreamWriter) ensureStarted(model string) {
	if s.started {
		return
	}
	s.started = true
	if model != "" {
		s.model = model
	}
	object := newResponsesObject(responseID(""), s.model)
	s.responseID = object.ID
	s.emit("response.created", map[string]any{"type": "response.created", "response": object})
	s.emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": object})
}

func (s *ResponsesStreamWriter) openText() {
	if s.textOpen {
		return
	}
	s.closeReasoning()
	s.textOpen = true
	s.textID = messageID("")
	index := s.outputIndex
	s.outputIndex++
	s.emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": index,
		"item": map[string]any{
			"type": "message", "id": s.textID, "status": "in_progress", "role": "assistant", "content": []any{},
		},
	})
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	s.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": s.textID, "output_index": index, "content_index": 0, "part": part,
	})
}

func (s *ResponsesStreamWriter) closeText() {
	if !s.textOpen {
		return
	}
	s.textOpen = false
	index := s.outputIndex - 1
	text := s.text.String()
	s.emit("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": s.textID, "output_index": index, "content_index": 0, "text": text,
	})
	s.emit("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": s.textID, "output_index": index, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	item := messageItem(text)
	s.output = append(s.output, item)
	s.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": index,
		"item": item,
	})
}

func (s *ResponsesStreamWriter) openReasoning() {
	if s.reasoningOpen {
		return
	}
	s.reasoningOpen = true
	s.reasoningID = reasoningID("")
	index := s.outputIndex
	s.outputIndex++
	s.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": index,
		"item": map[string]any{"type": "reasoning", "id": s.reasoningID, "summary": []any{}},
	})
	s.emit("response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "item_id": s.reasoningID,
		"output_index": index, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (s *ResponsesStreamWriter) closeReasoning() {
	if !s.reasoningOpen {
		return
	}
	s.reasoningOpen = false
	index := s.outputIndex - 1
	text := s.reasoning.String()
	s.emit("response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "item_id": s.reasoningID,
		"output_index": index, "summary_index": 0, "text": text,
	})
	s.emit("response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "item_id": s.reasoningID,
		"output_index": index, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	})
	item := reasoningItem(text)
	s.output = append(s.output, item)
	s.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": index,
		"item": item,
	})
}

// closeCalls flushes every buffered tool call as completed items.
func (s *ResponsesStreamWriter) closeCalls() {
	for _, index := range s.callOrder {
		state := s.calls[index]
		call := openAIToolCall{
			ID:    state.callID,
			Type:  "function",
			Index: index,
		}
		call.Function.Name = state.name
		call.Function.Arguments = state.args.String()
		if strings.TrimSpace(call.Function.Arguments) == "" {
			call.Function.Arguments = "{}"
		}
		item := toolCallItem(call, s.kinds)
		s.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": s.outputIndex,
			"item": inProgressToolItem(item),
		})
		if item["type"] == "function_call" {
			s.emit("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": item["id"],
				"output_index": s.outputIndex, "delta": call.Function.Arguments,
			})
			s.emit("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": item["id"],
				"output_index": s.outputIndex, "arguments": call.Function.Arguments,
			})
		}
		s.output = append(s.output, item)
		s.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": s.outputIndex,
			"item": item,
		})
		s.outputIndex++
	}
	s.calls = map[int]*streamedToolCall{}
	s.callOrder = nil
}

// inProgressToolItem renders a finished tool-call item as its in-progress
// counterpart for the output_item.added event.
func inProgressToolItem(item map[string]any) map[string]any {
	pending := make(map[string]any, len(item)+1)
	for key, value := range item {
		pending[key] = value
	}
	switch pending["type"] {
	case "function_call":
		pending["arguments"] = ""
	case "local_shell_call":
		pending["action"] = map[string]any{"type": "exec", "command": []string{}}
	case "custom_tool_call":
		pending["input"] = ""
	}
	pending["status"] = "in_progress"
	return pending
}

// Complete closes every open item and emits the terminal event. Safe to
// call more than once; the first call wins.
func (s *ResponsesStreamWriter) Complete() {
	if s.finished {
		return
	}
	s.finished = true
	if !s.started {
		// Nothing ever arrived from upstream; still hand the client a
		// well-formed empty response so it can end its turn cleanly.
		s.ensureStarted("")
	}
	s.closeReasoning()
	s.closeText()
	s.closeCalls()

	status := "completed"
	details := map[string]any(nil)
	switch s.stopReason {
	case "length":
		status, details = "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		status, details = "incomplete", map[string]any{"reason": "content_filter"}
	}
	object := newResponsesObject(s.responseID, s.model)
	object.Status = status
	object.IncompleteDetails = details
	object.Usage = s.usage
	object.Output = s.output
	s.emit("response.completed", map[string]any{
		"type": "response.completed", "response": object,
	})
}

// Fail aborts the stream with an error payload, emitting response.failed
// (or error if nothing was sent yet).
func (s *ResponsesStreamWriter) Fail(raw json.RawMessage) {
	if s.finished {
		return
	}
	s.finished = true
	var failure struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	_ = json.Unmarshal(raw, &failure)
	payload := map[string]any{"message": failure.Message, "type": failure.Type, "code": failure.Code}
	if failure.Message == "" {
		payload["message"] = "upstream request failed"
	}
	if !s.started {
		s.started = true
		object := newResponsesObject(responseID(""), s.model)
		s.responseID = object.ID
		object.Status = "failed"
		object.Error = payload
		s.emit("error", map[string]any{"type": "error", "code": failure.Code, "message": payload["message"], "param": nil})
		return
	}
	s.closeReasoning()
	s.closeText()
	s.closeCalls()
	object := newResponsesObject(s.responseID, s.model)
	object.Status = "failed"
	object.Error = payload
	s.emit("response.failed", map[string]any{"type": "response.failed", "response": object})
}

func (s *ResponsesStreamWriter) nextSeq() int {
	s.sequence++
	return s.sequence
}

func (s *ResponsesStreamWriter) emit(event string, payload map[string]any) {
	payload["sequence_number"] = s.nextSeq()
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
