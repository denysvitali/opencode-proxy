package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustParseResponses(t *testing.T, body string) *ResponsesRequest {
	t.Helper()
	request, err := ParseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return request
}

func TestResponsesRequestToChatCompletions(t *testing.T) {
	request := mustParseResponses(t, `{
		"model":"x-preview-f-free",
		"instructions":"You are Codex.",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":[\"ls\"]}"},
			{"type":"function_call_output","call_id":"call_1","output":"{\"output\":\"a.txt\"}"},
			{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAA"}
		],
		"tools":[
			{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"command":{"type":"array"}}},"strict":false},
			{"type":"web_search"}
		],
		"tool_choice":"auto",
		"parallel_tool_calls":false,
		"stream":true,
		"max_output_tokens":512
	}`)

	data, kinds, err := request.ToChatCompletions("x-preview-f-free")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if kinds.ShellTool || len(kinds.CustomTools) != 0 {
		t.Fatalf("unexpected tool kinds: %+v", kinds)
	}

	var converted openAIRequest
	if err := json.Unmarshal(data, &converted); err != nil {
		t.Fatalf("decode converted: %v", err)
	}
	if converted.Model != "x-preview-f-free" {
		t.Fatalf("model = %q", converted.Model)
	}
	if !converted.Stream || converted.StreamOptionsUsage == nil || !converted.StreamOptionsUsage.IncludeUsage {
		t.Fatalf("stream options missing: %+v", converted.StreamOptionsUsage)
	}
	if converted.MaxTokens != 512 {
		t.Fatalf("max_tokens = %d", converted.MaxTokens)
	}
	if *converted.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls should be false")
	}

	// messages: system(instructions), user, assistant(tool call), tool result.
	// The reasoning item must be dropped.
	var roles []string
	for _, message := range converted.Messages {
		roles = append(roles, message.Role)
	}
	want := []string{"system", "user", "assistant", "tool"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	if converted.Messages[0].Content != "You are Codex." {
		t.Fatalf("instructions not mapped to system: %v", converted.Messages[0].Content)
	}
	calls := converted.Messages[2].ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "shell" {
		t.Fatalf("function_call not translated: %+v", calls)
	}
	if converted.Messages[3].ToolCallID != "call_1" || converted.Messages[3].Content != `{"output":"a.txt"}` {
		t.Fatalf("function_call_output not translated: %+v", converted.Messages[3])
	}

	// tools: flat Responses shape becomes nested chat-completions shape;
	// server-side tools are dropped.
	if len(converted.Tools) != 1 || converted.Tools[0].Function.Name != "shell" {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	if converted.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %v", converted.ToolChoice)
	}
}

func TestResponsesLocalShellAndCustomToolsRoundTrip(t *testing.T) {
	request := mustParseResponses(t, `{
		"model":"m",
		"input":[{"type":"message","role":"user","content":"patch it"}],
		"tools":[
			{"type":"local_shell"},
			{"type":"custom","name":"apply_patch","description":"apply a patch"}
		]
	}`)
	data, kinds, err := request.ToChatCompletions("m")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !kinds.ShellTool || !kinds.CustomTools["apply_patch"] {
		t.Fatalf("kinds not recorded: %+v", kinds)
	}

	var converted openAIRequest
	if err := json.Unmarshal(data, &converted); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range converted.Tools {
		names[tool.Function.Name] = true
	}
	if !names["shell"] || !names["apply_patch"] {
		t.Fatalf("expected shell and apply_patch functions, got %v", names)

	}
	patch := converted.Tools[1].Function.Parameters
	if !strings.Contains(string(patch), `"input"`) {
		t.Fatalf("freeform tool schema missing input wrapper: %s", patch)
	}

	// A model calling shell/apply_patch comes back as native item types.
	out := []map[string]any{}
	appendResponsesOutput(&out, "", "", []openAIToolCall{
		{ID: "call_a", Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "shell", Arguments: `{"command":["ls","-la"],"timeout_ms":1000}`}},
		{ID: "call_b", Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "apply_patch", Arguments: `{"input":"*** Begin Patch\n+hi\n*** End Patch"}`}},
	}, kinds)

	if out[0]["type"] != "local_shell_call" {
		t.Fatalf("shell call type = %v", out[0]["type"])
	}
	action := out[0]["action"].(map[string]any)
	if action["type"] != "exec" {
		t.Fatalf("action = %v", action)
	}
	command := action["command"].([]string)
	if len(command) != 2 || command[0] != "ls" {
		t.Fatalf("command = %v", command)
	}
	if out[1]["type"] != "custom_tool_call" || out[1]["input"] == "" {
		t.Fatalf("custom call = %v", out[1])
	}
}

