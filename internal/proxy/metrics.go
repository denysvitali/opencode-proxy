package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opencode_proxy_http_requests_total",
		Help: "Total HTTP requests handled by opencode-proxy.",
	}, []string{"method", "route", "status", "protocol"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opencode_proxy_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"method", "route", "status", "protocol"})
)

func metricsHandler() http.Handler {
	return promhttp.Handler()
}

func observeRequest(method, route, protocol string, status int, duration time.Duration) {
	statusLabel := strconv.Itoa(status)
	httpRequestsTotal.WithLabelValues(method, route, statusLabel, protocol).Inc()
	httpRequestDuration.WithLabelValues(method, route, statusLabel, protocol).Observe(duration.Seconds())
}

func routeLabel(path string) string {
	switch {
	case path == "/":
		return "/"
	case path == "/healthz":
		return "/healthz"
	case path == "/readyz":
		return "/readyz"
	case path == "/metrics":
		return "/metrics"
	case path == "/v1/models":
		return "/v1/models"
	case path == "/v1/messages":
		return "/v1/messages"
	case path == "/v1/messages/count_tokens":
		return "/v1/messages/count_tokens"
	case strings.HasPrefix(path, "/v1/"):
		return "/v1/*"
	default:
		return "other"
	}
}

func protocolLabel(path string) string {
	switch path {
	case "/v1/messages", "/v1/messages/count_tokens":
		return "anthropic"
	case "/v1/models":
		return "openai"
	default:
		return "http"
	}
}
