package engine

// INSERT INTO triggered-table SELECT ... compilation to bytecode. Tests check
// that trigger fires occur inside the source loop and that NEW.rowid is correct.

import (
	"strings"
	"testing"
)

// insertSelectTriggerShapes: statements whose promotion this slice covers.
var insertSelectTriggerShapes = []conflictShapeCase{
	// The census shape, verbatim from vdbe_total_test.go's residual list.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a,b FROM s`},

	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a,b FROM s`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE s(b)`, `CREATE TABLE log(x,y)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`},
		`INSERT INTO t(b) SELECT b FROM s`},

	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(z)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.b); END`},
		`INSERT INTO t(b) SELECT z FROM s`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a FROM s UNION ALL SELECT a FROM s`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a FROM s WHERE a>1 ORDER BY a`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a FROM t`},

	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log SELECT count(*) FROM t; END`},
		`INSERT INTO t SELECT a FROM s`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ua AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a*10); END`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`},
		`INSERT INTO t SELECT a FROM s`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.a>1 BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a FROM s`},

	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t SELECT a,b FROM s`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`, `CREATE TABLE s(a,b)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a,b FROM s`},
}

// TestInsertSelectTriggerCompilesToBytecode checks that all shapes compile.
func TestInsertSelectTriggerCompilesToBytecode(t *testing.T) {
	for i, tc := range insertSelectTriggerShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestInsertSelectTriggerFiresInsideTheScanLoop checks that trigger fires
// occur inside the scan loop with different rowid registers for BEFORE and AFTER.
func TestInsertSelectTriggerFiresInsideTheScanLoop(t *testing.T) {
	for i, tc := range insertSelectTriggerShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		opened, rewind, next := -1, -1, -1
		cursor := -1
		var fires []int
		for j := range prog.Insns {
			switch prog.Insns[j].Op {
			case OpOpenDerived:
				opened, cursor = j, prog.Insns[j].P1
			case OpRewind:
				rewind = j
			case OpNext:
				next = j
			case OpFireTriggers:
				fires = append(fires, j)
			}
		}
		if opened < 0 || rewind < 0 || next < 0 {
			t.Errorf("[%d] %q: open=%d rewind=%d next=%d -- the source is not being SCANNED",
				i, tc.stmt, opened, rewind, next)
			continue
		}
		if prog.Insns[rewind].P1 != cursor || prog.Insns[next].P1 != cursor {
			t.Errorf("[%d] %q: the loop drives cursor %d/%d, not the derived source's cursor %d",
				i, tc.stmt, prog.Insns[rewind].P1, prog.Insns[next].P1, cursor)
		}
		if len(fires) == 0 {
			t.Errorf("[%d] %q: compiled to a program that fires NO triggers. The rows land and the\n"+
				"side effect vanishes -- a wrong answer, not a gap.", i, tc.stmt)
			continue
		}
		rowidRegs := map[int]bool{}
		for _, j := range fires {
			if !(rewind < j && j < next) {
				t.Errorf("[%d] %q: the OpFireTriggers at %d is OUTSIDE the loop (rewind=%d next=%d).\n"+
					"It would fire once for the whole statement, which a one-row source cannot tell\n"+
					"apart from correct.", i, tc.stmt, j, rewind, next)
			}
			plan, ok := prog.Insns[j].P4.(*triggerFirePlan)
			if !ok || plan == nil {
				t.Errorf("[%d] %q: OpFireTriggers with no plan", i, tc.stmt)
				continue
			}
			if plan.nCols <= 0 {
				t.Errorf("[%d] %q: fire plan has nCols=%d; opFireTriggers slices the NEW row by it",
					i, tc.stmt, plan.nCols)
			}
			bodies := 0
			for _, ct := range plan.triggers {
				bodies += len(ct.body)
			}
			if bodies == 0 {
				t.Errorf("[%d] %q: the fire plan at %d is empty of compiled body statements", i, tc.stmt, j)
			}
			rowidRegs[prog.Insns[j].P2] = true
		}
		if len(fires) == 2 && len(rowidRegs) != 2 {
			t.Errorf("[%d] %q: both fires read the SAME rowid register. A BEFORE INSERT program must see\n"+
				"-1 for an auto-assigned rowid (C SQLite has not allocated one at insert.c:1495)\n"+
				"and the AFTER one the real value.", i, tc.stmt)
		}
	}
}

