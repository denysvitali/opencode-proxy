package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file translates the OpenAI Responses API — the wire protocol Codex
// CLI speaks — into chat-completions requests Zen understands, and back.
//
// The mapping is not always mechanical:
//
//   - Responses tools are flat ({type,name,parameters}); chat-completions
//     nests them under "function".
//   - Codex's local_shell tool becomes a plain "shell" function upstream and
//     function calls named "shell" become local_shell_call items on the way
//     back, so Codex keeps executing them natively.
//   - Freeform custom tools (apply_patch) get a synthetic {"input": string}
//     schema; arguments are unwrapped back to custom_tool_call items.
//   - reasoning items are dropped: encrypted reasoning is provider-specific
//     and replaying it elsewhere is meaningless.

// ResponsesRequest is the subset of the OpenAI Responses API the proxy
// understands. Unknown fields are ignored during parsing.
type ResponsesRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions"`
	Input             json.RawMessage `json:"input"`
	Tools             []ResponsesTool `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	MaxOutputTokens   int             `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
}

// ResponsesTool is a flat Responses API tool definition.
type ResponsesTool struct {
	Type        string          `json:"type"` // "function", "local_shell", "custom", or a server tool like "web_search"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// responsesItem is one entry of a Responses request input array.
type responsesItem struct {
	Type string `json:"type"`
	Role string `json:"role"`

	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`

	Content   json.RawMessage `json:"content"`
	Input     json.RawMessage `json:"input"` // freeform payload of custom_tool_call
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Summary   json.RawMessage `json:"summary"`
}

// ParseResponsesRequest decodes an OpenAI Responses API request body.
func ParseResponsesRequest(body []byte) (*ResponsesRequest, error) {
	var request ResponsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	return &request, nil
}

// ResponsesToolKinds records which special Responses tools were declared so
// model output can be converted back into the item types Codex expects.
type ResponsesToolKinds struct {
	CustomTools map[string]bool
	ShellTool   bool
}

const shellToolName = "shell"

var shellToolParameters = json.RawMessage(`{
	"type":"object",
	"properties":{
		"command":{"type":"array","items":{"type":"string"},"description":"The command to run with its arguments."},
		"timeout_ms":{"type":"integer","description":"Optional timeout in milliseconds."},
		"working_directory":{"type":"string","description":"Optional working directory."}
	},
	"required":["command"]
}`)

var freeformToolParameters = json.RawMessage(`{
	"type":"object",
	"properties":{"input":{"type":"string","description":"The raw freeform payload."}},
	"required":["input"]
}`)

// ToChatCompletions renders a Responses request as an OpenAI
// chat-completions request for the given (already resolved) model. The
// returned tool kinds must be kept around for translating the response back.
func (r *ResponsesRequest) ToChatCompletions(model string) ([]byte, *ResponsesToolKinds, error) {
	kinds := &ResponsesToolKinds{}
	converted := openAIRequest{
		Model:       model,
		MaxTokens:   r.MaxOutputTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stream:      r.Stream,
	}
	if r.Stream {
		converted.StreamOptionsUsage = &streamOptions{IncludeUsage: true}
	}
	if converted.MaxTokens <= 0 {
		// Some providers reject an absent limit with every token spent;
		// others reject zero. A sane default avoids both.
		converted.MaxTokens = 1 << 15
	}

	if instructions := strings.TrimSpace(r.Instructions); instructions != "" {
		converted.Messages = append(converted.Messages, openAIMessage{Role: "system", Content: instructions})
	}

	messages, err := r.translateInput()
	if err != nil {
		return nil, nil, err
	}
	converted.Messages = append(converted.Messages, messages...)
	if len(converted.Messages) == 0 {
		return nil, nil, fmt.Errorf("request contains no messages")
	}

	for _, tool := range r.Tools {
		switch tool.Type {
		case "function":
			if tool.Name == "" {
				continue
			}
			converted.Tools = append(converted.Tools, openAITool{
				Type: "function",
				Function: openAIFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  sanitizeSchema(tool.Parameters),
				},
			})
		case "custom":
			if tool.Name == "" {
				continue
			}
			// Freeform tools carry no JSON schema upstream; wrap the payload
			// in a single string field so function-calling providers can use
			// them, and unwrap on the way back.
			converted.Tools = append(converted.Tools, openAITool{
				Type: "function",
				Function: openAIFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  freeformToolParameters,
				},
			})
			if kinds.CustomTools == nil {
				kinds.CustomTools = map[string]bool{}
			}
			kinds.CustomTools[tool.Name] = true
		case "local_shell":
			converted.Tools = append(converted.Tools, openAITool{
				Type: "function",
				Function: openAIFunction{
					Name:        shellToolName,
					Description: "Runs a shell command and returns its output.",
					Parameters:  shellToolParameters,
				},
			})
			kinds.ShellTool = true
		default:
			// Server-side tools (web_search, computer_use, ...) have no
			// upstream equivalent through Zen; drop them rather than fail.
		}
	}

	converted.ToolChoice = translateResponsesToolChoice(r.ToolChoice)
	converted.ParallelToolCalls = r.ParallelToolCalls

	encoded, err := json.Marshal(converted)
	if err != nil {
		return nil, nil, err
	}
	return encoded, kinds, nil
}

// translateInput flattens the Responses input (a bare string or an item
// array) into chat-completions messages.
func (r *ResponsesRequest) translateInput() ([]openAIMessage, error) {
	if len(r.Input) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(r.Input, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		return []openAIMessage{{Role: "user", Content: text}}, nil
	}

	var items []responsesItem
	if err := json.Unmarshal(r.Input, &items); err != nil {
		return nil, fmt.Errorf("decode input: %w", err)
	}

	var messages []openAIMessage
	for _, item := range items {
		translated, err := translateResponseItem(item)
		if err != nil {
			return nil, err
		}
		messages = append(messages, translated...)
	}
	return messages, nil
}

// translateResponseItem converts one input item into zero or more
// chat-completions messages.
func translateResponseItem(item responsesItem) ([]openAIMessage, error) {
	switch item.Type {
	case "", "message":
		role := item.Role
		if role == "" {
			role = "user"
		}
		content, parts, err := responsesItemContent(item.Content)
		if err != nil {
			return nil, err
		}
		switch role {
		case "assistant", "system", "developer":
			upstreamRole := role
			if role == "developer" {
				upstreamRole = "system"
			}
			if content == "" {
				return nil, nil
			}
			return []openAIMessage{{Role: upstreamRole, Content: content}}, nil
		default: // user
			if len(parts) > 0 {
				if content != "" {
					parts = append([]any{map[string]any{"type": "text", "text": content}}, parts...)
				}
				return []openAIMessage{{Role: "user", Content: parts}}, nil
			}
			if content == "" {
				return nil, nil
			}
			return []openAIMessage{{Role: "user", Content: content}}, nil
		}

	case "function_call":
		return toolCallsMessage(responsesCall{
			CallID:    item.CallID,
			Name:      item.Name,
			Arguments: item.Arguments,
		}), nil

	case "custom_tool_call":
		// The freeform payload rides in "input"; wrap it as
		// {"input": ...} to match the synthetic function schema.
		payload := item.Arguments
		if text, ok := plainString(item.Input); ok && payload == "" {
			payload = text
		}
		wrapped, err := json.Marshal(map[string]string{"input": payload})
		if err != nil {
			return nil, fmt.Errorf("encode custom tool input: %w", err)
		}
		return toolCallsMessage(responsesCall{
			CallID:    item.CallID,
			Name:      item.Name,
			Arguments: string(wrapped),
		}), nil

	case "local_shell_call":
		action := item.Action
		if len(action) == 0 {
			action = json.RawMessage(`{}`)
		}
		return toolCallsMessage(responsesCall{
			CallID:    item.CallID,
			Name:      shellToolName,
			Arguments: compactJSON(action),
		}), nil

	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		output := flattenResponsesOutput(item.Output)
		callID := item.CallID
		if callID == "" {
			callID = item.ID
		}
		if callID == "" {
			return nil, fmt.Errorf("%s without call_id", item.Type)
		}
		return []openAIMessage{{Role: "tool", ToolCallID: callID, Content: output}}, nil

	case "reasoning":
		// Encrypted reasoning belongs to whichever provider produced it and
		// cannot be replayed anywhere else.
		return nil, nil
	case "item_reference":
		// Server-side conversation state; this proxy is stateless by design
		// (Codex sends store=false and full history).
		return nil, nil
	case "unknown":
		return nil, nil
	default:
		// Forward-compatible: ignore item types added after this proxy was
		// written instead of failing the whole request.
		return nil, nil
	}
}

// responsesItemContent decodes a message item's content, which may be a bare
// string or an array of typed parts. It returns the concatenated text plus
// any image parts.
func responsesItemContent(raw json.RawMessage) (string, []any, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil, nil
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, fmt.Errorf("decode content: %w", err)
	}
	var builder strings.Builder
	var images []any
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text", "summary_text":
			if part.Text == "" {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteString("\n\n")
			}
			builder.WriteString(part.Text)
		case "input_image", "output_image":
			if url := part.ImageURL; url != "" {
				images = append(images, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		}
	}
	return builder.String(), images, nil
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

// responsesCall is the common shape of function_call, custom_tool_call and
// local_shell_call items once normalized.
type responsesCall struct {
	CallID    string
	Name      string
	Arguments string
}

// toolCallsMessage renders a call item as an assistant message carrying a
// single tool call.
func toolCallsMessage(call responsesCall) []openAIMessage {
	id := call.CallID
	if id == "" {
		id = "call_opencode_proxy"
	}
	arguments := call.Arguments
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	toolCall := openAIToolCall{Index: 0, ID: id, Type: "function"}
	toolCall.Function.Name = call.Name
	toolCall.Function.Arguments = arguments
	message := openAIMessage{Role: "assistant"}
	message.ToolCalls = []openAIToolCall{toolCall}
	return []openAIMessage{message}
}

// flattenResponsesOutput normalizes a call-output payload into plain text.
// Codex wraps shell results as JSON like {"output":"...","metadata":...};
// keep that verbatim so the model sees exit codes and metadata too.
func flattenResponsesOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if text, ok := plainString(raw); ok {
		return text
	}
	return compactJSON(raw)
}

// translateResponsesToolChoice maps a Responses tool_choice ("auto",
// "none", "required", {"type":"function","name":...}) onto its
// chat-completions equivalent.
func translateResponsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		switch name {
		case "auto", "none", "required":
			return name
		}
		return "auto"
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil
	}
	if choice.Type == "function" && choice.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	}
	if choice.Type == "allowed_tools" {
		return "auto"
	}
	return "auto"
}
