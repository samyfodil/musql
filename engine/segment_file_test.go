package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A segment file has to survive the trip through the filesystem with its
// zero-copy property intact -- which is the entire reason it exists. A mapping
// whose segments are misaligned still ANSWERS correctly (segment.Value handles
// it), so the alignment claim has to be asserted directly or it would rot
// silently into the slow path.
func TestSegmentFileRoundTripKeepsTheFastPath(t *testing.T) {
	stmts := []string{`CREATE TABLE t(a INTEGER, b TEXT, c REAL)`, `CREATE TABLE u(x INTEGER)`}
	for i := 0; i < 2000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,'s%d',%d.25)`, i-1000, i, i))
	}
	for i := 0; i < 5; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO u VALUES(%d)`, i))
	}
	p := newSegPair(t, stmts...)
	path := p.path

	f, err := OpenSegmentFile(path)
	if err != nil {
		t.Fatalf("OpenSegmentFile: %v", err)
	}
	defer f.Close()
	if len(f.Tables()) != 2 {
		t.Fatalf("file holds %d tables, want 2", len(f.Tables()))
	}
	var tt SegmentFileTable
	for _, x := range f.Tables() {
		if x.Name == "t" {
			tt = x
		}
	}
	if tt.Name == "" || tt.Segments() == 0 {
		t.Fatal("table t did not survive the file")
	}
	if len(tt.Cols) != 3 || tt.Cols[0] != "a" {
		t.Fatalf("column names did not survive: %v", tt.Cols)
	}
	if tt.SQL == "" {
		t.Fatal("the CREATE text did not survive")
	}

	segs, err := tt.openSegments()
	if err != nil {
		t.Fatalf("openSegments: %v", err)
	}

	// THE claim: column a is a clean int64 block, so it must still be readable
	// as a Go slice straight out of the mapping.
	total := 0
	for _, s := range segs {
		kind, why := s.scanKind(0)
		if kind != segScanSlice {
			t.Fatalf("column a lost the fast path through the file: %q", why)
		}
		col, ok := s.Int64Column(0)
		if !ok {
			t.Fatal("Int64Column declined a segment read from a file")
		}
		for i := range col {
			if v := s.Value(0, i); v.I != col[i] {
				t.Fatalf("row %d: slice %d, accessor %d", i, col[i], v.I)
			}
		}
		total += s.nRows
	}
	if total != 2000 {
		t.Fatalf("segments hold %d rows, want 2000", total)
	}

	// ...and the answers match the plain loop over the same rows.
	for _, bound := range []int64{-2000, -1000, -1, 0, 500, 999, 5000} {
		want := int(p.mustPlain(`SELECT count(*) FROM t WHERE a > ?`, Value{Typ: Int, I: bound})[0][0].I)
		got := 0
		for _, s := range segs {
			got += segmentCountGreater(s, 0, bound)
		}
		if got != want {
			t.Fatalf("a > %d: file says %d, engine says %d", bound, got, want)
		}
	}
}

func TestSegmentFileRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.musq")
	if err := os.WriteFile(bad, []byte("not a segment file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSegmentFile(bad); err == nil {
		t.Error("a non-segment-file opened cleanly")
	}
	empty := filepath.Join(dir, "empty.musq")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSegmentFile(empty); err == nil {
		t.Error("an empty file opened cleanly")
	}
}
