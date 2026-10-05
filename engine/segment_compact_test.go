package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Compaction folds a database's delta back into its segments. The truth
// each test compares against is the SAME database built with every statement
// before its segments were laid out -- no delta at all -- so a compaction that
// lost, duplicated or reordered a row answers differently from it.

// deltaFixture is table t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT) with
// rows rowids, laid out in segments, then stmts committed on top: those land in
// the delta. Rowids are SPACED (10, 20, ...) so a later insert can land strictly
// between two segment rows, which is the merge's ordering branch.
func deltaFixture(t *testing.T, rows int, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.musq")
	base := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`}
	for i := 1; i <= rows; i++ {
		base = append(base, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,'r%d')`, i*10, i*10, i))
	}
	buildDB(t, path, base...)
	execDB(t, path, `VACUUM`)
	if len(stmts) > 0 {
		execDB(t, path, stmts...)
	}
	return path
}

// noDeltaFixture is the same rows and statements with no delta: everything in
// segments.
func noDeltaFixture(t *testing.T, rows int, stmts ...string) string {
	t.Helper()
	path := deltaFixture(t, rows)
	if len(stmts) > 0 {
		execDB(t, path, stmts...)
	}
	execDB(t, path, `VACUUM`)
	return path
}

func renderT(t *testing.T, path string) string {
	t.Helper()
	out, err := queryDB(t, path, `SELECT rowid, id, k, s FROM t ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// segTableIsDirty reports whether table t has rows in the delta, which keeps its
// columnar fast paths declined.
func segTableIsDirty(t *testing.T, path string) bool {
	t.Helper()
	rp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	schema, serr := rp.Schema()
	if serr != nil {
		t.Fatal(serr)
	}
	for _, r := range schema {
		if r.Type == "table" && r.Name == "t" {
			return !rp.segCleanFor(r.RootPage)
		}
	}
	t.Fatal("table t not found in the schema")
	return false
}

func TestCompactionFoldsTheDeltaBackIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"inserts, updates and deletes", []string{
			`INSERT INTO t VALUES(35,350,'mid')`,
			`UPDATE t SET k = -1 WHERE id = 30`,
			`DELETE FROM t WHERE id = 60`,
			`INSERT INTO t VALUES(500,5000,'end')`}},
		{"every row deleted", []string{`DELETE FROM t`}},
		{"only inserts", []string{
			`INSERT INTO t VALUES(5,5,'a')`,
			`INSERT INTO t VALUES(15,15,'b')`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := deltaFixture(t, 8, tc.stmts...)
			want := renderT(t, noDeltaFixture(t, 8, tc.stmts...))
			if got := renderT(t, path); got != want {
				t.Fatalf("the merged read is already wrong; compaction is not what this test should be blaming\n got:\n%s\nwant:\n%s", got, want)
			}
			if !segTableIsDirty(t, path) {
				t.Fatal("the delta left the table clean, so compaction has nothing to fold and this test proves nothing")
			}
			did, err := CompactSegmentFile(path)
			if err != nil {
				t.Fatalf("CompactSegmentFile: %v", err)
			}
			if !did {
				t.Fatal("compaction reported no work with a non-empty delta")
			}
			if _, serr := os.Stat(segDeltaPath(path)); !os.IsNotExist(serr) {
				t.Error("the delta is still there after compaction")
			}
			if got := renderT(t, path); got != want {
				t.Errorf("compaction changed the answer\n before:\n%s\n after:\n%s", want, got)
			}
			if segTableIsDirty(t, path) {
				t.Error("the table is still reported dirty after compaction, so its columnar " +
					"fast paths stay declined -- which is the whole reason to compact")
			}
		})
	}
}

// TestCompactionPreservesTheStoredRowShape: a compacted segment stores an IPK
// column as NULL, as every other writer does -- the rowid is the row's key.
func TestCompactionPreservesTheStoredRowShape(t *testing.T) {
	path := deltaFixture(t, 6, `INSERT INTO t VALUES(35,350,'mid')`, `UPDATE t SET k = 7 WHERE id = 20`)
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	f, err := OpenSegmentFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, tb := range f.Tables() {
		if tb.IPK < 0 {
			t.Fatalf("the compacted file lost table %q's IPK column", tb.Name)
		}
		segs, serr := tb.openSegments()
		if serr != nil {
			t.Fatal(serr)
		}
		for si, sg := range segs {
			for i := 0; i < sg.nRows; i++ {
				if tb.IPK >= sg.Width(i) {
					continue
				}
				if v := sg.Value(tb.IPK, i); v.Typ != Null {
					t.Fatalf("%s segment %d row %d (rowid %d): the IPK column holds %v, "+
						"want NULL -- a stored row carries its rowid in the key, not in the column",
						tb.Name, si, i, sg.Rowid(i), v.Typ)
				}
			}
		}
	}
}

// TestCompactionIsCrashSafe: a delta left beside a file that already absorbed it
// -- a crash between the rename and the delta's removal -- is refused, because
// its tombstones would be re-applied to rowids a later insert could reuse.
func TestCompactionIsCrashSafe(t *testing.T) {
	path := deltaFixture(t, 6, `INSERT INTO t VALUES(35,350,'mid')`)
	delta, err := os.ReadFile(segDeltaPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(segDeltaPath(path), delta, 0o644); werr != nil {
		t.Fatal(werr)
	}
	if rp, oerr := Open(path); oerr == nil {
		rp.Close()
		t.Error("a delta left beside the file that already absorbed it was accepted")
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), filepath.Base(path)+".compact-") {
			t.Errorf("a temporary compaction file was left behind: %s", e.Name())
		}
	}
}

// TestCompactionEmptyDeltaIsNotWork pins the cheap case: nothing to fold means
// nothing is rewritten.
func TestCompactionEmptyDeltaIsNotWork(t *testing.T) {
	path := deltaFixture(t, 4)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	did, err := CompactSegmentFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if did {
		t.Error("compaction reported work with no delta at all")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("compaction rewrote the segment file with no delta to fold")
	}
}
