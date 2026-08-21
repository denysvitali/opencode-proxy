package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustTranslate(t *testing.T, body string) map[string]any {
	t.Helper()
	request, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	encoded, err := ToOpenAI(request, request.Model)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode translated request: %v", err)
	}
	return decoded
}

func TestToOpenAIConvertsToolsAndSystem(t *testing.T) {
	decoded := mustTranslate(t, `{
		"model": "x-preview-f-free",
		"max_tokens": 64,
		"system": [{"type": "text", "text": "be terse", "cache_control": {"type": "ephemeral"}}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
		"tools": [{
			"name": "get_weather",
			"description": "Get weather",
			"input_schema": {"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "properties": {"city": {"type": "string"}}}
		}],
		"tool_choice": {"type": "any", "disable_parallel_tool_use": true},
		"context_management": {"edits": []},
		"output_config": {"effort": "high"}
	}`)

	messages := decoded["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected system + user message, got %d", len(messages))
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be terse" {
		t.Fatalf("unexpected system message: %v", system)
	}

	tools := decoded["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "get_weather" {
		t.Fatalf("tool name not carried over: %v", function)
	}
	parameters := function["parameters"].(map[string]any)
	if _, present := parameters["$schema"]; present {
		t.Fatal("$schema should be stripped from tool parameters")
	}
	if decoded["tool_choice"] != "required" {
		t.Fatalf("expected tool_choice required, got %v", decoded["tool_choice"])
	}
	if decoded["parallel_tool_calls"] != false {
		t.Fatalf("expected parallel_tool_calls false, got %v", decoded["parallel_tool_calls"])
	}
	for _, dropped := range []string{"context_management", "output_config"} {
		if _, present := decoded[dropped]; present {
			t.Fatalf("%s should not be forwarded", dropped)
		}
	}
}

func TestToOpenAISplitsToolResults(t *testing.T) {
	decoded := mustTranslate(t, `{
		"model": "x-preview-f-free",
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "weather?"}]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "internal"},
				{"type": "text", "text": "checking"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "18C"}]},
				{"type": "text", "text": "thanks"}
			]}
		]
	}`)

	messages := decoded["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected user, assistant, tool, user; got %d messages", len(messages))
	}

	assistant := messages[1].(map[string]any)
	if assistant["content"] != "checking" {
		t.Fatalf("thinking block should be dropped and text kept: %v", assistant["content"])
	}
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("unexpected tool arguments: %v", call)
	}

	toolResult := messages[2].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "toolu_1" || toolResult["content"] != "18C" {
		t.Fatalf("unexpected tool message: %v", toolResult)
	}
	if messages[3].(map[string]any)["content"] != "thanks" {
		t.Fatalf("trailing user text lost: %v", messages[3])
	}
}

