package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/denysvitali/opencode-proxy/internal/config"
	"github.com/denysvitali/opencode-proxy/internal/zen"
	"github.com/sirupsen/logrus"
)

const catalogRefreshInterval = 10 * time.Minute

type Server struct {
	config config.Config
	zen    *zen.Client
	log    *logrus.Logger

	catalogMu      sync.RWMutex
	catalog        []string
	catalogFetched bool
}

func New(cfg config.Config, client *zen.Client, logger *logrus.Logger) *Server {
	server := &Server{config: cfg, zen: client, log: logger}
	server.refreshCatalog(context.Background())
	return server
}

// StartCatalogRefresh keeps the Zen model catalog warm in the background.
func (s *Server) StartCatalogRefresh(ctx context.Context) {
	ticker := time.NewTicker(catalogRefreshInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshCatalog(ctx)
			}
		}
	}()
}

func (s *Server) refreshCatalog(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, err := s.zen.Models(ctx)
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if err != nil {
		if s.catalogFetched {
			s.log.WithError(err).Warn("model catalog refresh failed; keeping previous catalog")
			return
		}
		s.log.WithError(err).Warn("model catalog unavailable; unknown models pass through unchanged")
		return
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	s.catalog = ids
	s.catalogFetched = true
}

func (s *Server) knownModels() []string {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	if !s.catalogFetched {
		return nil
	}
	return s.catalog
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.authenticate(s.dashboard))
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.Handle("GET /metrics", metricsHandler())
	mux.HandleFunc("GET /v1/models", s.authenticate(s.models))
	mux.HandleFunc("POST /v1/messages", s.authenticate(s.messages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.authenticate(s.countTokens))
	return s.recoverPanics(s.logRequests(s.withRequestID(mux)))
}

func (s *Server) ValidateListenAddress() error {
	host, _, err := net.SplitHostPort(s.config.Server.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	ip := net.ParseIP(host)
	isLoopback := host == "localhost" || ip != nil && ip.IsLoopback()
	if !isLoopback && s.config.Server.APIKey == "" && !s.config.Server.AllowInsecure {
		return errors.New("refusing a non-loopback listener without OPENCODE_PROXY_API_KEY; use --allow-insecure to override")
	}
	return nil
}

type healthResponse struct {
	Status string `json:"status"`
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
}
