// random()/randomblob() output bytes cannot be compared in differential tests,
// but everything else (storage class, byte length, row count, deterministic
// cells) is compared. These functions use crypto/rand for genuine unpredictability.
package engine

import (
	"crypto/rand"
	"encoding/binary"
	"math"
)

// randomInt64 returns a uniformly-random int64, excluding -9223372036854775808.
// This matches SQLite's exclusion: abs() of that number returns itself, raising
// an overflow error that should never happen.
func randomInt64() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Read failure is rare; return 0 as a safe fallback.
		return 0
	}
	r := int64(binary.LittleEndian.Uint64(b[:]))
	if r < 0 {
		r = -(r & math.MaxInt64)
	}
	return r
}

// randomBytes fills buf with cryptographically random bytes. A read failure
// leaves buf as-is (typically all-zero) rather than panicking.
func randomBytes(buf []byte) {
	_, _ = rand.Read(buf)
}
