package compat

// Tests for the oracle's prepare-only probe for optimize() queries.
// Verifies that the probe rejects missing tables and does not actually optimize.

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMinerR25PrepareProbeRejectsMissingTable(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// Exactly the fts2q.test shape: the CREATE VIRTUAL TABLE was declined on
	// both sides, so no such table exists on the oracle either.
	if !tclCGOCannotPrepare(db, "SELECT OPTIMIZE(t1) FROM t1 LIMIT 1") {
		t.Fatal("oracle prepared a SELECT over a table it does not have; " +
			"the harness would book a mutual rejection as a musql coverage gap")
	}
	// The control: a statement the oracle really can compile must NOT be
	// reported as rejected, or every optimize() decline silently disappears
	// into mutualReject whether or not the oracle agrees.
	if tclCGOCannotPrepare(db, "SELECT 1") {
		t.Fatal("oracle failed to prepare SELECT 1")
	}
}

func TestMinerR25PrepareProbeDoesNotOptimize(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	for _, s := range []string{
		"CREATE VIRTUAL TABLE t1 USING fts3(content)",
		"INSERT INTO t1(rowid, content) VALUES(1, 'one two three')",
		"INSERT INTO t1(rowid, content) VALUES(2, 'four five six')",
		"INSERT INTO t1(rowid, content) VALUES(3, 'seven eight nine')",
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	segdirs := func() int {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM t1_segdir").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := segdirs()
	if before < 2 {
		t.Fatalf("setup produced %d %%_segdir rows; need >1 for an optimize() to be observable", before)
	}

	if tclCGOCannotPrepare(db, "SELECT optimize(t1) FROM t1 LIMIT 1") {
		t.Fatal("oracle could not prepare optimize() over a table it HAS")
	}
	if after := segdirs(); after != before {
		t.Fatalf("preparing optimize() merged the index: %%_segdir went %d -> %d; "+
			"the probe is not free and would desynchronize the oracle from the engine",
			before, after)
	}

	// And the counter-proof that the check above can actually observe a merge:
	// RUNNING the same statement really does collapse the segments.
	if _, err := db.Exec("SELECT optimize(t1) FROM t1 LIMIT 1"); err != nil {
		t.Fatalf("running optimize(): %v", err)
	}
	if after := segdirs(); after >= before {
		t.Fatalf("running optimize() left %%_segdir at %d (was %d) -- this gate cannot "+
			"observe the side effect it exists to rule out", after, before)
	}
}
