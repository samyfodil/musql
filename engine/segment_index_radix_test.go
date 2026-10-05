package engine

import (
	"cmp"
	"math"
	"math/rand"
	"slices"
	"testing"
)

// radixOrderInt64 must order positions exactly as the comparison sort of
// (value, position) pairs it replaced.
func TestRadixOrderInt64MatchesComparisonSort(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	gen := map[string]func(i int) int64{
		"random":    func(int) int64 { return int64(rng.Uint64()) },
		"small":     func(int) int64 { return int64(rng.Intn(10)) },
		"negatives": func(int) int64 { return int64(rng.Intn(2000)) - 1000 },
		"extremes": func(int) int64 {
			return []int64{math.MinInt64, math.MaxInt64, -1, 0, 1, math.MinInt64 + 1}[rng.Intn(6)]
		},
		"sorted":   func(i int) int64 { return int64(i) },
		"reversed": func(i int) int64 { return -int64(i) },
		"equal":    func(int) int64 { return 42 },
		"highbyte": func(int) int64 { return int64(rng.Intn(4)) << 56 },
	}
	for name, g := range gen {
		for _, n := range []int{1, 2, 257, 5000} {
			vals := make([]int64, n)
			for i := range vals {
				vals[i] = g(i)
			}
			want := make([]int32, n)
			for i := range want {
				want[i] = int32(i)
			}
			slices.SortFunc(want, func(x, y int32) int {
				if c := cmp.Compare(vals[x], vals[y]); c != 0 {
					return c
				}
				return cmp.Compare(x, y)
			})
			if got := radixOrderInt64(vals); !slices.Equal(got, want) {
				t.Errorf("%s n=%d: radix order differs from the comparison sort", name, n)
			}
		}
	}
}
