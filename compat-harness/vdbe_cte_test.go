// Tests WITH clause (common table expressions) compilation and execution.
// resolveCTERows machinery at OPEN time to actually produce rows -- and, for
// a recursive CTE, a queue program compiled with the statement
// (compileRecursiveCTE). TestCTEStatementsMatchCSQLite below asserts BYTE-EXACT
// row parity (queryCTEMatches) for every WITH-clause query shape C SQLite
// itself accepts, and mutual rejection (queryCTERejected) for every shape
// C SQLite itself rejects (an invalid CTE definition -- column-count
// mismatch, a genuine self-reference cycle, a CTE body naming a table that
// doesn't exist) -- exact error text is not required there (this package's
// error text for those cases doesn't always match C SQLite's verbatim;
// see cteSchemaCols' own doc comment, vdbe_join_codegen.go, for the one case
// that DOES now match: a genuine reference cycle, "circular reference: %s").
// TestCTEDuplicateNameErrorTextMatches is the one already-exact-match case,
// caught by the PARSER (sql_parser.go) before either engine's execution
// begins at all.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// execCTEBoth runs sqlText (a DDL/DML setup statement) against both the
// pure-Go engine writer and a live real-SQLite connection, requiring
// identical success/failure and, on failure, identical error text (this
// package's own "engine: " prefix stripped) -- mirrors vdbe_view_test.go's
// execViewBoth.
func execCTEBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
	t.Helper()
	engErr := db.Exec(sqlText)
	_, realErr := sdb.Exec(sqlText)
	if (engErr == nil) != (realErr == nil) {
		t.Fatalf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engErr, realErr)
	}
	if engErr == nil {
		return
	}
	engMsg := strings.TrimPrefix(engErr.Error(), "engine: ")
	realMsg := realErr.Error()
	if engMsg != realMsg {
		t.Fatalf("Exec(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", sqlText, engMsg, realMsg)
	}
}

// queryCTEMatches confirms q -- a WITH-clause query C SQLite accepts --
// is not just accepted by the integrated engine path too (a freshly
// snapshotted pager, so it observes every mutation made so far this session
// -- see (*engine.Session).SnapshotPager), but produces BYTE-EXACT matching
// result columns/rows (queryResultsMatch, pureengine_test.go). ordered
// selects positional (row-order-sensitive) comparison -- required for a
// recursive CTE's own fixed-point evaluation order, and for an explicit
// outer ORDER BY -- versus unordered (set) comparison otherwise.
func queryCTEMatches(t *testing.T, db *engine.Session, sdb *sql.DB, q string, ordered bool) {
	t.Helper()
	pager, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	gotCols, gotRows, gotErr := pager.Query(q)
	wantCols, wantRows, wantErr := cgoSelect(t, sdb, q, nil)
	if wantErr != nil {
		t.Fatalf("Query(%s): C SQLite unexpectedly errored (%v) -- re-verify this case belongs in the accepted matrix", q, wantErr)
	}
	if gotErr != nil {
		t.Fatalf("Query(%s): engine unexpectedly declined (%v); C SQLite accepts this shape", q, gotErr)
	}
	gotStrRows := engineRowsToStrings(gotRows)
	if ok, reason := queryResultsMatch(gotCols, gotStrRows, wantCols, wantRows, ordered); !ok {
		t.Fatalf("Query(%s): result mismatch: %s\n  engine: cols=%v rows=%v\n  real:   cols=%v rows=%v", q, reason, gotCols, gotStrRows, wantCols, wantRows)
	}
}

// queryCTERejected confirms q -- a WITH-clause query C SQLite itself
// REJECTS (an invalid CTE definition, never a capability gap) -- is also
// rejected by the integrated engine path, so this package's own "never wrong"
// contract holds even for a shape it has no reason to accept. Exact error
// text is NOT required (this package's own CTE-body validation doesn't
// always reproduce C SQLite's wording verbatim -- see this file's package
// doc comment).
func queryCTERejected(t *testing.T, db *engine.Session, sdb *sql.DB, q string) {
	t.Helper()
	pager, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	_, _, gotErr := pager.Query(q)
	_, _, wantErr := cgoSelect(t, sdb, q, nil)
	if wantErr == nil {
		t.Fatalf("Query(%s): C SQLite unexpectedly accepted this invalid-CTE shape -- re-verify this case", q)
	}
	if gotErr == nil {
		t.Fatalf("Query(%s): engine unexpectedly accepted a CTE shape C SQLite rejects (%v) -- this would be a WRONG answer, not a decline", q, wantErr)
	}
}

// TestCTEStatementsMatchCSQLite is the WITH-clause conformance gate, at
// page sizes 512 and 4096.
func TestCTEStatementsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testCTEScenario(t, pageSize)
		})
	}
}

func testCTEScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("cte_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Discard()

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // a single logical connection, so every setup/query call below sees the SAME in-memory schema

	// ---- base tables ----
	for _, s := range []string{
		`CREATE TABLE t1(a INTEGER, b TEXT)`,
		`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE t2(a INTEGER, c TEXT)`,
		`INSERT INTO t2 VALUES(1,'p'),(2,'q')`,
		`CREATE TABLE edges(parent INTEGER, child INTEGER)`,
		`INSERT INTO edges VALUES (1,2),(1,3),(2,4),(2,5),(3,6),(3,7)`,
		`CREATE TABLE cyc(a INTEGER, b INTEGER)`,
		`INSERT INTO cyc VALUES (1,2),(2,3),(3,1)`,
	} {
		execCTEBoth(t, db, sdb, s)
	}

	// ---- query matrix: every statement C SQLite ACCEPTS must match it
	// byte-exact (queryCTEMatches). ordered is true whenever row order is
	// significant (an explicit outer ORDER BY, or a recursive CTE's own
	// queue order -- see engine/vdbe_recursive_cte.go). ----
	for _, tc := range []struct {
		q       string
		ordered bool
	}{
		// single CTE
		{`WITH c AS (SELECT a, b FROM t1 WHERE a > 1) SELECT * FROM c`, false},

		// multiple CTEs, one referencing another (both directions --
		// verified directly against C SQLite that listing order doesn't
		// matter for a plain, non-recursive WITH clause)
		{`WITH c1 AS (SELECT a FROM t1 WHERE a > 1), c2 AS (SELECT a FROM c1 WHERE a < 3) SELECT * FROM c2`, false},
		{`WITH c2 AS (SELECT * FROM c1), c1 AS (SELECT a FROM t1) SELECT * FROM c2`, false},

		// explicit column-rename list
		{`WITH c(x,y) AS (SELECT a, b FROM t1) SELECT * FROM c`, false},

		// CTE shadows a real table of the same name
		{`WITH t1(x) AS (SELECT 99) SELECT * FROM t1`, false},

		// CTE joined with a real table
		{`WITH c AS (SELECT a, b FROM t1) SELECT c.a, t2.c FROM c JOIN t2 ON c.a = t2.a`, false},

		// CTE referenced twice (two aliases) in one query
		{`WITH c AS (SELECT a FROM t1) SELECT c1.a, c2.a FROM c AS c1, c AS c2 WHERE c1.a = c2.a`, false},

		// recursive CTE: classic linear counter, with and without the literal
		// RECURSIVE keyword (verified directly against C SQLite that both
		// are evaluated identically -- see cte.go's detectRecursiveShape)
		{`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 5) SELECT * FROM c`, true},
		{`WITH c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 5) SELECT * FROM c`, true},

		// recursive CTE: UNION (not ALL) dedups against the whole
		// accumulated set, which is also what makes a cyclic graph
		// traversal terminate instead of looping forever
		{`WITH RECURSIVE c(n) AS (SELECT 1 UNION SELECT cyc.b FROM c, cyc WHERE cyc.a = c.n) SELECT * FROM c`, false},

		// recursive CTE: graph traversal via a real join in the recursive arm
		{`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT edges.child FROM c, edges WHERE edges.parent = c.n) SELECT * FROM c`, false},

		// recursive CTE: outer ORDER BY/LIMIT applied to the fully-materialized result
		{`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 100) SELECT * FROM c ORDER BY n DESC LIMIT 3`, true},
	} {
		queryCTEMatches(t, db, sdb, tc.q, tc.ordered)
	}

	// ---- invalid CTE shapes: C SQLite itself REJECTS every one of
	// these; the engine must decline too (never silently accept and answer
	// wrong), though not necessarily with the same error text. ----
	for _, q := range []string{
		// column-count-mismatch error (explicit list disagrees with the
		// SELECT's own column count)
		`WITH c(x,y,z) AS (SELECT a, b FROM t1) SELECT * FROM c`,
		`WITH c(x) AS (SELECT a, b FROM t1) SELECT * FROM c`,

		// self-reference outside the recognized recursive shape: real
		// SQLite itself rejects this as a "circular reference" -- and so
		// does the engine, with the IDENTICAL wording (cteSchemaCols'
		// compile-time circularity guard, vdbe_join_codegen.go).
		`WITH c AS (SELECT * FROM c) SELECT * FROM c`,

		// a CTE referencing a table that doesn't exist: C SQLite itself
		// rejects this as "no such table".
		`WITH c AS (SELECT * FROM nosuchtable) SELECT * FROM c`,
	} {
		queryCTERejected(t, db, sdb, q)
	}
}

// TestCTEDuplicateNameErrorTextMatches confirms the one CTE-query error text
// still guaranteed to match exactly between the two engines: a duplicate
// WITH-table name is caught by the PARSER (sql_parser.go), before either
// engine's execution even begins -- so this one still surfaces the
// identical, CTE-specific error message on both engines.
func TestCTEDuplicateNameErrorTextMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cte_dupname.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Discard()

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	pager, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	const q = `WITH c AS (SELECT 1), c AS (SELECT 2) SELECT * FROM c`
	_, _, gotErr := pager.Query(q)
	_, _, wantErr := cgoSelect(t, sdb, q, nil)
	if gotErr == nil || wantErr == nil {
		t.Fatalf("Query(%s): expected both engines to reject a duplicate WITH-table name: engine=%v real=%v", q, gotErr, wantErr)
	}
	gotMsg := strings.TrimPrefix(gotErr.Error(), "engine: ")
	wantMsg := wantErr.Error()
	if gotMsg != wantMsg {
		t.Fatalf("Query(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", q, gotMsg, wantMsg)
	}
}
