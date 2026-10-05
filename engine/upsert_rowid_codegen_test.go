package engine

// Test UPSERT with rowid assignments in the DO UPDATE clause.
// Verify these statements compile correctly without errors.

import (
	"strings"
	"testing"
)

// upsertRowidShapes contains test cases where DO UPDATE assigns the rowid.
var upsertRowidShapes = []conflictShapeCase{
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=9`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=a+100, b=excluded.b`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=excluded.a+40 WHERE b='x'`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT DO UPDATE SET a=9`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,CHECK(a<100))`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=500`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,g AS (a*2))`}, `INSERT INTO t(a,b) VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=6`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b TEXT) STRICT`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=3, b=4`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b UNIQUE)`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=9, b='w'`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE INDEX ix ON t(b)`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=t.a*2`},
}

func TestUpsertRowidReassignCompilesToBytecode(t *testing.T) {
	for _, tc := range upsertRowidShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: %q should COMPILE, got error: %v", tc.stmt, err)
			continue
		}
	}
}

// TestUpsertRowidReassignEmitsMustBeInt is the NON-VACUITY half of the
// assertion above. "it compiles" is satisfied by a program that quietly stores
// the new key in the row image and never moves the row, which is exactly what
// deleting the decline WITHOUT adding the rowid register would produce -- and
// what the compile test alone cannot see.
//
// Two structural facts are asserted, both of which the C fixes:
//
//   - an OpMustBeInt with NO jump target (P2 == 0) is emitted, which is
//     update.c:894's one-operand spelling. Its absence is a wrong answer, not
//     a missing feature: "DO UPDATE SET a='abc'" / "SET a=NULL" / "SET a=7.5"
//     are each "datatype mismatch" in the 3.53.3 oracle, and without it they
//     would store a rowid of 0.
//   - OpUpsertStore's P3 (the NEW rowid) differs from its P2 (the existing
//     one). They are the SAME register for every DO UPDATE that does not
//     assign the key -- update.c:882-884's "regNewRowid is the same register
//     as regOldRowid" when the rowid is not being modified -- so this is the
//     one bit that says the row can actually move.
func TestUpsertRowidReassignEmitsMustBeInt(t *testing.T) {
	prog, err := compileShape(t, conflictShapeCase{
		[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`},
		`INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET a=9`,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	sawMustBeInt, sawMovingStore := false, false
	for i := range prog.Insns {
		switch prog.Insns[i].Op {
		case OpMustBeInt:
			if prog.Insns[i].P2 != 0 {
				t.Errorf("OpMustBeInt carries a jump target (P2=%d); update.c:894 emits the one-operand form, so a non-integer key is a hard error",
					prog.Insns[i].P2)
			}
			sawMustBeInt = true
		case OpUpsertStore:
			if prog.Insns[i].P2 != prog.Insns[i].P3 {
				sawMovingStore = true
			}
		}
	}
	if !sawMustBeInt {
		t.Errorf("no OpMustBeInt in the DO UPDATE block: a non-integer rowid would be stored instead of raising \"datatype mismatch\"")
	}
	if !sawMovingStore {
		t.Errorf("OpUpsertStore's new-rowid register equals its existing-rowid register: the row cannot move, so the SET is silently dropped")
	}
}

