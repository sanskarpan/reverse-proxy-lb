// Package netutil resolves the real client IP of an HTTP request in a way that is
// safe against client-supplied forwarding headers.
package netutil

import (
	"net"
	"net/http"
	"strings"
)

// ParseCIDRs parses a list of CIDR blocks or bare IPs into *net.IPNet. Bare IPs are
// treated as /32 (IPv4) or /128 (IPv6). Unparseable entries are skipped.
func ParseCIDRs(entries []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(e); err == nil {
			nets = append(nets, ipnet)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return nets
}

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// peerIP extracts the bare IP (no port) from r.RemoteAddr.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// ClientIP returns the real client IP. The direct peer (RemoteAddr) is authoritative
// unless it is a trusted proxy, in which case the right-most non-trusted address in
// the X-Forwarded-For chain (falling back to X-Real-IP) is used. When no trusted
// proxies are configured, forwarding headers are ignored entirely — this prevents a
// client from spoofing its IP or evading per-IP rate limits by rotating the header.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := peerIP(r)

	// Direct client (or no trust configured): headers are not trustworthy.
	if len(trusted) == 0 || !ipInNets(net.ParseIP(peer), trusted) {
		return peer
	}

	// Peer is a trusted proxy: walk XFF right-to-left, skipping trusted hops.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(parts[i])
			if ip == "" {
				continue
			}
			if !ipInNets(net.ParseIP(ip), trusted) {
				return ip
			}
		}
	}

	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}

	return peer
}
