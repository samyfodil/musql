package engine

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"testing"
)

// TestSegStreamMatchesBuildSegment: a segment streamed a row at a time is the
// one buildSegmentWith lays out from the same rows without indexes, byte for
// byte -- over every column shape the census can pick, mixed columns that fall
// to exceptions, NULLs, intReal floats, dense and sparse rowids -- and stays
// so after a truncate.
func TestSegStreamMatchesBuildSegment(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	val := func(kind int) Value {
		switch kind {
		case 0:
			return Value{Typ: Null}
		case 1:
			return Value{Typ: Int, I: rng.Int63n(1<<40) - 1<<39}
		case 2:
			f := float64(rng.Intn(1000)) / 7
			return Value{Typ: Float, F: f}
		case 3:
			return Value{Typ: Float, F: float64(rng.Intn(50)), intReal: true}
		case 4:
			return Value{Typ: Text, S: []byte(fmt.Sprintf("t%d", rng.Intn(1e6)))}
		default:
			b := make([]byte, rng.Intn(9))
			rng.Read(b)
			return Value{Typ: Blob, S: b}
		}
	}
	for trial := 0; trial < 300; trial++ {
		nCols := 1 + rng.Intn(6)
		nRows := 1 + rng.Intn(300)
		if trial%20 == 0 {
			nRows = streamChunk - 2 + rng.Intn(2*streamChunk+5) // across chunk boundaries
		}
		cols := make([]columnInfo, nCols)
		// Each column mostly one kind, sometimes NULLs, sometimes strays.
		main := make([]int, nCols)
		for c := range cols {
			cols[c] = columnInfo{Name: fmt.Sprintf("c%d", c)}
			main[c] = rng.Intn(6)
		}
		stray := rng.Intn(4)
		dense := rng.Intn(2) == 0
		var rowids []uint64
		var rows [][]Value
		rid := uint64(rng.Int63n(1 << 30))
		st := newSegStream(nCols)
		for i := 0; i < nRows; i++ {
			if dense {
				rid++
			} else {
				rid += 1 + uint64(rng.Intn(3))
			}
			row := make([]Value, nCols)
			for c := range row {
				k := main[c]
				switch r := rng.Intn(20); {
				case r == 0 && stray > 0:
					k = rng.Intn(6)
				case r == 1 && stray > 1:
					k = 0
				}
				row[c] = val(k)
			}
			rowids, rows = append(rowids, rid), append(rows, row)
			if !st.add(rid, row) {
				t.Fatal("a full-width row refused")
			}
		}
		check := func(what string, n int) {
			t.Helper()
			want, err := buildSegmentWith(nil, cols, rowids[:n], rows[:n], false)
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.finish()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("trial %d %s (%d rows x %d cols): streamed segment differs from buildSegmentWith", trial, what, n, nCols)
			}
			if _, err := openSegment(got); err != nil {
				t.Fatalf("trial %d %s: %v", trial, what, err)
			}
		}
		check("whole", nRows)
		if nRows > 1 {
			k := 1 + rng.Intn(nRows-1)
			st.truncate(k)
			check("truncated", k)
		}
	}
}

// TestLazyColumnIndexMatchesFile: a segment written without its indexes (an
// append load's) answers every equality lookup exactly as the same rows'
// segment with indexes in the file does -- integer, REAL and TEXT columns, a
// column with NULLs (no index either way) -- and an index-less segment holding
// a short row answers from none, since its cell is not what the row reads as.
func TestLazyColumnIndexMatchesFile(t *testing.T) {
	cols := []columnInfo{{Name: "k"}, {Name: "f"}, {Name: "s"}, {Name: "n"}}
	var rowids []uint64
	var rows [][]Value
	for i := 0; i < 5000; i++ {
		n := Value{Typ: Int, I: int64(i % 3)}
		if i%7 == 0 {
			n = Value{Typ: Null}
		}
		rowids = append(rowids, uint64(10+2*i))
		rows = append(rows, []Value{
			{Typ: Int, I: int64(i % 37)},
			{Typ: Float, F: float64(i%11) / 4},
			{Typ: Text, S: []byte(fmt.Sprintf("s%d", i%53))},
			n,
		})
	}
	open := func(indexes bool) *segment {
		b, err := buildSegmentWith(nil, cols, rowids, rows, indexes)
		if err != nil {
			t.Fatal(err)
		}
		sg, err := openSegment(b)
		if err != nil {
			t.Fatal(err)
		}
		return sg
	}
	file, lazy := open(true), open(false)
	for c := range cols {
		if lazy.cols[c].idxOff != 0 {
			t.Fatalf("column %d: an index-less build wrote an index", c)
		}
	}
	for v := int64(-1); v < 40; v++ {
		want, okW := file.segEqualityRows(0, v)
		got, okG := lazy.segEqualityRows(0, v)
		if !okW || !okG || !slices.Equal(want, got) {
			t.Fatalf("k = %d: file %v %v, lazy %v %v", v, okW, want, okG, got)
		}
		fw, okW := file.segEqualityRowsFloat(1, float64(v)/4)
		fg, okG := lazy.segEqualityRowsFloat(1, float64(v)/4)
		if okW != okG || !slices.Equal(fw, fg) {
			t.Fatalf("f = %v: file %v %v, lazy %v %v", float64(v)/4, okW, fw, okG, fg)
		}
		p := []byte(fmt.Sprintf("s%d", v))
		sw, okW := file.segEqualityRowsBytes(2, p)
		sgot, okG := lazy.segEqualityRowsBytes(2, p)
		if okW != okG || !slices.Equal(sw, sgot) {
			t.Fatalf("s = %s: file %v %v, lazy %v %v", p, okW, sw, okG, sgot)
		}
		_, okW = file.segEqualityRows(3, v)
		_, okG = lazy.segEqualityRows(3, v)
		if okW || okG {
			t.Fatal("a column with NULLs answered from an index")
		}
	}

	short := append([][]Value(nil), rows...)
	short[100] = short[100][:0:0] // reads k as its DEFAULT, whatever the cell holds
	b, err := buildSegmentWith(nil, cols, rowids, short, false)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := openSegment(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sg.segEqualityRows(0, 0); ok {
		t.Fatal("a segment with a short row built an index from its cells")
	}
}
