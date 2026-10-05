package compat

// Tests for WINDOW functions inside scalar, IN, and EXISTS subqueries.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var windowSubquerySchema = []string{
	"CREATE TABLE t1(a, b, c)",
	"INSERT INTO t1 VALUES(1,10,100),(2,20,200),(3,30,300),(2,40,400)",
	"CREATE TABLE t2(x, y)",
	"INSERT INTO t2 VALUES(1,2),(3,4),(2,6)",
}

func TestWindowInSubquery(t *testing.T) {
	for _, q := range []string{
		// Scalar subquery, uncorrelated and correlated.
		"SELECT x, (SELECT sum(b) OVER (ORDER BY b) FROM t1) FROM t2 ORDER BY x",
		"SELECT x, (SELECT sum(b) OVER (ORDER BY b) FROM t1 LIMIT 1) FROM t2 ORDER BY x",
		"SELECT x, (SELECT row_number() OVER () FROM t1) FROM t2 ORDER BY x",
		"SELECT x, (SELECT sum(b) OVER (PARTITION BY a) FROM t1 WHERE b < t2.y*20) FROM t2 ORDER BY x",
		"SELECT (SELECT ntile(2) OVER (ORDER BY a) FROM t1) FROM t2 ORDER BY x",
		"SELECT (SELECT dense_rank() OVER (ORDER BY a) FROM t1) FROM t2 ORDER BY x",
		"SELECT (SELECT lead(b) OVER (ORDER BY b) FROM t1) FROM t2 ORDER BY x",
		"SELECT (SELECT first_value(b) OVER (ORDER BY b DESC) FROM t1) FROM t2 ORDER BY x",
		// IN and EXISTS.
		"SELECT x FROM t2 WHERE x IN (SELECT sum(b) OVER (ORDER BY b) FROM t1) ORDER BY x",
		"SELECT x FROM t2 WHERE x IN (SELECT row_number() OVER () FROM t1) ORDER BY x",
		"SELECT x FROM t2 WHERE EXISTS (SELECT row_number() OVER () FROM t1) ORDER BY x",
		"SELECT x, x IN (SELECT row_number() OVER () FROM t1) FROM t2 ORDER BY x",
		// A window inside a subquery inside a derived table.
		"SELECT * FROM (SELECT x, (SELECT max(b) OVER () FROM t1) AS m FROM t2) ORDER BY x",
		// The already-working derived-table spelling, as the control.
		"SELECT * FROM (SELECT sum(b) OVER (ORDER BY b) AS s FROM t1) ORDER BY s",
	} {
		if !differ(t, "windowsub", append(append([]string(nil), windowSubquerySchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
