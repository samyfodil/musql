package engine

import (
	"path/filepath"
	"testing"
)

// TestReadLeavesStoredRowsInStoredShape verifies reads don't mutate the row store.
func TestReadLeavesStoredRowsInStoredShape(t *testing.T) {
	sess, err := Create(filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, r REAL, v INTEGER)`,
		`CREATE INDEX tv ON t(v)`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<4000) INSERT INTO t SELECT x, x, x FROM c`,
	} {
		if e := sess.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	tbl := sess.findTableMeta("t")
	loaded := map[uint64]Value{}
	for _, rid := range []uint64{1, 7, 4000} {
		loaded[rid] = tbl.rows.row(rid)[1]
	}
	stored := func(what string) {
		t.Helper()
		for _, rid := range []uint64{1, 7, 4000} {
			row := tbl.rows.row(rid)
			if len(row) != 3 {
				t.Fatalf("%s: row %d is %v", what, rid, row)
			}
			if row[0].Typ != Null {
				t.Errorf("%s: row %d's IPK slot is %v, want the stored NULL", what, rid, row[0])
			}
			if row[1].Typ != loaded[rid].Typ || row[1].F != loaded[rid].F || row[1].I != loaded[rid].I {
				t.Errorf("%s: row %d's REAL column is %v, want it as stored, %v", what, rid, row[1], loaded[rid])
			}
		}
	}
	stored("after the load")
	for _, q := range []struct {
		sql  string
		args []Value
	}{
		{`SELECT * FROM t`, nil},                                   // full scan
		{`SELECT * FROM t WHERE id = ?`, []Value{{Typ: Int, I: 7}}}, // rowid seek
		{`SELECT * FROM t WHERE v = ?`, []Value{{Typ: Int, I: 4000}}},
	} {
		_, rows, qerr := sess.Query(q.sql, q.args)
		if qerr != nil {
			t.Fatalf("%s: %v", q.sql, qerr)
		}
		if len(rows) == 0 || rows[0][0].Typ != Int || rows[0][1].Typ != Float {
			t.Fatalf("%s: read %v, want the normalized row", q.sql, rows)
		}
		stored("after " + q.sql)
	}
}
