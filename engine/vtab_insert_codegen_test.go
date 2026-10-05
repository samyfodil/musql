package engine

// Tests virtual table INSERT codegen for fts3/fts4, fts5, and rtree.
// Validates compilation, opcode generation, and correct answers.

import (
	"errors"
	"strings"
	"testing"
)

// vtabInsertShapes: virtual table INSERT statements.
var vtabInsertShapes = []conflictShapeCase{
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f VALUES('hello')`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, `INSERT INTO r VALUES(1,0.0,1.0)`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts3(x)`}, `INSERT INTO f VALUES('hello')`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`}, `INSERT INTO f VALUES('hello')`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree_i32(id,x0,x1)`}, `INSERT INTO r VALUES(1,0,1)`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`}, `INSERT INTO f VALUES('a','b'),('c','d'),('e','f')`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, `INSERT INTO r VALUES(1,0.0,1.0),(2,2.0,3.0)`},

	// An explicit column list, out of declaration order, and a partial one.
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`}, `INSERT INTO f(y,x) VALUES('b','a')`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`}, `INSERT INTO f(y) VALUES('b')`},

	// The rowid/docid spellings: a real hidden column (fts3's docid), the rowid
	// pseudo-column an INSERT's IDLIST may name (insert.c:1096), and fts5's
	// synthetic leading slot.
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f(docid,x) VALUES(7,'hello')`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f(rowid,x) VALUES(7,'hello')`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, `INSERT INTO r(rowid,id,x0,x1) VALUES(5,9,0.0,1.0)`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`}, `INSERT INTO f(rowid,x) VALUES(7,'hello')`},

	// Real EXPRESSIONS in the tuple, which is the whole point: each one is
	// coded into its tuple's register block as opcodes.
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f VALUES('a'||'b')`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f VALUES(upper('a')||CAST(2 AS TEXT))`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, `INSERT INTO r VALUES(1+1, abs(-2.5), 3.0*2)`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f VALUES(CASE WHEN 1 THEN 'y' ELSE 'n' END)`},

	// A bound parameter (OpVariable), and a connection-state function
	// (OpConnState) -- fts3e's own "VALUES(last_insert_rowid(),...)" idiom.
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f VALUES(?)`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f(docid,x) VALUES(last_insert_rowid()+1,'hello')`},

	// The fts3/fts4 and fts5 COMMAND CHANNEL: an IDLIST naming the table
	// itself. It maps to no declared slot at all, which is exactly why this
	// emitter hands the module the raw tuple rather than resolving slots here.
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `INSERT INTO f(f) VALUES('optimize')`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`, `INSERT INTO f VALUES('a')`}, `INSERT INTO f(f) VALUES('rebuild')`},

	// A schema-qualified target (the qualifier picks a catalog; delete.c:31-46's
	// sqlite3SrcListLookup is the same routine for a vtab).
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO main.f VALUES('hello')`},

	// An OR clause the module itself can express -- IGNORE for any vtab
	// (vdbe.c:8750), REPLACE for fts5 (fts5_main.c's fts5UpdateMethod). Both
	// reach insertIntoVtab's own gate unchanged; the emitter only codes values.
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT OR IGNORE INTO f VALUES('hello')`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`}, `INSERT OR REPLACE INTO f(rowid,x) VALUES(1,'hello')`},

	// The vtab INSERT is itself a TRIGGER BODY statement, so its tuple names
	// NEW.* -- resolved by OpParam, which reads the firing statement's own
	// NEW/OLD row registers.
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
		`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO f VALUES(new.a); END`},
		`INSERT INTO t VALUES('hello')`},
}

// TestVtabInsertCompilesToBytecode is the RULE #1 assertion no differential
// harness can make: every shape above must LOWER. The trigger-body case covers
// the sub-program route, where the vtab INSERT is compiled as the trigger's
// body rather than as the top-level statement. A behavioural test cannot stand
// in for this one, because insertIntoVtab answers correctly either way.
func TestVtabInsertCompilesToBytecode(t *testing.T) {
	for i, tc := range vtabInsertShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestVtabInsertEmitsAVInsertOpcode is the non-vacuity half, and it pins the
// register block itself.
//
// "It compiles" is not the property that matters: an emitter that produced
// Init/Halt and nothing else would pass the test above while inserting nothing
// at all. This asserts that each shape emits exactly one OpVInsert carrying a
// real *vtabInsertPlan, that its P2*P3 covers the statement's whole VALUES
// grid, and that EVERY register in that grid is the destination of an
// instruction that precedes the opcode.
//
// The last of those is the assertion a behavioural test cannot make, and the
// reason is the same one TestViewInsertFillsEveryNewRowRegister states: a
// register nothing wrote reads as NULL (a fresh register's zero Value), so an
// emitter that silently skipped a tuple slot would insert a NULL and only be
// caught by whichever behavioural case happened to look at that column. Only
// OpSCopy ever writes into the block (compileVtabInsertStmt emits nothing else
// into it), and it carries its destination in P2.
func TestVtabInsertEmitsAVInsertOpcode(t *testing.T) {
	for i, tc := range vtabInsertShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		found := 0
		walkFirePlans(prog, func(p *Program) {
			for j := range p.Insns {
				if p.Insns[j].Op != OpVInsert {
					continue
				}
				found++
				in := p.Insns[j]
				plan, ok := in.P4.(*vtabInsertPlan)
				if !ok || plan == nil || plan.vm == nil || plan.stmt == nil {
					t.Errorf("[%d] %q: OpVInsert with no usable plan (%T)", i, tc.stmt, in.P4)
					continue
				}
				if in.P2 <= 0 || in.P3 <= 0 {
					t.Errorf("[%d] %q: OpVInsert covers %d tuples of %d values -- it would hand the module nothing",
						i, tc.stmt, in.P3, in.P2)
					continue
				}
				if got, want := in.P3, len(plan.stmt.rows); got != want {
					t.Errorf("[%d] %q: OpVInsert P3=%d but the statement has %d VALUES tuples",
						i, tc.stmt, got, want)
				}
				written := map[int]bool{}
				for k := 0; k < j; k++ {
					if p.Insns[k].Op == OpSCopy {
						written[p.Insns[k].P2] = true
					}
				}
				for r := 0; r < in.P3; r++ {
					for v := 0; v < in.P2; v++ {
						reg := in.P1 + r*in.P2 + v
						if !written[reg] {
							t.Errorf("[%d] %q: register %d (tuple %d, value %d) is never written before the\n"+
								"OpVInsert at %d. It would READ as NULL, because a fresh register's zero Value is\n"+
								"NULL -- which is exactly why no behavioural test catches this.",
								i, tc.stmt, reg, r, v, j)
						}
					}
				}
			}
		})
		if found != 1 {
			t.Errorf("[%d] %q: compiled to a program with %d OpVInsert instructions, want exactly 1.\n"+
				"Zero means the statement inserts nothing; more than one means the per-statement\n"+
				"batching vdbe_vtab_write.go's deviation (2) depends on has been split.", i, tc.stmt, found)
		}
	}
}

// vtabInsertRowSourceShapes: the two row sources that are NOT a VALUES grid --
// DEFAULT VALUES and a source SELECT. They are kept out of vtabInsertShapes
// because TestVtabInsertEmitsAVInsertOpcode's assertions are about that grid
// (P2*P3 covering it, P3 == len(stmt.rows), every register SCopy'd), none of
// which describes either shape: DEFAULT VALUES codes NO register at all (the
// module expands it, vtabInsertRowValues) and a SELECT source fills ONE row's
// block per iteration of a scan.
var vtabInsertRowSourceShapes = []struct {
	sel bool // true when the row source is a SELECT (P3 == -1)
	tc  conflictShapeCase
}{
	// The census shape, verbatim from vdbe_total_test.go's residual list.
	{false, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f DEFAULT VALUES`}},
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`},
		`INSERT INTO f SELECT x FROM s`}},

	// DEFAULT VALUES across the writable module set.
	{false, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts3(x,y)`}, `INSERT INTO f DEFAULT VALUES`}},
	{false, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts5(x,y)`}, `INSERT INTO f DEFAULT VALUES`}},
	{false, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, `INSERT INTO r DEFAULT VALUES`}},

	// A SELECT source across the module set, with an explicit column list, a
	// compound source, a source that reads the target itself (which the
	// materialization is what makes terminate), and an EMPTY source.
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts3(x)`, `CREATE TABLE s(x)`},
		`INSERT INTO f SELECT x FROM s`}},
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`, `CREATE TABLE s(x)`},
		`INSERT INTO f SELECT x FROM s`}},
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE s(id,x0,x1)`},
		`INSERT INTO r SELECT id,x0,x1 FROM s`}},
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO f(y,x) SELECT a,b FROM s`}},
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`},
		`INSERT INTO f SELECT x FROM s UNION ALL SELECT x FROM s`}},
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f SELECT x FROM f`}},
	{true, conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`},
		`INSERT INTO f SELECT x FROM s WHERE 0`}},
}

