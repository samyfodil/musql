// Tests aggregate calls that escape through FROM-clause derived tables.
// An aggregate call inside a select-list subquery wrapping a derived table
// must be detected and hoisted to the enclosing aggregate query.
package compat

import "testing"

// TestAggHoistDerivedFromExactShape is the exact reported gap: a scalar
// subquery in an aggregate query's select list, wrapping a FROM-clause
// derived table whose own body contains the escaping aggregate call.
func TestAggHoistDerivedFromExactShape(t *testing.T) {
	setup := []string{
		`CREATE TABLE g(grp INTEGER, val INTEGER)`,
		`INSERT INTO g VALUES (1,10),(1,20),(2,30),(2,40)`,
	}
	differ(t, "aggregate call inside a derived-table-wrapped select-list subquery, qualified column", append(append([]string{}, setup...), []string{
		`SELECT grp, (SELECT v FROM (SELECT sum(g.val)*2 AS v)) FROM g GROUP BY grp`,
	}...))
	differ(t, "same shape, HAVING via EXISTS instead of the select list", append(append([]string{}, setup...), []string{
		`SELECT grp FROM g GROUP BY grp HAVING EXISTS (SELECT v FROM (SELECT sum(g.val)*2 AS v) WHERE v > 50)`,
	}...))
	differ(t, "same shape, ORDER BY", append(append([]string{}, setup...), []string{
		`SELECT grp FROM g GROUP BY grp ORDER BY (SELECT v FROM (SELECT sum(g.val)*2 AS v)) DESC`,
	}...))
}

// TestAggHoistDerivedFromNesting confirms the fix recurses through more than
// one layer of pure derived-table wrapping, and that a NON-aggregate
// reference (a bare correlated column, not inside any aggregate call) wrapped
// the same way was already working before this fix -- pinning that this
// change did not touch that already-correct path.
func TestAggHoistDerivedFromNesting(t *testing.T) {
	setup := []string{
		`CREATE TABLE g(grp INTEGER, val INTEGER)`,
		`INSERT INTO g VALUES (1,10),(1,20),(2,30),(2,40)`,
	}
	differ(t, "two layers of derived-table wrapping around the aggregate call", append(append([]string{}, setup...), []string{
		`SELECT grp, (SELECT v FROM (SELECT v2 AS v FROM (SELECT sum(g.val)*2 AS v2))) FROM g GROUP BY grp`,
	}...))
	differ(t, "already-working control: a bare correlated column (no aggregate) wrapped in a derived table", append(append([]string{}, setup...), []string{
		`SELECT grp, sum(val), (SELECT v FROM (SELECT g.grp*1000 AS v)) FROM g GROUP BY grp`,
	}...))
}

// TestAggHoistDerivedFromShadowing constructs the 3-level-deep adversarial
// shadowing case this mechanism's family has twice needed before an
// intermediate level's own alias must shadow an outer name before it ever
// reaches the escaping-reference walk. Here the MIDDLE derived table defines
// its own "grp" (from the hoisted aggregate itself), which must win over the
// outermost query's real "grp" column for a reference written at the
// INNERMOST level -- i.e. real-column-at-an-intermediate-level wins, exactly
// as the ordinary (non-aggregate) case's own shadowing rule already does.
func TestAggHoistDerivedFromShadowing(t *testing.T) {
	setup := []string{
		`CREATE TABLE g(grp INTEGER, val INTEGER)`,
		`INSERT INTO g VALUES (1,10),(1,20),(2,30),(2,40)`,
	}
	differ(t, "middle derived table's own alias (itself fed by the hoisted aggregate) shadows the outer real column", append(append([]string{}, setup...), []string{
		`SELECT grp, (SELECT v FROM (SELECT grp AS v FROM (SELECT sum(g.val) AS grp))) FROM g GROUP BY grp`,
	}...))
	differ(t, "innermost derived table's own alias shadows too, no escape at all", append(append([]string{}, setup...), []string{
		`SELECT grp, (SELECT v FROM (SELECT w AS v FROM (SELECT 999 AS w))) FROM g GROUP BY grp`,
	}...))
}

// TestAggHoistDerivedFromPastNonAggregateEnclosing is the mined regression
// this fix's own FROM-item descent introduced (aggnested.test#4, real corpus
// fixture): an aggregate call reached through the new descent whose argument
// needs to escape PAST an intervening aggregate query (t1, which has no such
// column) to an even-further-out, NON-aggregate enclosing query's own column
// (t0.c1). Before the fix below, the descent's "isCompoundMember && no local
// scope" rule (materializedOuterRefInAggArgArm, sql_group.go) wrongly treated
// that as escaping to t1's own aggregate context -- which t1 cannot satisfy
// -- turning a previously-served statement into a decline. See that
// function's own callOwnedByScope doc comment for the full trace.
func TestAggHoistDerivedFromPastNonAggregateEnclosing(t *testing.T) {
	differ(t, "aggnested.test#4 regression: aggregate arg escapes through a derived table AND past a non-aggregate enclosing query", []string{
		`CREATE TABLE t0(c1, c2)`,
		`INSERT INTO t0 VALUES(1,2)`,
		`CREATE TABLE t1(c3, c4)`,
		`INSERT INTO t1 VALUES(3,4)`,
		`SELECT * FROM t0 WHERE EXISTS (SELECT 1 FROM t1 GROUP BY c3 HAVING ( SELECT count(*) FROM (SELECT 1 UNION ALL SELECT sum(DISTINCT c1) ) ) ) BETWEEN 1 AND 1`,
	})
}

// TestAggHoistDerivedFromCompoundArmStillHoistsToImmediateOwner is an
// adversarial case an earlier, coarser attempt at the regression fix above
// got wrong: it gated the escape rule off for EVERY derived-table-reached
// compound arm, regardless of whether the reference actually belonged to the
// immediately enclosing aggregate query. That silently let this shape fall
// through to ordinary anchor-row correlation, which computed the aggregate
// over a single row instead of the whole group -- a real wrong answer, not
// just a needless decline. The fix (callOwnedByScope, checked against h's own
// scope before treating a reference as escaping at all) must still hoist here
// precisely because "val" genuinely belongs to g, the query this detection
// was invoked for -- unlike the aggnested.test case above, where "c1" does
// not belong to the immediately enclosing aggregate query at all. Uses sum
// (not min/max) deliberately: min/max compound-arm escapes decline for a
// separate, pre-existing, unrelated reason (recordSpec's own anchor-row
// census restriction, verified to apply identically with no derived-table
// wrapping at all -- not something this fix touches or needs to solve).
func TestAggHoistDerivedFromCompoundArmStillHoistsToImmediateOwner(t *testing.T) {
	setup := []string{
		`CREATE TABLE g(grp INTEGER, val INTEGER)`,
		`INSERT INTO g VALUES (1,10),(1,20),(2,30),(2,40)`,
	}
	differ(t, "compound arm reached via one derived-table hop still hoists to the immediately enclosing aggregate", append(append([]string{}, setup...), []string{
		`SELECT grp, (SELECT v FROM (SELECT sum(val) AS v UNION ALL SELECT sum(val)*2 AS v)) FROM g GROUP BY grp`,
	}...))
	differ(t, "same, three derived-table hops deep", append(append([]string{}, setup...), []string{
		`SELECT grp, (SELECT v FROM (SELECT v2 AS v FROM (SELECT v3 AS v2 FROM (SELECT sum(val) AS v3 UNION ALL SELECT sum(val)*2 AS v3)))) FROM g GROUP BY grp`,
	}...))
}
