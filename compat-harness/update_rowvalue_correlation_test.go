// Tests UPDATE SET-clause subqueries with row values: checks that
// setSubqueryReadsTable and setSubqueryCorrelatesToRow handle RowExpr correctly.
//
// Fix: both functions now recurse into RowExpr.Elems exactly like any other
// list of sub-expressions (mirroring FuncExpr.Args' existing treatment in
// both, and checkExprSupported's own established RowExpr-in-InExpr handling,
// sql_eval.go). There is no special resolution barrier to preserve:
// SQLite's resolver has no TK_VECTOR-specific case in resolve.c (grepped
// ~/.cache/musql/sqlite-353/src/resolve.c, no hits) -- a row value's
// elements are ordinary expressions resolved exactly like any other
// sub-expression position, which is what the generic per-element recursion
// this fix adds now does.
//
// Mined fixture: compat-harness/testdata/tcl/in7.test, statement group 4.0
// (byte-identical SQL below) -- a regression test SQLite itself carries for
// an unrelated SubrtnSig/infinite-recursion bug. The trigger's SECOND body
// statement's SET value is exactly this shape: a SELECT over "t1 NATURAL
// RIGHT JOIN t1" whose WHERE contains a row-value IN subquery, "(b,a) IN
// (SELECT rowid, d FROM t3)", nested two levels inside the SET subquery.
// Before this fix, firing the AFTER UPDATE trigger hit
// "engine: UPDATE t1: a SET subquery that reads the table being updated is
// not supported by this write path" on the OUTER "UPDATE t1 SET
// b=x'3333'" statement (which has no subquery of its own at all -- the
// decline surfaces while executing the trigger body, not the firing
// statement's own SET/WHERE). C SQLite's own answer for the final
// "SELECT quote(b) FROM t1" is X'3333', asserted directly in the .test file.
package compat

import "testing"

// TestUpdateRowValueInSubqueryNotCorrelated is the exact in7.test 4.0
// sequence, unmodified. The trigger's own updates are order-dependent
// SQLite-internal detail (deliberately not decomposed further here); the
// point of this test is that the WHOLE sequence -- including the trigger
// firing this engine used to decline mid-execution -- now answers and
// matches the real oracle.
func TestUpdateRowValueInSubqueryNotCorrelated(t *testing.T) {
	differ(t, "in7.test-4.0-rowvalue-in-subquery-set", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,x'1111')`,
		`CREATE TABLE t2(c)`,
		`CREATE TABLE t3(d)`,
		`CREATE TRIGGER t1tr UPDATE ON t1 BEGIN
    UPDATE t1 SET b=x'2222' FROM t2;
    UPDATE t1
       SET b = (SELECT a IN (SELECT a
                               FROM t1
                              WHERE (b,a) IN (SELECT rowid, d
                                                FROM t3
                                             )
                            )
                  FROM t1 NATURAL RIGHT JOIN t1
               );
  END`,
		`UPDATE t1 SET b=x'3333'`,
		`SELECT quote(b) FROM t1`,
	})
}

// TestUpdateRowValueSetSubqueryUncorrelated isolates the narrow fix from the
// trigger/SubrtnSig machinery above: a plain top-level UPDATE whose SET
// value is a row-value-IN subquery reading the target table, with the row
// value's own elements bound entirely by the subquery's OWN FROM (never
// escaping to the row being updated) -- the shape setSubqueryReadsTable now
// still declines to correlated, but setSubqueryCorrelatesToRow's new RowExpr
// case now correctly clears as uncorrelated.
func TestUpdateRowValueSetSubqueryUncorrelated(t *testing.T) {
	differ(t, "plain-rowvalue-in-subquery-uncorrelated", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE t SET b = (SELECT a IN (SELECT a FROM t WHERE (b,a) IN (SELECT 20, 2)) FROM t LIMIT 1)`,
		`SELECT a, b FROM t ORDER BY a`,
	})
}

// TestUpdateRowValueSetSubqueryCorrelated is the control: a row value whose
// element genuinely IS qualified to the outer row (via an alias the subquery's
// own FROM does not introduce) is correlated, and is answered off the live,
// partially-updated table exactly as C SQLite does.
func TestUpdateRowValueSetSubqueryCorrelated(t *testing.T) {
	differ(t, "rowvalue-in-subquery-correlated", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE t SET b = (SELECT count(*) FROM t t2 WHERE (t.b, t2.a) IN (SELECT 1, 1))`,
		`SELECT a, b FROM t ORDER BY a`,
		`UPDATE t SET b = (SELECT count(*) FROM t t2 WHERE (t.b, t2.b) IN (SELECT 1, 1))+b`,
		`SELECT a, b FROM t ORDER BY a`,
	})
}
