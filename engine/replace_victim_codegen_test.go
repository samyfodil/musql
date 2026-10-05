package engine

// Tests for REPLACE with DELETE triggers under recursive_triggers=ON: verifies
// that conflict-resolving INSERT triggers DELETE programs and runs them under
// OE_Replace with compiled trigger bodies.

import (
	"strings"
	"testing"
)

// replaceVictimShapes: conflict-resolving INSERTs whose REPLACE fires the
// target's own DELETE triggers. Every setup turns the pragma ON first.
var replaceVictimShapes = []conflictShapeCase{
	// The census shape, verbatim from vdbe_total_test.go's list.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`},
	// A rowid conflict rather than a UNIQUE-index one (insert.c's OTHER
	// OE_Replace arm, insert.c:2339-2340).
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`},
	// BEFORE, and BEFORE+AFTER together.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`},
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b'||old.a); END`,
		`CREATE TRIGGER ta AFTER DELETE ON t BEGIN INSERT INTO log VALUES('a'||old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`},
	// A WHEN guard on the DELETE trigger.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t WHEN old.a>5 BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`},
	// A DECLARED per-constraint default rather than an explicit OR-clause.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.b); END`},
		`INSERT INTO t VALUES(1,'x')`},
	// A SELECT source (compileInsertSelectWrite's own copy of the block).
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT OR REPLACE INTO t SELECT a,b FROM s`},
	// A WITHOUT ROWID target.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(k TEXT PRIMARY KEY,v) WITHOUT ROWID`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.k); END`},
		`INSERT OR REPLACE INTO t VALUES('a','x')`},
	// The victim's trigger body writing the TARGET table -- a cascade back
	// into the statement's own table, which is what triggerPrgMemo bounds.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO t VALUES(old.a,'r'); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`},
	// RETURNING, which the route this replaced refused outright.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x') RETURNING a,b`},
	// A PLAIN insert on such a table -- no OR-clause at all. It could never
	// REPLACE, but the old decline refused it anyway, so it is here to pin
	// that the widening is deliberate.
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT INTO t VALUES(1,'x')`},
}

func TestReplaceVictimShapesCompileToBytecode(t *testing.T) {
	for _, tc := range replaceVictimShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: %q should COMPILE, got error: %v", tc.stmt, err)
			continue
		}
	}
}

// insertPlansOf collects every insertPlan a program's instructions carry.
func insertPlansOf(prog *Program) []*insertPlan {
	var out []*insertPlan
	for i := range prog.Insns {
		if p, ok := prog.Insns[i].P4.(*insertPlan); ok {
			out = append(out, p)
		}
	}
	return out
}

// TestReplaceVictimPlansAreCompiled is the non-vacuity assertion: a compiled
// program with NO victim-delete plans on it silently skips the triggers, and
// the compile test above cannot tell the difference.
func TestReplaceVictimPlansAreCompiled(t *testing.T) {
	prog, err := compileShape(t, conflictShapeCase{
		[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tb BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b'||old.a); END`,
			`CREATE TRIGGER ta AFTER DELETE ON t BEGIN INSERT INTO log VALUES('a'||old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	plans := insertPlansOf(prog)
	if len(plans) == 0 {
		t.Fatalf("no insertPlan on the program at all")
	}
	for _, p := range plans {
		if p.replaceDelBefore == nil {
			t.Errorf("insertPlan.replaceDelBefore is nil: the victim's BEFORE DELETE trigger would be SKIPPED, not fired")
		}
		if p.replaceDelAfter == nil {
			t.Errorf("insertPlan.replaceDelAfter is nil: the victim's AFTER DELETE trigger would be SKIPPED, not fired")
		}
		for _, fp := range []*triggerFirePlan{p.replaceDelBefore, p.replaceDelAfter} {
			if fp == nil {
				continue
			}
			if len(fp.triggers) == 0 {
				t.Errorf("victim-delete fire plan has no triggers")
			}
			for _, ct := range fp.triggers {
				if len(ct.body) == 0 {
					t.Errorf("victim-delete trigger %q compiled to an EMPTY body", ct.tr.name)
				}
			}
		}
	}
}

// TestSubProgramsOfWalksVictimDeletePlans asserts the GATE's own reach rather
// than the compiler's, and it exists because a mutation test found nothing
// else that could: deleting subProgramsOf's *insertPlan case leaves every
// other assertion in this file, and TestPromotedWriteShapesAllCompile itself,
// passing.
//
// That is not because the case is unnecessary. It is because nothing can
// currently produce a victim-delete body that declines in place: a body
// compileTriggerBodyStmt declines makes compileReplaceVictimDeletePlans return
// an error, so the WHOLE statement declines and never reaches the walk at all.
// The case is what keeps the walk honest if a future change ever does let a
// sub-program decline in place -- the exact hole batch J's review found when
// the walk scanned prog.Insns and nothing else, and certified a nested-trigger
// promotion whose trigger bodies it could not see.
//
// What this test owns is the REACH itself, which is what the hole was really
// about. subProgramsOf must descend into an insertPlan's victim-delete trigger
// plans, or anything that walks a program graph -- a future gate, a lowering
// backend, an optimiser -- silently misses every trigger body attached there.
func TestSubProgramsOfWalksVictimDeletePlans(t *testing.T) {
	body := &Program{Insns: []Instruction{{Op: OpHalt}}}
	for _, tc := range []struct {
		name string
		plan *insertPlan
	}{
		{"BEFORE", &insertPlan{replaceDelBefore: &triggerFirePlan{triggers: []*compiledTrigger{{body: []*Program{body}}}}}},
		{"AFTER", &insertPlan{replaceDelAfter: &triggerFirePlan{triggers: []*compiledTrigger{{body: []*Program{body}}}}}},
		{"WHEN guard", &insertPlan{replaceDelAfter: &triggerFirePlan{triggers: []*compiledTrigger{{when: body}}}}},
	} {
		var found bool
		for _, sub := range subProgramsOf(tc.plan) {
			if sub == body {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: subProgramsOf did not descend into insertPlan's victim-delete trigger plans -- "+
				"a graph walk over this program would silently miss every trigger body hanging there", tc.name)
		}
	}
}

// TestReplaceVictimPlansAbsentWithoutThePragma is the same assertion from the
// other side: with recursive_triggers OFF the C leaves pTrigger zero
// (insert.c:2214-2218), so the plans must be nil and the victim must be
// dropped silently. A compile that attached them anyway would fire triggers
// C SQLite does not.
func TestReplaceVictimPlansAbsentWithoutThePragma(t *testing.T) {
	prog, err := compileShape(t, conflictShapeCase{
		[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'x')`,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, p := range insertPlansOf(prog) {
		if p.replaceDelBefore != nil || p.replaceDelAfter != nil {
			t.Errorf("victim-delete plans attached with recursive_triggers OFF: the triggers would fire where C SQLite fires nothing")
		}
	}
}

