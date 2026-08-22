package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/denysvitali/opencode-proxy/internal/translate"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// retryableUpstream reports whether a failed upstream call is worth a second
// attempt: transport errors, or gateway statuses that usually mean the
// upstream was momentarily overloaded.
func retryableUpstream(response *http.Response, err error) bool {
	if err != nil {
		return true
	}
	switch response.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// messages accepts Anthropic Messages API requests and forwards them to
// OpenCode Zen.
//
// Anthropic's own models go to Zen's /messages endpoint byte-for-byte, with
// only the model field rewritten. Every other model is translated to an
// OpenAI chat-completions request, because Zen forwards Anthropic tool
// definitions to those providers unconverted and they reject the request.
func (s *Server) messages(w http.ResponseWriter, request *http.Request) {
	body, err := s.readBody(w, request)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body could not be read")
		return
	}

	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON request")
		return
	}

	resolvedModel := s.config.ResolveModel(envelope.Model, s.knownModels())
	setProxyRequestMeta(w, resolvedModel, envelope.Stream)

	native := s.config.UsesAnthropicUpstream(resolvedModel)
	upstreamPath := "/messages"
	var forwarded []byte
	var translated *translate.Request
	if native {
		forwarded, err = withModel(body, resolvedModel)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request could not be encoded")
			return
		}
	} else {
		upstreamPath = "/chat/completions"
		translated, err = translate.ParseRequest(body)
		if err == nil {
			forwarded, err = translate.ToOpenAI(translated, resolvedModel)
		}
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request could not be translated for this model: "+err.Error())
			return
		}
	}

	accept := "application/json"
	if envelope.Stream {
		accept = "text/event-stream"
	}
	ctx, span := s.tracer().Start(request.Context(), "zen.messages",
		trace.WithAttributes(
			attribute.String("opencode.model", resolvedModel),
			attribute.Bool("opencode.stream", envelope.Stream),
			attribute.String("opencode.upstream_path", upstreamPath)))
	defer span.End()
	// Nothing has been written to the client yet and the body is fully
	// buffered, so a quick retry on a transient upstream failure is free and
	// saves the whole turn. fetch hands the same call to the relay helpers,
	// which retry a broken body while nothing has reached the client.
	fetch := func() (*http.Response, error) {
		return s.zen.Do(ctx, http.MethodPost, upstreamPath, forwarded, accept)
	}
	response, err := s.doWithRetry(ctx, span, upstreamPath, forwarded, accept)
	if err != nil {
		noteResponseError(w, "api_error", err.Error())
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		span.SetStatus(codes.Error, response.Status)
		s.logRejectedBody(response, resolvedModel, forwarded)
		// Without an upstream key Zen rejects paid models with 401/403; the
		// relayed body would blame the client's credentials, so explain the
		// actual cause instead.
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && !s.zen.HasAPIKey() {
			message := fmt.Sprintf("model %q requires an OpenCode Zen API key but none is configured; set OPENCODE_API_KEY", resolvedModel)
			noteResponseError(w, anthropicErrorType(response.StatusCode), message)
			writeAnthropicError(w, response.StatusCode, anthropicErrorType(response.StatusCode), message)
			return
		}
		s.relayUpstreamError(w, response)
		return
	}

	// Every relay below retries a broken upstream body for as long as nothing
	// has reached the client; the request only ever looks slower, never
	// broken. Once content has flowed, replaying would duplicate it, so the
	// break is reported as the protocol's in-band error event instead.
	switch {
	case envelope.Stream && native:
		s.streamPassthrough(ctx, w, response, response.Header.Get("Content-Type"), fetch, messageStopSeen, emitStreamError, failAnthropic)
	case envelope.Stream:
		s.streamTranslatedEvents(ctx, w, response, fetch, resolvedModel, translated.WantsThinking())
	case native:
		s.relayNativeJSON(ctx, w, response, fetch)
	default:
		s.relayTranslatedJSON(ctx, w, response, fetch, resolvedModel, translated.WantsThinking())
	}
}

// upstreamFetch performs one upstream attempt for the request.
type upstreamFetch func() (*http.Response, error)

// midstreamFailureMessage is the client-facing message for an upstream stream
// that broke after content had already been forwarded.
const midstreamFailureMessage = "the upstream stream was interrupted mid-response"

// retryReader performs one more upstream attempt for the body phase. Every
// failure mode — transport error or non-2xx reply — becomes a reader that
// fails immediately, so retry loops treat all breaks alike.
func (s *Server) retryReader(fetch upstreamFetch) io.Reader {
	response, err := fetch()
	if err != nil {
		return errReader{err: err}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		return errReader{err: fmt.Errorf("upstream returned %s", response.Status)}
	}
	return response.Body
}

