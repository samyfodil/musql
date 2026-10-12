package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestUnionAllStreamedParams runs an aggregate over a UNION ALL derived table
// with bound parameters, repeatedly on one prepared statement, over rows in
// segments and in the delta, and after a failing arm: the streamed arms must
// each see the bindings, restart cleanly, and leave nothing behind when one
// of them fails.
func TestUnionAllStreamedParams(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "u.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3000; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES(?, ?, ?)", i, i%10, "v"); err != nil {
			t.Fatal(err)
		}
	}
	check := func(label string) {
		t.Helper()
		st, err := db.Prepare("SELECT count(*), sum(k) FROM (SELECT k FROM t WHERE k < ? UNION ALL SELECT k FROM t WHERE k > ? UNION ALL SELECT k FROM t WHERE id = ?)")
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		for round := 0; round < 3; round++ {
			for _, c := range []struct{ lo, hi, id, n, sum int }{
				{3, 7, 5, 900 + 600 + 1, 300*(0+1+2) + 300*(8+9) + 5},
				{0, 9, 1, 0 + 0 + 1, 1},
				{10, -1, 2999, 3000 + 3000 + 1, 300*45 + 300*45 + 9},
			} {
				var n, sum int
				if err := st.QueryRow(c.lo, c.hi, c.id).Scan(&n, &sum); err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				if n != c.n || sum != c.sum {
					t.Fatalf("%s round %d %+v: got %d, %d", label, round, c, n, sum)
				}
			}
		}
	}
	check("delta")
	if _, err := db.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	check("segments")
	if _, err := db.Exec("INSERT INTO t VALUES(5000, 4, 'v')"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM (SELECT k FROM t WHERE k = 4 UNION ALL SELECT k FROM t WHERE id = 5000)").Scan(&n); err != nil || n != 301+1 {
		t.Fatalf("segments and delta: %d, %v", n, err)
	}
	if _, err := db.Exec("DELETE FROM t WHERE id = 5000"); err != nil {
		t.Fatal(err)
	}
	// A failing second arm is the statement's error, and the next statement
	// on the same connection is unaffected.
	if err := db.QueryRow("SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT json_extract('{bad', '$$') FROM t)").Scan(&n); err == nil {
		t.Log("no error from the bad arm (it was not evaluated)")
	}
	check("after failure")
}
