package metrics

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPrometheusHandlerContentType(t *testing.T) {
	m := New()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	m.PrometheusHandler(rec, req)

	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("unexpected Content-Type: %q", got)
	}
}

func TestPrometheusHandlerGlobalMetrics(t *testing.T) {
	m := New()
	m.IncrRequest()
	m.IncrRequest()
	m.IncrError()
	m.IncrRetry()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.PrometheusHandler(rec, req)

	body := rec.Body.String()

	wantLines := []string{
		"# HELP rplb_requests_total",
		"# TYPE rplb_requests_total counter",
		"# TYPE rplb_errors_total counter",
		"# TYPE rplb_retries_total counter",
		"# TYPE rplb_uptime_seconds gauge",
		"# TYPE rplb_avg_response_time_ms gauge",
	}
	for _, l := range wantLines {
		if !strings.Contains(body, l) {
			t.Errorf("body missing line %q\nbody:\n%s", l, body)
		}
	}

	// The counter sample line must render a numeric value.
	if !strings.Contains(body, "rplb_requests_total 2") {
		t.Errorf("expected rplb_requests_total 2, body:\n%s", body)
	}
	if !strings.Contains(body, "rplb_errors_total 1") {
		t.Errorf("expected rplb_errors_total 1, body:\n%s", body)
	}
}

func TestPrometheusHandlerBackendMetrics(t *testing.T) {
	m := New()
	backend := "http://backend-1:8080"
	m.RecordBackendRequest(backend)
	m.RecordBackendRequest(backend)
	m.RecordBackendError(backend)
	m.RecordBackendLatency(backend, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.PrometheusHandler(rec, req)

	body := rec.Body.String()

	// At least one backend line labelled by backend url.
	backendLine := `rplb_backend_requests_total{backend="http://backend-1:8080"} 2`
	if !strings.Contains(body, backendLine) {
		t.Errorf("expected backend line %q\nbody:\n%s", backendLine, body)
	}

	if !strings.Contains(body, `backend="`) {
		t.Errorf("expected a backend=\"...\" label in body:\n%s", body)
	}

	if !strings.Contains(body, `rplb_backend_errors_total{backend="http://backend-1:8080"} 1`) {
		t.Errorf("expected backend errors line in body:\n%s", body)
	}
}

func TestPrometheusHandlerValuesAreNumbers(t *testing.T) {
	m := New()
	m.IncrRequest()
	m.RecordBackendRequest("http://a")
	m.RecordBackendLatency("http://a", 5*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.PrometheusHandler(rec, req)

	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Sample line: "<metric>[{labels}] <value>". The value is the
		// final whitespace-separated field and must parse as a float.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("malformed sample line: %q", line)
		}
		value := fields[len(fields)-1]
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			t.Errorf("value %q on line %q is not a number: %v", value, line, err)
		}
	}
}

func TestEscapeLabelValue(t *testing.T) {
	cases := map[string]string{
		`plain`:              `plain`,
		`a"b`:                `a\"b`,
		`a\b`:                `a\\b`,
		"a\nb":               `a\nb`,
		"back\\slash\"quote": `back\\slash\"quote`,
	}
	for in, want := range cases {
		if got := escapeLabelValue(in); got != want {
			t.Errorf("escapeLabelValue(%q) = %q, want %q", in, got, want)
		}
	}
}
