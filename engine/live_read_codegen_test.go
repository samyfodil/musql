package engine

// This file tests LIVE-READ promotion for triggers.
// It verifies shapes compile, use the live seam, and answer from the firing instant.
// an uncorrelated subquery inside it is bracketed in OP_Once (expr.c:3889-3890)
// whose already-run bits live in the PER-INVOCATION VdbeFrame that OP_Program
// zeroes on every firing (vdbe.c:7581-7582, read at vdbe.c:2711-2715).

import "testing"

// liveReadShapes: statements whose promotion this batch is about. Each carries
// a trigger whose guard or body reads a table.
var liveReadShapes = []conflictShapeCase{
	// NEW./OLD. named inside a live sub-program -- a body VALUES subquery, a
	// WHEN guard's subquery, and a body statement's WHERE subquery. All three
	// were pinned as DECLINES right up until the trigger context was threaded
	// through the seam (Program.LiveTrig + trigOnlyOuter, vdbe_live_read.go),
	// which is what makes the SECOND lowering resolve them the same way the
	// first did.
	//
	// Settled against the 3.53.3 oracle before they were moved, which is what
	// the decline list's own message asked for -- and the two properties worth
	// naming were checked with them: the body's read sees rows the SAME
	// statement already wrote (the firing instant, which is the whole point of
	// the seam), and a NEW./OLD. operand keeps its column's DECLARED COLLATION
	// ("x COLLATE NOCASE" compared against a plain y matches case-insensitively
	// on both engines). That second one is the property a LITERAL fold through
	// compiler.rowOuter would have lost, and it is why an OpParam-backed
	// reference is a different thing from the fold this file still refuses.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s WHERE x=new.a)); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE king(a,b,PRIMARY KEY(a))`, `CREATE TABLE prince(c,d)`,
		`CREATE TRIGGER kt AFTER INSERT ON prince WHEN NOT EXISTS (SELECT a FROM king WHERE a=new.c) BEGIN INSERT INTO king VALUES(new.c,NULL); END`},
		`INSERT INTO prince VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER DELETE ON t BEGIN DELETE FROM s WHERE x IN (SELECT a FROM t WHERE a<>old.a); END`},
		`DELETE FROM t`},

	// The two census shapes, verbatim from vdbe_total_test.go's list.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'x')`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT x FROM s; END`},
		`INSERT INTO t VALUES(1,'x')`},

	// A PERSISTED virtual table, read from a body statement's subquery, from a
	// WHEN guard's subquery, and as a body's source SELECT. These three used to
	// be pinned as DECLINES, on a refusal that was deliberately wholesale --
	// "a vtab source in a trigger body declined before this batch too, so
	// refusing all of them changes nothing that used to work". Narrowing it to
	// a TABLE-VALUED FUNCTION source promotes them, and the reason is exact:
	// materializeVtab short-circuits before it ever looks at params/outer for
	// an fts3/fts4/fts5 or writable-module item that is not a table function
	// (vtab.go's three "&& !it.TableFunc" early returns), so the fresh machine
	// a live sub-program runs on supplies everything such a source needs.
	//
	// Settled against the 3.53.3 oracle first, as the decline test's own
	// message demanded -- including the LIVE half, which is the property that
	// makes this a promotion rather than a coincidence: a body that WRITES the
	// vtab and then reads it logs 1,2,3 over three separate inserts AND over
	// one three-row insert, on both engines. A frozen image logs 0,0,0.
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM r)); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM r)>0 BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO log SELECT x FROM f; END`},
		`INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO f VALUES(new.a); INSERT INTO log SELECT count(*) FROM f; END`},
		`INSERT INTO t VALUES('one'),('two')`},

	// A body VALUES tuple holding a scalar subquery -- the shape whose decline
	// TestH3TriggerBodyValuesSubqueryReadsLive used to pin.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s)); END`},
		`INSERT INTO t VALUES(1)`},
	// ...and over the FIRING table itself, which is the case a frozen image
	// gets visibly wrong (0 for every row instead of 1,2,3).
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`},
		`INSERT INTO t VALUES(1),(2),(3)`},

	// EXISTS and a row-value IN, so the OpExists / OpRowSub arms of
	// programReadsFrozenSnapshot are covered as well as OpSubquery.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES(EXISTS(SELECT 1 FROM s)); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x,y)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES(((1,2) IN (SELECT x,y FROM s))); END`},
		`INSERT INTO t VALUES(1)`},

	// A body DELETE whose WHERE holds an IN subquery (the OpInSub arm), and a
	// body UPDATE whose SET holds one over a table that is NOT the target.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN DELETE FROM s WHERE x IN (SELECT a FROM t); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x,n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN UPDATE s SET n=(SELECT count(*) FROM t); END`},
		`INSERT INTO t VALUES(1)`},

	// The firing statement is an UPDATE / a DELETE, not an INSERT.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER UPDATE ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s)); END`},
		`UPDATE t SET a=a+1`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER DELETE ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`},
		`DELETE FROM t`},

	// A BEFORE trigger, and a nested one whose inner body reads.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tu AFTER INSERT ON u BEGIN INSERT INTO log VALUES((SELECT count(*) FROM u)); END`,
		`CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a); END`},
		`INSERT INTO t VALUES(1)`},

	// A guard and a body that BOTH read, on one trigger.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t WHEN (SELECT count(*) FROM s)<2 BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log SELECT count(*) FROM s; END`},
		`INSERT INTO t VALUES(1)`},

	// A TABLE-VALUED FUNCTION source, in the three positions the decline list
	// below used to hold it: a body statement's source SELECT whose argument
	// names NEW, the same with a CONSTANT argument, and one nested inside a
	// body subquery's own derived table (so the promotion is not merely about
	// the top-level instruction list).
	//
	// These were the LAST thing programNeedsRowContext refused, and the refusal
	// was half right: the VALUES were always on the machine (runSubOnce runs a
	// live sub-program through execTrig, which seeds trigOld/trigNew), but the
	// run-time fold had no TRIGGER CONTEXT to lower "NEW.x" under, so the
	// argument resolved nowhere. derivedSource.vtabTrig (vdbe_op.go) carries
	// that context from the compile that emitted the OpOpenDerived to
	// foldVtabInputValue, and the argument becomes an OpParam read of the
	// firing row -- which is C's own shape for it (a table-valued function's
	// arguments are hidden-column constraints coded by sqlite3ExprCodeTarget,
	// expr.c:4950, inside the trigger sub-program where resolve.c:525-543 binds
	// NEW./OLD.).
	//
	// The first is attach.test 5.10 verbatim, which 3.53.3 answers ("a|1") and
	// which this engine answered as an error until this. Settled against the
	// oracle before they moved, as the decline list's message demanded --
	// compat-harness/trigger_tvf_live_row_test.go runs eleven spellings of it
	// through differ(), including a multi-row firing (the argument must be
	// re-folded per firing row, not cached), an OLD. argument on an UPDATE and
	// on a DELETE trigger, a nested trigger whose inner body must read the
	// INNER firing row, and a NEW. operand whose column carries a declared
	// COLLATE NOCASE (the property a literal fold would have lost).
	{[]string{`CREATE TABLE t1(x)`, `CREATE TABLE t2(a,b)`,
		`CREATE TRIGGER x1 AFTER INSERT ON t1 BEGIN INSERT INTO t2(a,b) SELECT key, value FROM json_each(NEW.x); END`},
		`INSERT INTO t1(x) VALUES('{"a":1}')`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log SELECT value FROM json_each('[1,2,3]'); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM (SELECT value FROM json_each('[1,2]')))); END`},
		`INSERT INTO t VALUES(1)`},
}

// TestLiveReadShapesCompileToBytecode is the RULE #1 assertion the differential
// harness cannot make. It checks the whole program GRAPH, not just the top
// instruction: a trigger body sub-program that failed to compile is exactly
// where these shapes used to hide, which is why the walk through P4 payloads
// had to be fixed first (0404988).
func TestLiveReadShapesCompileToBytecode(t *testing.T) {
	for i, tc := range liveReadShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestLiveReadSubProgramsAreLive is the non-vacuity half, and the one that
// catches the dangerous mutation rather than the obvious one.
//
// "It compiles" is not the property that matters -- a trigger body would also
// compile if its subquery were bound to the FIRING statement's snapshot, and it
// would then answer 0 where the oracle answers 1,2,3. So this walks every
// sub-program a guard or body reads through and asserts BOTH halves of what
// makes the promotion sound:
//
//   - programReadsFrozenSnapshot says no (which is the condition
//     compileTriggerBodyStmt itself checks, so a body that fails it would have
//     declined -- restated here so the guard's own removal is caught too);
//   - and every subquery payload carries Program.LiveSource, i.e. the run-time
//     re-lowering has something to re-lower. Dropping that one assignment
//     leaves TestLiveReadShapesCompileToBytecode green and every answer stale.
func TestLiveReadSubProgramsAreLive(t *testing.T) {
	for i, tc := range liveReadShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		reads := 0
		forEachTriggerSubProgram(prog, func(kind string, p *Program) {
			if programReadsFrozenSnapshot(p) {
				t.Errorf("[%d] %q: the compiled %s reads a snapshot frozen at the FIRING statement's\n"+
					"compile time. A guard/body runs per firing row and must see the database as that\n"+
					"firing sees it (vdbe_live_read.go).", i, tc.stmt, kind)
			}
			for j := range p.Insns {
				// Only the READ opcodes -- the same set
				// programReadsFrozenSnapshot judges. An OpFireTriggers payload
				// also carries sub-programs (a nested trigger's own guard and
				// body), and those are visited by this walk in their own right;
				// they are not reads and carry no LiveSource.
				switch p.Insns[j].Op {
				case OpSubquery, OpExists, OpInSub, OpRowSub, OpOpenDerived:
				default:
					continue
				}
				for _, sub := range subProgramsOf(p.Insns[j].P4) {
					if sub == nil {
						continue
					}
					reads++
					if sub.LiveSource == nil {
						t.Errorf("[%d] %q: a %s's %v sub-program carries no LiveSource, so nothing\n"+
							"re-lowers it at run time and it answers from the compile-time image.",
							i, tc.stmt, kind, p.Insns[j].Op)
					}
				}
			}
		})
		if reads == 0 {
			t.Errorf("[%d] %q: no trigger sub-program reads anything, so this case asserts nothing.\n"+
				"Either the fire plan stopped being compiled or the shape stopped carrying a read.", i, tc.stmt)
		}
	}
}

// TestLiveReadRunsAtFireTime is the behavioural half, kept engine-side rather
// than left to the harness alone because it is the ONLY assertion here that
// fails if the run-time re-lowering (liveLower on runSubOnce, vdbe.go) is
// removed while the compile-time half stays. Every script's expected answer was
// taken from the 3.53.3 oracle first -- see compat-harness/live_read_k_test.go,
// which runs these same scripts through differ() against C SQLite.
//
// A frozen pre-statement image answers a zero to every one of them.
func TestLiveReadRunsAtFireTime(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []int64
	}{
		{"body subquery sees an earlier BODY STATEMENT's write",
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
				`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES((SELECT count(*) FROM s)); END`},
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log`, []int64{1, 2, 3}},
		{"body subquery sees an earlier ROW of the firing statement",
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE log(n)`,
				`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`},
			`INSERT INTO t VALUES(1),(2),(3)`, `SELECT n FROM log`, []int64{1, 2, 3}},
		{"WHEN guard sees a write the body made on an earlier row",
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
				`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM s)<2 BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1),(2),(3),(4)`, `SELECT n FROM log`, []int64{1, 2}},
		{"body INSERT ... SELECT sees an earlier body statement's write",
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(y)`,
				`CREATE TRIGGER tb AFTER INSERT ON t BEGIN INSERT INTO s VALUES(new.a); INSERT INTO log SELECT x FROM s; END`},
			`INSERT INTO t VALUES(1),(2)`, `SELECT y FROM log`, []int64{1, 1, 2}},
	}
	for _, tc := range cases {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("[%s] create: %v", tc.name, err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		// It must really COMPILE -- otherwise the answer below proves nothing
		// about the compiled seam this file is here to gate.
		if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
			t.Errorf("[%s] compile: %v", tc.name, cerr)
		}
		if err := db.Exec(tc.stmt); err != nil {
			t.Fatalf("[%s] %s: %v", tc.name, tc.stmt, err)
		}
		got := queryInts(t, db, tc.query)
		if len(got) != len(tc.want) {
			t.Errorf("[%s] %s = %v, want %v (a snapshot frozen at the firing statement's compile time answers all zeroes)", tc.name, tc.query, got, tc.want)
		} else {
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%s] %s = %v, want %v (a snapshot frozen at the firing statement's compile time answers all zeroes)", tc.name, tc.query, got, tc.want)
					break
				}
			}
		}
		db.Discard()
	}
}

