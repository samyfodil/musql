package engine

// Tests for VIEW INSTEAD OF INSERT: INSERT statements against views with
// INSTEAD OF INSERT triggers. Tests verify compilation, trigger firing,
// declined shapes, and answer correctness including column naming and affinity.

import (
	"strings"
	"testing"
)

// viewInsertShapes: statements whose promotion this batch is about.
var viewInsertShapes = []conflictShapeCase{
	// The census shape, verbatim from vdbe_total_test.go's list.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v VALUES(1,2)`},

	// Multi-row VALUES: one fire per tuple, each with its own row-end label.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v VALUES(1,2),(3,4),(5,6)`},

	// An explicit column list, including a partial one and a doubly-named one.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v(c,a) VALUES(1,2)`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v(c) VALUES(1)`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v(a,a) VALUES(1,2)`},

	// A WHEN guard, and a WHEN guard carrying a subquery (which takes the LIVE
	// read seam, vdbe_live_read.go, exactly as a table trigger's does).
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v WHEN new.a > 2 BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO v VALUES(1),(3)`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO v VALUES(1)`},

	// A body whose own read must bind at FIRE time.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log SELECT count(*) FROM b; END`},
		`INSERT INTO v VALUES(1),(2)`},

	// Two INSTEAD OF triggers on one view: both fire, newest first.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER v1 INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES('one'); END`,
		`CREATE TRIGGER v2 INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES('two'); END`},
		`INSERT INTO v VALUES(1)`},

	// A RAISE(IGNORE) body -- the per-row skipAddr the emitter copies the plan
	// for.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO v VALUES(1),(2),(3)`},

	// The view's own column RENAME list, and a view whose body is a join --
	// both exercise viewColumnInfos rather than a bare "SELECT * FROM one".
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v(p,q) AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.p,new.q); END`},
		`INSERT INTO v(q,p) VALUES(1,2)`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE d(z)`, `CREATE TABLE log(x,y)`,
		`CREATE VIEW v AS SELECT a, z FROM b, d`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a,new.z); END`},
		`INSERT INTO v VALUES(1,2)`},

	// A view over a view, and a TEMP view with a TEMP trigger.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW inner1 AS SELECT a FROM b`,
		`CREATE VIEW v AS SELECT a FROM inner1`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO v VALUES(5)`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE TEMP VIEW v AS SELECT a FROM b`,
		`CREATE TEMP TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO v VALUES(3)`},

	// A schema-qualified target, and the REPLACE spelling routed through
	// compileWriteProgram's "REPLACE" case -- the latter still declines, so it
	// is NOT here; the qualified one is.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO main.v VALUES(6)`},

	// The view INSERT is itself a TRIGGER BODY statement (an ordinary table's
	// AFTER trigger writing a view) -- the whole outer statement must compile,
	// INSTEAD OF plan and all.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a+100); END`,
		`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO v VALUES(new.a); END`},
		`INSERT INTO t VALUES(1),(2)`},

	// A view whose INSTEAD OF body writes ANOTHER view (INSTEAD OF cascading
	// into INSTEAD OF), which is what the shared TriggerPrg memo has to carry.
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a FROM b`, `CREATE VIEW w AS SELECT a FROM b`,
		`CREATE TRIGGER wi INSTEAD OF INSERT ON w BEGIN INSERT INTO log VALUES(new.a*10); END`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO w VALUES(new.a); END`},
		`INSERT INTO v VALUES(4)`},

	// ---- the ROW-SOURCE shapes (this slice) ----
	//
	// DEFAULT VALUES: insert.c's nColumn==0 (insert.c:1214), which the parser
	// already models as one empty tuple, so every view column takes the OpNull
	// the C emits for a column with no source slot.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v DEFAULT VALUES`},

	// INSERT ... SELECT: positional, an explicit column list (the unnamed view
	// column stays NULL), a doubly-named one (first-wins), a compound source, a
	// source that reads the view's own base table, and one whose source yields
	// NO rows at all.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v SELECT a,c FROM s`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v(c) SELECT a FROM s`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v(a,a) SELECT a,c FROM s`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE s(a)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
		`INSERT INTO v SELECT a FROM s UNION ALL SELECT a FROM s`},
	{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
		`INSERT INTO v SELECT a FROM b`},
	{[]string{`CREATE TABLE b(a)`, `CREATE TABLE s(a)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
		`INSERT INTO v SELECT a FROM s WHERE 0`},
}

// viewInsertSelectShapes is the subset of viewInsertShapes whose row source is
// a SELECT -- the ones TestViewInsertSelectSourceScansACursor checks the LOOP
// SHAPE of. Kept as its own list rather than sniffed out of the slice above so
// that adding a shape there cannot silently skip that check.
var viewInsertSelectShapes = []conflictShapeCase{
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v SELECT a,c FROM s`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
		`INSERT INTO v(c) SELECT a FROM s`},
	{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
		`INSERT INTO v SELECT a FROM b`},
}

