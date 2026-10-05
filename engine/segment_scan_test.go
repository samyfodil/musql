package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// The fast path is an optimisation OF the general path, never a second
// implementation of it. This pins them against each other over data built so
// that every reason to decline the fast path is reachable, because a fast path
// that silently disagreed would be the one failure this project cannot have: a
// wrong answer that is also faster.
func TestSegmentScanPathsAgree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantKind segScanKind
		build    func(i int) Value
	}{
		{"clean int64 column", segScanSlice, func(i int) Value {
			return Value{Typ: Int, I: int64(i%1000 - 500)}
		}},
		{"a NULL forces the general path", segScanValues, func(i int) Value {
			if i == 137 {
				return Value{Typ: Null}
			}
			return Value{Typ: Int, I: int64(i)}
		}},
		{"an exception forces the general path", segScanValues, func(i int) Value {
			if i == 42 {
				return Value{Typ: Text, S: []byte("not an int")}
			}
			return Value{Typ: Int, I: int64(i)}
		}},
		{"a text column is never the fast path", segScanValues, func(i int) Value {
			return Value{Typ: Text, S: []byte(fmt.Sprintf("%d", i))}
		}},
		{"a real column is never the fast path", segScanValues, func(i int) Value {
			return Value{Typ: Float, F: float64(i)}
		}},
		{"an all-NULL column", segScanValues, func(int) Value { return Value{Typ: Null} }},
		{"negative values, where a sign bug would show", segScanSlice, func(i int) Value {
			return Value{Typ: Int, I: int64(-i)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 2000
			cols := []columnInfo{{Name: "x"}}
			rows := make([][]Value, n)
			rowids := make([]uint64, n)
			for i := range rows {
				rows[i] = []Value{tc.build(i)}
				rowids[i] = uint64(i + 1)
			}
			raw, err := buildSegment(cols, rowids, rows)
			if err != nil {
				t.Fatalf("buildSegment: %v", err)
			}
			s, err := openSegment(raw)
			if err != nil {
				t.Fatalf("openSegment: %v", err)
			}
			kind, why := s.scanKind(0)
			if kind != tc.wantKind {
				t.Fatalf("scanKind = %v (%q), want %v", kind, why, tc.wantKind)
			}

			// Whatever path it takes, the answer must equal the one computed
			// straight off the rows the segment was built from.
			rng := rand.New(rand.NewSource(1))
			for k := 0; k < 25; k++ {
				bound := int64(rng.Intn(2200) - 1100)
				// The oracle is compareValues, the same comparator every other
				// read path in this engine uses -- not a hand-rolled integer
				// test. SQLite orders ACROSS storage classes, so a REAL or a
				// TEXT in this column is greater than any integer bound, and an
				// Int-only expectation here was wrong for exactly the columns
				// this test exists to cover.
				probe := Value{Typ: Int, I: bound}
				want := 0
				for _, r := range rows {
					if compareValues(r[0], probe) > 0 {
						want++
					}
				}
				if got := segmentCountGreater(s, 0, bound); got != want {
					t.Fatalf("bound %d: segment says %d, the rows say %d", bound, got, want)
				}
			}
		})
	}
}

// The fast path must also agree with the general path when BOTH are available
// -- forced here by running the general loop against a column that qualifies
// for the slice, which no production caller does but which is the only way to
// compare them directly on identical data.
func TestSegmentFastPathMatchesValueAccessor(t *testing.T) {
	const n = 5000
	cols := []columnInfo{{Name: "x"}}
	rows := make([][]Value, n)
	rowids := make([]uint64, n)
	rng := rand.New(rand.NewSource(7))
	for i := range rows {
		rows[i] = []Value{{Typ: Int, I: int64(rng.Intn(1_000_000) - 500_000)}}
		rowids[i] = uint64(i + 1)
	}
	raw, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatalf("buildSegment: %v", err)
	}
	s, err := openSegment(raw)
	if err != nil {
		t.Fatalf("openSegment: %v", err)
	}
	if kind, why := s.scanKind(0); kind != segScanSlice {
		t.Fatalf("this column should qualify for the slice: %v (%q)", kind, why)
	}
	col, ok := s.Int64Column(0)
	if !ok {
		t.Fatal("Int64Column declined a column scanKind accepted")
	}
	for i := 0; i < n; i++ {
		if v := s.Value(0, i); v.Typ != Int || v.I != col[i] {
			t.Fatalf("row %d: slice says %d, accessor says %+v", i, col[i], v)
		}
	}
}
