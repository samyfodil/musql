// This file tests TRUE/FALSE as aggregate arguments.
package compat

import "testing"

func TestWindowR24AggregateBooleanLiteralArgument(t *testing.T) {
	setup := []string{
		`CREATE TABLE v1(v2, v3)`,
		`INSERT INTO v1 VALUES(1,'a'),(2,'b'),(3,'a')`,
	}
	q := func(sql ...string) []string { return append(append([]string{}, setup...), sql...) }

	differ(t, "TRUE/FALSE as an aggregate argument", q(
		`SELECT COUNT(DISTINCT TRUE) FROM v1`,
		`SELECT COUNT(TRUE) FROM v1`,
		`SELECT COUNT(TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT COUNT(DISTINCT TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT sum(TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT sum(DISTINCT TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT max(FALSE) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT min(TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT total(FALSE) FROM v1 GROUP BY v3 ORDER BY v3`,
		// the group_concat SEPARATOR, which aggArgExprs walks separately
		`SELECT group_concat(v2, TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		// beside a real column, and inside a larger argument expression
		`SELECT count(v2 + TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT sum(CASE WHEN v2>1 THEN TRUE ELSE FALSE END) FROM v1 GROUP BY v3 ORDER BY v3`,
		`SELECT count(*) FILTER (WHERE TRUE) FROM v1 GROUP BY v3 ORDER BY v3`,
		// the positions that already worked, so the fallback ordering is pinned
		`SELECT TRUE, FALSE FROM v1`,
		`SELECT count(*) FROM v1 GROUP BY TRUE`,
		`SELECT count(*) FROM v1 ORDER BY TRUE`,
	))

	// distinctagg.test 7.0 itself, generated column and all. It was pinned
	// separately as a group-emission-order DECLINE (a3b0fca): the ported planner
	// declines a DISTINCT aggregate argument, so groupEmissionOrderProvable fell
	// through to groupingCouldArriveOrdered, which found that likelihood(v3,0.1)
	// peels to the UNIQUE generated column v3 (sqlite3ExprSkipCollateAndLikely,
	// expr.c:218-232; EP_Unlikely at resolve.c:1161) and could therefore be
	// delivered by an index without a sort -- select.c's groupBySort==0 arm,
	// where groups come out in SCAN order rather than ascending key order.
	//
	// The statement is SERVED again and byte-identical to the oracle's 0 rows
	// (g1 is empty, so there is no emission order to get wrong), so it belongs
	// in the battery rather than behind a decline pin, exactly as that pin's own
	// doc comment instructed -- a stale pin is what lets the next regression
	// here pass unnoticed.
	differ(t, "distinctagg.test 7.0, the generated-column spelling", []string{
		`CREATE TABLE g1 ( v2 UNIQUE, v3 AS( TYPEOF ( NULL ) ) UNIQUE )`,
		`SELECT COUNT ( DISTINCT TRUE ) FROM g1 GROUP BY likelihood ( v3 , 0.100000 )`,
	})

	// A real column named "true" must still WIN over the fallback -- the reason
	// the fallback is consulted only after both lookups fail.
	differ(t, "a column actually named true wins", []string{
		`CREATE TABLE b1("true", g)`,
		`INSERT INTO b1 VALUES(7,'x'),(9,'x'),(11,'y')`,
		`SELECT sum(true) FROM b1`,
		`SELECT g, sum(true) FROM b1 GROUP BY g ORDER BY g`,
		`SELECT g, count(DISTINCT true) FROM b1 GROUP BY g ORDER BY g`,
		`SELECT g, max(true) FROM b1 GROUP BY g ORDER BY g`,
	})

}

// TestWindowR24AggBooleanArgOuterColumn: over o1("true")=(5),(6) and
// i1(k)=(1),(2),(3), C SQLite answers ONE row, 5|11, for each spelling --
// an unqualified TRUE or "true" is o1's column because o1 binds it, so sum()
// re-associates with the outer query (lookupName's literal fallback applies
// only when no NameContext binds the name, resolve.c:719-745). The engine
// answered two rows for the unqualified spellings until
// checkAggregateAssociation was handed the enclosing compile chain
// (fallbackNameBindsOutward, engine/sql_agg.go).
func TestWindowR24AggBooleanArgOuterColumn(t *testing.T) {
	setup := []string{
		`CREATE TABLE o1("true")`,
		`CREATE TABLE i1(k)`,
		`INSERT INTO o1 VALUES(5),(6)`,
		`INSERT INTO i1 VALUES(1),(2),(3)`,
	}
	for _, q := range []string{
		`SELECT "true", (SELECT sum(true) FROM i1) FROM o1 ORDER BY 1`,
		`SELECT "true", (SELECT sum("true") FROM i1) FROM o1 ORDER BY 1`,
		`SELECT "true", (SELECT sum(o1."true") FROM i1) FROM o1 ORDER BY 1`,
	} {
		differ(t, q, append(append([]string{}, setup...), q))
	}
}