// TestViewInsertSelectSourceScansACursor verifies that the trigger fires for
// each row from the SELECT source, and that WritePager is properly set.
func TestViewInsertSelectSourceScansACursor(t *testing.T) {
	for i, tc := range viewInsertSelectShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		if prog.WritePager == nil {
			t.Errorf("[%d] %q: the program declares no WritePager, so cachedWriteProgram will REUSE it\n"+
				"and a second run replays this compile's frozen snapshot.", i, tc.stmt)
		}
		opened, rewind, next, fire := -1, -1, -1, -1
		var cursor int
		for j := range prog.Insns {
			switch prog.Insns[j].Op {
			case OpOpenDerived:
				opened, cursor = j, prog.Insns[j].P1
			case OpRewind:
				rewind = j
			case OpNext:
				next = j
			case OpFireTriggers:
				fire = j
			}
		}
		if opened < 0 {
			t.Errorf("[%d] %q: no OpOpenDerived. The source SELECT is not being scanned at all --\n"+
				"an emitter that fires once answers a one-row source correctly and passes\n"+
				"every behavioural case in this file.", i, tc.stmt)
			continue
		}
		if rewind < 0 || next < 0 || fire < 0 {
			t.Errorf("[%d] %q: rewind=%d next=%d fire=%d -- the fire is not inside a scan loop",
				i, tc.stmt, rewind, next, fire)
			continue
		}
		if !(opened < rewind && rewind < fire && fire < next) {
			t.Errorf("[%d] %q: instruction order is open=%d rewind=%d fire=%d next=%d; the fire must sit\n"+
				"BETWEEN the rewind and the loop-back, or it runs once for the whole source",
				i, tc.stmt, opened, rewind, fire, next)
		}
		if prog.Insns[rewind].P1 != cursor || prog.Insns[next].P1 != cursor {
			t.Errorf("[%d] %q: the loop drives cursor %d/%d, not the derived source's cursor %d",
				i, tc.stmt, prog.Insns[rewind].P1, prog.Insns[next].P1, cursor)
		}
		plan, ok := prog.Insns[fire].P4.(*triggerFirePlan)
		if !ok || plan == nil {
			t.Errorf("[%d] %q: OpFireTriggers with no plan", i, tc.stmt)
			continue
		}
		fromCursor := 0
		for c := 0; c < plan.nCols; c++ {
			reg := prog.Insns[fire].P1 + c
			for k := rewind; k < fire; k++ {
				if prog.Insns[k].Op == OpColumn && prog.Insns[k].P1 == cursor && prog.Insns[k].P3 == reg {
					fromCursor++
				}
			}
		}
		if fromCursor == 0 {
			t.Errorf("[%d] %q: not one of the %d NEW-row registers is fed by an OpColumn off the source\n"+
				"cursor. The rows the trigger sees do not come from the SELECT.", i, tc.stmt, plan.nCols)
		}
	}
}