// TestInsertSelectTriggerAnswers checks that trigger fires produce correct results.
func TestInsertSelectTriggerAnswers(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		{"after-fires-per-source-row", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(a,b)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1,'one'),(2,'two'),(3,'three')`},
			`INSERT INTO t SELECT a,b FROM s`, `SELECT x FROM log ORDER BY rowid`,
			[]string{"1", "2", "3"}},
		{"before-sees-minus-one-after-sees-the-rowid", []string{`CREATE TABLE w(a)`, `CREATE TABLE ws(a)`,
			`CREATE TABLE wlog(t,r)`,
			`CREATE TRIGGER wb BEFORE INSERT ON w BEGIN INSERT INTO wlog VALUES('B',new.rowid); END`,
			`CREATE TRIGGER wa AFTER INSERT ON w BEGIN INSERT INTO wlog VALUES('A',new.rowid); END`,
			`INSERT INTO ws VALUES('p'),('q')`},
			`INSERT INTO w SELECT a FROM ws`, `SELECT t,r FROM wlog ORDER BY rowid`,
			[]string{"B,-1", "A,1", "B,-1", "A,2"}},
		{"ipk-before-sees-minus-one", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`,
			`CREATE TABLE s(b)`, `CREATE TABLE log(x,y)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a); END`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`,
			`INSERT INTO s VALUES('one'),('two')`},
			`INSERT INTO t(b) SELECT b FROM s`, `SELECT x,y FROM log ORDER BY rowid`,
			[]string{"B,-1", "A,1", "B,-1", "A,2"}},
		{"raise-ignore-skips-one-source-row", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(a,b)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1,'one'),(2,'two'),(3,'three')`},
			`INSERT INTO t SELECT a,b FROM s`, `SELECT a FROM t ORDER BY rowid`,
			[]string{"1", "3"}},
		{"raise-ignore-does-not-log-the-skipped-row", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(a,b)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1,'one'),(2,'two'),(3,'three')`},
			`INSERT INTO t SELECT a,b FROM s`, `SELECT x FROM log ORDER BY rowid`,
			[]string{"1", "3"}},
		{"body-read-is-live", []string{`CREATE TABLE u(a)`, `CREATE TABLE us(a)`, `CREATE TABLE ulog(n)`,
			`CREATE TRIGGER ua AFTER INSERT ON u BEGIN INSERT INTO ulog SELECT count(*) FROM u; END`,
			`INSERT INTO us VALUES(10),(20),(30)`},
			`INSERT INTO u SELECT a FROM us`, `SELECT n FROM ulog ORDER BY rowid`,
			[]string{"1", "2", "3"}},
		{"self-sourced-insert-terminates", []string{`CREATE TABLE v(a)`, `CREATE TABLE vlog(x)`,
			`CREATE TRIGGER va AFTER INSERT ON v BEGIN INSERT INTO vlog VALUES(new.a); END`,
			`INSERT INTO v VALUES(1),(2)`},
			`INSERT INTO v SELECT a FROM v`, `SELECT a FROM v ORDER BY rowid`,
			[]string{"1", "2", "1", "2"}},
		{"when-guard", []string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.a>1 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1),(2),(3)`},
			`INSERT INTO t SELECT a FROM s`, `SELECT x FROM log ORDER BY rowid`,
			[]string{"2", "3"}},
		{"nested-trigger", []string{`CREATE TABLE t(a)`, `CREATE TABLE s(a)`, `CREATE TABLE u(a)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER ua AFTER INSERT ON u BEGIN INSERT INTO log VALUES(new.a*10); END`,
			`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`,
			`INSERT INTO s VALUES(1),(2)`},
			`INSERT INTO t SELECT a FROM s`, `SELECT x FROM log ORDER BY rowid`,
			[]string{"10", "20"}},
		{"empty-source-fires-nothing", []string{`CREATE TABLE z(a)`, `CREATE TABLE zs(a)`,
			`CREATE TABLE zlog(x)`,
			`CREATE TRIGGER za AFTER INSERT ON z BEGIN INSERT INTO zlog VALUES(new.a); END`},
			`INSERT INTO z SELECT a FROM zs`, `SELECT count(*) FROM zlog`, []string{"0"}},
	}
	for _, tc := range cases {
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
			// The statement must really be running as BYTECODE, or this test
			// measures nothing about the emitter it is named for.
			_, cerr := db.compileWrite(tc.stmt)
			if cerr != nil {
				t.Fatalf("compile %q: %v", tc.stmt, cerr)
			}
			if err := db.Exec(tc.stmt); err != nil {
				t.Fatalf("exec %q: %v", tc.stmt, err)
			}
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("%q then %q: got %v, want %v", tc.stmt, tc.query, got, tc.want)
			}
		})
	}
}

