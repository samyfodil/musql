package compat

// Gates "X [NOT] IN (SELECT ...)" membership tests, including collation resolution
// from both operands and row-value comparisons under per-column affinity and collation.

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// inSubqueryFixture: base tables with NULLs, affinity mix, and collation variants.
var inSubqueryFixture = []string{
	`CREATE TABLE t1(a INTEGER, b INTEGER, c TEXT)`,
	`INSERT INTO t1 VALUES(1,10,'x'),(2,20,'y'),(3,30,'z'),(4,NULL,'w')`,
	`CREATE TABLE t2(a INTEGER, b INTEGER)`,
	`INSERT INTO t2 VALUES(1,10),(3,30),(4,NULL)`,
	`CREATE TABLE ta(i INTEGER, t TEXT)`,
	`INSERT INTO ta VALUES(5,'5')`,
	`CREATE TABLE tc(x TEXT COLLATE NOCASE, y TEXT, z TEXT COLLATE RTRIM)`,
	`INSERT INTO tc VALUES('ABC','abc','GHI  ')`,
}

func inSubqueryCase(t *testing.T, stmts ...string) {
	t.Helper()
	full := append(append([]string(nil), inSubqueryFixture...), stmts...)
	if !differ(t, "insub/"+stmts[0], full) {
		t.Errorf("diverged on: %v", stmts)
	}
}

// TestInSubqueryCollationMatchesCSQLite gates collation resolution for IN subqueries.
func TestInSubqueryCollationMatchesCSQLite(t *testing.T) {
	for _, q := range []string{
		// Subquery column's declared collation when X has none.
		`SELECT 'abc' IN (SELECT x FROM tc)`,
		`SELECT 'GHI' IN (SELECT z FROM tc)`,
		`SELECT 'abc' NOT IN (SELECT x FROM tc)`,
		// Explicit collation on right outranks declared on X.
		`SELECT x IN (SELECT y COLLATE RTRIM FROM tc) FROM tc`,
		`SELECT 'ABC' IN (SELECT y COLLATE NOCASE FROM tc)`,
		// Explicit collation on X outranks everything.
		`SELECT 'abc' COLLATE BINARY IN (SELECT x FROM tc)`,
		`SELECT x COLLATE RTRIM IN (SELECT y FROM tc) FROM tc`,
		// X's declared collation vs right's default BINARY.
		`SELECT x IN (SELECT y FROM tc) FROM tc`,
		`SELECT y IN (SELECT x FROM tc) FROM tc`,
		// Computed columns carry no collation.
		`SELECT 'abc' IN (SELECT upper(x) FROM tc)`,
		`SELECT 'ABC' IN (SELECT x||'' FROM tc)`,
		// Affinity tests.
		`SELECT '5' IN (SELECT i FROM ta)`,
		`SELECT 5 IN (SELECT t FROM ta)`,
	} {
		inSubqueryCase(t, q)
	}
	// Write WHERE clause tests.
	inSubqueryCase(t,
		`CREATE TABLE hit(n INTEGER)`,
		`INSERT INTO hit VALUES(0)`,
		`UPDATE hit SET n=1 WHERE 'abc' IN (SELECT x FROM tc)`,
		`SELECT n FROM hit`,
		`UPDATE hit SET n=2 WHERE 'ABC' IN (SELECT y COLLATE NOCASE FROM tc)`,
		`SELECT n FROM hit`,
		`DELETE FROM hit WHERE 'abc' IN (SELECT x FROM tc)`,
		`SELECT count(*) FROM hit`)
}

