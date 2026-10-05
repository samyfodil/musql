package engine

// Compiler-side tests for pseudo-row promotions: what they emit and where they stop.
// The differential gate cannot see compilation details; this file pins the mechanism.

import (
	"errors"
	"strings"
	"testing"
)

// progHas checks whether a program or any sub-program holds a matching instruction.
func progHas(prog *Program, want func(Instruction) bool) bool {
	seen := map[*Program]bool{}
	var walk func(*Program) bool
	walk = func(p *Program) bool {
		if p == nil || seen[p] {
			return false
		}
		seen[p] = true
		for i := range p.Insns {
			in := p.Insns[i]
			if want(in) {
				return true
			}
			switch p4 := in.P4.(type) {
			case *Program:
				if walk(p4) {
					return true
				}
			case *inSubPlan:
				if walk(p4.prog) {
					return true
				}
			case *rowSubPlan:
				if walk(p4.prog) {
					return true
				}
			case *derivedSource:
				if walk(p4.prog) {
					return true
				}
			case *triggerFirePlan:
				for _, ct := range p4.triggers {
					if walk(ct.when) {
						return true
					}
					for _, b := range ct.body {
						if walk(b) {
							return true
						}
					}
				}
			}
		}
		return false
	}
	return walk(prog)
}

func pseudoRowOp(which int) func(Instruction) bool {
	return func(in Instruction) bool { return in.Op == OpPseudoRow && in.P1 == which }
}

func paramOp(which int) func(Instruction) bool {
	return func(in Instruction) bool { return in.Op == OpParam && in.P1 == which }
}

// topSubPrograms is every sub-program prog's own instructions invoke directly.
func topSubPrograms(prog *Program) []*Program {
	var out []*Program
	for _, in := range prog.Insns {
		switch p4 := in.P4.(type) {
		case *Program:
			out = append(out, p4)
		case *inSubPlan:
			out = append(out, p4.prog)
		case *rowSubPlan:
			out = append(out, p4.prog)
		}
	}
	return out
}

