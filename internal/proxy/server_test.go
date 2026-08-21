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

	cfg := config.Config{Proxy: config.ProxyConfig{DefaultModel: "gemini-3-flash"}}
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
	if receivedModel != "gemini-3-flash" {
		t.Fatalf("upstream model = %q, want gemini-3-flash", receivedModel)
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
	server.catalog = []string{"kimi-k3"}
	server.catalogFetched = true
	server.catalogMu.Unlock()

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"kimi-k3","max_tokens":8,"messages":[]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if receivedModel != "kimi-k3" {
		t.Fatalf("upstream model = %q, want kimi-k3", receivedModel)
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
