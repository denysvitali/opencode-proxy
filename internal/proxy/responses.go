package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/denysvitali/opencode-proxy/internal/translate"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// responses serves POST /v1/responses, the OpenAI Responses API that Codex
// CLI speaks. Requests are translated to chat-completions for Zen and the
// reply — streaming or not — is translated back into Responses events.
func (s *Server) responses(w http.ResponseWriter, request *http.Request) {
	body, err := s.readBody(w, request)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "request body could not be read")
		return
	}

	parsed, err := translate.ParseResponsesRequest(body)
	if err != nil || parsed.Model == "" {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON request")
		return
	}

	resolvedModel := s.config.ResolveModel(parsed.Model, s.knownModels())
	setProxyRequestMeta(w, resolvedModel, parsed.Stream)

	forwarded, kinds, err := parsed.ToChatCompletions(resolvedModel)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "request could not be translated for this model: "+err.Error())
		return
	}

	accept := "application/json"
	if parsed.Stream {
		accept = "text/event-stream"
	}
	ctx, span := s.tracer().Start(request.Context(), "zen.responses",
		trace.WithAttributes(
			attribute.String("opencode.model", resolvedModel),
			attribute.Bool("opencode.stream", parsed.Stream)))
	defer span.End()

	response, err := s.doWithRetry(ctx, span, "/chat/completions", forwarded, accept)
	if err != nil {
		noteResponseError(w, "api_error", err.Error())
		writeResponsesError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		span.SetStatus(codes.Error, response.Status)
		s.logRejectedBody(response, resolvedModel, forwarded)
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && !s.zen.HasAPIKey() {
			message := fmt.Sprintf("model %q requires an OpenCode Zen API key but none is configured; set OPENCODE_API_KEY", resolvedModel)
			noteResponseError(w, responsesErrorType(response.StatusCode), message)
			writeResponsesError(w, response.StatusCode, responsesErrorType(response.StatusCode), message)
			return
		}
		s.relayUpstreamErrorAs(w, response, writeResponsesError)
		return
	}

	if !parsed.Stream {
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<24))
		if err != nil {
			noteResponseError(w, "api_error", err.Error())
			writeResponsesError(w, http.StatusBadGateway, "api_error", "upstream response could not be read")
			return
		}
		converted, err := translate.FromOpenAIToResponses(data, resolvedModel, kinds)
		if err != nil {
			noteResponseError(w, "api_error", err.Error())
			writeResponsesError(w, http.StatusBadGateway, "api_error", "upstream response could not be translated")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(converted)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	var flush func()
	if flusher, ok := w.(http.Flusher); ok {
		flush = flusher.Flush
	}
	writer := translate.NewResponsesStreamWriter(w, flush, resolvedModel, kinds)
	if err := writer.Consume(response.Body); err != nil {
		s.log.WithError(err).Warn("upstream stream ended early")
		writer.Complete()
	}
}

// chatCompletions serves POST /v1/chat/completions as a near-passthrough:
// Zen already speaks this dialect natively, so only the model field is
// rewritten (and unknown names mapped to the default). This gives Codex's
// wire_api="chat" mode — and any OpenAI client — a direct route.
func (s *Server) chatCompletions(w http.ResponseWriter, request *http.Request) {
	body, err := s.readBody(w, request)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body could not be read")
		return
	}

	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}

	resolvedModel := s.config.ResolveModel(envelope.Model, s.knownModels())
	setProxyRequestMeta(w, resolvedModel, envelope.Stream)

	forwarded, err := withModel(body, resolvedModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request could not be encoded")
		return
	}

	accept := "application/json"
	if envelope.Stream {
		accept = "text/event-stream"
	}
	ctx, span := s.tracer().Start(request.Context(), "zen.chat_completions",
		trace.WithAttributes(
			attribute.String("opencode.model", resolvedModel),
			attribute.Bool("opencode.stream", envelope.Stream)))
	defer span.End()

	response, err := s.doWithRetry(ctx, span, "/chat/completions", forwarded, accept)
	if err != nil {
		noteResponseError(w, "api_error", err.Error())
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		span.SetStatus(codes.Error, response.Status)
		s.logRejectedBody(response, resolvedModel, forwarded)
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && !s.zen.HasAPIKey() {
			message := fmt.Sprintf("model %q requires an OpenCode Zen API key but none is configured; set OPENCODE_API_KEY", resolvedModel)
			noteResponseError(w, responsesErrorType(response.StatusCode), message)
			writeOpenAIError(w, response.StatusCode, message)
			return
		}
		s.relayUpstreamErrorAs(w, response, func(w http.ResponseWriter, status int, _ string, message string) {
			writeOpenAIError(w, status, message)
		})
		return
	}

	contentType := response.Header.Get("Content-Type")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else if envelope.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(response.StatusCode)
	passthrough(w, response.Body)
}

// doWithRetry performs an upstream call with one quick retry on transient
// failures while nothing has been written to the client yet.
func (s *Server) doWithRetry(ctx context.Context, span trace.Span, path string, body []byte, accept string) (*http.Response, error) {
	var response *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		response, err = s.zen.Do(ctx, http.MethodPost, path, body, accept)
		if attempt >= upstreamRetries || !retryableUpstream(response, err) {
			break
		}
		if response != nil {
			_ = response.Body.Close()
		}
		s.log.WithField("attempt", attempt+1).Warn("upstream unavailable; retrying")
		select {
		case <-ctx.Done():
			response, err = nil, ctx.Err()
		case <-time.After(upstreamRetryDelay):
		}
		if err != nil {
			break
		}
	}
	if err != nil && span != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return response, err
}

// logRejectedBody dumps a rejected forwarded body at debug level.
func (s *Server) logRejectedBody(response *http.Response, model string, forwarded []byte) {
	if !s.log.IsLevelEnabled(logrus.DebugLevel) {
		return
	}
	s.log.WithFields(logrus.Fields{
		"request_id": response.Header.Get("x-request-id"),
		"model":      model,
		"status":     response.StatusCode,
		"body_size":  len(forwarded),
		"body":       truncateLogValue(string(forwarded), rejectedBodyLogLimit),
	}).Debug("upstream rejected request body")
}

// relayUpstreamErrorAs relays an upstream error body using a writer function,
// so both Anthropic- and OpenAI-shaped endpoints can share it.
func (s *Server) relayUpstreamErrorAs(w http.ResponseWriter, response *http.Response, write func(w http.ResponseWriter, status int, errorType, message string)) {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	message := zenErrorMessage(data)
	errorType := responsesErrorType(response.StatusCode)
	noteResponseError(w, errorType, message)
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
	write(w, response.StatusCode, errorType, message)
}

func writeResponsesError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    errorType,
			"code":    nil,
			"message": message,
			"param":   nil,
		},
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	writeResponsesError(w, status, responsesErrorType(status), message)
}

// responsesErrorType maps an HTTP status onto an OpenAI-style error type.
func responsesErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}