// TestViewInsertCompilesToBytecode verifies that all view INSERT shapes
// compile to bytecode.
func TestViewInsertCompilesToBytecode(t *testing.T) {
	for i, tc := range viewInsertShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestViewInsertEmitsAnInsteadOfFirePlan verifies that each shape emits
// OpFireTriggers with a compiled trigger body.
func TestViewInsertEmitsAnInsteadOfFirePlan(t *testing.T) {
	for i, tc := range viewInsertShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		fires, bodies := 0, 0
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
					t.Errorf("[%d] %q: fire plan has nCols=%d; opFireTriggers slices the NEW row by it",
						i, tc.stmt, plan.nCols)
				}
				fires++
				for _, ct := range plan.triggers {
					bodies += len(ct.body)
				}
			}
		})
		if fires == 0 {
			t.Errorf("[%d] %q: compiled to a program that fires NO triggers. A view INSERT that fires\n"+
				"nothing does nothing -- this is the mutation TestViewInsertCompilesToBytecode cannot see.",
				i, tc.stmt)
		}
		if bodies == 0 {
			t.Errorf("[%d] %q: every fire plan is empty of compiled body statements", i, tc.stmt)
		}
	}
}

// viewInsertDeclined: shapes this compiler deliberately does NOT model. Each
// must decline CLEANLY -- half-serving one here would be a wrong answer rather
// than a gap. See compileViewInsertStmt's own list for the reason attached to
// each.
var viewInsertDeclined = []struct {
	why string
	tc  conflictShapeCase
}{
	// A plain "INSERT INTO v VALUES(1) RETURNING a" used to sit here, on the
	// reason "returningPlan is keyed on a *tableMeta, which a view has none of".
	// A view answers the four fields that plan reads (viewReturningMeta), and
	// the row image is the NEW register block the fire already uses -- served
	// now, one RETURNING row per VALUES tuple, emitted before each row's
	// skipAddr so a RAISE(IGNORE) takes the returned row with it -- and a
	// SUBQUERY in the list works too, on C's own lifetime rule. See
	// compat-harness/view_returning_test.go, which covers the SELECT-source
	// spelling too. One shape of it still declines:
	{"RETURNING mixing the two subquery lifetimes (one naming the view, one not)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE TABLE s(n)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
			`INSERT INTO v VALUES(1) RETURNING a,(SELECT count(*) FROM v),(SELECT n FROM s)`}},
	{"UPSERT (insert.c:1296's PREPARE-time \"cannot UPSERT a view\", plus markPrepareFailed)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
			`INSERT INTO v VALUES(1) ON CONFLICT(a) DO NOTHING`}},
	{"a TRIGGER BODY's INSERT ... SELECT (its source must be read as of the FIRING, and may name NEW)",
		conflictShapeCase{[]string{`CREATE TABLE t(k)`, `CREATE TABLE b(a)`, `CREATE TABLE s(z)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO v SELECT z FROM s; END`},
			`INSERT INTO t VALUES(1)`}},
	// "an explicit OR clause" used to sit here. It was declined on the ground
	// that "the TABLE path declines an OR-clause on a triggered table", which
	// stopped being true the day compileInsertStmt started passing its own
	// stmt.orAction/stmt.explicitOr to compileTriggerFirePlanOrconf. For a VIEW
	// the clause reaches ONE thing -- the INSTEAD OF program (insert.c:1494-1496
	// hands onError to sqlite3CodeRowTrigger; every other consumer is inside the
	// "if( !isView ){" at insert.c:1500) -- and compileViewFirePlan already took
	// the policy as parameters. It is served now; see
	// TestViewInsertCompiledAnswers' "or-" cases and, for the oracle,
	// compat-harness/view_or_alias_promoted_test.go.
	{"a subquery inside a VALUES tuple (this emitter installs no pager for the view path)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE TABLE s(z)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
			`INSERT INTO v VALUES((SELECT count(*) FROM s))`}},
	{"no matching INSTEAD OF INSERT trigger (a PREPARE-time rejection plus markPrepareFailed)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`},
			`INSERT INTO v VALUES(1)`}},
	{"a body naming NEW.rowid, which a view does not have (build.c:3026 / resolve.c:564)",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.rowid); END`},
			`INSERT INTO v VALUES(1)`}},
	{"a body naming OLD, which an INSERT event does not supply",
		conflictShapeCase{[]string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(old.a); END`},
			`INSERT INTO v VALUES(1)`}},
	{"a wrong-arity tuple (checkViewInsertArity owns both of SQLite's two wordings)",
		conflictShapeCase{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v VALUES(1)`}},
	{"a column list naming a column the view does not have",
		conflictShapeCase{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v(zz) VALUES(1)`}},
	{"a view over a table that does not exist (a real prepare error, worded by viewColumnInfos)",
		conflictShapeCase{[]string{`CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM nosuch`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO v VALUES(1)`}},
}

// TestViewInsertDeclinedShapesDeclineCleanly verifies that unsupported
// shapes decline rather than being approximated.
func TestViewInsertDeclinedShapesDeclineCleanly(t *testing.T) {
	for _, d := range viewInsertDeclined {
		_, err := compileShape(t, d.tc)
		if err != nil {
			// The decline, or a hard error for a shape SQLite itself rejects.
			// What must not happen is a compiled program.
			continue
		}
		t.Errorf("%q COMPILED, but this batch does not model it: %s.\n"+
			"Serving it here without the semantics that shape needs is a WRONG ANSWER,\n"+
			"not a promotion (AGENTS.md invariant 1).", d.tc.stmt, d.why)
	}
}

// TestViewInsertCompiledAnswers verifies the correct answers for compiled
// view INSERT statements.
func TestViewInsertCompiledAnswers(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		// The statement's OR clause governs the INSTEAD OF program, and the
		// only way to SEE that is a body step that conflicts: "OR IGNORE"
		// makes the body's own INSERT skip the duplicate and the second body
		// step still run, where the bare spelling aborts the whole statement.
		// insert.c:1494-1496 -> trigger.c:1376 (the memo key) -> trigger.c:1137
		// (pParse->eOrconf, imposed on every body step).
		{"or-ignore-governs-the-body", []string{`CREATE TABLE b(a UNIQUE)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`},
			`INSERT OR IGNORE INTO v VALUES(1),(2)`, `SELECT x FROM log ORDER BY rowid`, []string{"1", "2"}},
		// Without the clause the same script aborts on the duplicate, so
		// nothing is logged at all -- which is what makes the case above a
		// measurement of the policy rather than of the emitter.
		{"no-or-clause-aborts-the-body", []string{`CREATE TABLE b(a UNIQUE)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`},
			`INSERT INTO v VALUES(1),(2)`, `SELECT x FROM log ORDER BY rowid`, nil},
		// "REPLACE INTO" is the OR REPLACE spelling and reaches the same place.
		{"or-replace-spelling", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`},
			`REPLACE INTO v VALUES(6)`, `SELECT x FROM log`, []string{"6"}},
		{"positional", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v VALUES(1,2)`, `SELECT a,c FROM b`, []string{"1,2"}},
		{"multi-row", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v VALUES(1,2),(3,4)`, `SELECT a,c FROM b ORDER BY rowid`, []string{"1,2", "3,4"}},
		{"named-out-of-order", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v(c,a) VALUES(1,2)`, `SELECT a,c FROM b`, []string{"2,1"}},
		// The OpNull seeding: a column the list does not name is NULL, not
		// whatever the register happened to hold.
		{"named-partial-leaves-null", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v(c) VALUES(7)`, `SELECT a,c FROM b`, []string{"<NULL>,7"}},
		// SQLite's historical first-wins quirk for a doubly-named column.
		{"doubly-named-column-first-wins", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v(a,a) VALUES(1,2)`, `SELECT a,c FROM b`, []string{"1,<NULL>"}},
		// insert.c:1490's "if( !isView )" around sqlite3TableAffinity: the NEW
		// row keeps the VALUES expression's own type. With the affinity pass
		// added this reads "text".
		{"no-affinity-conversion", []string{`CREATE TABLE b(t TEXT)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT t FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(typeof(new.t)); END`},
			`INSERT INTO v VALUES(5)`, `SELECT x FROM log`, []string{"integer"}},
		// A tuple slot NO column maps to is never CODED (insert.c:1363's loop
		// runs over table columns, reading pList->a[aTabColMap[i]-1]), so it is
		// never evaluated and its integer overflow never happens. Verified
		// against 3.53.3, where this statement succeeds. A route that evaluates
		// every tuple expression regardless raises it -- a pre-existing
		// divergence this emitter deliberately does not inherit.
		{"unmapped-tuple-expression-is-not-evaluated", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v(a,a) VALUES(1, abs(-9223372036854775807-1))`, `SELECT a, c IS NULL FROM b`, []string{"1,1"}},
		// ...and the slot that DID win the race is coded, so its error IS the
		// statement's.
		{"mapped-tuple-expression-still-errors", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v(a,a) VALUES(abs(-9223372036854775807-1), 1)`, `SELECT count(*) FROM b`, []string{"0"}},
		// The per-row plan copy: RAISE(IGNORE) abandons THIS row and the next
		// one still fires.
		{"raise-ignore-skips-one-row", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO v VALUES(1),(2),(3)`, `SELECT x FROM log ORDER BY rowid`, []string{"1", "3"}},
		// A body read binds at FIRE time, not at the firing statement's
		// compile time (vdbe_live_read.go). A frozen image answers 0,0.
		{"body-read-is-live", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log SELECT count(*) FROM b; END`},
			`INSERT INTO v VALUES(1),(2)`, `SELECT x FROM log ORDER BY rowid`, []string{"1", "2"}},
		// Two triggers, newest first (matchingInsteadOfTriggers walks backward).
		{"two-triggers-newest-first", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER v1 INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES('one'); END`,
			`CREATE TRIGGER v2 INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES('two'); END`},
			`INSERT INTO v VALUES(1)`, `SELECT x FROM log ORDER BY rowid`, []string{"two", "one"}},
		// INSTEAD OF cascading into INSTEAD OF.
		{"view-body-writes-another-view", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`, `CREATE VIEW w AS SELECT a FROM b`,
			`CREATE TRIGGER wi INSTEAD OF INSERT ON w BEGIN INSERT INTO log VALUES(new.a*10); END`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO w VALUES(new.a); END`},
			`INSERT INTO v VALUES(4)`, `SELECT x FROM log`, []string{"40"}},
		// The view's explicit "(p,q)" rename list decides which slot a named
		// column list feeds.
		{"view-rename-list", []string{`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v(p,q) AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.p,new.q); END`},
			`INSERT INTO v(q,p) VALUES(1,2)`, `SELECT x,y FROM log`, []string{"2,1"}},

		// ---- the ROW-SOURCE shapes. Every expectation is the 3.53.3 oracle's,
		// measured directly.
		//
		// DEFAULT VALUES: one row, every view column NULL. A view column has no
		// DEFAULT of its own (sqlite3ColumnExpr returns 0), so insert.c's
		// default arm emits OP_Null for each.
		{"default-values", []string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v DEFAULT VALUES`, `SELECT a IS NULL, c IS NULL FROM b`, []string{"1,1"}},
		// The SCAN: three source rows must fire three times. An emitter that
		// fired once passes every compile-level check that does not look for
		// the loop.
		{"select-source-fires-per-row", []string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO s VALUES(1,2),(3,4),(5,6)`},
			`INSERT INTO v SELECT a,c FROM s`, `SELECT a,c FROM b ORDER BY rowid`,
			[]string{"1,2", "3,4", "5,6"}},
		// A named column list over a SELECT source scatters by colIdx and
		// leaves the unnamed view column NULL, exactly as the VALUES spelling
		// does.
		{"select-source-named-partial", []string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(z)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO s VALUES(7),(8)`},
			`INSERT INTO v(c) SELECT z FROM s`, `SELECT a IS NULL, c FROM b ORDER BY rowid`,
			[]string{"1,7", "1,8"}},
		// The source is MATERIALIZED before the first fire (insert.c:1167-1169's
		// useTempTable, which a view target always takes because it always has
		// a trigger). Without that, the body's own writes would feed the scan
		// and this would not terminate.
		{"select-source-reads-its-own-base-table", []string{`CREATE TABLE b(a)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`INSERT INTO b VALUES(1),(2)`},
			`INSERT INTO v SELECT a FROM b`, `SELECT a FROM b ORDER BY rowid`,
			[]string{"1", "2", "1", "2"}},
		// An empty source fires nothing at all -- the OpRewind's jump past the
		// body.
		{"select-source-empty", []string{`CREATE TABLE b(a)`, `CREATE TABLE s(a)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`},
			`INSERT INTO v SELECT a FROM s`, `SELECT count(*) FROM b`, []string{"0"}},
		// RAISE(IGNORE) abandons THIS source row and the scan advances -- the
		// single plan copy's skipAddr, which for a loop is the loop's own row
		// end rather than a per-tuple one.
		{"select-source-raise-ignore-skips-one-row", []string{`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE TABLE s(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(1),(2),(3)`},
			`INSERT INTO v SELECT a FROM s`, `SELECT x FROM log ORDER BY rowid`, []string{"1", "3"}},
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
			// The statement must really COMPILE, or this case asserts nothing
			// about the codegen.
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile %q: %v", tc.stmt, cerr)
			}
			// An expected error case is spelled as "the write left nothing
			// behind", so Exec's error is deliberately not fatal here.
			db.Exec(tc.stmt)
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("%q then %q:\n got %v\nwant %v", tc.stmt, tc.query, got, tc.want)
			}
		})
	}
}

