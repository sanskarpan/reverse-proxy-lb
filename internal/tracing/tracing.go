// Package tracing provides OpenTelemetry distributed tracing setup for the
// reverse-proxy-lb. It installs a global TracerProvider and W3C TraceContext
// propagator, and exposes an HTTP middleware wrapper that creates spans for
// every proxied request.
package tracing

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config carries the tracing configuration block sourced from config.TracingConfig.
type Config struct {
	Enabled     bool
	Exporter    string
	Endpoint    string
	SampleRate  float64
	ServiceName string
}

// Setup installs the global TracerProvider and W3C propagator. When cfg.Enabled
// is false a noop provider is installed so downstream code compiles and runs
// without emitting any telemetry or dialling an external endpoint.
//
// The returned shutdown func must be called on process exit (after all in-flight
// requests have drained) to flush and export buffered spans.
func Setup(cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		// Install a no-op provider so otel.Tracer() and otel.GetTextMapPropagator()
		// are always non-nil; this avoids nil-dereferences in middleware that calls
		// otel.Tracer() unconditionally.
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		return func(_ context.Context) error { return nil }, nil
	}

	var tp *sdktrace.TracerProvider

	switch cfg.Exporter {
	case "otlp", "":
		exp, expErr := otlptracegrpc.New(
			context.Background(),
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			// Insecure is used here so that an operator can point at a local
			// collector (localhost:4317) without a TLS certificate. Production
			// deployments should run the collector with TLS and remove this option.
			otlptracegrpc.WithInsecure(),
		)
		if expErr != nil {
			return nil, fmt.Errorf("tracing: failed to create OTLP exporter: %w", expErr)
		}

		sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithSampler(sampler),
			sdktrace.WithResource(defaultResource(cfg.ServiceName)),
		)
	default:
		return nil, fmt.Errorf("tracing: unsupported exporter %q (supported: otlp)", cfg.Exporter)
	}

	// Install as global provider so any library instrumentation (e.g. otelhttp)
	// picks it up via otel.GetTracerProvider().
	otel.SetTracerProvider(tp)

	// Install the W3C TraceContext + Baggage propagator as the global propagator so
	// the "traceparent" and "baggage" headers are read/written on every request.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown = func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}
	return shutdown, nil
}

// Tracer returns a named tracer from the global provider. The tracer name
// ("reverse-proxy-lb") is the instrumentation scope that appears in the spans
// exported by this package.
func Tracer() trace.Tracer {
	return otel.Tracer("reverse-proxy-lb")
}

// Middleware wraps an http.Handler with OpenTelemetry tracing. Each request
// gets a span named "<METHOD> <path>" so spans are grouped by route rather than
// per-URL (which would produce unbounded cardinality for parameterised paths).
// The span is created from the global provider, which is a noop when tracing is
// disabled, so this middleware is safe to install unconditionally.
func Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "reverse-proxy-lb",
			otelhttp.WithSpanNameFormatter(func(op string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}),
		)
	}
}
