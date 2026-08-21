package proxy

import (
	"net/http"
)

// models serves the OpenAI-style model list from the cached Zen catalog.
func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	ids := s.knownModels()
	if ids == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "model catalog unavailable")
		return
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"owned_by": "opencode",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
