package metrics

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// latencyBucketBounds are the upper bounds (in seconds) of the fixed
// response-latency histogram, exposed as rplb_response_latency_seconds.
// The implicit +Inf bucket is handled separately by histCount.
var latencyBucketBounds = []float64{
	0.005, 0.010, 0.025, 0.050, 0.100,
	0.250, 0.500, 1.0, 2.5, 5.0, 10.0,
}

type Metrics struct {
	mu               sync.RWMutex
	TotalRequests    uint64
	TotalErrors      uint64
	TotalRetries     uint64
	BackendRequests  map[string]*BackendMetrics
	ResponseTimes    []time.Duration
	responseTimesMu  sync.Mutex
	maxResponseTimes int
	startTime        time.Time

	// Fixed-bucket latency histogram (seconds). histBuckets[i] counts
	// observations that fall in (bounds[i-1], bounds[i]]; they are made
	// cumulative at exposition time. histCount is the total number of
	// observations (equivalent to the +Inf bucket). histSum is the sum
	// of all observed durations in seconds (scaled by 1e9 as ns to keep
	// it in an integer atomic, converted back on read).
	histBuckets  [11]uint64 // len(latencyBucketBounds)
	histCount    uint64
	histSumNanos uint64

	// Requests bucketed by HTTP status class.
	class2xx uint64
	class3xx uint64
	class4xx uint64
	class5xx uint64

	// In-flight requests gauge and rate-limited counter.
	inFlight    int64
	rateLimited uint64

	// snapshotFunc, when set, is invoked at scrape time to obtain
	// per-backend up/circuit-state gauges without importing balancer.
	snapshotMu   sync.RWMutex
	snapshotFunc func() []BackendGauge
}

// BackendGauge is a scrape-time snapshot of a single backend's health,
// supplied by a callback registered via SetSnapshotFunc.
type BackendGauge struct {
	URL          string
	Up           bool
	CircuitState int // 0=closed, 1=open, 2=half-open
}

type BackendMetrics struct {
	Requests  uint64
	Errors    uint64
	Latencies []time.Duration
	mu        sync.Mutex
}

type PrometheusMetrics struct {
	TotalRequests     float64       `json:"total_requests"`
	TotalErrors       float64       `json:"total_errors"`
	TotalRetries      float64       `json:"total_retries"`
	Uptime            float64       `json:"uptime_seconds"`
	RequestsPerSecond float64       `json:"requests_per_second"`
	AvgResponseTime   float64       `json:"avg_response_time_ms"`
	BackendStats      []BackendStat `json:"backends"`
}

