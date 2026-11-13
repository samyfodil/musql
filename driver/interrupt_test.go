package driver_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestContextInterruptsStatement: a statement whose context ends stops at its
// next loop with C's "interrupted", which also says the context's own error,
// and the connection runs the next one.
func TestContextInterruptsStatement(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "i.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER, b TEXT)`,
		`CREATE TABLE t2(a INTEGER, b TEXT)`,
		`INSERT INTO t SELECT value, 'v' || value FROM generate_series(1, 200000)`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 3000000) SELECT sum(x) FROM c, t WHERE t.a = c.x % 7`,
		`SELECT sum(a.value * b.value) FROM generate_series(1, 100000) a, generate_series(1, 100000) b`,
		`SELECT count(*) FROM t x, t y WHERE x.a + y.a = -1`,
		`SELECT sum(length(x.b || y.b)) FROM t x, t y`,
	} {
		ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		start := time.Now()
		_, err := db.QueryContext(ctx, q)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "interrupted") || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: %v, want interrupted by the deadline", q, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: stopped after %v", q, d)
		}
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 200000 {
			t.Fatalf("after %s: %d, %v", q, n, err)
		}
	}
	ctx2, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := db.ExecContext(ctx2, `INSERT INTO t2 SELECT x.a, y.b FROM t x, t y`); err == nil || !strings.Contains(err.Error(), "interrupted") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("insert: %v, want interrupted", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM t2`).Scan(&n); err != nil || n != 0 {
		t.Errorf("after the insert: %d rows, %v", n, err)
	}
}
