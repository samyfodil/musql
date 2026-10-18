package engine

import (
	"bytes"
	"fmt"
	"math/rand"
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
