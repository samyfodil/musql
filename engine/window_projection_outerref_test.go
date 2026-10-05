package engine

// Tests window queries with outer-scope column references in projections.

import (
	"path/filepath"
	"strings"
	"testing"
)

// windowSubPlanOf digs the FIRST *windowPlan out of a compiled program,
// descending into sub-Programs: the shapes this file drives put the window
// query inside a select-list subquery, so its OpWindowFinal is never at the top
// level.
func windowSubPlanOf(t *testing.T, p *ReadOnlyPager, sql string) *windowPlan {
	t.Helper()
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	prog, err := compileSelectScanRow(p, stmt, nil, nil)
	if err != nil {
		t.Fatalf("compile %q: %v", sql, err)
	}
	var find func(*Program) *windowPlan
	find = func(pr *Program) *windowPlan {
		for _, in := range pr.Insns {
			if in.Op == OpWindowFinal {
				if pl, ok := in.P4.(*windowPlan); ok {
					return pl
				}
			}
		}
		for _, in := range pr.Insns {
			if sub, ok := in.P4.(*Program); ok && sub != nil {
				if pl := find(sub); pl != nil {
					return pl
				}
			}
		}
		return nil
	}
	if pl := find(prog); pl != nil {
		return pl
	}
	t.Fatalf("compile %q: no OpWindowFinal carrying a *windowPlan", sql)
	return nil
}

