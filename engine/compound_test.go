package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"path/filepath"
	"testing"
)

// buildCompoundTestDB creates tables for compound SELECT testing (UNION, INTERSECT, EXCEPT).
func buildCompoundTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compound.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	exec(`CREATE TABLE ca (a INTEGER, b TEXT)`)
	type crow struct {
		a any
		b any
	}
	for _, r := range []crow{
		{1, "x"},
		{1, "x"}, // exact duplicate within ca
		{2, nil},
		{nil, nil},
		{nil, nil}, // all-NULL duplicate within ca
		{3, "y"},
		{4, "z"}, // only in ca, not cb
	} {
		exec(`INSERT INTO ca (a,b) VALUES (?,?)`, r.a, r.b)
	}

	exec(`CREATE TABLE cb (a INTEGER, b TEXT)`)
	for _, r := range []crow{
		{1, "x"},
		{2, nil},
		{5, "w"}, // only in cb, not ca
		{nil, nil},
		{3, "y"},
		{3, "y"}, // exact duplicate within cb
	} {
		exec(`INSERT INTO cb (a,b) VALUES (?,?)`, r.a, r.b)
	}

	exec(`CREATE TABLE cc (a INTEGER, b TEXT)`)
	for _, r := range []crow{
		{4, "z"},
		{6, "v"},
	} {
		exec(`INSERT INTO cc (a,b) VALUES (?,?)`, r.a, r.b)
	}

	exec(`CREATE TABLE wide_a (a INTEGER)`)
	exec(`INSERT INTO wide_a (a) VALUES (1)`)
	exec(`CREATE TABLE wide_b (a INTEGER, b INTEGER)`)
	exec(`INSERT INTO wide_b (a,b) VALUES (1,2)`)

	return path, db
}