type BackendStat struct {
	URL          string  `json:"url"`
	Requests     float64 `json:"requests"`
	Errors       float64 `json:"errors"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

func New() *Metrics {
	return &Metrics{
		BackendRequests:  make(map[string]*BackendMetrics),
		maxResponseTimes: 1000,
		startTime:        time.Now(),
	}
}

func (m *Metrics) IncrRequest() {
	atomic.AddUint64(&m.TotalRequests, 1)
}

func (m *Metrics) IncrError() {
	atomic.AddUint64(&m.TotalErrors, 1)
}

func (m *Metrics) IncrRetry() {
	atomic.AddUint64(&m.TotalRetries, 1)
}

func (m *Metrics) RecordBackendRequest(url string) {
	m.mu.RLock()
	bm, exists := m.BackendRequests[url]
	m.mu.RUnlock()

	if !exists {
		m.mu.Lock()
		if bm, exists = m.BackendRequests[url]; !exists {
			bm = &BackendMetrics{}
			m.BackendRequests[url] = bm
		}
		m.mu.Unlock()
	}

	atomic.AddUint64(&bm.Requests, 1)
}

func (m *Metrics) RecordBackendError(url string) {
	m.mu.RLock()
	bm, exists := m.BackendRequests[url]
	m.mu.RUnlock()

	if exists {
		atomic.AddUint64(&bm.Errors, 1)
	}
}

func (m *Metrics) RecordResponseTime(d time.Duration) {
	m.responseTimesMu.Lock()
	m.ResponseTimes = append(m.ResponseTimes, d)
	if len(m.ResponseTimes) > m.maxResponseTimes {
		m.ResponseTimes = m.ResponseTimes[1:]
	}
	m.responseTimesMu.Unlock()

	// Feed the fixed-bucket latency histogram.
	secs := d.Seconds()
	atomic.AddUint64(&m.histCount, 1)
	atomic.AddUint64(&m.histSumNanos, uint64(d.Nanoseconds()))
	for i, ub := range latencyBucketBounds {
		if secs <= ub {
			atomic.AddUint64(&m.histBuckets[i], 1)
			break
		}
	}
}

// IncrStatusClass increments the counter for the HTTP status class of
// code (2xx/3xx/4xx/5xx). Codes outside 200-599 are ignored.
func (m *Metrics) IncrStatusClass(code int) {
	switch {
	case code >= 200 && code < 300:
		atomic.AddUint64(&m.class2xx, 1)
	case code >= 300 && code < 400:
		atomic.AddUint64(&m.class3xx, 1)
	case code >= 400 && code < 500:
		atomic.AddUint64(&m.class4xx, 1)
	case code >= 500 && code < 600:
		atomic.AddUint64(&m.class5xx, 1)
	}
}

// IncInFlight increments the in-flight requests gauge.
func (m *Metrics) IncInFlight() {
	atomic.AddInt64(&m.inFlight, 1)
}

// DecInFlight decrements the in-flight requests gauge.
func (m *Metrics) DecInFlight() {
	atomic.AddInt64(&m.inFlight, -1)
}

// IncrRateLimited increments the rate-limited requests counter.
func (m *Metrics) IncrRateLimited() {
	atomic.AddUint64(&m.rateLimited, 1)
}

// SetSnapshotFunc registers a callback invoked at scrape time to obtain
// per-backend up/circuit-state gauges. Passing nil clears it.
func (m *Metrics) SetSnapshotFunc(fn func() []BackendGauge) {
	m.snapshotMu.Lock()
	m.snapshotFunc = fn
	m.snapshotMu.Unlock()
}

func (m *Metrics) RecordBackendLatency(url string, d time.Duration) {
	m.mu.RLock()
	bm, exists := m.BackendRequests[url]
	m.mu.RUnlock()

	if exists {
		bm.mu.Lock()
		bm.Latencies = append(bm.Latencies, d)
		if len(bm.Latencies) > 100 {
			bm.Latencies = bm.Latencies[1:]
		}
		bm.mu.Unlock()
	}
}

func (m *Metrics) GetPrometheusMetrics() PrometheusMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()

	totalReqs := atomic.LoadUint64(&m.TotalRequests)
	totalErrs := atomic.LoadUint64(&m.TotalErrors)
	totalRetries := atomic.LoadUint64(&m.TotalRetries)

	uptime := time.Since(m.startTime).Seconds()
	var avgRespTime float64

	m.responseTimesMu.Lock()
	if len(m.ResponseTimes) > 0 {
		var total time.Duration
		for _, rt := range m.ResponseTimes {
			total += rt
		}
		avgRespTime = float64(total) / float64(len(m.ResponseTimes)) / float64(time.Millisecond)
	}
	m.responseTimesMu.Unlock()

	var rps float64
	if uptime > 0 {
		rps = float64(totalReqs) / uptime
	}

	backendStats := make([]BackendStat, 0, len(m.BackendRequests))
	for url, bm := range m.BackendRequests {
		reqs := atomic.LoadUint64(&bm.Requests)
		errs := atomic.LoadUint64(&bm.Errors)

		var avgLat float64
		bm.mu.Lock()
		if len(bm.Latencies) > 0 {
			var total time.Duration
			for _, l := range bm.Latencies {
				total += l
			}
			avgLat = float64(total) / float64(len(bm.Latencies)) / float64(time.Millisecond)
		}
		bm.mu.Unlock()

		backendStats = append(backendStats, BackendStat{
			URL:          url,
			Requests:     float64(reqs),
			Errors:       float64(errs),
			AvgLatencyMs: avgLat,
		})
	}

	return PrometheusMetrics{
		TotalRequests:     float64(totalReqs),
		TotalErrors:       float64(totalErrs),
		TotalRetries:      float64(totalRetries),
		Uptime:            uptime,
		RequestsPerSecond: rps,
		AvgResponseTime:   avgRespTime,
		BackendStats:      backendStats,
	}
}

func (m *Metrics) Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m.GetPrometheusMetrics())
}

// escapeLabelValue escapes a label value according to the Prometheus
// text exposition format: backslash, double-quote and newline must be
// escaped. See https://prometheus.io/docs/instrumenting/exposition_formats/
func escapeLabelValue(v string) string {
	// Order matters: escape backslashes first.
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// formatFloat renders a float64 as a Prometheus numeric value.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// PrometheusHandler serves metrics in the Prometheus text exposition
// format (version 0.0.4). It reuses GetPrometheusMetrics for the numbers.
func (m *Metrics) PrometheusHandler(w http.ResponseWriter, r *http.Request) {
	pm := m.GetPrometheusMetrics()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var b strings.Builder

	writeMetric := func(name, help, typ, value string) {
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(help)
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(typ)
		b.WriteByte('\n')
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(value)
		b.WriteByte('\n')
	}

	writeMetric("rplb_requests_total", "Total number of requests handled by the proxy.", "counter", formatFloat(pm.TotalRequests))
	writeMetric("rplb_errors_total", "Total number of errored requests.", "counter", formatFloat(pm.TotalErrors))
	writeMetric("rplb_retries_total", "Total number of request retries.", "counter", formatFloat(pm.TotalRetries))
	writeMetric("rplb_uptime_seconds", "Proxy uptime in seconds.", "gauge", formatFloat(pm.Uptime))
	writeMetric("rplb_avg_response_time_ms", "Average response time in milliseconds.", "gauge", formatFloat(pm.AvgResponseTime))

	// Per-backend metrics, labelled by backend url.
	b.WriteString("# HELP rplb_backend_requests_total Total number of requests routed to a backend.\n")
	b.WriteString("# TYPE rplb_backend_requests_total counter\n")
	for _, bs := range pm.BackendStats {
		b.WriteString(`rplb_backend_requests_total{backend="`)
		b.WriteString(escapeLabelValue(bs.URL))
		b.WriteString(`"} `)
		b.WriteString(formatFloat(bs.Requests))
		b.WriteByte('\n')
	}

	b.WriteString("# HELP rplb_backend_errors_total Total number of errored requests per backend.\n")
	b.WriteString("# TYPE rplb_backend_errors_total counter\n")
	for _, bs := range pm.BackendStats {
		b.WriteString(`rplb_backend_errors_total{backend="`)
		b.WriteString(escapeLabelValue(bs.URL))
		b.WriteString(`"} `)
		b.WriteString(formatFloat(bs.Errors))
		b.WriteByte('\n')
	}

	b.WriteString("# HELP rplb_backend_avg_latency_ms Average backend latency in milliseconds.\n")
	b.WriteString("# TYPE rplb_backend_avg_latency_ms gauge\n")
	for _, bs := range pm.BackendStats {
		b.WriteString(`rplb_backend_avg_latency_ms{backend="`)
		b.WriteString(escapeLabelValue(bs.URL))
		b.WriteString(`"} `)
		b.WriteString(formatFloat(bs.AvgLatencyMs))
		b.WriteByte('\n')
	}

	// Response-latency histogram (seconds) with cumulative buckets.
	histCount := atomic.LoadUint64(&m.histCount)
	histSum := float64(atomic.LoadUint64(&m.histSumNanos)) / 1e9
	b.WriteString("# HELP rplb_response_latency_seconds Response latency in seconds.\n")
	b.WriteString("# TYPE rplb_response_latency_seconds histogram\n")
	var cumulative uint64
	for i, ub := range latencyBucketBounds {
		cumulative += atomic.LoadUint64(&m.histBuckets[i])
		b.WriteString(`rplb_response_latency_seconds_bucket{le="`)
		b.WriteString(formatFloat(ub))
		b.WriteString(`"} `)
		b.WriteString(formatFloat(float64(cumulative)))
		b.WriteByte('\n')
	}
	b.WriteString(`rplb_response_latency_seconds_bucket{le="+Inf"} `)
	b.WriteString(formatFloat(float64(histCount)))
	b.WriteByte('\n')
	b.WriteString("rplb_response_latency_seconds_sum ")
	b.WriteString(formatFloat(histSum))
	b.WriteByte('\n')
	b.WriteString("rplb_response_latency_seconds_count ")
	b.WriteString(formatFloat(float64(histCount)))
	b.WriteByte('\n')

	// Requests by status class.
	b.WriteString("# HELP rplb_requests_by_class_total Total requests bucketed by HTTP status class.\n")
	b.WriteString("# TYPE rplb_requests_by_class_total counter\n")
	classes := []struct {
		name string
		val  uint64
	}{
		{"2xx", atomic.LoadUint64(&m.class2xx)},
		{"3xx", atomic.LoadUint64(&m.class3xx)},
		{"4xx", atomic.LoadUint64(&m.class4xx)},
		{"5xx", atomic.LoadUint64(&m.class5xx)},
	}
	for _, c := range classes {
		b.WriteString(`rplb_requests_by_class_total{class="`)
		b.WriteString(c.name)
		b.WriteString(`"} `)
		b.WriteString(formatFloat(float64(c.val)))
		b.WriteByte('\n')
	}

	// In-flight requests gauge.
	writeMetric("rplb_inflight_requests", "Number of requests currently being processed.", "gauge", formatFloat(float64(atomic.LoadInt64(&m.inFlight))))

	// Rate-limited requests counter.
	writeMetric("rplb_rate_limited_total", "Total number of rate-limited requests.", "counter", formatFloat(float64(atomic.LoadUint64(&m.rateLimited))))

	// Scrape-time backend health gauges from the registered snapshot.
	m.snapshotMu.RLock()
	fn := m.snapshotFunc
	m.snapshotMu.RUnlock()
	if fn != nil {
		gauges := fn()
		b.WriteString("# HELP rplb_backend_up Whether a backend is currently healthy (1) or not (0).\n")
		b.WriteString("# TYPE rplb_backend_up gauge\n")
		for _, g := range gauges {
			b.WriteString(`rplb_backend_up{backend="`)
			b.WriteString(escapeLabelValue(g.URL))
			b.WriteString(`"} `)
			if g.Up {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
			b.WriteByte('\n')
		}
		b.WriteString("# HELP rplb_backend_circuit_state Circuit breaker state per backend (0=closed,1=open,2=half-open).\n")
		b.WriteString("# TYPE rplb_backend_circuit_state gauge\n")
		for _, g := range gauges {
			b.WriteString(`rplb_backend_circuit_state{backend="`)
			b.WriteString(escapeLabelValue(g.URL))
			b.WriteString(`"} `)
			b.WriteString(formatFloat(float64(g.CircuitState)))
			b.WriteByte('\n')
		}
	}

	w.Write([]byte(b.String()))
}

func (m *Metrics) GetAvgResponseTime() time.Duration {
	m.responseTimesMu.Lock()
	defer m.responseTimesMu.Unlock()

	if len(m.ResponseTimes) == 0 {
		return 0
	}

	var total time.Duration
	for _, rt := range m.ResponseTimes {
		total += rt
	}
	return total / time.Duration(len(m.ResponseTimes))
}
