// Window projection compilation is total: either compiles or statement fails.
// Tests the families that previously took the deleted projectWindowRow arm.
package compat

import "testing"

var winProjTotalFixture = []string{
	`CREATE TABLE t1(a INTEGER, b INTEGER, c1 TEXT COLLATE NOCASE)`,
	`INSERT INTO t1 VALUES(1,10,'aBCd'),(2,20,'x'),(3,30,'y')`,
	`CREATE TABLE u(k INTEGER, v INTEGER)`,
	`INSERT INTO u VALUES(1,100),(2,200),(3,300)`,
	// o is the enclosing-query workhorse: c declares NOCASE and n is TYPELESS
	// (no affinity), so both defend metadata a buffered column must carry. The
	// all-NULL row makes "the enclosing row is NULL" distinguishable from "there
	// is no enclosing row".
	`CREATE TABLE o(i INTEGER, c TEXT COLLATE NOCASE, n)`,
	`INSERT INTO o VALUES(5,'apple','-1'),(12,'APPLE',0),(NULL,NULL,NULL)`,
	`CREATE TABLE inr(x INTEGER, y TEXT)`,
	`INSERT INTO inr VALUES(1,'a'),(2,'b'),(3,'c')`,
	// Two FROM items offering the SAME column name, which is the only way to
	// reach the projection compile's ambiguity refusal.
	`CREATE TABLE j1(id INTEGER, sh INTEGER)`,
	`CREATE TABLE j2(id2 INTEGER, sh INTEGER)`,
	`INSERT INTO j1 VALUES(1,7),(2,8)`,
	`INSERT INTO j2 VALUES(1,7),(2,9)`,
}

// Aggregates in window query's ORDER BY are prohibited (ported rule).
func TestWindowProjectionAggregateInOrderBy(t *testing.T) {
	flLockstep(t, "winproj-agg-orderby", winProjTotalFixture,
		// windowerr.test 2.2's shape: the aggregate names the window call's own
		// output ALIAS, which is still an aggregate in the ORDER BY.
		`SELECT sum(a) OVER () AS xyz FROM t1 ORDER BY sum(xyz)`,
		`SELECT sum(a) OVER () FROM t1 ORDER BY sum(b)`,
		`SELECT a, row_number() OVER (ORDER BY a) FROM t1 ORDER BY count(*)`,
		`SELECT a, count(*) OVER () FROM t1 ORDER BY max(b) + 1`,
		// CONTROLS -- every one of these must ANSWER.
		//
		// SF_Aggregate is set by the GROUP BY, so the callback never runs.
		`SELECT a, sum(b) OVER () FROM t1 GROUP BY a ORDER BY max(b)`,
		// SF_Aggregate is set by the select list's own aggregate.
		`SELECT count(*), sum(a) OVER () FROM t1 ORDER BY max(b)`,
		// The aggregate belongs to a SUBQUERY of the ORDER BY term, which
		// xSelectCallback = 0 leaves alone.
		`SELECT a, count(*) OVER () FROM t1 ORDER BY (SELECT sum(v) FROM u), a`,
		// An ordinary ORDER BY over a window query, which must be untouched.
		`SELECT a, count(*) OVER () FROM t1 ORDER BY b DESC`,
		`SELECT a, count(*) OVER () FROM t1 ORDER BY 1`,
	)
}

// Uncorrelated subquery caching in projection register machine.
func TestWindowProjectionUncorrelatedSubquery(t *testing.T) {
	flLockstep(t, "winproj-uncorrelated-sub", winProjTotalFixture,
		// The corpus statement: the select-list aggregate is hoisted into a
		// derived table (compileScanGroupedWindow) and the projection is left
		// holding "_w0 COLLATE nocase IN (SELECT 'aBCd')" -- an uncorrelated IN
		// whose LHS carries a declared collation the comparison needs.
		`SELECT count() OVER (), group_concat(c1) IN (SELECT 'aBCd') FROM t1`,
		`SELECT count() OVER (), group_concat(c1) IN (SELECT 'ABCD') FROM t1`,
		`SELECT a, (SELECT max(v) FROM u), sum(b) OVER () FROM t1 ORDER BY a`,
		`SELECT a, a IN (SELECT k FROM u WHERE v>150), count(*) OVER () FROM t1 ORDER BY a`,
		`SELECT a, EXISTS(SELECT 1 FROM u WHERE k=99), count(*) OVER () FROM t1 ORDER BY a`,
		`SELECT a, EXISTS(SELECT 1 FROM u WHERE k=2), count(*) OVER () FROM t1 ORDER BY a`,
		// The lifetime case: an uncorrelated value (constant across the batch)
		// beside a correlated one (a different value per row), in ONE
		// projection, with a window call to force this path.
		`SELECT a, (SELECT max(v) FROM u), (SELECT v FROM u WHERE k=t1.a),
		   row_number() OVER (ORDER BY a) FROM t1 ORDER BY a`,
		// The same pair in the ORDER BY, which is a projection expression too.
		`SELECT a, count(*) OVER () FROM t1 ORDER BY (SELECT max(v) FROM u) - a`,
		`SELECT a, count(*) OVER () FROM t1 ORDER BY (SELECT v FROM u WHERE k=t1.a) DESC`,
		// The whole window query correlated to an enclosing row, so the machine
		// (and its cache) is rebuilt per outer row.
		`SELECT o.i, (SELECT (SELECT max(v) FROM u) + count(*) OVER () FROM inr LIMIT 1)
		   FROM o ORDER BY 1`,
		// A NULL-producing uncorrelated subquery, so "no row" is not confused
		// with "the value 0".
		`SELECT a, (SELECT v FROM u WHERE k=99), count(*) OVER () FROM t1 ORDER BY a`,
	)
}