func TestFromOpenAIToResponsesNonStreaming(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl_9",
		"model":"kimi-k3",
		"choices":[{
			"finish_reason":"stop",
			"message":{
				"content":"hello world",
				"reasoning_content":"thinking hard",
				"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]
			}
		}],
		"usage":{"prompt_tokens":11,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}
	}`)
	data, err := FromOpenAIToResponses(body, "kimi-k3", nil)
	if err != nil {
		t.Fatal(err)
	}
	var response responsesObject
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "response" || response.Status != "completed" {
		t.Fatalf("envelope = %+v", response)
	}
	if !strings.HasPrefix(response.ID, "resp_") {
		t.Fatalf("id = %q", response.ID)
	}
	if len(response.Output) != 3 {
		t.Fatalf("output items = %d: %+v", len(response.Output), response.Output)
	}
	if response.Output[0]["type"] != "reasoning" {
		t.Fatalf("first item = %v", response.Output[0])
	}
	if response.Output[1]["type"] != "message" {
		t.Fatalf("second item = %v", response.Output[1])
	}
	call := response.Output[2]
	if call["type"] != "function_call" || call["call_id"] != "call_7" || call["name"] != "get_weather" {
		t.Fatalf("tool item = %v", call)
	}
	if response.Usage == nil || response.Usage.InputTokens != 11 || response.Usage.OutputTokens != 5 || response.Usage.TotalTokens != 16 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Usage.InputTokensDetails.CachedTokens != 3 {
		t.Fatalf("cached tokens = %+v", response.Usage)
	}
}

func TestResponsesStreamWriterEmitsFullEventSequence(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"he"}}]}`,
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"comm"}}]}}]}`,
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":[\"ls\"]}"}}]}}]}`,
		`data: {"id":"chatcmpl_1","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		`data: {"id":"chatcmpl_1","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	var output strings.Builder
	writer := NewResponsesStreamWriter(&output, nil, "test-model", nil)
	if err := writer.Consume(strings.NewReader(upstream)); err != nil {
		t.Fatal(err)
	}

	body := output.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		`"type":"response.output_item.added"`,
		`"type":"message"`,
		`event: response.output_text.delta`,
		`"delta":"he"`,
		`event: response.output_text.done`,
		`"text":"hello"`,
		`event: response.output_item.done`,
		`event: response.function_call_arguments.delta`,
		`event: response.function_call_arguments.done`,
		`"arguments":"{\"command\":[\"ls\"]}"`,
		`event: response.completed`,
		`"input_tokens":4`,
		`"output_tokens":6`,
		`"total_tokens":10`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	// completed must be the terminal event
	frames := strings.Split(strings.TrimSpace(body), "event: ")
	last := frames[len(frames)-1]
	if !strings.HasPrefix(last, "response.completed") {
		t.Fatalf("terminal event = %q, want response.completed", strings.SplitN(last, "\n", 2)[0])
	}
}

func TestResponsesStreamWriterClassifiesShellCalls(t *testing.T) {
	kinds := &ResponsesToolKinds{ShellTool: true}
	upstream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"shell","arguments":"{\"command\":[\"pwd\"]}"}}]}},{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	var output strings.Builder
	writer := NewResponsesStreamWriter(&output, nil, "m", kinds)
	if err := writer.Consume(strings.NewReader(upstream)); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	if !strings.Contains(body, `"type":"local_shell_call"`) {
		t.Fatalf("shell call not classified:\n%s", body)
	}
	if !strings.Contains(body, `"action":{"command":["pwd"],"type":"exec"}`) && !strings.Contains(body, `"type":"exec"`) {
		t.Fatalf("exec action missing:\n%s", body)
	}
}

func TestResponsesStreamWriterEmptyUpstreamStillCompletes(t *testing.T) {
	var output strings.Builder
	writer := NewResponsesStreamWriter(&output, nil, "m", nil)
	if err := writer.Consume(strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	for _, want := range []string{"event: response.created", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestResponsesStreamWriterUpstreamErrorFails(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"error":{"message":"boom","type":"server_error"}}`,
		"",
	}, "\n\n")
	var output strings.Builder
	writer := NewResponsesStreamWriter(&output, nil, "m", nil)
	if err := writer.Consume(strings.NewReader(upstream)); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "boom") {
		t.Fatalf("error event missing:\n%s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("failed stream must not complete:\n%s", body)
	}
}

func TestResponsesInputAsString(t *testing.T) {
	request := mustParseResponses(t, `{"model":"m","input":"just a question"}`)
	messages, err := request.translateInput()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" || messages[0].Content != "just a question" {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestResponsesToolChoiceNamedFunction(t *testing.T) {
	choice := translateResponsesToolChoice([]byte(`{"type":"function","name":"shell"}`))
	asMap, ok := choice.(map[string]any)
	if !ok {
		t.Fatalf("choice = %#v", choice)
	}
	function := asMap["function"].(map[string]any)
	if function["name"] != "shell" {
		t.Fatalf("function = %v", function)
	}
}
