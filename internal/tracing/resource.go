package tracing

import (
	"context"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// defaultResource builds an OTel Resource that identifies this service. The
// service.name attribute is the primary grouping key in most backends
// (Jaeger, Grafana Tempo, etc.).
//
// ctx is forwarded to resource.New so that callers can pass a cancellable or
// deadline-bound context. Pass context.Background() when there is no
// tighter scope available.
func defaultResource(ctx context.Context, serviceName string) *resource.Resource {
	if serviceName == "" {
		serviceName = "rplb"
	}
	// resource.New can return an error when it cannot detect environment
	// attributes; we fall back to a minimal resource in that case so Setup
	// never fails for a resource-detection issue.
	r, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		r = resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
		)
	}
	return r
}
