package compat

// Tests prepare-only oracle probes for VACUUM INTO: target-expression
// self-reference like bare columns, qualified columns, or undefined functions
// fail at prepare time in both engines.

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestVacuumCannotPrepareRejectsSelfReferenceTargets(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t1(a)"); err != nil {
		t.Fatal(err)
	}

	// Every shape vacuum-into.test's own do_catchsql_test/do_test negative
	// cases (TCL test names vacuum-into-320/330/340/410/420) exercise: a
	// target expression with a TK_COLUMN or TK_FUNCTION node the self-
	// reference resolver rejects before OP_Vacuum can ever run. Only
	// "VACUUM INTO target()" (vacuum-into-410, mined as vacuum-into.test#2 --
	// it alone is written with the miner-visible "execsql" construct) is
	// corpus-mined; the other four are do_catchsql_test blocks the miner
	// never sees (this file's own top comment) and are checked here directly
	// instead.
	for _, stmt := range []string{
		"VACUUM INTO target()",          // vacuum-into-410 (the mined statement, vacuum-into.test#2)
		"VACUUM INTO target2()",         // vacuum-into-420 (not mined)
		"VACUUM INTO x",                 // vacuum-into-320 (not mined)
		"VACUUM INTO t1.nosuchcol",      // vacuum-into-330 (not mined)
		"VACUUM INTO main.t1.nosuchcol", // vacuum-into-340 (not mined)
	} {
		if !tclVacuumCannotPrepare(db, stmt, stmt) {
			t.Errorf("%q: oracle prepared it; the harness would book this mutual rejection as a musql coverage gap", stmt)
		}
	}
}

// The controls: a VACUUM the oracle really can prepare must NOT be reported
// as rejected (or every genuine musql VACUUM gap would silently disappear
// into mutualReject), a non-VACUUM statement must not be touched by this
// probe at all (it is restricted to the one leading keyword, same as
// tclAttachCannotPrepare's ATTACH/DETACH gate), and "VACUUM INTO null" stays
// OUTSIDE this probe's reach on purpose: C SQLite's target-type check
// (vacuum.c:178-183, "non-text filename") runs at STEP inside
// sqlite3RunVacuum, not at prepare -- a NULL literal has no TK_COLUMN/
// TK_FUNCTION node for the case-(4) resolver to reject -- so the oracle
// prepares it fine and this probe correctly answers false, leaving that one
// statement's classification untouched by this change.
func TestVacuumCannotPrepareControls(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t1(a)"); err != nil {
		t.Fatal(err)
	}

	if tclVacuumCannotPrepare(db, "VACUUM", "VACUUM") {
		t.Error("plain VACUUM reported as un-prepareable")
	}
	if tclVacuumCannotPrepare(db, "VACUUM INTO 'out.db'", "VACUUM INTO 'out.db'") {
		t.Error("VACUUM INTO a literal path reported as un-prepareable")
	}
	if tclVacuumCannotPrepare(db, "VACUUM INTO null", "VACUUM INTO null") {
		t.Error("VACUUM INTO null prepares fine on the oracle (its refusal is at STEP); " +
			"this probe must not claim it as a mutual rejection")
	}
	if tclVacuumCannotPrepare(db, "SELECT nosuchfunc()", "SELECT nosuchfunc()") {
		t.Error("a non-VACUUM statement must be ignored by this probe (leading-keyword gate)")
	}
}