// TestReplaceVictimBodyCompilesUnderReplace pins the orconf half. The victim's
// trigger program is coded under the LITERAL OE_Replace (insert.c:2339-2340 /
// :2612-2614), which trigger.c:1137 then installs over each body statement's
// OWN clause -- so a body written "INSERT OR IGNORE INTO other ..." must
// compile as a REPLACE.
//
// Asserted on the compiled PLAN rather than only on the answer, because the
// answer alone is also produced by the run-time fire-state half: if the
// compile-time half were dropped and only that kept, a body statement whose
// target has no triggers would still be running its own baked-in ABORT.
//
// The rule itself was verified against mattn/go-sqlite3 3.53.3, over
// other(x UNIQUE,y) already holding (1,'orig') and t(a INTEGER PRIMARY KEY,b)
// holding (1,'one') with an AFTER DELETE trigger whose body is
// "INSERT OR IGNORE INTO other VALUES(1,'from-trigger')":
// "INSERT OR REPLACE INTO t VALUES(1,'two')" leaves other holding
// (1,'from-trigger') -- the REPLACE really does override the body's own
// IGNORE, and does not merely fill in for a missing clause.
func TestReplaceVictimBodyCompilesUnderReplace(t *testing.T) {
	db := newRVDTestDB(t)
	rvdExecAll(t, db,
		`PRAGMA recursive_triggers=ON`,
		`CREATE TABLE other(x UNIQUE, y)`,
		`INSERT INTO other VALUES(1,'orig')`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT OR IGNORE INTO other VALUES(1,'from-trigger'); END`,
	)
	prog, cerr := db.compileWrite(`INSERT OR REPLACE INTO t VALUES(1,'two')`)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	found := false
	for _, p := range insertPlansOf(prog) {
		if p.replaceDelAfter == nil {
			continue
		}
		for _, ct := range p.replaceDelAfter.triggers {
			for _, body := range ct.body {
				for i := range body.Insns {
					bp, ok := body.Insns[i].P4.(*insertPlan)
					if !ok {
						continue
					}
					found = true
					if !bp.explicitOr || bp.action != conflictReplace {
						t.Errorf("victim-delete body statement compiled with action=%v explicitOr=%v, want REPLACE/true -- trigger.c:1137 overrides the body's own OR IGNORE",
							bp.action, bp.explicitOr)
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("no body insertPlan found on the victim's AFTER DELETE program")
	}
	if err := db.Exec(`INSERT OR REPLACE INTO t VALUES(1,'two')`); err != nil {
		t.Fatalf("INSERT OR REPLACE: %v", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT x,y FROM other`)), []string{"1,from-trigger"}; !equalStrSlices(got, want) {
		t.Fatalf("other = %v, want %v", got, want)
	}
}

