// This file tests DEFAULT clause IS/ISNOT truth-value tests.
// DEFAULT clauses must use literal comparison semantics, not content-based truthiness.
package compat

import "testing"

// TestDefaultIsBoolTruthTest covers all four IS-TRUE-family forms with various operand types.
func TestDefaultIsBoolTruthTest(t *testing.T) {
	for _, c := range []struct{ name, create, insert, read string }{
		{"clock-is-not-false",
			`CREATE TABLE t(c0 DEFAULT (time('now','start of day') IS NOT FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"clock-is-true",
			`CREATE TABLE t(c0 DEFAULT (time('now','start of day') IS TRUE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"clock-is-not-true",
			`CREATE TABLE t(c0 DEFAULT (time('now','start of day') IS NOT TRUE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"clock-is-false",
			`CREATE TABLE t(c0 DEFAULT (time('now','start of day') IS FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},

		{"text-literal-is-not-false",
			`CREATE TABLE t(c0 DEFAULT ('00:00:00' IS NOT FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"text-literal-is-true",
			`CREATE TABLE t(c0 DEFAULT ('00:00:00' IS TRUE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"text-literal-is-not-true",
			`CREATE TABLE t(c0 DEFAULT ('00:00:00' IS NOT TRUE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"text-literal-is-false",
			`CREATE TABLE t(c0 DEFAULT ('00:00:00' IS FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"text-literal-truthy-is-not-false",
			`CREATE TABLE t(c0 DEFAULT ('01:00:00' IS NOT FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"text-literal-truthy-is-false",
			`CREATE TABLE t(c0 DEFAULT ('01:00:00' IS FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},

		{"numeric-nonzero-is-true",
			`CREATE TABLE t(c0 DEFAULT (abs(-5) IS TRUE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"numeric-nonzero-is-not-true",
			`CREATE TABLE t(c0 DEFAULT (abs(-5) IS NOT TRUE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"numeric-nonzero-is-false-control",
			`CREATE TABLE t(c0 DEFAULT (abs(-5) IS FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"numeric-nonzero-is-not-false-control",
			`CREATE TABLE t(c0 DEFAULT (abs(-5) IS NOT FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, typeof(c0) FROM t`},
		{"numeric-exactly-one",
			`CREATE TABLE t(c0 DEFAULT (1 IS TRUE), c1 DEFAULT (1 IS NOT TRUE),
				c2 DEFAULT (1 IS FALSE), c3 DEFAULT (1 IS NOT FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, c1, c2, c3 FROM t`},

		{"null-operand",
			`CREATE TABLE t(c0 DEFAULT (NULL IS TRUE), c1 DEFAULT (NULL IS NOT TRUE),
				c2 DEFAULT (NULL IS FALSE), c3 DEFAULT (NULL IS NOT FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0, c1, c2, c3, typeof(c0), typeof(c1), typeof(c2), typeof(c3) FROM t`},

		{"check-constraint-control",
			`CREATE TABLE t(c0 DEFAULT 5 CHECK ('00:00:00' IS NOT FALSE))`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT c0 FROM t`},

		{"plain-select-control",
			`CREATE TABLE t(x)`,
			`INSERT INTO t DEFAULT VALUES`,
			`SELECT ('00:00:00' IS NOT FALSE), ('00:00:00' IS TRUE),
				('00:00:00' IS NOT TRUE), ('00:00:00' IS FALSE),
				(abs(-5) IS TRUE), (abs(-5) IS NOT TRUE) FROM t`},
	} {
		differ(t, c.name, []string{c.create, c.insert, c.read})
	}
}

// TestDefaultIsBoolTruthTestAlterAndReplay tests truth values through ALTER TABLE ADD COLUMN.
func TestDefaultIsBoolTruthTestAlterAndReplay(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`ALTER TABLE t ADD COLUMN g DEFAULT ('00:00:00' IS NOT FALSE)`,
		`INSERT INTO t(a) VALUES(2)`,
		`SELECT a, g, typeof(g) FROM t ORDER BY a`,
	}
	differ(t, "alter-add-column", stmts)

	stmts2 := []string{
		`CREATE TABLE t(c0 DEFAULT (time('now','start of day') IS NOT FALSE))`,
		`INSERT INTO t DEFAULT VALUES`,
		`INSERT INTO t DEFAULT VALUES`,
		`SELECT count(*), count(DISTINCT c0), c0 FROM t`,
	}
	differ(t, "deferred-repeat-insert", stmts2)
}

// TestDefaultIsBoolTruthTestAlterOnEmptyTable tests ALTER TABLE ADD COLUMN on empty tables,
// which is the only way to drive a DEFAULT clause through the ALTER code path.
func TestDefaultIsBoolTruthTestAlterOnEmptyTable(t *testing.T) {
	for _, c := range []struct {
		name  string
		stmts []string
	}{
		{"alter-empty-text-literal", []string{
			`CREATE TABLE t(a)`,
			`ALTER TABLE t ADD COLUMN g DEFAULT ('00:00:00' IS NOT FALSE)`,
			`INSERT INTO t(a) VALUES(1)`,
			`SELECT a, g, typeof(g) FROM t`}},
		{"alter-empty-function-call", []string{
			`CREATE TABLE t(a)`,
			`ALTER TABLE t ADD COLUMN g DEFAULT (abs(-5) IS TRUE)`,
			`INSERT INTO t(a) VALUES(1)`,
			`SELECT a, g, typeof(g) FROM t`}},
		{"alter-empty-replay-stable", []string{
			`CREATE TABLE t(a)`,
			`ALTER TABLE t ADD COLUMN g DEFAULT ('00:00:00' IS NOT FALSE)`,
			`INSERT INTO t(a) VALUES(1)`,
			`INSERT INTO t(a) VALUES(2)`,
			`SELECT a, g FROM t ORDER BY a`}},
	} {
		differAlter(t, c.name, c.stmts)
	}
}
