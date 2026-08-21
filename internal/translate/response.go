package translate

import (
	"encoding/json"
	"strings"
)

// thinkingSignature is a placeholder for the signature Anthropic attaches to
// thinking blocks. Upstream reasoning traces are unsigned, and the proxy drops
// thinking blocks on the way back up, so the value is never verified.
const thinkingSignature = "opencode-proxy"

type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string           `json:"finish_reason"`
		Message      openAIMessageOut `json:"message"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

type openAIMessageOut struct {
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []openAIToolCall `json:"tool_calls"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// AnthropicMessageOut is an Anthropic Messages API response.
type AnthropicMessageOut struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []map[string]any `json:"content"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// FromOpenAI converts a non-streaming chat-completions response into an
// Anthropic Messages response.
func FromOpenAI(body []byte, model string, includeThinking bool) ([]byte, error) {
	var response openAIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	out := AnthropicMessageOut{
		ID:      messageID(response.ID),
		Type:    "message",
		Role:    "assistant",
		Model:   model,
		Content: []map[string]any{},
		Usage: AnthropicUsage{
			InputTokens:          response.Usage.PromptTokens,
			OutputTokens:         response.Usage.CompletionTokens,
			CacheReadInputTokens: response.Usage.PromptTokensDetails.CachedTokens,
		},
	}
	if response.Model != "" {
		out.Model = response.Model
	}

	if len(response.Choices) > 0 {
		choice := response.Choices[0]
		if includeThinking && choice.Message.ReasoningContent != "" {
			out.Content = append(out.Content, map[string]any{
				"type":      "thinking",
				"thinking":  choice.Message.ReasoningContent,
				"signature": thinkingSignature,
			})
		}
		if choice.Message.Content != "" {
			out.Content = append(out.Content, map[string]any{"type": "text", "text": choice.Message.Content})
		}
		for _, call := range choice.Message.ToolCalls {
			out.Content = append(out.Content, toolUseBlock(call))
		}
		if reason := stopReason(choice.FinishReason); reason != "" {
			out.StopReason = &reason
		}
	}
	if len(out.Content) == 0 {
		out.Content = append(out.Content, map[string]any{"type": "text", "text": ""})
	}

	return json.Marshal(out)
}

func toolUseBlock(call openAIToolCall) map[string]any {
	input := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}
	return map[string]any{
		"type":  "tool_use",
		"id":    toolUseID(call.ID),
		"name":  call.Function.Name,
		"input": input,
	}
}

// stopReason maps an OpenAI finish_reason to its Anthropic equivalent.
func stopReason(finish string) string {
	switch finish {
	case "stop", "content_filter":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "":
		return ""
	default:
		return "end_turn"
	}
}

// messageID prefixes upstream IDs so clients that expect Anthropic's msg_
// namespace stay happy.
func messageID(id string) string {
	if id == "" {
		return "msg_opencode_proxy"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

func toolUseID(id string) string {
	if id == "" {
		return "toolu_opencode_proxy"
	}
	if strings.HasPrefix(id, "toolu_") {
		return id
	}
	return "toolu_" + id
}
