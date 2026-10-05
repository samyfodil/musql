package engine

// Compile-time proofs for conflict x upsert shapes that previous differential gates couldn't catch.
// Three shapes: UPDATE identity move, DO UPDATE with triggers, DELETE trigger recursion.
//     TestReplaceVictimUpdatePin fails for the DELETE and UPDATE trigger
//     bodies. NOT for the INSERT body, measured: that one raises a UNIQUE
//     violation of its own without any pin, so it agrees with the oracle by
//     accident. Kept in the table anyway -- it is the second of the two
//     spellings the oracle was measured on -- but the DELETE and UPDATE bodies
//     are the ones carrying the assertion.
//   - opUpsertStore's setCol re-read removed: NOTHING fails, and that is
//     recorded rather than papered over. A compiled BEFORE UPDATE program
//     cannot write the target table at all (nestedBeforeTriggerDeclined refuses
//     a body statement whose target carries a BEFORE trigger, at every depth),
//     so update.c's After-BEFORE-trigger-reload-loop is unreachable from here.
//     It is emitted anyway because compileUpdateStmt emits its twin for the
//     same unreachable reason, and because widening that decline -- which is
//     the standing next job on the BEFORE-trigger cluster -- is what would make
//     it reachable.

import (
	"fmt"
	"strings"
	"testing"
)

// countOps tallies one opcode across prog and every sub-program.
func countOps(prog *Program, op OpCode, seen map[*Program]bool) int {
	if prog == nil || seen[prog] {
		return 0
	}
	seen[prog] = true
	n := 0
	for i := range prog.Insns {
		if prog.Insns[i].Op == op {
			n++
		}
		for _, sub := range subProgramsOf(prog.Insns[i].P4) {
			n += countOps(sub, op, seen)
		}
	}
	return n
}

// compileConflictShape is compileShape with the program handed back for
// non-vacuity checks.
func compileConflictShape(t *testing.T, setup []string, stmt string) *Program {
	t.Helper()
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	prog, cerr := db.compileWrite(stmt)
	if cerr != nil {
		t.Fatalf("%q: should COMPILE, got: %v", stmt, cerr)
	}
	return prog
}

// Executes statements engine-direct to test write-program cache.
func execScript(t *testing.T, setup, acts []string, dump string) ([]string, error) {
	t.Helper()
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	for _, s := range acts {
		if err := db.Exec(s); err != nil {
			return nil, fmt.Errorf("%q: %w", s, err)
		}
	}
	return rvdRowStrings(rvdQuery(t, db, dump)), nil
}

