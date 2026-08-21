package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/denysvitali/opencode-proxy/internal/config"
	"github.com/denysvitali/opencode-proxy/internal/zen"
	"github.com/sirupsen/logrus"
)

type runtime struct {
	config config.Config
	log    *logrus.Logger
	client *zen.Client
}

func newRuntime() (*runtime, error) {
	_, cfg, err := config.New(options.configFile)
	if err != nil {
		return nil, err
	}
	if options.authFile != "" {
		cfg.AuthFile = options.authFile
	}
	if options.baseURL != "" {
		cfg.BaseURL = options.baseURL
	}
	if options.apiKey != "" {
		cfg.APIKey = options.apiKey
	}
	if options.logLevel != "" {
		cfg.LogLevel = options.logLevel
	}
	if options.logFormat != "" {
		cfg.LogFormat = options.logFormat
	}

	logger, err := newLogger(cfg)
	if err != nil {
		return nil, err
	}

	key, keySource, err := cfg.ResolveAPIKey()
	if err != nil {
		logger.WithError(err).Warn("no upstream API key configured; model requests will fail until OPENCODE_API_KEY is set")
	} else {
		logger.WithField("source", keySource).Info("upstream API key loaded")
	}

	client := zen.New(cfg.BaseURL, key)
	client.HTTP = zen.DefaultHTTPClient()
	return &runtime{config: cfg, log: logger, client: client}, nil
}

func newLogger(cfg config.Config) (*logrus.Logger, error) {
	logger := logrus.New()
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	logger.SetLevel(level)
	logger.SetOutput(os.Stderr)
	switch cfg.LogFormat {
	case "text":
		logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	case "json":
		logger.SetFormatter(&logrus.JSONFormatter{})
	default:
		return nil, errors.New("log format must be text or json")
	}
	return logger, nil
}

func shutdownServer(ctx <-chan struct{}, server *http.Server) {
	<-ctx
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownContext)
}
