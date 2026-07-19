package netutil

import (
	"net/http"
	"testing"
)

// FuzzClientIP ensures ClientIP never panics on arbitrary RemoteAddr / XFF input,
// with and without trusted proxies.
func FuzzClientIP(f *testing.F) {
	f.Add("1.2.3.4:5678", "10.0.0.1, 9.9.9.9", true)
	f.Add("", "", false)
	f.Add("::1", "garbage,,, ,", true)
	f.Add("[2001:db8::1]:443", "2001:db8::2", false)
	trusted := ParseCIDRs([]string{"10.0.0.0/8"})
	f.Fuzz(func(t *testing.T, remote, xff string, useTrusted bool) {
		r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", xff)
		var nets = trusted
		if !useTrusted {
			nets = nil
		}
		_ = ClientIP(r, nets) // must not panic
	})
}

// FuzzParseCIDRs ensures CIDR/IP parsing never panics on arbitrary input.
func FuzzParseCIDRs(f *testing.F) {
	f.Add("10.0.0.0/8")
	f.Add("not-an-ip")
	f.Add("1.2.3.4")
	f.Fuzz(func(t *testing.T, s string) { _ = ParseCIDRs([]string{s}) })
}
