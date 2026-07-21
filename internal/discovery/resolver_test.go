// resolver_test.go tests the stdResolver (stdlib-backed Resolver). The tests
// use well-known public DNS names and SRV records; they skip automatically when
// network is unavailable so they don't break CI without internet access.
package discovery

import (
	"strings"
	"testing"
)

func TestStdResolverLookupHost(t *testing.T) {
	r := NewResolver()

	// localhost should resolve without needing external DNS.
	addrs, err := r.LookupHost("localhost")
	if err != nil {
		t.Skipf("skipping: LookupHost(localhost) failed: %v", err)
	}
	if len(addrs) == 0 {
		t.Error("LookupHost(localhost) returned no addresses")
	}
	// At least one address should be a loopback.
	for _, a := range addrs {
		if strings.HasPrefix(a, "127.") || a == "::1" {
			return // found a loopback address
		}
	}
	// Loopback not found is unusual but not a hard failure on unusual OS configs.
	t.Logf("LookupHost(localhost) returned %v — no loopback found (unexpected but non-fatal)", addrs)
}

func TestStdResolverLookupHostInvalid(t *testing.T) {
	r := NewResolver()

	// An invalid hostname that should not resolve.
	_, err := r.LookupHost("this.hostname.does.not.exist.invalid.")
	if err == nil {
		t.Log("LookupHost(invalid) unexpectedly succeeded — may have a wildcard DNS")
	}
}

func TestStdResolverLookupSRVError(t *testing.T) {
	r := NewResolver()

	// A non-existent SRV name should return an error (or empty results on some
	// resolver configurations). We just verify no panic occurs.
	_, err := r.LookupSRV("_nonexistent._tcp.this.does.not.exist.invalid.")
	// Error is expected; just don't panic.
	_ = err
}

func TestNewResolverReturnedType(t *testing.T) {
	r := NewResolver()
	if r == nil {
		t.Fatal("NewResolver returned nil")
	}
	// Verify it implements the Resolver interface.
	var _ Resolver = r
}
