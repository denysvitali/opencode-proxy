package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denysvitali/opencode-proxy/internal/config"
	"github.com/denysvitali/opencode-proxy/internal/zen"
	"github.com/sirupsen/logrus"
)

func TestResponsesEndpointNonStreaming(t *testing.T) {
	var received struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %q", request.URL.Path)
		}
		_ = json.NewDecoder(request.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","model":"kimi-k3","choices":[{"finish_reason":"stop","message":{"content":"hi there"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{Proxy: config.ProxyConfig{DefaultModel: "kimi-k3"}})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"kimi-k3",
		"instructions":"be terse",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]
	}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if received.Model != "kimi-k3" || len(received.Messages) != 2 || received.Messages[0].Content != "be terse" {
		t.Fatalf("upstream request = %+v", received)
	}

	var response struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "response" || response.Status != "completed" {
		t.Fatalf("envelope = %+v", response)
	}
	if len(response.Output) != 1 || response.Output[0].Type != "message" || response.Output[0].Content[0].Text != "hi there" {
		t.Fatalf("output = %+v", response.Output)
	}
	if response.Usage.InputTokens != 3 || response.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestResponsesEndpointStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hey"}}]}`,
			`data: {"id":"c1","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
			`data: [DONE]`,
			"",
		}, "\n\n")))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"kimi-k3","stream":true,"input":"hi"}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type = %q", contentType)
	}
	body := recorder.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	frames := strings.Split(strings.TrimSpace(body), "event: ")
	if !strings.HasPrefix(frames[len(frames)-1], "response.completed") {
		t.Fatalf("terminal event wrong:\n%s", body)
	}
}

func TestChatCompletionsPassthroughRewritesModelAndRelays(t *testing.T) {
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %q", request.URL.Path)
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		receivedModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c9","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{Proxy: config.ProxyConfig{DefaultModel: "x-preview-f-free"}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"totally-unknown-model","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if receivedModel != "x-preview-f-free" {
		t.Fatalf("upstream model = %q", receivedModel)
	}
	var response struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "chat.completion" || response.Choices[0].Message.Content != "ok" {
		t.Fatalf("body not relayed verbatim: %s", recorder.Body.String())
	}
}

func TestResponsesEndpointRelaysUpstreamErrorShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Message != "slow down" {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestResponsesEndpointExplainsMissingUpstreamKeyOn401(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key provided"}}`))
	}))
	defer upstream.Close()

	client := zenClientFor(t, upstream, "")
	logger := discardLogger()
	server := New(config.Config{}, client, logger)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-opus-5","input":"hi"}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "requires an OpenCode Zen API key") {
		t.Fatalf("expected keyless explanation, got: %s", body)
	}
}

func zenClientFor(t *testing.T, upstream *httptest.Server, key string) *zen.Client {
	t.Helper()
	return zen.New("http://"+strings.TrimPrefix(upstream.URL, "http://"), key)
}

func discardLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return logger
}
