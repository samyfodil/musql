package compat

// count() with NO arguments is C SQLite's own spelling of count(*): the
// builtin is registered with both nArg 0 and nArg 1, and countStep bumps the
// counter unconditionally when argc==0. It is the ONLY zero-argument aggregate
// SQLite accepts -- the companions probed here (sum/avg/total/min/max/
// group_concat, and count(DISTINCT)) are errors on both engines, which is what
// keeps this widening from turning into a blanket "any aggregate may take no
// argument".

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var zeroArgCountSchema = []string{
	"CREATE TABLE t1(a, b)",
	"INSERT INTO t1 VALUES(1,NULL),(1,2),(NULL,3),(2,4)",
	"CREATE TABLE empty1(a)",
}

func TestZeroArgCount(t *testing.T) {
	for _, q := range []string{
		// The plain whole-table aggregate, and the count(*) it must equal.
		"SELECT count(), count(*), count(a), count(b) FROM t1",
		"SELECT count() FROM empty1",
		"SELECT count() FROM t1 WHERE 0",
		"SELECT count() FROM t1 WHERE a IS NOT NULL",
		// FROM-less.
		"SELECT count()",
		// Per group, and in HAVING / ORDER BY.
		"SELECT a, count() FROM t1 GROUP BY a ORDER BY a",
		"SELECT a, count() FROM t1 GROUP BY a HAVING count()>1",
		"SELECT a, count() AS n FROM t1 GROUP BY a ORDER BY count(), a",
		// FILTER rides on the accumulator exactly as it does for count(*).
		"SELECT count() FILTER (WHERE a IS NOT NULL), count(*) FILTER (WHERE a IS NOT NULL) FROM t1",
		// As a window function, plain and framed.
		"SELECT a, count() OVER () FROM t1 ORDER BY rowid",
		"SELECT a, count() OVER (ORDER BY a) FROM t1 ORDER BY rowid",
		"SELECT a, count() OVER (PARTITION BY a) FROM t1 ORDER BY rowid",
		"SELECT count() OVER w FROM t1 WINDOW w AS (ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING)",
		// In a subquery, and on the write path's SELECT source.
		"SELECT (SELECT count() FROM t1)",
		"SELECT a FROM t1 WHERE (SELECT count() FROM t1) = 4 ORDER BY rowid",
		// Distinct is a syntax error on both engines; so is every OTHER
		// zero-argument aggregate. These are the boundary, not the feature.
		"SELECT count(DISTINCT) FROM t1",
		"SELECT sum() FROM t1",
		"SELECT avg() FROM t1",
		"SELECT total() FROM t1",
		"SELECT min() FROM t1",
		"SELECT max() FROM t1",
		"SELECT group_concat() FROM t1",
		"SELECT count(a,b) FROM t1",
	} {
		if !differ(t, "zeroargcount", append(append([]string(nil), zeroArgCountSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestZeroArgCountExec covers the exec side: count() reaching the write path
// through an INSERT ... SELECT (the corpus's insert2.test spelling).
func TestZeroArgCountExec(t *testing.T) {
	stmts := append(append([]string(nil), zeroArgCountSchema...),
		"CREATE TABLE dst(k, n)",
		"INSERT INTO dst SELECT a, count() FROM t1 GROUP BY a",
		"SELECT k, n FROM dst ORDER BY k",
	)
	if !differ(t, "zeroargcountexec", stmts) {
		t.Error("diverged")
	}
}
