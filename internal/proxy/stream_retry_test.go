package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denysvitali/opencode-proxy/internal/config"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// outcomeDelta captures a retry-outcome counter so a test can assert its own
// increment regardless of what earlier tests already recorded.
func outcomeDelta(phase, outcome string) func() float64 {
	before := testutil.ToFloat64(upstreamRetryOutcomes.WithLabelValues(phase, outcome))
	return func() float64 {
		return testutil.ToFloat64(upstreamRetryOutcomes.WithLabelValues(phase, outcome)) - before
	}
}

// TestMessagesRetriesCleanEmptyStream covers the failure opencode shipped
// v1.18.20 for: an upstream 200 whose body ends before any event. The proxy
// retries while nothing has reached the client, so the turn just looks slower.
func TestMessagesRetriesCleanEmptyStream(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// Only /chat/completions may be scripted; Server.New also fetches the
		// model catalog from this upstream.
		if request.URL.Path != "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			return // 200, then hang up: the "clean empty stream".
		}
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	recovered := outcomeDelta(retryPhaseBody, retryRecovered)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"kimi-k3","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
	body := recorder.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("retried stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("a retried break must stay invisible to the client:\n%s", body)
	}
	if recovered() < 1 {
		t.Fatalf("expected a %q/%q metric increment", retryPhaseBody, retryRecovered)
	}
}

// TestMessagesRetriesBrokenNativeStreamBeforeCommit covers an upstream that
// accepts the request, sends headers, and drops the connection before any
// event. Nothing has been forwarded yet, so the proxy retries transparently
// instead of letting the turn die.
func TestMessagesRetriesBrokenNativeStreamBeforeCommit(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempts++
		if attempts == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
	body := recorder.Body.String()
	if strings.Count(body, "event: message_start") != 1 {
		t.Fatalf("expected exactly one message_start after the retry:\n%s", body)
	}
	if !strings.Contains(body, `"type":"message_stop"`) {
		t.Fatalf("retried stream did not complete:\n%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("a retried break must stay invisible to the client:\n%s", body)
	}
}

// TestMessagesSurfacesMidstreamBreakAfterContent covers the case a retry
// cannot fix: the upstream breaks once deltas have already been forwarded.
// Replaying would duplicate content, so the stream must end with the
// protocol's in-band error event instead of a silent truncation.
func TestMessagesSurfacesMidstreamBreakAfterContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	surfaced := outcomeDelta(retryPhaseBody, retrySurfaced)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"api_error"`) {
		t.Fatalf("midstream break must surface an in-band error event:\n%s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("a broken stream must not look completed:\n%s", body)
	}
	if surfaced() < 1 {
		t.Fatalf("expected a %q/%q metric increment", retryPhaseBody, retrySurfaced)
	}
}

// TestChatCompletionsRetriesCleanEmptyStream pins the same transparent-retry
// contract for the OpenAI chat-completions passthrough.
func TestChatCompletionsRetriesCleanEmptyStream(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			return // 200, then hang up: the "clean empty stream".
		}
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"kimi-k3","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
	body := recorder.Body.String()
	for _, want := range []string{"data: {\"id\":\"c1\"", "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("retried stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\"error\"") {
		t.Fatalf("a retried break must stay invisible to the client:\n%s", body)
	}
}

// TestResponsesSurfacesMidstreamBreakAfterContent pins the Responses API half:
// a chat-completions stream that stops before any finish_reason must end as
// response.failed instead of a completed envelope hiding the truncation.
func TestResponsesSurfacesMidstreamBreakAfterContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	surfaced := outcomeDelta(retryPhaseBody, retrySurfaced)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"kimi-k3","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("midstream break must surface response.failed:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("a broken stream must not look completed:\n%s", body)
	}
	if surfaced() < 1 {
		t.Fatalf("expected a %q/%q metric increment", retryPhaseBody, retrySurfaced)
	}
}

// TestMessagesTranslatedMidstreamBreakNotMasked pins the translate-side half
// of the bug: a chat-completions stream that stops without [DONE] used to be
// finished off with a normal message_stop, hiding the truncation.
func TestMessagesTranslatedMidstreamBreakNotMasked(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream, config.Config{})
	surfaced := outcomeDelta(retryPhaseBody, retrySurfaced)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"kimi-k3","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("truncated translation stream must surface an error event:\n%s", body)
	}
	if strings.Contains(body, "event: message_stop") {
		t.Fatalf("truncated stream must not be finished off as a completion:\n%s", body)
	}
	if surfaced() < 1 {
		t.Fatalf("expected a %q/%q metric increment", retryPhaseBody, retrySurfaced)
	}
}
