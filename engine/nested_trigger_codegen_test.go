package engine

// Tests nested triggers (trigger bodies writing triggered tables). Verifies that
// nested trigger code generation works and that cyclic trigger graphs compile
// without infinite recursion via memoization.

import "testing"

// nestedTriggerShapes tests various nested trigger scenarios.
var nestedTriggerShapes = []conflictShapeCase{
	// Two levels, INSERT -> INSERT.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,new.b); END`},
		`INSERT INTO t VALUES(1,'x')`},

	// Three levels.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tv AFTER INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO v VALUES(new.a); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`},
		`INSERT INTO t VALUES(1)`},

	// The inner write is a DELETE / an UPDATE, each firing that table's own
	// trigger -- the DELETE and UPDATE guards had the same refusal.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ud AFTER DELETE ON u BEGIN INSERT INTO log VALUES(old.a); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN DELETE FROM u WHERE a=new.a; END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER uu AFTER UPDATE ON u BEGIN INSERT INTO log VALUES(new.b); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET b='q' WHERE a=new.a; END`},
		`INSERT INTO t VALUES(1)`},

	// The OUTER statement is a DELETE / an UPDATE whose trigger body writes a
	// triggered table.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO u VALUES(old.a); END`},
		`DELETE FROM t WHERE a=1`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(new.b); END`},
		`UPDATE t SET b='q'`},

	// A WHEN guard at each level, and two triggers on the inner table.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER u1 AFTER INSERT ON u WHEN new.a>0 BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER u2 AFTER INSERT ON u BEGIN INSERT INTO log VALUES(-new.a); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t WHEN new.a<>0 BEGIN INSERT INTO u VALUES(new.a); END`},
		`INSERT INTO t VALUES(1)`},

	// A diamond: one trigger reached down two paths of a single statement.
	// SQLite codes it ONCE (getRowTrigger's list is per top-level Parse), which
	// is what triggerPrgMemo reproduces.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`, `CREATE TABLE w(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ww AFTER INSERT ON w BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO w VALUES(new.a); END`,
		`CREATE TRIGGER vv AFTER INSERT ON v BEGIN INSERT INTO w VALUES(new.a); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO v VALUES(new.a); END`},
		`INSERT INTO t VALUES(1)`},

	// The CYCLES. Each of these is a compile that did not terminate before
	// triggerPrgMemo existed.
	{[]string{`CREATE TABLE t(a)`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO t VALUES(new.a+1); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a+1); END`,
		`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO t VALUES(new.a+1); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
		`CREATE TRIGGER ud AFTER DELETE ON u BEGIN INSERT INTO t VALUES(old.a); END`,
		`CREATE TRIGGER uu AFTER INSERT ON u BEGIN DELETE FROM u WHERE a=new.a-1; END`},
		`INSERT INTO t VALUES(1)`},

	// The CASCADE-REWRITES-A-LATER-ROW family, which is where round 1 was
	// oracle-wrong: a cascade modifies a row the outer DELETE/UPDATE scan has
	// not visited yet, and OpNotExists' re-seek is what makes that row's OLD.*
	// (and its SET right-hand sides) the LIVE values C SQLite reads. Every
	// one of these has a matching case in
	// compat-harness/nested_trigger_reseek_test.go -- pinned HERE as well
	// because that battery cannot see whether the shape still COMPILES, only
	// whether the answer matched.
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a, old.b); INSERT INTO u VALUES(old.a); END`,
		`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b='UP' WHERE a=2; END`},
		`DELETE FROM t`},
	{[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE u(z)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER INSERT ON u BEGIN UPDATE t1 SET b='CHANGED' WHERE a=new.z; END`,
		`CREATE TRIGGER rd AFTER DELETE ON t1 BEGIN INSERT INTO u VALUES(old.a+2); INSERT INTO log VALUES('d:'||old.a||'/'||old.b); END`},
		`DELETE FROM t1 WHERE a=1 OR a=3`},
	{[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES('u:'||old.a); END`,
		`CREATE TRIGGER rd AFTER DELETE ON t1 BEGIN UPDATE t1 SET b='CHANGED' WHERE a=old.a+2; INSERT INTO log VALUES('d:'||old.a||'/'||old.b); END`},
		`DELETE FROM t1 WHERE a=1 OR a=3`},
	{[]string{`CREATE TABLE t1(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(z)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER INSERT ON u BEGIN UPDATE t1 SET b=b||'*' WHERE a=new.z; END`,
		`CREATE TRIGGER rd AFTER DELETE ON t1 BEGIN INSERT INTO u VALUES(old.a+2); INSERT INTO log VALUES('d:'||old.a||'/'||old.b); END`},
		`DELETE FROM t1 WHERE a IN (1,3)`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v,w)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a,old.b); INSERT INTO u VALUES(old.a); END`,
		`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN DELETE FROM t WHERE a=2; INSERT INTO t VALUES(2,'REBORN'); END`},
		`DELETE FROM t`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE u(x)`, `CREATE TABLE log(v)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(old.a); INSERT INTO log VALUES(old.a||'/'||old.b||'->'||new.b); END`,
		`CREATE TRIGGER ui AFTER INSERT ON u WHEN new.x=1 BEGIN UPDATE t SET b='UP' WHERE a=3; END`},
		`UPDATE t SET b=b||'!'`},
}

// walkFirePlans visits prog and every sub-program reachable through an
// OpFireTriggers payload, calling visit on each. The visited sets are not
// tidiness: a cyclic trigger graph compiles to a CYCLIC program graph (a body
// program whose fire plan holds the very trigger whose body it is), so an
// unguarded walk would not terminate either.
func walkFirePlans(prog *Program, visit func(*Program)) {
	seenProg := map[*Program]bool{}
	seenTrig := map[*compiledTrigger]bool{}
	var walk func(p *Program)
	walk = func(p *Program) {
		if p == nil || seenProg[p] {
			return
		}
		seenProg[p] = true
		visit(p)
		for i := range p.Insns {
			if p.Insns[i].Op != OpFireTriggers {
				continue
			}
			plan, ok := p.Insns[i].P4.(*triggerFirePlan)
			if !ok {
				continue
			}
			for _, ct := range plan.triggers {
				if seenTrig[ct] {
					continue
				}
				seenTrig[ct] = true
				walk(ct.when)
				for _, b := range ct.body {
					walk(b)
				}
			}
		}
	}
	walk(prog)
}

// TestNestedTriggerCompilesToBytecode is the RULE #1 assertion the differential
// harness cannot make: every shape must LOWER, trigger bodies included.
// compat-harness/nested_trigger_adversarial_test.go goes on passing whether or
// not it does, which is exactly why this assertion exists.
func TestNestedTriggerCompilesToBytecode(t *testing.T) {
	for i, tc := range nestedTriggerShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestNestedTriggerEmitsANestedFirePlan is the non-vacuity half: it is not
// enough that these compile, the INNER table's triggers must actually be part
// of the compiled program. Without this, a change that quietly stopped
// compiling the nested fire plan -- firing nothing at all for the inner write
// -- would pass the test above.
func TestNestedTriggerEmitsANestedFirePlan(t *testing.T) {
	tc := conflictShapeCase{
		setup: []string{`CREATE TABLE t(a,b)`, `CREATE TABLE u(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,new.b); END`},
		stmt: `INSERT INTO t VALUES(1,'x')`,
	}
	prog, err := compileShape(t, tc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// depth 0 = the statement itself, 1 = tt's body, 2 = tu's body.
	names := map[string]bool{}
	walkFirePlans(prog, func(p *Program) {
		for i := range p.Insns {
			if p.Insns[i].Op != OpFireTriggers {
				continue
			}
			plan := p.Insns[i].P4.(*triggerFirePlan)
			for _, ct := range plan.triggers {
				names[ct.tr.name] = true
			}
		}
	})
	for _, want := range []string{"tt", "tu"} {
		if !names[want] {
			t.Errorf("compiled program graph never mentions trigger %q; fired triggers found: %v.\n"+
				"The nested trigger is not being compiled, so the inner write would fire NOTHING.",
				want, names)
		}
	}
}

// TestNestedTriggerCycleCompilesFinitely is the memo's regression test. Every
// case is a trigger graph with a CYCLE, which before triggerPrgMemo recursed
// until the goroutine stack overflowed -- a fatal runtime error, not even a
// recoverable panic, on statements C SQLite runs.
//
// Compiling is the assertion: reaching the end of this test at all is the
// result being pinned. It is separate from the table above so the failure names
// the mechanism.
func TestNestedTriggerCycleCompilesFinitely(t *testing.T) {
	for _, tc := range []conflictShapeCase{
		{[]string{`CREATE TABLE t(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO t VALUES(new.a+1); END`},
			`INSERT INTO t VALUES(1)`},
		{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a+1); END`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO t VALUES(new.a+1); END`},
			`INSERT INTO t VALUES(1)`},
		{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO v VALUES(new.a); END`,
			`CREATE TRIGGER vv AFTER INSERT ON v BEGIN INSERT INTO t VALUES(new.a); END`},
			`INSERT INTO t VALUES(1)`},
	} {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("%q: a cyclic trigger graph must still compile, got: %v", tc.stmt, err)
		}
	}
}