// TestVtabInsertRowSourceCompilesToBytecode is the RULE #1 assertion for the
// two non-VALUES row sources.
func TestVtabInsertRowSourceCompilesToBytecode(t *testing.T) {
	for i, d := range vtabInsertRowSourceShapes {
		if _, err := compileShape(t, d.tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, d.tc.stmt, err)
		}
	}
}

// TestVtabInsertRowSourceIsNotVacuous is the assertion no differential test can
// make, and the one that matters most for the SELECT source.
//
// An emitter that handed the module ONE row -- with no cursor, no loop --
// answers a one-row source correctly and passes every behavioural case anyone
// naturally writes. So this pins the LOOP STRUCTURE off the emitted program:
// a derived source is opened, an OpVInsertRow sits inside an OpRewind/OpNext
// pair over THAT cursor, its whole register block is fed by OpColumn off it,
// and the single OpVInsert that follows the loop is marked P3 == -1 (the
// "replay what was collected" form) and carries the source's arity in
// plan.srcWidth.
//
// For DEFAULT VALUES it pins the opposite: no scan, no collected row, and a
// plan whose statement really is the defaultValues one -- because the row this
// shape inserts is manufactured by the MODULE (vtabInsertRowValues), so an
// emitter that coded a register block here would be inventing a second answer.
//
// Program.WritePager is pinned for the SELECT source too: without it
// cachedWriteProgram reuses the program and a second run of the same SQL reads
// the FIRST run's frozen snapshot -- the live bug beginViewWriteScan's note
// records for the view twins, and the one compileInsertSelectWrite's own
// "INSERT INTO ev SELECT y FROM ev" note records for the table one.
func TestVtabInsertRowSourceIsNotVacuous(t *testing.T) {
	for i, d := range vtabInsertRowSourceShapes {
		prog, err := compileShape(t, d.tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, d.tc.stmt, err)
			continue
		}
		vins, opened, rewind, next, collect := -1, -1, -1, -1, -1
		cursor := -1
		for j := range prog.Insns {
			switch prog.Insns[j].Op {
			case OpVInsert:
				vins = j
			case OpVInsertRow:
				collect = j
			case OpOpenDerived:
				opened, cursor = j, prog.Insns[j].P1
			case OpRewind:
				rewind = j
			case OpNext:
				next = j
			}
		}
		if vins < 0 {
			t.Errorf("[%d] %q: no OpVInsert -- the statement hands the module nothing", i, d.tc.stmt)
			continue
		}
		plan, ok := prog.Insns[vins].P4.(*vtabInsertPlan)
		if !ok || plan == nil || plan.vm == nil || plan.stmt == nil {
			t.Errorf("[%d] %q: OpVInsert with no usable plan (%T)", i, d.tc.stmt, prog.Insns[vins].P4)
			continue
		}
		if !d.sel {
			if collect >= 0 || opened >= 0 {
				t.Errorf("[%d] %q: DEFAULT VALUES emitted a scan (open=%d collect=%d). The row it inserts is\n"+
					"the MODULE's (vtabInsertRowValues); coding one here would be a second answer.",
					i, d.tc.stmt, opened, collect)
			}
			if !plan.stmt.defaultValues || plan.srcWidth != -1 {
				t.Errorf("[%d] %q: plan says defaultValues=%v srcWidth=%d, want true/-1",
					i, d.tc.stmt, plan.stmt.defaultValues, plan.srcWidth)
			}
			continue
		}
		if prog.WritePager == nil {
			t.Errorf("[%d] %q: the program declares no WritePager, so cachedWriteProgram will REUSE it\n"+
				"and a second run replays this compile's frozen snapshot.", i, d.tc.stmt)
		}
		if prog.Insns[vins].P3 != -1 {
			t.Errorf("[%d] %q: OpVInsert P3=%d, want -1 (replay the collected rows)", i, d.tc.stmt, prog.Insns[vins].P3)
		}
		if plan.srcWidth <= 0 {
			t.Errorf("[%d] %q: plan.srcWidth=%d; the source arity is what catches a wrong-width SELECT\n"+
				"that yields no rows at all", i, d.tc.stmt, plan.srcWidth)
		}
		if opened < 0 || collect < 0 || rewind < 0 || next < 0 {
			t.Errorf("[%d] %q: open=%d collect=%d rewind=%d next=%d -- the source is not being SCANNED.\n"+
				"An emitter that collected one row answers a one-row source correctly and would\n"+
				"otherwise pass every test here.", i, d.tc.stmt, opened, collect, rewind, next)
			continue
		}
		if !(opened < rewind && rewind < collect && collect < next && next < vins) {
			t.Errorf("[%d] %q: instruction order is open=%d rewind=%d collect=%d next=%d vinsert=%d;\n"+
				"the collect must sit INSIDE the loop and the OpVInsert AFTER it",
				i, d.tc.stmt, opened, rewind, collect, next, vins)
		}
		if prog.Insns[rewind].P1 != cursor || prog.Insns[next].P1 != cursor {
			t.Errorf("[%d] %q: the loop drives cursor %d/%d, not the derived source's cursor %d",
				i, d.tc.stmt, prog.Insns[rewind].P1, prog.Insns[next].P1, cursor)
		}
		if got := prog.Insns[collect].P2; got != plan.srcWidth {
			t.Errorf("[%d] %q: OpVInsertRow collects %d values but the source is %d wide",
				i, d.tc.stmt, got, plan.srcWidth)
		}
		// Every register the collect reads must be the DESTINATION of an
		// OpColumn off the source cursor. A register nothing wrote reads as
		// NULL (a fresh register's zero Value), so a skipped column would store
		// a silent NULL -- the failure mode TestVtabInsertEmitsAVInsertOpcode
		// states for the VALUES grid.
		for v := 0; v < prog.Insns[collect].P2; v++ {
			reg := prog.Insns[collect].P1 + v
			ok := false
			for k := rewind; k < collect; k++ {
				if prog.Insns[k].Op == OpColumn && prog.Insns[k].P1 == cursor && prog.Insns[k].P3 == reg {
					ok = true
				}
			}
			if !ok {
				t.Errorf("[%d] %q: register %d (source column %d) is never read off cursor %d inside the\n"+
					"loop. It would be collected as NULL.", i, d.tc.stmt, reg, v, cursor)
			}
		}
	}
}