// TestUpsertExcludedSetSubqueryCodegen pins the mechanism behind
// compat-harness's TestUpsertExcludedInSetSubquery: a SET subquery naming the
// proposed row is a CORRELATED live sub-program that reads the DO UPDATE's own
// registers (OpOuterAggReg through emitUpsertArm's live stub) -- C's
// TK_REGISTER plus EP_VarSelect -- and a DO UPDATE with no subquery pays for
// none of it.
func TestUpsertExcludedSetSubqueryCodegen(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	withSub, err := db.compileWrite(`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	subs := topSubPrograms(withSub)
	if len(subs) == 0 {
		t.Fatal("no sub-program for the SET subquery")
	}
	for _, sp := range subs {
		if !sp.Correlated || sp.LiveSource == nil || sp.LiveRow == nil {
			t.Errorf("SET subquery naming excluded: Correlated=%v live=%v -- it must re-run per conflicting row, lowered live",
				sp.Correlated, sp.LiveSource != nil)
		}
		if !progHas(sp, func(in Instruction) bool { return in.Op == OpOuterAggReg }) {
			t.Error("excluded.v did not compile to an OpOuterAggReg read of the proposed row's register")
		}
	}

	// A DO UPDATE with no subquery must be unchanged instruction for
	// instruction: the pushed register scope already answers every reference
	// written directly in the SET list, which is what C does with all of them
	// (resolve.c:572-576's TK_REGISTER).
	plain, err := db.compileWrite(`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=excluded.v+1`)
	if err != nil {
		t.Fatalf("compile plain: %v", err)
	}
	if progHas(plain, pseudoRowOp(2)) || progHas(plain, paramOp(2)) || len(topSubPrograms(plain)) != 0 {
		t.Error("a DO UPDATE whose SET holds no subquery pays for machinery it cannot use")
	}
}

// TestUpsertSetSubqueryShapesCompile pins that the DO UPDATE's SET subqueries
// COMPILE -- a table-reading one included, at any nesting depth -- and that one
// reading a table is lowered LIVE, as the DO UPDATE runs: C codes the block
// inside the insertion loop (upsert.c:325-326), so the frozen pre-statement
// image, which used to bound this list, is not its answer.
func TestUpsertSetSubqueryShapesCompile(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO u VALUES(1,10)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		stmt  string
		table bool // reads a table, so must be lowered live
	}{
		{"no row source", `INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`, false},
		{"FROM-less aggregate", `INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT sum(excluded.v))`, false},
		{"FROM-less compound", `INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v UNION SELECT 9 LIMIT 1)`, false},
		{"row-value from a FROM-less subquery", `INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET (k,v)=(SELECT 3,4)`, false},
		{"reads another table", `INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT count(*) FROM s)`, true},
		{"reads the target table", `INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT max(v) FROM u)`, true},
		{"reads a table one level deeper", `INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT (SELECT count(*) FROM s))`, true},
	} {
		prog, cerr := db.compileWrite(tc.stmt)
		if cerr != nil {
			t.Errorf("[%s] %s: %v", tc.name, tc.stmt, cerr)
			continue
		}
		if !tc.table {
			continue
		}
		for _, sp := range topSubPrograms(prog) {
			if sp.LiveSource == nil {
				t.Errorf("[%s] %s: a table-reading SET subquery is not lowered live", tc.name, tc.stmt)
			}
		}
	}
}

// TestTriggerBodyLimitIsCodedNotFolded pins that a LIMIT naming the firing row
// becomes a COUNTER REGISTER (OpLimitCounter over an OpParam read) rather than a
// compile-time constant. A fold is not merely slower here, it is impossible: the
// body is compiled once and fired per row.
func TestTriggerBodyLimitIsCodedNotFolded(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER, c INTEGER)`,
		`CREATE TABLE u(b INTEGER)`,
		`CREATE TABLE dst(v INTEGER)`,
		`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a; END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := db.compileWrite(`INSERT INTO t VALUES(2,0)`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !progHas(prog, func(in Instruction) bool { return in.Op == OpLimitCounter }) {
		t.Error("no OpLimitCounter: the trigger body's LIMIT was not coded into a register")
	}
	if !progHas(prog, paramOp(1)) {
		t.Error("the LIMIT's new.a did not compile to an OpParam read of the firing row")
	}

	// A CONSTANT clause keeps the fold, so no counter opcode is emitted at all.
	if err := db.Exec(`DROP TRIGGER tg`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER tg2 AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT 1+1; END`); err != nil {
		t.Fatal(err)
	}
	folded, err := db.compileWrite(`INSERT INTO t VALUES(2,0)`)
	if err != nil {
		t.Fatalf("compile folded: %v", err)
	}
	if progHas(folded, func(in Instruction) bool { return in.Op == OpLimitCounter }) {
		t.Error("a constant LIMIT should still be folded at compile time, not coded")
	}
}

// TestTriggerBodyLimitDeclinesWhereItCannotBeCoded pins the post-condition
// compileSubProgram checks instead of predicting: a codegen body that cannot
// take a LIMIT register makes the statement REFUSED rather than compiled with
// the clause silently dropped. An ignored LIMIT answers every row where the
// oracle answers some.
//
// The SORTED body used to be on this list and is not any more: compileScanSorted
// codes the counter now, for the same reason compileScanPlain does, and the six
// shapes that unlocked are verified against 3.53.3 in
// compat-harness/pseudorow_subquery_test.go ("sorted ..." cases) -- including
// the sorter-bound trap that only shows when the LIMIT is a literal and the
// OFFSET is the coded half.
func TestTriggerBodyLimitDeclinesWhereItCannotBeCoded(t *testing.T) {
	for _, body := range []string{
		// Grouped, and aggregate: separate codegen bodies with their own bounds.
		`INSERT INTO dst SELECT count(*) FROM u GROUP BY b LIMIT new.a`,
		`INSERT INTO dst SELECT count(*) FROM u LIMIT new.a`,
		// Compound: a Program whose arms ARE the frames has no instructions of
		// its own for a counter to live in. Refused BEFORE the compile, at
		// compileSubProgram's own compound arm, because that arm returns early
		// and the post-condition never runs -- measured, with the check
		// missing this wrote THREE rows where 3.53.3 writes two.
		`INSERT INTO dst SELECT b FROM u UNION SELECT 9 LIMIT new.a`,
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			`CREATE TABLE t(a INTEGER, c INTEGER)`,
			`CREATE TABLE u(b INTEGER)`,
			`CREATE TABLE dst(v INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN ` + body + `; END`,
		} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", body, s, err)
			}
		}
		_, cerr := db.compileWrite(`INSERT INTO t VALUES(2,0)`)
		if cerr == nil {
			t.Errorf("[%s] compiled: if this shape now codes its LIMIT, verify it against the oracle and pin it in compat-harness/pseudorow_subquery_test.go", body)
		} else if !errors.Is(cerr, errVDBEUnsupported) && !errors.Is(cerr, errVDBESemantic) {
			t.Errorf("[%s] refusal is neither a decline nor a semantic error: %v", body, cerr)
		}
		db.Discard()
	}
}

