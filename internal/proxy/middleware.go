package proxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		expected := s.config.Server.APIKey
		if expected == "" {
			next(w, request)
			return
		}

		provided := request.Header.Get("x-api-key")
		if authorization := request.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
			provided = strings.TrimPrefix(authorization, "Bearer ")
		}
		if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		next(w, request)
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		started := time.Now()
		response := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(response, request)

		duration := time.Since(started)
		route := routeLabel(request.URL.Path)
		protocol := protocolLabel(request.URL.Path)
		observeRequest(request.Method, route, protocol, response.status, duration)

		if isQuietPath(request.URL.Path) && response.status < 400 {
			return
		}

		fields := logrus.Fields{
			"method":      request.Method,
			"path":        request.URL.Path,
			"route":       route,
			"protocol":    protocol,
			"status":      response.status,
			"request_id":  response.Header().Get("x-request-id"),
			"duration_ms": duration.Milliseconds(),
		}
		if response.errorType != "" {
			fields["error_type"] = response.errorType
		}
		if response.errorMessage != "" {
			fields["error"] = truncateLogValue(response.errorMessage, 240)
		}
		if model := response.Header().Get("x-opencode-proxy-model"); model != "" {
			fields["model"] = model
		}
		if stream := response.Header().Get("x-opencode-proxy-stream"); stream != "" {
			fields["stream"] = stream == "true"
		}
		if agent := request.Header.Get("User-Agent"); agent != "" {
			fields["user_agent"] = truncateLogValue(agent, 120)
		}

		entry := s.log.WithFields(fields)
		switch {
		case response.status >= 500:
			entry.Error("request")
		case response.status >= 400:
			entry.Warn("request")
		default:
			entry.Info("request")
		}
	})
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("x-request-id")
		if requestID == "" || len(requestID) > 128 {
			requestID = newRequestID()
		}
		w.Header().Set("x-request-id", requestID)
		next.ServeHTTP(w, request)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.WithField("panic", fmt.Sprint(recovered)).Error("request panic")
				writeAnthropicError(w, http.StatusInternalServerError, "api_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, request)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status       int
	wroteHeader  bool
	errorType    string
	errorMessage string
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *loggingResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *loggingResponseWriter) noteError(errorType, message string) {
	if errorType != "" {
		w.errorType = errorType
	}
	if message != "" {
		w.errorMessage = message
	}
}

func noteResponseError(w http.ResponseWriter, errorType, message string) {
	if recorder, ok := w.(*loggingResponseWriter); ok {
		recorder.noteError(errorType, message)
	}
}

func setProxyRequestMeta(w http.ResponseWriter, model string, stream bool) {
	if model != "" {
		w.Header().Set("x-opencode-proxy-model", model)
	}
	if stream {
		w.Header().Set("x-opencode-proxy-stream", "true")
	} else {
		w.Header().Set("x-opencode-proxy-stream", "false")
	}
}

func isQuietPath(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/metrics"
}

func truncateLogValue(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func newRequestID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
