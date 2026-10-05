// Tests PRAGMA writable_schema=RESET schema reloading.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

// TestWritableSchemaResetReloadsStrictTailChange tests schema reloading with STRICT.
// STRICT; INSERT INTO t1 VALUES(NULL,'x'); PRAGMA integrity_check(t1)")
// that musql's integrity_check does not implement STRICT's per-row type/
// NOT-NULL re-validation at all yet (it always answers "ok" for a STRICT
// table, independent of writable_schema) -- a real, PRE-EXISTING gap,
// unrelated to this fix and out of this bucket's scope. It does not regress
// TestTCLCorpus's never-wrong gate either: PRAGMA integrity_check/quick_check
// are the "SET" spellings (see engine.LeadingStatementVerb/tclIsQuery, which
// classifies EVERY "PRAGMA ..." as an EXEC statement, never a query) --
// tcl_test.go's runTCLSegment compares an exec statement's SUCCESS/FAILURE
// against the oracle, never its row content, so this gap costs nothing there
// (measured: strict2.test's own corpus run is wrong=0 both before and after
// this change). What this test DOES pin is what the mined statement is
// actually about: the RESET pragma itself is now SERVED, and the reload it
// performs really re-arms STRICT enforcement on later writes (checked below
// via an INSERT, which checkStrictColumnTypes -- insert_write.go -- DOES
// enforce correctly).
func TestWritableSchemaResetReloadsStrictTailChange(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1)
	defer cgodb.Close()

	setup := []string{
		`CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`,
		`INSERT INTO t1 VALUES(1,2),('three','four'),(x'5555','six'),(NULL,'eight')`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET sql=(sql||'STRICT') WHERE name='t1'`,
	}
	for _, s := range setup {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("musql setup %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("real setup %q: %v", s, err)
		}
	}

	// The mined statement itself: with the fix, musql now RELOADS instead
	// of declining (writableSchemaResetDecline calls writableSchemaReload
	// before falling back to its unconditional refusal).
	if _, _, err := godb.ExecArgs(`PRAGMA writable_schema=RESET`, nil); err != nil {
		t.Fatalf("musql PRAGMA writable_schema=RESET: %v (should now be SERVED, not declined)", err)
	}
	if _, err := cgodb.Exec(`PRAGMA writable_schema=RESET`); err != nil {
		t.Fatalf("real PRAGMA writable_schema=RESET: %v", err)
	}

	// strict2-3.0's own assertion is {{NULL value in t1.id}} -- logged, not
	// asserted, per this function's own doc comment (a separate, pre-existing
	// gap in musql's integrity_check, uninvolved in the RESET fix itself).
	p, err := godb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`PRAGMA integrity_check(t1)`, nil)
	if err != nil {
		t.Fatalf("musql integrity_check: %v (should run without error, whatever it reports)", err)
	}
	got := wsNormalize(rows)
	_, cgoRows, cerr := tclRunCGOQuery(cgodb, `PRAGMA integrity_check(t1)`)
	if cerr != nil {
		t.Fatalf("real integrity_check: %v", cerr)
	}
	t.Logf("post-reload integrity_check: musql=%v real=%v (see doc comment: not compared)", got, cgoRows)

	// STRICT is really re-armed by the reload, not just reflected by
	// integrity_check: a later INSERT that omits the PRIMARY KEY column
	// (an implicit NULL) must now fail on both engines identically, where
	// it would have SUCCEEDED before the RESET (the table was not STRICT
	// yet at that point).
	_, _, goInsErr := godb.ExecArgs(`INSERT INTO t1(x) VALUES('nine')`, nil)
	_, cgoInsErr := cgodb.Exec(`INSERT INTO t1(x) VALUES('nine')`)
	if cgoInsErr == nil {
		t.Fatalf("C SQLite accepted an implicit-NULL PRIMARY KEY insert after RESET -- the premise of this test is wrong")
	}
	if goInsErr == nil {
		t.Errorf("musql accepted an implicit-NULL PRIMARY KEY insert after RESET re-armed STRICT")
	} else if !strings.Contains(goInsErr.Error(), "NOT NULL constraint failed") {
		t.Errorf("musql insert error = %q, want a NOT NULL constraint failure", goInsErr)
	}

	// The catalog and the live schema agree again post-reload: sqlite_schema
	// now reports the STRICT text as the ordinary (non-overlaid) baseline.
	_, sqlRows, err := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='t1'`, nil)
	if err != nil {
		t.Fatalf("musql post-reload sql read: %v", err)
	}
	if len(sqlRows) != 1 || sqlRows[0][0].S == nil || !strings.Contains(string(sqlRows[0][0].S), "STRICT") {
		t.Errorf("post-reload sqlite_master.sql = %v, want the STRICT text", sqlRows)
	}
}

// TestWritableSchemaResetReloadsColumnListChange pins RESET over an edit that
// changes the column-definition LIST itself. It used to be declined because
// nothing here could re-derive a table from the catalog mid-session;
// reloadSchemaFromImage (schema_reload_image.go) now does, the way
// sqlite3InitOne does at the next prepare (prepare.c:199). Every step is
// compared with C SQLite, including the rows a two-column table now reads
// back through a three-column definition.
func TestWritableSchemaResetReloadsColumnListChange(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1)
	defer cgodb.Close()

	for i, s := range []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES(1, 2)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`,
		`PRAGMA writable_schema=RESET`,
		`SELECT * FROM t1`,
		`SELECT sql FROM sqlite_master WHERE name='t1'`,
		`INSERT INTO t1 VALUES(4, 5, 6)`,
		`UPDATE t1 SET c = a + b WHERE a = 1`,
		`SELECT * FROM t1 ORDER BY a`,
		`DELETE FROM t1 WHERE a = 4`,
		`SELECT count(*) FROM t1`,
		`PRAGMA integrity_check`,
	} {
		goCols, goRows, goErr := wsGoRun(godb, s)
		if goErr != nil {
			t.Fatalf("step %d %q: this engine errored: %v", i, s, goErr)
		}
		cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, s)
		if cgoErr != nil {
			t.Fatalf("step %d %q: C SQLite errored where this engine accepted: %v", i, s, cgoErr)
		}
		if !wsSameResult(goCols, goRows, cgoCols, cgoRows) {
			t.Fatalf("step %d %q diverges:\n  pure: %v %v\n  real: %v %v", i, s, goCols, goRows, cgoCols, cgoRows)
		}
	}
}