// TestInsertRowSourceProgramIsNotCachedAcrossAChangedSource is the assertion
// Program.WritePager exists for, and it is a WRONG-ANSWER gate rather than a
// staleness nicety.
//
// Both new row-source emitters (emitVtabInsertSelect, and emitViewInsertSelect
// in vdbe_view_write.go) compile their source SELECT against a snapshot taken
// at COMPILE time. cachedWriteProgram keys the write-plan cache on the SQL
// text plus db.schemaGen/db.txGen, none of which moves when rows change -- so
// running the same statement twice would replay the FIRST compile's image.
// Declaring WritePager is what makes cachedWriteProgram refuse to cache it.
//
// Delete that field from either emitter and this test fails while every
// compile-level and single-run behavioural test in this file still passes,
// which is exactly the shape of the live bug 'beginViewWriteScan' records for
// the view UPDATE/DELETE twins.
func TestInsertRowSourceProgramIsNotCachedAcrossAChangedSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		{"vtab", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`,
			`INSERT INTO s VALUES('a')`},
			`INSERT INTO f SELECT x FROM s`, `SELECT x FROM f ORDER BY docid`,
			[]string{"a", "a", "b"}},
		{"view", []string{`CREATE TABLE b(a)`, `CREATE TABLE s(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`INSERT INTO s VALUES('a')`},
			`INSERT INTO v SELECT a FROM s`, `SELECT a FROM b ORDER BY rowid`,
			[]string{"a", "a", "b"}},
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
			if err := db.Exec(tc.stmt); err != nil {
				t.Fatalf("first %q: %v", tc.stmt, err)
			}
			// A source row neither the first compile nor the first run could
			// have seen. No DDL and no rollback, so schemaGen and txGen have
			// not moved and a cached program WOULD be reused.
			if err := db.Exec(`INSERT INTO s VALUES('b')`); err != nil {
				t.Fatalf("insert: %v", err)
			}
			if err := db.Exec(tc.stmt); err != nil {
				t.Fatalf("second %q: %v", tc.stmt, err)
			}
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v, want %v.\n"+
					"An INSERT ... SELECT program that carries a compile-time source snapshot must not be\n"+
					"CACHED -- the second run would replay the first run's image.", got, tc.want)
			}
		})
	}
}

