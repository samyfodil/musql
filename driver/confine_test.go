package driver

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfine(t *testing.T) {
	RegisterConnectionHook(func(c *Conn, dsn string) error {
		switch {
		case strings.Contains(dsn, "confine=ro"):
			c.Confine(true)
		case strings.Contains(dsn, "confine=rw"):
			c.Confine(false)
		}
		return nil
	})
	dir := t.TempDir()
	p := filepath.Join(dir, "c.musq")
	setup, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec("CREATE TABLE t(a); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	setup.Close()

	rw, err := sql.Open("sqlite", p+"?confine=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	rw.SetMaxOpenConns(1)
	if _, err := rw.Exec("INSERT INTO t VALUES (2)"); err != nil {
		t.Fatalf("confined rw insert: %v", err)
	}
	for _, q := range []string{
		"ATTACH '" + filepath.Join(dir, "x.musq") + "' AS x",
		"VACUUM INTO '" + filepath.Join(dir, "y.musq") + "'",
	} {
		if _, err := rw.Exec(q); err == nil {
			t.Errorf("%s: want refusal", q)
		}
	}

	ro, err := sql.Open("sqlite", p+"?confine=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	ro.SetMaxOpenConns(1)
	var n int
	if err := ro.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 2 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	for _, q := range []string{
		"INSERT INTO t VALUES (3)",
		"CREATE TABLE u(a)",
		"PRAGMA query_only=0",
		"DELETE FROM t",
		"PRAGMA query_only=0; INSERT INTO t VALUES (4)",
	} {
		if _, err := ro.Exec(q); err == nil {
			t.Errorf("%s: want refusal", q)
		}
	}
	if _, err := ro.Query("PRAGMA query_only=0"); err == nil {
		t.Error("query path: want refusal")
	}
	if err := ro.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 2 {
		t.Fatalf("after refused writes: n=%d err=%v", n, err)
	}
}
