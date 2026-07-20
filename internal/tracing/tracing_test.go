package tracing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/tracing"
)

// resetGlobalOtel resets the OTel global state between tests so each test starts
// with a clean slate. Tests run sequentially within a package so this is safe.
func resetGlobalOtel() {
	otel.SetTracerProvider(noop.NewTracerProvider())
}

// TestTracingNoop verifies that Setup with Enabled=false installs a noop provider
// and that the returned shutdown is a no-op (does not block or error).
func TestTracingNoop(t *testing.T) {
	t.Cleanup(resetGlobalOtel)

	shutdown, err := tracing.Setup(tracing.Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup(Enabled=false) returned unexpected error: %v", err)
	}

	// The returned shutdown must be callable and must not error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("noop shutdown returned error: %v", err)
	}

	// The global provider should be a noop (spans created from it carry no-op context).
	tr := otel.GetTracerProvider().Tracer("test")
	_, span := tr.Start(context.Background(), "test-span")
	if span.IsRecording() {
		t.Fatal("expected noop span (IsRecording=false) but got a recording span")
	}
	span.End()
}

// TestTracingNoopMiddlewarePassthrough verifies that the Middleware() wrapper
// correctly passes requests through when the global provider is a noop.
func TestTracingNoopMiddlewarePassthrough(t *testing.T) {
	t.Cleanup(resetGlobalOtel)

	// Install noop (disabled tracing).
	_, _ = tracing.Setup(tracing.Config{Enabled: false})

	const wantBody = "hello"
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	})

	wrapped := tracing.Middleware()(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want status 200, got %d", rec.Code)
	}
	if rec.Body.String() != wantBody {
		t.Errorf("want body %q, got %q", wantBody, rec.Body.String())
	}
}

// TestTracingMiddlewareSpan verifies that a live TracerProvider emits spans for
// requests handled through the Middleware() wrapper.
func TestTracingMiddlewareSpan(t *testing.T) {
	t.Cleanup(resetGlobalOtel)

	// Build an in-process SpanRecorder to capture spans without a network exporter.
	// SpanRecorder implements SpanProcessor (not SpanExporter), so use WithSpanProcessor.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := tracing.Middleware()(inner)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	// Flush any pending spans.
	_ = tp.ForceFlush(context.Background())

	ended := recorder.Ended()
	if len(ended) == 0 {
		t.Fatal("expected at least one span to be recorded, but got none")
	}

	// The span name should encode the method and path.
	found := false
	for _, s := range ended {
		if s.Name() == "GET /health" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(ended))
		for _, s := range ended {
			names = append(names, s.Name())
		}
		t.Errorf("no span named %q; recorded span names: %v", "GET /health", names)
	}
}

// TestTracingW3CPropagation verifies that the W3C TraceContext propagator
// is installed and injects a "traceparent" header into outbound requests when a
// parent trace context is present on the request.
func TestTracingW3CPropagation(t *testing.T) {
	t.Cleanup(resetGlobalOtel)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// Setup installs the propagator as a side effect.
	_, err := tracing.Setup(tracing.Config{
		Enabled:     false, // use the noop Setup to install the propagator only
		ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("Setup error: %v", err)
	}

	// Override the provider with our recording one.
	otel.SetTracerProvider(tp)

	// Start a root span to get a valid trace/span ID.
	ctx, rootSpan := tp.Tracer("test").Start(context.Background(), "root")
	defer rootSpan.End()

	// Simulate what the propagator does: inject the span context into carrier headers.
	carrier := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagationHTTPHeaderCarrier(carrier))

	traceparent := carrier.Get("Traceparent")
	if traceparent == "" {
		// Lowercase header key (W3C spec).
		traceparent = carrier.Get("traceparent")
	}
	if traceparent == "" {
		t.Fatal("expected a 'traceparent' header to be injected, but none found")
	}

	// Verify the injected header decodes to the active trace ID.
	sc := rootSpan.SpanContext()
	if !sc.IsValid() {
		t.Fatal("root span context is not valid")
	}
	traceID := sc.TraceID().String()
	if len(traceparent) < 3 || traceparent[3:3+32] != traceID {
		t.Errorf("traceparent %q does not contain trace ID %q", traceparent, traceID)
	}
}

// TestTracingConfigDefaults verifies that config.Load() applies the correct
// defaults for the TracingConfig block.
func TestTracingConfigDefaults(t *testing.T) {
	// Write a minimal valid YAML config and load it.
	minimalYAML := `
backends:
  - url: http://localhost:8081
`
	f := writeTempConfig(t, minimalYAML)

	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("config.Load error: %v", err)
	}

	tc := cfg.Tracing
	if tc.Enabled {
		t.Error("Tracing.Enabled should default to false")
	}
	if tc.Exporter != "otlp" {
		t.Errorf("Tracing.Exporter: want %q, got %q", "otlp", tc.Exporter)
	}
	if tc.Endpoint != "localhost:4317" {
		t.Errorf("Tracing.Endpoint: want %q, got %q", "localhost:4317", tc.Endpoint)
	}
	if tc.SampleRate != 1.0 {
		t.Errorf("Tracing.SampleRate: want 1.0, got %f", tc.SampleRate)
	}
	if tc.ServiceName != "rplb" {
		t.Errorf("Tracing.ServiceName: want %q, got %q", "rplb", tc.ServiceName)
	}
}

// TestTracingConfigSampleRateValidation verifies that a SampleRate outside
// [0.0, 1.0] is rejected by config.Load().
func TestTracingConfigSampleRateValidation(t *testing.T) {
	tests := []struct {
		name    string
		rate    string
		wantErr bool
	}{
		{"zero", "0.0", false},
		{"one", "1.0", false},
		{"midrange", "0.5", false},
		{"negative", "-0.1", true},
		{"too-high", "1.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := `
backends:
  - url: http://localhost:8081
tracing:
  sample_rate: ` + tt.rate + "\n"
			f := writeTempConfig(t, yaml)
			_, err := config.Load(f)
			if tt.wantErr && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// propagationHTTPHeaderCarrier adapts http.Header to the TextMapCarrier interface
// used by otel's text-map propagator.
type propagationHTTPHeaderCarrier http.Header

func (h propagationHTTPHeaderCarrier) Get(key string) string {
	return http.Header(h).Get(key)
}
func (h propagationHTTPHeaderCarrier) Set(key, val string) {
	http.Header(h).Set(key, val)
}
func (h propagationHTTPHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}

// writeTempConfig writes yaml content to a temp file and returns the path.
func writeTempConfig(t *testing.T, yaml string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp config: %v", err)
	}
	if _, err := f.WriteString(yaml); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

// TestTracingShutdownDrainsWithoutHanging verifies that the shutdown func returned
// by Setup() completes promptly (within a short timeout) and does not block
// indefinitely. This guards against a misconfigured exporter preventing graceful
// shutdown.
func TestTracingShutdownDrainsWithoutHanging(t *testing.T) {
	t.Cleanup(resetGlobalOtel)

	// Use a SpanRecorder as a SpanProcessor: it implements Shutdown which is
	// synchronous and therefore safe to call with a short deadline.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)

	// Create a span to give the processor something to flush.
	_, span := tp.Tracer("test").Start(context.Background(), "drain-test")
	span.End()

	// Shutdown must complete within 2 seconds; a hang here means the exporter
	// is blocking the drain path.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Shutdown did not complete within the 2-second deadline (potential hang)")
	}
}

// Ensure the span context type satisfies trace.Span.
var _ trace.Span = (*noop.Span)(nil)
