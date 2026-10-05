package compat

// Gate for WITHOUT ROWID table planning and compound subquery column usage in the planner.
// (which update_from.go joins into the synthetic pass-one SELECT) and its SET
// value is a scalar subquery holding two UNIONs.

import "testing"

// wrPlanFixture is the rowvalue.test 30.3 schema with rows: t1 carries a
// non-INTEGER PRIMARY KEY, so it has a real automatic index (x is not the
// rowid), and t2 is WITHOUT ROWID. C SQLite plans "FROM t1, t2 WHERE x<a"
// as "SCAN t2" then "SEARCH t1 USING COVERING INDEX sqlite_autoindex_t1_1
// (x<?)" (verified with EXPLAIN QUERY PLAN against the 3.53.3 oracle), i.e.
// t2 in PRIMARY KEY order outermost and t1 in x order inside it -- neither of
// which is this engine's own rowid pass over t1.
var wrPlanFixture = []string{
	`CREATE TABLE t1(x INT PRIMARY KEY, y, z)`,
	`CREATE TABLE t2(a,b,c,d,e,PRIMARY KEY(a,b))WITHOUT ROWID`,
	`INSERT INTO t1 VALUES(5,'y5','z5'),(1,'y1','z1'),(3,'y3','z3')`,
	`INSERT INTO t2 VALUES(9,'b9','c9','d9','e9'),(2,'b2','c2','d2','e2'),(7,'b7','c7','d7','e7')`,
}

func TestWithoutRowidJoinAnchorOrder(t *testing.T) {
	for _, q := range []string{
		// The anchor of a whole-table aggregate: t2's FIRST row in scan order
		// paired with t1's. SQLite's outer loop is t2 in PRIMARY KEY order, so
		// a=2; this engine used to decline, and answering from its own rowid
		// pass over t1 would have reported a=7.
		`SELECT t2.a, t2.b, count(*) FROM t1, t2 WHERE x<a`,
		// Same, reporting the INNER loop's first row too: x=1, the smallest x
		// the index walk reaches, not t1's first rowid (x=5).
		`SELECT t1.x, t2.a, count(*) FROM t1, t2 WHERE x<a`,
		// Per-group anchors, grouped on either side of the join.
		`SELECT t2.a, count(*) FROM t1, t2 WHERE x<a GROUP BY t1.y`,
		`SELECT t1.z, count(*) FROM t1, t2 WHERE x<a GROUP BY t2.c`,
		// An ORDER-SENSITIVE aggregate reports the whole arrival order rather
		// than just its first row.
		`SELECT group_concat(t1.x||'/'||t2.a) FROM t1, t2 WHERE x<a`,
		`SELECT group_concat(t2.b) FROM t2, t1 WHERE x<a`,
		// No WHERE at all: the cross product, which still has to arrive in the
		// planner's nesting order.
		`SELECT group_concat(t2.a||t1.y) FROM t1, t2`,
	} {
		differ(t, "withoutrowid join anchor: "+q, append(append([]string{}, wrPlanFixture...), q))
	}
}

// TestRowValue303UpdateFromWithoutRowidTarget is rowvalue.test 30.3 verbatim --
// one of the six statements TestTCLCorpus still counted as an engine gap. Both
// tables are empty, so the UPDATE changes nothing on either engine; what it
// exercises is the COMPILE, which used to decline because update_from.go's
// synthetic pass-one SELECT ("SELECT t2.a, t2.b, <SET exprs> FROM t1, t2 WHERE
// x<a") is an aggregate query -- the sum() inside the SET subquery's window
// PARTITION BY re-associates outward to it -- reading t2.a and t2.b as bare
// columns over a FROM clause the ported planner refused.
func TestRowValue303UpdateFromWithoutRowidTarget(t *testing.T) {
	differ(t, "rowvalue.test 30.3", []string{
		`CREATE TABLE t1(x INT PRIMARY KEY, y, z)`,
		`CREATE TABLE t2(a,b,c,d,e,PRIMARY KEY(a,b))WITHOUT ROWID`,
		`UPDATE t2 SET (d,d,a)=(SELECT EXISTS(SELECT 1 IN(SELECT max( 1 IN(SELECT x ORDER BY 1)) OVER(PARTITION BY sum((SELECT y FROM t1 UNION SELECT x ORDER BY 1)))INTERSECT SELECT EXISTS(SELECT 1 FROM t1 UNION SELECT x ORDER BY 1) ORDER BY 1) ORDERa)|9 AS blob, 2, 3) FROM t1 WHERE x<a`,
		`SELECT * FROM t2`,
		// ...and with rows on both sides, so the compiled program is actually
		// exercised rather than merely produced.
		`INSERT INTO t1 VALUES(5,'y5','z5'),(1,'y1','z1')`,
		`INSERT INTO t2 VALUES(9,'b9','c9','d9','e9'),(2,'b2','c2','d2','e2')`,
		`UPDATE t2 SET (d,d,a)=(SELECT EXISTS(SELECT 1 IN(SELECT max( 1 IN(SELECT x ORDER BY 1)) OVER(PARTITION BY sum((SELECT y FROM t1 UNION SELECT x ORDER BY 1)))INTERSECT SELECT EXISTS(SELECT 1 FROM t1 UNION SELECT x ORDER BY 1) ORDER BY 1) ORDERa)|9 AS blob, 2, 3) FROM t1 WHERE x<a`,
		`SELECT * FROM t2 ORDER BY a, b`,
	})
}

// TestCompoundSubqueryColUsed is the colUsed half on ORDINARY rowid tables,
// where it is not entangled with the WITHOUT ROWID chain: an index whose
// COVERING test decides the scan order, and a compound subquery that used to
// make that test unanswerable.
func TestCompoundSubqueryColUsed(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b,c)`,
		`CREATE INDEX i1 ON t(a,b)`,
		`INSERT INTO t VALUES(2,'y','r0'),(1,'z','r1'),(2,'x','r2'),(3,'w','r3'),(1,'v','r4')`,
	}
	for _, q := range []string{
		// i1 covers a and b, the only columns read, so SQLite scans the index
		// and the rows arrive in (a,b) order rather than rowid order. The
		// compound in the scalar subquery is what used to hide that.
		`SELECT a, (SELECT count(*) FROM t AS s UNION SELECT 9) FROM t`,
		// The same question through the aggregate anchor: c is a bare column,
		// so the group's first row in scan order decides it.
		`SELECT c, count(*), (SELECT 1 UNION SELECT 2) FROM t`,
		// A CORRELATED compound arm: "s.a=t.a" resolves inside the subquery, so
		// it credits the subquery's own item, while nothing climbs out -- and
		// the second arm's bare literal names no column at all.
		`SELECT c, count(*), (SELECT b FROM t AS s WHERE s.a=t.a UNION SELECT 'q' ORDER BY 1) FROM t`,
		// An arm that DOES climb out: "c" in the second arm is the enclosing
		// t's, so it sets t's colUsed bit for c -- which stops i1 covering and
		// puts the scan back in rowid order.
		`SELECT a, (SELECT count(*) FROM t AS s UNION SELECT c) FROM t`,
		// An ORDER-SENSITIVE aggregate over the same covering index.
		`SELECT group_concat(a||b), (SELECT 7 UNION SELECT 8) FROM t`,
	} {
		differ(t, "compound subquery colUsed: "+q, append(append([]string{}, setup...), q))
	}
}
