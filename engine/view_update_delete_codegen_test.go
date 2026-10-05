package engine

// Tests for VIEW INSTEAD OF UPDATE and INSTEAD OF DELETE: UPDATE and DELETE
// statements against views with INSTEAD OF triggers. Tests verify compilation,
// materialization and scanning, trigger firing, unmodeled declines, and answer
// correctness.

import (
	"strings"
	"testing"
)

// viewUpdateDeleteShapes: statements whose promotion this batch is about.
var viewUpdateDeleteShapes = []conflictShapeCase{
	// The two census shapes, verbatim from vdbe_total_test.go's list.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET c=new.c; END`},
		`UPDATE v SET c=9`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`},
		`DELETE FROM v WHERE a=1`},

	// No WHERE at all: every materialized row fires.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`},
		`DELETE FROM v`},

	// A WHERE over the view's columns, on both verbs.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET c=new.c WHERE a=old.a; END`},
		`UPDATE v SET c=c+1 WHERE a>1`},

	// A SET whose right-hand side reads the OLD row, and a multi-target SET
	// (the simultaneity the setReg/OpSCopy split exists for).
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a,new.c); END`},
		`UPDATE v SET a=c, c=a`},

	// A WHEN guard, and one carrying a subquery (the LIVE read seam).
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v WHEN old.a > 2 BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM v`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(new.a); END`},
		`UPDATE v SET a=1`},

	// A body whose own read must bind at FIRE time.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; INSERT INTO log SELECT count(*) FROM b; END`},
		`DELETE FROM v`},

	// Two INSTEAD OF triggers on one view: both fire, newest first.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER v1 INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('one'); END`,
		`CREATE TRIGGER v2 INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('two'); END`},
		`DELETE FROM v`},

	// A RAISE(IGNORE) body -- the skipAddr the emitter patches to the row end.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN SELECT RAISE(IGNORE) WHERE old.a=2; INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM v`},

	// The view's own column RENAME list, and a view whose body is a join --
	// both exercise viewColumnInfos rather than a bare "SELECT * FROM one".
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v(p,q) AS SELECT a,c FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.p,new.q); END`},
		`UPDATE v SET q=1 WHERE p=2`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE d(z)`, `CREATE TABLE log(x,y)`,
		`CREATE VIEW v AS SELECT a, z FROM b, d`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a,old.z); END`},
		`DELETE FROM v`},

	// A view over a view, and a TEMP view with a TEMP trigger.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW inner1 AS SELECT a FROM b`,
		`CREATE VIEW v AS SELECT a FROM inner1`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM v`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE TEMP VIEW v AS SELECT a FROM b`,
		`CREATE TEMP TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`},
		`UPDATE v SET a=3`},

	// A schema-qualified target.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM main.v`},

	// A WHERE carrying a subquery -- the writeSubqueryPager arm of
	// beginViewWriteScan.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE s(z)`, `CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`},
		`DELETE FROM v WHERE a > (SELECT count(*) FROM s)`},

	// The view write is itself a TRIGGER BODY statement -- the LIVE
	// materialization arm (trig != nil), where the whole outer statement must
	// compile, INSTEAD OF plan and all.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a+100); END`,
		`CREATE TRIGGER ti AFTER INSERT ON t BEGIN DELETE FROM v; END`},
		`INSERT INTO t VALUES(1)`},

	// A view whose INSTEAD OF body writes ANOTHER view (INSTEAD OF cascading
	// into INSTEAD OF), which is what the shared TriggerPrg memo has to carry.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a FROM b`, `CREATE VIEW w AS SELECT a FROM b`,
		`CREATE TRIGGER wd INSTEAD OF DELETE ON w BEGIN INSERT INTO log VALUES(old.a*10); END`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM w WHERE a=old.a; END`},
		`DELETE FROM v`},
}

// TestViewUpdateDeleteCompilesToBytecode is the RULE #1 assertion the
// differential harness cannot make. It walks the whole program GRAPH, because
// an INSTEAD OF body's own sub-program has to lower as completely as the
// statement that fires it.
func TestViewUpdateDeleteCompilesToBytecode(t *testing.T) {
	for i, tc := range viewUpdateDeleteShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestViewUpdateDeleteMaterializesAndScans is the non-vacuity half that is
// specific to THESE two verbs, and it is the assertion that separates them
// from the INSERT batch.
//
// INSERT needed no cursor at all; UPDATE and DELETE are exactly the verbs that
// do, because sqlite3MaterializeView runs the view's SELECT into an ephemeral
// table (delete.c:167-168's SRT_EphemTab dest) and the statement then SCANS it.
// An emitter that fired the trigger ONCE, with no cursor, would answer
// correctly for a one-row view and pass every test above. So this asserts the
// loop structurally: an OpOpenDerived carrying a compiled sub-Program, an
// OpRewind over that same cursor, and an OpNext closing the loop back above
// the fire.
func TestViewUpdateDeleteMaterializesAndScans(t *testing.T) {
	for i, tc := range viewUpdateDeleteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		found := false
		walkFirePlans(prog, func(p *Program) {
			for j := range p.Insns {
				if p.Insns[j].Op != OpOpenDerived {
					continue
				}
				ds, ok := p.Insns[j].P4.(*derivedSource)
				if !ok || ds == nil || ds.prog == nil {
					continue
				}
				cur := p.Insns[j].P1
				var rewind, next, fire bool
				for k := range p.Insns {
					switch p.Insns[k].Op {
					case OpRewind:
						rewind = rewind || p.Insns[k].P1 == cur
					case OpNext:
						// P2 must land back INSIDE the loop, i.e. after the
						// OpOpenDerived that opened it.
						next = next || (p.Insns[k].P1 == cur && p.Insns[k].P2 > j && p.Insns[k].P2 < k)
					case OpFireTriggers:
						fire = fire || (k > j)
					}
				}
				if rewind && next && fire {
					found = true
				}
			}
		})
		if !found {
			t.Errorf("[%d] %q: no MATERIALIZE-and-SCAN loop in the emitted program.\n"+
				"sqlite3MaterializeView (delete.c:142-170) runs the view's SELECT into an ephemeral\n"+
				"table and the statement scans it (delete.c:428-435 / update.c:630-636). An emitter\n"+
				"that fires once without a cursor answers a ONE-ROW view correctly and passes every\n"+
				"other test in this file.", i, tc.stmt)
		}
	}
}

// TestViewUpdateDeleteEmitsAnInsteadOfFirePlan asserts each shape really does
// emit an OpFireTriggers whose plan carries the view's own trigger with a
// compiled body, that the plan's row width is the VIEW's column count (the
// number opFireTriggers slices the parent registers by), and that the plan
// supplies OLD -- which is the half a DELETE exists for.
func TestViewUpdateDeleteEmitsAnInsteadOfFirePlan(t *testing.T) {
	for i, tc := range viewUpdateDeleteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		fires, bodies, withOld := 0, 0, 0
		walkFirePlans(prog, func(p *Program) {
			for j := range p.Insns {
				if p.Insns[j].Op != OpFireTriggers {
					continue
				}
				plan, ok := p.Insns[j].P4.(*triggerFirePlan)
				if !ok || plan == nil {
					t.Errorf("[%d] %q: OpFireTriggers with no plan", i, tc.stmt)
					continue
				}
				if plan.nCols <= 0 {
					t.Errorf("[%d] %q: fire plan has nCols=%d; opFireTriggers slices the row by it",
						i, tc.stmt, plan.nCols)
				}
				fires++
				if plan.hasOld {
					if p.Insns[j].P3 < 0 {
						t.Errorf("[%d] %q: fire plan supplies OLD but P3 (the OLD base register) is %d;\n"+
							"opFireTriggers then hands the body a nil OLD row", i, tc.stmt, p.Insns[j].P3)
					}
					withOld++
				}
				for _, ct := range plan.triggers {
					bodies += len(ct.body)
				}
			}
		})
		if fires == 0 {
			t.Errorf("[%d] %q: compiled to a program that fires NO triggers. A view UPDATE/DELETE that\n"+
				"fires nothing does nothing -- the mutation TestViewUpdateDeleteCompilesToBytecode\n"+
				"cannot see.", i, tc.stmt)
		}
		if bodies == 0 {
			t.Errorf("[%d] %q: every fire plan is empty of compiled body statements", i, tc.stmt)
		}
		if withOld == 0 {
			t.Errorf("[%d] %q: no fire plan supplies OLD. Both of this batch's verbs do (hasOld is\n"+
				"true for triggerDelete and triggerUpdate alike), and OLD.* is the whole row image a\n"+
				"DELETE body has to work from.", i, tc.stmt)
		}
	}
}

// viewUpdateDeleteDeclined: shapes this compiler deliberately does NOT model.
// None of them may COMPILE -- half-serving one would be a wrong answer, and a
// clean refusal is the lesser defect (AGENTS.md invariant 1). See
// compileViewDeleteStmt/compileViewUpdateStmt for the reason attached to each.
var viewUpdateDeleteDeclined = []struct {
	why string
	tc  conflictShapeCase
}{
	// "DELETE/UPDATE ... RETURNING" used to sit here, declined because
	// "returningPlan is keyed on a *tableMeta a view has none of". The plan
	// needed four fields of one and a view answers all four (viewReturningMeta,
	// vdbe_view_write.go); the row image was always there too -- it is the
	// NEW/OLD register block the INSTEAD OF fire already reads. Served now, with
	// the three semantics that decide it measured against 3.53.3: a body's
	// RAISE(IGNORE) takes the returned row with it, the values are the NEW/OLD
	// image and NOT what the body wrote, and each verb keeps its own affinity
	// rule. A SUBQUERY in the list works too, on C's own lifetime rule -- frozen
	// over another table, per row over the VIEW or correlated to the affected
	// row. See compat-harness/view_returning_test.go.
	{"a leading WITH (a CTE unconditionally SHADOWS a same-named table -- select.c:6028/6036 -- so the synthesized \"SELECT * FROM <view>\" could resolve to the CTE)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`},
			`WITH cc(z) AS (SELECT 1) DELETE FROM v WHERE a IN (SELECT z FROM cc)`}},
	{"INDEXED BY over a view (C SQLite says \"no such index\"; this emitter declines the shape rather than modelling the hint, so it refuses too -- in its own words, not the oracle's)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE INDEX ix ON b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`},
			`DELETE FROM v INDEXED BY ix WHERE a=1`}},
	// "UPDATE <view> ... FROM" used to sit here, on the reason "a different
	// algorithm: update.c:630's materialize is guarded nChangeFrom==0 &&
	// isView". The reason was right and it was a reason to WRITE that
	// algorithm, not to decline it: update.c:243-247 is the whole of it, and
	// pass two is this file's own emitter unchanged. It has its own file now
	// (vdbe_view_update_from.go) and its own proofs
	// (view_update_from_codegen_test.go). The three shapes of it this file's
	// emitter still declines -- RETURNING, INDEXED BY and an explicit OR --
	// are asserted there, in viewUpfromDeclined.
	// "an explicit OR clause" used to sit here, declined on a reason that had
	// expired -- see compileViewInsertStmt's own gate. For a VIEW the clause
	// reaches ONE thing, the INSTEAD OF program (update.c:984-985 hands onError
	// to sqlite3CodeRowTrigger; every other consumer is inside the "if(
	// !isView ){" on the next line), and compileViewFirePlan already took the
	// policy as parameters. Served now: see TestViewUpdateDeleteCompiledAnswers'
	// "update-or-" case, and compat-harness/view_or_alias_promoted_test.go.
	{"no matching INSTEAD OF DELETE trigger (a compile-time rejection -- db.viewModifyError; runWrite never starts, so changes() keeps the previous statement's count)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`},
			`DELETE FROM v`}},
	{"an INSTEAD OF INSERT trigger only -- the DELETE still has no match",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
			`DELETE FROM v`}},
	// Both alias entries below name the QUALIFIED spelling deliberately: the
	// UNQUALIFIED one is served now (viewAliasedTargetIgnorable), and these two
	// are the halves 3.53.3 itself rejects, which must keep failing to compile
	// rather than resolve against a scope the C does not have.
	{"an \"AS <alias>\" on the target -- sqlite3MaterializeView resolves the WHERE in a scope with no alias (delete.c:159/161), so C SQLite ERRORS on \"WHERE q.a=...\" while accepting the unqualified spelling",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`},
			`DELETE FROM v AS q WHERE q.a=1`}},
	{"an \"AS <alias>\" on an UPDATE target, for the identical reason (update.c:631)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET a=new.a; END`},
			`UPDATE v AS q SET a=2 WHERE q.a=1`}},
	{"a WHERE naming rowid, which a view does not have (build.c:3026 / resolve.c:564)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`},
			`DELETE FROM v WHERE rowid=1`}},
	{"a SET target the view does not have",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET a=new.a; END`},
			`UPDATE v SET zz=1`}},
	{"a body naming NEW, which a DELETE event does not supply",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(new.a); END`},
			`DELETE FROM v`}},
	{"a view over a table that does not exist (a real prepare error, worded at prepare time)",
		conflictShapeCase{[]string{`CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM nosuch`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`},
			`DELETE FROM v`}},
}

// TestViewUpdateDeleteUnmodeledShapesDoNotCompile pins the OTHER half of "never
// wrong": a shape this emitter does not model must be REFUSED, not
// approximated. Every case here errored (or was refused) before this batch and
// must go on doing so.
func TestViewUpdateDeleteUnmodeledShapesDoNotCompile(t *testing.T) {
	for _, d := range viewUpdateDeleteDeclined {
		_, err := compileShape(t, d.tc)
		if err != nil {
			// A hard error is also an acceptable outcome for a shape SQLite
			// itself rejects; what must not happen is a compiled program.
			continue
		}
		// Reached only when the compile SUCCEEDED (the arm above continues on an
		// error), and a shape this batch does not model must not compile.
		{
			t.Errorf("%q COMPILED, but this batch does not model it: %s.\n"+
				"Serving it here without the semantics it needs is a WRONG ANSWER,\n"+
				"not a promotion (AGENTS.md invariant 1).", d.tc.stmt, d.why)
		}
	}
}

// TestViewUpdateDeleteCompiledAnswers is the behavioural half. Every expected
// value below is the 3.53.3 oracle's --
// compat-harness/view_instead_of_update_delete_codegen_test.go runs the same
// scripts through differ(). It is kept engine-side as well because several of
// them fail on a mutation the compile-level tests above cannot see: dropping
// the WHERE, chaining the SET assignments instead of making them simultaneous,
// dropping update.c:983's affinity pass, or collecting the rows before firing
// instead of firing per row.
func TestViewUpdateDeleteCompiledAnswers(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		// The statement's OR clause governs the INSTEAD OF program
		// (update.c:984-985 -> trigger.c:1376, the memo key -> trigger.c:1137,
		// pParse->eOrconf imposed on every body step). The pair below is what
		// SHOWS that rather than merely that the statement compiles: the same
		// body step, whose own INSERT hits u's UNIQUE constraint, keeps the
		// pre-existing row under OR IGNORE and overwrites it under OR REPLACE.
		// Both spellings must also still reach the log, since the policy
		// decides the CONFLICT, not whether the body runs.
		{"update-or-ignore-governs-the-body", []string{`CREATE TABLE b(a)`, `CREATE TABLE u(k UNIQUE, tag)`,
			`CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO u VALUES(new.a,'new'); INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`, `INSERT INTO u VALUES(9,'old')`},
			`UPDATE OR IGNORE v SET a=9`, `SELECT k, tag, (SELECT count(*) FROM log) FROM u`, []string{"9,old,1"}},
		{"update-or-replace-governs-the-body", []string{`CREATE TABLE b(a)`, `CREATE TABLE u(k UNIQUE, tag)`,
			`CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO u VALUES(new.a,'new'); INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`, `INSERT INTO u VALUES(9,'old')`},
			`UPDATE OR REPLACE v SET a=9`, `SELECT k, tag, (SELECT count(*) FROM log) FROM u`, []string{"9,new,1"}},
		// An "AS <alias>" on the target is IGNORED, exactly as delete.c:159
		// ignores it: the WHERE resolves against the VIEW's own name, so the
		// unqualified spelling works. The two qualified spellings 3.53.3
		// rejects stay in viewUpdateDeleteDeclined above.
		{"delete-target-alias-unqualified", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2)`},
			`DELETE FROM v AS q WHERE a=2`, `SELECT x FROM log ORDER BY rowid`, []string{"2"}},
		{"update-target-alias-unqualified", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1),(2)`},
			`UPDATE v AS q SET a=7 WHERE a=2`, `SELECT x FROM log ORDER BY rowid`, []string{"7"}},
		{"delete-all-rows-fire", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`},
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`, []string{"1", "2", "3"}},
		// The WHERE really filters. Without it this reads 1|2|3.
		{"delete-where-filters", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`},
			`DELETE FROM v WHERE a>1`, `SELECT x FROM log ORDER BY rowid`, []string{"2", "3"}},
		// The body really deletes, and the scan is over a MATERIALIZATION the
		// body cannot shrink underneath it: all three originally-matching rows
		// fire even though the first body empties the table.
		{"delete-body-empties-base-all-still-fire", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b; INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`},
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`, []string{"1", "2", "3"}},
		// RAISE(IGNORE) abandons THIS row; the next still fires.
		{"delete-raise-ignore-skips-one-row", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN SELECT RAISE(IGNORE) WHERE old.a=2; INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(1),(2),(3)`},
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`, []string{"1", "3"}},
		// Two triggers, newest first (matchingInsteadOfTriggers walks backward).
		{"delete-two-triggers-newest-first", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER v1 INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('one'); END`,
			`CREATE TRIGGER v2 INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('two'); END`,
			`INSERT INTO b VALUES(1)`},
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`, []string{"two", "one"}},
		// A view DELETE reports changes()==0 -- it stores no row of its own,
		// and runOneTrigger saves/restores the counter around every body.
		{"delete-reports-zero-changes", []string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`,
			`INSERT INTO b VALUES(1),(2)`},
			`DELETE FROM v`, `SELECT changes()`, []string{"0"}},

		{"update-new-is-old-with-sets-applied", []string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a,new.c); END`,
			`INSERT INTO b VALUES(1,2)`},
			`UPDATE v SET c=9`, `SELECT x,y FROM log`, []string{"1,9"}},
		// SIMULTANEOUS assignment: "SET a=c, c=a" SWAPS. Chained assignment
		// (each SET reading the partly-built NEW row) answers 2,2.
		{"update-sets-are-simultaneous", []string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a,new.c); END`,
			`INSERT INTO b VALUES(1,2)`},
			`UPDATE v SET a=c, c=a`, `SELECT x,y FROM log`, []string{"2,1"}},
		// update.c:983's UNGUARDED sqlite3TableAffinity: the NEW row takes the
		// VIEW's own per-column affinities. Without the OpAffinity block this
		// reads "integer". This is the exact OPPOSITE of the INSERT verb,
		// whose matching call sits behind "if( !isView )" (insert.c:1490).
		{"update-applies-view-affinity", []string{`CREATE TABLE b(t TEXT)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT t FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(typeof(new.t)); END`,
			`INSERT INTO b VALUES('q')`},
			`UPDATE v SET t=5`, `SELECT x FROM log`, []string{"text"}},
		// ...and a WHEN guard reading the converted value FIRES, because
		// sqlite3CodeRowTrigger codes the WHEN clause after :983.
		{"update-when-sees-the-converted-value", []string{`CREATE TABLE b(t TEXT)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT t FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v WHEN new.t='5' BEGIN INSERT INTO log VALUES('fired'); END`,
			`INSERT INTO b VALUES('q')`},
			`UPDATE v SET t=5`, `SELECT x FROM log`, []string{"fired"}},
		// A column the SET does not name keeps its OLD value.
		{"update-unassigned-column-carries-over", []string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(old.a,new.a); END`,
			`INSERT INTO b VALUES(7,8)`},
			`UPDATE v SET c=1`, `SELECT x,y FROM log`, []string{"7,7"}},
		// ROW-AT-A-TIME: the SET is coded in update.c's SECOND loop, beside the
		// fire (:954 and :984-985), so row N's expressions see what row N-1's
		// body wrote. A collect-then-fire loop answers tc=3,3,3 -- which is
		// what the route this shape used to decline to answered, and what
		// compat-harness/viewvalues_f3_test.go used to PIN.
		{"update-is-row-at-a-time", []string{`CREATE TABLE base(k INTEGER PRIMARY KEY, a TEXT)`,
			`CREATE VIEW v AS SELECT k,a FROM base`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE base SET a=new.a WHERE k=old.k; END`,
			`INSERT INTO base VALUES(1,'a1'),(2,'a2'),(3,'a3')`},
			`UPDATE v SET a = 'tc=' || total_changes()`, `SELECT a FROM base ORDER BY k`,
			[]string{"tc=3", "tc=4", "tc=5"}},
		// The view's explicit "(p,q)" rename list names the OLD/NEW columns.
		{"update-view-rename-list", []string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v(p,q) AS SELECT a,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.p,new.q); END`,
			`INSERT INTO b VALUES(1,2)`},
			`UPDATE v SET q=9 WHERE p=1`, `SELECT x,y FROM log`, []string{"1,9"}},
		{"update-reports-zero-changes", []string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET a=new.a; END`,
			`INSERT INTO b VALUES(1),(2)`},
			`UPDATE v SET a=5`, `SELECT changes()`, []string{"0"}},
		// A view over a view: the materialization recurses through the read
		// compiler's own view source (resolveViewSource).
		{"delete-view-over-view", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW inner1 AS SELECT a FROM b`, `CREATE VIEW v AS SELECT a FROM inner1`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO b VALUES(4),(5)`},
			`DELETE FROM v`, `SELECT x FROM log ORDER BY rowid`, []string{"4", "5"}},
		// INSTEAD OF cascading into INSTEAD OF.
		{"delete-body-writes-another-view", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`, `CREATE VIEW w AS SELECT a FROM b`,
			`CREATE TRIGGER wd INSTEAD OF DELETE ON w BEGIN INSERT INTO log VALUES(old.a*10); END`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM w WHERE a=old.a; END`,
			`INSERT INTO b VALUES(4)`},
			`DELETE FROM v`, `SELECT x FROM log`, []string{"40"}},
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
			if eerr := db.Exec(tc.stmt); eerr != nil {
				t.Fatalf("exec %q: %v", tc.stmt, eerr)
			}
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("%q then %q:\n got %v\nwant %v", tc.stmt, tc.query, got, tc.want)
			}
		})
	}
}

// TestViewUpdateDeleteBodyWritesReachTheFile is the DURABILITY assertion, and
// it is the one the whole "does it answer correctly" family cannot make.
//
// A view UPDATE/DELETE stores no row of its own, so writeCtx.rowsAffected
// stays 0 and runOneTrigger restores it around every body anyway. runWrite
// calls markFileChanged only when changeCounterPending AND wroteRows() hold,
// and doCommit short-circuits to success whenever commitIsNoOp() does -- so a
// program that omits OpChangeCounter returns success, answers every in-session
// query correctly, and silently drops its trigger bodies' rows at Close. That
// is the defect class 0a9bbc1 and 7a39a8c were both about, and nothing above
// notices it. See view_insert_codegen_test.go's identical assertion.
func TestViewUpdateDeleteBodyWritesReachTheFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trig  string
		stmt  string
		query string
		want  string
	}{
		{"delete", `CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE a=old.a; END`,
			`DELETE FROM v WHERE a=1`, `SELECT count(*) FROM b`, "1"},
		{"update", `CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET c=new.c WHERE a=old.a; END`,
			`UPDATE v SET c=99 WHERE a=1`, `SELECT c FROM b WHERE a=1`, "99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/vud.musq"
			db, err := Create(path)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			for _, s := range []string{
				`CREATE TABLE b(a,c)`,
				`CREATE VIEW v AS SELECT a,c FROM b`,
				tc.trig,
				`INSERT INTO b VALUES(1,2),(3,4)`,
			} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close (setup): %v", err)
			}
			// A SECOND session, so the view write is the only thing it does --
			// with the setup in the same session its own markFileChanged would
			// mask a missing one here.
			db2, err := OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			_, cerr := db2.compileWrite(tc.stmt)
			if cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			if err := db2.Exec(tc.stmt); err != nil {
				t.Fatalf("%q: %v", tc.stmt, err)
			}
			if err := db2.Close(); err != nil {
				t.Fatalf("Close (write): %v", err)
			}
			db3, err := OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (readback): %v", err)
			}
			defer db3.Discard()
			got := rvdRowStrings(rvdQuery(t, db3, tc.query))
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("the INSTEAD OF body's write did not survive Close: got %v, want [%s].\n"+
					"OpChangeCounter is what sets writeCtx.changeCounterPending, half of runWrite's\n"+
					"markFileChanged gate -- without it commitIsNoOp throws the body's writes away.",
					got, tc.want)
			}
		})
	}
}
