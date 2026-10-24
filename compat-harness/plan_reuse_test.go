package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanReuseAcrossCommits holds prepared statements on one connection while
// a second connection to the same file commits data and schema changes, and
// compares every execution with C's. musql's plans outlive those commits (the
// session reopens and keeps its read plans), so a plan that kept anything of
// the data it was compiled over shows up here as a divergence. "SELECT *" is
// left out: C reports a statement's column count before the step that
// re-prepares it, so across an ADD COLUMN the cgo driver reads the old count.
func TestPlanReuseAcrossCommits(t *testing.T) {
	dir := t.TempDir()
	open := func(drv, path string) *sql.DB {
		t.Helper()
		db, err := sql.Open(drv, path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		return db
	}
	cPath, mPath := filepath.Join(dir, "c.db"), filepath.Join(dir, "m.musq")
	cA, mA := open("sqlite3", cPath), open("sqlite", mPath)
	cB, mB := open("sqlite3", cPath), open("sqlite", mPath)
	both := func(db1, db2 *sql.DB, stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if _, err := db1.Exec(s); err != nil {
				t.Fatalf("cgo %s: %v", s, err)
			}
			if _, err := db2.Exec(s); err != nil {
				t.Fatalf("musql %s: %v", s, err)
			}
		}
	}
	both(cA, mA,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REAL)`,
		`CREATE INDEX t_b ON t(b)`,
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO t VALUES(1, 'k1', 1.5), (2, 'k2', 2.5), (3, 'k1', 3.5)`,
		`INSERT INTO u VALUES(1, 1), (2, 2)`,
	)
	queries := []string{
		`SELECT b, c FROM t WHERE a = ?`,
		`SELECT count(*), sum(a), min(b), max(c) FROM t`,
		`SELECT a, b FROM t WHERE b BETWEEN 'k1' AND 'k3' ORDER BY b, a`,
		`SELECT t.a, u.v FROM t JOIN u ON u.k = t.a ORDER BY t.a`,
		`SELECT b, count(*) FROM t GROUP BY b ORDER BY b`,
		`SELECT a FROM t WHERE a IN (SELECT k FROM u WHERE v > 1) ORDER BY a`,
		`SELECT a, (SELECT count(*) FROM u WHERE u.k <= t.a) FROM t ORDER BY a`,
		`SELECT a FROM t WHERE b = 'k2' OR a = 3 ORDER BY a`,
		`SELECT a, sum(c) OVER (ORDER BY a ROWS 1 PRECEDING) FROM t ORDER BY a`,
		`SELECT a, b, c FROM t ORDER BY a`,
		`SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u)`,
		`SELECT k FROM u WHERE k IN (SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u WHERE v > 1)) ORDER BY k`,
	}
	type pair struct{ c, m *sql.Stmt }
	var stmts []pair
	for _, q := range queries {
		cs, err := cA.Prepare(q)
		if err != nil {
			t.Fatalf("cgo prepare %s: %v", q, err)
		}
		ms, err := mA.Prepare(q)
		if err != nil {
			t.Fatalf("musql prepare %s: %v", q, err)
		}
		t.Cleanup(func() { cs.Close(); ms.Close() })
		stmts = append(stmts, pair{cs, ms})
	}
	const upd = `UPDATE t SET c = c + 1 WHERE a IN (SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u WHERE v > 2))`
	cu, err := cA.Prepare(upd)
	if err != nil {
		t.Fatal(err)
	}
	mu, err := mA.Prepare(upd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cu.Close(); mu.Close() })
	check := func(step string) {
		t.Helper()
		for i, p := range stmts {
			var args []any
			if strings.Contains(queries[i], "?") {
				args = []any{2}
			}
			cv, mv := preparedRows(p.c, args...), preparedRows(p.m, args...)
			if cv != mv {
				t.Errorf("%s: %s\n  cgo:   %s\n  musql: %s", step, queries[i], cv, mv)
			}
		}
		if _, err := cu.Exec(); err != nil {
			t.Fatalf("cgo %s: %v", upd, err)
		}
		if _, err := mu.Exec(); err != nil {
			t.Fatalf("musql %s: %v", upd, err)
		}
	}
	check("start")
	for i, step := range []string{
		`INSERT INTO t VALUES(4, 'k3', 4.5)`,
		`INSERT INTO u VALUES(3, 3)`,
		`UPDATE t SET b = 'k2' WHERE a = 1`,
		`DELETE FROM u WHERE k = 1`,
		`INSERT INTO u SELECT a + 10, a FROM t`,
		`CREATE INDEX t_c ON t(c)`,
		`ANALYZE`,
		`ALTER TABLE t ADD COLUMN d DEFAULT 'dd'`,
		`DROP INDEX t_b`,
		`INSERT INTO t(a, b, c) VALUES(5, 'k1', 0.5)`,
	} {
		both(cB, mB, step)
		check(fmt.Sprintf("after %d: %s", i, step))
	}
}