// TestRowValueInSubqueryMatchesCSQLite gates row-value membership tests with
// per-column affinity and collation, including three-valued logic with NULLs.
func TestRowValueInSubqueryMatchesCSQLite(t *testing.T) {
	for _, q := range []string{
		// Basic shapes with NULLs exercising three-valued rules.
		`SELECT * FROM t1 WHERE (a,b) IN (SELECT a,b FROM t2) ORDER BY a`,
		`SELECT * FROM t1 WHERE (a,b) NOT IN (SELECT a,b FROM t2) ORDER BY a`,
		`SELECT a, (a,b) IN (SELECT a,b FROM t2) FROM t1 ORDER BY a`,
		`SELECT a, (a,b) NOT IN (SELECT a,b FROM t2) FROM t1 ORDER BY a`,
		// Three columns, reversed pairing, correlated subquery.
		`SELECT * FROM t1 WHERE (a,b,c) IN (SELECT a,b,'x' FROM t2) ORDER BY a`,
		`SELECT * FROM t1 WHERE (b,a) IN (SELECT a,b FROM t2) ORDER BY a`,
		`SELECT * FROM t1 WHERE (a,b) IN (SELECT a,b FROM t2 WHERE b=t1.b) ORDER BY a`,
		`SELECT * FROM t1 WHERE (a,b) IN (SELECT a,b FROM t2 WHERE a>1) ORDER BY a`,
		// Empty set: FALSE for IN, TRUE for NOT IN.
		`SELECT * FROM t1 WHERE (a,b) IN (SELECT a,b FROM t2 WHERE 0) ORDER BY a`,
		`SELECT * FROM t1 WHERE (a,b) NOT IN (SELECT a,b FROM t2 WHERE 0) ORDER BY a`,
		// Combined with other operators and aggregates.
		`SELECT * FROM t1 WHERE (a,b) IN (SELECT a,b FROM t2) OR a=2 ORDER BY a`,
		`SELECT * FROM t1 WHERE NOT ((a,b) IN (SELECT a,b FROM t2)) ORDER BY a`,
		`SELECT count(*) FROM t1 WHERE (a,b) IN (SELECT a,b FROM t2)`,
		// FROM-less queries with three-valued results.
		`SELECT (1,2) IN (SELECT 1,2)`,
		`SELECT (1,2) IN (SELECT 1,3)`,
		`SELECT (1,NULL) IN (SELECT 1,2)`,
		`SELECT (1,NULL) IN (SELECT 3,4)`,
		`SELECT (1,NULL) IN (SELECT 1,2 UNION ALL SELECT 3,4)`,
		`SELECT (1,2) IN (SELECT 1,NULL)`,
		`SELECT (1,2) IN (SELECT 1,NULL UNION ALL SELECT 1,2)`,
		`SELECT (NULL,NULL) IN (SELECT 1,2)`,
		`SELECT (1,2) NOT IN (SELECT 1,3)`,
		`SELECT (1,NULL) NOT IN (SELECT 1,2)`,
		// Width mismatch: error on both engines.
		`SELECT * FROM t1 WHERE (a,b) IN (SELECT 1)`,
		`SELECT * FROM t1 WHERE (a) IN (SELECT a,b FROM t2)`,
		// Per-column affinity and collation.
		`SELECT ('5','5') IN (SELECT i,t FROM ta)`,
		`SELECT (5,5) IN (SELECT t,t FROM ta)`,
		`SELECT ('abc','abc') IN (SELECT x,y FROM tc)`,
		`SELECT ('ABC','ABC') IN (SELECT x,y FROM tc)`,
		`SELECT ('abc','ABC') IN (SELECT y,x FROM tc)`,
		`SELECT (x,y) IN (SELECT 'abc','abc') FROM tc`,
		`SELECT (x,y) IN (SELECT 'abc' COLLATE BINARY,'abc') FROM tc`,
	} {
		inSubqueryCase(t, q)
	}
	// Write WHERE clause tests.
	inSubqueryCase(t,
		`CREATE TABLE hit(n INTEGER)`,
		`INSERT INTO hit VALUES(0)`,
		`UPDATE hit SET n=1 WHERE (1,10) IN (SELECT a,b FROM t2)`,
		`SELECT n FROM hit`,
		`DELETE FROM hit WHERE (9,9) IN (SELECT a,b FROM t2)`,
		`SELECT count(*) FROM hit`)
}

// TestRowValueInSubqueryFuzz randomizes probe width, NULL placement, and collation,
// exercising the three-valued AND and per-column resolution.
func TestRowValueInSubqueryFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260728))
	// Literals: NULL for partial unknown rows, '5'/5 for affinity coercion.
	lits := []string{"1", "2", "NULL", "'x'", "'5'", "5", "'ABC'", "'abc'"}
	const iters = 300
	for i := 0; i < iters; i++ {
		width := 2 + rng.Intn(2)
		probe := make([]string, width)
		for j := range probe {
			probe[j] = lits[rng.Intn(len(lits))]
		}
		// 1-3 set rows with same width as probe.
		nrows := 1 + rng.Intn(3)
		arms := make([]string, nrows)
		for r := range arms {
			row := make([]string, width)
			for j := range row {
				row[j] = lits[rng.Intn(len(lits))]
			}
			arms[r] = "SELECT " + strings.Join(row, ",")
		}
		not := ""
		if rng.Intn(2) == 0 {
			not = "NOT "
		}
		q := fmt.Sprintf("SELECT (%s) %sIN (%s)", strings.Join(probe, ","), not, strings.Join(arms, " UNION ALL "))
		if !differ(t, fmt.Sprintf("rowin/%d", i), append(append([]string(nil), inSubqueryFixture...), q)) {
			t.Fatalf("stopping at first divergence (iteration %d): %s", i, q)
		}
	}
}
