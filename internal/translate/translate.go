// Package translate converts between the Anthropic Messages API and the
// OpenAI chat-completions API.
//
// OpenCode Zen exposes both, but its /messages endpoint forwards Anthropic
// tool definitions verbatim to OpenAI-shaped upstreams, which reject them
// ("[1210] Invalid API parameter", "tools[0].function.name is invalid or
// missing"). Requests for those models are therefore translated here and sent
// to /chat/completions instead, and the response is translated back so the
// client still speaks pure Anthropic.
package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Request is the subset of the Anthropic Messages API the proxy understands.
// Anthropic-only fields with no OpenAI equivalent (context_management,
// output_config, cache_control, metadata) are dropped during translation.
type Request struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []AnthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	StopSequences []string           `json:"stop_sequences"`
	Stream        bool               `json:"stream"`
	Tools         []AnthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Thinking      json.RawMessage    `json:"thinking"`
}

type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text"`
	Thinking  string           `json:"thinking"`
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Input     json.RawMessage  `json:"input"`
	ToolUseID string           `json:"tool_use_id"`
	Content   json.RawMessage  `json:"content"`
	IsError   bool             `json:"is_error"`
	Source    *anthropicSource `json:"source"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type openAIRequest struct {
	Model              string          `json:"model"`
	Messages           []openAIMessage `json:"messages"`
	MaxTokens          int             `json:"max_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Stop               []string        `json:"stop,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              []openAITool    `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	StreamOptionsUsage *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ParseRequest decodes an Anthropic Messages request body.
func ParseRequest(body []byte) (*Request, error) {
	var request Request
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	return &request, nil
}

// WantsThinking reports whether the client asked for extended thinking, which
// decides whether upstream reasoning traces are surfaced as thinking blocks.
func (r *Request) WantsThinking() bool {
	if len(r.Thinking) == 0 {
		return false
	}
	var thinking struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(r.Thinking, &thinking); err != nil {
		return false
	}
	return thinking.Type != "" && thinking.Type != "disabled"
}

// ToOpenAI renders an Anthropic request as an OpenAI chat-completions request.
func ToOpenAI(request *Request, model string) ([]byte, error) {
	converted := openAIRequest{
		Model:       model,
		MaxTokens:   request.MaxTokens,
		Temperature: request.Temperature,
		TopP:        request.TopP,
		Stop:        request.StopSequences,
		Stream:      request.Stream,
	}
	if request.Stream {
		converted.StreamOptionsUsage = &streamOptions{IncludeUsage: true}
	}

	if system := systemText(request.System); system != "" {
		converted.Messages = append(converted.Messages, openAIMessage{Role: "system", Content: system})
	}
	for _, message := range request.Messages {
		translated, err := translateMessage(message)
		if err != nil {
			return nil, err
		}
		converted.Messages = append(converted.Messages, translated...)
	}
	if len(converted.Messages) == 0 {
		return nil, fmt.Errorf("request contains no messages")
	}

	for _, tool := range request.Tools {
		if tool.Name == "" {
			continue
		}
		converted.Tools = append(converted.Tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  sanitizeSchema(tool.InputSchema),
			},
		})
	}
	converted.ToolChoice, converted.ParallelToolCalls = translateToolChoice(request.ToolChoice)

	return json.Marshal(converted)
}

// translateMessage converts one Anthropic message into the one or more OpenAI
// messages it corresponds to. Anthropic carries tool results inside a user
// message; OpenAI needs a separate "tool" message per result.
func translateMessage(message AnthropicMessage) ([]openAIMessage, error) {
	role := message.Role
	if role == "" {
		role = "user"
	}

	if text, ok := plainString(message.Content); ok {
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		return []openAIMessage{{Role: role, Content: text}}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode %s content: %w", role, err)
	}

	var messages []openAIMessage
	var text strings.Builder
	var parts []any
	var toolCalls []openAIToolCall
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(block.Text)
		case "tool_use":
			call := openAIToolCall{Index: len(toolCalls), ID: block.ID, Type: "function"}
			call.Function.Name = block.Name
			call.Function.Arguments = compactJSON(block.Input)
			toolCalls = append(toolCalls, call)
		case "tool_result":
			messages = append(messages, openAIMessage{
				Role:       "tool",
				ToolCallID: block.ToolUseID,
				Content:    toolResultText(block),
			})
		case "image":
			if url := imageURL(block.Source); url != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "thinking", "redacted_thinking":
			// Reasoning traces are provider-specific and are not replayed upstream.
		}
	}

	if len(parts) > 0 {
		if text.Len() > 0 {
			parts = append([]any{map[string]any{"type": "text", "text": text.String()}}, parts...)
		}
		messages = append(messages, openAIMessage{Role: role, Content: parts})
		return messages, nil
	}
	if text.Len() > 0 || len(toolCalls) > 0 {
		converted := openAIMessage{Role: role, ToolCalls: toolCalls}
		if text.Len() > 0 {
			converted.Content = text.String()
		}
		messages = append(messages, converted)
	}
	return messages, nil
}

// systemText flattens an Anthropic system prompt, which may be a string or a
// list of text blocks, into a single string.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if text, ok := plainString(raw); ok {
		return text
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(block.Text)
	}
	return builder.String()
}

// toolResultText flattens a tool_result payload, which may be a string or a
// list of blocks, into the plain string OpenAI expects.
func toolResultText(block anthropicBlock) string {
	if len(block.Content) == 0 {
		return ""
	}
	if text, ok := plainString(block.Content); ok {
		return text
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(block.Content, &blocks); err != nil {
		return string(block.Content)
	}
	var builder strings.Builder
	for _, nested := range blocks {
		if nested.Type != "text" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(nested.Text)
	}
	return builder.String()
}

func imageURL(source *anthropicSource) string {
	if source == nil {
		return ""
	}
	switch source.Type {
	case "url":
		return source.URL
	case "base64":
		if source.Data == "" {
			return ""
		}
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		return "data:" + mediaType + ";base64," + source.Data
	}
	return ""
}

// translateToolChoice maps an Anthropic tool_choice to its OpenAI equivalent,
// returning the choice and whether parallel tool calls must be disabled.
func translateToolChoice(raw json.RawMessage) (any, *bool) {
	if len(raw) == 0 {
		return nil, nil
	}
	var choice struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, nil
	}
	var parallel *bool
	if choice.DisableParallelToolUse {
		disabled := false
		parallel = &disabled
	}
	switch choice.Type {
	case "auto":
		return "auto", parallel
	case "any":
		return "required", parallel
	case "none":
		return "none", parallel
	case "tool":
		if choice.Name == "" {
			return "auto", parallel
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, parallel
	}
	return nil, parallel
}

// sanitizeSchema drops JSON Schema keywords that OpenAI-shaped providers
// commonly reject while leaving the schema's meaning intact.
func sanitizeSchema(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		return schema
	}
	delete(fields, "$schema")
	if _, ok := fields["type"]; !ok {
		fields["type"] = json.RawMessage(`"object"`)
	}
	if _, ok := fields["properties"]; !ok {
		fields["properties"] = json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return schema
	}
	return encoded
}

// compactJSON renders a tool input as compact JSON, falling back to an empty
// object when the client sent nothing usable.
func compactJSON(raw json.RawMessage) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil || buffer.Len() == 0 {
		return "{}"
	}
	return buffer.String()
}

func plainString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, `"`) {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}