// fetchBody reads a non-streaming upstream body to the end, retrying while
// the transfer fails before anything has been written to the client.
func (s *Server) fetchBody(ctx context.Context, response *http.Response, fetch upstreamFetch) ([]byte, error) {
	var body io.Reader = response.Body
	for attempt := 0; ; attempt++ {
		data, readErr := io.ReadAll(io.LimitReader(body, relayBodyLimit))
		if closer, ok := body.(io.Closer); ok {
			_ = closer.Close()
		}
		if readErr == nil {
			if attempt > 0 {
				noteRetryOutcome(retryPhaseBody, retryRecovered)
			}
			return data, nil
		}
		if attempt >= midstreamRetries || !retryPause(ctx, attempt) {
			noteRetryOutcome(retryPhaseBody, retryExhausted)
			return nil, readErr
		}
		noteRetryAttempt(retryPhaseBody)
		s.log.WithError(readErr).WithField("attempt", attempt+1).Warn("upstream body failed before any output; retrying")
		body = s.retryReader(fetch)
	}
}

// streamPassthrough relays a native SSE stream byte-for-byte with the common
// retry contract: transparent retries while nothing has reached the client,
// then the protocol's in-band failure once content has flowed. done reports
// whether a buffered tail holds the stream's completion marker; surfaceErr
// and giveUp write that failure in the caller's dialect.
func (s *Server) streamPassthrough(ctx context.Context, w http.ResponseWriter, response *http.Response, contentType string, fetch upstreamFetch, done func([]byte) bool, surfaceErr func(http.ResponseWriter), giveUp func(http.ResponseWriter, string)) {
	var body io.Reader = response.Body
	for attempt := 0; ; attempt++ {
		relayed, complete, streamErr := pipeSSE(w, contentType, body, done)
		if closer, ok := body.(io.Closer); ok {
			_ = closer.Close()
		}
		if complete {
			if attempt > 0 {
				noteRetryOutcome(retryPhaseBody, retryRecovered)
			}
			return
		}
		if relayed > 0 {
			noteRetryOutcome(retryPhaseBody, retrySurfaced)
			s.log.WithError(streamErr).Warn("upstream stream broke after content was forwarded; surfacing an in-stream error")
			noteResponseError(w, "api_error", midstreamFailureMessage)
			surfaceErr(w)
			return
		}
		if !s.retryOrGiveUp(ctx, w, streamErr, attempt, giveUp) {
			return
		}
		body = s.retryReader(fetch)
	}
}

// streamTranslatedEvents converts Zen's OpenAI chat-completions stream into
// Anthropic events. A broken upstream body is retried transparently for as
// long as nothing has reached the client — the request only ever looks
// slower. Once content has flowed, restarting upstream would duplicate it,
// so the break is surfaced as the protocol's in-band error event instead of
// the silent truncation clients read as finish: unknown.
func (s *Server) streamTranslatedEvents(ctx context.Context, w http.ResponseWriter, response *http.Response, fetch upstreamFetch, model string, thinking bool) {
	var body io.Reader = response.Body
	for attempt := 0; ; attempt++ {
		gate := &gatedWriter{open: func() io.Writer {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			return w
		}}
		writer := translate.NewStreamWriter(gate, func() {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}, model, thinking)
		consumeErr := writer.Consume(body)
		if closer, ok := body.(io.Closer); ok {
			_ = closer.Close()
		}
		streamErr := consumeErr
		if streamErr == nil && gate.wrote == 0 {
			// A 200 that ends without emitting anything is the "clean empty
			// stream" clients experienced as a silent stop.
			streamErr = errors.New("upstream ended the stream without emitting any events")
		}
		if streamErr == nil {
			if attempt > 0 {
				noteRetryOutcome(retryPhaseBody, retryRecovered)
			}
			return
		}
		if gate.wrote > 0 {
			noteRetryOutcome(retryPhaseBody, retrySurfaced)
			s.log.WithError(streamErr).Warn("upstream stream broke after content was forwarded; surfacing an in-stream error")
			noteResponseError(w, "api_error", midstreamFailureMessage)
			writer.Fail(midstreamFailureMessage)
			return
		}
		if !s.retryOrGiveUp(ctx, w, streamErr, attempt, failAnthropic) {
			return
		}
		body = s.retryReader(fetch)
	}
}

// retryOrGiveUp decides whether another body-phase attempt may start. It
// returns false when the loop must end: either the retry budget is spent and
// the client gets a clean 502 in its own dialect, or the client stopped
// waiting.
func (s *Server) retryOrGiveUp(ctx context.Context, w http.ResponseWriter, streamErr error, attempt int, giveUp func(http.ResponseWriter, string)) bool {
	if attempt < midstreamRetries && retryPause(ctx, attempt) {
		noteRetryAttempt(retryPhaseBody)
		s.log.WithError(streamErr).WithField("attempt", attempt+1).Warn("upstream stream failed before any output; retrying")
		return true
	}
	noteRetryOutcome(retryPhaseBody, retryExhausted)
	s.log.WithError(streamErr).Warn("upstream stream failed before any output")
	message := streamFailureMessage(streamErr)
	noteResponseError(w, "api_error", message)
	giveUp(w, message)
	return false
}

