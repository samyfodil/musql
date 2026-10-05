package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSegmentSeekAgreesWithScan verifies row seeks match scans across all delta states.
func TestSegmentSeekAgreesWithScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	sess, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Exec(`CREATE TABLE t(a INTEGER, b TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 200; i++ {
		if err := sess.Exec(fmt.Sprintf(`INSERT INTO t(rowid,a,b) VALUES(%d,%d,'v%d')`, i, i*3, i)); err != nil {
			t.Fatal(err)
		}
	}
	// Commit to write rows, then create delta state.
	if _, err := sess.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`UPDATE t SET a = 9999, b = 'updated' WHERE rowid IN (7, 63, 199)`,
		`DELETE FROM t WHERE rowid IN (5, 64, 200)`,
		`INSERT INTO t(rowid,a,b) VALUES(500,12345,'new')`,
	} {
		if err := sess.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := sess.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	// Read it back the way a query does.
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	root := uint32(0)
	schema, serr := p.Schema()
	if serr != nil {
		t.Fatal(serr)
	}
	for _, r := range schema {
		if r.Type == "table" && r.Name == "t" {
			root = r.RootPage
		}
	}
	if root == 0 {
		t.Fatal("table t has no root")
	}

	// The scan is the oracle.
	want := map[int64][]Value{}
	seq, errFn := p.ScanTable(root)
	for rid, vals := range seq {
		cp := make([]Value, len(vals))
		copy(cp, vals)
		want[int64(rid)] = cp
	}
	if e := errFn(); e != nil {
		t.Fatal(e)
	}
	if len(want) != 198 { // 200 - 3 deleted + 1 inserted
		t.Fatalf("the scan found %d rows, expected 198 -- the fixture is wrong, not the seek", len(want))
	}

	// Probe every rowid the table could hold, present or not, so a seek that
	// invents a row is caught as well as one that loses one.
	checked := 0
	for rid := int64(0); rid <= 520; rid++ {
		got, found, served := p.SeekRowidSegments(root, rid)
		if !served {
			t.Fatalf("rowid %d: the segment source declined to serve the seek", rid)
		}
		exp, expFound := want[rid]
		if found != expFound {
			t.Errorf("rowid %d: seek found=%v, scan found=%v", rid, found, expFound)
			continue
		}
		if !found {
			continue
		}
		checked++
		if len(got) != len(exp) {
			t.Errorf("rowid %d: seek gave %d columns, scan gave %d", rid, len(got), len(exp))
			continue
		}
		for c := range exp {
			if compareValues(got[c], exp[c]) != 0 || got[c].Typ != exp[c].Typ {
				t.Errorf("rowid %d col %d: seek %v, scan %v", rid, c, got[c], exp[c])
			}
		}
	}
	if checked != 198 {
		t.Errorf("compared %d rows, expected 198", checked)
	}
}

// findRowid's BOUND CHECK is what keeps a many-segment table from paying a
// binary search per segment, and getting it wrong would lose rows at a
// segment's edges. Pinned directly on the segment, at both ends and just
// outside them.
func TestSegmentFindRowidBounds(t *testing.T) {
	cols := []columnInfo{{Name: "a"}}
	rowids := []uint64{10, 20, 30, 40, 50}
	rows := [][]Value{
		{{Typ: Int, I: 1}}, {{Typ: Int, I: 2}}, {{Typ: Int, I: 3}},
		{{Typ: Int, I: 4}}, {{Typ: Int, I: 5}},
	}
	buf, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := openSegment(buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rid  int64
		want int
		hit  bool
	}{
		{9, 0, false}, {10, 0, true}, {11, 0, false},
		{30, 2, true}, {49, 0, false}, {50, 4, true}, {51, 0, false},
		{0, 0, false}, {1 << 40, 0, false},
	} {
		got, hit := sg.findRowid(tc.rid)
		if hit != tc.hit || (hit && got != tc.want) {
			t.Errorf("findRowid(%d) = (%d,%v), want (%d,%v)", tc.rid, got, hit, tc.want, tc.hit)
		}
	}
}
