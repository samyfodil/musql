package engine_test

import (
	"testing"

	"github.com/samyfodil/musql/engine"
)

// Tests pure-engine features: FROM-less aggregates, ALL quantifier, and GROUP BY matching.

// TestFromLessAggregate exercises execNoFromAggregate: a FROM-less SELECT
// whose select-list contains an aggregate call.
func TestFromLessAggregate(t *testing.T) {
	path, db := buildAggTestDB(t) // no table is actually touched; any engine.DB will do
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT count(*)`,
		`SELECT count(*), sum(5), avg(3), count(1)`,
		`SELECT count(*) WHERE 1=0`,
		`SELECT count(*) WHERE 1=1`,
		`SELECT count(*) WHERE NULL`,
		`SELECT count(*) LIMIT 0`,
		`SELECT count(*) LIMIT 1 OFFSET 1`,
		`SELECT sum(5)`,
		`SELECT -count(*) * -31`,
		`SELECT coalesce(sum(5), 0)`,
		`SELECT count(*) AS n, sum(5) AS s`,
	} {
		t.Run(q, func(t *testing.T) { mustMatch(t, p, db, q) })
	}

	// A bare column reference has no FROM clause to name a column of --
	// still rejected (error), never guessed at.
	mustError(t, p, `SELECT count(*), x`, "no FROM clause: x has no table to resolve against")
}

// TestAggregateWrappedExpression exercises queryAggregate's (sql_agg.go)
// generalized item planning: a select-list item may combine an aggregate
// call with operators/scalar functions, not just be the aggregate call
// verbatim.
func TestAggregateWrappedExpression(t *testing.T) {
	path, db := buildAggTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT -count(*) * -31 FROM nums`,
		`SELECT count(*) + sum(x) FROM nums`,
		`SELECT coalesce(sum(x), 0) FROM nums`,
		`SELECT coalesce(sum(x), 0) FROM empty_nums`,
		`SELECT CASE WHEN count(*) > 3 THEN 'many' ELSE 'few' END FROM nums`,
		`SELECT - MAX(x) / COUNT(*) FROM nums`,
	} {
		t.Run(q, func(t *testing.T) { mustMatch(t, p, db, q) })
	}
}

// TestFunctionCallAllQuantifier exercises "ALL" as a function-call
// argument-list quantifier (sql_parser.go): a pure no-op synonym for
// omitting the quantifier entirely, verified to behave identically to the
// unquantified form and distinctly from DISTINCT.
func TestFunctionCallAllQuantifier(t *testing.T) {
	path, db := buildAggTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT count(ALL x) FROM nums`,
		`SELECT sum(ALL x) FROM nums`,
		`SELECT - COUNT(ALL + + x) FROM nums`,
		`SELECT MIN(ALL - - x) FROM nums`,
		`SELECT MAX(ALL x) / COUNT(*) FROM nums`,
	} {
		t.Run(q, func(t *testing.T) { mustMatch(t, p, db, q) })
	}
}

// TestStarAcrossJoinedTablesSharingColumnNames exercises expandSelectList's
// per-table-qualified "*" expansion: t1/t2 (buildJoinTestDB) both declare an
// "id" column, so an unqualified "*" across both must not trip the same
// "ambiguous column name" check a genuinely unqualified reference to "id"
// correctly does (TestJoinAmbiguousColumnErrors) -- "*" always means "every
// joined table's own columns", never a name that gets re-resolved.
func TestStarAcrossJoinedTablesSharingColumnNames(t *testing.T) {
	path, db := buildJoinTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT * FROM t1, t2`,
		`SELECT * FROM t1 JOIN t2 ON t1.id = t2.t1_id`,
		`SELECT DISTINCT * FROM t1, t2`,
		`SELECT * FROM t1, t2, t3`,
	} {
		t.Run(q, func(t *testing.T) { mustMatchGroup(t, p, db, q) })
	}
}

// TestGroupByColumnIdentityMatch exercises rewriteGroupExpr's resolved-
// column-identity GROUP BY key match (sql_group.go): a bare, unqualified
// column reference elsewhere in the select-list is accepted whenever it
// resolves to the exact same physical column as one of the (possibly
// qualified) GROUP BY expressions -- not only when it is textually
// identical to one.
func TestGroupByColumnIdentityMatch(t *testing.T) {
	path, db := buildGroupTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		// GROUP BY qualifies the column; select-list references it bare.
		`SELECT category FROM sales GROUP BY sales.category`,
		`SELECT category || '!' FROM sales GROUP BY sales.category`,
		`SELECT category, count(*) FROM sales GROUP BY sales.category`,
		// GROUP BY leaves it bare; select-list qualifies it.
		`SELECT sales.category FROM sales GROUP BY category`,
		// A per-row expression mixing both spellings of the same column.
		`SELECT category || sales.category FROM sales GROUP BY category`,
		`SELECT sales.category || category FROM sales GROUP BY sales.category`,
	} {
		t.Run(q, func(t *testing.T) { mustMatchGroup(t, p, db, q) })
	}

	// A bare column that resolves to NO GROUP BY expression is no longer an
	// error: it is SQLite's bare-column extension, anchored to the group's
	// first row here (no min/max in the query) -- see
	// aggAccumulators.anchorRow (vdbe_agg.go). Checked against the reference
	// engine rather than asserted to fail.
	mustMatchGroup(t, p, db, `SELECT region FROM sales GROUP BY category`)
}

// TestStarWithGroupBy exercises "*" in a GROUP BY query's select-list
// accepted both when "*" expands to exactly the GROUP BY expressions (every
// output column IS a grouping key) and when it pulls in ungrouped columns
// (each resolved against the group's anchor row -- SQLite's bare-column
// extension). Both forms are verified against the reference engine.
func TestStarWithGroupBy(t *testing.T) {
	path, db := buildGroupTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT * FROM sales GROUP BY id, category, region, amount, qty`,
		`SELECT DISTINCT * FROM sales GROUP BY id, category, region, amount, qty`,
		`SELECT * FROM sales GROUP BY sales.id, category, region, amount, qty`,
	} {
		t.Run(q, func(t *testing.T) { mustMatchGroup(t, p, db, q) })
	}

	// "*" pulling in ungrouped columns (id/region/amount/qty) is likewise no
	// longer rejected -- each takes its value from the group's anchor row,
	// exactly as C SQLite does.
	mustMatchGroup(t, p, db, `SELECT * FROM sales GROUP BY category`)
}
