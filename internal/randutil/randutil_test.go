package randutil_test

import (
	"testing"

	"reverse-proxy-lb/internal/randutil"
)

// TestNewRandProducesDistinctSequences verifies that two Rand instances created
// back-to-back produce different sequences — the guarantee that fails when seeds
// are derived from wall-clock time alone.
func TestNewRandProducesDistinctSequences(t *testing.T) {
	const draws = 20
	a, b := randutil.NewRand(), randutil.NewRand()

	matches := 0
	for i := 0; i < draws; i++ {
		if a.Int63() == b.Int63() {
			matches++
		}
	}
	// Collision probability for independent sequences: (2^-63)^draws ≈ 0.
	// Any match indicates the seeds are identical.
	if matches > 0 {
		t.Fatalf("two Rand instances produced %d/%d identical values — seeds likely collided", matches, draws)
	}
}

// TestSecureInt64Unique checks that successive calls produce different values.
func TestSecureInt64Unique(t *testing.T) {
	seen := make(map[int64]bool, 100)
	for i := 0; i < 100; i++ {
		v := randutil.SecureInt64()
		if seen[v] {
			t.Fatalf("SecureInt64 returned duplicate value %d after %d calls", v, i)
		}
		seen[v] = true
	}
}
