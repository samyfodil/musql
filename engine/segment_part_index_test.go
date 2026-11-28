package engine

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

// TestSeekRowsOverMixedClasses: a column holding NULLs or values of another
// storage class is seeked through the index of its other rows. seekRows
// answers every row equal to the probe, ascending, with no row the index
// itself rules out; segEqualityCount, which nothing tests again, answers the
// exact count or declines. Each column type a segment stores is covered, its
// index in the file and built on first use.
func TestSeekRowsOverMixedClasses(t *testing.T) {
	cols := []columnInfo{{Name: "i"}, {Name: "f"}, {Name: "n"}, {Name: "s"}, {Name: "z"}}
	var rowids []uint64
	var rows [][]Value
	for k := 0; k < 3000; k++ {
		i := Value{Typ: Int, I: int64(k % 37)}
		f := Value{Typ: Float, F: float64(k%29) + 0.25}
		n := Value{Typ: Int, I: int64(k % 31)}
		if k%3 == 0 {
			n = Value{Typ: Float, F: float64(k%31) + 0.5}
		}
		s := Value{Typ: Text, S: fmt.Appendf(nil, "s%d", k%41)}
		z := Value{Typ: Int, I: int64(k % 37)}
		switch {
		case k%97 == 0:
			i, f, n, s, z = Value{Typ: Null}, Value{Typ: Null}, Value{Typ: Null}, Value{Typ: Null}, Value{Typ: Null}
		case k%211 == 0:
			i, f, n = Value{Typ: Text, S: []byte("7")}, Value{Typ: Int, I: 1<<60 + 1}, Value{Typ: Text, S: []byte("x")}
			s = Value{Typ: Int, I: 7}
		case k%223 == 0:
			i, f, s = Value{Typ: Float, F: 7.5}, Value{Typ: Text, S: []byte("7.25")}, Value{Typ: Float, F: 7}
		case k == 1000:
			i, f = Value{Typ: Float, F: 7}, Value{Typ: Blob, S: []byte("7")}
		}
		rowids = append(rowids, uint64(1+3*k))
		rows = append(rows, []Value{i, f, n, s, z})
	}
	probes := []Value{{Typ: Null}, {Typ: Int, I: 7}, {Typ: Int, I: 0}, {Typ: Int, I: 36}, {Typ: Int, I: 1 << 60}, {Typ: Int, I: 1<<60 + 1},
		{Typ: Float, F: 7}, {Typ: Float, F: 7.5}, {Typ: Float, F: 7.25}, {Typ: Float, F: 3.5}, {Typ: Float, F: 1e300}, {Typ: Float, F: math.Inf(1)},
		{Typ: Text, S: []byte("7")}, {Typ: Text, S: []byte("x")}, {Typ: Text, S: []byte("s7")}, {Typ: Text, S: []byte("7.25")},
		{Typ: Blob, S: []byte("7")}}
	phys := map[PhysicalType]bool{}
	for _, indexes := range []bool{true, false} {
		b, err := buildSegmentWith(nil, cols, rowids, rows, indexes)
		if err != nil {
			t.Fatal(err)
		}
		sg, err := openSegment(b)
		if err != nil {
			t.Fatal(err)
		}
		for c := range cols {
			phys[sg.cols[c].phys] = true
			if sg.cols[c].nullOff == 0 && !sg.hasException(c) && sg.cols[c].phys != PhysNumeric {
				t.Fatalf("column %d (%v) has no NULL and no exception", c, sg.cols[c].phys)
			}
			for _, p := range probes {
				var want []int32
				for k := range rows {
					if v := sg.Value(c, k); v.Typ != Null && p.Typ != Null && compareValues(v, p) == 0 {
						want = append(want, int32(k))
					}
				}
				got, ok := sg.seekRows(c, p, nil)
				if ok {
					var kept []int32
					for _, k := range got {
						if v := sg.Value(c, int(k)); v.Typ != Null && compareValues(v, p) == 0 {
							kept = append(kept, k)
						}
					}
					if !slices.IsSorted(got) || !slices.Equal(kept, want) {
						t.Errorf("column %d (%v), %v: rows %v, want %v among them", c, sg.cols[c].phys, p, got, want)
					}
				}
				if p.Typ == Int {
					if n, ok := sg.segEqualityCount(c, p.I); ok && n != len(want) {
						t.Errorf("column %d (%v), %v: count %d, want %d", c, sg.cols[c].phys, p, n, len(want))
					}
				}
			}
		}
	}
	for _, ph := range []PhysicalType{PhysInt64, PhysFloat64, PhysNumeric, PhysText} {
		if !phys[ph] {
			t.Errorf("no column was stored %v", ph)
		}
	}
}
