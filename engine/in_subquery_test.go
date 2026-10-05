// Tests for IN subqueries.
package engine_test

import (
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestInSubqueryMatchesReference(t *testing.T) {
	path, db := buildSubqueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		`SELECT a FROM t1 WHERE a IN (SELECT a FROM t2)`,
		`SELECT a FROM t1 WHERE a NOT IN (SELECT a FROM t2)`,

		// outer row.
		`SELECT a FROM t1 WHERE b IN (SELECT b FROM t2 WHERE t2.a <= t1.a)`,
		`SELECT a FROM t1 WHERE b NOT IN (SELECT b FROM t2 WHERE t2.a <= t1.a)`,

		// row) is NULL regardless of the subquery's contents.
		`SELECT a, c FROM t1 WHERE c IN (SELECT b FROM t2 WHERE t2.a <= t1.a)`,
		`SELECT a, c FROM t1 WHERE c NOT IN (SELECT b FROM t2 WHERE t2.a <= t1.a)`,

		// The full unfiltered subquery result set contains a NULL (t2.b's
		// a=4 row): any outer value present in the set is still a definite
		// match (a match wins over NULL-ambiguity); anything absent becomes
		// NULL, never false, because of that NULL member.
		`SELECT a, b FROM t2 WHERE b IN (SELECT b FROM t2)`,
		`SELECT a, b FROM t2 WHERE b NOT IN (SELECT b FROM t2)`,

		// Empty subquery result (t3 is always empty): IN is always false
		// and NOT IN is always true, even for outer rows whose compared
		// column is itself NULL (t1's a=3 row has NULL b) -- an empty set
		// has nothing to be null-ambiguous about.
		`SELECT a FROM t1 WHERE a IN (SELECT x FROM t3)`,
		`SELECT a FROM t1 WHERE a NOT IN (SELECT x FROM t3)`,
		`SELECT a, b FROM t1 WHERE b NOT IN (SELECT x FROM t3)`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}

// TestInSubqueryMultiColumnIsError documents that "x IN (SELECT ...)"
// requires the subquery to return exactly one column -- an error, never a
// guess -- exactly like a scalar SubqueryExpr (see subquery_test.go's
// TestSubqueryExprKnownUnsupported).
func TestInSubqueryMultiColumnIsError(t *testing.T) {
	path, _ := buildSubqueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	mustError(t, p, `SELECT a FROM t1 WHERE a IN (SELECT a, b FROM t2)`,
		"an IN subquery returning more than one result column is an error, not a guess")
}

// TestInTableNameMatchesReference tests SQLite's other documented form of
// IN's right-hand side, a bare table name ("X [NOT] IN <table-name>",
// https://www.sqlite.org/lang_expr.html#the_in_and_not_in_operators): the
// parser desugars it (parseInRHS, sql_parser.go) into the exact equivalent
// subquery form SQLite itself documents ("IN (SELECT * FROM <table-name>)"),
// so it shares evalIn's membership/NULL semantics exactly -- these cases
// mirror TestInSubqueryMatchesReference's t3 (always-empty) coverage plus
// t4 (single column, with a NULL member) to exercise a non-empty set whose
// three-valued NULL handling actually matters.
func TestInTableNameMatchesReference(t *testing.T) {
	path, db := buildSubqueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// t4 = {2, 4, NULL}: a=2/4 are definite matches (win over the NULL
		// member); a=1/3/5 are absent, but the NULL member makes that
		// unprovable -> NULL, never false. NOT IN negates the definite
		// matches and leaves NULL as NULL.
		`SELECT a FROM t1 WHERE a IN t4`,
		`SELECT a FROM t1 WHERE a NOT IN t4`,
		`SELECT a, a IN t4 FROM t1`,
		`SELECT a, a NOT IN t4 FROM t1`,

		// Also exercise a NULL left operand against a non-empty set (t1.b has
		// a NULL row, a=3): still NULL, same three-valued rule.
		`SELECT a, b IN t4 FROM t1`,

		// Empty table (t3): IN is always false, NOT IN always true, even for
		// an outer NULL (t1.b's a=3 row) -- an empty set has nothing to be
		// null-ambiguous about.
		`SELECT a FROM t1 WHERE a IN t3`,
		`SELECT a FROM t1 WHERE a NOT IN t3`,
		`SELECT a, b NOT IN t3 FROM t1`,

		// A literal (non-column) left operand, not just a bare column.
		`SELECT 2 IN t4`,
		`SELECT 2 NOT IN t4`,
		`SELECT null IN t4`,
		`SELECT null IN t3`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}

// TestInTableNameMultiColumnIsError documents that "X IN <table-name>"
// requires the named table to have exactly one column -- exactly like the
// desugared subquery form it's built on (TestInSubqueryMultiColumnIsError)
// -- rejected as an error rather than guessed at (e.g. by picking some
// arbitrary column).
func TestInTableNameMultiColumnIsError(t *testing.T) {
	path, _ := buildSubqueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	mustError(t, p, `SELECT a FROM t2 WHERE a IN t1`,
		"IN <table-name> requires the named table to have exactly one column, t1 has three")
	mustError(t, p, `SELECT a FROM t2 WHERE a NOT IN t1`,
		"NOT IN <table-name> requires the named table to have exactly one column, t1 has three")
}
