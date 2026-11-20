package engine

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestRadixSortByKeyIsStable(t *testing.T) {
	type row struct {
		k  int64
		at int
	}
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 63, 64, 1000, 70000} {
		for _, span := range []int64{3, 1000, math.MaxInt64} {
			rows := make([]row, n)
			for i := range rows {
				k := r.Int64N(span)
				if span == math.MaxInt64 && r.IntN(2) == 0 {
					k = -k - 1
				}
				rows[i] = row{k, i}
			}
			want := slices.Clone(rows)
			slices.SortStableFunc(want, func(a, b row) int { return cmp.Compare(a.k, b.k) })
			got := radixSortByKey(rows, func(rw row) int64 { return rw.k })
			if !slices.Equal(got, want) {
				t.Fatalf("n=%d span=%d: radix order differs from a stable sort", n, span)
			}
		}
	}
}
