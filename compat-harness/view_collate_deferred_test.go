// Tests that CREATE VIEW defers collation validation until the view is referenced.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestViewCollateDeferredToReference tests that collation errors in view bodies are deferred.
func TestViewCollateDeferredToReference(t *testing.T) {
	differ(t, "unknown collation in a view's own result column", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE VIEW v AS SELECT a COLLATE bogus FROM t`,
	})
	differ(t, "...and referencing it errors", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE VIEW v AS SELECT a COLLATE bogus FROM t`,
		`SELECT * FROM v`,
	})
	differ(t, "unknown collation in a view's ORDER BY", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE VIEW v AS SELECT a FROM t ORDER BY 1 COLLATE bogus`,
	})
	differ(t, "...and referencing it errors", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE VIEW v AS SELECT a FROM t ORDER BY 1 COLLATE bogus`,
		`SELECT * FROM v`,
	})
	differ(t, "unknown collation in a view's WHERE", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE VIEW v AS SELECT a FROM t WHERE a = 1 COLLATE bogus`,
		`SELECT * FROM v`,
	})
	differ(t, "unknown collation buried in a CASE arm", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE VIEW v AS SELECT CASE WHEN a=1 THEN a COLLATE bogus ELSE a END FROM t`,
		`SELECT * FROM v`,
	})
	differ(t, "referenced via INSERT ... RETURNING", []string{
		`CREATE TABLE t(a)`,
		`CREATE VIEW v AS SELECT a COLLATE bogus FROM t`,
		`INSERT INTO t VALUES(1) RETURNING (SELECT a FROM v)`,
	})
	// A KNOWN collation anywhere in a view body is unaffected: CREATE
	// succeeds and so does every later reference.
	differ(t, "a known collation in a view body works throughout", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(2),(1)`,
		`CREATE VIEW v AS SELECT a COLLATE nocase FROM t ORDER BY a COLLATE nocase`,
		`SELECT * FROM v`,
	})
	// A view with no COLLATE at all is untouched by this change.
	differ(t, "a plain view is untouched", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`CREATE VIEW v AS SELECT a FROM t`,
		`SELECT * FROM v`,
	})
}

// TestViewCollateDeferredINSTEADOF pins the INSTEAD OF INSERT column-mapping
// path (engine/view_trigger.go's viewColumnInfos), which resolves a view's
// columns without materializing any of its rows -- a separate call site from
// an ordinary SELECT, and one this file's fix touches independently.
//
// Driven directly (single db.Exec each side), NOT through differ(): the
// shared worker's runOne retries a failed db.Query with db.Exec on the SAME
// connection (compat-harness/worker/main.go), and C SQLite caches a
// view's resolved Table object (build.c:3151's pTable->nCol/aCol) on the
// FIRST attempt even though that attempt itself reports an error --
// sqlite3ResultSetOfSelect's own nErr check (select.c:2453) runs BEFORE
// sqlite3SubqueryColumnTypes raises the collation error, so pSelTab comes
// back non-NULL and the view's column count/list get cached anyway. A SECOND
// prepare of the identical trigger on the SAME connection then finds
// nCol>0, skips re-resolution entirely, and silently succeeds -- the exact
// same "does not do so twice" cache pragmaViewInfo's own doc comment already
// records for trusted_schema=OFF, just reached through a trigger program
// instead of PRAGMA table_info. musql has no such cache (re-resolves a
// view's body on every reference, deliberately) and so declines
// consistently; reproducing the oracle's cache would mean answering where a
// fresh connection's oracle refuses, a wrong answer rather than a gap. A
// SINGLE attempt per engine is what actually exercises the shape this file
// closes, matching a client that runs the statement once.
func TestViewCollateDeferredINSTEADOF(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t(a)`,
		`CREATE VIEW v AS SELECT a COLLATE bogus FROM t`,
		`CREATE TRIGGER tr INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(NEW.a); END`,
	}
	target := `INSERT INTO v VALUES(5)`

	edb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range stmts {
		if _, err := edb.Exec(s); err != nil {
			t.Fatalf("musql setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	_, eerr := edb.Exec(target)
	_, cerr := cdb.Exec(target)
	if (eerr == nil) != (cerr == nil) {
		t.Errorf("[%s] accept/reject disagrees\n  musql=%v\n  cgo=%v", target, eerr, cerr)
	}
}

// TestViewCollateDeferredFlatten pins the query-flattening path
// (engine/flatten_limit_r35c.go's r37cViewBody), which can inline a view's
// body directly into its caller: an unknown collation there must not
// silently inline as though it were BINARY.
func TestViewCollateDeferredFlatten(t *testing.T) {
	differ(t, "a flatten-eligible view (LIMIT transfer) with an unknown collation", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3),(4),(5)`,
		`CREATE VIEW v AS SELECT a COLLATE bogus FROM t LIMIT 2`,
		`SELECT * FROM v ORDER BY 1 DESC`,
	})
}

// TestViewCollateDeferredPragmaTableInfoGap records what this file's coarse,
// whole-body check does NOT reproduce exactly. C SQLite's PRAGMA
// table_info(v)/table_xinfo(v) only resolves a view's own RESULT-COLUMN list
// (build.c:3115's sqlite3ViewGetColumnNames -> sqlite3ResultSetOfSelect,
// select.c:2439 -- never sqlite3Select() over the WHOLE body), so an unknown
// collation sitting ONLY in WHERE/ORDER BY/GROUP BY does not fail there,
// verified directly against mattn/go-sqlite3 3.53.3. This engine declines
// the whole view body uniformly at every "first reference" site instead of
// replicating that narrower per-site scope -- a safe decline, since every
// one of these shapes was completely unreachable before this change (a
// CREATE VIEW naming an unknown collation anywhere always failed, with no
// exceptions). Asserted one-sided so the boundary stays visible; if a later
// change narrows pragmaViewInfo's own check to match C SQLite exactly,
// this fails and should become a differ case.
//
// Driven with a single db.Query per engine, NOT differ()/run(): the shared
// worker retries a failed Query with an Exec on the same connection, and
// driver's own Exec path for a row-returning bare PRAGMA (execArgs,
// driver/conn.go) does not evaluate it at all -- Exec discards a
// row-returning statement's rows for ANY pragma, collation-related or not,
// which would silently mask this test's real target (musql's Query path)
// behind an unrelated pre-existing gap. A single Query is what a real
// PRAGMA table_info(v) caller actually issues.
func TestViewCollateDeferredPragmaTableInfoGap(t *testing.T) {
	for _, stmts := range [][]string{
		{
			`CREATE TABLE t(a)`,
			`CREATE VIEW v AS SELECT a FROM t WHERE a = 1 COLLATE bogus`,
		},
		{
			`CREATE TABLE t(a)`,
			`CREATE VIEW v AS SELECT a FROM t ORDER BY 1 COLLATE bogus`,
		},
	} {
		target := `PRAGMA table_info(v)`

		edb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "e.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := edb.Exec(s); err != nil {
				t.Fatalf("musql setup %q: %v", s, err)
			}
			if _, err := cdb.Exec(s); err != nil {
				t.Fatalf("cgo setup %q: %v", s, err)
			}
		}
		erows, eerr := edb.Query(target)
		if eerr == nil {
			erows.Close()
		}
		crows, cerr := cdb.Query(target)
		if cerr == nil {
			crows.Close()
		}
		edb.Close()
		cdb.Close()
		if eerr == nil {
			t.Errorf("expected %q to still decline via musql (whole-body check); if it now answers, this may be worth narrowing to match the oracle exactly", target)
		}
		if cerr != nil {
			t.Errorf("expected the oracle to accept %q (its check is result-columns-only); got %v -- this test's own premise may be stale", target, cerr)
		}
	}
}
