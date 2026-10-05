package engine

import (
	"math/rand"
	"slices"
	"testing"
)

// TestRadixSortRowidsIsSignedOrder holds radixSortRowids to rowidLess's order
// against a comparison sort, across negative rowids, the extremes, a dense run
// (most byte passes skipped) and the small-n fallback.
func TestRadixSortRowidsIsSignedOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := map[string][]uint64{}
	dense := make([]uint64, 5000)
	for i := range dense {
		dense[i] = uint64(100_000 + i)
	}
	rng.Shuffle(len(dense), func(i, j int) { dense[i], dense[j] = dense[j], dense[i] })
	cases["dense"] = dense
	mixed := make([]uint64, 3000)
	for i := range mixed {
		mixed[i] = uint64(rng.Int63() - rng.Int63())
	}
	mixed = append(mixed, uint64(1<<63), uint64(1<<63-1), 0, ^uint64(0))
	cases["mixed"] = mixed
	cases["small"] = []uint64{5, ^uint64(0), 3, uint64(1 << 63), 0}
	for name, keys := range cases {
		want := slices.Clone(keys)
		slices.SortFunc(want, func(a, b uint64) int {
			if rowidLess(a, b) {
				return -1
			}
			if rowidLess(b, a) {
				return 1
			}
			return 0
		})
		got := slices.Clone(keys)
		radixSortRowids(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: radix order differs from rowidLess's", name)
		}
	}
}
