// This file tests nested correlated aggregates: aggregates with both local
// and outer-correlated columns in their argument. Results are compared against
// C SQLite at both page sizes.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// aggNestedCase is one test statement with an ordered flag.
type aggNestedCase struct {
	sql     string
	ordered bool
}

// aggNestedCorpus holds test cases for aggregates mixing local and correlated columns.
var aggNestedCorpus = []aggNestedCase{
	// aggnested-1.3/1.4 shape: value argument LOCAL (ga2's own b1), separator
	// OUTER-correlated (ga1's a1) -- one output row per ga1 row, each using
	// that row's own a1 as separator.
	{`SELECT (SELECT group_concat(b1, a1) FROM ga2) FROM ga1 ORDER BY ga1.rowid`, true},
	// value argument OUTER-correlated (ga1's a1), separator LOCAL (ga2's b1)
	// -- association still stays local because SOME argument (the
	// separator) resolves locally.
	{`SELECT (SELECT group_concat(a1, b1) FROM ga2) FROM ga1 ORDER BY ga1.rowid`, true},

	// aggnested-4.2 shape: x OUTER-correlated (aa's own column), y LOCAL
	// (bb's own column), combined in one aggregate's argument expression.
	{`SELECT (SELECT sum(x + y) FROM bb) FROM aa`, true},
	{`SELECT (SELECT sum(x * y) FROM bb) FROM aa`, true},

	// aggnested-3.13/3.14 shape: a ROW-MODE (no GROUP BY) outer query whose
	// select list is itself a correlated scalar aggregate subquery mixing a
	// local (c2's value2) and outer (c1's value1) column -- one row per c1
	// row.
	{`SELECT value1, (SELECT sum(value2 = value1) FROM c2) FROM c1 ORDER BY c1.rowid`, true},
	{`SELECT value1, (SELECT sum(value2 = value1) FROM c2) FROM c1 WHERE value1 IN (SELECT max(value1) FROM c1 GROUP BY id1) ORDER BY c1.rowid`, true},

	// aggnested-3.2/3.3 shape: the OUTER scope is itself a derived table that
	// is a GROUP BY query using SQLite's "honored min/max" bare-column
	// extension (this package's own pre-existing, separately-verified
	// feature -- sql_group.go's findHonoredMinMax) to resolve its own bare
	// "value1 AS xyz" -- and the correlated aggregate subquery referencing
	// that derived column (xyz) still resolves correctly once combined with
	// a local column (d2's value2).
	{`SELECT (SELECT sum(value2 = xyz) FROM d2) FROM (SELECT value1 AS xyz, max(x1) AS pqr FROM d1 GROUP BY id1)`, true},
	{`SELECT (SELECT sum(value2 <> xyz) FROM d2) FROM (SELECT value1 AS xyz, max(x1) AS pqr FROM d1 GROUP BY id1)`, true},

	// A CASE expression (not just a bare BinaryExpr) mixing a local and an
	// outer column inside an aggregate's argument, over several distinct
	// outer rows -- specifically exercises that the VDBE's decline-and-
	// fall-back (rather than a stale cached value) holds across MULTIPLE
	// distinct correlated invocations, not just one.
	{`SELECT id, (SELECT sum(CASE WHEN v = val THEN 1 ELSE 0 END) FROM inner1) FROM outer1 ORDER BY id`, true},
}