func wantRows(t *testing.T, what string, got []string, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

// ---- shape 1: UPDATE conflict clause reassigning the row identity ----

func TestConflictIdentityMoveCompiles(t *testing.T) {
	// Non-vacuity: the promotion IS update.c:875-878's re-seek, so a
	// conflict-resolving UPDATE must carry exactly one OpNotExists over its
	// scan cursor. Without this the "it compiled" assertion alone would pass
	// over a program that had gone back to the presence test.
	for _, tc := range []struct {
		setup []string
		stmt  string
	}{
		{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`}, `UPDATE OR REPLACE t SET k=k+1`},
		{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, v) WITHOUT ROWID`}, `UPDATE OR REPLACE t SET k=k+1`},
		{[]string{`CREATE TABLE t(x,y,v, PRIMARY KEY(x,y)) WITHOUT ROWID`}, `UPDATE OR REPLACE t SET y=y+1`},
		{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `UPDATE OR REPLACE t SET a=1`},
		{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE,b)`}, `UPDATE t SET a=1`},
	} {
		prog := compileConflictShape(t, tc.setup, tc.stmt)
		if n := countOps(prog, OpNotExists, map[*Program]bool{}); n != 1 {
			t.Errorf("%q: %d OpNotExists, want exactly 1 (update.c:875-878's per-row re-seek)", tc.stmt, n)
		}
		if n := countOps(prog, OpSkipIfRowGone, map[*Program]bool{}); n != 0 {
			t.Errorf("%q: %d OpSkipIfRowGone -- the presence TEST is what the SEEK replaced", tc.stmt, n)
		}
	}
	// A plain UPDATE emits neither: nothing it does can change a row the scan
	// has not reached, which is update.c:733-739's own ONEPASS test.
	prog := compileConflictShape(t, []string{`CREATE TABLE t(a,b)`}, `UPDATE t SET b=b+1`)
	if n := countOps(prog, OpNotExists, map[*Program]bool{}); n != 0 {
		t.Errorf("plain UPDATE: %d OpNotExists, want 0", n)
	}
}

// TestConflictIdentityMoveAnswers pins what the promotion actually changed.
// Every want below is mattn/go-sqlite3 3.53.3's answer, measured directly; each
// was a LIVE WRONG ANSWER on the route this replaced
// (applyPendingUpdatesConflict built its pendings list from the pre-statement
// image too, so declining bought these nothing).
func TestConflictIdentityMoveAnswers(t *testing.T) {
	// rowid table: iteration one moves row 1 onto rowid 2 and REPLACE deletes
	// the old row 2; iteration two re-seeks rowid 2, finds the MOVED row, and
	// moves it again. Was (2,x) with changes()=1.
	got, err := execScript(t,
		[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1`}, `SELECT k,b FROM t ORDER BY k`)
	wantRows(t, "rowid move", got, err, "3,x")

	got, err = execScript(t,
		[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1`}, `SELECT k,b FROM t ORDER BY k`)
	wantRows(t, "rowid move, 3 rows", got, err, "4,x")

	// The WHERE is evaluated in PASS ONE, over the untouched image -- row 3 is
	// filtered out on its FROZEN 'z' even though row 2's REPLACE has since
	// rewritten rowid 3. Was (2,x),(3,z),(5,w).
	got, err = execScript(t,
		[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z'),(4,'w')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1 WHERE b<>'z'`}, `SELECT k,b FROM t ORDER BY k`)
	wantRows(t, "rowid move with WHERE", got, err, "3,x", "5,w")

	// A declared per-constraint default is conflict-aware too.
	got, err = execScript(t,
		[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE t SET a=a+1`}, `SELECT a,b FROM t ORDER BY a`)
	wantRows(t, "declared REPLACE default", got, err, "3,x")

	// WITHOUT ROWID: update.c:868-869 seeks the PRIMARY KEY RECORD, so the
	// second iteration finds whichever row now HOLDS key 2 -- which this
	// engine's internal row-store id does not follow. Was (2,x) / ('b','x').
	got, err = execScript(t,
		[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,v) WITHOUT ROWID`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1`}, `SELECT k,v FROM t ORDER BY k`)
	wantRows(t, "WITHOUT ROWID INTEGER PK move", got, err, "3,x")

	got, err = execScript(t,
		[]string{`CREATE TABLE t(k TEXT PRIMARY KEY,v) WITHOUT ROWID`, `INSERT INTO t VALUES('a','x'),('b','y')`},
		[]string{`UPDATE OR REPLACE t SET k=char(unicode(k)+1)`}, `SELECT k,v FROM t ORDER BY k`)
	wantRows(t, "WITHOUT ROWID TEXT PK move", got, err, "c,x")

	got, err = execScript(t,
		[]string{`CREATE TABLE t(x,y,v, PRIMARY KEY(x,y)) WITHOUT ROWID`, `INSERT INTO t VALUES(1,1,'a'),(1,2,'b')`},
		[]string{`UPDATE OR REPLACE t SET y=y+1`}, `SELECT x,y,v FROM t ORDER BY x,y`)
	wantRows(t, "composite PK move (2nd column)", got, err, "1,3,a")

	// The unchanged half, kept so a "just always re-read live" mutation is
	// caught: OR IGNORE deletes nothing, so no row is ever rewritten and the
	// answer must be exactly what it always was.
	got, err = execScript(t,
		[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`},
		[]string{`UPDATE OR IGNORE t SET k=k+1`}, `SELECT k,b FROM t ORDER BY k`)
	wantRows(t, "OR IGNORE move", got, err, "1,x", "2,y", "4,z")

	// A REPLACE victim that is itself a later match still DROPS OUT -- the
	// re-seek is a seek AND a presence test, and this is the case the
	// OpSkipIfRowGone it replaced was there for.
	got, err = execScript(t,
		[]string{`CREATE TABLE t(a,b UNIQUE)`, `INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`},
		[]string{`UPDATE OR REPLACE t SET b=b+10`}, `SELECT a,b FROM t ORDER BY a`)
	wantRows(t, "victim drops out of the scan", got, err, "1,11", "2,12", "3,13")
}

