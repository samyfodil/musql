// Tests engine-side RETURNING with triggers, verifying that RETURNING fires
// before user AFTER triggers.
package engine

import (
	"errors"
	"strings"
	"testing"
)

// returningTriggerShapes: RETURNING crossed with a real trigger on the target,
// one per verb, plus the variations that exercise the guards the order runs
// below. Not one of them compiled to bytecode before this batch.
var returningTriggerShapes = []conflictShapeCase{
	// The three census shapes, verbatim from writeShapeResidualCases.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,2) RETURNING a,b`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM t WHERE a=1 RETURNING a,b`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`UPDATE t SET b=9 RETURNING a,b`},

	// BEFORE triggers. RETURNING sits BELOW a BEFORE program's ignoreJump, so
	// this is the arm that would silently emit a row for an IGNOREd write if
	// the label were resolved above the block.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,2) RETURNING a,b`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`UPDATE t SET b=9 RETURNING a,b`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM t WHERE a=1 RETURNING a,b`},

	// Both timings at once, which is the shape that pins that the two AFTER
	// entries (RETURNING and the user program) are ORDERED rather than merely
	// both present.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.b); END`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`},
		`UPDATE t SET b=9 RETURNING a,b`},

	// RETURNING * and a rowid reference beside a trigger.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,2) RETURNING *, rowid`},
	// An INTEGER PRIMARY KEY: emitReturning remaps the IPK slot to the rowid
	// register, and the AFTER program reads the same register block.
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t(b) VALUES(2) RETURNING a,b`},
	// A generated column beside a trigger: rederiveGeneratedFromRowid runs
	// inside the block, so it must still run at its new position.
	{[]string{`CREATE TABLE t(id INTEGER PRIMARY KEY, v, g AS (id*2))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.v); END`},
		`INSERT INTO t(v) VALUES('x') RETURNING id,v,g`},
	// A multi-tuple VALUES list beside a trigger: each tuple gets its own
	// RETURNING block AND its own AFTER fire, in that order, per tuple.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,2),(3,4) RETURNING a,b`},
	// A NESTED trigger: the AFTER program writes a table that is itself
	// triggered. This is the case the program-GRAPH walk exists for.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,new.b); END`},
		`INSERT INTO t VALUES(1,2) RETURNING a,b`},
	// A trigger on a table the statement does NOT target, which must not gate
	// anything -- the guard is per-table.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE other(x)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tro AFTER INSERT ON other BEGIN INSERT INTO log VALUES(new.x); END`},
		`INSERT INTO t VALUES(1,2) RETURNING a,b`},
}