// TestLiveReadDeclinesWhatItCannotReLower pins the RESTRICTION that makes the
// seam safe, which is as load-bearing as the promotion itself -- and which this
// batch got WRONG on its first pass, so the list below is evidence, not theory.
//
// The run-time lowering is handed no enclosing compiler and no firing row
// (compileLiveSubProgram, vdbe_live_read.go), so anything a sub-SELECT can only
// resolve THROUGH one must decline -- otherwise the compile-time lowering could
// bind it and the run-time lowering could not, and the two would disagree. A
// NEW./OLD. reference is the case that matters in practice, and it is also
// where this repo already has an oracle-verified wrong answer pinned: a
// compiled OLD.x arrives without the column's declared collation
// (compat-harness/trigger_when_subquery_test.go's
// TestTriggerWhenSubqueryDeclaredCollationIsDeclined). Refusing the whole class
// keeps that decline exactly where it was.
//
// The VIRTUAL TABLE cases are the ones reasoning missed. A table-valued
// function's ARGUMENT is the one way of naming NEW./OLD. that the lowering does
// NOT resolve -- resolveFrom cannot evaluate it either and deliberately falls
// back to the function's columns alone (join.go), deferring the argument to
// run time where the VM's runVtabOnce supplies the machine's row context. A
// live sub-program is run by Program.exec on a fresh machine that has none, so
// the promotion turned attach.test 5.10's "INSERT INTO t2(a,b) SELECT key,
// value FROM json_each(NEW.x)" into "no such table: NEW" for a statement the
// oracle answers. Caught by compat-harness/trigger_r35b_outer_row_test.go, not
// by any assertion written for this batch -- which is the argument for keeping
// a differential suite whose failing set is compared against the parent commit
// rather than merely "still green".
//
// A leading (statement-level) WITH on a body statement is in the list for the
// OTHER reason recorded at the guard -- the run-time lowering pushes no CTE
// scope, and select.c:6028/6036 makes a CTE shadow a same-named table
// unconditionally, so re-lowering without the scope would silently read the
// TABLE. MEASURED while writing this: it never gets that far, because the
// PARSER refuses the shape ("expected SELECT, got INSERT") and so does the
// 3.53.3 oracle, at CREATE TRIGGER. The case is kept, and logs rather than
// asserts, so that a parser which later accepts it lands here instead of
// silently taking the live route.
func TestLiveReadDeclinesWhatItCannotReLower(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
	}{
		{"a statement-level WITH on a body INSERT",
			[]string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`, `CREATE TABLE log(n)`,
				`CREATE TRIGGER tb AFTER INSERT ON t BEGIN WITH c AS (SELECT x FROM s) INSERT INTO log SELECT x FROM c; END`},
			`INSERT INTO t VALUES(1)`},

		// The three TABLE-VALUED FUNCTION cases that stood here are PROMOTED --
		// they are the last three entries of liveReadShapes above, with the
		// oracle evidence this list's own message demanded. What closed them is
		// derivedSource.vtabTrig (vdbe_op.go): the run-time argument fold is
		// handed the trigger context the source was resolved under, so
		// "json_each(NEW.x)" compiles to an OpParam read of the firing row
		// instead of failing to resolve. See programNeedsRowContext's own doc
		// comment (vdbe_live_read.go) for what is left of that predicate.
	}
	for _, tc := range cases {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("[%s] create: %v", tc.name, err)
		}
		bad := false
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				// A body shape the PARSER refuses is a legitimate outcome too
				// (the statement-level WITH is one -- C SQLite rejects it at
				// CREATE TRIGGER, verified: the oracle errors on the INSERT).
				t.Logf("[%s] setup %q declined: %v", tc.name, s, err)
				bad = true
				break
			}
		}
		if !bad {
			_, cerr := db.compileWrite(tc.stmt)
			switch {
			case cerr != nil:
				t.Logf("[%s] compile error (the expected outcome): %v", tc.name, cerr)
			default:
				t.Errorf("[%s] %q COMPILED. The live re-lowering has no enclosing compiler and no CTE\n"+
					"scope, so it cannot reproduce this binding -- serving it means the two lowerings\n"+
					"disagree. If this is now genuinely reproducible, settle it against the oracle\n"+
					"first and move the case into liveReadShapes.", tc.name, tc.stmt)
			}
		}
		db.Discard()
	}
}

// forEachTriggerSubProgram calls visit for every trigger WHEN guard and body
// sub-program reachable from prog, at any nesting depth. seen guards the cycle
// a self-firing trigger graph compiles to (see walkFirePlans, which does the
// same for the whole program rather than the guards and bodies alone).
func forEachTriggerSubProgram(prog *Program, visit func(kind string, p *Program)) {
	seenProg := map[*Program]bool{}
	seenTrig := map[*compiledTrigger]bool{}
	var walk func(p *Program)
	walk = func(p *Program) {
		if p == nil || seenProg[p] {
			return
		}
		seenProg[p] = true
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
				if ct.when != nil {
					visit("trigger WHEN guard", ct.when)
					walk(ct.when)
				}
				for _, b := range ct.body {
					visit("trigger body statement", b)
					walk(b)
				}
			}
		}
	}
	walk(prog)
}

// onlyTriggerBody returns the single trigger body sub-program in prog's graph,
// failing the test if there is not exactly one.
func onlyTriggerBody(t *testing.T, prog *Program) *Program {
	t.Helper()
	var found []*Program
	forEachTriggerSubProgram(prog, func(kind string, p *Program) {
		if kind == "trigger body statement" {
			found = append(found, p)
		}
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly one compiled trigger body, found %d", len(found))
	}
	return found[0]
}

// queryInts runs a read against db's current (uncommitted) state and returns
// the first column of every row as an int64, in row order.
func queryInts(t *testing.T, db *Session, sqlText string) []int64 {
	t.Helper()
	pg, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := pg.Query(sqlText)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r[0].I)
	}
	return out
}

// TestProgramReadsFrozenSnapshotFailsClosed is a CONTRACT test on the check
// itself, written because a mutation test found it untestable any other way.
//
// programReadsFrozenSnapshot lists the subquery opcodes explicitly, and each
// arm treats "not a payload I recognise, or one with no LiveSource" as FROZEN.
// Every one of those arms is dead against the live compiler as it stands: a
// body compiled with compiler.liveDB set routes EVERY subquery through
// liveSubSelect, so a frozen one cannot be built. Deleting all four arms
// therefore changes no observable behaviour today -- measured, by deleting them
// and watching the whole shape battery above still pass.
//
// That is precisely why they need their own test. The arms exist so that a
// future emitter which forgets liveSubSelect, or a P4 payload type nobody
// added to the list, DECLINES rather than silently reading the firing
// statement's snapshot -- the exact failure this batch exists to remove. A
// guard whose only justification is the future is worth nothing unless
// something asserts its contract now.
//
// # The "frozen" fixture used to be an EMPTY program, and that was stale
//
// It was `&Program{}` -- which this table's own first case already asserts is
// NOT frozen, because an empty program reads nothing at all. The arms passed it
// only because they treated "no LiveSource" as proof of an image rather than
// asking whether there was one, and that cost a real capability: a body
// statement's upsert whose SET holds "(SELECT 7 LIMIT new.a)" compiles to a
// sub-program with no cursor, no derived source and no WritePager, reads
// nothing but the machine's own trigger row, and was refused as though it held
// the firing statement's snapshot. The arms RECURSE now, applying this same
// test one level in, so the fixture has to be a program that genuinely holds an
// image -- and the four "frozen" rows below still pin exactly what they always
// pinned, because an OpOpenRead sub-program is refused at any depth.
func TestProgramReadsFrozenSnapshotFailsClosed(t *testing.T) {
	live := &Program{LiveSource: &SelectStmt{}}
	// A sub-program that genuinely reads through a compile-time pager: a b-tree
	// cursor rooted in the compiled image.
	frozen := &Program{Insns: []Instruction{{Op: OpOpenRead}}}
	// One that holds no image at all -- neither live nor frozen, because there
	// is nothing for it to have frozen. See this function's doc comment.
	imageless := &Program{Insns: []Instruction{{Op: OpParam, P1: 1}, {Op: OpResultRow}}}
	cases := []struct {
		name string
		prog *Program
		want bool
	}{
		{"empty program", &Program{}, false},
		{"a WritePager of its own", &Program{WritePager: &ReadOnlyPager{}}, true},
		{"OpOpenRead (a b-tree cursor rooted in the compiled image)",
			&Program{Insns: []Instruction{{Op: OpOpenRead}}}, true},

		{"OpSubquery, live", &Program{Insns: []Instruction{{Op: OpSubquery, P4: live}}}, false},
		{"OpSubquery, frozen", &Program{Insns: []Instruction{{Op: OpSubquery, P4: frozen}}}, true},
		{"OpSubquery, unrecognised payload", &Program{Insns: []Instruction{{Op: OpSubquery, P4: 42}}}, true},

		{"OpExists, live", &Program{Insns: []Instruction{{Op: OpExists, P4: live}}}, false},
		{"OpExists, frozen", &Program{Insns: []Instruction{{Op: OpExists, P4: frozen}}}, true},

		{"OpInSub, live", &Program{Insns: []Instruction{{Op: OpInSub, P4: &inSubPlan{prog: live}}}}, false},
		{"OpInSub, frozen", &Program{Insns: []Instruction{{Op: OpInSub, P4: &inSubPlan{prog: frozen}}}}, true},
		{"OpInSub, no program at all", &Program{Insns: []Instruction{{Op: OpInSub, P4: &inSubPlan{}}}}, true},

		{"OpRowSub, live", &Program{Insns: []Instruction{{Op: OpRowSub, P4: &rowSubPlan{prog: live}}}}, false},
		{"OpRowSub, frozen", &Program{Insns: []Instruction{{Op: OpRowSub, P4: &rowSubPlan{prog: frozen}}}}, true},

		{"OpOpenDerived, live", &Program{Insns: []Instruction{{Op: OpOpenDerived, P4: &derivedSource{prog: live}}}}, false},
		{"OpOpenDerived, frozen", &Program{Insns: []Instruction{{Op: OpOpenDerived, P4: &derivedSource{prog: frozen}}}}, true},
		{"OpOpenDerived, a CTE/vtab source carrying no program", &Program{Insns: []Instruction{{Op: OpOpenDerived, P4: &derivedSource{}}}}, true},

		// One live read does not excuse a frozen one beside it.
		{"a live subquery AND a frozen one", &Program{Insns: []Instruction{
			{Op: OpSubquery, P4: live}, {Op: OpExists, P4: frozen}}}, true},

		// A sub-program with no LiveSource that holds NO IMAGE is not frozen:
		// there is nothing for it to have frozen. This is the promotion the
		// recursion makes -- see this function's doc comment -- and the rows
		// below it are what keep the recursion from becoming a hole.
		{"OpSubquery, imageless (no cursor, no derived source, no WritePager)",
			&Program{Insns: []Instruction{{Op: OpSubquery, P4: imageless}}}, false},
		{"OpInSub, imageless", &Program{Insns: []Instruction{
			{Op: OpInSub, P4: &inSubPlan{prog: imageless}}}}, false},
		{"OpRowSub, imageless", &Program{Insns: []Instruction{
			{Op: OpRowSub, P4: &rowSubPlan{prog: imageless}}}}, false},
		// ...but an image one level DEEPER is still an image. The recursion is
		// what finds it; without it this row reads as imageless.
		{"OpSubquery, imageless around a frozen one", &Program{Insns: []Instruction{
			{Op: OpSubquery, P4: &Program{Insns: []Instruction{{Op: OpSubquery, P4: frozen}}}}}}, true},
		{"OpSubquery, imageless around a WritePager", &Program{Insns: []Instruction{
			{Op: OpSubquery, P4: &Program{Insns: []Instruction{
				{Op: OpSubquery, P4: &Program{WritePager: &ReadOnlyPager{}}}}}}}}, true},
		// An OpOpenDerived is NOT recursed through: its rows come from a
		// materialization this compile owns, so a body carrying one keeps the
		// decline it has always had.
		{"OpOpenDerived, imageless body", &Program{Insns: []Instruction{
			{Op: OpOpenDerived, P4: &derivedSource{prog: imageless}}}}, true},
	}
	for _, tc := range cases {
		if got := programReadsFrozenSnapshot(tc.prog); got != tc.want {
			t.Errorf("[%s] programReadsFrozenSnapshot = %v, want %v.\n"+
				"This check is what stops a trigger guard or body from being served out of the FIRING\n"+
				"statement's compile-time image; every arm must fail CLOSED on anything it cannot\n"+
				"prove is re-lowered at run time (Program.LiveSource, vdbe_live_read.go).", tc.name, got, tc.want)
		}
	}
}
