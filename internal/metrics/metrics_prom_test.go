package metrics

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scrape returns the Prometheus exposition body for m.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.PrometheusHandler(rec, req)
	return rec.Body.String()
}

// sampleValue extracts the numeric value of the first sample line whose
// metric+labels prefix matches want. Returns (value, true) on success.
func sampleValue(body, want string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, want+" ") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, false
			}
			v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

func TestHistogramCumulativeAndCount(t *testing.T) {
	m := New()

	// Observations chosen to land in distinct buckets.
	durs := []time.Duration{
		2 * time.Millisecond,   // <= 0.005
		8 * time.Millisecond,   // <= 0.010
		40 * time.Millisecond,  // <= 0.050
		300 * time.Millisecond, // <= 0.500
		7 * time.Second,        // <= 10.0
	}
	for _, d := range durs {
		m.RecordResponseTime(d)
	}

	body := scrape(t, m)

	// _count must equal number of observations.
	if v, ok := sampleValue(body, "rplb_response_latency_seconds_count"); !ok || v != float64(len(durs)) {
		t.Fatalf("count = %v (ok=%v), want %d\nbody:\n%s", v, ok, len(durs), body)
	}

	// +Inf bucket equals total count.
	if v, ok := sampleValue(body, `rplb_response_latency_seconds_bucket{le="+Inf"}`); !ok || v != float64(len(durs)) {
		t.Fatalf("+Inf bucket = %v (ok=%v), want %d", v, ok, len(durs))
	}

	// Buckets must be monotonically non-decreasing (cumulative).
	bounds := []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10"}
	var prev float64 = -1
	for _, ub := range bounds {
		key := `rplb_response_latency_seconds_bucket{le="` + ub + `"}`
		v, ok := sampleValue(body, key)
		if !ok {
			t.Fatalf("missing bucket %q\nbody:\n%s", key, body)
		}
		if v < prev {
			t.Fatalf("bucket %q value %v < previous %v (not cumulative)", key, v, prev)
		}
		prev = v
	}

	// Specific cumulative checks.
	if v, _ := sampleValue(body, `rplb_response_latency_seconds_bucket{le="0.005"}`); v != 1 {
		t.Errorf("le=0.005 cumulative = %v, want 1", v)
	}
	if v, _ := sampleValue(body, `rplb_response_latency_seconds_bucket{le="0.01"}`); v != 2 {
		t.Errorf("le=0.01 cumulative = %v, want 2", v)
	}
	if v, _ := sampleValue(body, `rplb_response_latency_seconds_bucket{le="0.05"}`); v != 3 {
		t.Errorf("le=0.05 cumulative = %v, want 3", v)
	}
	if v, _ := sampleValue(body, `rplb_response_latency_seconds_bucket{le="0.5"}`); v != 4 {
		t.Errorf("le=0.5 cumulative = %v, want 4", v)
	}
	if v, _ := sampleValue(body, `rplb_response_latency_seconds_bucket{le="10"}`); v != 5 {
		t.Errorf("le=10 cumulative = %v, want 5", v)
	}

	// _sum must be positive.
	if v, ok := sampleValue(body, "rplb_response_latency_seconds_sum"); !ok || v <= 0 {
		t.Errorf("sum = %v (ok=%v), want > 0", v, ok)
	}

	if !strings.Contains(body, "# TYPE rplb_response_latency_seconds histogram") {
		t.Errorf("missing histogram TYPE line")
	}
}

func TestStatusClassCounters(t *testing.T) {
	m := New()
	for _, c := range []int{200, 204, 301, 404, 400, 500, 503, 100 /* ignored */} {
		m.IncrStatusClass(c)
	}

	body := scrape(t, m)

	checks := map[string]float64{
		`rplb_requests_by_class_total{class="2xx"}`: 2,
		`rplb_requests_by_class_total{class="3xx"}`: 1,
		`rplb_requests_by_class_total{class="4xx"}`: 2,
		`rplb_requests_by_class_total{class="5xx"}`: 2,
	}
	for key, want := range checks {
		if v, ok := sampleValue(body, key); !ok || v != want {
			t.Errorf("%s = %v (ok=%v), want %v", key, v, ok, want)
		}
	}
	if !strings.Contains(body, "# TYPE rplb_requests_by_class_total counter") {
		t.Errorf("missing requests_by_class TYPE line")
	}
}

func TestInFlightGauge(t *testing.T) {
	m := New()
	m.IncInFlight()
	m.IncInFlight()
	m.IncInFlight()
	m.DecInFlight()

	body := scrape(t, m)
	if v, ok := sampleValue(body, "rplb_inflight_requests"); !ok || v != 2 {
		t.Fatalf("inflight = %v (ok=%v), want 2\nbody:\n%s", v, ok, body)
	}
	if !strings.Contains(body, "# TYPE rplb_inflight_requests gauge") {
		t.Errorf("missing inflight TYPE line")
	}
}