// relayNativeJSON buffers a non-streaming Anthropic response and relays it
// verbatim once complete.
func (s *Server) relayNativeJSON(ctx context.Context, w http.ResponseWriter, response *http.Response, fetch upstreamFetch) {
	status := response.StatusCode
	contentType := response.Header.Get("Content-Type")
	data, err := s.fetchBody(ctx, response, fetch)
	if err != nil {
		noteResponseError(w, "api_error", err.Error())
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream response could not be read")
		return
	}
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// relayTranslatedJSON buffers a non-streaming chat-completions response and
// converts it to the Anthropic message shape.
func (s *Server) relayTranslatedJSON(ctx context.Context, w http.ResponseWriter, response *http.Response, fetch upstreamFetch, model string, thinking bool) {
	data, err := s.fetchBody(ctx, response, fetch)
	if err != nil {
		noteResponseError(w, "api_error", err.Error())
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream response could not be read")
		return
	}
	converted, err := translate.FromOpenAI(data, model, thinking)
	if err != nil {
		noteResponseError(w, "api_error", err.Error())
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream response could not be translated")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(converted)
}

// pipeSSE relays a server-sent-events body to the client, flushing after
// every read. It reports how many bytes reached the client and whether the
// stream carried its completion marker (checked by done against a rolling
// window of the tail).
func pipeSSE(w http.ResponseWriter, contentType string, body io.Reader, done func([]byte) bool) (relayed int64, complete bool, err error) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	var window []byte
	opened := false
	for {
		read, readErr := body.Read(buffer)
		if read > 0 {
			if !opened {
				opened = true
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(http.StatusOK)
			}
			if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
				// The client is gone; there is nothing left to relay or report.
				return relayed, true, nil
			}
			if flusher != nil {
				flusher.Flush()
			}
			relayed += int64(read)
			window = append(window, buffer[:read]...)
			if !complete && done(window) {
				complete = true
			}
			if len(window) > 128 {
				window = window[len(window)-128:]
			}
		}
		if readErr != nil {
			return relayed, complete, readErr
		}
	}
}

// messageStopSeen reports whether the buffered stream tail holds Anthropic's
// terminating event — the compact JSON form Zen relays, or the event-name
// form in case the stream is ever reserialized upstream.
func messageStopSeen(window []byte) bool {
	return bytes.Contains(window, []byte(`"type":"message_stop"`)) ||
		bytes.Contains(window, []byte("event: message_stop"))
}

// emitStreamError ends an already-started Anthropic SSE stream with the
// protocol's in-band error event. The client sees a real failure it can
// replay instead of a silent truncation.
func emitStreamError(w http.ResponseWriter) {
	payload, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": midstreamFailureMessage},
	})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// streamFailureMessage renders an upstream stream failure for the client,
// keeping the shared cause visible without leaking transport internals.
func streamFailureMessage(err error) string {
	if err == nil {
		return midstreamFailureMessage
	}
	return "upstream stream failed: " + err.Error()
}

// failAnthropic answers a spent retry budget with an Anthropic-shaped 502.
func failAnthropic(w http.ResponseWriter, message string) {
	writeAnthropicError(w, http.StatusBadGateway, "api_error", message)
}

// gatedWriter commits the response headers the moment the first byte of
// streamed output flows, so a failure before that point can still be answered
// with a clean non-2xx error.
type gatedWriter struct {
	open  func() io.Writer
	w     io.Writer
	wrote int64
}

func (g *gatedWriter) Write(payload []byte) (int, error) {
	g.wrote += int64(len(payload))
	if g.w == nil {
		g.w = g.open()
	}
	return g.w.Write(payload)
}

// tracer returns the proxy-wide OpenTelemetry tracer.
func (s *Server) tracer() trace.Tracer {
	return otel.Tracer("opencode-proxy")
}

// withModel rewrites only the model field of a JSON object, preserving every
// other value exactly as the client sent it.
func withModel(body []byte, model string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = encoded
	return json.Marshal(fields)
}

type tokenCountResponse struct {
	InputTokens int `json:"input_tokens"`
}

func (s *Server) countTokens(w http.ResponseWriter, request *http.Request) {
	body, err := s.readBody(w, request)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON request")
		return
	}
	estimate := (len(body) + 2) / 3
	if estimate < 1 {
		estimate = 1
	}
	w.Header().Set("Warning", `299 opencode-proxy "token count is a conservative estimate"`)
	writeJSON(w, http.StatusOK, tokenCountResponse{InputTokens: estimate})
}

func (s *Server) relayUpstreamError(w http.ResponseWriter, response *http.Response) {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	message := zenErrorMessage(data)
	noteResponseError(w, anthropicErrorType(response.StatusCode), message)
	if len(data) > 0 {
		if response.Header.Get("Content-Type") != "" {
			w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
		return
	}
	writeAnthropicError(w, response.StatusCode, anthropicErrorType(response.StatusCode), message)
}