// buildAggNestedDB creates tables needed for the test corpus.
func buildAggNestedDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("aggnested_%d.sqlite", pageSize))
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

	// Shape A: aggnested-1.3/1.4 (group_concat mixed value/separator).
	exec(`CREATE TABLE ga1(a1 INTEGER)`)
	exec(`INSERT INTO ga1 VALUES (1), (2), (3)`)
	exec(`CREATE TABLE ga2(b1 INTEGER)`)
	exec(`INSERT INTO ga2 VALUES (4), (5)`)

	// Shape B: aggnested-4.1/4.2 (sum(x+y) mixed).
	exec(`CREATE TABLE aa(x INTEGER)`)
	exec(`INSERT INTO aa VALUES (123)`)
	exec(`CREATE TABLE bb(y INTEGER)`)
	exec(`INSERT INTO bb VALUES (456)`)

	// Shape C: aggnested-3.13/3.14 (row-mode outer, correlated sum).
	exec(`CREATE TABLE c1(id1 INTEGER PRIMARY KEY, value1 INTEGER)`)
	exec(`INSERT INTO c1 VALUES (4469, 12), (4476, 11), (4470, 34)`)
	exec(`CREATE TABLE c2(value2 INTEGER)`)
	exec(`INSERT INTO c2 VALUES (1)`)

	// Shape D: aggnested-3.2/3.3 (GROUP BY + honored min/max + correlated).
	exec(`CREATE TABLE d1(id1 INTEGER, value1 INTEGER, x1 INTEGER)`)
	exec(`INSERT INTO d1 VALUES (4469, 2, 98), (4469, 1, 99), (4469, 3, 97)`)
	exec(`CREATE TABLE d2(value2 INTEGER)`)
	exec(`INSERT INTO d2 VALUES (1)`)

	// Shape E: CASE mixing local/outer, several distinct outer rows (VDBE
	// stale-cache stress).
	exec(`CREATE TABLE outer1(id INTEGER, val INTEGER)`)
	exec(`INSERT INTO outer1 VALUES (1, 10), (2, 20), (3, 30), (4, 10)`)
	exec(`CREATE TABLE inner1(v INTEGER)`)
	exec(`INSERT INTO inner1 VALUES (10), (20), (10), (30)`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// TestAggNestedCorrelatedResultParity verifies results match C SQLite at both page sizes.
func TestAggNestedCorrelatedResultParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testAggNestedScenario(t, pageSize)
		})
	}
}

func testAggNestedScenario(t *testing.T, pageSize int) {
	path := buildAggNestedDB(t, pageSize)

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
	total := 0
	for _, tc := range aggNestedCorpus {
		total++
		sqlText := tc.sql

		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			wrong++
			t.Errorf("[%s] C SQLite unexpectedly errored (%v) -- not a genuine engine-only gap", sqlText, cErr)
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			wrong++
			t.Errorf("[%s] QueryVDBE errored where C SQLite answered: %v", sqlText, vErr)
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, !tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, engineRowsToStrings(vVals), cCols, cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			wrong++
			t.Errorf("[%s] p.Query errored where C SQLite answered: %v", sqlText, eErr)
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, !tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
				sqlText, reason, eCols, engineRowsToStrings(eVals), cCols, cRows)
		}
	}
	t.Logf("aggnested correlated-aggregate parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("aggnested correlated-aggregate parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEAggNestedReassociation tests aggregates with no local column references,
// where C SQLite re-associates them to the enclosing query.
func TestVDBEAggNestedReassociation(t *testing.T) {
	path := buildAggNestedDB(t, 4096)

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
		`SELECT (SELECT string_agg(a1,'x') FROM ga2) FROM ga1`,
		`SELECT (SELECT group_concat(a1) FROM ga2) FROM ga1`,
		`SELECT (SELECT sum(a1) FROM ga2) FROM ga1`,
		`SELECT (SELECT max(x) FROM bb) FROM aa`,
	} {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
			continue
		}
		if len(cRows) != 1 {
			t.Errorf("[%s] C SQLite returned %d row(s), the re-association premise says 1 -- this gate's oracle evidence no longer holds", sqlText, len(cRows))
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE declined a re-associated aggregate C SQLite answers with one row: %v", sqlText, vErr)
			continue
		}
		if len(vVals) != len(cRows) {
			t.Errorf("[%s] QueryVDBE returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(vVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				sqlText, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Errorf("[%s] p.Query declined a re-associated aggregate C SQLite answers: %v", sqlText, eErr)
			continue
		}
		if len(eVals) != len(cRows) {
			t.Errorf("[%s] p.Query returned %d row(s), C SQLite %d -- a wrong ROW COUNT", sqlText, len(eVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				sqlText, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}
}
