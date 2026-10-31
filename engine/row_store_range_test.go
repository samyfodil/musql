package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestMaxRowidInOverSegments: the largest rowid of a range in a store over
// segments -- rows in the file's segments, in the delta, killed in the delta,
// written and dropped by the session -- is the one a walk of every row finds.
func TestMaxRowidInOverSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.musq")
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`}
	for i := 1; i <= 2000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d)`, i*3, i))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `DELETE FROM t WHERE id % 7 = 0`, `INSERT INTO t VALUES(7000, 1)`, `INSERT INTO t VALUES(14, 1)`,
		`UPDATE t SET v = 0 WHERE id % 5 = 0`)
	s, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	for _, q := range []string{`DELETE FROM t WHERE id % 11 = 0`, `INSERT INTO t VALUES(5002, 1)`, `INSERT INTO t VALUES(33, 1)`} {
		if err := s.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tbl := s.tableNamed("t")
	if err := s.ensureTableLoaded(tbl); err != nil {
		t.Fatal(err)
	}
	st := tbl.rows
	if st.seg == nil {
		t.Fatal("the store is not over segments")
	}
	rng := rand.New(rand.NewSource(1))
	for range 500 {
		lo := uint64(rng.Intn(7100) + 1)
		hi := lo + uint64(rng.Intn(400))
		var want uint64
		found := false
		for rid := range st.all() {
			if rid >= lo && rid < hi && (!found || rid > want) {
				want, found = rid, true
			}
		}
		st.rangeKnown = false
		if got, ok := st.maxRowidIn(lo, hi); ok != found || got != want {
			t.Fatalf("[%d, %d): got %d %v, want %d %v", lo, hi, got, ok, want, found)
		}
	}
}
