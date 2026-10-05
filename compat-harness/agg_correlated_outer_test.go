package compat

// Tests aggregates in subqueries that reference columns from enclosing queries.
// Aggregates can reference outer columns in their arguments, separators, and
// as bare columns. When an aggregate's columns are all outer, C SQLite
// re-associates the aggregate with the enclosing query.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var aggOuterSchema = []string{
	"CREATE TABLE t1(a1, b1)",
	"INSERT INTO t1 VALUES(1,'-'),(2,'+'),(3,'/')",
	"CREATE TABLE t2(a2, b2)",
	"INSERT INTO t2 VALUES(7,8),(9,10),(2,4)",
	"CREATE TABLE aa(x)",
	"INSERT INTO aa VALUES(1),(2)",
	"CREATE TABLE bb(y)",
	"INSERT INTO bb VALUES(10),(20)",
	"CREATE TABLE e1(z)",
}

func TestAggregateCorrelatedOuterRef(t *testing.T) {
	for _, q := range []string{
		// A mixed local+outer aggregate ARGUMENT.
		"SELECT x, (SELECT sum(x+y) FROM bb) FROM aa ORDER BY x",
		"SELECT a1, (SELECT count(a1+a2) FROM t2) FROM t1 ORDER BY a1",
		"SELECT a1, (SELECT sum(a2=a1) FROM t2) FROM t1 ORDER BY a1",
		"SELECT a1, (SELECT total(a2*a1) FROM t2) FROM t1 ORDER BY a1",
		"SELECT a1, (SELECT avg(a2-a1) FROM t2) FROM t1 ORDER BY a1",
		"SELECT a1, (SELECT max(a2*a1) FROM t2) FROM t1 ORDER BY a1",
		// group_concat / string_agg with a CORRELATED separator.
		"SELECT a1, (SELECT group_concat(a2,b1) FROM t2) FROM t1 ORDER BY a1",
		"SELECT a1, (SELECT string_agg(a2,b1) FROM t2) FROM t1 ORDER BY a1",
		// A correlated BARE column beside the aggregate (the anchor rule does
		// not apply -- it is not a column of this query's own FROM at all).
		"SELECT (SELECT max(a2)+a1 FROM t2) FROM t1 ORDER BY a1",
		"SELECT (SELECT a1+max(a2) FROM t2) FROM t1 ORDER BY a1",
		"SELECT (SELECT max(a2)+a1+b2 FROM t2) FROM t1 ORDER BY a1",
		// Two levels out.
		"SELECT (SELECT (SELECT max(y)+x FROM bb) FROM e1) FROM aa ORDER BY x",
		"SELECT (SELECT (SELECT max(x+y) FROM bb) FROM e1) FROM aa ORDER BY x",
		// The correlated aggregate under WHERE / an empty inner table.
		"SELECT x FROM aa WHERE (SELECT sum(x+y) FROM bb) > 31 ORDER BY x",
		"SELECT x, (SELECT sum(x+z) FROM e1) FROM aa ORDER BY x",
		// A correlated reference inside a GROUP BY subquery's HAVING.
		"SELECT x, (SELECT count(*) FROM bb GROUP BY y HAVING sum(y)>x) FROM aa ORDER BY x",
		// A plain uncorrelated aggregate subquery, as the control.
		"SELECT x, (SELECT sum(y) FROM bb) FROM aa ORDER BY x",
		// A reference that resolves NOWHERE stays an error on both engines --
		// passing an unresolvable column through instead of rejecting it made
		// "HAVING count(*)<z" answer zero rows where SQLite says "no such
		// column: z" (select5.test).
		"SELECT b2, count(*) FROM t2 GROUP BY b2 HAVING count(*)<zzz",
		"SELECT zzz, count(*) FROM t2 GROUP BY b2",
		"SELECT count(*) FROM t2 ORDER BY zzz",
		"SELECT max(zzz) FROM t2",
		"SELECT count(*), zzz",
		"SELECT sum(zzz)",
	} {
		if !differ(t, "aggouter", append(append([]string(nil), aggOuterSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestAggregateReassociationHoisted pins the shape this file used to require a
// DECLINE for: every column of the aggregate's argument list belongs to the
// OUTER query, so C SQLite treats the aggregate as the OUTER query's own and
// the whole statement collapses to one row. engine/vdbe_agg_hoist.go now
// reproduces that, so the assertion becomes the one that actually matters --
// the engine and the oracle agree, rows and row count alike, which a
// "must error" check could never have shown.
func TestAggregateReassociationHoisted(t *testing.T) {
	for _, q := range []string{
		"SELECT (SELECT string_agg(a1,'x') FROM t2) FROM t1",
		"SELECT (SELECT group_concat(a1) FROM t2) FROM t1",
		"SELECT (SELECT sum(a1) FROM t2) FROM t1",
	} {
		if !differ(t, "aggouter", append(append([]string(nil), aggOuterSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
