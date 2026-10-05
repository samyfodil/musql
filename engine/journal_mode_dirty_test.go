package engine

import (
	"strings"
	"testing"
)

// TestJournalModeDirtyBoundaryDeclines tests PRAGMA journal_mode changes that
// must be declined when a transaction has already written.
func TestJournalModeDirtyBoundaryDeclines(t *testing.T) {
	mustDecline := func(t *testing.T, setup []string, wantErr string) {
		t.Helper()
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for i, s := range setup {
			// A "!"-prefixed setup line is EXPECTED to fail; its error is the
			// point of the scenario, not a setup problem.
			expectFail := strings.HasPrefix(s, "!")
			if expectFail {
				s = s[1:]
			}
			err := db.Exec(s)
			if i == len(setup)-1 {
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Errorf("%q: want error containing %q, got %v", s, wantErr, err)
				}
				return
			}
			if err != nil && !expectFail {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
	}
	written := "inside a transaction that has already written"

	t.Run("same-value-update", func(t *testing.T) {
		// rows-affected 1, pager CLEAN in C SQLite (probed: the setter
		// TAKES there) -- but this engine cannot tell a byte-identical
		// overwrite from a real one, so UPDATE stays on the declined side.
		mustDecline(t, []string{
			"CREATE TABLE u(a)", "INSERT INTO u VALUES(9)",
			"BEGIN", "UPDATE u SET a=a",
			"PRAGMA journal_mode=persist",
		}, written)
	})
	t.Run("zero-rows-kept-insert", func(t *testing.T) {
		// INSERT OR IGNORE that keeps nothing: pager clean, oracle TAKES.
		mustDecline(t, []string{
			"CREATE TABLE uq(a INTEGER PRIMARY KEY)", "INSERT INTO uq VALUES(1)",
			"BEGIN", "INSERT OR IGNORE INTO uq VALUES(1)",
			"PRAGMA journal_mode=persist",
		}, written)
	})
	t.Run("temp-table-write", func(t *testing.T) {
		// The TEMP pager is not main's: oracle TAKES; target resolution
		// keeps the flag unset.
		mustDecline(t, []string{
			"CREATE TEMP TABLE tt(z)",
			"BEGIN", "INSERT INTO tt VALUES(5)",
			"PRAGMA journal_mode=persist",
		}, written)
	})
	t.Run("failed-statement", func(t *testing.T) {
		// A failed INSERT dirtied-then-rolled-back pages; whether the oracle
		// pager is left CACHEMOD was deliberately not probed -- unrecorded
		// stays declined. The "!"-prefixed setup line is EXPECTED to fail.
		mustDecline(t, []string{
			"CREATE TABLE uq(a INTEGER PRIMARY KEY)", "INSERT INTO uq VALUES(1)",
			"BEGIN", "!INSERT INTO uq VALUES(1)",
			"PRAGMA journal_mode=persist",
		}, written)
	})
	t.Run("attached-database-touched", func(t *testing.T) {
		// Each ATTACHed database is a further pager the unqualified setter
		// moves independently -- but ONLY once this transaction has touched
		// it (attachedDB.touchedInTxn, a STRICTLY EARLIER pager state than
		// CACHEMOD -- see attachedPagersProvablyClean, pragma.go). A pure
		// READ routed to aux (PRAGMA aux.integrity_check, exactly like a real
		// write, per attach_write.go's execRoutedToAttached) still marks it
		// touched, so the decline holds even though aux itself was never
		// WRITTEN: this engine cannot tell whether C SQLite's own
		// per-pager dirtied check would have let a touched-but-clean pager
		// through (see attachedPagersProvablyClean's doc comment).
		dir := t.TempDir()
		mustDecline(t, []string{
			"ATTACH '" + dir + "/aux.db' AS aux",
			"CREATE TABLE u(a)",
			"BEGIN",
			"PRAGMA aux.integrity_check",
			"INSERT INTO u VALUES(1)",
			"PRAGMA journal_mode=persist",
		}, written)
	})
	t.Run("qualified-setter", func(t *testing.T) {
		mustDecline(t, []string{
			"CREATE TABLE u(a)",
			"BEGIN", "INSERT INTO u VALUES(1)",
			"PRAGMA main.journal_mode=persist",
		}, written)
	})
	t.Run("concrete-temp-mode", func(t *testing.T) {
		// A previously-modeled concrete TEMP journal mode would have to move
		// (or not) with temp's own untracked dirtiness -- declined.
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetTempJournalMode("persist")
		for _, s := range []string{"CREATE TABLE u(a)", "BEGIN", "INSERT INTO u VALUES(1)"} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		if err := db.Exec("PRAGMA journal_mode=truncate"); err == nil || !strings.Contains(err.Error(), written) {
			t.Errorf("concrete temp mode: want the dirtied-transaction decline, got %v", err)
		}
	})

	t.Run("insert-then-ignored", func(t *testing.T) {
		// The served side, engine-level: a definite dirty INSERT makes the
		// setter a silent no-op reporting/keeping the old mode.
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range []string{"PRAGMA journal_mode=delete", "CREATE TABLE u(a)", "BEGIN", "INSERT INTO u VALUES(1)"} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		if err := db.Exec("PRAGMA journal_mode=persist"); err != nil {
			t.Fatalf("ignored setter errored: %v", err)
		}
		if got := db.journalMode(); got != "delete" {
			t.Errorf("journal mode moved to %q inside a dirtied transaction, want delete", got)
		}
		if err := db.Exec("COMMIT"); err != nil {
			t.Fatal(err)
		}
		if got := db.journalMode(); got != "delete" {
			t.Errorf("journal mode is %q after COMMIT, want delete", got)
		}
	})

	// TestTCLCorpus's jrnlmode.test#4 mines this EXACT shape (jrnlmode-6.1):
	// two earlier ATTACHed databases (aux/aux2, from jrnlmode-5.x, never
	// DETACHed) are still open when "PRAGMA journal_mode = truncate; CREATE
	// TABLE t4(a, b); BEGIN; INSERT INTO t4 VALUES(1, 2); PRAGMA
	// journal_mode = memory" runs -- and used to decline outright on
	// len(db.attached) == 0 alone. C SQLite's own per-pager loop
	// (pragma.c:762's `for(ii=db->nDb-1; ...)`) processes aux/aux2
	// independently of main: each is UNTOUCHED by this transaction (only
	// main.t4 was written), so sqlite3PagerOkToChangeJournalMode(pager.c:7506)
	// lets EACH of them take the switch even though main -- provably dirtied
	// -- stays put. Verified directly against mattn/go-sqlite3 3.53.3: after
	// this exact sequence, "PRAGMA journal_mode" (main) still reads
	// "truncate" while "PRAGMA aux.journal_mode" reads "memory".
	t.Run("attached-database-untouched-served", func(t *testing.T) {
		dir := t.TempDir()
		db, err := Create(dir + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range []string{
			"ATTACH '" + dir + "/aux.db' AS aux",
			"ATTACH '" + dir + "/aux2.db' AS aux2",
			"CREATE TABLE main.t1(a, b, c)",
			"PRAGMA journal_mode = truncate",
			"CREATE TABLE t4(a, b)",
			"BEGIN",
			"INSERT INTO t4 VALUES(1, 2)",
		} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		if err := db.Exec("PRAGMA journal_mode = memory"); err != nil {
			t.Fatalf("unqualified switch with untouched attachments errored: %v", err)
		}
		if got := db.journalMode(); got != "truncate" {
			t.Errorf("main journal mode = %q inside the dirtied transaction, want truncate (untouched)", got)
		}
		for _, a := range db.attached {
			if a.journalMode != "memory" {
				t.Errorf("attachment %q journalMode = %q, want memory (untouched, so the unqualified setter reaches it)", a.name, a.journalMode)
			}
		}
		if err := db.Exec("COMMIT"); err != nil {
			t.Fatal(err)
		}
		if got := db.journalMode(); got != "truncate" {
			t.Errorf("main journal mode = %q after COMMIT, want truncate", got)
		}
	})
}
