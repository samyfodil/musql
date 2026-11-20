package engine

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// sortedOn orders a segment's rows by its key columns, ties in row order:
// checked against a stable comparison sort, over keys with many ties, both
// signs and both ends of int64, on either side of the radix sort's 64-row
// cutoff, and over rows already in order on some keys, but for their last.
func TestSegmentSortedOnOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 238))
	cols := []columnInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	for _, shape := range []string{"random", "ordered", "last out of order"} {
		for _, n := range []int{1, 10, 63, 64, 65, 1000, 5000} {
			rowids := make([]uint64, n)
			rows := make([][]Value, n)
			for i := range n {
				rowids[i] = uint64(i + 1)
				a := int64(rng.IntN(5)) - 2
				b := []int64{math.MinInt64, -1 << 40, -300, 0, 255, 256, 1 << 40, math.MaxInt64}[rng.IntN(8)]
				c := rng.Int64() - math.MaxInt64/2
				if i%7 == 0 {
					c = 3
				}
				if shape != "random" {
					a, b = int64(i/3)-100, int64(i)
					if shape == "last out of order" && i == n-1 && n > 1 {
						a, b = -101, -1
					}
				}
				rows[i] = []Value{{Typ: Int, I: a}, {Typ: Int, I: b}, {Typ: Int, I: c}}
			}
			buf, err := buildSegment(cols, rowids, rows)
			if err != nil {
				t.Fatal(err)
			}
			sg, err := openSegment(buf)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range [][]int{{0}, {1}, {2}, {0, 1}, {1, 0}, {0, 2}, {0, 1, 2}, {2, 0}, {1, 1}} {
				blocks, perm, ok := sg.sortedOn(key)
				if !ok {
					t.Fatalf("%s n=%d key %v: not sorted", shape, n, key)
				}
				want := make([]int32, n)
				for i := range want {
					want[i] = int32(i)
				}
				slices.SortStableFunc(want, func(x, y int32) int {
					for _, c := range key {
						if d := cmp.Compare(rows[x][c].I, rows[y][c].I); d != 0 {
							return d
						}
					}
					return 0
				})
				if !slices.Equal(perm, want) {
					t.Fatalf("%s n=%d key %v: order %v, want %v", shape, n, key, perm[:min(n, 12)], want[:min(n, 12)])
				}
				for j, c := range key {
					for i := range n {
						if blocks[j][i] != rows[i][c].I {
							t.Fatalf("%s n=%d key %v: block %d row %d is %d, want %d", shape, n, key, j, i, blocks[j][i], rows[i][c].I)
						}
					}
				}
				if _, again, _ := sg.sortedOn(key); !slices.Equal(again, want) {
					t.Fatalf("%s n=%d key %v: the kept order differs", shape, n, key)
				}
			}
		}
	}
}
