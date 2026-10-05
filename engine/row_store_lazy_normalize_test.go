package engine

import (
	"path/filepath"
	"testing"
)

// TestWriteScanNormalizesEachRow verifies write scans normalize rows lazily.
func TestWriteScanNormalizesEachRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.musq")
	sess, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, r REAL, k INTEGER, v)`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<1000) INSERT INTO t SELECT x, x, x % 10, NULL FROM c`,
	} {
		if e := sess.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if sess, err = OpenWrite(path); err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	if e := sess.Exec(`UPDATE t SET v = id * 2 || ':' || typeof(r) WHERE k = 3`); e != nil {
		t.Fatal(e)
	}
	_, rows, qerr := sess.Query(`SELECT count(*), min(v), max(v) FROM t WHERE k = 3 AND v = id * 2 || ':real'`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 1 || rows[0][0].I != 100 {
		_, got, _ := sess.Query(`SELECT id, v FROM t WHERE k = 3 LIMIT 3`, nil)
		t.Fatalf("UPDATE read id/typeof(r) wrong: %v matched, sample %v", rows, got)
	}
	tbl := sess.findTableMeta("t")
	for _, rid := range []uint64{3, 13, 993} {
		if row := tbl.rows.row(rid); row[0].Typ != Null {
			t.Errorf("row %d's stored IPK slot became %v", rid, row[0])
		}
	}
}