// TestReplaceVictimCompiledAnswers runs the promoted shapes end to end. Every
// expected value was measured against mattn/go-sqlite3 3.53.3 before it was
// written here, and each case also asserts the statement really COMPILED --
// otherwise a decline could make the whole table pass on answers produced some
// other way.
func TestReplaceVictimCompiledAnswers(t *testing.T) {
	cases := []struct {
		name    string
		setup   []string
		stmt    string
		wantE   string
		wantTbl []string
		wantLog []string
	}{
		{"AFTER on a rowid conflict",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('a-'||old.a||'-'||old.b); END`,
				`INSERT INTO t VALUES(1,'one')`},
			`INSERT OR REPLACE INTO t VALUES(1,'two')`, "", []string{"1,two"}, []string{"a-1-one"}},
		{"BEFORE on a UNIQUE-index conflict",
			[]string{`CREATE TABLE t(a,b UNIQUE)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b2-'||old.a); END`,
				`INSERT INTO t VALUES('x',1)`},
			`INSERT OR REPLACE INTO t VALUES('y',1)`, "", []string{"y,1"}, []string{"b2-x"}},
		// TWO victims, one per UNIQUE constraint. The oracle logs them in
		// db.indexes' own registration order with the SECOND constraint's
		// victim first -- measured, not guessed.
		{"two victims",
			[]string{`CREATE TABLE t(a UNIQUE,b UNIQUE)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('c3-'||old.a||'-'||old.b); END`,
				`INSERT INTO t VALUES(1,10)`, `INSERT INTO t VALUES(2,20)`},
			`INSERT OR REPLACE INTO t VALUES(1,20)`, "", []string{"1,20"}, []string{"c3-2-20", "c3-1-10"}},
		// A BEFORE RAISE(IGNORE) makes the victim SURVIVE, so the candidate's
		// own conflict is still there and the statement fails.
		{"BEFORE RAISE(IGNORE) keeps the victim",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td BEFORE DELETE ON t BEGIN SELECT RAISE(IGNORE); END`,
				`INSERT INTO t VALUES(1,'one')`},
			`INSERT OR REPLACE INTO t VALUES(1,'two')`, "UNIQUE constraint failed: t.a", []string{"1,one"}, nil},
		{"BEFORE RAISE(ABORT)",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td BEFORE DELETE ON t BEGIN SELECT RAISE(ABORT,'no delete'); END`,
				`INSERT INTO t VALUES(1,'one')`},
			`INSERT OR REPLACE INTO t VALUES(1,'two')`, "no delete", []string{"1,one"}, nil},
		// insert.c's post-cascade RE-CHECK: the AFTER program re-creates a row
		// under the very key the candidate wants, and the retest aborts.
		{"the re-check",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO t VALUES(old.a,'resurrected'); END`,
				`INSERT INTO t VALUES(1,'one')`},
			`INSERT OR REPLACE INTO t VALUES(1,'two')`, "UNIQUE constraint failed: t.a", []string{"1,one"}, nil},
		// A later row's NOT NULL failure unwinds the whole statement, the
		// victim delete and the trigger's own write with it.
		{"statement unwind",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b NOT NULL)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('tf-'||old.a); END`,
				`INSERT INTO t VALUES(1,'one'),(2,'two')`},
			`INSERT OR REPLACE INTO t VALUES(1,'X'),(2,NULL)`, "NOT NULL constraint failed", []string{"1,one", "2,two"}, nil},
		{"WHEN guard, false",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t WHEN old.a>5 BEGIN INSERT INTO log VALUES('tg-'||old.a); END`,
				`INSERT INTO t VALUES(1,'one')`},
			`INSERT OR REPLACE INTO t VALUES(1,'X')`, "", []string{"1,X"}, nil},
		{"WHEN guard, true",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t WHEN old.a>5 BEGIN INSERT INTO log VALUES('tg-'||old.a); END`,
				`INSERT INTO t VALUES(9,'nine')`},
			`INSERT OR REPLACE INTO t VALUES(9,'Y')`, "", []string{"9,Y"}, []string{"tg-9"}},
		// No explicit OR-clause at all: the REPLACE comes from the column's
		// own declared default, which effectiveHitAction resolves. Oracle:
		// ta holds (1,'two') and the log holds ta-one.
		{"declared ON CONFLICT REPLACE default",
			[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('ta-'||old.b); END`,
				`INSERT INTO t VALUES(1,'one')`},
			`INSERT INTO t VALUES(1,'two')`, "", []string{"1,two"}, []string{"ta-one"}},
		{"multi-row, one victim per conflicting tuple",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('tb-'||old.a); END`,
				`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`},
			`INSERT OR REPLACE INTO t VALUES(1,'X'),(4,'Y'),(2,'Z')`, "",
			[]string{"1,X", "2,Z", "3,three", "4,Y"}, []string{"tb-1", "tb-2"}},
		{"SELECT source",
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE src(a,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('tc-'||old.a); END`,
				`INSERT INTO src VALUES(1,'s1'),(5,'s5')`, `INSERT INTO t VALUES(1,'one')`},
			`INSERT OR REPLACE INTO t SELECT a,b FROM src`, "", []string{"1,s1", "5,s5"}, []string{"tc-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newRVDTestDB(t)
			rvdExecAll(t, db, append([]string{`PRAGMA recursive_triggers=ON`}, tc.setup...)...)
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
			if got := rvdRowStrings(rvdQuery(t, db, `SELECT * FROM t ORDER BY 1`)); !equalStrSlices(got, tc.wantTbl) {
				t.Fatalf("%q: t = %v, want %v", tc.stmt, got, tc.wantTbl)
			}
			got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log ORDER BY rowid`))
			if len(got) == 0 && len(tc.wantLog) == 0 {
				return
			}
			if !equalStrSlices(got, tc.wantLog) {
				t.Fatalf("%q: log = %v, want %v", tc.stmt, got, tc.wantLog)
			}
		})
	}
}