// TestCompoundSelect compares the pure-Go engine's compound SELECT execution
// (sql_compound.go) against the same SQL run through database/sql against
// a reference: UNION ALL's duplicate-
// preserving concatenation, UNION's dedup (including the NULL-equal
// grouping rule, same as DISTINCT/GROUP BY -- an all-NULL row present in
// both operands collapses to one), INTERSECT/EXCEPT set semantics, a
// left-to-right associative chain of more than two arms, ORDER BY and LIMIT/
// OFFSET applied to the combined result (never to a single arm), ORDER BY
// by result-column position and by name, and GROUP BY/aggregate/DISTINCT
// arms feeding into a compound.
func TestCompoundSelect(t *testing.T) {
	path, db := buildCompoundTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// UNION ALL: plain concatenation, duplicates preserved (including ca's
		// own internal duplicate rows).
		`SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb`,

		// UNION: dedup across both operands. The all-NULL row appears (at
		// least) twice in ca and once in cb -- it must collapse to a single
		// output row (NULL-equal dedup, not "=" semantics).
		`SELECT a, b FROM ca UNION SELECT a, b FROM cb`,

		// INTERSECT: rows present in both ca and cb, deduped -- including the
		// all-NULL row, present in both.
		`SELECT a, b FROM ca INTERSECT SELECT a, b FROM cb`,

		// EXCEPT: rows in ca not present in cb (deduped): only (4,'z') should
		// survive.
		`SELECT a, b FROM ca EXCEPT SELECT a, b FROM cb`,

		// EXCEPT is not commutative: rows in cb not in ca.
		`SELECT a, b FROM cb EXCEPT SELECT a, b FROM ca`,

		// A single-column projection still applies NULL-equal dedup.
		`SELECT b FROM ca UNION SELECT b FROM cb`,

		// Three-arm chain, left-to-right associative:
		// (ca UNION ALL cb) EXCEPT cc: (4,'z') is removed by EXCEPT cc since
		// cc contains it, leaving the concatenated-and-deduped remainder.
		`SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb EXCEPT SELECT a, b FROM cc`,

		// Same three-arm shape with UNION (not UNION ALL) as the first
		// connective: (ca UNION cb) EXCEPT cc -- left-to-right, never
		// "ca UNION (cb EXCEPT cc)" -- verified directly against the
		// reference engine.
		`SELECT a, b FROM ca UNION SELECT a, b FROM cb EXCEPT SELECT a, b FROM cc`,

		// ORDER BY on a compound: resolves against the leftmost SELECT's
		// result-column names, by name and by ordinal position, ASC/DESC,
		// applied to the whole combined+deduped result.
		`SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a`,
		`SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a DESC, b ASC`,
		`SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY 1, 2`,
		`SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb ORDER BY a, b`,

		// LIMIT/OFFSET on a compound: applied to the final combined (and, if
		// present, ordered) result -- never to a single arm.
		`SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a, b LIMIT 3`,
		`SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a, b LIMIT 3 OFFSET 2`,
		`SELECT a, b FROM ca UNION ALL SELECT a, b FROM cb LIMIT 5`,

		// DISTINCT within one arm, still combined and (for UNION) further
		// deduped across arms.
		`SELECT DISTINCT a FROM ca UNION SELECT a FROM cb`,

		// Aggregate arms: each arm is its own one-row aggregate result: two
		// single-row results combined by the set operator.
		`SELECT count(*) FROM ca UNION SELECT count(*) FROM cb`,
		`SELECT count(*) FROM ca UNION ALL SELECT count(*) FROM cb`,

		// GROUP BY arm combined with a plain arm (same column shape: a and an
		// aggregate).
		`SELECT a, count(*) FROM ca GROUP BY a UNION SELECT a, count(*) FROM cb GROUP BY a`,

		// WHERE narrows each arm before combining.
		`SELECT a, b FROM ca WHERE a > 1 UNION SELECT a, b FROM cb WHERE a > 1`,

		// A constant (FROM-less) arm combined with a table-backed arm.
		`SELECT 4, 'z' UNION SELECT a, b FROM ca`,

		// Compound as a scalar/IN/EXISTS subquery: evalSubquery/evalIn/
		// evalExists reenter execSelect generically, so a compound in any of
		// those positions must work exactly like a single-arm subquery.
		`SELECT a FROM ca WHERE a IN (SELECT a FROM ca UNION SELECT a FROM cb)`,
		`SELECT EXISTS (SELECT a FROM ca INTERSECT SELECT a FROM cc)`,
		`SELECT (SELECT a FROM ca UNION ALL SELECT a FROM cb ORDER BY a LIMIT 1)`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// TestCompoundSelectRejectsColumnCountMismatch verifies that a compound
// SELECT whose arms return different numbers of result columns is rejected
// at execution time (an error) rather than combined incorrectly (e.g.
// silently truncating/padding one side) -- the "never wrong" invariant means
// this must fail loudly, not guess.
func TestCompoundSelectRejectsColumnCountMismatch(t *testing.T) {
	path, _ := buildCompoundTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT a FROM wide_a UNION SELECT a, b FROM wide_b`,
		`SELECT a, b FROM wide_b UNION ALL SELECT a FROM wide_a`,
		`SELECT a FROM wide_a INTERSECT SELECT a, b FROM wide_b`,
		`SELECT a FROM wide_a EXCEPT SELECT a, b FROM wide_b`,
	} {
		mustError(t, p, q, "compound SELECT arms with mismatched column counts")
	}
}

// TestCompoundSelectRejectsUnsupportedOrderByTerm verifies that an ORDER BY
// term on a compound SELECT which isn't a result-column position or name --
// an arbitrary recomputed expression -- is rejected rather than silently
// misevaluated (there is no per-row table context left to evaluate it
// against once every arm's rows have been flattened into scalar result
// tuples; see resolveCompoundOrderIndex, sql_compound.go).
func TestCompoundSelectRejectsUnsupportedOrderByTerm(t *testing.T) {
	path, _ := buildCompoundTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	mustError(t, p, `SELECT a, b FROM ca UNION SELECT a, b FROM cb ORDER BY a + 1`,
		"ORDER BY on a compound SELECT with a non-column/non-ordinal term")
}
