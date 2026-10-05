package engine

// PREPARE-time compiler errors and changes() state after failed statements.

import (
	"errors"
	"testing"
)

var prepareFailureCases = []struct {
	name  string
	setup []string
	stmt  string
}{
	{"UPDATE SET names no column", []string{`CREATE TABLE t(a)`}, `UPDATE t SET a=zzz`},
	{"DELETE WHERE names no function", []string{`CREATE TABLE t(a)`}, `DELETE FROM t WHERE a=nosuchfn(1)`},
	{"DELETE WHERE names no column", []string{`CREATE TABLE t(a)`}, `DELETE FROM t WHERE zzz=1`},
	{"UPDATE WHERE names no column", []string{`CREATE TABLE t(a)`}, `UPDATE t SET a=9 WHERE zzz=1`},
	{"INSERT SELECT names no function", []string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`}, `INSERT INTO t SELECT nosuchfn(a) FROM s`},
	{"INSERT SELECT names no column", []string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`}, `INSERT INTO t SELECT zzz FROM s`},
	// Not one of the five census spellings above: found by probing what those
	// fixes did NOT reach. buildReturningPlan (returning_write.go) runs
	// checkExprSupported over every RETURNING output column, and emitReturning
	// wrapped its verdict into a blanket decline -- so the failure was
	// rediscovered at RUN time, below execReturningViaVM's own deferred
	// setChanges (vdbe_write.go), instead of at prepare time.
	{"RETURNING names no function", []string{`CREATE TABLE t(a)`}, `INSERT INTO t VALUES(1) RETURNING nosuchfn(a)`},
}

// TestPrepareFailureIsAHardCompileError pins the SEPARATION itself: an
// unresolvable name is errVDBESemantic (a genuine C-SQLite prepare-time
// rejection -- resolve.c:785/795 for a column, resolve.c:1290 for a function),
// not errVDBEUnsupported, which is this engine saying it cannot LOWER a shape.
// The two are not interchangeable: C SQLite rejects these statements
// outright, so reporting a capability gap for one would be a different answer.
func TestPrepareFailureIsAHardCompileError(t *testing.T) {
	for _, tc := range prepareFailureCases {
		db := newPrepareFailureDB(t, tc.setup)
		prog, err := db.compileWrite(tc.stmt)
		switch {
		case err == nil:
			t.Errorf("[%s] %q compiled to a %d-instruction program; expected a hard compile error",
				tc.name, tc.stmt, len(prog.Insns))
		case !errors.Is(err, errVDBESemantic):
			t.Errorf("[%s] %q: err = %v; want one wrapping errVDBESemantic. An errVDBEUnsupported "+
				"here reports a CAPABILITY gap for a statement C SQLite rejects at prepare "+
				"time, which is a different answer and not merely a different wrapper.",
				tc.name, tc.stmt, err)
		}
		db.Discard()
	}
}

// TestPrepareFailureLeavesChangesAlone is the behaviour that separation buys,
// asserted without the oracle so it still fails if the compile path is later
// re-routed: a statement rejected while preparing publishes nothing
// (sqlite3VdbeHalt's "assert( p->eVdbeState==VDBE_RUN_STATE );", vdbeaux.c:3335,
// and the changeCntOn-guarded publish at vdbeaux.c:3481).
func TestPrepareFailureLeavesChangesAlone(t *testing.T) {
	for _, tc := range prepareFailureCases {
		db := newPrepareFailureDB(t, tc.setup)
		if err := db.Exec(`INSERT INTO t VALUES(1),(2),(3)`); err != nil {
			t.Fatalf("[%s] seed: %v", tc.name, err)
		}
		if db.nChange != 3 {
			t.Fatalf("[%s] seed left changes()=%d, want 3 -- the case cannot measure anything", tc.name, db.nChange)
		}
		if err := db.Exec(tc.stmt); err == nil {
			t.Errorf("[%s] %q unexpectedly succeeded", tc.name, tc.stmt)
		}
		if db.nChange != 3 {
			t.Errorf("[%s] %q left changes()=%d, want 3 (the previous statement's count, untouched)",
				tc.name, tc.stmt, db.nChange)
		}
		db.Discard()
	}
}

func newPrepareFailureDB(t *testing.T, setup []string) *Session {
	t.Helper()
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	return db
}
