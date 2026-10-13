package engine

import (
	"math/rand"
	"testing"
)

// TestContainsFoldAsNaive holds r35aContainsFold's block search to the
// byte-at-a-time compare it replaced.
func TestContainsFoldAsNaive(t *testing.T) {
	naive := func(s, sub string) bool {
		if len(sub) == 0 || len(s) < len(sub) {
			return len(sub) == 0
		}
		for i := 0; i+len(sub) <= len(s); i++ {
			j := 0
			for ; j < len(sub); j++ {
				if asciiLowerByte(s[i+j]) != sub[j] {
					break
				}
			}
			if j == len(sub) {
				return true
			}
		}
		return false
	}
	rng := rand.New(rand.NewSource(5))
	alpha := "tTeEmMpPxX_ .1"
	subs := []string{"temp", "temporary", "t", "e", "tt", "_", ".1", "mp", "x", "TEMP"}
	for trial := 0; trial < 200000; trial++ {
		b := make([]byte, rng.Intn(24))
		for i := range b {
			b[i] = alpha[rng.Intn(len(alpha))]
		}
		s := string(b)
		for _, sub := range subs {
			if got, want := r35aContainsFold(s, sub), naive(s, sub); got != want {
				t.Fatalf("r35aContainsFold(%q, %q) = %v, want %v", s, sub, got, want)
			}
		}
	}
	if !r35aContainsFold("", "") || r35aContainsFold("", "a") {
		t.Fatal("empty cases")
	}
}
