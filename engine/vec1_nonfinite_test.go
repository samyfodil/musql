package engine

import (
	"math"
	"math/rand"
	"testing"
)

// TestVec1NonFiniteMatchesPerElement holds the two-lanes-a-word check to the
// per-element one it replaced: every length up to a few words, each position
// holding NaN, +Inf, -Inf, the largest finite value or a random one.
func TestVec1NonFiniteMatchesPerElement(t *testing.T) {
	perElement := func(b []byte) bool {
		for i := 0; i+4 <= len(b); i += 4 {
			if vec1Native.Uint32(b[i:])&0x7F800000 == 0x7F800000 {
				return true
			}
		}
		return false
	}
	specials := []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), math.MaxFloat32, -math.MaxFloat32, 0, -0}
	rng := rand.New(rand.NewSource(1))
	for n := 0; n <= 40; n++ {
		for trial := 0; trial < 50; trial++ {
			b := make([]byte, 4*n+trial%4) // a ragged tail is ignored by both
			for i := 0; i < n; i++ {
				f := float32(rng.NormFloat64() * 1e3)
				if trial%2 == 0 && rng.Intn(4*n) == 0 { // at most a few, half the trials none
					f = specials[rng.Intn(len(specials))]
				}
				vec1Native.PutUint32(b[4*i:], math.Float32bits(f))
			}
			if got, want := vec1NonFinite(b), perElement(b); got != want {
				t.Fatalf("n=%d %x: got %v, want %v", n, b, got, want)
			}
		}
	}
}
