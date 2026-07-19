package netutil

import (
	"net/http"
	"testing"
)

func req(remoteAddr string, headers map[string]string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// ID 3: with no trusted proxies, forwarding headers must be ignored entirely so a
// client cannot spoof its IP or evade per-IP rate limits.
func TestClientIP_IgnoresSpoofedHeaderWhenUntrusted(t *testing.T) {
	r := req("9.9.9.9:5555", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if got := ClientIP(r, nil); got != "9.9.9.9" {
		t.Errorf("expected peer 9.9.9.9 (header ignored), got %q", got)
	}
}

// A different spoofed header must still resolve to the same peer, so rate-limit
// buckets cannot be multiplied by rotating the header.
func TestClientIP_RotatingHeaderCannotEvadeRateLimit(t *testing.T) {
	r1 := req("9.9.9.9:1", map[string]string{"X-Forwarded-For": "1.1.1.1"})
	r2 := req("9.9.9.9:2", map[string]string{"X-Forwarded-For": "2.2.2.2"})
	if ClientIP(r1, nil) != ClientIP(r2, nil) {
		t.Error("rotating X-Forwarded-For should not change the resolved client IP")
	}
}

// When the direct peer IS a trusted proxy, the real client is taken from the XFF
// chain (right-most non-trusted hop).
func TestClientIP_TrustedProxyHonorsForwardedFor(t *testing.T) {
	trusted := ParseCIDRs([]string{"10.0.0.0/8"})
	r := req("10.0.0.5:5555", map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.9"})
	if got := ClientIP(r, trusted); got != "203.0.113.7" {
		t.Errorf("expected real client 203.0.113.7, got %q", got)
	}
}

func TestClientIP_FallsBackToRealIPHeader(t *testing.T) {
	trusted := ParseCIDRs([]string{"10.0.0.0/8"})
	r := req("10.0.0.5:5555", map[string]string{"X-Real-IP": "203.0.113.7"})
	if got := ClientIP(r, trusted); got != "203.0.113.7" {
		t.Errorf("expected 203.0.113.7 from X-Real-IP, got %q", got)
	}
}

func TestParseCIDRs_BareIP(t *testing.T) {
	nets := ParseCIDRs([]string{"192.168.1.1", "bogus", "10.0.0.0/8"})
	if len(nets) != 2 {
		t.Fatalf("expected 2 valid nets, got %d", len(nets))
	}
}
