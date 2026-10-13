package engine

import (
	"bytes"
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

// TestBytesIndexPrefixOrderMatchesComparisonSort checks the bytes index's
// prefix radix against a plain sort by (bytes, position), over values built
// to tie on the 8-byte prefix: equal values, zero-padded short ones, and
// long ones differing only past the prefix.
func TestBytesIndexPrefixOrderMatchesComparisonSort(t *testing.T) {
	vals := [][]byte{
		{}, {0}, {0, 0}, {1}, {1, 0}, []byte("abcdefgh"), []byte("abcdefgh"),
		[]byte("abcdefghZ"), []byte("abcdefghA"), []byte("abcdefg"), []byte("abcdefg\x00"),
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 1}, {0x80}, {0x7f}, []byte("abcdefghA"),
	}
	var heap []byte
	var cells []int64
	for _, v := range vals {
		cells = append(cells, int64(len(heap))|int64(len(v))<<32)
		heap = append(heap, v...)
	}
	got, ok := buildSegColumnIndexBytes(cells, heap)
	if !ok {
		t.Fatal("no index")
	}
	at := func(c int64) []byte { return heap[uint32(c) : uint32(c)+uint32(uint64(c)>>32)] }
	compare := func(a, b int64) int { return bytes.Compare(at(a), at(b)) }
	order := make([]int32, len(cells))
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortFunc(order, func(x, y int32) int {
		if c := compare(cells[x], cells[y]); c != 0 {
			return c
		}
		return cmp.Compare(x, y)
	})
	want, _ := buildSegColumnIndexOrdered(cells, segIdxCodecBytes, compare, order)
	if !bytes.Equal(got, want) {
		t.Fatal("the prefix-radix index differs from the comparison-sorted one")
	}
}