// TestRecursiveTriggersFlipDropsTheWritePlan pins the cache hazard the
// compile-time decision created, and the older one it uncovered. "PRAGMA
// recursive_triggers" is read at COMPILE time now
// (compileReplaceVictimDeletePlans), while the write-plan cache is keyed on
// statement TEXT plus schemaGen/txGen -- so without SetRecursiveTriggers'
// db.writePlans drop, the second of two byte-identical statements replays the
// plan compiled under the OLD flag.
//
// Measured against mattn/go-sqlite3 3.53.3: the oracle logs exactly ONE 'd-1'
// either way round. Before the drop this engine produced ZERO for off->on (an
// OLDER defect: a plan compiled with the flag OFF was cached even while the
// shape still declined, and turning the flag ON afterwards could not reach it)
// and TWO for on->off.
func TestRecursiveTriggersFlipDropsTheWritePlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
	}{{"off then on", "0"}, {"on then off", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			second := "1"
			if tc.first == "1" {
				second = "0"
			}
			db := newRVDTestDB(t)
			rvdExecAll(t, db,
				`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
				`CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('d-'||old.a); END`,
				`PRAGMA recursive_triggers=`+tc.first,
				`INSERT INTO t VALUES(1,'one')`,
				`INSERT OR REPLACE INTO t VALUES(1,'two')`,
				`PRAGMA recursive_triggers=`+second,
				`INSERT OR REPLACE INTO t VALUES(1,'two')`, // byte-identical text
			)
			if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log ORDER BY rowid`)), []string{"d-1"}; !equalStrSlices(got, want) {
				t.Fatalf("log = %v, want %v -- the write-plan cache replayed a plan compiled under the other flag", got, want)
			}
		})
	}
}

