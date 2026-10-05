// Gate for non-recursive CTE references that correlate to an enclosing GROUP BY's row.
package compat

import "testing"

// TestWith3Test42Verbatim is with3.test's own mined statement 4.2
// (testdata/tcl/with3.test), a chrome-ticket-1043236 regression for real
// SQLite: three levels of non-recursive WITH nested inside a scalar
// subquery, itself inside an outer GROUP BY, where the innermost CTE's row
// production only reaches the outer row through compiler.rowOuter -- the
// live-row fallback, never a compiled c.outer chain, because the enclosing
// query is a GROUP BY, whose select-list subqueries reach the outer row
// through that live scope (compileScanGroupBy, vdbe_agg_codegen.go) rather
// than through a compiled outer chain.
func TestWith3Test42Verbatim(t *testing.T) {
	differ(t, "with3.test 4.2 verbatim -- 3-deep nested CTE reaching an outer GROUP BY row via rowOuter", []string{
		`SELECT (
		   WITH t1(a) AS (VALUES(1))
		   SELECT (
		     WITH t2(b) AS (
		       WITH t3(c) AS (
		         WITH t4(d) AS (VALUES('elvis'))
		         SELECT t4a.d FROM t4 AS t4a JOIN t4 AS t4b LEFT JOIN t4 AS t4c
		       )
		       SELECT c FROM t3 WHERE a = 1
		     )
		     SELECT t2a.b FROM t2 AS t2a JOIN t2 AS t2x
		   )
		   FROM t1 GROUP BY 1
		 )
		 GROUP BY 1`,
	})
}

// TestWith3DeepNestedCTEColumnMinimal is the investigator's own binary-search
// minimization of the mined statement above: a two-level nested WITH,
// referencing the outer CTE's column ONLY from inside the inner CTE's body,
// consumed by a scalar subquery under an outer GROUP BY.
func TestWith3DeepNestedCTEColumnMinimal(t *testing.T) {
	differ(t, "two-level nested WITH reaching an outer GROUP BY row via rowOuter", []string{
		`SELECT (WITH t1(a) AS (VALUES(1)) SELECT (WITH t2(b) AS (SELECT a) SELECT b FROM t2) FROM t1 GROUP BY 1)`,
	})
}

// TestWith3DeepNestedCTEColumnNoGroupByControl is the positive control this
// candidate's own repro steps used to isolate the trigger: the identical
// nested-WITH correlation with NO outer GROUP BY already worked before this
// fix (the enclosing scalar subquery there is reached via a live c.outer
// compile chain, not compiler.rowOuter, so Program.Correlated was already
// true) -- confirming this fix changes behavior only for the GROUP BY shape,
// not this one.
func TestWith3DeepNestedCTEColumnNoGroupByControl(t *testing.T) {
	differ(t, "identical nested-WITH correlation with no outer GROUP BY -- already worked", []string{
		`SELECT (WITH t1(a) AS (VALUES(1)) SELECT (WITH t2(b) AS (SELECT a) SELECT b FROM t2) FROM t1)`,
	})
}

// TestWith3DeepNestedCTEColumnDerivedTableControl is the second control: an
// ordinary (CTE-free) derived table at the identical two-hop correlation
// depth, under the identical outer GROUP BY -- resolved through
// resolveDerivedSource's existing derivedOuterBarrier/prog.Correlated wiring
// rather than resolveCorrelatedCTESource, so it already worked before this
// fix and must go on working identically after it.
func TestWith3DeepNestedCTEColumnDerivedTableControl(t *testing.T) {
	differ(t, "ordinary derived table, same depth, same outer GROUP BY -- already worked", []string{
		`SELECT (WITH t1(a) AS (VALUES(1)) SELECT (SELECT b FROM (SELECT a AS b)) FROM t1 GROUP BY 1)`,
	})
}