// TestViewInsertFillsEveryNewRowRegister verifies that all NEW-row registers
// are written before the trigger fires.
func TestViewInsertFillsEveryNewRowRegister(t *testing.T) {
	for i, tc := range viewInsertShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		for j := range prog.Insns {
			if prog.Insns[j].Op != OpFireTriggers {
				continue
			}
			plan, ok := prog.Insns[j].P4.(*triggerFirePlan)
			if !ok || plan == nil || prog.Insns[j].P1 < 0 {
				continue
			}
			written := map[int]bool{}
			for k := 0; k < j; k++ {
				switch prog.Insns[k].Op {
				case OpNull, OpSCopy:
					written[prog.Insns[k].P2] = true
				case OpColumn:
					// The SELECT row source (emitViewInsertSelect) fills the
					// block from the scan cursor instead of from coded
					// expressions. OpColumn carries its destination in P3.
					written[prog.Insns[k].P3] = true
				}
			}
			for c := 0; c < plan.nCols; c++ {
				if !written[prog.Insns[j].P1+c] {
					t.Errorf("[%d] %q: NEW-row register %d (column %d of %d) is never written before\n"+
						"the OpFireTriggers at %d. It happens to READ as NULL because a fresh register's\n"+
						"zero Value is NULL -- which is exactly why no behavioural test catches this.",
						i, tc.stmt, prog.Insns[j].P1+c, c, plan.nCols, j)
				}
			}
		}
	}
}

