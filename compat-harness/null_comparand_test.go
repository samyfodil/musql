package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	mush "github.com/samyfodil/musql/driver"
)

// A NULL comparand, checked against the oracle. "col = NULL" must match
// nothing, while "x NOT BETWEEN NULL AND y" matches every row with x > y.
// Expected values come from C, not hand-derived three-valued logic.
func TestNullComparandMatchesCSQLite(t *testing.T) {
	dir := t.TempDir()
	queries := []string{
		`SELECT COUNT(*) FROM tab0 WHERE NOT - ( 3 ) + col4 NOT BETWEEN NULL AND col0`,
		`SELECT COUNT(*) FROM tab0 WHERE col4 BETWEEN NULL AND col0`,
		`SELECT COUNT(*) FROM tab0 WHERE col4 NOT BETWEEN NULL AND col0`,
		`SELECT COUNT(*) FROM tab0 WHERE col4 < NULL`,
		`SELECT COUNT(*) FROM tab0 WHERE NOT (col4 < NULL)`,
		`SELECT COUNT(*) FROM tab0 WHERE col4 = NULL`,
		`SELECT COUNT(*) FROM tab0 WHERE col4 > NULL OR col0 < NULL`,
		`SELECT COUNT(*) FROM tab0 WHERE col4 NOT BETWEEN col0 AND NULL`,
		`SELECT COUNT(*) FROM tab0 WHERE NOT (col4 NOT BETWEEN NULL AND col0)`,
	}
	got := map[string][]int64{}
	for _, e := range []struct{ label, drv string }{{"musql", mush.DriverName}, {"C", "sqlite3"}} {
		db, err := sql.Open(e.drv, filepath.Join(dir, e.label+".db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`CREATE TABLE tab0(pk INTEGER PRIMARY KEY, col0 INTEGER, col4 INTEGER)`); err != nil {
			t.Fatal(err)
		}
		tx, _ := db.Begin()
		st, _ := tx.Prepare(`INSERT INTO tab0 VALUES(?,?,?)`)
		for i := 0; i < 1000; i++ {
			st.Exec(i, i*7%1000, i*13%1000)
		}
		st.Close()
		tx.Commit()
		for _, q := range queries {
			var n int64
			if err := db.QueryRow(q).Scan(&n); err != nil {
				t.Fatalf("%s %s: %v", e.label, q, err)
			}
			got[e.label] = append(got[e.label], n)
		}
		db.Close()
	}
	bad := 0
	for i, q := range queries {
		m, c := got["musql"][i], got["C"][i]
		mark := "ok   "
		if m != c {
			mark, bad = "WRONG", bad+1
		}
		t.Logf("%s musql=%-5d C=%-5d  %s", mark, m, c, q)
	}
	if len(got["musql"]) != len(queries) {
		t.Fatalf("only %d of %d ran", len(got["musql"]), len(queries))
	}
	if bad != 0 {
		t.Errorf("%d of %d queries disagree with C SQLite", bad, len(queries))
	}
	fmt.Print("")
}