// TestReplaceVictimPlansOnlyWhereReplaceCanResolve is the OTHER non-vacuity
// half, and the one this batch's first round got wrong: the plans must be
// absent -- and the victim's trigger body therefore never COMPILED -- for a
// statement no constraint of which can resolve to OE_Replace.
//
// It matters because compiling is not free of consequence. A body that will not
// compile raises a HARD error (a missing table, an arity mismatch), not an
// errVDBEUnsupported decline, so an eager compile makes every such error the
// enclosing INSERT's own. Round 1 did exactly that, and eight spellings of a
// plain INSERT that the 3.53.3 oracle runs happily became errors.
//
// The C gate is replaceResolutionCoded's (vdbe_write.go): sqlite3GenerateRowDelete
// -- the call that codes the body -- lives only inside a "case OE_Replace:" arm
// of sqlite3GenerateConstraintChecks' onError switch (insert.c:2314/2339-2340
// for the rowid arm, insert.c:2601's "assert( onError==OE_Replace );" for the
// index one), and every onError there is resolved at CODEGEN time.
//
// wantPlans is measured, not derived: an INSERT whose body the oracle compiled
// is one it REFUSES TO PREPARE when that body is broken, and the refusal
// arrives with no conflicting row in the table for a REPLACE to ever reach.
// Each case below was run against mattn/go-sqlite3 3.53.3 with
// "CREATE TABLE u(x,y)" and "AFTER DELETE ON t BEGIN INSERT INTO u
// VALUES(1,2,3); END" before it was written here.
func TestReplaceVictimPlansOnlyWhereReplaceCanResolve(t *testing.T) {
	const ipk = `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`
	for _, tc := range []struct {
		name      string
		schema    []string
		stmt      string
		wantPlans bool
	}{
		// No OR-clause and no declared clause: every constraint resolves to
		// OE_Abort (insert.c:2248 / :2474), so no arm is coded.
		{"plain INSERT", []string{ipk}, `INSERT INTO t VALUES(1,'x')`, false},
		{"INSERT OR IGNORE", []string{ipk}, `INSERT OR IGNORE INTO t VALUES(1,'x')`, false},
		{"INSERT OR ABORT", []string{ipk}, `INSERT OR ABORT INTO t VALUES(1,'x')`, false},
		{"INSERT OR FAIL", []string{ipk}, `INSERT OR FAIL INTO t VALUES(1,'x')`, false},
		{"INSERT OR ROLLBACK", []string{ipk}, `INSERT OR ROLLBACK INTO t VALUES(1,'x')`, false},
		{"plain INSERT vs a UNIQUE index", []string{`CREATE TABLE t(a,b UNIQUE)`}, `INSERT INTO t VALUES(1,'x')`, false},

		// overrideError!=OE_Default wins uniformly over every constraint
		// (insert.c:2246-2247 / :2471-2472).
		{"INSERT OR REPLACE", []string{ipk}, `INSERT OR REPLACE INTO t VALUES(1,'x')`, true},
		{"INSERT OR REPLACE vs a UNIQUE index", []string{`CREATE TABLE t(a,b UNIQUE)`}, `INSERT OR REPLACE INTO t VALUES(1,'x')`, true},
		{"INSERT OR REPLACE ... SELECT", []string{ipk, `CREATE TABLE src(p,q)`}, `INSERT OR REPLACE INTO t SELECT p,q FROM src`, true},

		// pkChng: insert.c:2241's "if( pkChng && pPk==0 )", with pkChng passed
		// as "ipkColumn>=0" (insert.c:1570). With the rowid left to be assigned
		// and no other constraint, NO arm is coded at all -- and the oracle
		// prepares "INSERT OR REPLACE INTO t(b) VALUES('x')" happily.
		{"INSERT OR REPLACE without the rowid", []string{ipk}, `INSERT OR REPLACE INTO t(b) VALUES('x')`, false},
		{"INSERT OR REPLACE ... SELECT without the rowid", []string{ipk, `CREATE TABLE src(p,q)`}, `INSERT OR REPLACE INTO t(b) SELECT q FROM src`, false},
		{"INSERT OR REPLACE DEFAULT VALUES", []string{ipk}, `INSERT OR REPLACE INTO t DEFAULT VALUES`, false},
		// ...but a UNIQUE index of its own puts the arm back (insert.c:2466).
		{"INSERT OR REPLACE without the rowid, but a UNIQUE column", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b UNIQUE)`}, `INSERT OR REPLACE INTO t(b) VALUES('x')`, true},
		// A table with no rowid alias and no UNIQUE constraint has nothing to
		// resolve at all.
		{"INSERT OR REPLACE with no constraint anywhere", []string{`CREATE TABLE t(a,b)`}, `INSERT OR REPLACE INTO t VALUES(1,'x')`, false},

		// The constraint's own declared clause governs when the statement gave
		// none: "onError = pTab->keyConf;" (insert.c:2245) and
		// "onError = pIdx->onError;" (insert.c:2466).
		{"declared REPLACE on the IPK", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE,b)`}, `INSERT INTO t VALUES(1,'x')`, true},
		{"declared REPLACE on a UNIQUE column", []string{`CREATE TABLE t(a,b UNIQUE ON CONFLICT REPLACE)`}, `INSERT INTO t VALUES('p',1)`, true},
		{"declared ABORT on the IPK", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT ABORT,b)`}, `INSERT INTO t VALUES(1,'x')`, false},
		{"declared REPLACE overridden by OR ABORT", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE,b)`}, `INSERT OR ABORT INTO t VALUES(1,'x')`, false},
		{"declared REPLACE on the IPK, rowid not supplied", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE,b)`}, `INSERT INTO t(b) VALUES('x')`, false},

		// An ON CONFLICT clause turns the constraint it covers into
		// OE_Ignore/OE_Update, never OE_Replace, and it is applied AFTER
		// overrideError (insert.c:2253-2260 / :2477-2484) -- so even
		// "INSERT OR REPLACE ... ON CONFLICT DO NOTHING" codes no arm.
		{"upsert DO NOTHING", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE,b)`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO NOTHING`, false},
		{"INSERT OR REPLACE with an upsert clause", []string{ipk}, `INSERT OR REPLACE INTO t VALUES(1,'x') ON CONFLICT(a) DO NOTHING`, false},
		{"upsert DO UPDATE", []string{ipk}, `INSERT OR REPLACE INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET b='z'`, false},

		// A WITHOUT ROWID table takes insert.c:2241's pPk!=0 exit, so the
		// rowid arm is never emitted for it and its PRIMARY KEY is resolved by
		// the index loop instead. The plans must still be there.
		{"WITHOUT ROWID under OR REPLACE", []string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`}, `INSERT OR REPLACE INTO t VALUES('a','x')`, true},
		{"WITHOUT ROWID, declared REPLACE on the PK", []string{`CREATE TABLE t(k TEXT PRIMARY KEY ON CONFLICT REPLACE, v) WITHOUT ROWID`}, `INSERT INTO t VALUES('a','x')`, true},
		{"WITHOUT ROWID, plain INSERT", []string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`}, `INSERT INTO t VALUES('a','x')`, false},

		// insert.c:1096's other ipkColumn source: a table with NO INTEGER
		// PRIMARY KEY whose column list names the rowid outright.
		{"explicit rowid slot on a table with no IPK", []string{`CREATE TABLE t(a,b)`}, `INSERT OR REPLACE INTO t(rowid,a) VALUES(1,'x')`, true},
		{"explicit rowid slot, no OR clause", []string{`CREATE TABLE t(a,b)`}, `INSERT INTO t(rowid,a) VALUES(1,'x')`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := append([]string{`PRAGMA recursive_triggers=ON`}, tc.schema...)
			setup = append(setup, `CREATE TABLE log(x)`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(1); END`)
			prog, err := compileShape(t, conflictShapeCase{setup, tc.stmt})
			if err != nil {
				t.Fatalf("compile %q: %v", tc.stmt, err)
			}
			plans := insertPlansOf(prog)
			if len(plans) == 0 {
				t.Fatalf("no insertPlan on the program at all")
			}
			for _, p := range plans {
				if got := p.replaceDelBefore != nil || p.replaceDelAfter != nil; got != tc.wantPlans {
					t.Errorf("%q: victim-delete plans present = %v, want %v", tc.stmt, got, tc.wantPlans)
				}
			}
		})
	}
}

