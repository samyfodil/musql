package engine

import (
	"strings"
	"testing"
	"time"
)

// TestR32NQuotefixTriggerText tests that trigger SQL is properly quoted after an ALTER TABLE RENAME.
func TestR32NQuotefixTriggerText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		alter string
		want  string
	}{
		{"a bare name in WHEN resolves to nothing, and VALUES has no source", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE TABLE log(m)`,
			`CREATE TRIGGER tr AFTER INSERT ON t1 WHEN new.a <> "zz" BEGIN INSERT INTO log VALUES("lit"); END`,
		}, `ALTER TABLE t1 RENAME c TO ccc`,
			`CREATE TRIGGER tr AFTER INSERT ON t1 WHEN new.a <> 'zz' BEGIN INSERT INTO log VALUES('lit'); END`},
		{"an UPDATE step resolves against its own target table", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE TABLE t2(p, q)`,
			`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN UPDATE t2 SET q = "p" WHERE q = "zz"; END`,
		}, `ALTER TABLE t1 RENAME c TO ccc`,
			`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN UPDATE t2 SET q = "p" WHERE q = 'zz'; END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if err := db.Exec(tc.alter); err != nil {
				t.Fatalf("%q: %v", tc.alter, err)
			}
			if len(db.triggers) != 1 {
				t.Fatalf("want 1 trigger, got %d", len(db.triggers))
			}
			if got := db.triggers[0].sql; got != tc.want {
				t.Errorf("after %q:\n got  %q\n want %q", tc.alter, got, tc.want)
			}
		})
	}
}

// TestR32NQuotefixSelfReferentialView pins the FLOOR under the quotefix pass's
// scope recursion. "CREATE VIEW v AS SELECT * FROM v" is creatable -- a view
// body is not resolved until the view is queried -- and the pass walks a view
// referenced from a FROM clause to over-approximate its output column list
// (r32nAddSourceNames -> r32nAddDerivedNames). Without r32nMaxDepth, and
// without the derived walk INHERITING the enclosing walk's depth, that pair
// bounces between itself forever and the ALTER never returns.
//
// It is asserted with a watchdog rather than by inspecting the result, because
// the failure mode is a hang, which no output comparison can catch.
func TestR32NQuotefixSelfReferentialView(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
	}{
		{"direct self reference", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE VIEW v1 AS SELECT "zz" FROM v1`,
		}},
		{"mutual reference", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE VIEW v1 AS SELECT "zz" FROM v2`,
			`CREATE VIEW v2 AS SELECT "yy" FROM v1`,
		}},
		{"self reference through a derived table", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE VIEW v1 AS SELECT "zz" FROM (SELECT * FROM v1)`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// db.Close() runs SYNCHRONOUSLY on every exit path below, not via
			// a bare "defer db.Close()", and this goroutine sends to done
			// only AFTER it returns: t.TempDir()'s cleanup fires the instant
			// this subtest's own function returns (which the select below
			// does as soon as it reads from done), with no regard for
			// whether a goroutine THIS subtest spawned is still running --
			// so a deferred Close racing that cleanup could still be
			// mid-flight (or not yet started) when the directory is removed
			// out from under it. That race is harmless when Close's own
			// error is silently discarded (as the old bare defer did), but
			// StructuralCheckAfterCommitForTest (globally on for this
			// package, incremental_delta_test.go) makes a losing Close
			// PANIC instead -- reopening db.path by name once the directory
			// is already gone -- which crashes the whole test binary rather
			// than failing just this test. Closing before done<-nil removes
			// the race outright: nothing can look at the temp dir again
			// until Close has genuinely finished.
			done := make(chan error, 1)
			go func() {
				db, err := Create(t.TempDir() + "/x.musq")
				if err != nil {
					done <- err
					return
				}
				for _, s := range tc.setup {
					if err := db.Exec(s); err != nil {
						db.Close()
						done <- err
						return
					}
				}
				// The pass runs here, over a schema whose view graph has a
				// cycle in it. Whatever it decides, it must DECIDE.
				db.Exec(`ALTER TABLE t1 RENAME a TO aaa`)
				closeErr := db.Close()
				done <- closeErr
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("ALTER TABLE did not return: the quotefix pass's scope walk is not bounded")
			}
		})
	}
}

