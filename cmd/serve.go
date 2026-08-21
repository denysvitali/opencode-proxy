package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/denysvitali/opencode-proxy/internal/proxy"
	"github.com/denysvitali/opencode-proxy/internal/tracing"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func newServeCommand() *cobra.Command {
	var listen string
	var defaultModel string
	var allowInsecure bool
	command := &cobra.Command{
		Use:   "serve",
		Short: "Serve an Anthropic-compatible API backed by OpenCode Zen",
		RunE: func(command *cobra.Command, _ []string) error {
			runtime, err := newRuntime()
			if err != nil {
				return err
			}
			if command.Flags().Changed("listen") {
				runtime.config.Server.Listen = listen
			}
			if command.Flags().Changed("model") {
				runtime.config.Proxy.DefaultModel = defaultModel
			}
			if allowInsecure {
				runtime.config.Server.AllowInsecure = true
			}

			shutdownTracing, err := tracing.Setup(command.Context())
			if err != nil {
				runtime.log.WithError(err).Warn("tracing disabled; continuing without OpenTelemetry")
			}

			handler := proxy.New(runtime.config, runtime.client, runtime.log)
			if err := handler.ValidateListenAddress(); err != nil {
				return err
			}
			server := &http.Server{
				Addr:              runtime.config.Server.Listen,
				Handler:           handler.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       2 * time.Minute,
			}

			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go shutdownServer(ctx.Done(), server)

			runtime.log.WithFields(logrus.Fields{
				"listen":      runtime.config.Server.Listen,
				"upstream":    runtime.config.BaseURL,
				"default":     runtime.config.Proxy.DefaultModel,
				"client_auth": runtime.config.Server.APIKey != "",
				"log_format":  runtime.config.LogFormat,
			}).Info("OpenCode proxy listening")
			err = server.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			if shutdownTracing != nil {
				shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if shutdownErr := shutdownTracing(shutdownContext); shutdownErr != nil && err == nil {
					err = shutdownErr
				}
			}
			return err
		},
	}
	command.Flags().StringVar(&listen, "listen", "127.0.0.1:8090", "listen address")
	command.Flags().StringVarP(&defaultModel, "model", "m", "", "default OpenCode Zen model for unknown requests")
	command.Flags().BoolVar(&allowInsecure, "allow-insecure", false, "allow unauthenticated non-loopback listening")
	return command
}
