package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestTempStorePragmaRoundTrips verifies that PRAGMA temp_store gets and sets
// correctly across statements on a connection.
func TestTempStorePragmaRoundTrips(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ts.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	get := func() int {
		t.Helper()
		var v int
		if err := conn.QueryRowContext(t.Context(), `PRAGMA temp_store`).Scan(&v); err != nil {
			t.Fatalf("PRAGMA temp_store: %v", err)
		}
		return v
	}
	set := func(v string) {
		t.Helper()
		if _, err := conn.ExecContext(t.Context(), `PRAGMA temp_store=`+v); err != nil {
			t.Fatalf("PRAGMA temp_store=%s: %v", v, err)
		}
	}
	if got := get(); got != 0 {
		t.Errorf("a fresh connection reports temp_store=%d, want 0", got)
	}
	for _, tc := range []struct {
		set  string
		want int
	}{{"2", 2}, {"1", 1}, {"memory", 2}, {"file", 1}, {"0", 0}} {
		set(tc.set)
		if got := get(); got != tc.want {
			t.Errorf("after temp_store=%s the getter says %d, want %d", tc.set, got, tc.want)
		}
	}

	// The setter's own rules, which only work once the value SURVIVES between
	// statements: re-setting the value already in force is a pure no-op
	// (changeTempStorage's first branch, pragma.c), while a real change
	// DISCARDS the temp database and everything in it (invalidateTempStorage).
	set("2")
	for _, s := range []string{`CREATE TEMP TABLE tt(a)`, `INSERT INTO tt VALUES(1)`} {
		if _, err := conn.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	set("2")
	var n int
	if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM tt`).Scan(&n); err != nil {
		t.Fatalf("re-setting temp_store to the value already in force dropped the temp table: %v", err)
	}
	if n != 1 {
		t.Errorf("temp table holds %d rows after a no-op temp_store set, want 1", n)
	}
	set("1")
	if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM tt`).Scan(&n); err == nil {
		t.Error("CHANGING temp_store left the temp table behind; C SQLite discards the temp database")
	}
}
