// Package tracing configures OpenTelemetry tracing with autoexport. The
// exporter is selected from the standard OTEL_* environment variables
// (OTEL_TRACES_EXPORTER, OTEL_EXPORTER_OTLP_PROTOCOL, OTEL_EXPORTER_OTLP_ENDPOINT,
// ...); it defaults to OTLP over HTTP against http://localhost:4318.
package tracing

import (
	"context"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup installs a global TracerProvider backed by an autoexport span
// exporter and returns a shutdown function that flushes pending spans.
func Setup(ctx context.Context) (func(context.Context) error, error) {
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