// Enclosing query reference in window projection subquery.
func TestWindowProjectionEnclosingRefInSubquery(t *testing.T) {
	flLockstep(t, "winproj-enclosing-in-sub", winProjTotalFixture,
		// The plain shape: o.n read from inside a subquery of the window
		// query's projection.
		`SELECT o.i, (SELECT (SELECT o.n) || count(*) OVER () FROM inr LIMIT 1) FROM o ORDER BY 1`,
		// The same, through a subquery that has a FROM of its own.
		`SELECT o.i, (SELECT (SELECT o.n FROM inr LIMIT 1) || count(*) OVER () FROM inr LIMIT 1)
		   FROM o ORDER BY 1`,
		// COLLATION: 'apple' = 'APPLE' is 1 under o.c's declared NOCASE and 0
		// under BINARY, so a buffered column that lost it answers differently
		// rather than failing.
		`SELECT o.i, (SELECT ((SELECT o.c) = 'APPLE') || count(*) OVER () FROM inr LIMIT 1)
		   FROM o ORDER BY 1`,
		`SELECT o.i, (SELECT ((SELECT o.c FROM inr LIMIT 1) = 'APPLE') || count(*) OVER () FROM inr LIMIT 1)
		   FROM o ORDER BY 1`,
		// The reference in the window query's own ORDER BY, which
		// buildWindowProjList puts in the same projection list.
		`SELECT o.i, (SELECT y || count(*) OVER () FROM inr ORDER BY (SELECT o.i) || y LIMIT 1)
		   FROM o ORDER BY 1`,
		// The SAME reference twice, once at the top level and once inside a
		// subquery: C dedups buffered columns by sqlite3ExprCompare
		// (window.c:794-801) and this engine buffers them separately, so the
		// two reads must still agree.
		`SELECT o.i, (SELECT o.n || (SELECT o.n) || count(*) OVER () FROM inr LIMIT 1)
		   FROM o ORDER BY 1`,
		// A name that resolves NOWHERE, written exactly where this seam would
		// buffer it. Buffering must not turn "no such column" into an answer:
		// windowBufCols takes any name the window query's own FROM does not
		// offer, and it is the SCAN BODY's own compile of that buffered column
		// that refuses this one (emitWindowOperands), so the statement fails in
		// both engines.
		`SELECT o.i, (SELECT (SELECT nosuchcol) || count(*) OVER () FROM inr LIMIT 1) FROM o`,
		// The CONTROL: the same nested correlated reference with no window
		// function at all, which never went near this seam.
		`SELECT o.i, (SELECT (SELECT o.n) || 'z' FROM inr LIMIT 1) FROM o ORDER BY 1`,
	)
}

// Nested window query references outer window query's row.
func TestWindowProjectionNestedWindowNamesTheRow(t *testing.T) {
	flLockstep(t, "winproj-nested-window-row", winProjTotalFixture,
		// The middle and outer halves of window1.test 34.2, over this file's
		// own fixture: the innermost window's spec ORDER BY names a column of
		// the query one level out.
		`SELECT sum(b) OVER () FROM t1
		   ORDER BY (SELECT total(d) OVER (ORDER BY a) FROM (SELECT 1 AS d) ORDER BY 1)`,
		`SELECT avg(a) OVER (
		   ORDER BY (SELECT sum(b) OVER () FROM t1
		     ORDER BY (SELECT total(d) OVER (ORDER BY b) FROM (SELECT 1 AS d) ORDER BY 1))
		 ) FROM t1`,
		// The same reference QUALIFIED, and reaching a real table rather than a
		// one-row derived one.
		`SELECT a, count(*) OVER () FROM t1
		   ORDER BY (SELECT max(x) OVER (ORDER BY t1.b) FROM inr LIMIT 1), a`,
		// The nested window's spec ORDER BY names a NOCASE column of the outer
		// window query, so a register read that dropped the declared collation
		// would order differently rather than fail.
		`SELECT a, count(*) OVER () FROM t1
		   ORDER BY (SELECT group_concat(y) OVER (ORDER BY t1.c1) FROM inr LIMIT 1), a`,
	)
}

// Ambiguous column names in window projection remain rejected.
func TestWindowProjectionAmbiguousName(t *testing.T) {
	flLockstep(t, "winproj-ambiguous", winProjTotalFixture,
		`SELECT count(*) OVER (), sh FROM j1, j2`,
		`SELECT count(*) OVER () FROM j1, j2 ORDER BY sh`,
		// CONTROLS: the qualified spellings, which must answer.
		`SELECT count(*) OVER (), j1.sh, j2.sh FROM j1, j2 ORDER BY j1.sh, j2.sh`,
		`SELECT count(*) OVER (), id FROM j1, j2 ORDER BY id, id2`,
	)
}