func windowOuterRefFixture(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wo.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE o(c TEXT COLLATE NOCASE, i INTEGER, n, t TEXT)`,
		`INSERT INTO o VALUES('apple', 5, '-1', 'x')`,
		`CREATE TABLE inr(x INTEGER, y TEXT)`,
		`INSERT INTO inr VALUES(1,'a'),(2,'b')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	pg, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

// TestWindowProjectionOuterRefIsCompiled is the claim: an enclosing query's
// column no longer stops the projection from lowering. Each shape must produce
// a real Program AND report the expected number of buffered columns, because a
// projection that lowered with zero lifts would mean the reference went
// somewhere else entirely.
func TestWindowProjectionOuterRefIsCompiled(t *testing.T) {
	p := windowOuterRefFixture(t)
	for _, tc := range []struct {
		sql   string
		nLift int
	}{
		{`SELECT (SELECT sum(x) OVER () || o.t FROM inr) FROM o`, 1},
		// The lifted reference as the WHOLE projection, with the window call
		// only in an ORDER BY the projection does not itself carry.
		{`SELECT (SELECT o.t FROM inr ORDER BY sum(x) OVER () LIMIT 1) FROM o`, 1},
		// The same reference twice is ONE column (window.c:794-801's dedup).
		{`SELECT (SELECT o.t || sum(x) OVER () || o.t FROM inr) FROM o`, 1},
		// Two different ones are two.
		{`SELECT (SELECT o.t || sum(x) OVER () || o.c FROM inr) FROM o`, 2},
		// An ORDER BY term is a projection expression too (buildWindowProjList).
		{`SELECT (SELECT group_concat(y) OVER () FROM inr ORDER BY o.t || y LIMIT 1) FROM o`, 1},
		// The window query's OWN column beside the lifted one: only the outer
		// reference is buffered, the rest still comes out of the register block.
		{`SELECT (SELECT x || y || o.t || count(*) OVER () FROM inr) FROM o`, 1},
	} {
		plan := windowSubPlanOf(t, p, tc.sql)
		if plan.proj == nil {
			t.Errorf("%s: projection did not compile", tc.sql)
			continue
		}
		if got := len(plan.projLifts); got != tc.nLift {
			t.Errorf("%s: %d buffered projection columns, want %d", tc.sql, got, tc.nLift)
		}
		if got, want := len(plan.proj.bufRegs), len(plan.projLifts); got != want {
			t.Errorf("%s: program has %d buffered registers for %d columns", tc.sql, got, want)
		}
		// The buffered columns are the operand block's FIRST slots, which is
		// what makes the column numbers the program was compiled against the
		// ones planWindowOperands hands out.
		if plan.nOps < len(plan.projLifts) {
			t.Errorf("%s: operand block %d narrower than its %d buffered columns", tc.sql, plan.nOps, len(plan.projLifts))
		}
	}
}

// TestWindowProjectionOuterRefCarriesMetadata pins what a buffered reference
// must still contribute to a COMPARISON compiled in the projection: its
// DECLARED collating sequence and its affinity. It reads them off the emitted
// opcode -- P4 is the collating sequence and P5's low nibble the comparison
// affinity (p5AffMask, vdbe_op.go) -- rather than off a hand-built context, so
// it fails if compiler.affCtx stops appending the scan body's scope chain for
// these references (the bufOut tail). That was a measured wrong answer:
// windowBufCols' doc comment carries the query, and the differential case is
// compat-harness/window_outer_ref_projection_test.go.
func TestWindowProjectionOuterRefCarriesMetadata(t *testing.T) {
	p := windowOuterRefFixture(t)
	for _, tc := range []struct {
		sql  string
		op   OpCode
		coll string
		aff  affinity
	}{
		// o.c declares COLLATE NOCASE: the comparison must be NOCASE, which is
		// the case that answered 0 for 'apple' when the chain was missing.
		{`SELECT (SELECT (o.c = 'APPLE') || count(*) OVER () FROM inr) FROM o`, OpEq, "NOCASE", affText},
		{`SELECT (SELECT (o.t = 'X') || count(*) OVER () FROM inr) FROM o`, OpEq, "BINARY", affText},
		// o.i declares INTEGER, so that operand against a TEXT literal collapses
		// to the generic NUMERIC coercion; with no affinity contributed it would
		// be affNone and '12' would never match 12.
		{`SELECT (SELECT (o.i = '12') || count(*) OVER () FROM inr) FROM o`, OpEq, "BINARY", affNumeric},
		// A TYPELESS column against a TEXT one: no affinity, and the bare
		// reference still BLOCKS the TEXT coercion a computed operand would
		// allow (isMaterializedRef -- view.test 27.*).
		{`SELECT (SELECT (o.n < o.t) || count(*) OVER () FROM inr) FROM o`, OpLt, "BINARY", affNone},
	} {
		plan := windowSubPlanOf(t, p, tc.sql)
		if plan.proj == nil {
			t.Errorf("%s: projection did not compile", tc.sql)
			continue
		}
		if len(plan.projLifts) == 0 {
			t.Errorf("%s: nothing buffered, so this asserts nothing", tc.sql)
			continue
		}
		found := false
		for _, in := range plan.proj.prog.Insns {
			if in.Op != tc.op {
				continue
			}
			found = true
			if got, _ := in.P4.(string); got != tc.coll {
				t.Errorf("%s: comparison collation %q, want %q", tc.sql, got, tc.coll)
			}
			if got := affinity(in.P5 & p5AffMask); got != tc.aff {
				t.Errorf("%s: comparison affinity %v, want %v", tc.sql, got, tc.aff)
			}
		}
		if !found {
			t.Errorf("%s: projection program emitted no %v", tc.sql, tc.op)
		}
	}
}

// TestWindowProjectionOuterRefNotLifted pins what the lift must leave alone: a
// reference this query's own FROM offers (including an AMBIGUOUS one, whose
// refusal is the correctness guard compiler.regScopeStrict exists for), and a
// reference inside a select-list sub-select, which window.c:756-770's
// p->pSubSelect guard excludes and compileColumn's outer-regScope arm already
// serves.
func TestWindowProjectionOuterRefNotLifted(t *testing.T) {
	p := windowOuterRefFixture(t)
	for _, sql := range []string{
		// No enclosing query at all: nothing to lift.
		`SELECT x, sum(x) OVER () FROM inr`,
		// The window query's own row read from a nested sub-select: served by
		// the register block, not buffered.
		`SELECT x, (SELECT y FROM inr AS i2 WHERE i2.x=inr.x), sum(x) OVER () FROM inr`,
	} {
		plan := windowSubPlanOf(t, p, sql)
		if len(plan.projLifts) != 0 {
			t.Errorf("%s: lifted %d columns, want none", sql, len(plan.projLifts))
		}
	}
	// An AMBIGUOUS reference over a join must never be BUFFERED. It is the one
	// case windowProjScopeOffers exists to keep out of this seam: a buffered
	// column is computed in the scan body, where resolveInScopes names one
	// cursor, so the statement would ANSWER where C SQLite raises "ambiguous
	// column name" (resolve.c:785). The refusal used to cost only the lowering,
	// with the error raised later, at run time; it is the compile's own error
	// now, which is what is asserted here.
	for _, sql := range []string{
		`SELECT x, count(*) OVER () FROM inr, inr AS inr2`,
		`SELECT rowid, count(*) OVER () FROM inr, inr AS inr2`,
	} {
		stmt, err := ParseSelect(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		switch _, cerr := compileSelectScanRow(p, stmt, nil, nil); {
		case cerr == nil:
			t.Errorf("%s: compiled, but an ambiguous reference must be an ERROR", sql)
		case !strings.Contains(cerr.Error(), "ambiguous"):
			t.Errorf("%s: error %v does not say \"ambiguous\" -- a buffered column would "+
				"answer here instead, which is the wrong direction", sql, cerr)
		}
	}
}
