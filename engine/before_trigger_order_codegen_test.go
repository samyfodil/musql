package engine

// Tests BEFORE-trigger emission order in INSERT statements. Verifies that
// the BEFORE program fires before constraint checks and rowid generation.

import "testing"

// beforeOrderCase is a table whose ONLY trigger is a BEFORE one, so the program
// carries exactly ONE OpFireTriggers and no disambiguation is needed.
type beforeOrderCase struct {
	name  string
	setup []string
	stmt  string
	// after names the opcodes that MUST be emitted below the fire, each with
	// the C line that puts it there.
	after []OpCode
}

var beforeOrderCases = []beforeOrderCase{
	// NOT NULL under ABORT: OpHaltIfNull. insert.c:2020-2030, inside
	// sqlite3GenerateConstraintChecks, which sqlite3Insert reaches at :1569 --
	// below the BEFORE call at :1494.
	{"notnull-abort", []string{
		`CREATE TABLE t(a, b NOT NULL)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'x')`,
		[]OpCode{OpHaltIfNull, OpNewRowid, OpInsert}},

	// NOT NULL under an explicit OR IGNORE: the check becomes an OpIsNull jump
	// to the row end (insert.c:2033-2036's "OP_IsNull, iReg, ignoreDest"). This
	// is the case the lifted guard was ABOUT -- the oracle keeps the BEFORE
	// program's writes for the row it skips. The table has no INTEGER PRIMARY
	// KEY and no explicit rowid target, so the rowid switch emits a bare
	// OpNewRowid and this OpIsNull is the only one in the program.
	{"notnull-ignore", []string{
		`CREATE TABLE t(a, b NOT NULL)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t VALUES(1,'x')`,
		[]OpCode{OpIsNull, OpNewRowid, OpInsert}},

	// A DECLARED ON CONFLICT default reaches the same crossing without any
	// OR-clause at all -- which is why the lifted guard's first term was
	// "db.tableHasDeclaredConflict(tbl)" and not just "stmt.explicitOr".
	{"declared-notnull-ignore", []string{
		`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'x')`,
		[]OpCode{OpIsNull, OpNewRowid, OpInsert}},

	// CHECK under ABORT: OpHaltError, from emitCheckConstraintsAction, which
	// the BEFORE-trigger trio had already moved below the fire (insert.c:2062+,
	// same GenerateConstraintChecks call).
	{"check-abort", []string{
		`CREATE TABLE t(a, b, CHECK(a<10))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'x')`,
		[]OpCode{OpHaltError, OpNewRowid, OpInsert}},

	// The rowid allocation itself, on a table whose INTEGER PRIMARY KEY the
	// statement leaves to the engine: insert.c:1538-1540's OP_NewRowid, past
	// the fire. This is the half the BEFORE-trigger trio moved, asserted here
	// because the two halves share one emitter and a later edit could undo
	// either.
	{"ipk-newrowid", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t(b) VALUES('x')`,
		[]OpCode{OpNewRowid, OpInsert}},

	// NOT NULL ON CONFLICT REPLACE: the DEFAULT substitution (insert.c:2008-2014)
	// moved down WITH the NOT NULL loop it belongs to, so its OpGoto/OpIsNull
	// pair is below the fire too. Asserted through OpHaltIfNull, which is the
	// re-check the substitution falls into (REPLACE demoted to ABORT).
	{"notnull-replace-default", []string{
		`CREATE TABLE t(a, b NOT NULL ON CONFLICT REPLACE DEFAULT 'dd')`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,NULL)`,
		[]OpCode{OpHaltIfNull, OpNewRowid, OpInsert}},

	// The SELECT spelling, which is a DIFFERENT emitter for everything above
	// the row body (compileInsertSelectWrite) and the SAME one for the row body
	// itself. C SQLite draws no distinction here at all -- both trigger
	// calls sit inside its ONE insertion loop (insert.c:1495 and :1604-1608,
	// above the endOfLoop the source's OP_Next targets at :1613/:1615) -- and
	// this case exists because the two emitters DID differ: the SELECT one
	// fired the BEFORE program from its tail, below the checks, until it was
	// handed to emitInsertRowBody through compiler.insertBeforeFire.
	{"select-source-notnull-ignore", []string{
		`CREATE TABLE t(a UNIQUE, b NOT NULL)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t SELECT a,b FROM s`,
		[]OpCode{OpIsNull, OpNewRowid, OpInsert}},

	{"select-source-declared-notnull-ignore", []string{
		`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a,b FROM s`,
		[]OpCode{OpIsNull, OpNewRowid, OpInsert}},
}

// beforeUpdateOrderCases is the UPDATE half of the same defect, in a DIFFERENT
// emitter: compileUpdateStmt builds its own row body rather than calling
// emitInsertRowBody, and it too ran the checks above the fire. update.c is
// unusually explicit about the order it wants --
//
//	/* Fire any BEFORE UPDATE triggers. This happens before constraints are
//	** verified. One could argue that this is wrong. */
//	                                              -- update.c:978-980
//
// -- with sqlite3GenerateConstraintChecks following at :1030. OpMakeRecord is
// asserted alongside the checks because it had to move with them: the NOT NULL
// ON CONFLICT REPLACE substitution writes into the row registers the record is
// built from.
var beforeUpdateOrderCases = []beforeOrderCase{
	{"update-notnull-abort", []string{
		`CREATE TABLE t(a, b NOT NULL)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE t SET b=NULL`,
		[]OpCode{OpHaltIfNull, OpMakeRecord, OpUpdateRow}},

	{"update-notnull-ignore", []string{
		`CREATE TABLE t(a, b NOT NULL)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE OR IGNORE t SET b=NULL`,
		[]OpCode{OpIsNull, OpMakeRecord, OpUpdateRow}},

	{"update-declared-notnull-ignore", []string{
		`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE t SET b=NULL`,
		[]OpCode{OpIsNull, OpMakeRecord, OpUpdateRow}},

	{"update-check-abort", []string{
		`CREATE TABLE t(a, b, CHECK(b<10))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE t SET b=b+1`,
		[]OpCode{OpHaltError, OpMakeRecord, OpUpdateRow}},

	// The STRICT datatype check is the one thing that must be ABOVE the fire
	// here, because update.c:981's sqlite3TableAffinity sits inside the
	// "if( tmask&TRIGGER_BEFORE )" arm -- so this case asserts the OPPOSITE
	// direction, and TestBeforeUpdateTypeCheckStraddlesTheFire below asserts
	// that it flips when the trigger does.
	{"update-strict-notnull", []string{
		`CREATE TABLE t(i INT, n INT NOT NULL) STRICT`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.i); END`},
		`UPDATE OR IGNORE t SET n=NULL, i=1`,
		[]OpCode{OpIsNull, OpMakeRecord, OpUpdateRow}},
}

// TestBeforeUpdateFiresAheadOfTheConstraintChecks is the UPDATE half of
// TestBeforeInsertFiresAheadOfTheConstraintChecks.
func TestBeforeUpdateFiresAheadOfTheConstraintChecks(t *testing.T) {
	for _, tc := range beforeUpdateOrderCases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := compileShape(t, conflictShapeCase{setup: tc.setup, stmt: tc.stmt})
			if err != nil {
				t.Fatalf("RULE #1: %q should COMPILE, got error: %v", tc.stmt, err)
			}
			fire := onlyOpIndex(t, prog, OpFireTriggers)
			for _, op := range tc.after {
				at := onlyOpIndex(t, prog, op)
				if at < fire {
					t.Errorf("%s is emitted at %d, ABOVE the BEFORE fire at %d.\n"+
						"update.c fires BEFORE UPDATE triggers at :984 and reaches\n"+
						"sqlite3GenerateConstraintChecks only at :1030 -- \"This happens before\n"+
						"constraints are verified. One could argue that this is wrong.\" (:978-980).\n"+
						"Emitting a check above the fire is unobservable under ABORT and WRONG under\n"+
						"IGNORE or FAIL.", op, at, fire)
				}
			}
		})
	}
}

// TestBeforeUpdateTypeCheckStraddlesTheFire pins the one emission whose side of
// the fire DEPENDS on the trigger, which is update.c's own conditional:
//
//	if( tmask&TRIGGER_BEFORE ){
//	  sqlite3TableAffinity(v, pTab, regNew);
//	  sqlite3CodeRowTrigger(...TRIGGER_BEFORE...);
//	                                              -- update.c:981-985
//
// sqlite3TableAffinity emits the isolated OP_TypeCheck for a STRICT table
// (insert.c:179-201), so a BEFORE program puts the datatype check ahead of the
// NOT NULL loop and nothing else does. It is OBSERVABLE -- over
// "u(i INT, t TEXT, n INT NOT NULL) STRICT", "UPDATE u SET n=NULL, i='abc'"
// answers "NOT NULL constraint failed: u.n" on 3.53.3 with no trigger AND with
// an AFTER one, and "cannot store TEXT value in INT column u.i" with a BEFORE
// one. The AFTER row is what makes this a fact about the BEFORE TIMING rather
// than about triggers, and it is why this is asserted rather than reasoned
// about: emitting OpTypeCheck unconditionally on either side is a wrong answer
// for half the tables.
func TestBeforeUpdateTypeCheckStraddlesTheFire(t *testing.T) {
	const strictTable = `CREATE TABLE u(i INT, t TEXT, n INT NOT NULL) STRICT`
	for _, tc := range []struct {
		name         string
		setup        []string
		typeAboveNot bool
	}{
		{"no-trigger", []string{strictTable}, false},
		{"after-trigger", []string{strictTable, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(old.i); END`}, false},
		{"before-trigger", []string{strictTable, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tb BEFORE UPDATE ON u BEGIN INSERT INTO log VALUES(old.i); END`}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := compileShape(t, conflictShapeCase{setup: tc.setup,
				stmt: `UPDATE u SET n=NULL, i='abc'`})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			typ := onlyOpIndex(t, prog, OpTypeCheck)
			nn := onlyOpIndex(t, prog, OpHaltIfNull)
			if got := typ < nn; got != tc.typeAboveNot {
				t.Errorf("OpTypeCheck at %d, NOT NULL OpHaltIfNull at %d: type-first = %v, want %v.\n"+
					"update.c emits sqlite3TableAffinity (which IS OP_TypeCheck for a STRICT table)\n"+
					"only inside the BEFORE arm at :981; with no BEFORE program the check happens\n"+
					"inside sqlite3GenerateConstraintChecks instead (insert.c:2080), below NOT NULL.\n"+
					"Measured on 3.53.3: no trigger and AFTER report the NOT NULL, BEFORE reports\n"+
					"the datatype.", typ, nn, got, tc.typeAboveNot)
			}
		})
	}
}

// TestBeforeInsertFiresAheadOfTheConstraintChecks is the assertion the lifted
// guard rests on.
func TestBeforeInsertFiresAheadOfTheConstraintChecks(t *testing.T) {
	for _, tc := range beforeOrderCases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := compileShape(t, conflictShapeCase{setup: tc.setup, stmt: tc.stmt})
			if err != nil {
				t.Fatalf("RULE #1: %q should COMPILE, got error: %v", tc.stmt, err)
			}
			fire := onlyOpIndex(t, prog, OpFireTriggers)
			for _, op := range tc.after {
				at := onlyOpIndex(t, prog, op)
				if at < fire {
					t.Errorf("%s is emitted at %d, ABOVE the BEFORE fire at %d.\n"+
						"insert.c fires the BEFORE program at :1494-1496 and reaches the rowid\n"+
						"(:1531-1540) and sqlite3GenerateConstraintChecks (:1569-1571) only after it.\n"+
						"Emitting a check above the fire is unobservable under ABORT and WRONG under\n"+
						"IGNORE or FAIL, which is the crossing compileInsertStmt used to decline.",
						op, at, fire)
				}
			}
		})
	}
}

// TestBeforeInsertFireIsNotVacuous guards the guard: every case above asserts
// "X comes after the fire", which a program with NO fire at all would satisfy
// vacuously -- and losing the fire is exactly what a botched edit to
// compiler.insertBeforeFire would do. It also pins the fire's payload as the
// BEFORE plan (its skipAddr is the row end, and it carries a NEW row and no
// OLD one -- insert.c has no OLD.* at INSERT time).
func TestBeforeInsertFireIsNotVacuous(t *testing.T) {
	for _, tc := range beforeOrderCases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := compileShape(t, conflictShapeCase{setup: tc.setup, stmt: tc.stmt})
			if err != nil {
				t.Fatalf("compile %q: %v", tc.stmt, err)
			}
			at := onlyOpIndex(t, prog, OpFireTriggers)
			fp, ok := prog.Insns[at].P4.(*triggerFirePlan)
			if !ok || fp == nil {
				t.Fatalf("OpFireTriggers at %d carries no *triggerFirePlan", at)
			}
			if len(fp.triggers) != 1 {
				t.Errorf("the BEFORE plan carries %d triggers, want the one this setup creates",
					len(fp.triggers))
			}
			if !fp.hasNew || fp.hasOld {
				t.Errorf("BEFORE INSERT plan: hasNew=%v hasOld=%v, want true/false", fp.hasNew, fp.hasOld)
			}
			// insert.c:1448-1450 -- "on a BEFORE trigger, we do not know what
			// the unique ID will be (because the insert has not happened yet)
			// so we substitute a rowid of -1". P2 is that register, and it must
			// NOT be the register OpInsert stores with.
			ins := onlyOpIndex(t, prog, OpInsert)
			if prog.Insns[at].P2 == prog.Insns[ins].P3 {
				t.Errorf("the BEFORE fire reads the same rowid register OpInsert stores with (%d).\n"+
					"NEW.rowid must be the -1/explicit value settled at insert.c:1452-1467, not the\n"+
					"allocated one.", prog.Insns[at].P2)
			}
		})
	}
}

// onlyOpIndex returns the index of the single instruction with opcode op in
// prog's TOP-LEVEL instruction list, failing if there is not exactly one. The
// "exactly one" is deliberate: it is what lets these cases assert on a bare
// opcode without a disambiguator, and it fails loudly if a future emitter adds
// a second use rather than quietly asserting about the wrong instruction.
func onlyOpIndex(t *testing.T, prog *Program, op OpCode) int {
	t.Helper()
	found := -1
	for i := range prog.Insns {
		if prog.Insns[i].Op != op {
			continue
		}
		if found >= 0 {
			t.Fatalf("%s appears at both %d and %d; this assertion needs exactly one", op, found, i)
		}
		found = i
	}
	if found < 0 {
		t.Fatalf("%s is not emitted at all", op)
	}
	return found
}
