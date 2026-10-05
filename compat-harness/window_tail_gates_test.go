package compat

// This file tests window functions in FROM-less statements and multi-column
// subqueries in IN clauses.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestNoFromWindowSingleRow tests window functions in FROM-less statements.
func TestNoFromWindowSingleRow(t *testing.T) {
	for _, q := range []string{
		// Ordinary aggregates used as window functions.
		"SELECT sum(44) OVER ()",
		"SELECT total(4) OVER ()",
		"SELECT avg(4) OVER ()",
		"SELECT count(*) OVER ()",
		"SELECT count(7) OVER ()",
		"SELECT min(4) OVER ()",
		"SELECT max(4) OVER ()",
		"SELECT group_concat(9) OVER ()",
		// The ranking / positional functions that exist only as window
		// functions.
		"SELECT row_number() OVER ()",
		"SELECT rank() OVER (ORDER BY 1)",
		"SELECT dense_rank() OVER (ORDER BY 2)",
		"SELECT cume_dist() OVER ()",
		"SELECT percent_rank() OVER ()",
		"SELECT ntile(1) OVER ()",
		"SELECT lead(44) OVER ()",
		"SELECT lag(7) OVER ()",
		"SELECT lag(7,1,99) OVER ()",
		"SELECT first_value(0) OVER ()",
		"SELECT last_value(8) OVER ()",
		"SELECT nth_value(5,1) OVER ()",
		"SELECT nth_value(5,2) OVER ()",
		// A window in the ORDER BY as well as the select list, and a plain
		// column alongside one.
		"SELECT +sum(0) OVER () ORDER BY +sum(0) OVER ()",
		"SELECT count(*) OVER (), 5",
		"SELECT sum(1) OVER () AS s ORDER BY s",
		// An explicit PARTITION BY / frame over the single row.
		"SELECT sum(3) OVER (PARTITION BY 1)",
		"SELECT sum(3) OVER (ORDER BY 1 ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)",
		"SELECT sum(3) OVER (ORDER BY 1 RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)",
		// A named WINDOW clause without a FROM.
		"SELECT sum(6) OVER w WINDOW w AS (ORDER BY 1)",
		// WHERE excludes the synthetic row entirely; LIMIT/OFFSET drop it.
		"SELECT sum(3) OVER () WHERE 0",
		"SELECT sum(3) OVER () WHERE 1",
		"SELECT sum(3) OVER () WHERE NULL",
		"SELECT sum(1) OVER () LIMIT 0",
		"SELECT sum(1) OVER () LIMIT 1 OFFSET 1",
		"SELECT sum(1) OVER () LIMIT 1",
		// DISTINCT and FILTER, which the window plan owns too.
		"SELECT DISTINCT sum(2) OVER ()",
		"SELECT count(*) FILTER (WHERE 1) OVER ()",
		"SELECT count(*) FILTER (WHERE 0) OVER ()",
		// The VALUES spelling of a one-row FROM-less SELECT.
		"VALUES(count(*)OVER())",
		"VALUES(row_number()OVER())",
	} {
		if !differ(t, "nofromwin", []string{q}) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

var noFromWindowCorrelatedSchema = []string{
	"CREATE TABLE t1(a)",
	"INSERT INTO t1 VALUES(1),(2),(3)",
	"CREATE TABLE t0(c0)",
	"INSERT INTO t0 VALUES(0),(9)",
}

// TestNoFromWindowCorrelated covers the shape the corpus actually holds most
// of: a FROM-LESS subquery carrying the window, correlated to the enclosing
// row. C SQLite evaluates it once per outer row, still over one row -- so
// "(SELECT sum(a) OVER (ORDER BY a))" is just a, and "(SELECT row_number()
// OVER ())" is 1 for every row.
func TestNoFromWindowCorrelated(t *testing.T) {
	for _, q := range []string{
		"SELECT (SELECT sum(a) OVER (ORDER BY a)) FROM t1 ORDER BY 1",
		"SELECT (SELECT max(a) OVER ()) FROM t1 ORDER BY 1",
		"SELECT (SELECT min(a) OVER ()) FROM t1 ORDER BY 1",
		"SELECT (SELECT count(a) OVER ()) FROM t1 ORDER BY 1",
		"SELECT (SELECT row_number() OVER ()) FROM t1",
		"SELECT (SELECT lead(a) OVER ()) FROM t1",
		"SELECT (SELECT group_concat(a) OVER ()) FROM t1 ORDER BY 1",
		"SELECT (SELECT max(a) OVER (ORDER BY a) + min(a) OVER (ORDER BY a)) FROM t1 ORDER BY 1",
		"SELECT (SELECT count(a) OVER () + total(a) OVER ()) FROM t1 ORDER BY 1",
		// NOT covered here, and still declined: a SUBQUERY (or an aggregate)
		// inside the window's OWN ORDER BY / PARTITION BY -- "(SELECT count(a)
		// OVER (ORDER BY (SELECT sum(a) FROM t1)) + total(a) OVER ())". That is
		// compileScanWindow's own separate decline, which this single-row
		// rewrite neither reaches nor changes.
		//
		// The FROM-less window subquery used as a WHERE predicate, at the top
		// level and nested through two derived tables.
		"SELECT * FROM t1 WHERE (SELECT sum(a) OVER (ORDER BY a)) ORDER BY a",
		"SELECT b AS c FROM (SELECT a AS b FROM (SELECT a FROM t1 WHERE a=1 OR (SELECT sum(a) OVER ())) WHERE b=1 OR b<10) WHERE c=1 OR c>=10",
		"SELECT * FROM t1 WHERE a%1 OR (SELECT sum(a) OVER (ORDER BY a%2)) ORDER BY a",
		// As an IN right-hand side, and inside a row-value IN.
		"SELECT * FROM t0 WHERE c0 IN (SELECT first_value(0) OVER ()) ORDER BY c0",
		"SELECT * FROM t0 WHERE (c0, 0) IN (SELECT first_value(0) OVER (), 0) ORDER BY c0",
	} {
		if !differ(t, "nofromwincorr", append(append([]string(nil), noFromWindowCorrelatedSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// ---- a multi-column SUBQUERY as IN's left-hand side ----
//
// "(SELECT a,b) IN (SELECT x,y ...)" is a ROW-VALUE membership test whose probe
// happens to be spelled as a subquery. It used to be rejected outright with
// "sub-select returns 2 columns - expected 1": the compiler took IN's arity
// from the LHS expression, and a SubqueryExpr counted as exactly one column no
// matter how wide it really was.
//
// C SQLite (verified against mattn/go-sqlite3): "SELECT (SELECT 3,4) IN
// (SELECT 3,4)" is 1, "SELECT (SELECT 3,4) IN (SELECT 5,6)" is 0, and "SELECT
// (SELECT 3,4) IN (SELECT 3)" is the prepare-time error "sub-select returns 1
// columns - expected 2" -- the LHS decides the arity, the RHS is checked
// against it.

var rowSubqueryINSchema = []string{
	"CREATE TABLE b3(a, b)",
	"INSERT INTO b3 VALUES(1,2),(3,4),(5,6)",
	"CREATE TABLE b4(a, b)",
	"INSERT INTO b4 VALUES(1,2),(3,4)",
	"CREATE TABLE b5(a, b)",
	"INSERT INTO b5 VALUES(3,4),(7,8)",
	"CREATE TABLE nc(x TEXT COLLATE NOCASE, y TEXT)",
	"INSERT INTO nc VALUES('ABC','def')",
	"CREATE TABLE bin(x TEXT, y TEXT)",
	"INSERT INTO bin VALUES('abc','def')",
	"CREATE TABLE ii(i INTEGER, t TEXT)",
	"INSERT INTO ii VALUES(5,'q')",
	"CREATE TABLE empt(a, b)",
}

func TestRowValueSubqueryINLeft(t *testing.T) {
	for _, q := range []string{
		// The arity rule itself, both answers and the width error.
		"SELECT (SELECT 3,4) IN (SELECT 3,4)",
		"SELECT (SELECT 3,4) IN (SELECT 5,6)",
		"SELECT (SELECT 3,4) IN (SELECT 3)",
		"SELECT (SELECT 3) IN (SELECT 3,4)",
		"SELECT (SELECT 3,4,5) IN (SELECT 3,4,5)",
		"SELECT (SELECT 3,4) NOT IN (SELECT 3,4)",
		"SELECT (SELECT 3,4) NOT IN (SELECT 5,6)",
		// A NULL in either operand, which is IN's three-valued logic.
		"SELECT (SELECT 3,NULL) IN (SELECT 3,4)",
		"SELECT (SELECT 3,NULL) IN (SELECT 5,6)",
		"SELECT (SELECT 3,4) IN (SELECT 3,NULL)",
		// The RHS is a real table, and the LHS is correlated to the outer row.
		"SELECT * FROM b3 WHERE (SELECT b3.a, b3.b) IN (SELECT a, b FROM b5) ORDER BY a",
		"SELECT * FROM b3 WHERE (SELECT b3.a, b3.b) NOT IN (SELECT a, b FROM b5) ORDER BY a",
		"SELECT * FROM b3 JOIN b4 ON b4.a = b3.a WHERE (SELECT b3.a, b3.b) IN (SELECT a, b FROM b5) ORDER BY b3.a",
		"SELECT a, (SELECT b3.a, b3.b) IN (SELECT a, b FROM b5) FROM b3 ORDER BY a",
		// An EMPTY LHS subquery is a row of NULLs, not an empty set.
		"SELECT (SELECT a, b FROM empt) IN (SELECT a, b FROM b5)",
		"SELECT (SELECT a, b FROM empt) IN (SELECT a, b FROM empt)",
		// COLLATION: the probe's per-column collating sequence has to come from
		// the LHS subquery's own output columns, since there is no element
		// expression to read it off. nc.x is NOCASE, bin.x is BINARY, and the
		// two answer DIFFERENTLY for the same pair of values -- which is what
		// makes defaulting to BINARY a wrong answer rather than an imprecise one.
		"SELECT (SELECT x, y FROM nc) IN (SELECT 'abc','def')",
		"SELECT (SELECT x, y FROM bin) IN (SELECT 'ABC','def')",
		"SELECT (SELECT x, y FROM nc) IN (SELECT x, y FROM bin)",
		"SELECT (SELECT x, y FROM bin) IN (SELECT x, y FROM nc)",
		"SELECT (SELECT x COLLATE BINARY, y FROM nc) IN (SELECT 'abc','def')",
		// AFFINITY, likewise per column and combined with the RHS's.
		"SELECT (SELECT '5','5') IN (SELECT 5,'5')",
		"SELECT (SELECT 5,5) IN (SELECT '5','5')",
		// A one-column subquery LHS stays the ordinary scalar IN it always was.
		"SELECT (SELECT 3) IN (SELECT 3)",
		"SELECT (SELECT a FROM b3 LIMIT 1) IN (SELECT a FROM b5)",
		// A COMPOUND operand contributes NEITHER an affinity NOR a collating
		// sequence -- the pairs below are the proof, since each plain spelling
		// and its compound twin answer DIFFERENTLY over the same values. Its
		// own ORDER BY still decides which row is the operand's first row.
		"SELECT (SELECT x, y FROM nc UNION SELECT 'zz','def') IN (SELECT 'abc','def')",
		"SELECT (SELECT x, y FROM nc UNION ALL SELECT 'zz','def') IN (SELECT 'abc','def')",
		"SELECT (SELECT i, t FROM ii UNION SELECT 99,'q') IN (SELECT '5','q')",
		"SELECT (SELECT i, t FROM ii UNION ALL SELECT 99,'q') IN (SELECT '5','q')",
		"SELECT (SELECT i, t FROM ii UNION ALL SELECT 99,'q') IN (SELECT 5,'q')",
		"SELECT (SELECT '5', t FROM ii UNION ALL SELECT '99','q') IN (SELECT i, t FROM ii)",
		"SELECT (SELECT 3,4 UNION SELECT 5,6 ORDER BY 1) IN (SELECT 3,4)",
		"SELECT (SELECT 3,4 UNION SELECT 5,6 ORDER BY 1) IN (SELECT 5,6)",
		"SELECT (SELECT 5,6 UNION SELECT 3,4 ORDER BY 1) IN (SELECT 3,4)",
		"SELECT (SELECT 5,6 UNION SELECT 3,4 ORDER BY 1) IN (SELECT 5,6)",
		"SELECT (SELECT 3,4 UNION SELECT 5,6 ORDER BY 1 DESC) IN (SELECT 3,4)",
		"SELECT (SELECT 3,4 UNION SELECT 5,6 ORDER BY 1 DESC) IN (SELECT 5,6)",
		"SELECT (SELECT 5,6 UNION SELECT 3,4 ORDER BY 1 DESC) IN (SELECT 3,4)",
		"SELECT (SELECT 5,6 UNION SELECT 3,4 ORDER BY 1 DESC) IN (SELECT 5,6)",
		"SELECT (SELECT 3,4 INTERSECT SELECT 3,4) IN (SELECT 3,4)",
		"SELECT (SELECT 3,4 EXCEPT SELECT 5,6) IN (SELECT 3,4)",
		// The compound on the RIGHT is the mirror, and was already right: the
		// LHS row value's own collation decides it.
		"SELECT ('ABC','def') IN (SELECT x, y FROM nc UNION SELECT 'zz','def')",
		"SELECT ('abc','def') IN (SELECT x, y FROM nc UNION SELECT 'zz','def')",
	} {
		if !differ(t, "rowsubin", append(append([]string(nil), rowSubqueryINSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