// ---- shape 2: upsert DO UPDATE against a table with UPDATE triggers ----

func TestUpsertUpdateTriggerCompiles(t *testing.T) {
	prog := compileConflictShape(t,
		[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.c); END`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.c); END`},
		`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`)
	// Non-vacuity: BOTH programs must be IN the emitted code. "It compiled"
	// alone was true of the version that stored the row and skipped the
	// triggers.
	if n := countOps(prog, OpFireTriggers, map[*Program]bool{}); n != 2 {
		t.Errorf("%d OpFireTriggers, want 2 (the DO UPDATE branch's BEFORE and AFTER programs)", n)
	}
	if n := countOps(prog, OpUpsertStore, map[*Program]bool{}); n != 1 {
		t.Errorf("%d OpUpsertStore, want 1", n)
	}
	// DO NOTHING updates nothing, so it fires nothing.
	prog = compileConflictShape(t,
		[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.c); END`},
		`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO NOTHING`)
	if n := countOps(prog, OpFireTriggers, map[*Program]bool{}); n != 0 {
		t.Errorf("DO NOTHING: %d OpFireTriggers, want 0", n)
	}
}

// TestUpsertUpdateTriggerAnswers: every want is 3.53.3's, measured directly.
// The whole shape was a hard ERROR here before applyUpsert learned it, and a
// decline after; these are the answers the COMPILED tail must reproduce.
func TestUpsertUpdateTriggerAnswers(t *testing.T) {
	trig := []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.c||'->'||new.c); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`}

	got, err := execScript(t, trig,
		[]string{`INSERT INTO t(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`},
		`SELECT a,b,c FROM t`)
	wantRows(t, "DO UPDATE row", got, err, "1,2,11")
	got, err = execScript(t, trig,
		[]string{`INSERT INTO t(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`}, `SELECT x FROM log`)
	wantRows(t, "DO UPDATE log", got, err, "au:10->11")

	// A DO UPDATE whose WHERE is false updates nothing and fires nothing --
	// indistinguishable from DO NOTHING, verified against the oracle.
	got, err = execScript(t, trig,
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=99 WHERE c>1000`}, `SELECT x FROM log`)
	wantRows(t, "DO UPDATE WHERE false log", got, err)

	// No conflict at all: the candidate is a plain insert, and an INSERT fires
	// no UPDATE trigger.
	got, err = execScript(t, trig,
		[]string{`INSERT INTO t(a,b,c) VALUES(7,2,10) ON CONFLICT(a) DO UPDATE SET c=c+1`}, `SELECT x FROM log`)
	wantRows(t, "no-conflict log", got, err)

	// The IPK-moving DO UPDATE, which already compiled, now with a trigger
	// around it: OLD.a is the conflicting row's rowid and NEW.a the new one.
	got, err = execScript(t,
		[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.a||'->'||new.a); END`,
			`INSERT INTO t VALUES(1,'x')`},
		[]string{`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=9`}, `SELECT x FROM log`)
	wantRows(t, "IPK-moving DO UPDATE log", got, err, "au:1->9")

	// A WHEN guard is evaluated per firing row, so the first upsert (c=20) is
	// silent and the second (c=99) logs.
	got, err = execScript(t,
		[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t WHEN new.c > 50 BEGIN INSERT INTO log VALUES('au:'||new.c); END`,
			`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,20) ON CONFLICT(a) DO UPDATE SET c=excluded.c`,
			`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`}, `SELECT x FROM log`)
	wantRows(t, "WHEN-guarded log", got, err, "au:99")

	// The write-program CACHE, which driver cannot exercise: the same
	// statement text twice on ONE connection must fire twice off ONE compile.
	got, err = execScript(t,
		[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||new.c); END`,
			`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,0) ON CONFLICT(a) DO UPDATE SET c=c+1`,
			`INSERT INTO t(a,b,c) VALUES(1,2,0) ON CONFLICT(a) DO UPDATE SET c=c+1`}, `SELECT x FROM log`)
	wantRows(t, "cached re-execution log", got, err, "au:11", "au:12")
}

