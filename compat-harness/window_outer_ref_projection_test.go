// Window query outer projection references are buffered as ephemeral row columns
// with proper affinity, collation, and storage class. Cases cover collation,
// affinity, and storage-class defense of outer references.
package compat

import "testing"

var winOuterRefFixture = []string{
	`CREATE TABLE sales(emp TEXT, region TEXT, total INTEGER)`,
	`INSERT INTO sales VALUES('Alice','North',34),('Frank','South',12),` +
		`('Charles','North',45),('Darrell','South',8),('Grant','South',23),` +
		`('Brad','North',22),('Elizabeth','South',99),('Horace','East',11)`,
	`CREATE TABLE o(c TEXT COLLATE NOCASE, i INTEGER, n, t TEXT)`,
	`INSERT INTO o VALUES('apple', 5, '-1', 'x'),('APPLE', 12, 0, 'y'),(NULL, NULL, NULL, NULL)`,
	`CREATE TABLE inr(x INTEGER, y TEXT)`,
	`INSERT INTO inr VALUES(1,'a'),(2,'b'),(3,'c')`,
	`CREATE TABLE t1(t1_id INTEGER PRIMARY KEY)`,
	`CREATE TABLE t2(t2_id INTEGER PRIMARY KEY)`,
	`CREATE TABLE t3(t3_id INTEGER PRIMARY KEY)`,
	`INSERT INTO t1 VALUES(1),(3),(5)`,
	`INSERT INTO t2 VALUES(3),(5)`,
	`INSERT INTO t3 VALUES(10),(11),(12)`,
}