// TestLimitExpressionCannotNameExcluded pins the asymmetry resolve.c's own
// memset creates and that an earlier revision of this engine got backwards,
// OVERWRITING committed rows: the NEW./OLD. arm reads pParse->pTriggerTab
// (resolve.c:525), a PARSE-level field "memset(&sNC, 0, sizeof(sNC))"
// (resolve.c:1903) cannot touch, while the excluded. arm reads
// pNC->ncFlags & NC_UUpsert (resolve.c:547), which the memset clears.
func TestLimitExpressionCannotNameExcluded(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE k(a INTEGER PRIMARY KEY, c INTEGER)`,
		`INSERT INTO k VALUES(1,10)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO k VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT excluded.c)`,
		`INSERT INTO k VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT 1 OFFSET excluded.c)`,
	} {
		if _, cerr := db.compileWrite(stmt); cerr == nil {
			t.Errorf("%s compiled: an upsert's excluded. must NOT resolve inside a LIMIT/OFFSET -- 3.53.3 raises \"no such column: excluded.c\" and leaves the row alone", stmt)
		}
	}
	// The control, from the same statement shape: new. DOES resolve there.
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER, c INTEGER)`,
		`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT new.a); END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, cerr := db.compileWrite(`INSERT INTO t VALUES(1,1)`); cerr != nil {
		t.Errorf("the NEW. spelling declined: %v", cerr)
	}
}

// TestReturningSubqueryAffectedRowCodegen pins the third pseudo-row: the
// affected row is snapshotted (OpPseudoRow P1=3) and read back through OpParam
// P1=3, and the resulting subquery is classified PER ROW rather than frozen --
// resolve.c:1403's nRef test, not sqlite3ProcessReturningSubqueries' FROM-list
// walk.
func TestReturningSubqueryAffectedRowCodegen(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := db.compileWrite(`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s WHERE x=a)`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !progHas(prog, pseudoRowOp(3)) {
		t.Error("no OpPseudoRow for the affected row: a RETURNING subquery is lowered live with no enclosing compiler, so without the snapshot the reference resolves nowhere")
	}
	if !progHas(prog, paramOp(3)) {
		t.Error("the correlated reference did not compile to an OpParam read of the affected row")
	}
	// The per-row LIFETIME, read off the emission that carries it. An UPDATE
	// emits ONE RETURNING block inside the row loop (returningSitePerRow), where
	// per-row-ness is the OpSubCacheReset actually resetting slots; an unrolled
	// VALUES list gets it the other way, by NOT rewinding the slot allocator
	// between tuples, so it has no reset instruction to inspect.
	if err := db.Exec(`INSERT INTO t VALUES(5,'z')`); err != nil {
		t.Fatal(err)
	}
	upd, err := db.compileWrite(`UPDATE t SET b='w' RETURNING a,(SELECT count(*) FROM s WHERE x=a)`)
	if err != nil {
		t.Fatalf("compile update: %v", err)
	}
	if !progHas(upd, paramOp(3)) {
		t.Error("the UPDATE spelling did not read the affected row")
	}
	if !progHas(upd, func(in Instruction) bool { return in.Op == OpSubCacheReset && in.P2 > 0 }) {
		t.Error("OpSubCacheReset resets nothing: a subquery naming the affected row is EP_VarSelect (resolve.c:1403-1404), so it must be re-evaluated per row rather than wrapped in OP_Once")
	}

	// The control: a RETURNING with no subquery pays for nothing.
	plain, err := db.compileWrite(`INSERT INTO t VALUES(1,'p') RETURNING a,b`)
	if err != nil {
		t.Fatalf("compile plain: %v", err)
	}
	if progHas(plain, pseudoRowOp(3)) {
		t.Error("a RETURNING list with no subquery pays for a snapshot it cannot use")
	}
}

// TestReturningSubqueryUncorrelatedStaysFrozen is the other half: a subquery
// naming nothing outside itself keeps C's OP_Once lifetime, so the reset it
// emits resets nothing. Without this the promotion above would silently
// re-evaluate every RETURNING subquery, which is three measured wrong answers
// (see returningSubqueryLifetimes' doc comment).
func TestReturningSubqueryUncorrelatedStaysFrozen(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
		`INSERT INTO t VALUES(1,10),(2,20)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := db.compileWrite(`UPDATE t SET b=b+1 RETURNING a,(SELECT count(*) FROM s)`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if progHas(prog, paramOp(3)) {
		t.Error("an uncorrelated RETURNING subquery read the affected row")
	}
	for i := range prog.Insns {
		if in := prog.Insns[i]; in.Op == OpSubCacheReset && in.P2 != 0 {
			t.Errorf("OpSubCacheReset resets %d slots: an uncorrelated RETURNING subquery must FREEZE at its first evaluation (expr.c:3889's OP_Once), not re-run per row", in.P2)
		}
	}
}

