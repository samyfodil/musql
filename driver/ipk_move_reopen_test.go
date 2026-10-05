package driver

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestIPKMoveSurvivesReopen: an UPDATE that moves a row to a new rowid must not
// leave the old rowid behind in the file. The segment delta recorded only the
// new rowid, so reopening the database showed the moved row twice.
func TestIPKMoveSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		"INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c'),(4,'d'),(5,'e')",
		"UPDATE t SET id=1000 WHERE id=3",
		"UPDATE t SET id=-id WHERE id<3",
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	db.Close()

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT rowid FROM t ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	want := []int64{-2, -1, 4, 5, 1000}
	if len(got) != len(want) {
		t.Fatalf("rowids after reopen = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rowids after reopen = %v, want %v", got, want)
		}
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 5 {
		t.Fatalf("count(*) after reopen = %d (%v), want 5", n, err)
	}
}