func TestReturningBesideATriggerCompilesToBytecode(t *testing.T) {
	for i, tc := range returningTriggerShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestReturningIsCodedBeforeTheAfterTrigger is the ORDER assertion, made
// against the emitted program rather than against an outcome, because the
// outcome only differs when an AFTER program RAISEs IGNORE -- a shape the
// behavioural test below covers, but which a reader of this file should be able
// to see pinned structurally too.
//
// C's order is a list order (trigger.c:68-78 prepends, trigger.c:1484 walks
// head-first), so the assertion is positional: the OpResultRow the RETURNING
// block ends with must precede the AFTER OpFireTriggers.
func TestReturningIsCodedBeforeTheAfterTrigger(t *testing.T) {
	for _, tc := range []conflictShapeCase{
		{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1,2) RETURNING a,b`},
		{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tr AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`UPDATE t SET b=9 RETURNING a,b`},
		{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tr AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
			`DELETE FROM t WHERE a=1 RETURNING a,b`},
	} {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("%q: compile: %v", tc.stmt, err)
			continue
		}
		// Each case above carries exactly ONE trigger, an AFTER one, so the
		// single OpFireTriggers in the program IS the AFTER fire -- no timing
		// field is needed to tell them apart, and nFire below fails the case
		// rather than let a second one make the comparison meaningless.
		resultRow, afterFire, nFire := -1, -1, 0
		for i := range prog.Insns {
			switch prog.Insns[i].Op {
			case OpResultRow:
				if resultRow < 0 {
					resultRow = i
				}
			case OpFireTriggers:
				nFire++
				if afterFire < 0 {
					afterFire = i
				}
			}
		}
		if nFire != 1 {
			t.Errorf("%q: %d OpFireTriggers, want exactly 1 -- this case can no longer tell the\n"+
				"AFTER fire apart from a BEFORE one, so its comparison means nothing", tc.stmt, nFire)
			continue
		}
		if resultRow < 0 {
			t.Errorf("%q: no OpResultRow -- the RETURNING block is not emitted at all", tc.stmt)
			continue
		}
		if afterFire < 0 {
			t.Errorf("%q: no AFTER OpFireTriggers -- this case is not testing what it claims", tc.stmt)
			continue
		}
		if resultRow > afterFire {
			t.Errorf("%q: the RETURNING OpResultRow is at %d, BELOW the AFTER OpFireTriggers at %d.\n"+
				"SQLite codes RETURNING FIRST among the AFTER triggers (sqlite3TriggerList prepends\n"+
				"the synthetic trigger, trigger.c:68-78; sqlite3CodeRowTrigger walks head-first,\n"+
				"trigger.c:1484), so an AFTER program that RAISEs IGNORE must land PAST a row that\n"+
				"has already been captured. Emitted below the fire, this answers no row at all.",
				tc.stmt, resultRow, afterFire)
		}
	}
}

// TestReturningTriggerOrderIsObservable is the behavioural half, and it is what
// makes the positional assertion above more than a style rule. Each case below
// was run against the 3.53.3 oracle; want is the oracle's own answer.
//
//	AFTER  INSERT RAISE(IGNORE) + RETURNING -> the row (RETURNING ran first)
//	BEFORE INSERT RAISE(IGNORE) + RETURNING -> nothing
//	AFTER  UPDATE RAISE(IGNORE) + RETURNING -> every row
//	BEFORE UPDATE RAISE(IGNORE) + RETURNING -> nothing
//	AFTER  DELETE RAISE(IGNORE) + RETURNING -> the row
//	BEFORE DELETE RAISE(IGNORE) + RETURNING -> nothing
//
// A cascade that removes a row the scan has not reached yet is the other half:
// the row-gone guard skips the RETURNING block too, so the vanished row is not
// reported. Both spellings measured on the oracle.
func TestReturningTriggerOrderIsObservable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  []string
	}{
		{"after-insert-ignore",
			[]string{`CREATE TABLE w(a,b)`,
				`CREATE TRIGGER wig AFTER INSERT ON w BEGIN SELECT RAISE(IGNORE); END`},
			`INSERT INTO w VALUES(1,2) RETURNING a,b`, []string{"1|2"}},
		{"before-insert-ignore",
			[]string{`CREATE TABLE w(a,b)`,
				`CREATE TRIGGER wig BEFORE INSERT ON w BEGIN SELECT RAISE(IGNORE); END`},
			`INSERT INTO w VALUES(1,2) RETURNING a,b`, nil},
		{"after-update-ignore",
			[]string{`CREATE TABLE t1(a,b)`, `INSERT INTO t1 VALUES(1,'p'),(2,'q'),(3,'r')`,
				`CREATE TRIGGER t1au AFTER UPDATE ON t1 BEGIN SELECT RAISE(IGNORE); END`},
			`UPDATE t1 SET b='Z' RETURNING a,b`, []string{"1|Z", "2|Z", "3|Z"}},
		{"before-update-ignore",
			[]string{`CREATE TABLE t1(a,b)`, `INSERT INTO t1 VALUES(1,'p'),(2,'q'),(3,'r')`,
				`CREATE TRIGGER t1bu BEFORE UPDATE ON t1 BEGIN SELECT RAISE(IGNORE); END`},
			`UPDATE t1 SET b='Y' RETURNING a,b`, nil},
		{"after-delete-ignore",
			[]string{`CREATE TABLE t1(a,b)`, `INSERT INTO t1 VALUES(1,'p'),(2,'q')`,
				`CREATE TRIGGER t1ad AFTER DELETE ON t1 BEGIN SELECT RAISE(IGNORE); END`},
			`DELETE FROM t1 WHERE a=1 RETURNING a,b`, []string{"1|p"}},
		{"before-delete-ignore",
			[]string{`CREATE TABLE t1(a,b)`, `INSERT INTO t1 VALUES(1,'p'),(2,'q')`,
				`CREATE TRIGGER t1bd BEFORE DELETE ON t1 BEGIN SELECT RAISE(IGNORE); END`},
			`DELETE FROM t1 WHERE a=1 RETURNING a,b`, nil},
		// The cascade cases. delete.c:768-774 / update.c:990-999 skip a row a
		// trigger already removed, and RETURNING is below that guard.
		{"delete-cascade-removes-a-later-match",
			[]string{`CREATE TABLE t2(a,b)`, `INSERT INTO t2 VALUES(1,'p'),(2,'q'),(3,'r'),(4,'s')`,
				`CREATE TRIGGER t2ad AFTER DELETE ON t2 BEGIN DELETE FROM t2 WHERE a=old.a+2; END`},
			`DELETE FROM t2 WHERE a=1 OR a=3 RETURNING a,b`, []string{"1|p"}},
		{"update-cascade-removes-a-later-match",
			[]string{`CREATE TABLE t3(a,b)`, `INSERT INTO t3 VALUES(1,'p'),(2,'q'),(3,'r'),(4,'s')`,
				`CREATE TRIGGER t3au AFTER UPDATE ON t3 BEGIN DELETE FROM t3 WHERE a=old.a+2; END`},
			`UPDATE t3 SET b='x-'||b WHERE a=1 OR a=3 RETURNING a,b`, []string{"1|x-p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			// It has to be real bytecode: a shape that failed to lower would
			// make this measure something else and pass for the wrong reason.
			_, cerr := db.compileWrite(tc.stmt)
			if cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			got := execReturningRows(t, db, tc.stmt)
			if strings.Join(got, ";") != strings.Join(tc.want, ";") {
				t.Errorf("%q\n got %v\nwant %v (the 3.53.3 oracle's answer)", tc.stmt, got, tc.want)
			}
		})
	}
}

// TestVtabReturningIsRejected pins the one member of this cluster that is not a
// promotion. "UPDATE/DELETE ... RETURNING" against ANY virtual table is a
// prepare-time ERROR in C SQLite (triggersReallyExist, trigger.c:838-852),
// so the compiler has to raise that error ITSELF, with SQLite's wording, rather
// than let the statement through to be refused a second way further down. See
// vtabReturningRejected for the C and the oracle measurements.
//
// It requires errVDBESemantic specifically: that is the sentinel a decline is
// propagated VERBATIM under (compileInsertSelectWrite, vdbe_write.go),
// where a bare errVDBEUnsupported is a capability decline whose wording says
// nothing about why C SQLite rejects the statement.
func TestVtabReturningIsRejected(t *testing.T) {
	for _, tc := range []struct {
		setup []string
		stmt  string
		want  string
	}{
		{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`UPDATE f SET x='b' RETURNING x`, "UPDATE RETURNING is not available on virtual tables"},
		{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`DELETE FROM f WHERE x='a' RETURNING x`, "DELETE RETURNING is not available on virtual tables"},
		{[]string{`CREATE VIRTUAL TABLE f USING fts3(x)`, `INSERT INTO f VALUES('a')`},
			`UPDATE f SET x='b' RETURNING x`, "UPDATE RETURNING is not available on virtual tables"},
		{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `INSERT INTO r VALUES(1,0.0,1.0)`},
			`UPDATE r SET x1=2.0 RETURNING id`, "UPDATE RETURNING is not available on virtual tables"},
		{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `INSERT INTO r VALUES(1,0.0,1.0)`},
			`DELETE FROM r RETURNING id`, "DELETE RETURNING is not available on virtual tables"},
		// The refusal precedes the SET-target and WHERE name resolution, and it
		// fires for a statement that would have matched no row -- both are the
		// oracle's own precedence (measured; see vtabReturningRejected).
		{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`UPDATE f SET nosuchcol='b' RETURNING x`, "UPDATE RETURNING is not available on virtual tables"},
		{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`UPDATE f SET x='b' WHERE 0 RETURNING x`, "UPDATE RETURNING is not available on virtual tables"},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		_, cerr := db.compileWrite(tc.stmt)
		switch {
		case cerr == nil:
			t.Errorf("%q compiled. C SQLite REJECTS it at prepare time (trigger.c:838-852).", tc.stmt)
		case !errors.Is(cerr, errVDBESemantic):
			t.Errorf("%q: error %v does not wear errVDBESemantic, so it reads as a capability\n"+
				"decline rather than as SQLite's own prepare-time rejection.", tc.stmt, cerr)
		case !strings.Contains(cerr.Error(), tc.want):
			t.Errorf("%q: got %q, want it to contain %q (SQLite's own wording)", tc.stmt, cerr, tc.want)
		}
		// It must also survive Exec, not just compileWrite.
		if err := db.Exec(tc.stmt); err == nil {
			t.Errorf("%q: Exec succeeded where C SQLite errors", tc.stmt)
		}
		db.Discard()
	}

	// INSERT is the ONE verb the C lets through (trigger.c:846's "op!=TK_INSERT"
	// guard), so it must NOT be rejected here. writeShapeResidualCases carries
	// it as "vtab INSERT ... RETURNING", and this asserts the rejection did not
	// widen onto it.
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	if err := db.Exec(`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, cerr := db.compileWrite(`INSERT INTO r VALUES(1,0.0,1.0) RETURNING id`); cerr != nil {
		t.Errorf("INSERT ... RETURNING into a vtab became an error (%v). C SQLite ALLOWS it --\n"+
			"trigger.c:846 rejects only UPDATE and DELETE -- and the oracle answers 1.", cerr)
	}
}

// execReturningRows runs a RETURNING statement through the engine and returns
// its rows as "col|col" strings.
func execReturningRows(t *testing.T, db *Session, sql string) []string {
	t.Helper()
	_, rows, err := db.ExecReturningArgs(sql, nil)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	var out []string
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = valueDebugString(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}