// vtabInsertDeclined: shapes this compiler deliberately does NOT model. Each
// must decline CLEANLY -- half-serving one here would be a wrong answer rather
// than a gap. See compileVtabInsertStmt's own list for the reason attached to
// each.
var vtabInsertDeclined = []struct {
	why string
	tc  conflictShapeCase
}{
	// "a RETURNING column carrying a table-reading SUBQUERY" WAS here and is
	// LOWERED now: compileSelfRowExprPaged takes a pager, so the column's
	// program can open the cursor that entry said it could not, and eval sizes
	// the pooled machine's cursors from the program.
	//
	// The entry's stated worry -- that the shape needs a PER-ROW snapshot
	// which a compiled program could not reproduce -- does not apply:
	// the pager handed to the compile only RESOLVES the subquery, while
	// selfRowExpr.eval takes its run-time pager from the per-row evalCtx
	// (vdbe_run.go's "m.pager = ctx.pager"), so the reads stay live. Verified
	// against the oracle: "INSERT INTO t1 VALUES(1,2,3),(2,4,5) RETURNING
	// (SELECT count(*) FROM t1)" answers 0 then 1 on BOTH engines.
	{"a RETURNING clause the PLAN BUILDER rejects (\"no such column\", with its own prepare-failure bookkeeping)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			`INSERT INTO r VALUES(1,0.0,1.0) RETURNING nosuchcol`}},
	{"a SELECT source with RETURNING (insertIntoVtab refuses the combination outright and owns the wording)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE s(a,b,c)`},
			`INSERT INTO r SELECT a,b,c FROM s RETURNING id`}},
	{"a TRIGGER BODY's INSERT ... SELECT (its source must be read as of the FIRING, and may name NEW)",
		conflictShapeCase{[]string{`CREATE TABLE t(k)`, `CREATE TABLE s(z)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
			`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO f SELECT z FROM s; END`},
			`INSERT INTO t VALUES(1)`}},
	{"an UPSERT (insert.c:1291-1293's PREPARE-time \"UPSERT not implemented for virtual table\")",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f VALUES('a') ON CONFLICT DO NOTHING`}},
	// "a subquery inside a VALUES tuple" WAS here and is LOWERED now (ded1f81):
	// the emitter takes a writeSubqueryPager when a tuple holds one, which is
	// the pager this entry's own reason said neither route installed.
	{"a leading WITH clause (its CTE scope is not pushed for this compile)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`WITH c(z) AS (VALUES('a')) INSERT INTO f VALUES('b')`}},
	{"ragged VALUES tuples (the tuples must agree with each other before anything is coded)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`},
			`INSERT INTO f VALUES('a','b'),('c')`}},
}

// TestVtabInsertDeclinedShapesDeclineCleanly pins the OTHER half of "never
// wrong": a shape this emitter does not model must decline CLEANLY, not be
// approximated. errVDBEUnsupported is required EXACTLY, with no "any error will
// do" escape hatch, because a decline that mutated into some other failure
// would be a behaviour change and the loose form would not see it.
//
// It is also what makes the guard list in compileVtabInsertStmt testable at all
// -- two of its terms are deliberately REDUNDANT (see that function's comment),
// and this is where a future edit that makes one of them load-bearing gets
// checked.
func TestVtabInsertDeclinedShapesDeclineCleanly(t *testing.T) {
	for _, d := range vtabInsertDeclined {
		prog, err := compileShape(t, d.tc)
		if err != nil {
			if !errors.Is(err, errVDBEUnsupported) {
				t.Errorf("%q declined with %v, which is not errVDBEUnsupported.\n"+
					"A shape this emitter does not model must decline CLEANLY: %s.", d.tc.stmt, err, d.why)
			}
			continue
		}
		if prog != nil {
			t.Errorf("%q COMPILED, but this batch does not model it: %s.\n"+
				"Serving it here without the semantics that shape needs is a WRONG ANSWER,\n"+
				"not a promotion (AGENTS.md invariant 1).", d.tc.stmt, d.why)
		}
	}
}

// TestVtabInsertArityIsStillChecked closes the one hole the compiled route
// opens by construction, and it is a NEVER-WRONG hole rather than a cosmetic
// one.
//
// compileVtabInsertStmt only requires the tuples to agree with EACH OTHER; how
// many values the TARGET wants is resolved per module, at run time
// (resolveVtabInsertColumns / fts3InsertTargets), so "INSERT INTO f(x,y)
// VALUES('a')" compiles happily and the mismatch is caught by
// vtabInsertRowValues' own arity check on its pre branch. Delete that check and
// nothing panics and nothing errors: insertIntoVtab's "for i, v := range vals"
// simply stops early and stores a SHORT row -- a silently wrong write, which is
// the exact failure mode AGENTS.md invariant 1 names. Measured as a mutation:
// with the check removed, every other test in this file still passed.
func TestVtabInsertArityIsStillChecked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  string
	}{
		{"fts4, too few values for the column list", []string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`},
			`INSERT INTO f(x,y) VALUES('a')`, `engine: table f has 2 columns but 1 values were supplied`},
		{"fts4, too many values", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f VALUES('a','b')`, `engine: table f has 1 columns but 2 values were supplied`},
		{"rtree, too few values", []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			`INSERT INTO r VALUES(1,0.0)`, `engine: table r has 3 columns but 2 values were supplied`},
		{"fts5, too many values", []string{`CREATE VIRTUAL TABLE f USING fts5(x)`},
			`INSERT INTO f VALUES('a','b')`, `engine: table f has 1 columns but 2 values were supplied`},

		// The SELECT source's own arity, and the case the PER-ROW check cannot
		// see: a source that yields NO rows. C SQLite reports the mismatch
		// either way, because it checks nColumn at PREPARE time
		// (insert.c:1154 feeding :1249-1258) -- verified against the 3.53.3
		// oracle over an EMPTY s, which still says "table f has 1 columns but 2
		// values were supplied". vtabInsertPlan.srcWidth is what carries it;
		// delete that check and the empty case SUCCEEDS silently, storing
		// nothing where SQLite errors, and every other test in this file still
		// passes. Measured as a mutation.
		{"fts4 SELECT source, too many columns", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`,
			`CREATE TABLE s(a)`, `INSERT INTO s VALUES('q')`},
			`INSERT INTO f SELECT a,a FROM s`, `engine: table f has 1 columns but 2 values were supplied`},
		{"fts4 SELECT source, too many columns, EMPTY source", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`,
			`CREATE TABLE s(a)`},
			`INSERT INTO f SELECT a,a FROM s`, `engine: table f has 1 columns but 2 values were supplied`},
		{"rtree SELECT source, too few columns, EMPTY source",
			[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE s(a,b)`},
			`INSERT INTO r SELECT a,b FROM s`, `engine: table r has 3 columns but 2 values were supplied`},
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
				t.Fatalf("compile %q: %v -- this case exists to exercise the RUN-TIME arity check", tc.stmt, cerr)
			}
			err = db.Exec(tc.stmt)
			if err == nil {
				t.Fatalf("%q SUCCEEDED. A wrong-arity vtab INSERT must error, not store a short row.", tc.stmt)
			}
			if err.Error() != tc.want {
				t.Fatalf("%q: got %q, want %q (the established wording, which must not drift)",
					tc.stmt, err.Error(), tc.want)
			}
		})
	}
}

// TestVtabInsertCompiledAnswers is the behavioural half: the compiled route
// must answer exactly what the 3.53.3 oracle does. Every expectation here was
// measured against it when insertIntoVtab / insertIntoFts3 were written -- see
// their doc comments for the measurements.
//
// Each case first asserts the statement really compiled, so a decline shows up
// as itself rather than as a confusing exec failure further down.
func TestVtabInsertCompiledAnswers(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		{"fts4 one row", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f VALUES('hello world')`, `SELECT docid,x FROM f`, []string{"1,hello world"}},
		{"fts4 multi-row, ascending docids", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f VALUES('a'),('b'),('c')`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,a", "2,b", "3,c"}},
		{"fts4 explicit docid, then auto", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f(docid,x) VALUES(5,'a'),(NULL,'b')`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"5,a", "6,b"}},
		{"fts4 named out of order", []string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`},
			`INSERT INTO f(y,x) VALUES('B','A')`, `SELECT x,y FROM f`, []string{"A,B"}},
		{"fts4 partial column list leaves the rest NULL", []string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`},
			`INSERT INTO f(y) VALUES('B')`, `SELECT x IS NULL, y FROM f`, []string{"1,B"}},
		{"fts4 MATCH sees the compiled value", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f VALUES('alpha'||' '||'beta')`, `SELECT docid FROM f WHERE f MATCH 'beta'`,
			[]string{"1"}},
		{"fts4 expression tuple", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f VALUES(upper('ab')||CAST(2 AS TEXT))`, `SELECT x FROM f`, []string{"AB2"}},

		// rtree COERCES every coordinate to REAL inside the module
		// (coerceCoords, vtab_rtree.go), so the STORED value is not the
		// register's own -- which is why a RETURNING clause, reading the
		// candidate registers, can disagree with a later SELECT (see
		// vtabCandidateRow). Pinned here because a compiled emitter that
		// "helpfully" applied an affinity of its own would still pass every
		// count-based check.
		{"rtree stores REAL coordinates", []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			`INSERT INTO r VALUES(1,0,10)`, `SELECT id, typeof(x0), typeof(x1), x0, x1 FROM r`,
			[]string{"1,real,real,0,10"}},
		{"rtree id is the rowid, and the rowid pseudo-column is discarded",
			[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			`INSERT INTO r(rowid,id,x0,x1) VALUES(5,9,0.0,1.0)`, `SELECT rowid,id FROM r`, []string{"9,9"}},
		{"rtree multi-row", []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			`INSERT INTO r VALUES(1,0.0,1.0),(2,2.0,3.0)`, `SELECT id FROM r ORDER BY id`,
			[]string{"1", "2"}},

		{"fts5 one row", []string{`CREATE VIRTUAL TABLE f USING fts5(x)`},
			`INSERT INTO f VALUES('hello')`, `SELECT rowid,x FROM f`, []string{"1,hello"}},
		{"fts5 explicit rowid", []string{`CREATE VIRTUAL TABLE f USING fts5(x)`},
			`INSERT INTO f(rowid,x) VALUES(4,'hello')`, `SELECT rowid,x FROM f`, []string{"4,hello"}},

		// last_insert_rowid() inside a tuple reads the PRE-statement value in
		// EVERY tuple, because all tuples are coded before the single
		// OpVInsert -- the whole tuple set is built before any row is stored.
		// Coding per tuple and inserting between them would answer 1 then 2
		// here.
		{"every tuple sees the pre-statement last_insert_rowid()",
			[]string{`CREATE TABLE seed(a)`, `INSERT INTO seed VALUES(1)`, `CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f(docid,x) VALUES(last_insert_rowid()+10,'a'),(last_insert_rowid()+20,'b')`,
			`SELECT docid FROM f ORDER BY docid`, []string{"11", "21"}},

		// A trigger body's vtab INSERT, whose tuple names NEW.* -- resolved by
		// OpParam off the firing statement's own NEW/OLD row registers.
		{"trigger body INSERT into an fts4 table",
			[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
				`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO f VALUES(new.a); END`},
			`INSERT INTO t VALUES('hello')`, `SELECT x FROM f`, []string{"hello"}},

		// ---- the ROW-SOURCE shapes. Every expectation below was measured
		// against the 3.53.3 oracle directly (the default build for fts3/fts4/
		// rtree, "-tags sqlite_fts5" for fts5), and in each DEFAULT VALUES case
		// the oracle's answer is IDENTICAL to the explicit all-NULL VALUES
		// tuple -- which is what vtabInsertRowValues' defaultValues arm makes
		// it.
		{"fts4 DEFAULT VALUES", []string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`},
			`INSERT INTO f DEFAULT VALUES`, `SELECT docid, x IS NULL, y IS NULL FROM f`, []string{"1,1,1"}},
		{"fts5 DEFAULT VALUES", []string{`CREATE VIRTUAL TABLE f USING fts5(x,y)`},
			`INSERT INTO f DEFAULT VALUES`, `SELECT rowid, x IS NULL, y IS NULL FROM f`, []string{"1,1,1"}},
		// rtree's own coercion turns the NULL coordinates into 0.0 and the NULL
		// id into the auto-assigned rowid -- byte-identical to
		// "INSERT INTO r VALUES(NULL,NULL,NULL)" on the oracle.
		{"rtree DEFAULT VALUES", []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			`INSERT INTO r DEFAULT VALUES`, `SELECT rowid,id,x0,x1 FROM r`, []string{"1,1,0,0"}},

		// The SCAN: three source rows must become three stored rows. An emitter
		// that collected one answers a one-row source correctly.
		{"fts4 SELECT source", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`,
			`INSERT INTO s VALUES('a'),('b'),('c')`},
			`INSERT INTO f SELECT x FROM s`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,a", "2,b", "3,c"}},
		// The full-text index is built from the SCANNED values, not just the
		// %_content row -- one segment for the whole statement, which is the
		// reason OpVInsert is one instruction (vdbe_vtab_write.go's deviation 2).
		{"fts4 SELECT source is indexed", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`,
			`INSERT INTO s VALUES('alpha beta'),('gamma')`},
			`INSERT INTO f SELECT x FROM s`, `SELECT docid FROM f WHERE f MATCH 'beta'`, []string{"1"}},
		{"fts4 SELECT source with a column list", []string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`,
			`CREATE TABLE s(a,b)`, `INSERT INTO s VALUES('A','B')`},
			`INSERT INTO f(y,x) SELECT a,b FROM s`, `SELECT x,y FROM f`, []string{"B,A"}},
		// The source is MATERIALIZED before the module is called
		// (insert.c:1167-1193's template 4), so a self-sourced INSERT sees only
		// the pre-statement rows and terminates.
		{"fts3 SELECT source reads the target itself", []string{`CREATE VIRTUAL TABLE h USING fts3(w)`,
			`INSERT INTO h VALUES('a'),('b')`},
			`INSERT INTO h SELECT w FROM h`, `SELECT docid,w FROM h ORDER BY docid`,
			[]string{"1,a", "2,b", "3,a", "4,b"}},
		{"rtree SELECT source", []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`CREATE TABLE s(id,x0,x1)`, `INSERT INTO s VALUES(1,0.0,1.0),(2,2.0,3.0)`},
			`INSERT INTO r SELECT id,x0,x1 FROM s`, `SELECT id,x0,x1 FROM r ORDER BY id`,
			[]string{"1,0,1", "2,2,3"}},
		{"fts5 SELECT source", []string{`CREATE VIRTUAL TABLE f USING fts5(x,y)`, `CREATE TABLE s(a,b)`,
			`INSERT INTO s VALUES('one','uno'),('two','dos')`},
			`INSERT INTO f SELECT a,b FROM s`, `SELECT rowid,x,y FROM f ORDER BY rowid`,
			[]string{"1,one,uno", "2,two,dos"}},
		// An EMPTY source stores nothing, and the module is still called with a
		// NON-NIL empty row list -- nil would mean "evaluate the row source
		// yourself", which for a SELECT-sourced statement finds no VALUES tuple
		// at all.
		{"fts4 SELECT source with no rows", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE s(x)`},
			`INSERT INTO f SELECT x FROM s`, `SELECT count(*) FROM f`, []string{"0"}},
		// The fts3/fts4 COMMAND CHANNEL from a SELECT source: the tuple reaches
		// fts3CommandInsert as a value, exactly as it does from VALUES.
		{"fts4 command channel from a SELECT source",
			[]string{`CREATE VIRTUAL TABLE g USING fts4(z)`, `INSERT INTO g VALUES('hello world')`,
				`CREATE TABLE cmd(c)`, `INSERT INTO cmd VALUES('optimize')`},
			`INSERT INTO g(g) SELECT c FROM cmd`, `SELECT docid,z FROM g`, []string{"1,hello world"}},
	}
	for _, tc := range cases {
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

// TestVtabInsertCommandChannelStillRuns pins the shape whose IDLIST names the
// table itself -- fts3/fts4's and fts5's command channel, which stores no row
// and is why this emitter hands the module the raw tuple instead of resolving
// declared slots itself (vdbe_vtab_write.go, deviation 1). A compiler that
// tried to resolve "f" as a column would reject the statement outright.
func TestVtabInsertCommandChannelStillRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		module  string
		command string
	}{
		{"fts4 optimize", `fts4`, `INSERT INTO f(f) VALUES('optimize')`},
		{"fts5 rebuild", `fts5`, `INSERT INTO f(f) VALUES('rebuild')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range []string{
				`CREATE VIRTUAL TABLE f USING ` + tc.module + `(x)`,
				`INSERT INTO f VALUES('alpha')`,
				`INSERT INTO f VALUES('beta')`,
			} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.command); cerr != nil {
				t.Fatalf("compile %q: %v", tc.command, cerr)
			}
			if err := db.Exec(tc.command); err != nil {
				t.Fatalf("exec %q: %v", tc.command, err)
			}
			got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM f ORDER BY rowid`))
			if strings.Join(got, "|") != "alpha|beta" {
				t.Fatalf("after %q the table reads %v, want [alpha beta]", tc.command, got)
			}
		})
	}
}

// TestVtabInsertReachesTheFile is the DURABILITY assertion, and it is the one
// every "does it answer correctly" test above cannot make.
//
// compileVtabInsertStmt deliberately emits NO OpChangeCounter, because
// insertIntoVtab / insertIntoFts3 call db.markFileChanged() themselves and do
// it unconditionally -- stronger than OpChangeCounter, whose runWrite half is
// additionally gated on wc.wroteRows(). Since 4938066 doCommit short-circuits
// to success whenever commitIsNoOp() does, so a statement that marks nothing
// returns success, answers every in-session query correctly, and silently drops
// its rows at Close. That is the defect class 0a9bbc1 and 7a39a8c were both
// about; this is the only thing standing between it and this emitter.
//
// BOTH SPELLINGS, because they are two emitters. The VALUES tuples come from
// compileVtabInsertStmt and the SELECT rows from emitVtabInsertSelect, and
// neither emits an OpChangeCounter -- so a change that gave one of them a
// counter of its own, or that moved markFileChanged inside a branch, would
// leave the other silently dropping its rows at Close with every in-session
// assertion in this file still green.
func TestVtabInsertReachesTheFile(t *testing.T) {
	type formCase struct{ module, form string }
	var cases []formCase
	for _, module := range []string{`fts4`, `fts5`, `rtree`} {
		for _, form := range []string{`VALUES`, `SELECT`} {
			cases = append(cases, formCase{module, form})
		}
	}
	for _, fc := range cases {
		module, form := fc.module, fc.form
		t.Run(module+"/"+form, func(t *testing.T) {
			schema := []string{`CREATE VIRTUAL TABLE v USING ` + module + `(x)`}
			stmt := `INSERT INTO v VALUES('hello')`
			query := `SELECT x FROM v`
			want := "hello"
			if module == "rtree" {
				schema = []string{`CREATE VIRTUAL TABLE v USING rtree(id,x0,x1)`}
				stmt = `INSERT INTO v VALUES(1,0.0,1.0)`
				query = `SELECT id FROM v`
				want = "1"
			}
			if form == `SELECT` {
				// The source is seeded in the SCHEMA session, so the insert
				// session below still does exactly one thing.
				if module == "rtree" {
					schema = append(schema, `CREATE TABLE s(a,b,c)`, `INSERT INTO s VALUES(1,0.0,1.0)`)
					stmt = `INSERT INTO v SELECT a,b,c FROM s`
				} else {
					schema = append(schema, `CREATE TABLE s(a)`, `INSERT INTO s VALUES('hello')`)
					stmt = `INSERT INTO v SELECT a FROM s`
				}
			}
			path := t.TempDir() + "/vi.musq"
			db, err := Create(path)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			for _, s := range schema {
				if err := db.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close (schema): %v", err)
			}

			// A SECOND session, so the INSERT is the only thing it does -- with
			// the CREATE in the same session its own markFileChanged would mask
			// a missing one here.
			db2, err := OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			if _, cerr := db2.compileWrite(stmt); cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			if err := db2.Exec(stmt); err != nil {
				t.Fatalf("%q: %v", stmt, err)
			}
			if err := db2.Close(); err != nil {
				t.Fatalf("Close (insert): %v", err)
			}

			db3, err := OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (readback): %v", err)
			}
			defer db3.Discard()
			got := rvdRowStrings(rvdQuery(t, db3, query))
			if len(got) != 1 || got[0] != want {
				t.Fatalf("the compiled vtab INSERT's row did not survive Close: got %v, want [%s].\n"+
					"insertIntoVtab's own markFileChanged is what this emitter relies on in place of\n"+
					"OpChangeCounter -- without it commitIsNoOp throws the write away.", got, want)
			}
		})
	}
}

// TestVtabInsertPublishesRowCounters pins the two counters opVInsert has to
// publish by hand, because nothing else on this path does: wctx.rowsAffected,
// which ExecArgs reports and changes() reads, and wctx.rowsInserted (insert.c's
// regRowCount, which "PRAGMA count_changes" answers and changes() is not).
func TestVtabInsertPublishesRowCounters(t *testing.T) {
	db, err := Create(t.TempDir()+"/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{`CREATE VIRTUAL TABLE f USING fts4(x)`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	n, _, err := db.ExecArgs(`INSERT INTO f VALUES('a'),('b'),('c')`, nil)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n != 3 {
		t.Fatalf("ExecArgs reported %d rows affected, want 3 (opVInsert must set wctx.rowsAffected)", n)
	}
	if got := db.nChange; got != 3 {
		t.Fatalf("changes() is %d, want 3", got)
	}
	if got := db.RowsInserted(); got != 3 {
		t.Fatalf("PRAGMA count_changes' rows-inserted is %d, want 3 (opVInsert must set wctx.rowsInserted)", got)
	}
}

// TestVtabInsertTupleWalkAnswers pins the ANSWERS a VALUES tuple must produce
// on the way into a virtual table: order, parameters, connection state and
// first-error-wins.
//
// Every case here COMPILES: compileSelfRowExprPaged lowers the table-reading
// subquery each one carries in RETURNING. Those subquery columns stay -- they
// are answers worth checking on the compiled route, and rewriting them out
// would discard coverage to no purpose.
//
// The tuple walk is the write path's ONE shared expression-list boundary, so
// "it is obviously just a left-to-right loop, one value per expression,
// stopping at the first error" is an argument -- and an argument no test can
// distinguish from its own negation is what this project has repeatedly been
// caught by. Each case below is a separate way for that loop to be wrong:
//
//	tuple ORDER      a named column list puts the tuple out of declared order,
//	                 so a walk that lost the pairing shows up
//	evalCtx.params   the bound parameter has to reach the expression
//	evalCtx.db       a connection-state function has to find a session
//	FIRST ERROR      the second slot raises, and the row must not store
//
func TestVtabInsertTupleWalkAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   []string
		stmt    string
		args    []Value
		wantRow string // "" when the statement must fail
		wantErr string
		query   string
		want    []string
	}{
		{
			name:    "fts5, named out of order, through a parameter and a function",
			setup:   []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `CREATE TABLE s(z)`, `INSERT INTO s VALUES(1),(2)`},
			stmt:    `INSERT INTO g(y,x) VALUES(upper(?), 'A') RETURNING x,y,(SELECT count(*) FROM s)`,
			args:    []Value{{Typ: Text, S: []byte("b")}},
			wantRow: "A,B,2",
			query:   `SELECT x,y FROM g`,
			want:    []string{"A,B"},
		},
		{
			name:    "rtree, the module's own REAL coercion still reaches RETURNING",
			setup:   []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `CREATE TABLE s(z)`, `INSERT INTO s VALUES(1),(2)`},
			stmt:    `INSERT INTO r VALUES(1,0,1) RETURNING id,typeof(x0),(SELECT count(*) FROM s)`,
			wantRow: "1,real,2",
			query:   `SELECT id FROM r`,
			want:    []string{"1"},
		},
		{
			name:  "fts5, a connection-state function in a tuple slot",
			setup: []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `CREATE TABLE s(z)`,
				`INSERT INTO s VALUES(1),(2)`},
			// A connection-state function needs evalCtx.db to have a session
			// in reach at all -- that is why vtabInsertRowValues threads db,
			// for fts3e's own "VALUES(last_insert_rowid(),...)" idiom (see its
			// doc comment). changes() rather than last_insert_rowid() here
			// only because the CREATE VIRTUAL TABLE above has already made the
			// rowid opaque for this session (markLastRowidOpaque,
			// vtab_write.go); the ctx field being exercised is the same one.
			stmt:    `INSERT INTO g VALUES(changes(), 'z') RETURNING x,(SELECT count(*) FROM s)`,
			wantRow: "2,2",
			query:   `SELECT x,y FROM g`,
			want:    []string{"2,z"},
		},
		{
			name:    "fts5, the FIRST error wins and nothing is stored",
			setup:   []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `CREATE TABLE s(z)`},
			stmt:    `INSERT INTO g VALUES('ok', abs(-9223372036854775807-1)) RETURNING x,(SELECT count(*) FROM s)`,
			// The error is reported UNWRAPPED -- no "INSERT into g: " prefix --
			// and that is parity, not a lost detail:
			// sqlite3_result_error(context,"integer overflow",-1)
			// (func.c:205) is the whole message C SQLite reports for abs()
			// here; it does not name the statement that ran the function.
			wantErr: `engine: integer overflow`,
			query:   `SELECT x,y FROM g`,
			want:    nil,
		},
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
				t.Fatalf("compile %q: %v -- every shape here must COMPILE", tc.stmt, cerr)
			}
			_, rows, xerr := db.ExecReturningArgs(tc.stmt, tc.args)
			if tc.wantErr != "" {
				if xerr == nil {
					t.Fatalf("%q SUCCEEDED, want %q", tc.stmt, tc.wantErr)
				}
				if xerr.Error() != tc.wantErr {
					t.Fatalf("%q: got %q, want %q", tc.stmt, xerr.Error(), tc.wantErr)
				}
			} else {
				if xerr != nil {
					t.Fatalf("%q: %v", tc.stmt, xerr)
				}
				got := rvdRowStrings(rows)
				if len(got) != 1 || got[0] != tc.wantRow {
					t.Fatalf("%q RETURNING gave %v, want [%s]", tc.stmt, got, tc.wantRow)
				}
			}
			stored := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(stored, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("%q left %v, want %v", tc.stmt, stored, tc.want)
			}
		})
	}
}
