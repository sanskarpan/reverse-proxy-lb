// Package randutil provides a cryptographically seeded source for
// non-cryptographic pseudo-random number generators. Using crypto/rand for
// the seed ensures that two replicas started simultaneously get distinct
// sequences even when their wall clocks agree to the nanosecond.
package randutil

import (
	"crypto/rand"
	"encoding/binary"
	mrand "math/rand"
)

// NewRand returns a *mrand.Rand seeded from crypto/rand.
// Panics only if the OS entropy source is unavailable, which is a fatal
// condition on any Unix/Windows system.
func NewRand() *mrand.Rand {
	return mrand.New(mrand.NewSource(SecureInt64())) //nolint:gosec // seeded from crypto/rand; non-crypto use
}

// SecureInt64 reads 8 bytes from crypto/rand and interprets them as a signed
// 64-bit integer suitable for use as a math/rand seed.
func SecureInt64() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("randutil: crypto/rand unavailable: " + err.Error())
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}
