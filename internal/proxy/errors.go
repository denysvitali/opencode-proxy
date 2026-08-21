package proxy

import (
	"encoding/json"
	"io"
	"net/http"
)

func (s *Server) readBody(w http.ResponseWriter, request *http.Request) ([]byte, error) {
	limit := s.config.Server.MaxBodyBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	request.Body = http.MaxBytesReader(w, request.Body, limit)
	defer request.Body.Close()
	return io.ReadAll(request.Body)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errorType, "message": message},
	})
}

func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

// zenErrorMessage extracts a human-readable message from an upstream error
// body without assuming a specific schema.
func zenErrorMessage(data []byte) string {
	if len(data) == 0 {
		return "upstream request failed"
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &parsed); err == nil {
		if parsed.Error.Message != "" {
			return parsed.Error.Message
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	return "upstream request failed"
}
