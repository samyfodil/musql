package engine

import (
	"math/rand"
	"testing"
)

// The equality index must agree with the scan it replaces, for every value --
// including ones absent from the column, which is where an index that returned
// a zero-value map entry as "no answer" rather than "zero rows" would differ.
//
// This is the only check that the index is not simply wrong: it answers
// count(*) WHERE col = ? for real queries, and a count that is confidently
// wrong is exactly the failure this repo fears most.
func TestEqualityIndexMatchesTheScan(t *testing.T) {
	cols := []columnInfo{{Name: "a"}, {Name: "b"}}
	rng := rand.New(rand.NewSource(99))
	const n = 3000
	rows := make([][]Value, n)
	rowids := make([]uint64, n)
	for i := range rows {
		// b is low-cardinality (many duplicates), a is near-unique: the index
		// has to be right for both shapes.
		rows[i] = []Value{{Typ: Int, I: int64(rng.Intn(n * 3))}, {Typ: Int, I: int64(rng.Intn(7))}}
		rowids[i] = uint64(i + 1)
	}
	raw, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	seg, err := openSegment(raw)
	if err != nil {
		t.Fatal(err)
	}

	for _, col := range []int{0, 1} {
		for _, v := range []int64{-1, 0, 1, 3, 6, 7, 1000, int64(n * 3), int64(n*3 + 1)} {
			preds := []segPred{{Col: col, Op: segEQ, Val: Value{Typ: Int, I: v}}}
			wantScan := segFilterCount(seg, preds)
			got, ok := seg.segEqualityCount(col, v)
			if !ok {
				t.Fatalf("col %d: the index declined a clean int64 column", col)
			}
			if got != wantScan {
				t.Errorf("col %d = %d: index %d, scan %d", col, v, got, wantScan)
			}
		}
	}

	// A column the index cannot handle must DECLINE rather than answer zero --
	// answering zero would be a wrong count that looks like an empty result.
	textCols := []columnInfo{{Name: "s"}}
	trows := [][]Value{{{Typ: Text, S: []byte("x")}}, {{Typ: Text, S: []byte("y")}}}
	traw, err := buildSegment(textCols, []uint64{1, 2}, trows)
	if err != nil {
		t.Fatal(err)
	}
	tseg, err := openSegment(traw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tseg.segEqualityCount(0, 0); ok {
		t.Error("the index claimed a TEXT column")
	}
}