// TestR32NQuotefixDeclineChangesNothing pins renameFixQuotes' place in the
// ALTER's ordering. The pass runs BEFORE the rename (alter.c:646), so a
// cascade that declines AFTER it must put every quotefixed byte back -- "a
// declined ALTER changes nothing" is this write path's own rule, and the
// quotefix is the first thing it now has to undo.
//
// The decline used here is the empty-IN() operand guard, which fires on the
// TABLE's own text (emptyInBlocks, this file) and is checked before anything
// is spliced, plus a trigger cascade that cannot be done. Both leave the
// schema exactly as it was, quotes included.
func TestR32NQuotefixDeclineChangesNothing(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := []string{
		`CREATE TABLE t1(a, b, c)`,
		// A second table whose CHECK the pass WOULD rewrite, to prove the undo
		// reaches objects that have nothing to do with the failing cascade.
		`CREATE TABLE ck(x CHECK (x <> "nope"))`,
		`CREATE VIEW v1 AS SELECT "a", "alsonope" FROM t1`,
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	ckSQL := db.findTableMeta("ck").sql
	viewSQL := db.findViewMeta("v1").sql

	// RENAME COLUMN a column that does not exist: the ALTER fails before the
	// pass ever runs.
	if err := db.Exec(`ALTER TABLE t1 RENAME nosuch TO x`); err == nil {
		t.Fatal("renaming a missing column should fail")
	}
	if got := db.findTableMeta("ck").sql; got != ckSQL {
		t.Errorf("a failed ALTER quotefixed ck anyway:\n got  %q\n want %q", got, ckSQL)
	}
	if got := db.findViewMeta("v1").sql; got != viewSQL {
		t.Errorf("a failed ALTER quotefixed v1 anyway:\n got  %q\n want %q", got, viewSQL)
	}

	// DROP COLUMN of a column the view names: declined, and nothing moves.
	if err := db.Exec(`ALTER TABLE t1 DROP COLUMN a`); err == nil {
		t.Fatal("dropping a column a view names should be declined")
	}
	if got := db.findTableMeta("ck").sql; got != ckSQL {
		t.Errorf("a declined DROP COLUMN left ck quotefixed:\n got  %q\n want %q", got, ckSQL)
	}
	if got := db.findViewMeta("v1").sql; got != viewSQL {
		t.Errorf("a declined DROP COLUMN left v1 quotefixed:\n got  %q\n want %q", got, viewSQL)
	}

	// ... and a SUCCEEDING one does the whole pass.
	if err := db.Exec(`ALTER TABLE t1 RENAME b TO bbb`); err != nil {
		t.Fatalf("ALTER TABLE t1 RENAME b TO bbb: %v", err)
	}
	if got := db.findTableMeta("ck").sql; !strings.Contains(got, `'nope'`) {
		t.Errorf("ck was not quotefixed: %q", got)
	}
	if got := db.findViewMeta("v1").sql; !strings.Contains(got, `'alsonope'`) || !strings.Contains(got, `"a"`) {
		t.Errorf("v1 quotefix wrong: %q", got)
	}
}

// TestR32NQuotefixIndexKeyDemotionRollback pins the other half of that rule:
// the quotefix itself can FAIL the ALTER (a bare double-quoted index key
// becomes a bare identifier on the next re-parse -- sqlite3StringToId,
// build.c:4217), and that failure must leave the schema untouched too,
// including the objects the pass had already rewritten before reaching it.
func TestR32NQuotefixIndexKeyDemotionRollback(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE ck(x CHECK (x <> "nope"))`,
		`CREATE INDEX i1 ON t1("val")`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	tblSQL := db.findTableMeta("t1").sql
	ckSQL := db.findTableMeta("ck").sql
	idxSQL := db.indexes[0].sql

	err = db.Exec(`ALTER TABLE t1 RENAME c TO ccc`)
	if err == nil {
		t.Fatal("want the ALTER to fail: the quotefixed key re-parses as a missing column")
	}
	// The oracle's own wording, verified against 3.53.3 through a real ALTER:
	// "error in index i1: no such column: val".
	if got := err.Error(); !strings.Contains(got, "error in index i1: no such column: val") {
		t.Errorf("wrong message: %q", got)
	}
	if got := db.findTableMeta("t1").sql; got != tblSQL {
		t.Errorf("t1 changed:\n got  %q\n want %q", got, tblSQL)
	}
	if got := db.findTableMeta("ck").sql; got != ckSQL {
		t.Errorf("ck was left quotefixed by a failed ALTER:\n got  %q\n want %q", got, ckSQL)
	}
	if got := db.indexes[0].sql; got != idxSQL {
		t.Errorf("i1 changed:\n got  %q\n want %q", got, idxSQL)
	}
}
