package engine

import (
	"fmt"
	"slices"
	"testing"
)

// TestSegmentReadAgreesWithBTree verifies segment and b-tree readers agree.
func TestSegmentReadAgreesWithBTree(t *testing.T) {
	schema := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b TEXT, c REAL, d BLOB, e)`,
		`CREATE TABLE small(x)`,
	}
	var rows []string
	for i := 0; i < 700; i++ {
		a := fmt.Sprintf("%d", i-350)
		if i%13 == 0 {
			a = "NULL"
		}
		e := fmt.Sprintf("%d", i)
		switch {
		case i == 77:
			e = "'a text in a number column'"
		case i%19 == 0:
			e = "NULL"
		}
		rows = append(rows, fmt.Sprintf(`INSERT INTO t VALUES(%d,%s,'b%d',%d.5,x'%02x%02x',%s)`,
			i+1, a, i, i, byte(i), byte(i>>8), e))
	}
	for i := 0; i < 3; i++ {
		rows = append(rows, fmt.Sprintf(`INSERT INTO small VALUES(%d)`, i))
	}
	// The SAME rows twice: as column blocks (VACUUMed into segments), and as the
	// delta's plain records (written after an empty table was laid out). The
	// second is the reference: no segment column is decoded for it.
	seg := newSegPair(t, append(slices.Clone(schema), rows...)...)
	ref := newSegPair(t, schema...)
	ref.delta(rows...)

	queries := []string{
		`SELECT count(*) FROM t`,
		`SELECT count(*) FROM t WHERE a > 0`,
		`SELECT count(*) FROM t WHERE a IS NULL`,
		`SELECT count(*) FROM t WHERE e < 100`,
		`SELECT count(*) FROM t WHERE e <> 5`,
		`SELECT sum(a), min(a), max(a) FROM t`,
		`SELECT id, a, b, c, d, e FROM t ORDER BY id LIMIT 5`,
		`SELECT id, a FROM t ORDER BY a DESC, id LIMIT 8`,
		`SELECT b, count(*) FROM t GROUP BY b ORDER BY b LIMIT 6`,
		`SELECT typeof(a), typeof(b), typeof(c), typeof(d), typeof(e) FROM t ORDER BY id LIMIT 3`,
		`SELECT count(*) FROM t WHERE b LIKE 'b1%'`,
		`SELECT rowid, id FROM t WHERE rowid IN (1, 78, 700)`,
		`SELECT count(*) FROM t, small`,
		`SELECT x FROM small ORDER BY x`,
		`SELECT total(c) FROM t`,
	}
	open := func(path string) *ReadOnlyPager {
		rp, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rp.Close() })
		return rp
	}
	want := make([][]string, len(queries))
	refRP := open(ref.path)
	for i, q := range queries {
		want[i] = renderQuery(t, refRP, q)
	}
	segRP := open(seg.path)
	served := 0
	rowsSchema, err := segRP.Schema()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rowsSchema {
		if r.Type == "table" && segRP.ScannedFromSegments(r.RootPage) {
			served++
		}
	}
	if served != 2 {
		t.Fatalf("%d tables are served from segments, want 2 -- this test would otherwise be comparing the delta with itself", served)
	}
	for _, mode := range []bool{false, true} {
		undo := ForceSegRowSourceForTest(mode) // the cursor's lazy segment rows, and the store's
		for i, q := range queries {
			got := renderQuery(t, segRP, q)
			if len(got) != len(want[i]) {
				t.Fatalf("lazy=%v %s: %d rows from segments, %d from the delta", mode, q, len(got), len(want[i]))
			}
			for r := range got {
				if got[r] != want[i][r] {
					t.Fatalf("lazy=%v %s row %d:\n segments %s\n delta    %s", mode, q, r, got[r], want[i][r])
				}
			}
		}
		undo()
	}
}

func renderQuery(t *testing.T, rp *ReadOnlyPager, q string) []string {
	t.Helper()
	_, rows, err := rp.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		s := ""
		for _, v := range r {
			s += fmt.Sprintf("%d:", v.Typ)
			switch v.Typ {
			case Int:
				s += fmt.Sprintf("%d|", v.I)
			case Float:
				s += fmt.Sprintf("%v|", v.F)
			case Text, Blob:
				s += fmt.Sprintf("%q|", v.S)
			default:
				s += "null|"
			}
		}
		out = append(out, s)
	}
	return out
}
