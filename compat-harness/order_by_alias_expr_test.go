package compat

// A select-list alias used in ORDER BY, including the expression form
// (e.g., ORDER BY +x, ORDER BY abs(x)), aggregate aliases, and window aliases.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var orderAliasSchema = []string{
	"CREATE TABLE t1(a, b, c)",
	"INSERT INTO t1 VALUES(3,10,'x'),(1,20,'y'),(2,30,'z'),(1,40,'w')",
	"CREATE TABLE t2(a, d)",
	"INSERT INTO t2 VALUES(1,100),(2,200)",
}

func TestOrderByAliasInExpression(t *testing.T) {
	for _, q := range []string{
		// Expression form with unary operators and functions.
		"SELECT a AS x FROM t1 ORDER BY +x",
		"SELECT a AS x FROM t1 ORDER BY -x",
		"SELECT a AS x FROM t1 ORDER BY -x DESC",
		"SELECT a-1 AS x FROM t1 ORDER BY abs(x)",
		"SELECT b-23 AS x FROM t1 ORDER BY -abs(x)",
		"SELECT a AS x, b AS y FROM t1 ORDER BY 10-(x+y)",
		"SELECT a AS x FROM t1 ORDER BY x*x, x",
		"SELECT c AS s FROM t1 ORDER BY s || 'q'",
		"SELECT a AS x FROM t1 ORDER BY CASE WHEN x>1 THEN 0 ELSE 1 END, x",
		// Aggregate aliases.
		"SELECT sum(b) AS s FROM t1 GROUP BY a ORDER BY s+1",
		"SELECT a, sum(b) AS s FROM t1 GROUP BY a ORDER BY s*-1",
		"SELECT b*2+1 AS x, count(*) AS y FROM t1 GROUP BY x ORDER BY 10-(x+y)",
		"SELECT avg(b) AS m FROM t1 GROUP BY a ORDER BY m-100",
		// Window aliases.
		"SELECT sum(b) OVER () AS w, a FROM t1 ORDER BY w+1, a",
		"SELECT sum(b) OVER (ORDER BY a) AS abc, a FROM t1 ORDER BY abc+5",
		"SELECT avg(b) OVER (ORDER BY a) AS z, a FROM t1 ORDER BY (a IS z), a",
		// Column wins over same-named alias.
		"SELECT a+0 AS a FROM t1 ORDER BY a+0",
		"SELECT b AS a FROM t1 ORDER BY a+0",
		"SELECT t1.a AS d FROM t1, t2 WHERE t1.a=t2.a ORDER BY d+0, t1.rowid",
		// Bare form (output column), collation, and ordinal.
		"SELECT a AS x FROM t1 ORDER BY x",
		"SELECT a AS x FROM t1 ORDER BY x COLLATE nocase",
		"SELECT a AS x FROM t1 ORDER BY 1",
		// Invalid names and aggregate misuse.
		"SELECT a AS x FROM t1 ORDER BY nosuchthing+1",
		"SELECT b AS bb FROM t1 ORDER BY sum(bb)",
	} {
		if !differ(t, "orderalias", append(append([]string(nil), orderAliasSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