// TestReplaceVictimBodyErrorsOnlyReachAReplacingInsert is the same rule stated
// as the SYMPTOM -- what a bug report shows, and what a mutation to
// replaceResolutionCoded has to break. The trigger body here cannot compile at
// all (five separate HARD errors, none of them errVDBEUnsupported), and both
// expectations were measured against mattn/go-sqlite3 3.53.3 first: the plain
// INSERT succeeds, the INSERT OR REPLACE is refused at PREPARE with no
// conflicting row present.
func TestReplaceVictimBodyErrorsOnlyReachAReplacingInsert(t *testing.T) {
	for _, body := range []struct {
		name  string
		setup []string
		trig  string
	}{
		{"arity mismatch", []string{`CREATE TABLE u(x,y)`}, `INSERT INTO u VALUES(1,2,3)`},
		{"no such table", nil, `INSERT INTO nosuch VALUES(1)`},
		{"no such column", []string{`CREATE TABLE u(x,y)`}, `INSERT INTO u(nope) VALUES(1)`},
		{"UPDATE of a missing table", nil, `UPDATE nosuch SET x=1`},
		{"body SELECT over a missing table", []string{`CREATE TABLE u(x)`}, `INSERT INTO u SELECT z FROM nosuch`},
		// A MAIN-schema trigger body cannot see a TEMP table. Measured against
		// 3.53.3, message for message: the plain INSERT succeeds, the
		// INSERT OR REPLACE raises "no such table: main.tmp".
		{"a TEMP table from a MAIN trigger body", []string{`CREATE TEMP TABLE tmp(x)`}, `INSERT INTO tmp VALUES(1)`},
	} {
		t.Run(body.name, func(t *testing.T) {
			setup := append([]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`}, body.setup...)
			setup = append(setup, `CREATE TRIGGER td AFTER DELETE ON t BEGIN `+body.trig+`; END`)

			db := newRVDTestDB(t)
			rvdExecAll(t, db, setup...)
			// Non-vacuity: the plain INSERT must SUCCEED as real bytecode.
			_, cerr := db.compileWrite(`INSERT INTO t VALUES(1,'x')`)
			if cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			if err := db.Exec(`INSERT INTO t VALUES(1,'x')`); err != nil {
				t.Fatalf("a plain INSERT must not inherit the victim-delete body's error: %v", err)
			}
			if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT a,b FROM t`)), []string{"1,x"}; !equalStrSlices(got, want) {
				t.Fatalf("t = %v, want %v", got, want)
			}

			db2 := newRVDTestDB(t)
			rvdExecAll(t, db2, setup...)
			if err := db2.Exec(`INSERT OR REPLACE INTO t VALUES(1,'x')`); err == nil {
				t.Fatalf("INSERT OR REPLACE must raise the victim-delete body's own error; the oracle refuses to prepare it")
			}
			if got := rvdRowStrings(rvdQuery(t, db2, `SELECT a,b FROM t`)); len(got) != 0 {
				t.Fatalf("t = %v, want empty", got)
			}
		})
	}
}