// nestedBeforeCascades: the nine shapes nestedBeforeTriggerDeclined used to
// refuse -- a trigger BODY statement writing a table that carries a BEFORE
// trigger of any event. They are PROMOTED now, and this table's assertion is
// inverted with them: each must compile, and must carry a fire plan.
//
// The decline was never about nesting or about timing, and -- measured -- it
// was never about the UPDATE or DELETE compilers either. It was ONE defect in
// emitInsertRowBody: it allocated the row's rowid BEFORE the OpFireTriggers
// that runs the BEFORE program, so a body statement inserting into the same
// table got back the rowid the outer row had already reserved. insert.c fires
// first (:1494-1496) and allocates after (:1539); this emitter now does too,
// and that alone makes all nine compilable. The UPDATE side needed no emitter
// change at all: update.c's After-BEFORE-trigger-reload-loop (:1002-1017) is
// already implemented one layer down, in opUpdateRow's plan.setCol merge --
// see the OpSkipIfRowGone emission in compileUpdateWrite for the measurement
// that settled it.
//
// A DIFFERENTIAL test cannot prove this promotion -- all nine were answered
// correctly before it, which is exactly why the decline survived so long -- so
// the assertion is at COMPILE level. Their ANSWERS are pinned separately,
// against the real 3.53.3 oracle, in
// compat-harness/before_trigger_cascade_test.go.
var nestedBeforeCascades = []struct {
	why string
	conflictShapeCase
}{
	{
		why: "BEFORE trigger whose body writes its own table (insert.c:1494-1496 vs 1538-1541)",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE tbl(a,b,c)`,
				`CREATE TRIGGER tbl_trig BEFORE INSERT ON tbl BEGIN INSERT INTO tbl VALUES(new.a,new.b,new.c); END`},
			`INSERT INTO tbl VALUES(1,2,3)`,
		},
	},
	{
		why: "BEFORE trigger reached through an AFTER cascade back into the outer table",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`,
				`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO t VALUES(new.a+100); END`,
				`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`},
			`INSERT INTO t VALUES(1)`,
		},
	},
	{
		why: "a body UPDATE of a table carrying a BEFORE trigger (the rowid-move route)",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(id INTEGER PRIMARY KEY, b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER ub BEFORE UPDATE ON u BEGIN INSERT INTO log VALUES(old.id); END`,
				`CREATE TRIGGER tt AFTER INSERT ON t BEGIN UPDATE u SET id=9 WHERE id=1; END`},
			`INSERT INTO t VALUES(1)`,
		},
	},

	// The six below are the CROSS-EVENT directions. Each has a cascade writing
	// a table that carries a BEFORE trigger for a DIFFERENT event and NO
	// trigger at all for the event the cascade uses, so each one slipped past
	// the guard while it was consulted from inside "if db.tableHasTriggers(tbl,
	// <this statement's event>)". Two of them were oracle-verified wrong
	// answers -- a spurious "UNIQUE constraint failed: t.a" and a silently
	// wrong rowid, both quoted in compat-harness/before_trigger_cascade_test.go
	// as "after-cascade-update" and "after-cascade-delete". Delete a case and
	// the differential battery still passes, because it never sees whether the
	// shape compiled: that is why they are pinned HERE.
	{
		why: "cascade UPDATEs a table whose only trigger is BEFORE INSERT",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`,
				`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(1); END`,
				`CREATE TRIGGER ua AFTER INSERT ON u BEGIN UPDATE t SET a=6 WHERE a=5; END`},
			`INSERT INTO t(b) VALUES('new')`,
		},
	},
	{
		why: "cascade DELETEs from a table whose only trigger is BEFORE INSERT",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE u(x)`,
				`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO u VALUES(1); END`,
				`CREATE TRIGGER ua AFTER INSERT ON u BEGIN DELETE FROM t WHERE a=5; END`},
			`INSERT INTO t(b) VALUES('new')`,
		},
	},
	{
		why: "cascade DELETEs from a table whose only triggers are on UPDATE",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE tt(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE x(v)`, `CREATE TABLE log(n)`,
				`CREATE TRIGGER bu BEFORE UPDATE ON tt BEGIN INSERT INTO x VALUES(old.a); END`,
				`CREATE TRIGGER xi AFTER INSERT ON x BEGIN DELETE FROM tt WHERE a=new.v+1; END`,
				`CREATE TRIGGER au AFTER UPDATE ON tt BEGIN INSERT INTO log VALUES(old.a); END`},
			`UPDATE tt SET b=b||'!'`,
		},
	},
	{
		why: "cascade UPDATEs a table whose only triggers are on DELETE",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE tt(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE x(v)`, `CREATE TABLE log(n)`,
				`CREATE TRIGGER bd BEFORE DELETE ON tt BEGIN INSERT INTO x VALUES(old.a); END`,
				`CREATE TRIGGER xi AFTER INSERT ON x BEGIN UPDATE tt SET a=a+100 WHERE a=new.v; END`,
				`CREATE TRIGGER ad AFTER DELETE ON tt BEGIN INSERT INTO log VALUES(old.a); END`},
			`DELETE FROM tt WHERE a=1`,
		},
	},
	{
		why: "cascade INSERTs into a table whose only trigger is BEFORE DELETE",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(x)`,
				`CREATE TRIGGER ub BEFORE DELETE ON u BEGIN SELECT 1; END`,
				`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`},
			`INSERT INTO t VALUES(1)`,
		},
	},
	{
		why: "cascade INSERTs into a table whose only trigger is BEFORE UPDATE",
		conflictShapeCase: conflictShapeCase{
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(x)`,
				`CREATE TRIGGER ub BEFORE UPDATE ON u BEGIN SELECT 1; END`,
				`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`},
			`INSERT INTO t VALUES(1)`,
		},
	},
}

// TestNestedBeforeCascadeCompiles is the compile-level proof for the
// BEFORE-trigger trio ("trigger body INSERT/UPDATE/DELETE into a
// BEFORE-triggered table"). Every shape must reach real bytecode.
//
// The NON-VACUITY half matters as much as the assertion: each case must also
// actually CARRY a trigger fire plan. Without that check a setup whose trigger
// silently failed to attach would pass by compiling an ordinary triggerless
// INSERT.
func TestNestedBeforeCascadeCompiles(t *testing.T) {
	for _, tc := range nestedBeforeCascades {
		prog, err := compileShape(t, tc.conflictShapeCase)
		if err != nil {
			t.Errorf("RULE #1: %q must COMPILE (%s), got error: %v", tc.stmt, tc.why, err)
			continue
		}
		nProg, nFire := 0, 0
		walkFirePlans(prog, func(p *Program) {
			nProg++
			for i := range p.Insns {
				if p.Insns[i].Op == OpFireTriggers {
					nFire++
				}
			}
		})
		if nFire == 0 {
			t.Errorf("VACUOUS: %q compiled with no OpFireTriggers anywhere (%d programs walked) -- "+
				"the setup's trigger did not attach, so this case proves nothing.", tc.stmt, nProg)
		}
	}
}

// TestDeleteLoopGuardsAVanishedRow pins the OpNotExists guard compileDeleteStmt
// emits, and pins that it is emitted ONLY where SQLite emits one.
//
// The wrong answer without it: "DELETE FROM t1 WHERE a=1 OR a=3" whose AFTER
// DELETE trigger deletes a=3 fired that trigger TWICE, because the scan's
// snapshot still yields the row the trigger removed. delete.c:768-774 re-seeks
// and bypasses the row; delete.c:358/498 are why a trigger-free DELETE needs no
// guard at all (it takes a one-pass plan).
func TestDeleteLoopGuardsAVanishedRow(t *testing.T) {
	countNotExists := func(tc conflictShapeCase) int {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		n := 0
		for i := range prog.Insns {
			if prog.Insns[i].Op == OpNotExists {
				n++
			}
		}
		return n
	}
	withTrigger := conflictShapeCase{
		[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER r1 AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM t1 WHERE a=1`,
	}
	if n := countNotExists(withTrigger); n != 1 {
		t.Errorf("a DELETE with triggers emitted %d OpNotExists guards, want 1 -- "+
			"a row an earlier row's trigger deleted would be deleted and fired again "+
			"(delete.c:768-774)", n)
	}
	noTrigger := conflictShapeCase{
		[]string{`CREATE TABLE t1(a,b)`},
		`DELETE FROM t1 WHERE a=1`,
	}
	if n := countNotExists(noTrigger); n != 0 {
		t.Errorf("a trigger-free DELETE emitted %d OpNotExists guards, want 0 -- "+
			"SQLite emits none either (eMode!=ONEPASS_OFF; delete.c:358/498/773)", n)
	}
	// delete.c emits the guard a SECOND time once the BEFORE program has been
	// coded, "if( addrStart<sqlite3VdbeCurrentAddr(v) )" (delete.c:822), because
	// that program may have moved the cursor or already deleted the row. The
	// condition is exactly "were there BEFORE triggers", so this counts 2.
	//
	// It used to be unable to FIRE at all: reaching this row from inside its
	// own BEFORE program means a trigger body writing a table that carries a
	// BEFORE trigger, which nestedBeforeTriggerDeclined refused -- so this
	// assertion was the only gate it had. That decline is gone, so the guard is
	// live now and a differential case pins it too:
	// compat-harness/before_trigger_cascade_test.go's
	// "body-delete-before-moves-row" is the review case this note describes
	// ("BEFORE DELETE ... cascade UPDATEs the row's rowid"), which without the
	// guard fires the AFTER DELETE trigger and counts changes()=1 where the
	// oracle fires nothing and counts 0. Both gates are kept: this one still
	// says WHERE the guard is emitted, which no answer comparison can.
	withBefore := conflictShapeCase{
		[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER b1 BEFORE DELETE ON t1 BEGIN INSERT INTO log VALUES(old.a); END`,
			`CREATE TRIGGER r1 AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM t1 WHERE a=1`,
	}
	if n := countNotExists(withBefore); n != 2 {
		t.Errorf("a DELETE with BEFORE triggers emitted %d OpNotExists guards, want 2 -- "+
			"delete.c re-seeks after the BEFORE program too (delete.c:817-823)", n)
	}
}

// TestUpdateLoopReseeksEachRow pins update.c's own top-of-loop re-seek, the
// guard delete.c's was ported without.
//
// It is not only a "did the row vanish" test: OpNotExists RE-READS the row
// (reseekRowStore, vdbe_cursor.go), which is what makes OLD.* and every column
// the SET does not assign the row's LIVE content -- update.c:906/961 both read
// the re-seeked iDataCur. Two oracle-verified wrong answers without it: an
// AFTER cascade that rewrote a row this scan had not reached updated the STALE
// image ("3|r!" for a row the oracle leaves "3|UP!"), and a BEFORE cascade that
// deleted a later row still fired that row's BEFORE trigger, whose own cascade
// then DESTROYED a third row the oracle keeps.
func TestUpdateLoopReseeksEachRow(t *testing.T) {
	countNotExists := func(tc conflictShapeCase) int {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		n := 0
		for i := range prog.Insns {
			if prog.Insns[i].Op == OpNotExists {
				n++
			}
		}
		return n
	}
	withTrigger := conflictShapeCase{
		[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE log(n)`,
			`CREATE TRIGGER u1 AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE t1 SET b='x' WHERE a=1`,
	}
	if n := countNotExists(withTrigger); n != 1 {
		t.Errorf("an UPDATE with triggers emitted %d OpNotExists re-seeks, want 1 -- "+
			"update.c:875-878 re-seeks the real table at the top of every iteration, "+
			"and everything below reads that cursor (update.c:906/961)", n)
	}
	noTrigger := conflictShapeCase{
		[]string{`CREATE TABLE t1(a,b)`},
		`UPDATE t1 SET b='x' WHERE a=1`,
	}
	if n := countNotExists(noTrigger); n != 0 {
		t.Errorf("a trigger-free UPDATE emitted %d OpNotExists re-seeks, want 0 -- "+
			"nothing can change a row mid-scan without triggers, and SQLite takes a "+
			"one-pass plan for it", n)
	}
}

// TestNestedTriggerMemoCodesEachTriggerOnce pins the memo's OTHER half. A
// diamond reaches trigger ww down two paths; SQLite's getRowTrigger returns the
// SAME TriggerPrg for both (trigger.c:1376-1385), so the sub-program exists
// once. Asserting pointer identity is what keeps a "just make a fresh copy per
// path" simplification -- which still terminates, and which the differential
// harness cannot see -- from turning a shared trigger graph into an exponential
// compile.
func TestNestedTriggerMemoCodesEachTriggerOnce(t *testing.T) {
	tc := conflictShapeCase{
		setup: []string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE v(a)`, `CREATE TABLE w(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ww AFTER INSERT ON w BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE TRIGGER uu AFTER INSERT ON u BEGIN INSERT INTO w VALUES(new.a); END`,
			`CREATE TRIGGER vv AFTER INSERT ON v BEGIN INSERT INTO w VALUES(new.a); END`,
			`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); INSERT INTO v VALUES(new.a); END`},
		stmt: `INSERT INTO t VALUES(1)`,
	}
	prog, err := compileShape(t, tc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	byName := map[string]map[*compiledTrigger]bool{}
	walkFirePlans(prog, func(p *Program) {
		for i := range p.Insns {
			if p.Insns[i].Op != OpFireTriggers {
				continue
			}
			for _, ct := range p.Insns[i].P4.(*triggerFirePlan).triggers {
				if byName[ct.tr.name] == nil {
					byName[ct.tr.name] = map[*compiledTrigger]bool{}
				}
				byName[ct.tr.name][ct] = true
			}
		}
	})
	if n := len(byName["ww"]); n != 1 {
		t.Errorf("trigger ww was coded %d times, want 1 -- the TriggerPrg memo is not being shared\n"+
			"across the two paths that reach it (SQLite: getRowTrigger, trigger.c:1376).", n)
	}
	if len(byName) != 4 {
		t.Errorf("compiled %d distinct triggers, want 4 (tt,uu,vv,ww): %v", len(byName), byName)
	}
}
