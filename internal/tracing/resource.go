package tracing

import (
	"context"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// defaultResource builds an OTel Resource that identifies this service. The
// service.name attribute is the primary grouping key in most backends
// (Jaeger, Grafana Tempo, etc.).
func defaultResource(serviceName string) *resource.Resource {
	if serviceName == "" {
		serviceName = "rplb"
	}
	// resource.New can return an error when it cannot detect environment
	// attributes; we fall back to a minimal resource in that case so Setup
	// never fails for a resource-detection issue.
	r, err := resource.New(
		context.TODO(),
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