// TestReplaceVictimNestedBodyUnderImposedReplace is the recursive case, and the
// one a "skip the compile for a plain INSERT" shortcut gets wrong: the outer
// statement IS an INSERT OR REPLACE, so t's victim-delete body is coded -- and
// trigger.c:1137 ("pParse->eOrconf = (orconf==OE_Default)?pStep->orconf:(u8)orconf;")
// imposes OE_Replace on that body's own "INSERT INTO v", which therefore codes
// v's victim-delete body too. Measured: the 3.53.3 oracle refuses to prepare
// the outer statement with exactly v's trigger's own error.
func TestReplaceVictimNestedBodyUnderImposedReplace(t *testing.T) {
	setup := []string{
		`PRAGMA recursive_triggers=ON`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
		`CREATE TABLE v(k INTEGER PRIMARY KEY,m)`,
		`CREATE TABLE u(x,y)`,
		`CREATE TRIGGER tv AFTER DELETE ON v BEGIN INSERT INTO u VALUES(1,2,3); END`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO v VALUES(1,'m'); END`,
		`INSERT INTO t VALUES(1,'one')`,
	}
	db := newRVDTestDB(t)
	rvdExecAll(t, db, setup...)
	err := db.Exec(`INSERT OR REPLACE INTO t VALUES(1,'two')`)
	if err == nil {
		t.Fatalf("expected v's trigger body error to reach the outer INSERT OR REPLACE")
	}
	if !strings.Contains(err.Error(), "table u has 2 columns") {
		t.Fatalf("error = %v, want the nested body's own arity error", err)
	}
	if got, want := rvdRowStrings(rvdQuery(t, db, `SELECT a,b FROM t`)), []string{"1,one"}; !equalStrSlices(got, want) {
		t.Fatalf("t = %v, want %v", got, want)
	}
	// The same body reached from a statement that CANNOT resolve to REPLACE is
	// never coded, so nothing errors.
	db2 := newRVDTestDB(t)
	rvdExecAll(t, db2, setup...)
	if err := db2.Exec(`INSERT INTO t(b) VALUES('three')`); err != nil {
		t.Fatalf("a non-replacing INSERT must not inherit the nested body's error: %v", err)
	}
}
