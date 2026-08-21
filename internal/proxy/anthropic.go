package proxy

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// messages accepts Anthropic Messages API requests and forwards them to the
// OpenCode Zen /messages endpoint, which is Anthropic-compatible. The body is
// passed through byte-for-byte except for a rewritten model field.
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

	forwarded, err := withModel(body, resolvedModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request could not be encoded")
		return
	}

	accept := "application/json"
	if envelope.Stream {
		accept = "text/event-stream"
	}
	ctx, span := s.tracer().Start(request.Context(), "zen.messages",
		trace.WithAttributes(attribute.String("opencode.model", resolvedModel), attribute.Bool("opencode.stream", envelope.Stream)))
	defer span.End()
	response, err := s.zen.Do(ctx, http.MethodPost, "/messages", forwarded, accept)
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
				"body":       truncateLogValue(string(forwarded), 8192),
			}).Debug("upstream rejected request body")
		}
		s.relayUpstreamError(w, response)
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