// TestInsertSelectTriggerDisablesTheXferOptimization checks that triggers
// disable the xfer optimization bypass.
func TestInsertSelectTriggerDisablesTheXferOptimization(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE src(a, g AS (a+1))`,
		`CREATE TABLE dst(a, g AS (a+1))`,
		`CREATE TABLE dst2(a, g AS (a+1))`,
		`CREATE TABLE dst3(a, g AS (a+1))`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON dst2 BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER tr3 BEFORE INSERT ON dst3 BEGIN INSERT INTO log VALUES(new.a*100); END`,
		`INSERT INTO src VALUES(1),(2)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if err := db.Exec(`INSERT INTO dst SELECT * FROM src`); err != nil {
		t.Fatalf("untriggered destination: %v -- the xfer bypass must still apply", err)
	}
	if got := rvdRowStrings(rvdQuery(t, db, `SELECT a,g FROM dst ORDER BY rowid`)); strings.Join(got, "|") != "1,2|2,3" {
		t.Fatalf("untriggered destination holds %v, want [1,2 2,3]", got)
	}
	for _, tc := range []struct{ stmt, want string }{
		{`INSERT INTO dst2 SELECT * FROM src`, `engine: table dst2 has 1 columns but 2 values were supplied`},
		{`INSERT INTO dst3 SELECT * FROM src`, `engine: table dst3 has 1 columns but 2 values were supplied`},
	} {
		err := db.Exec(tc.stmt)
		if err == nil {
			t.Fatalf("%q SUCCEEDED. A trigger disables xferOptimization (insert.c:1032), so the\n"+
				"ordinary arity check applies and the 3.53.3 oracle rejects this statement.", tc.stmt)
		}
		if err.Error() != tc.want {
			t.Fatalf("%q: got %q, want %q", tc.stmt, err.Error(), tc.want)
		}
	}
	if got := rvdRowStrings(rvdQuery(t, db, `SELECT count(*) FROM log`)); strings.Join(got, "|") != "0" {
		t.Fatalf("log holds %v, want [0] -- neither rejected statement may have fired a trigger", got)
	}
}

// TestInsertSelectTriggerUnmodeledShapesDoNotCompile checks that unmodeled
// shapes fail to compile.
func TestInsertSelectTriggerUnmodeledShapesDoNotCompile(t *testing.T) {
	for _, d := range []struct {
		why string
		tc  conflictShapeCase
	}{
		// "an OR clause" and "a declared ON CONFLICT default" were the first
		// two entries here and are GONE -- both were promoted by the
		// conflict x trigger slice, which is where the reasons they gave are
		// answered:
		//
		//   - the OR clause DOES override every body statement's conflict
		//     policy (trigger.c:1137), and this compiler models that now:
		//     compileInsertStmt passes stmt.orAction/stmt.explicitOr to
		//     compileTriggerFirePlanOrconf, which is codeRowTrigger's orconf
		//     argument, and compileTriggerBodyStmt bakes it into each body
		//     statement.
		//   - the declared default never reached the trigger call at all:
		//     sqlite3Insert hands sqlite3CodeRowTrigger its OWN onError
		//     parameter (insert.c:1494-1496, :1604-1607), and
		//     sqlite3GenerateConstraintChecks overrides that per constraint
		//     internally without handing it back.
		//
		// Both are pinned against the oracle in
		// compat-harness/conflict_trigger_codegen_diff_test.go
		// (ins-select-or-ignore, ins-select-declared-ignore) and asserted to
		// compile in engine/conflict_trigger_codegen_test.go. What is still
		// declined is the crossing with a BEFORE program, below.
		// "a conflict clause on a table that also has a BEFORE INSERT trigger",
		// and the same crossing through a DECLARED default, were the last two
		// entries here and are GONE -- both are in insertSelectTriggerShapes
		// above now. The reason they gave was TRUE and was fixed rather than
		// argued away: this emitter did run the constraint checks above the
		// BEFORE fire (insert.c:1494 vs :1569), and emitInsertRowBody now emits
		// them below it. The SELECT spelling needed one edit of its own --
		// compileInsertSelectWrite fired the BEFORE program from insertTail,
		// which is BELOW the checks, and now hands it to emitInsertRowBody
		// through compiler.insertBeforeFire like the VALUES spelling does.
		// THIS TEST is what caught that: with only compileInsertStmt's guard
		// lifted, both cases compiled to a program that fired too late.
		// "RETURNING beside a trigger" WAS here and is GONE, and the reason is
		// worth recording rather than just deleting.
		//
		// compileInsertStmt's guard carried two decline terms, one per cluster:
		// "stmt.returning != nil" (the RETURNING cluster) and
		// "stmt.selectStmt != nil" (this one). Each cluster removed its own and
		// still carried the other's, so their CROSSING -- "INSERT ... SELECT
		// ... RETURNING into a triggered table" -- was reachable by neither.
		// The integration dropped both, which is the union the two clusters
		// imply, and the crossing now compiles.
		//
		// Restoring the decline here would NOT buy correctness, which is the
		// measurement that settles it. Over the setup in
		// compat-harness/insert_select_returning_once_test.go, with an AFTER
		// INSERT trigger writing log and a RETURNING subquery counting it:
		//
		//	oracle                      1|20, 2|20, 3|20
		//	compiled (today)            1|21, 2|21, 3|21
		//	declined                    1|20, 2|21, 3|22
		//
		// Both routes were wrong and they were wrong in different ways: the
		// compiled one has C's OP_Once LIFETIME (trigger.c:998-1013) with the
		// snapshot taken one AFTER-trigger fire too late, the declined route
		// re-evaluated per row. So this is not a shape a decline protects, and
		// pinning it here would assert a decline that is neither correct nor
		// the status quo. It is recorded as a known divergence under paydown by
		// TestInsertSelectReturningOnceSnapshotPoint, whose fix is the snapshot
		// POINT on the INSERT ... SELECT route -- RETURNING-cluster work, not
		// this cluster's.
		// "an upsert against a table with an INSERT trigger" was the last
		// entry here and is GONE. Its parenthetical -- "an upsert's DO UPDATE
		// has no fire to interleave with" -- was the mechanism, and it was
		// BUILT rather than argued away: emitUpsertTail now takes the AFTER
		// plan and emits it inside its plain-insert branch, the only outcome
		// that reaches insert.c:1604-1607, while the BEFORE program fires
		// ahead of the tail exactly where insert.c:1494-1496 fires it. Pinned
		// structurally in engine/conflict_trigger_codegen_test.go's
		// TestUpsertAfterFireSitsOnThePlainInsertBranch and against the oracle
		// in compat-harness/conflict_trigger_codegen_diff_test.go's upsert-*
		// cases. What stays declined is the DO UPDATE branch meeting this
		// table's own UPDATE triggers -- and THAT is gone too now
		// (declineUpsertUpdateTriggers is deleted): a DO UPDATE branch IS an
		// UPDATE of the conflicting row, upsert.c:325-326 calling
		// sqlite3Update outright, so emitUpsertTail emits the target's
		// BEFORE/AFTER UPDATE programs around its re-store. The oracle half for
		// the SELECT row source is
		// compat-harness/insert_select_upsert_trigger_test.go.
	} {
		_, err := compileShape(t, d.tc)
		if err != nil {
			// A hard error is also an acceptable outcome for a shape SQLite
			// itself rejects; what must not happen is a compiled program.
			continue
		}
		// Reached only when the compile SUCCEEDED (the arm above continues on an
		// error), and a shape this slice does not model must not compile.
		{
			t.Errorf("%q COMPILED, but this slice does not model it: %s.\n"+
				"Serving it here without the semantics it needs is a WRONG ANSWER,\n"+
				"not a promotion (AGENTS.md invariant 1).", d.tc.stmt, d.why)
		}
	}
}