func TestRateLimitedCounter(t *testing.T) {
	m := New()
	m.IncrRateLimited()
	m.IncrRateLimited()

	body := scrape(t, m)
	if v, ok := sampleValue(body, "rplb_rate_limited_total"); !ok || v != 2 {
		t.Fatalf("rate_limited = %v (ok=%v), want 2", v, ok)
	}
	if !strings.Contains(body, "# TYPE rplb_rate_limited_total counter") {
		t.Errorf("missing rate_limited TYPE line")
	}
}

func TestSnapshotFuncGauges(t *testing.T) {
	m := New()

	// No snapshot registered: no backend health series.
	if strings.Contains(scrape(t, m), "rplb_backend_up{") {
		t.Fatalf("did not expect backend_up without snapshot func")
	}

	m.SetSnapshotFunc(func() []BackendGauge {
		return []BackendGauge{
			{URL: "http://a:8080", Up: true, CircuitState: 0},
			{URL: "http://b:8080", Up: false, CircuitState: 1},
			{URL: "http://c:8080", Up: true, CircuitState: 2},
		}
	})

	body := scrape(t, m)

	upChecks := map[string]float64{
		`rplb_backend_up{backend="http://a:8080"}`: 1,
		`rplb_backend_up{backend="http://b:8080"}`: 0,
		`rplb_backend_up{backend="http://c:8080"}`: 1,
	}
	for key, want := range upChecks {
		if v, ok := sampleValue(body, key); !ok || v != want {
			t.Errorf("%s = %v (ok=%v), want %v", key, v, ok, want)
		}
	}

	stateChecks := map[string]float64{
		`rplb_backend_circuit_state{backend="http://a:8080"}`: 0,
		`rplb_backend_circuit_state{backend="http://b:8080"}`: 1,
		`rplb_backend_circuit_state{backend="http://c:8080"}`: 2,
	}
	for key, want := range stateChecks {
		if v, ok := sampleValue(body, key); !ok || v != want {
			t.Errorf("%s = %v (ok=%v), want %v", key, v, ok, want)
		}
	}

	if !strings.Contains(body, "# TYPE rplb_backend_up gauge") {
		t.Errorf("missing backend_up TYPE line")
	}
	if !strings.Contains(body, "# TYPE rplb_backend_circuit_state gauge") {
		t.Errorf("missing backend_circuit_state TYPE line")
	}
}

// TestExistingSeriesPreserved ensures the additive changes did not drop any
// of the original series and that the exposition parses as valid Prometheus
// text (every sample line has a numeric final field, and TYPE lines exist).
func TestExistingSeriesPreserved(t *testing.T) {
	m := New()
	m.IncrRequest()
	m.IncrError()
	m.IncrRetry()
	m.RecordBackendRequest("http://backend:9000")
	m.RecordBackendError("http://backend:9000")
	m.RecordBackendLatency("http://backend:9000", 12*time.Millisecond)
	m.RecordResponseTime(20 * time.Millisecond)
	m.IncrStatusClass(200)
	m.IncrRateLimited()
	m.IncInFlight()

	body := scrape(t, m)

	original := []string{
		"# TYPE rplb_requests_total counter",
		"# TYPE rplb_errors_total counter",
		"# TYPE rplb_retries_total counter",
		"# TYPE rplb_uptime_seconds gauge",
		"# TYPE rplb_avg_response_time_ms gauge",
		"# TYPE rplb_backend_requests_total counter",
		"# TYPE rplb_backend_errors_total counter",
		"# TYPE rplb_backend_avg_latency_ms gauge",
	}
	for _, l := range original {
		if !strings.Contains(body, l) {
			t.Errorf("original series missing: %q", l)
		}
	}

	// Every non-comment, non-empty line must end in a parseable float.
	sawType := false
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "# TYPE ") {
				sawType = true
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("malformed sample line: %q", line)
		}
		if _, err := strconv.ParseFloat(fields[len(fields)-1], 64); err != nil {
			t.Errorf("non-numeric value on line %q: %v", line, err)
		}
	}
	if !sawType {
		t.Errorf("no # TYPE lines present")
	}

	// JSON handler still works and reflects a request.
	req := httptest.NewRequest(http.MethodGet, "/metrics.json", nil)
	rec := httptest.NewRecorder()
	m.Handler(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("JSON handler Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"total_requests"`) {
		t.Errorf("JSON body missing total_requests:\n%s", rec.Body.String())
	}
}
