package compat

// Derived tables and compound arms can reference outer columns; no caching.

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildCorrelatedOuterScopeDB builds an outer table whose rows each select a
// DIFFERENT subset of the inner table, so a correlated body materialized once
// and reused cannot pass. outer.a is 1,2,3 and inner.f is 4,8: "f > a*3"
// matches 2 rows, then 1, then 0.
func buildCorrelatedOuterScopeDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/correlated_outer_scope.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	exec := func(sqlText string) {
		t.Helper()
		if err := db.Exec(sqlText); err != nil {
			t.Fatalf("Exec(%s): %v", sqlText, err)
		}
	}
	exec(`CREATE TABLE outr (a INTEGER, b INTEGER)`)
	exec(`INSERT INTO outr VALUES (1, 10)`)
	exec(`INSERT INTO outr VALUES (2, 20)`)
	exec(`INSERT INTO outr VALUES (3, 30)`)
	exec(`CREATE TABLE innr (d INTEGER, f INTEGER)`)
	exec(`INSERT INTO innr VALUES (1, 4)`)
	exec(`INSERT INTO innr VALUES (2, 8)`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// correlatedOuterScopeCorpus is (statement, order-sensitive) -- the same shape
// vdbeCorrelatedCorpus uses. Every one must COMPILE (QueryVDBE must not
// decline) and must match C SQLite exactly.
var correlatedOuterScopeCorpus = []subCase{
	// --- a derived table inside a correlated scalar subquery ---
	// 2 / 1 / 0: a materialize-once derived table answers 2 / 2 / 2.
	{"SELECT a, (SELECT count(*) FROM (SELECT f FROM innr WHERE f > a*3)) FROM outr ORDER BY a", true},
	{"SELECT a, (SELECT group_concat(f) FROM (SELECT f FROM innr WHERE f > a*3)) FROM outr ORDER BY a", true},
	// The correlated reference is one level deeper still (derived inside
	// derived), so the outward walk has to cross two FROM barriers.
	{"SELECT a, (SELECT count(*) FROM (SELECT * FROM (SELECT f FROM innr WHERE f > a*3))) FROM outr ORDER BY a", true},
	// The correlated column feeds an EXPRESSION of the derived select list
	// rather than its WHERE.
	{"SELECT a, (SELECT sum(z) FROM (SELECT f*a AS z FROM innr)) FROM outr ORDER BY a", true},
	// A derived table under EXISTS and under IN, not only under a scalar
	// subquery -- three different opcodes decide cached-vs-re-run.
	{"SELECT a FROM outr WHERE EXISTS (SELECT 1 FROM (SELECT f FROM innr WHERE f = a*4)) ORDER BY a", true},
	{"SELECT a FROM outr WHERE b IN (SELECT f*5 FROM (SELECT f FROM innr WHERE f > a*3)) ORDER BY a", true},
	// A QUALIFIED reference to an outer DERIVED table's own alias -- the
	// tkt3346.test shape, where the enclosing scope is itself a derived table.
	{"SELECT b FROM (SELECT * FROM outr) AS x WHERE (SELECT y FROM (SELECT x.b = 10 AS y)) = 0 ORDER BY b", true},
	{"SELECT x.a, (SELECT y FROM (SELECT x.b + 1 AS y)) FROM (SELECT * FROM outr) AS x ORDER BY x.a", true},

	// --- a compound arm referencing an enclosing query's column ---
	{"SELECT a, (SELECT count(*) FROM (SELECT f FROM innr WHERE f > a*3 UNION SELECT 99)) FROM outr ORDER BY a", true},
	{"SELECT a FROM outr WHERE NOT EXISTS (SELECT f FROM innr WHERE f > a*3 EXCEPT SELECT 4) ORDER BY a", true},
	{"SELECT a FROM outr WHERE b IN (SELECT 10 INTERSECT SELECT b) ORDER BY a", true},
	{"SELECT a, (SELECT max(q) FROM (SELECT f AS q FROM innr WHERE f > a*3 UNION ALL SELECT a)) FROM outr ORDER BY a", true},
	// The compound is the correlated body itself (not nested in a derived
	// table), reached through OpSubquery rather than OpOpenDerived.
	{"SELECT a, (SELECT a UNION SELECT 999) FROM outr ORDER BY a", true},
}

// TestCorrelatedOuterScopeParity is the hard gate: every statement compiles to
// bytecode and agrees with C SQLite cell for cell.
func TestCorrelatedOuterScopeParity(t *testing.T) {
	path := buildCorrelatedOuterScopeDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong := 0
	for _, tc := range correlatedOuterScopeCorpus {
		vCols, vVals, vErr := p.QueryArgs(tc.sql, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error (this shape must compile, not decline)\n  vdbe=%v\n  cgo=%v", tc.sql, vErr, cErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				tc.sql, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("correlated outer-scope gate: %d statements, wrong=%d", len(correlatedOuterScopeCorpus), wrong)
	if wrong != 0 {
		t.Fatalf("correlated outer-scope gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestCorrelatedOuterScopeNoLateral pins the NEGATIVE half of the rule: a
// derived table sees an ENCLOSING query's columns but never its OWN query's
// sibling FROM items -- SQL has no LATERAL, and C SQLite rejects each of
// these with "no such column"/"no such table". Both engines must reject; only
// the wording differs, which the mined-corpus gate scores as mutual agreement.
//
// Without this, "resolve outward" could quietly become "resolve anywhere" and
// start answering a statement C SQLite refuses -- a wrong answer, not a gap.
func TestCorrelatedOuterScopeNoLateral(t *testing.T) {
	path := buildCorrelatedOuterScopeDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		`SELECT * FROM outr, (SELECT outr.a)`,
		`SELECT * FROM outr, (SELECT f FROM innr WHERE f = outr.a)`,
		`SELECT * FROM outr AS o JOIN (SELECT o.a AS z) ON 1`,
		`SELECT * FROM outr UNION SELECT * FROM (SELECT innr.d, outr.b FROM innr)`,
	} {
		_, _, vErr := p.QueryArgs(sqlText, nil)
		_, _, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr == nil {
			t.Fatalf("[%s] fixture is wrong: C SQLite ACCEPTED it, so it does not test the no-LATERAL rule", sqlText)
		}
		if vErr == nil {
			t.Errorf("[%s] engine ACCEPTED a statement C SQLite rejects (%v) -- a derived table must not see its own query's FROM items", sqlText, cErr)
			continue
		}
		if !strings.Contains(vErr.Error(), "no such column") && !strings.Contains(vErr.Error(), "no such table") {
			t.Errorf("[%s] rejected, but not as a name-resolution failure: %v", sqlText, vErr)
		}
	}
}
