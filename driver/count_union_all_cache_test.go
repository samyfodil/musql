package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestCountUnionAllPlanCached runs the same count(*) over a UNION ALL text
// repeatedly across writes, a schema change and different bound values: the
// plan kept for the text must follow the data and the bindings.
func TestCountUnionAllPlanCached(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "c.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 100; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES(?, ?)", i, i%10); err != nil {
			t.Fatal(err)
		}
	}
	const q = "SELECT count(*) FROM (SELECT k FROM t WHERE k < ? UNION ALL SELECT k FROM t WHERE k > 7)"
	count := func(lo, want int) {
		t.Helper()
		for range 3 {
			var n int
			if err := db.QueryRow(q, lo).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != want {
				t.Fatalf("lo=%d: got %d, want %d", lo, n, want)
			}
		}
	}
	count(3, 30+20)
	count(1, 10+20)
	if _, err := db.Exec("INSERT INTO t VALUES(1000, 9)"); err != nil {
		t.Fatal(err)
	}
	count(3, 30+21)
	if _, err := db.Exec("DROP TABLE t"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(q, 3).Scan(new(int)); err == nil {
		t.Fatal("count over a dropped table succeeded")
	}
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t VALUES(1, 0)"); err != nil {
		t.Fatal(err)
	}
	count(3, 1)
}
