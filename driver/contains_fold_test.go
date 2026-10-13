package driver

import (
	"math/rand"
	"strings"
	"testing"
)

// TestContainsFoldMatchesNaive holds containsFold to upper-casing both sides
// and calling strings.Contains, over random text built from the needle's own
// letters in both cases, so near-misses and overlaps are common.
func TestContainsFoldMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	alphabet := "rReEtTuUnNiIgG _x"
	for _, needle := range []string{"RETURNING", "MAX_SIZE", "R"} {
		for trial := 0; trial < 20000; trial++ {
			b := make([]byte, rng.Intn(40))
			for i := range b {
				b[i] = alphabet[rng.Intn(len(alphabet))]
			}
			hay := string(b)
			if rng.Intn(4) == 0 {
				k := rng.Intn(len(hay) + 1)
				hay = hay[:k] + strings.ToLower(needle) + hay[k:]
			}
			want := strings.Contains(strings.ToUpper(hay), needle)
			if got := containsFold(hay, needle); got != want {
				t.Fatalf("containsFold(%q, %q) = %v, want %v", hay, needle, got, want)
			}
		}
	}
}