// TestPseudoRowSlotsAreNotInterchangeable pins that the three rows are separate
// channels. An upsert written inside a trigger body has a firing NEW row AND a
// proposed row live at once, and resolve.c gates them on different things, so a
// reference to one must never read the other.
func TestPseudoRowSlotsAreNotInterchangeable(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE k(a INTEGER PRIMARY KEY, c INTEGER)`,
		`INSERT INTO k VALUES(1,10)`,
		`CREATE TABLE t(a INTEGER, c INTEGER)`,
		`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c=(SELECT excluded.c*100+new.c); END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := db.compileWrite(`INSERT INTO t VALUES(1,3)`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !progHas(prog, paramOp(1)) || !progHas(prog, func(in Instruction) bool { return in.Op == OpOuterAggReg }) {
		t.Error("the body's subquery must read BOTH the firing NEW row (OpParam P1=1) and the proposed row (OpOuterAggReg, the DO UPDATE's registers)")
	}
	// And the value, end to end: 5*100 + 3.
	if err := db.Exec(`INSERT INTO t VALUES(1,3)`); err != nil {
		t.Fatalf("fire: %v", err)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, qerr := p.Query(`SELECT c FROM k`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 1 || rows[0][0].Typ != Int || rows[0][0].I != 503 {
		t.Errorf("k.c = %v, want 503 (excluded.c=5 * 100 + new.c=3)", rows)
	}
}

// TestExcludedMeansTheTargetWhenTheTargetShadowsIt pins the collision the
// upsert's REGISTER scopes resolve by scope ORDER: with the target table itself
// named "excluded", C resolves the name to the TARGET row (its own
// upsert3.test expects "DO UPDATE SET c=excluded.c+1" to INCREMENT the stored
// row). A subquery now resolves through the same two scopes in the same order
// (emitUpsertArm's live stub), so it means the target row too -- it used to
// decline, because a pseudo-row snapshot could only ever answer the proposed
// one. compat-harness's "named-excluded" case holds it to the oracle.
func TestExcludedMeansTheTargetWhenTheTargetShadowsIt(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	c := func() int64 {
		t.Helper()
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		defer p.Close()
		_, rows, qerr := p.Query(`SELECT c FROM excluded WHERE a = 1`)
		if qerr != nil || len(rows) != 1 {
			t.Fatalf("read back: %v %v", rows, qerr)
		}
		return rows[0][0].I
	}
	for _, s := range []string{
		`CREATE TABLE excluded(a INTEGER PRIMARY KEY, c INT DEFAULT 0)`,
		`INSERT INTO excluded VALUES(1,5)`,
		`INSERT INTO excluded(a) VALUES(1) ON CONFLICT(a) DO UPDATE SET c=(SELECT excluded.c+1)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := c(); got != 6 {
		t.Errorf("c = %d, want 6: excluded names the TARGET row here (5), not the proposed row's default (0)", got)
	}
	// With the target ALIASED, "excluded" is the pseudo-table again: the
	// proposed row's c is its DEFAULT, 0.
	if err := db.Exec(`INSERT INTO excluded AS base(a) VALUES(1) ON CONFLICT(a) DO UPDATE SET c=(SELECT excluded.c+1)`); err != nil {
		t.Fatal(err)
	}
	if got := c(); got != 1 {
		t.Errorf("c = %d, want 1: aliased, excluded is the proposed row", got)
	}
}

// TestPseudoRowReadWithNoRowReports pins AGENTS.md invariant 2 on the new
// OpParam arms: a machine with no such row must REPORT, never index off the end
// of an empty slice.
func TestPseudoRowReadWithNoRowReports(t *testing.T) {
	for _, which := range []int{2, 3} {
		prog := &Program{
			NReg:       1,
			NResultCol: 1,
			ColNames:   []string{"x"},
			Insns: []Instruction{
				{Op: OpParam, P1: which, P2: 0, P3: 0},
				{Op: OpResultRow, P1: 0, P2: 1},
				{Op: OpHalt},
			},
		}
		_, err := prog.exec(nil, nil)
		if err == nil {
			t.Errorf("OpParam P1=%d on a machine with no such row returned no error", which)
		} else if !strings.Contains(err.Error(), "no trigger row") {
			t.Errorf("OpParam P1=%d: %v", which, err)
		}
	}
}

// TestPseudoRowRowidReadWithNoRowReports is the ROWID half of the arm above,
// and it is the half that used to fail OPEN: "op.P2 < 0" answered a bare 0 for
// a machine carrying no such row, and 0 is a perfectly legal rowid, so nothing
// downstream could tell an answer from an empty slot. See execWithParent, whose
// missing copy is how a machine reached one of these with no row.
func TestPseudoRowRowidReadWithNoRowReports(t *testing.T) {
	for _, which := range []int{0, 1, 2, 3} {
		prog := &Program{
			NReg:       1,
			NResultCol: 1,
			ColNames:   []string{"x"},
			Insns: []Instruction{
				{Op: OpParam, P1: which, P2: -1, P3: 0},
				{Op: OpResultRow, P1: 0, P2: 1},
				{Op: OpHalt},
			},
		}
		_, err := prog.exec(nil, nil)
		if err == nil {
			t.Errorf("OpParam P1=%d P2=-1 on a machine with no such row answered rowid 0 instead of reporting", which)
		} else if !strings.Contains(err.Error(), "no trigger row") {
			t.Errorf("OpParam P1=%d P2=-1: %v", which, err)
		}
	}
}

// findOneReset returns the single OpSubCacheReset in prog's own instructions.
func findOneReset(t *testing.T, prog *Program) Instruction {
	t.Helper()
	var got []Instruction
	for i := range prog.Insns {
		if prog.Insns[i].Op == OpSubCacheReset {
			got = append(got, prog.Insns[i])
		}
	}
	if len(got) != 1 {
		t.Fatalf("want exactly one OpSubCacheReset, found %d", len(got))
	}
	return got[0]
}

// TestUpsertSetSubqueryLifetimeIsCoded pins the DO UPDATE block's own OP_Once
// discipline -- the thing the differential gate can see the ANSWER of but not
// the mechanism. A SET subquery naming "excluded" is EP_VarSelect
// (resolve.c:846-852 bumps nRef, :1402-1404 sets the flag, expr.c:3889 is what
// the flag suppresses), so it is re-run for every conflicting row: it binds as
// a CORRELATED sub-program, which takes no cache slot at all. One naming
// nothing outside itself keeps OP_Once. So the block's reset clears nothing,
// and a SET list holding one of each -- which used to decline, because one
// contiguous reset cannot serve two lifetimes -- compiles.
func TestUpsertSetSubqueryLifetimeIsCoded(t *testing.T) {
	setup := []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v, w)`,
		`CREATE TABLE src(k,v)`,
		`INSERT INTO u VALUES(1,10,0)`,
	}
	for _, tc := range []struct {
		name       string
		stmt       string
		correlated []bool // per SET subquery, in order
	}{
		{"names excluded", `INSERT INTO u SELECT k,v,0 FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`, []bool{true}},
		{"names excluded through a compound", `INSERT INTO u SELECT k,v,0 FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v UNION SELECT 0 ORDER BY 1 LIMIT 1)`, []bool{true}},
		{"names nothing outside itself", `INSERT INTO u SELECT k,v,0 FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT abs(random()))`, []bool{false}},
		{"one of each", `INSERT INTO u SELECT k,v,0 FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1), w=(SELECT abs(random()))`, []bool{true, false}},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range setup {
			if err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
		prog, cerr := db.compileWrite(tc.stmt)
		if cerr != nil {
			t.Errorf("[%s] %s: %v", tc.name, tc.stmt, cerr)
			db.Discard()
			continue
		}
		subs := topSubPrograms(prog)
		if len(subs) != len(tc.correlated) {
			t.Errorf("[%s] %d SET sub-programs, want %d", tc.name, len(subs), len(tc.correlated))
		} else {
			for k, sp := range subs {
				if sp.Correlated != tc.correlated[k] {
					t.Errorf("[%s] SET subquery %d: Correlated=%v, want %v -- one naming the proposed row must re-run\n"+
						"  for every conflicting row, and one naming nothing must freeze at the first", tc.name, k, sp.Correlated, tc.correlated[k])
				}
			}
		}
		if reset := findOneReset(t, prog); reset.P2 != 0 {
			t.Errorf("[%s] OpSubCacheReset P2=%d, want 0: the block's slots hold only OP_Once subqueries", tc.name, reset.P2)
		}
		db.Discard()
	}
}

// TestUpsertOnceSetSubquerySharesOneSlotAcrossVALUESTuples pins the STRUCTURE
// behind the unrolled-VALUES half of that same OP_Once discipline.
//
// C codes the DO UPDATE block ONCE (sqlite3UpsertDoUpdate's single
// sqlite3Update, upsert.c:325-326), so its OP_Once (expr.c:3889) fires once for
// the whole statement. This compiler UNROLLS a VALUES list and emits the block
// once per tuple, so the only way to spell one OP_Once is to hand every tuple
// the SAME run-once cache slot: runSubOnce's "if e.done" (vdbe.go) then serves
// tuple 2 the value tuple 1 computed. compiler.upsertOnceSubBase is that
// sharing, and this is what asserts it rather than trusting the answer.
//
// The per-row lifetime is asserted alongside, because the two must NOT be made
// alike: a SET subquery naming "excluded" is EP_VarSelect, and every tuple has
// its own proposed row, so it is a correlated sub-program in every tuple --
// re-run, never cached.
func TestUpsertOnceSetSubquerySharesOneSlotAcrossVALUESTuples(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stmt       string
		correlated bool
	}{
		{"uncorrelated SET subquery", `INSERT INTO u VALUES(1,100),(2,200),(3,300) ON CONFLICT(k) DO UPDATE SET v=(SELECT abs(random()))`, false},
		{"SET subquery naming excluded", `INSERT INTO u VALUES(1,100),(2,200),(3,300) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`, true},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`, `INSERT INTO u VALUES(1,10),(2,10),(3,10)`} {
			if err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
		prog, cerr := db.compileWrite(tc.stmt)
		if cerr != nil {
			t.Errorf("[%s] %s: %v", tc.name, tc.stmt, cerr)
			db.Discard()
			continue
		}
		var resets []Instruction
		for i := range prog.Insns {
			if prog.Insns[i].Op == OpSubCacheReset {
				resets = append(resets, prog.Insns[i])
			}
		}
		if len(resets) != 3 {
			t.Errorf("[%s] %s: %d OpSubCacheReset for a 3-tuple VALUES list, want 3 -- the block is emitted once per unrolled tuple", tc.name, tc.stmt, len(resets))
			db.Discard()
			continue
		}
		bases := map[int]bool{}
		for _, r := range resets {
			bases[r.P1] = true
			if r.P2 != 0 {
				t.Errorf("[%s] OpSubCacheReset P2=%d, want 0", tc.name, r.P2)
			}
		}
		if !tc.correlated && len(bases) != 1 {
			t.Errorf("[%s] %s\n  %d DISTINCT slot bases across the three tuples, want 1: a once-lifetime subquery\n"+
				"  must share ONE slot with every other tuple -- that IS expr.c:3889's OP_Once",
				tc.name, tc.stmt, len(bases))
		}
		subs := topSubPrograms(prog)
		if len(subs) != 3 {
			t.Errorf("[%s] %d SET sub-programs, want one per tuple", tc.name, len(subs))
		}
		for _, sp := range subs {
			if sp.Correlated != tc.correlated {
				t.Errorf("[%s] a tuple's SET subquery has Correlated=%v, want %v", tc.name, sp.Correlated, tc.correlated)
			}
		}
		db.Discard()
	}
}

// TestReturningCompoundSubqueryLifetimeIsClassified pins that
// returningSubqueryLifetimes reaches into a compound's ARMS. A compound Program
// carries no Insns of its own, so a walk of Insns alone reports "names nothing"
// for every UNION and freezes a subquery C re-runs per row.
func TestReturningCompoundSubqueryLifetimeIsClassified(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stmt     string
		wantP2Gt bool
	}{
		{"compound naming the affected row",
			`UPDATE t SET b='z' RETURNING a,(SELECT count(*) FROM s WHERE x=a UNION SELECT 99 ORDER BY 1 LIMIT 1)`, true},
		{"compound naming it in the SECOND arm",
			`UPDATE t SET b='z' RETURNING a,(SELECT 0 UNION ALL SELECT a ORDER BY 1 DESC LIMIT 1)`, true},
		{"compound naming nothing outside itself",
			`UPDATE t SET b='z' RETURNING a,(SELECT count(*) FROM s UNION SELECT 99 ORDER BY 1 LIMIT 1)`, false},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			`CREATE TABLE t(a,b)`,
			`CREATE TABLE s(x)`,
			`INSERT INTO s VALUES(1),(2),(2)`,
			`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		} {
			if err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
		prog, cerr := db.compileWrite(tc.stmt)
		if cerr != nil {
			t.Errorf("[%s] %s: %v", tc.name, tc.stmt, cerr)
			db.Discard()
			continue
		}
		reset := findOneReset(t, prog)
		if got := reset.P2 > 0; got != tc.wantP2Gt {
			t.Errorf("[%s] %s\n  OpSubCacheReset P2=%d, want %s -- resolve.c:1403's nRef test does not care how the\n"+
				"  SELECT is spelled, so an ARM naming the affected row makes the whole subquery per-row",
				tc.name, tc.stmt, reset.P2, map[bool]string{true: "> 0", false: "0"}[tc.wantP2Gt])
		}
		db.Discard()
	}
}

// TestLimitPromotionWithheldFromASchemaQualifiedReference pins the promotion
// GATE itself, at the level of the predicate, because the behaviour it protects
// is DOUBLY covered and so a differential fixture cannot isolate it.
//
// resolve.c admits the whole pseudo-row block only when "cnt==0 && zDb==0"
// (:522), so "main.new.a" is not a firing-row reference at all and 3.53.3
// answers it "no such column". Two independent things now say so here:
// exprNamesFiringRow refuses to PROMOTE it onto the coded route (this test),
// and resolveTriggerParam refuses to RESOLVE it if it gets there anyway
// (compat-harness/pseudorow_lifetime_test.go's
// TestPseudoRowsRejectASchemaQualifier). With the second in place the first is
// invisible to any answer-level gate -- both spellings end in an error -- which
// is exactly why the predicate is asserted directly rather than trusted.
//
// It is the gate that has to hold: promotion decides which ROUTE compiles the
// clause, and a route whose resolver ever learns to answer a three-part name
// would then answer it here too.
func TestLimitPromotionWithheldFromASchemaQualifiedReference(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    Expr
		want bool
	}{
		{"new.a", ColumnExpr{Qualifier: "new", Name: "a"}, true},
		{"old.a", ColumnExpr{Qualifier: "old", Name: "a"}, true},
		{"NEW.a, folded", ColumnExpr{Qualifier: "NEW", Name: "a"}, true},
		{"main.new.a", ColumnExpr{Schema: "main", Qualifier: "new", Name: "a"}, false},
		{"main.old.a", ColumnExpr{Schema: "main", Qualifier: "old", Name: "a"}, false},
		{"temp.new.a", ColumnExpr{Schema: "temp", Qualifier: "new", Name: "a"}, false},
		{"nested under an operator", BinaryExpr{Op: "+", L: ColumnExpr{Qualifier: "new", Name: "a"}, R: LiteralExpr{}}, true},
		{"nested, schema-qualified", BinaryExpr{Op: "+", L: ColumnExpr{Schema: "main", Qualifier: "new", Name: "a"}, R: LiteralExpr{}}, false},
		{"an ordinary column", ColumnExpr{Qualifier: "u", Name: "a"}, false},
	} {
		if got := exprNamesFiringRow(tc.e); got != tc.want {
			t.Errorf("[%s] exprNamesFiringRow = %v, want %v -- resolve.c:522 gates the pseudo-row\n"+
				"  block on \"cnt==0 && zDb==0\", so a schema qualifier takes the reference out of it\n"+
				"  before the NEW./OLD. arms at :537/:540 are reached.", tc.name, got, tc.want)
		}
	}
}

// TestLimitPromotionWithheldWhenTheFromShadowsIt pins the shadowing bound on the
// coded LIMIT. See limitOffsetNamesFiringRow (vdbe_codegen.go) for the C and for
// the PRE-EXISTING wrong answer this deliberately does not fix.
func TestLimitPromotionWithheldWhenTheFromShadowsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantCode bool
	}{
		{"nothing shadows it", `INSERT INTO dst SELECT b FROM u LIMIT new.a`, true},
		{"a table named new", `INSERT INTO dst SELECT b FROM new LIMIT new.a`, false},
		{"a table named old", `INSERT INTO dst SELECT b FROM old LIMIT new.a`, false},
		{"an item ALIASED new", `INSERT INTO dst SELECT x.b FROM u AS new, u AS x LIMIT new.a`, false},
		// An alias REPLACES the table name for a qualified reference, so a
		// table named "new" under another alias no longer answers "new." and
		// the promotion holds.
		{"the table named new is aliased away", `INSERT INTO dst SELECT z.b FROM new AS z LIMIT new.a`, true},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			`CREATE TABLE t(a INTEGER, c INTEGER)`,
			`CREATE TABLE u(a INTEGER, b INTEGER)`,
			`CREATE TABLE new(a INTEGER, b INTEGER)`,
			`CREATE TABLE old(a INTEGER, b INTEGER)`,
			`CREATE TABLE dst(v INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN ` + tc.body + `; END`,
		} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		prog, cerr := db.compileWrite(`INSERT INTO t VALUES(2,0)`)
		switch {
		case tc.wantCode && cerr != nil:
			t.Errorf("[%s] %s: declined: %v", tc.name, tc.body, cerr)
		case tc.wantCode:
			if !progHas(prog, func(in Instruction) bool { return in.Op == OpLimitCounter }) {
				t.Errorf("[%s] %s: no OpLimitCounter", tc.name, tc.body)
			}
		case cerr == nil:
			t.Errorf("[%s] %s: COMPILED. A FROM item named new/old WINS the name in 3.53.3 (resolve.c:521's\n"+
				"  cnt==0 gate), which compileColumn's arm order inverts, so serving the clause answers the\n"+
				"  FIRING row where the oracle answers the table. Fix compileColumn's order before promoting.", tc.name, tc.body)
		case !errors.Is(cerr, errVDBEUnsupported) && !errors.Is(cerr, errVDBESemantic):
			t.Errorf("[%s] %s: refusal is neither a decline nor a semantic error: %v", tc.name, tc.body, cerr)
		}
		db.Discard()
	}
}
