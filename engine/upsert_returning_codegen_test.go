// Tests that UPSERT ... RETURNING statements compile and produce correct results,
// including both INSERT and UPDATE arms, and various source types.
package engine

import (
	"strings"
	"testing"
)

// upsertReturningCase is one statement, its setup, and the rows RETURNING must
// produce (in order).
type upsertReturningCase struct {
	name  string
	setup []string
	stmt  string
	want  []string
}

var upsertReturningCases = []upsertReturningCase{
	// The two arms, which is the whole of the shape: a candidate that does NOT
	// conflict is inserted and reports itself; one that DOES conflict runs the
	// DO UPDATE and reports the row AFTER the SET list ran.
	{"insert arm", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`},
		`INSERT INTO t VALUES(2,'two') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
		[]string{"2|two"}},
	{"update arm reports the UPDATED row", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
		[]string{"1|upd"}},
	// The two outcomes that STORE nothing report nothing: both leave
	// sqlite3GenerateConstraintChecks through ignoreDest = endOfLoop
	// (insert.c:2361-2371 / :1569-1571), which is below the AFTER block that
	// codes the RETURNING.
	{"do nothing over a real conflict", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO NOTHING RETURNING a,b`,
		nil},
	{"do update whose own WHERE is false", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' WHERE b='nope' RETURNING a,b`,
		nil},
	{"returning star", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING *`,
		[]string{"1|upd"}},
	// The INTEGER PRIMARY KEY reads the ROWID register, and a SET that moves
	// the row reports the NEW one -- resolve.c:589-594 resolves the IPK
	// reference to regNewRowid, which update.c splits off regOldRowid exactly
	// when the SET assigns the key (update.c:609/614-616).
	{"do update that moves the rowid", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET a=99 RETURNING a, rowid, b`,
		[]string{"99|99|one"}},
	// One statement, both arms, in source order.
	{"multi-tuple VALUES takes both arms", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `INSERT INTO t VALUES(1,22,33)`},
		`INSERT INTO t(a,b,c) VALUES(1,'x','y'),(2,'p','q') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING *`,
		[]string{"1|upd|33", "2|p|q"}},
	// A SELECT row source reaches the SAME tail, so it inherits the block; the
	// C draws no distinction either (the AFTER call at insert.c:1604-1608 sits
	// inside the one insertion loop). The route this promotion replaced DECLINED
	// the crossing explicitly -- "an UPSERT over an INSERT ... SELECT source is
	// not supported by this write path" -- so it is a shape that used to ERROR
	// where the oracle answers.
	{"SELECT row source", []string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE s(a,b)`,
		`INSERT INTO t VALUES(1,'one')`, `INSERT INTO s VALUES(1,'sx'),(2,'sy')`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b RETURNING a,b`,
		[]string{"1|sx", "2|sy"}},
	// "excluded" is the candidate; the unqualified name is the stored row.
	{"excluded and the target in one SET list", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `INSERT INTO t VALUES(1,'one',10)`},
		`INSERT INTO t VALUES(1,'X',99) ON CONFLICT(a) DO UPDATE SET b=excluded.b, c=c+1 RETURNING a,b,c`,
		[]string{"1|X|11"}},
	// A generated column is re-derived for the RETURNING image, not read out of
	// the record (rederiveGeneratedFromRowid).
	{"generated column", []string{`CREATE TABLE t(a UNIQUE, b, g AS (b*2))`, `INSERT INTO t(a,b) VALUES(1,10)`},
		`INSERT INTO t(a,b) VALUES(1,50) ON CONFLICT(a) DO UPDATE SET b=99 RETURNING *`,
		[]string{"1|99|198"}},
	// A WITHOUT ROWID target: no rowid register to read, so this is the case
	// that would break if the IPK remap were applied unconditionally.
	{"without rowid", []string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `INSERT INTO t VALUES('a',1)`},
		`INSERT INTO t VALUES('a',2) ON CONFLICT(k) DO UPDATE SET v=v+10 RETURNING k,v`,
		[]string{"a|11"}},
	// An expression list, not just columns.
	{"expressions", []string{`CREATE TABLE t(a UNIQUE,b)`, `INSERT INTO t VALUES(1,'one')`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a+1, upper(b), b||'!'`,
		[]string{"2|UPD|upd!"}},
}

// TestUpsertReturningCompilesAndAnswers is the pair of assertions this
// promotion stands on: every case COMPILES, and answers what the 3.53.3 oracle
// answers.
func TestUpsertReturningCompilesAndAnswers(t *testing.T) {
	for _, tc := range upsertReturningCases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			prog, cerr := db.compileWrite(tc.stmt)
			if cerr != nil {
				t.Fatalf("compile %q: %v", tc.stmt, cerr)
			}
			if len(prog.ColNames) == 0 {
				t.Errorf("%q compiled with NO ColNames: the RETURNING output names are "+
					"emitUpsertTail's return value and have to reach the Program, or the "+
					"result set has no column names at all", tc.stmt)
			}
			got := execReturningRows(t, db, tc.stmt)
			if strings.Join(got, ";") != strings.Join(tc.want, ";") {
				t.Fatalf("%q: got %v, want %v", tc.stmt, got, tc.want)
			}
		})
	}
}

// TestUpsertReturningFiresTriggersAroundIt pins the ORDER the two emissions sit
// in, which is the half a row-value assertion cannot see: RETURNING is coded
// FIRST among the AFTER programs, because sqlite3TriggerList PREPENDS the
// synthetic trigger (trigger.c:68-78) and sqlite3CodeRowTrigger walks that list
// in order (trigger.c:1484). A RAISE(IGNORE) in a user AFTER program therefore
// lands PAST a row that has already been captured -- so with the emission moved
// below the fire, these answer NOTHING.
//
// Both spellings are here because an upsert has two arms and each has its own
// AFTER program: AFTER INSERT on the plain-insert arm, AFTER UPDATE on the DO
// UPDATE arm.
func TestUpsertReturningFiresTriggersAroundIt(t *testing.T) {
	for _, tc := range []upsertReturningCase{
		{"after UPDATE RAISE(IGNORE) does not swallow the row",
			[]string{`CREATE TABLE t(a UNIQUE,b)`, `INSERT INTO t VALUES(1,'one')`,
				`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN SELECT RAISE(IGNORE); END`},
			`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
			[]string{"1|upd"}},
		{"after INSERT RAISE(IGNORE) does not swallow the row",
			[]string{`CREATE TABLE t(a UNIQUE,b)`, `INSERT INTO t VALUES(1,'one')`,
				`CREATE TRIGGER ti AFTER INSERT ON t BEGIN SELECT RAISE(IGNORE); END`},
			`INSERT INTO t VALUES(2,'two') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
			[]string{"2|two"}},
		// The BEFORE UPDATE program can have edited the row since OpUpsertFind
		// read it, and C reloads every unassigned column into regNew before the
		// store (update.c:1002-1017) -- which is the block RETURNING reads.
		// opUpsertStore's own merge does that for the RECORD; updatePlan.rowRegs
		// is the register half. Without it this answers 1|upd|c1 where the
		// oracle answers 1|upd|trig.
		{"BEFORE UPDATE trigger's edit reaches the RETURNING row",
			[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `INSERT INTO t VALUES(1,'b1','c1')`,
				`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN UPDATE t SET c='trig' WHERE a=old.a; END`},
			`INSERT INTO t(a,b) VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b,c`,
			[]string{"1|upd|trig"}},
		// A BEFORE UPDATE program that DELETES the row: the store is skipped
		// (OpSkipIfRowGone) and so is the RETURNING row, which is update.c's own
		// labelContinue at :990-999.
		{"BEFORE UPDATE trigger deletes the row",
			[]string{`CREATE TABLE t(a UNIQUE,b)`, `INSERT INTO t VALUES(1,'one'),(2,'two')`,
				`CREATE TRIGGER tbd BEFORE UPDATE ON t BEGIN DELETE FROM t WHERE a=old.a; END`},
			`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='upd' RETURNING a,b`,
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile %q: %v", tc.stmt, cerr)
			}
			got := execReturningRows(t, db, tc.stmt)
			if strings.Join(got, ";") != strings.Join(tc.want, ";") {
				t.Fatalf("%q: got %v, want %v", tc.stmt, got, tc.want)
			}
		})
	}
}

// TestUpsertReturningBoundParameters covers the axis the differential harness
// is structurally blind to: run() takes a []string and binds nothing, so a
// RETURNING column carrying a "?" passes every gate in compat-harness whatever
// it answers (the lesson vtab_returning_param_test.go was written from). Both
// arms are exercised, with a parameter in the VALUES tuple, in the DO UPDATE
// SET list and in the RETURNING list at once.
//
// The oracle side is compat-harness/upsert_returning_param_test.go, which runs
// this same statement through BOTH drivers directly (sql.Open("sqlite3", ...)
// beside sql.Open("sqlite", ...)) because that is the only way to bind anything
// in that directory.
func TestUpsertReturningBoundParameters(t *testing.T) {
	const stmt = `INSERT INTO t VALUES(?1,'ins') ON CONFLICT(a) DO UPDATE SET b=?2 RETURNING a, b, ?3`
	for _, tc := range []struct {
		name string
		key  int64
		want string
	}{
		{"update arm", 1, "1|setval|tag"},
		{"insert arm", 2, "2|ins|tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			args := []Value{{Typ: Int, I: tc.key}, {Typ: Text, S: []byte("setval")}, {Typ: Text, S: []byte("tag")}}
			_, rows, xerr := db.ExecReturningArgs(stmt, args)
			if xerr != nil {
				t.Fatalf("exec: %v", xerr)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1", len(rows))
			}
			parts := make([]string, len(rows[0]))
			for i, v := range rows[0] {
				parts[i] = valueDebugString(v)
			}
			if got := strings.Join(parts, "|"); got != tc.want {
				t.Fatalf("got %q, want %q -- a bound parameter in this block is invisible "+
					"to the differential harness, which binds nothing", got, tc.want)
			}
		})
	}
}

// TestUpsertReturningBadColumnStaysAnError pins the half of this promotion that
// must NOT become an answer. C SQLite resolves a RETURNING reference only
// against the modified table itself -- resolve.c:531-533 requires the qualifier
// to be pParse->pTriggerTab->zName -- so a foreign qualifier is a PREPARE-time
// "no such column: zzz.a" there, and an unrecognised one must stay an error
// here whichever route serves it. "excluded" is one of those: codeReturningTrigger
// sets NC_UBaseReg and NOT NC_UUpsert (trigger.c:1075), so the pseudo-table the
// SET list can name is invisible to the RETURNING list. So is the target's own
// ALIAS, for the same reason -- the test is the TABLE's name.
func TestUpsertReturningBadColumnStaysAnError(t *testing.T) {
	for _, stmt := range []string{
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='u' RETURNING zzz.a`,
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='u' RETURNING excluded.b`,
		`INSERT INTO t AS base VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='u' RETURNING base.a`,
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='u' RETURNING nosuchcol`,
	} {
		db, err := Create(t.TempDir()+"/x.musq")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		for _, s := range []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'one')`} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		if _, _, xerr := db.ExecReturningArgs(stmt, nil); xerr == nil {
			t.Errorf("%q was ANSWERED; the 3.53.3 oracle raises \"no such column\" at prepare time", stmt)
		}
		db.Discard()
	}
}