// TestViewInsertBodyWritesReachTheFile verifies that trigger body writes are
// actually committed to the file, not just visible in-session.
func TestViewInsertBodyWritesReachTheFile(t *testing.T) {
	path := t.TempDir() + "/vi.musq"
	db, err := Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE b(a,c)`,
		`CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close (schema): %v", err)
	}

	// A SECOND session, so the INSERT is the only thing it does -- with the
	// CREATEs in the same session their own markFileChanged would mask a
	// missing one here.
	db2, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if _, cerr := db2.compileWrite(`INSERT INTO v VALUES(1,2)`); cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if err := db2.Exec(`INSERT INTO v VALUES(1,2)`); err != nil {
		t.Fatalf("view insert: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close (insert): %v", err)
	}

	db3, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite (readback): %v", err)
	}
	defer db3.Discard()
	got := rvdRowStrings(rvdQuery(t, db3, `SELECT a,c FROM b`))
	if len(got) != 1 || got[0] != "1,2" {
		t.Fatalf("the INSTEAD OF body's row did not survive Close: got %v, want [1,2].\n"+
			"OpChangeCounter is what sets writeCtx.changeCounterPending, half of runWrite's\n"+
			"markFileChanged gate -- without it commitIsNoOp throws the body's writes away.", got)
	}
}