// TestUpsertUpdateTriggerOrconf pins the one rule the tree-walking route still
// gets wrong, and the reason emitUpsertTail passes orconfSet=true.
//
// sqlite3UpsertDoUpdate calls sqlite3Update with a LITERAL OE_Abort
// (upsert.c:325-326), not OE_Default, so trigger.c:1137
// ("pParse->eOrconf = (orconf==OE_Default)?pStep->orconf:(u8)orconf;")
// OVERRIDES every body statement's own conflict clause with ABORT. A
// written-out UPDATE with no OR-clause passes OE_Default (parse.y:491) and does
// not. Measured against 3.53.3 over t(a UNIQUE,b,c) and log(x UNIQUE) already
// holding 1, with "AFTER UPDATE ... INSERT OR IGNORE INTO log VALUES(1)": the
// upsert spelling FAILS "UNIQUE constraint failed: log.x" and leaves t
// unchanged, while "UPDATE t SET c=99" succeeds and the IGNORE absorbs it.
func TestUpsertUpdateTriggerOrconf(t *testing.T) {
	setup := []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x UNIQUE)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT OR IGNORE INTO log VALUES(1); END`,
		`INSERT INTO log VALUES(1)`, `INSERT INTO t(a,b,c) VALUES(1,2,10)`}

	_, err := execScript(t, setup,
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		`SELECT a,b,c FROM t`)
	if err == nil {
		t.Fatalf("upsert DO UPDATE: expected the body's OR IGNORE to be overridden to ABORT")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed: log.x") {
		t.Fatalf("upsert DO UPDATE: %v, want a UNIQUE violation on log.x", err)
	}
	got, gerr := execScript(t, setup, nil, `SELECT a,b,c FROM t`)
	wantRows(t, "upsert aborted, table untouched", got, gerr, "1,2,10")

	// The contrast, and the half that must NOT change: a written-out UPDATE
	// passes OE_Default, so the body keeps its own IGNORE.
	got, gerr = execScript(t, setup, []string{`UPDATE t SET c=99`}, `SELECT a,b,c FROM t`)
	wantRows(t, "plain UPDATE keeps the body's IGNORE", got, gerr, "1,2,99")
}

// ---- shape 3: conflict write vs DELETE triggers, recursive_triggers ON ----

func TestReplaceVictimUpdateCompiles(t *testing.T) {
	setup := []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tbd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b'||old.a); END`,
		`CREATE TRIGGER tad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`}
	prog := compileConflictShape(t, setup, `UPDATE OR REPLACE t SET a=1`)
	// Non-vacuity, and it needs the PLAN rather than an opcode: the victim's
	// programs are run straight out of updatePlan by opUpdateRow, reached
	// through no OpFireTriggers at all (which is why subProgramsOf has an
	// *updatePlan case).
	var plan *updatePlan
	for i := range prog.Insns {
		if p, ok := prog.Insns[i].P4.(*updatePlan); ok && prog.Insns[i].Op == OpUpdateRow {
			plan = p
		}
	}
	if plan == nil {
		t.Fatalf("no OpUpdateRow in the compiled program")
	}
	if plan.replaceDelBefore == nil || plan.replaceDelAfter == nil {
		t.Fatalf("victim-delete plans: before=%v after=%v, want both compiled",
			plan.replaceDelBefore != nil, plan.replaceDelAfter != nil)
	}
	// With the pragma OFF the C reads no trigger list at all (insert.c:2220-2222
	// is inside "if( db->flags&SQLITE_RecTriggers )"), so nothing is compiled.
	prog = compileConflictShape(t, setup[1:], `UPDATE OR REPLACE t SET a=1`)
	for i := range prog.Insns {
		if p, ok := prog.Insns[i].P4.(*updatePlan); ok && prog.Insns[i].Op == OpUpdateRow {
			if p.replaceDelBefore != nil || p.replaceDelAfter != nil {
				t.Errorf("recursive_triggers OFF: victim-delete plans compiled anyway")
			}
		}
	}
}

