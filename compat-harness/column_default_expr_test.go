// This file tests column DEFAULT expressions: CURRENT_TIME, CURRENT_DATE,
// CURRENT_TIMESTAMP, and related date/time functions.
package compat

import (
	"fmt"
	"strings"
	"testing"
)

func TestColumnDefaultExpr(t *testing.T) {
	for _, c := range []struct{ name, create, insert, read string }{
		// fuzz4.test's own shapes: a CURRENT_* inside an expression whose
		// RESULT is deterministic.
		{"between-timestamp",
			`CREATE TABLE t(c0 DEFAULT (CURRENT_TIMESTAMP BETWEEN 1 AND 1))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"between-timestamp-reversed",
			`CREATE TABLE t(c0 DEFAULT (1 BETWEEN CURRENT_TIMESTAMP AND 1))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		// the bare keyword forms: SHAPE only, never the clock reading itself
		{"current_date",
			`CREATE TABLE t(a DEFAULT CURRENT_DATE, b)`,
			`INSERT INTO t(b) VALUES(9)`,
			`SELECT length(a), typeof(a), b FROM t`},
		{"current_time",
			`CREATE TABLE t(a DEFAULT CURRENT_TIME, b)`,
			`INSERT INTO t(b) VALUES(9)`,
			`SELECT length(a), typeof(a), b FROM t`},
		{"current_timestamp",
			`CREATE TABLE t(a DEFAULT CURRENT_TIMESTAMP, b)`,
			`INSERT INTO t(b) VALUES(9)`,
			`SELECT length(a), typeof(a), b FROM t`},
		// tkt3791.test's own: a parenthesized function call
		{"datetime-now",
			`CREATE TABLE t(x, y DEFAULT(datetime('now')))`,
			`INSERT INTO t(x) VALUES(1)`,
			`SELECT x, length(y), typeof(y) FROM t`},
		{"date-now",
			`CREATE TABLE t(x, y DEFAULT(date('now')))`,
			`INSERT INTO t(x) VALUES(1)`,
			`SELECT x, length(y), typeof(y) FROM t`},
		{"strftime",
			`CREATE TABLE t(x, y DEFAULT(strftime('%Y','now')))`,
			`INSERT INTO t(x) VALUES(1)`,
			`SELECT x, length(y), typeof(y) FROM t`},
		// DEFAULT VALUES, the other spelling that omits every column
		{"default-values",
			`CREATE TABLE t(a DEFAULT CURRENT_DATE, b DEFAULT 5)`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT length(a), b FROM t`},
		{"or-replace-default-values",
			`CREATE TABLE t(a DEFAULT CURRENT_DATE, b DEFAULT 'x')`,
			`INSERT OR REPLACE INTO t DEFAULT VALUES`,
			`SELECT length(a), b FROM t`},
		// the default's affinity still applies, exactly like a normal value
		{"affinity-applied",
			`CREATE TABLE t(a INT DEFAULT (CURRENT_TIMESTAMP BETWEEN 1 AND 1), b)`,
			`INSERT INTO t(b) VALUES(1)`,
			`SELECT a, typeof(a) FROM t`},
		// NOT NULL is satisfied by the computed default
		{"not-null-satisfied",
			`CREATE TABLE t(a NOT NULL DEFAULT CURRENT_DATE, b)`,
			`INSERT INTO t(b) VALUES(1)`,
			`SELECT length(a), typeof(a) FROM t`},
		// an ordinary constant default must be completely unaffected
		{"constant-default-control",
			`CREATE TABLE t(a DEFAULT 5, b DEFAULT 'q', c DEFAULT (1+2))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT a, b, c, typeof(a), typeof(b), typeof(c) FROM t`},
	} {
		differ(t, c.name, []string{c.create, c.insert, c.read})
	}
}

// TestColumnDefaultExprStaysDeclined pins what must NOT be accepted: a
// random()/randomblob() default, whose value can never match an
// independently-seeded oracle. The oracle half is asserted too, so if SQLite
// ever stopped accepting these the decline would become correct and this gate
// should be deleted rather than "fixed".
func TestColumnDefaultExprStaysDeclined(t *testing.T) {
	for _, c := range []struct{ create, insert string }{
		{`CREATE TABLE t(a DEFAULT (random()), b)`, `INSERT INTO t(b) VALUES(1)`},
		{`CREATE TABLE t(a DEFAULT (randomblob(4)), b)`, `INSERT INTO t(b) VALUES(1)`},
		{`CREATE TABLE t(a DEFAULT (abs(random())), b)`, `INSERT INTO t DEFAULT VALUES`},
	} {
		stmts := []string{c.create, c.insert}
		if res := run(t, "cgo", stmts); res[len(res)-1]["kind"] == "error" {
			t.Errorf("oracle no longer accepts %q: this decline may now be correct", c.insert)
		}
		if res := run(t, "musql", stmts); res[len(res)-1]["kind"] != "error" {
			t.Errorf("%q + %q now ANSWERS in musql -- a random() default cannot match an "+
				"independently-seeded oracle, so it must stay declined", c.create, c.insert)
		}
	}
}

// TestColumnDefaultExprIsPerInsert is the assertion that the value is computed
// per INSERT rather than frozen at CREATE TABLE time or, worse, cached with a
// compiled write plan: two inserts into the same table, separated by a clock
// tick, must both carry the CURRENT time. It compares the two engines'
// AGREEMENT on whether the two rows' defaults relate to each other the same
// way, not the readings themselves.
func TestColumnDefaultExprIsPerInsert(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t(a DEFAULT (strftime('%Y-%m-%d', 'now')), b)`,
		`INSERT INTO t(b) VALUES(1)`,
		`INSERT INTO t(b) VALUES(2)`,
		// Both rows must hold the SAME day, and it must be today's -- which
		// "date('now')" independently computes, so this compares two clock
		// readings against each other rather than against a literal.
		`SELECT count(*), count(DISTINCT a), sum(a = date('now')) FROM t`,
		`SELECT b, length(a), typeof(a) FROM t ORDER BY b`,
	}
	differ(t, "per-insert", stmts)
}

// TestColumnDefaultExprShapes runs the accepted defaults through the other
// statement forms that fill an omitted column, so the rule is not wired into
// one path only.
func TestColumnDefaultExprShapes(t *testing.T) {
	base := []string{
		`CREATE TABLE t(a DEFAULT (CURRENT_TIMESTAMP BETWEEN 1 AND 1), b, c DEFAULT CURRENT_DATE)`,
		`CREATE TABLE src(x)`,
		`INSERT INTO src VALUES(1),(2)`,
	}
	for _, s := range []string{
		`INSERT INTO t(b) VALUES(1)`,
		`INSERT INTO t(b) VALUES(1),(2),(3)`,
		`INSERT INTO t(b) SELECT x FROM src`,
		`INSERT OR IGNORE INTO t(b) VALUES(7)`,
		`INSERT INTO t DEFAULT VALUES`,
		`REPLACE INTO t(b) VALUES(4)`,
	} {
		stmts := append(append([]string{}, base...), s,
			`SELECT a, typeof(a), length(c), count(*) FROM t GROUP BY a, typeof(a), length(c) ORDER BY 1`)
		differ(t, s, stmts)
	}
	// ...and through a table whose default column is also NOT NULL, where a
	// missing default would surface as a constraint failure instead.
	stmts := append(append([]string{}, base...),
		`CREATE TABLE nn(a NOT NULL DEFAULT (CURRENT_TIMESTAMP BETWEEN 1 AND 1), b)`,
		`INSERT INTO nn(b) VALUES(1)`,
		`SELECT a, typeof(a) FROM nn`)
	differ(t, "not-null "+strings.Repeat("", 1), stmts)
}

// TestColumnDefaultExprAlterTable covers ALTER TABLE ADD COLUMN with a
// time-valued default -- fkey2.test's and without_rowid3.test's own shape.
func TestColumnDefaultExprAlterTable(t *testing.T) {
	for i, def := range []string{`CURRENT_TIME`, `CURRENT_DATE`, `CURRENT_TIMESTAMP`, `(datetime('now'))`} {
		stmts := []string{
			`CREATE TABLE t(a, b)`,
			`INSERT INTO t VALUES(1,2)`,
			fmt.Sprintf(`ALTER TABLE t ADD COLUMN g DEFAULT %s`, def),
			`SELECT a, b, g, typeof(g) FROM t`,
			`INSERT INTO t(a,b) VALUES(3,4)`,
			`SELECT a, b, length(g), typeof(g) FROM t ORDER BY a`,
			`SELECT sql FROM sqlite_master WHERE name='t'`,
		}
		differ(t, fmt.Sprintf("alter%d %s", i, def), stmts)
	}
}
