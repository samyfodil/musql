package compat

// This file tests that an aggregate's second argument (e.g., group_concat's
// separator) is evaluated eagerly on every row, including those that don't
// contribute to the result.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestAggSecondArgumentIsEager(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(x,y)",
		"INSERT INTO t VALUES(NULL, -9223372036854775808)",
	}
	for _, c := range []struct{ name, sql string }{
		// The two live divergences: a NULL value / a NULL key contributes
		// nothing, and the second argument is evaluated anyway.
		{"gc-sep-null-value", "SELECT group_concat(x, abs(y)) FROM t"},
		{"jgo-value-null-key", "SELECT json_group_object(x, abs(y)) FROM t"},
		// ... and the one-argument forms, whose argument is evaluated the same
		// way and which already agreed.
		{"gc-arg", "SELECT group_concat(abs(y)) FROM t"},
		{"jga-arg", "SELECT json_group_array(abs(y)) FROM t"},
		{"sum-arg", "SELECT sum(abs(y)) FROM t"},
		// The FILTER half: a rejected row evaluates NO argument, so these
		// answer rather than raising -- including the NULL (not merely false)
		// filter, which is sqlite3ExprIfFalse's SQLITE_JUMPIFNULL.
		{"filter-skips-arg", "SELECT sum(abs(y)) FILTER (WHERE 0) FROM t"},
		{"filter-null-skips-arg", "SELECT sum(abs(y)) FILTER (WHERE NULL) FROM t"},
		{"filter-skips-sep", "SELECT group_concat(x, abs(y)) FILTER (WHERE 0) FROM t"},
		{"filter-skips-jgo", "SELECT json_group_object(x, abs(y)) FILTER (WHERE 0) FROM t"},
		// A second aggregate beside a filtered one keeps its own answer: the
		// FILTER's jump is per aggregate, not per row.
		{"filter-is-per-aggregate", "SELECT sum(abs(y)) FILTER (WHERE 0), count(*) FROM t"},
		// ... and the same three shapes under GROUP BY, which compiles through
		// a different plan family (the hash aggregate).
		{"gc-sep-null-value-grouped", "SELECT x, group_concat(x, abs(y)) FROM t GROUP BY x"},
		{"filter-skips-arg-grouped", "SELECT x, sum(abs(y)) FILTER (WHERE 0) FROM t GROUP BY x"},
	} {
		differ(t, c.name, append(append([]string{}, setup...), c.sql))
	}
}
