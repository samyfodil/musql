package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func authConn(t *testing.T) (*sql.Conn, *Conn) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var c *Conn
	if err := conn.Raw(func(dc any) error { c = dc.(*Conn); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)`,
		`INSERT INTO t VALUES (1, 'x', 10), (2, 'y', 20)`,
	} {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	return conn, c
}

func TestAuthorizerCalls(t *testing.T) {
	conn, c := authConn(t)
	var calls []string
	c.SetAuthorizer(func(action int, a1, a2, db, trig string) int {
		calls = append(calls, fmt.Sprintf("%d %s %s %s", action, a1, a2, db))
		return engine.AuthOK
	})
	var n int
	if err := conn.QueryRowContext(context.Background(), `SELECT count(*) FROM t WHERE c > ?`, 5).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count %d, %v", n, err)
	}
	want := []string{"21   ", "31  count ", "20 t c main"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls %q, want %q", calls, want)
	}
}

func TestAuthorizerDeny(t *testing.T) {
	ctx := context.Background()
	conn, c := authConn(t)
	c.SetAuthorizer(func(action int, a1, a2, db, trig string) int {
		if action == engine.AuthRead && a2 == "c" || action == engine.AuthInsert {
			return engine.AuthDeny
		}
		return engine.AuthOK
	})
	_, err := conn.QueryContext(ctx, `SELECT c FROM t`)
	if err == nil || err.Error() != "access to t.c is prohibited" || !errors.Is(err, engine.ErrAuth) {
		t.Fatalf("read: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO t(b) VALUES (1)`); err == nil || err.Error() != "not authorized" {
		t.Fatalf("insert: %v", err)
	}
	var b string
	if err := conn.QueryRowContext(ctx, `SELECT b FROM t WHERE a = 1`).Scan(&b); err != nil || b != "x" {
		t.Fatalf("allowed read %q, %v", b, err)
	}
	// A statement prepared under one authorizer is authorized again under the next.
	st, err := conn.PrepareContext(ctx, `SELECT c FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.QueryContext(ctx); err == nil {
		t.Fatal("prepared read allowed")
	}
	c.SetAuthorizer(nil)
	rows, err := st.QueryContext(ctx)
	if err != nil {
		t.Fatalf("after removing: %v", err)
	}
	rows.Close()
}

func TestAuthorizerIgnore(t *testing.T) {
	ctx := context.Background()
	conn, c := authConn(t)
	c.SetAuthorizer(func(action int, a1, a2, db, trig string) int {
		switch {
		case action == engine.AuthRead && a2 == "c", action == engine.AuthFunction && a2 == "upper":
			return engine.AuthIgnore
		case action == engine.AuthInsert:
			return engine.AuthIgnore
		}
		return engine.AuthOK
	})
	rows, err := conn.QueryContext(ctx, `SELECT b, c, upper(b), * FROM t WHERE a = 1`)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	if !slices.Equal(cols, []string{"b", "c", "upper(b)", "a", "b", "c"}) {
		t.Fatalf("columns %q", cols)
	}
	var b, cc, up, a, b2, c2 any
	if !rows.Next() {
		t.Fatal("no row")
	}
	if err := rows.Scan(&b, &cc, &up, &a, &b2, &c2); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if fmt.Sprintf("%v %v %v %v %v %v", b, cc, up, a, b2, c2) != "x <nil> <nil> 1 x <nil>" {
		t.Fatalf("row %v %v %v %v %v %v", b, cc, up, a, b2, c2)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO t(b) VALUES ('z')`); err != nil {
		t.Fatal(err)
	}
	c.SetAuthorizer(nil)
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("an ignored INSERT inserted: %d, %v", n, err)
	}
}
