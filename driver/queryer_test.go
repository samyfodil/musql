package driver

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Conn implements QueryerContext and ExecerContext, so an unprepared db.Query
// or db.Exec no longer goes through a prepared driver statement, whose
// NumInput is where database/sql checked the argument count. The check, and
// its message, must not have gone with it.
func TestUnpreparedArgumentCount(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "q.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t(a, b)"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		query bool
		sql   string
		args  []any
		want  string
	}{
		{true, "SELECT ?", nil, "sql: expected 1 arguments, got 0"},
		{true, "SELECT 1", []any{1}, "sql: expected 0 arguments, got 1"},
		{false, "INSERT INTO t VALUES (?, ?)", []any{1}, "sql: expected 2 arguments, got 1"},
		{false, "INSERT INTO t VALUES (?1, ?1)", []any{1, 2}, "sql: expected 1 arguments, got 2"},
	} {
		if c.query {
			err = db.QueryRow(c.sql, c.args...).Scan(new(any))
		} else {
			_, err = db.Exec(c.sql, c.args...)
		}
		if err == nil || err.Error() != c.want {
			t.Errorf("%s %v: %v, want %q", c.sql, c.args, err, c.want)
		}
	}
	var a, b int
	if _, err := db.Exec("INSERT INTO t VALUES (?, ?)", 3, 4); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT a, b FROM t WHERE a = ?", 3).Scan(&a, &b); err != nil || a != 3 || b != 4 {
		t.Fatalf("%d %d %v", a, b, err)
	}
	if _, err := db.Exec("SELEC 1"); err == nil {
		t.Fatal("a syntax error was accepted")
	}
}
