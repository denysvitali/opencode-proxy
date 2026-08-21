// Package tracing configures OpenTelemetry tracing with autoexport. The
// exporter is selected from the standard OTEL_* environment variables
// (OTEL_TRACES_EXPORTER, OTEL_EXPORTER_OTLP_PROTOCOL, OTEL_EXPORTER_OTLP_ENDPOINT,
// ...); it defaults to OTLP over HTTP against http://localhost:4318.
package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Configured reports whether any OTEL_* environment variable selects an
// exporter or endpoint. Without one, autoexport would keep dialing the
// default localhost collector and spam connection errors.
func Configured() bool {
	for _, key := range []string{
		"OTEL_TRACES_EXPORTER",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// Setup installs a global TracerProvider backed by an autoexport span
// exporter and returns a shutdown function that flushes pending spans.
// When no OTEL_* configuration is present it returns a nil shutdown and
// leaves the global no-op provider in place.
func Setup(ctx context.Context) (func(context.Context) error, error) {
	if !Configured() {
		return nil, nil
	}
	exporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, err
	}
	resource, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewSchemaless(semconv.ServiceName("opencode-proxy")),
	)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider.Shutdown, nil
}