func TestReplaceVictimUpdateAnswers(t *testing.T) {
	setup := []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tbd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b'||old.a); END`,
		`CREATE TRIGGER tad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('a'||old.a); END`,
		`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`}
	got, err := execScript(t, setup, []string{`UPDATE OR REPLACE t SET a=a+1 WHERE a>=2`}, `SELECT x FROM log`)
	wantRows(t, "victim delete triggers fire", got, err, "b3", "a3")

	// The victim IS a later match: it is deleted, its triggers fire, and its
	// own update never happens (the re-seek finds nothing).
	setup = []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
		`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`}
	got, err = execScript(t, setup, []string{`UPDATE OR REPLACE t SET a=a+1`}, `SELECT a,b FROM t ORDER BY a`)
	wantRows(t, "victim is a later match", got, err, "2,one", "4,three")
	got, err = execScript(t, setup, []string{`UPDATE OR REPLACE t SET a=a+1`}, `SELECT x FROM log`)
	wantRows(t, "victim is a later match, log", got, err, "2")

	// With the pragma OFF nothing fires, which is the same statement's other
	// half and the thing SetRecursiveTriggers has to drop the plan cache for.
	got, err = execScript(t, setup[1:], []string{`UPDATE OR REPLACE t SET a=a+1`}, `SELECT x FROM log`)
	wantRows(t, "recursive_triggers OFF, log", got, err)

	// A BEFORE DELETE RAISE(IGNORE) leaves the victim in place, so the recheck
	// finds it and the statement aborts -- and the table is untouched.
	got, err = execScript(t,
		[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`,
			`CREATE TRIGGER tbd BEFORE DELETE ON t BEGIN SELECT RAISE(IGNORE); END`,
			`INSERT INTO t VALUES(1,'one'),(2,'two')`},
		[]string{`UPDATE OR REPLACE t SET a=a+1 WHERE a=1`}, `SELECT a,b FROM t ORDER BY a`)
	if err == nil {
		t.Fatalf("BEFORE RAISE(IGNORE): expected the post-cascade recheck to abort")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("BEFORE RAISE(IGNORE): %v, want a UNIQUE violation from the recheck", err)
	}
}

// TestReplaceVictimUpdatePin is insert.c:2611-2618's OP_CursorLock, and the
// data-destroying wrong answer it retired. See writeCtx.pinnedTable for the C
// and the oracle measurement; the short version is that a victim's DELETE
// trigger which WRITES the table the UPDATE is walking is
// SQLITE_CONSTRAINT_PINNED with the table untouched, and this engine used to
// answer success having dropped two of three rows.
func TestReplaceVictimUpdatePin(t *testing.T) {
	for _, body := range []string{
		`DELETE FROM t WHERE b='three'`,
		`INSERT INTO t VALUES(old.a,'resurrected')`,
		`UPDATE t SET b='edited' WHERE b='three'`,
	} {
		setup := []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`,
			`CREATE TRIGGER tad AFTER DELETE ON t BEGIN ` + body + `; END`,
			`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`}
		got, err := execScript(t, setup, []string{`UPDATE OR REPLACE t SET a=a+1 WHERE b='one'`},
			`SELECT a,b FROM t ORDER BY a`)
		if err == nil {
			t.Errorf("body %q: expected a pinned-cursor constraint failure, got none", body)
			continue
		}
		if !strings.Contains(err.Error(), "constraint failed") {
			t.Errorf("body %q: %v, want \"constraint failed\"", body, err)
		}
		got, gerr := execScript(t, setup, nil, `SELECT a,b FROM t ORDER BY a`)
		wantRows(t, "pinned statement leaves the table untouched (body "+body+")", got, gerr,
			"1,one", "2,two", "3,three")
	}
	// A victim's DELETE trigger that writes some OTHER table is the ordinary,
	// unpinned case and must keep working -- saveAllCursors filters by root
	// page, so only a write to the pinned table's own b-tree trips it.
	got, err := execScript(t,
		[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO t VALUES(1,'one'),(2,'two')`},
		[]string{`UPDATE OR REPLACE t SET a=a+1 WHERE b='one'`}, `SELECT x FROM log`)
	wantRows(t, "unpinned: a write to another table", got, err, "2")

	// And the INSERT half has NO pin at all -- insert.c:2611 tests isUpdate --
	// so the same trigger body must be allowed there.
	got, err = execScript(t,
		[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`,
			`CREATE TRIGGER tad AFTER DELETE ON t BEGIN DELETE FROM t WHERE b='three'; END`,
			`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`},
		[]string{`INSERT OR REPLACE INTO t VALUES(1,'new')`}, `SELECT a,b FROM t ORDER BY a`)
	wantRows(t, "INSERT half is unpinned", got, err, "1,new", "2,two")
}