func TestWindowOuterRefProjection(t *testing.T) {
	flLockstep(t, "window-outer-ref", winOuterRefFixture,
		`SELECT emp, region, (
		   SELECT sum(total) OVER (
		     ORDER BY total RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
		   ) || outer.emp FROM sales
		 ) FROM sales AS outer ORDER BY emp`,
		`SELECT t1.* FROM t1, t2 WHERE
		   t1_id=t2_id AND t1_id IN (
		     SELECT t1_id + row_number() OVER ( ORDER BY t1_id ) FROM t3
		   )`,
		`SELECT i, (SELECT o.t FROM inr LIMIT 1) FROM o ORDER BY i`,
		`SELECT i, (SELECT max(x) OVER () || o.t FROM inr LIMIT 1) FROM o ORDER BY i`,
		`SELECT (SELECT count(*) OVER () || coalesce(o.t,'<null>') FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT (SELECT o.t || sum(x) OVER () || o.t FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT i, (SELECT x || o.t FROM inr ORDER BY o.t || x LIMIT 1) FROM o ORDER BY i`,
		`SELECT i, (SELECT group_concat(y) OVER () FROM inr ORDER BY o.t || y LIMIT 1) FROM o ORDER BY i`,
		// operand in place would corrupt the later reader (the bug G2 found on
		// the ordinary column block).
		`SELECT (SELECT CAST(o.n AS INTEGER) || '-' || o.n || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// A FROM-LESS window query (compileNoFromWindow), whose one synthetic
		// row still carries the operand block the buffered column lives in.
		`SELECT i, (SELECT count(*) OVER () || o.t) FROM o ORDER BY i`,
		`SELECT i, (SELECT o.t || row_number() OVER (ORDER BY o.i)) FROM o ORDER BY i`,
		// Two enclosing levels: the reference names the OUTERMOST one.
		`SELECT (SELECT (SELECT max(x) OVER () || o.t FROM inr LIMIT 1) FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// A JOIN in the enclosing query, so the reference is qualified against
		// one of two live cursors.
		`SELECT (SELECT sum(x) OVER () || o.t || inr.y FROM inr LIMIT 1) FROM o, inr WHERE inr.x=1 ORDER BY 1`,
	)
}

// TestWindowOuterRefProjectionCollation is the condition the shape test cannot
// create: the buffered value has to carry the source column's DECLARED
// collating sequence into the projection's own comparison, exactly as C's
// ephemeral column does (sqlite3ColumnSetColl, select.c:2426-2429).
//
// o.c declares COLLATE NOCASE and every literal below is cased the OTHER way,
// so a buffered column that lost the collation compares BINARY and answers 0
// where 3.53.3 answers 1. Measured, not assumed: with compiler.affCtx's bufOut
// tail missing, the first two cases answered 0 for the 'apple' row where the
// oracle answers 1.
func TestWindowOuterRefProjectionCollation(t *testing.T) {
	flLockstep(t, "window-outer-ref-collation", winOuterRefFixture,
		`SELECT (SELECT (o.c = 'APPLE') || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT (SELECT (o.c < 'B') || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// An EXPLICIT COLLATE around the reference OVERRIDES the declared one,
		// which is the other half of resolveCompareCollation's two tiers.
		`SELECT (SELECT (o.c COLLATE BINARY = 'APPLE') || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT (SELECT (o.t = 'X') || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// CASE ... WHEN uses the same comparison machinery.
		`SELECT (SELECT (CASE o.c WHEN 'APPLE' THEN 'hit' ELSE 'miss' END) || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// IN over a list, whose element collation is resolved the same way.
		`SELECT (SELECT (o.c IN ('APPLE','PEAR')) || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
	)
}

// TestWindowOuterRefProjectionAffinity is the other half: a buffered column has
// to keep its AFFINITY (select.c:2381) and its storage-class defense, or the
// projection's comparison coerces the wrong side.
//
// o.i declares INTEGER, so "o.i = '12'" applies NUMERIC coercion to the text
// literal and is TRUE; a buffered value with no affinity compares 12 against
// '12' as different storage classes and is FALSE. o.n is TYPELESS, which is
// isMaterializedRef's case: a bare typeless column BLOCKS the TEXT coercion a
// computed one would allow (view.test 27.*).
func TestWindowOuterRefProjectionAffinity(t *testing.T) {
	flLockstep(t, "window-outer-ref-affinity", winOuterRefFixture,
		`SELECT (SELECT (o.i = '12') || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT (SELECT (o.i < '9') || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT (SELECT (o.i IN ('5','12')) || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT (SELECT (o.i BETWEEN '1' AND '9') || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// The typeless column against a TEXT one, both spellings: the bare
		// reference (materialized, no coercion) and a computed one.
		`SELECT (SELECT (o.n < o.t) || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		`SELECT (SELECT (o.n+0 < o.t) || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
	)
}

// TestWindowOuterRefProjectionUnchanged pins the shapes the lift must NOT
// touch, each for its own reason.
//
// A reference inside a scalar sub-select of the projection is window.c:756-770's
// p->pSubSelect guard: there C lifts only columns naming the WINDOW query's own
// FROM, and this compiler already serves those through compileColumn's
// outer-regScope arm. A reference this query's own FROM offers but the register
// resolver refuses -- an AMBIGUOUS one over a join -- must keep declining and
// keep raising "ambiguous column name" (resolve.c:785) rather than becoming a
// buffered column that answers.
func TestWindowOuterRefProjectionUnchanged(t *testing.T) {
	flLockstep(t, "window-outer-ref-unchanged", winOuterRefFixture,
		// The window query's OWN row, read from inside a select-list subquery.
		`SELECT (SELECT (SELECT y FROM inr AS i2 WHERE i2.x=inr.x) || sum(x) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// Both at once: the window query's own row AND the enclosing one.
		`SELECT (SELECT (SELECT y FROM inr AS i2 WHERE i2.x=inr.x) || o.t FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// Ambiguous over a join -- an error in both engines.
		`SELECT x, count(*) OVER () FROM inr, inr AS inr2 ORDER BY 1`,
		`SELECT (SELECT x || count(*) OVER () FROM inr, inr AS inr2 LIMIT 1) FROM o`,
		// A name no query in scope offers at all.
		`SELECT (SELECT nosuchcol || count(*) OVER () FROM inr LIMIT 1) FROM o`,
		// A bare TRUE keyword resolves to no column anywhere and must stay the
		// literal 1 rather than becoming a buffered read of nothing.
		`SELECT (SELECT TRUE || count(*) OVER () FROM inr LIMIT 1) FROM o`,
	)
}
