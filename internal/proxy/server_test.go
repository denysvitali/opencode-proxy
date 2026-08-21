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

func newTestServer(t *testing.T, upstream *httptest.Server, cfg config.Config) *Server {
	t.Helper()
	client := zen.New("http://"+strings.TrimPrefix(upstream.URL, "http://"), "test-key")
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return New(cfg, client, logger)
}

func TestMessagesRewritesModelAndPassesThrough(t *testing.T) {
	var receivedPath, receivedModel, receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		receivedPath = request.URL.Path
		receivedAuth = request.Header.Get("Authorization")
		var body struct {
			Model  string `json:"model"`
			System any    `json:"system"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		receivedModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"gemini-3-flash","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := config.Config{Proxy: config.ProxyConfig{DefaultModel: "claude-sonnet-4-6"}}
	server := newTestServer(t, upstream, cfg)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-3-5-haiku-20241022","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if receivedPath != "/messages" {
		t.Fatalf("upstream path = %q", receivedPath)
	}
	if receivedAuth != "Bearer test-key" {
		t.Fatalf("upstream auth = %q", receivedAuth)
	}
	if receivedModel != "claude-sonnet-4-6" {
		t.Fatalf("upstream model = %q, want claude-sonnet-4-6", receivedModel)
	}
	var response struct {
		ID      string `json:"id"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "msg_1" || len(response.Content) != 1 || response.Content[0].Text != "hi" {
		t.Fatalf("unexpected passthrough body: %s", recorder.Body.String())
	}
}

func TestMessagesKnownModelPassesThroughUnchanged(t *testing.T) {
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		receivedModel = body.Model
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	server.catalogMu.Lock()
	server.catalog = []string{"claude-opus-5"}
	server.catalogFetched = true
	server.catalogMu.Unlock()

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-opus-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if receivedModel != "claude-opus-5" {
		t.Fatalf("upstream model = %q, want claude-opus-5", receivedModel)
	}
}

// TestMessagesTranslatesNonAnthropicModel covers the reason this proxy needs a
// translation layer at all: Zen hands Anthropic-shaped tool definitions to
// OpenAI-shaped providers unconverted, and they reject the request.
func TestMessagesTranslatesNonAnthropicModel(t *testing.T) {
	var receivedPath string
	var received struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		receivedPath = request.URL.Path
		_ = json.NewDecoder(request.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","model":"kimi-k3","choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"kimi-k3","max_tokens":64,
		"system":"be terse",
		"messages":[{"role":"user","content":[{"type":"text","text":"weather in Paris?"}]}],
		"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object","properties":{}}}]
	}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if receivedPath != "/chat/completions" {
		t.Fatalf("upstream path = %q, want /chat/completions", receivedPath)
	}
	if len(received.Tools) != 1 || received.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools were not translated: %+v", received.Tools)
	}
	if len(received.Messages) != 2 || received.Messages[0].Role != "system" {
		t.Fatalf("messages were not translated: %+v", received.Messages)
	}

	var response struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "message" || response.StopReason != "tool_use" {
		t.Fatalf("unexpected response envelope: %s", recorder.Body.String())
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" || response.Content[0].Name != "get_weather" {
		t.Fatalf("tool call not translated back: %s", recorder.Body.String())
	}
}

func TestMessagesTranslatesStreamingResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"kimi-k3","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type = %q", contentType)
	}
	body := recorder.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
}

func TestCountTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/models" {
			t.Errorf("count_tokens must not hit upstream, got %s %s", request.Method, request.URL.Path)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	body := `{"model":"claude-x","messages":[{"role":"user","content":"hello world"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := (len(body) + 2) / 3
	if response.InputTokens != want {
		t.Fatalf("tokens = %d, want %d", response.InputTokens, want)
	}
}

func TestClientAuthentication(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	cfg := config.Config{Server: config.ServerConfig{APIKey: "secret"}}
	server := newTestServer(t, upstream, cfg)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	request.Header.Set("x-api-key", "secret")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusUnauthorized {
		t.Fatalf("authenticated request rejected")
	}
}

func TestModelsEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"claude-opus-5","object":"model"},{"id":"big-pickle","object":"model"}]}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 2 || response.Data[0].ID != "claude-opus-5" {
		t.Fatalf("unexpected model list: %s", recorder.Body.String())
	}
}

// TestMessagesRetriesTransientUpstreamFailure covers the single retry on
// upstream gateway errors: the first 503 must not fail the whole turn. Only
// the /messages path may fail — Server.New also fetches the model catalog
// from this upstream, and that call must not consume the scripted failure.
func TestMessagesRetriesTransientUpstreamFailure(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"temporarily overloaded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{Proxy: config.ProxyConfig{DefaultModel: "claude-sonnet-4-6"}})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
}

// TestMessagesExplainsMissingUpstreamKeyOn401 covers keyless deployments: a
// relayed upstream "invalid API key" would send users chasing their own
// credentials, so the proxy names the real cause instead.
func TestMessagesExplainsMissingUpstreamKeyOn401(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key provided"}}`))
	}))
	defer upstream.Close()

	client := zen.New("http://"+strings.TrimPrefix(upstream.URL, "http://"), "")
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	server := New(config.Config{}, client, logger)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-opus-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "requires an OpenCode Zen API key") || strings.Contains(body, "Invalid API key provided") {
		t.Fatalf("expected keyless explanation, got: %s", body)
	}
}

func TestHelloEndpointAnswersGetAndHead(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request := httptest.NewRequest(method, "/api/hello", nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s /api/hello status = %d, want 200", method, recorder.Code)
		}
	}
}