// TestUpsertWithoutRowidReassignUnchanged pins the neighbouring shape this
// promotion deliberately did NOT touch. A WITHOUT ROWID table has ipkIndex ==
// -1, so the old "idx == tbl.ipkIndex" decline could never fire for it and
// "DO UPDATE SET <pk column> = ..." ALREADY compiled -- correctly, because the
// row store's key is opaque and independent of the PRIMARY KEY columns, so
// nothing moves and checkUniqueIndexesForRow still enforces the key.
//
// That asymmetry is exactly the hole a reviewer found in batch H2's
// neighbouring decline (the "conflict clause reassigning the row identity"
// one, which had to grow a WITHOUT ROWID arm -- and which is itself gone now:
// compileUpdateStmt emits update.c's re-seek for both key kinds instead of
// refusing the shape, so its predicate was deleted with it). It is recorded
// here as a PIN rather than a fix because the
// answers were measured against mattn/go-sqlite3 3.53.3 and already agreed:
// over w(k TEXT PRIMARY KEY, v) WITHOUT ROWID holding ('a','x'),('c','z'),
// "INSERT INTO w VALUES('a','y') ON CONFLICT(k) DO UPDATE SET k='b'" leaves
// ('b','x'),('c','z') with changes()=1, and the same clause with "SET k='c'"
// is "UNIQUE constraint failed: w.k" leaving both rows untouched.
func TestUpsertWithoutRowidReassignUnchanged(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO w VALUES('a','x'),('c','z')`,
	)
	_, cerr := db.compileWrite(`INSERT INTO w VALUES('a','y') ON CONFLICT(k) DO UPDATE SET k='b'`)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if err := db.Exec(`INSERT INTO w VALUES('a','y') ON CONFLICT(k) DO UPDATE SET k='b'`); err != nil {
		t.Fatalf("DO UPDATE SET k='b': %v", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT k,v FROM w ORDER BY k`)), []string{"b,x", "c,z"}; !equalStrSlices(got, want) {
		t.Fatalf("w = %v, want %v", got, want)
	}
	err := db.Exec(`INSERT INTO w VALUES('b','q') ON CONFLICT(k) DO UPDATE SET k='c'`)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: w.k") {
		t.Fatalf("DO UPDATE SET k='c' = %v, want a UNIQUE violation on w.k", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT k,v FROM w ORDER BY k`)), []string{"b,x", "c,z"}; !equalStrSlices(got, want) {
		t.Fatalf("w = %v, want %v (a failed upsert must not touch the table)", got, want)
	}
}

// TestUpsertRowidReassignAnswers runs the shapes, so a mutation that keeps the
// program's SHAPE but breaks its meaning still fails something here. Every
// expected value was measured against mattn/go-sqlite3 3.53.3 first.
func TestUpsertRowidReassignAnswers(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		wantE string   // substring of the expected error, "" for success
		want  []string // the table afterwards
	}{
		{"moves the row", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(5,'z')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=9`, "", []string{"5,z", "9,x"}},
		{"collides with an existing rowid", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(5,'z')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=5`, "UNIQUE constraint failed", []string{"1,x", "5,z"}},
		{"non-integer key", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a='abc'`, "datatype mismatch", []string{"1,x"}},
		{"NULL key", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=NULL`, "datatype mismatch", []string{"1,x"}},
		{"non-integral REAL key", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=7.5`, "datatype mismatch", []string{"1,x"}},
		{"integral REAL key", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=7.0`, "", []string{"7,x"}},
		{"numeric TEXT key", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a='8'`, "", []string{"8,x"}},
		// The CHECK constraint must see the NEW key -- emitCheckConstraints is
		// passed ipkReg == -1 for a DO UPDATE, so the coercion has to happen in
		// the row image's own slot rather than on a copy.
		{"CHECK over the new key", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,CHECK(a<>4))`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=4`, "CHECK constraint failed", []string{"1,x"}},
		// ...and so must a generated column, STORED (whose value really is in
		// the record) included.
		{"STORED generated over the new key", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,g TEXT AS (a*2) STORED)`, `INSERT INTO t(a,b) VALUES(1,'x')`},
			`INSERT INTO t(a,b) VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=6`, "", []string{"6,x,12"}},
		// ...and so must a CHECK naming the ROWID itself, which is a SEPARATE
		// register from the row image's IPK slot here and was reading the
		// PRE-MOVE value. sqlite3UpsertDoUpdate codes this branch as a plain
		// sqlite3Update (upsert.c:325-326), whose regNewData IS regNewRowid
		// (update.c:1031-1032); the CHECK block sets
		// "pParse->iSelfTab = -(regNewData+1)" (insert.c:2066) and expr.c's
		// "if( iCol<0 ){ return -1-pParse->iSelfTab; }" (expr.c:5063-5064)
		// resolves a bare rowid to exactly that register. Both directions are
		// measured against mattn/go-sqlite3 3.53.3, and BOTH were wrong before
		// -- the first stored (500,'x') silently, the second raised.
		{"CHECK over the rowid, moving out of range", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,CHECK(rowid < 100))`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=500`, "CHECK constraint failed: rowid < 100", []string{"1,x"}},
		{"CHECK over the rowid, moving onto the forbidden value", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,CHECK(rowid<>7))`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=7`, "CHECK constraint failed: rowid<>7", []string{"1,x"}},
		{"CHECK equating the IPK column and the rowid", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,CHECK(a=rowid))`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=500`, "", []string{"500,x"}},
		{"CHECK over a rowid ALIAS", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,CHECK(_rowid_ < 100))`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=500`, "CHECK constraint failed: _rowid_ < 100", []string{"1,x"}},
		// The non-moving DO UPDATE, where newRowidReg IS existRowidReg -- the
		// register split only happens for a chngRowid statement, mirroring
		// update.c:609/614-616's own "regOldRowid = regNewRowid" aliasing.
		{"CHECK over the rowid, not moving", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b,CHECK(rowid < 100))`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`, "", []string{"1,z"}},
		{"DO UPDATE WHERE false", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=9 WHERE b='no'`, "", []string{"1,x"}},
		{"no conflict at all -- the plain insert", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x')`},
			`INSERT INTO t VALUES(2,'y') ON CONFLICT(a) DO UPDATE SET a=9`, "", []string{"1,x", "2,y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newRVDTestDB(t)
			rvdExecAll(t, db, tc.setup...)
			_, cerr := db.compileWrite(tc.stmt)
			if cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			err := db.Exec(tc.stmt)
			switch {
			case tc.wantE == "" && err != nil:
				t.Fatalf("%q: %v", tc.stmt, err)
			case tc.wantE != "" && err == nil:
				t.Fatalf("%q: expected %q, got success", tc.stmt, tc.wantE)
			case tc.wantE != "" && !strings.Contains(err.Error(), tc.wantE):
				t.Fatalf("%q: error = %v, want it to contain %q", tc.stmt, err, tc.wantE)
			}
			cols := "a,b"
			if strings.Contains(tc.setup[0], "AS (a*2)") {
				cols = "a,b,g"
			}
			if got := rvdRowStrings(rvdQuery(t, db, `SELECT `+cols+` FROM t ORDER BY a`)); !equalStrSlices(got, tc.want) {
				t.Fatalf("%q: t = %v, want %v", tc.stmt, got, tc.want)
			}
		})
	}
}
