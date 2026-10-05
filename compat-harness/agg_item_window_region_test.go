// This file tests aggregate items (subqueries in aggregate queries) with window
// functions that reference the enclosing group.
package compat

import "testing"

// TestAggItemWindowRegionBareRef tests bare group columns in window query projections.
func TestAggItemWindowRegionBareRef(t *testing.T) {
	flLockstep(t, "agg item window region: bare group column", []string{
		`CREATE TABLE t(k INTEGER, v INTEGER)`,
		`CREATE TABLE b(n INTEGER)`,
		`INSERT INTO t VALUES(1,10),(1,20),(2,30),(2,40),(3,50)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
	},
		// The classic: max(n + v) OVER () with v naming the group's anchor row.
		`SELECT k, count(*), (SELECT max(n + v) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// The same reference in a window's own PARTITION BY / ORDER BY.
		`SELECT k, (SELECT sum(n) OVER (ORDER BY n + v) FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT count(*) OVER (PARTITION BY n % v) FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// And in the window body's WHERE, which the per-level flag used to
		// cover too even though a WHERE is compiled by the scan body.
		`SELECT k, (SELECT max(n) OVER () FROM b WHERE n < v LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// A bare name the window query's OWN from shadows must still be the
		// window query's -- lookupName's inner-first walk (resolve.c:703
		// before :704), not the group's.
		`SELECT k, (SELECT max(n) OVER () FROM b AS s(v) LIMIT 1) FROM t GROUP BY k ORDER BY k`,
	)
}

// TestAggItemWindowRegionPlaceholder tests placeholders in window query projections.
func TestAggItemWindowRegionPlaceholder(t *testing.T) {
	flLockstep(t, "agg item window region: placeholders", []string{
		`CREATE TABLE t(k INTEGER, v INTEGER)`,
		`CREATE TABLE b(n INTEGER)`,
		`INSERT INTO t VALUES(1,10),(1,20),(2,30),(2,40),(3,50)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
	},
		// A HOISTED aggregate: sum(t.v) belongs to the OUTER query, is
		// finalized before the body runs, and stands in the window level's
		// select list as a groupAggExpr.
		`SELECT k, (SELECT sum(t.v) + max(n) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// The GROUP BY key column.
		`SELECT k, (SELECT k + max(n) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// The group's ANCHOR ROW, named with the aggregate query's own
		// qualifier -- a groupBareColExpr.
		`SELECT k, (SELECT t.v + max(n) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// The same placeholder twice, which must dedup to ONE batch column.
		`SELECT k, (SELECT t.v + max(n) OVER () + t.v FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// A placeholder inside a SUBQUERY of the projection, one frame further
		// out than the buffered register.
		`SELECT k, (SELECT (SELECT t.v) + max(n) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
	)
}

// TestAggItemWindowRegionCollation is the region's collation half. A buffered
// operand that lost its DECLARED collation compares BINARY and silently changes
// a comparison's truth value -- the wrong answer windowBufCols.aff exists for,
// re-asked here for a PLACEHOLDER, which carries its own affinity and collation
// on the node rather than through that context.
func TestAggItemWindowRegionCollation(t *testing.T) {
	flLockstep(t, "agg item window region: collation", []string{
		`CREATE TABLE o(g INTEGER, c TEXT COLLATE NOCASE)`,
		`CREATE TABLE inr(x INTEGER)`,
		`INSERT INTO o VALUES(1,'apple'),(2,'Banana')`,
		`INSERT INTO inr VALUES(1),(2)`,
	},
		`SELECT g, (SELECT (o.c = 'APPLE') || count(*) OVER () FROM inr LIMIT 1) FROM o GROUP BY g ORDER BY g`,
		// NOT the hoisted spelling, "(SELECT (max(o.c)='APPLE') || count(*)
		// OVER () ...)": that one is a SEPARATE live decline, present with this
		// region's flag both lifted and set -- "aggregate inside a select-list
		// subquery whose argument belongs to the enclosing query" is raised by
		// the hoist pass before this region is ever reached (measured both
		// ways).
	)
}

// TestAggItemWindowRegionRowValue is window1.test#25's own shape and the two
// nearby ones the corpus carries -- the statements that PANICKED before the
// substitution pass grew its guard, and that the guard then kept off the
// compiled route.
func TestAggItemWindowRegionRowValue(t *testing.T) {
	flLockstep(t, "agg item window region: window1.test#25", []string{
		`CREATE TABLE t0(c0 INTEGER)`,
		`INSERT INTO t0 VALUES(0),(1)`,
	},
		`SELECT (0, 0) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
	)
}

// TestAggItemWindowRegionNestedFromless is the corpus's other reaching shape: a
// FROM-less subquery carrying both a window call and an aggregate that
// re-associates outward, written in a WHERE.
func TestAggItemWindowRegionNestedFromless(t *testing.T) {
	flLockstep(t, "agg item window region: nested FROM-less", []string{
		`CREATE TABLE a(c INTEGER)`,
		`INSERT INTO a VALUES(1),(4),(4)`,
	},
		`SELECT a.c FROM a JOIN a AS b ON a.c=4 JOIN a AS e ON a.c=e.c
		   WHERE a.c=(SELECT (SELECT coalesce(lead(2) OVER(),0) + sum(d.c)) FROM a AS d WHERE a.c)`,
	)
}

// TestAggItemQualifiedRowid pins the OTHER shape this round promoted: a
// PSEUDO-ROWID of the aggregate query's own table, written with that query's
// qualifier inside one of its select-list subqueries.
//
// It used to decline outright, on the correct observation that substituting it
// as an ordinary column ("groupBareColExpr{idx: idx}") would read the anchor
// row's FIRST COLUMN instead -- a wrong value, and a NON-NULL one on a LEFT
// JOIN's NULL-extended side, which changes a comparison's truth value rather
// than just a displayed number. The placeholder's isRowid form reads the
// anchor row's trailing rowid block instead, which is the slot the oracle's
// answer requires. The LEFT JOIN cases below are what separate the two.
func TestAggItemQualifiedRowid(t *testing.T) {
	flLockstep(t, "agg item qualified pseudo-rowid", []string{
		`CREATE TABLE t(k INTEGER, v INTEGER)`,
		`CREATE TABLE u(w INTEGER)`,
		`INSERT INTO t VALUES(1,10),(1,20),(2,30),(3,40)`,
		`INSERT INTO u VALUES(1),(2),(3),(4)`,
	},
		`SELECT k, (SELECT count(*) FROM u WHERE u.w = t.rowid) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT t.rowid) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(w + t.rowid) FROM u) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT t._rowid_) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT t.oid) FROM t GROUP BY k ORDER BY k`,
	)
	// The NULL-extended side: r.rowid is NULL for every group whose right arm
	// found no match, and "r.rowid IS NULL" is the comparison that separates a
	// correct read from one that answered the first column.
	flLockstep(t, "agg item qualified pseudo-rowid: LEFT JOIN", []string{
		`CREATE TABLE l(a INTEGER, b INTEGER)`,
		`CREATE TABLE r(a INTEGER, c INTEGER)`,
		`INSERT INTO l VALUES(1,10),(2,20),(3,30)`,
		`INSERT INTO r VALUES(1,100)`,
	},
		`SELECT l.a, (SELECT r.rowid) FROM l LEFT JOIN r ON r.a = l.a GROUP BY l.a ORDER BY l.a`,
		`SELECT l.a, (SELECT r.rowid IS NULL) FROM l LEFT JOIN r ON r.a = l.a GROUP BY l.a ORDER BY l.a`,
		`SELECT l.a, (SELECT count(*) FROM r AS r2 WHERE r2.rowid = r.rowid) FROM l LEFT JOIN r ON r.a = l.a GROUP BY l.a ORDER BY l.a`,
	)
}