func TestWantsThinking(t *testing.T) {
	cases := map[string]bool{
		`{"model":"m","messages":[]}`:                                false,
		`{"model":"m","messages":[],"thinking":{"type":"adaptive"}}`: true,
		`{"model":"m","messages":[],"thinking":{"type":"disabled"}}`: false,
	}
	for body, want := range cases {
		request, err := ParseRequest([]byte(body))
		if err != nil {
			t.Fatalf("parse %s: %v", body, err)
		}
		if got := request.WantsThinking(); got != want {
			t.Fatalf("WantsThinking(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestFromOpenAI(t *testing.T) {
	converted, err := FromOpenAI([]byte(`{
		"id": "abc",
		"model": "x-preview-f-free",
		"choices": [{
			"finish_reason": "tool_calls",
			"message": {
				"content": "let me check",
				"reasoning_content": "thinking out loud",
				"tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]
			}
		}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 7, "prompt_tokens_details": {"cached_tokens": 4}}
	}`), "x-preview-f-free", true)
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}

	var message AnthropicMessageOut
	if err := json.Unmarshal(converted, &message); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if message.ID != "msg_abc" || message.Role != "assistant" {
		t.Fatalf("unexpected envelope: %+v", message)
	}
	if len(message.Content) != 3 {
		t.Fatalf("expected thinking, text and tool_use blocks, got %d", len(message.Content))
	}
	if message.Content[0]["type"] != "thinking" || message.Content[1]["type"] != "text" {
		t.Fatalf("unexpected block order: %v", message.Content)
	}
	toolUse := message.Content[2]
	if toolUse["id"] != "toolu_call_1" || toolUse["name"] != "get_weather" {
		t.Fatalf("unexpected tool_use block: %v", toolUse)
	}
	if message.StopReason == nil || *message.StopReason != "tool_use" {
		t.Fatalf("unexpected stop reason: %v", message.StopReason)
	}
	if message.Usage.InputTokens != 12 || message.Usage.OutputTokens != 7 || message.Usage.CacheReadInputTokens != 4 {
		t.Fatalf("unexpected usage: %+v", message.Usage)
	}
}

func TestFromOpenAIDropsThinkingWhenNotRequested(t *testing.T) {
	converted, err := FromOpenAI([]byte(`{"id":"a","choices":[{"finish_reason":"stop","message":{"content":"hi","reasoning_content":"secret"}}]}`), "m", false)
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}
	if strings.Contains(string(converted), "secret") {
		t.Fatalf("reasoning leaked into response: %s", converted)
	}
}

func TestStreamWriterEmitsAnthropicEvents(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"abc","model":"x-preview-f-free","choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}`,
		`data: {"id":"abc","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`data: {"id":"abc","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
		`data: {"id":"abc","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
		`data: {"id":"abc","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`,
		`data: [DONE]`,
		`data: {"choices":[],"cost":"0"}`,
	}, "\n\n")

	var out strings.Builder
	writer := NewStreamWriter(&out, nil, "x-preview-f-free", true)
	if err := writer.Consume(strings.NewReader(upstream)); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("upstream sentinel leaked into Anthropic stream:\n%s", out.String())
	}
	events := parseEvents(t, out.String())

	var names []string
	for _, event := range events {
		names = append(names, event.name)
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected event sequence:\n got %v\nwant %v", names, want)
	}

	// Blocks must be indexed sequentially: thinking 0, text 1, tool_use 2.
	thinking := events[1].data["content_block"].(map[string]any)
	if thinking["type"] != "thinking" || events[1].data["index"].(float64) != 0 {
		t.Fatalf("unexpected thinking block start: %v", events[1].data)
	}
	if delta := events[2].data["delta"].(map[string]any); delta["type"] != "thinking_delta" || delta["thinking"] != "hmm" {
		t.Fatalf("unexpected thinking delta: %v", delta)
	}
	if events[5].data["index"].(float64) != 1 || events[5].data["content_block"].(map[string]any)["type"] != "text" {
		t.Fatalf("unexpected text block start: %v", events[5].data)
	}
	if delta := events[6].data["delta"].(map[string]any); delta["type"] != "text_delta" || delta["text"] != "Hello" {
		t.Fatalf("unexpected text delta: %v", delta)
	}

	toolBlock := events[8].data["content_block"].(map[string]any)
	if events[8].data["index"].(float64) != 2 || toolBlock["id"] != "toolu_call_1" || toolBlock["name"] != "get_weather" {
		t.Fatalf("unexpected tool_use block start: %v", events[8].data)
	}
	var arguments strings.Builder
	for _, event := range events[9:11] {
		delta := event.data["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			t.Fatalf("unexpected tool delta: %v", delta)
		}
		arguments.WriteString(delta["partial_json"].(string))
	}
	if arguments.String() != `{"city":"Paris"}` {
		t.Fatalf("tool arguments did not reassemble: %q", arguments.String())
	}

	final := events[12].data
	if final["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("unexpected stop reason: %v", final)
	}
	if usage := final["usage"].(map[string]any); usage["input_tokens"].(float64) != 5 || usage["output_tokens"].(float64) != 9 {
		t.Fatalf("unexpected usage: %v", usage)
	}
}

type sseEvent struct {
	name string
	data map[string]any
}

func parseEvents(t *testing.T, stream string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(stream), "\n\n") {
		var event sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.data); err != nil {
					t.Fatalf("decode event data %q: %v", line, err)
				}
			}
		}
		events = append(events, event)
	}
	return events
}
