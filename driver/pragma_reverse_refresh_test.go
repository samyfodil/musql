package driver

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// "PRAGMA reverse_unordered_selects" is per CONNECTION and survives another
// connection's commit, which rebuilds this connection's held session
// (Session.reopen) from the file: the session the next write compiles in has to
// hear it again, from the stamp in openWriteOrCreatePath. The UPDATE's SET reads
// the rows before it, so the direction C walks (backward under the pragma,
// where.c:7126) decides the values: 1,3,6,10,15,21 -- forward would store
// 1,3,7,15,31,63.
func TestReverseUnorderedSurvivesAnotherConnectionsCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	a, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, n)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,4),(5,5)`,
		`PRAGMA reverse_unordered_selects=1`,
	} {
		if _, err := a.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	b, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Exec(`INSERT INTO t VALUES(6,6)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(`UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <= t.id)`); err != nil {
		t.Fatal(err)
	}
	rows, err := a.Query(`SELECT n FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var n int64
		rows.Scan(&n)
		got = append(got, n)
	}
	want := []int64{1, 3, 6, 10, 15, 21}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
