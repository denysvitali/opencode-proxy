package proxy

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/denysvitali/opencode-proxy/internal/translate"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

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
	response, err := s.zen.Do(ctx, http.MethodPost, upstreamPath, forwarded, accept)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		noteResponseError(w, "api_error", err.Error())
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		span.SetStatus(codes.Error, response.Status)
		if s.log.IsLevelEnabled(logrus.DebugLevel) {
			s.log.WithFields(logrus.Fields{
				"request_id": response.Header.Get("x-request-id"),
				"model":      resolvedModel,
				"status":     response.StatusCode,
				"body":       truncateLogValue(string(forwarded), 1<<20),
			}).Debug("upstream rejected request body")
		}
		s.relayUpstreamError(w, response)
		return
	}

	if !native {
		s.writeTranslated(w, response, resolvedModel, envelope.Stream, translated.WantsThinking())
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

// writeTranslated converts an OpenAI chat-completions response back into the
// Anthropic shape the client expects, streaming when the client asked to.
func (s *Server) writeTranslated(w http.ResponseWriter, response *http.Response, model string, stream, thinking bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		var flush func()
		if flusher, ok := w.(http.Flusher); ok {
			flush = flusher.Flush
		}
		writer := translate.NewStreamWriter(w, flush, model, thinking)
		if err := writer.Consume(response.Body); err != nil {
			s.log.WithError(err).Warn("upstream stream ended early")
			writer.Finish()
		}
		return
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<24))
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

// passthrough copies an upstream response to the client, flushing after every
// read so server-sent events stream in real time.
func passthrough(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
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
