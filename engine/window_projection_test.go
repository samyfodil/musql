package engine

// Tests for the window path's outer projection compilation. Verifies that
// the projected expressions compile correctly and declined shapes remain declined.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// windowPlanOf digs the *windowPlan out of a compiled program's OpWindowFinal.
func windowPlanOf(t *testing.T, p *ReadOnlyPager, sql string) *windowPlan {
	t.Helper()
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	prog, err := compileSelectScanRow(p, stmt, nil, nil)
	if err != nil {
		t.Fatalf("compile %q: %v", sql, err)
	}
	for _, in := range prog.Insns {
		if in.Op == OpWindowFinal {
			if pl, ok := in.P4.(*windowPlan); ok {
				return pl
			}
		}
	}
	t.Fatalf("compile %q: no OpWindowFinal carrying a *windowPlan", sql)
	return nil
}

// openWindowFixture creates a scratch database, runs setup, and returns a
// read-only pager over it. Shared with window_operand_subquery_test.go.
func openWindowFixture(t *testing.T, setup []string) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wp.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func windowProjFixture(t *testing.T) *ReadOnlyPager {
	t.Helper()
	return openWindowFixture(t, []string{
		`CREATE TABLE w(a INTEGER, b TEXT, c TEXT COLLATE NOCASE, d)`,
		`INSERT INTO w VALUES(1,'A','apple',10),(2,'B','APPLE',20),(3,'C','pear',30)`,
		`CREATE TABLE u(k, v)`,
		`INSERT INTO u VALUES(1,100),(2,200)`,
		// A second table sharing w's column names, for the ambiguous-reference
		// cases.
		`CREATE TABLE w2(a INTEGER, z)`,
		`INSERT INTO w2 VALUES(1,'p'),(2,'q')`,
		`CREATE VIEW wv AS SELECT a, b, d FROM w`,
	})
}

// TestWindowProjectionIsCompiled is the assertion the parity file cannot make:
// for each of these the projection must be a real Program, not a soft arm
// answering identically.
func TestWindowProjectionIsCompiled(t *testing.T) {
	p := windowProjFixture(t)
	for _, sql := range []string{
		// A bare window result, and one wrapped in arithmetic -- the second is
		// the case that needs compiler.winRegs to be a REGISTER read
		// (expr.c:5358-5360) rather than a special top-level shape.
		`SELECT sum(d) OVER () FROM w`,
		`SELECT a, sum(d) OVER (ORDER BY a) + 1 FROM w`,
		`SELECT a*2, b||'!', CASE WHEN a>1 THEN d ELSE -d END, sum(d) OVER () FROM w`,
		// CAST beside a second read of the same column: the projection's
		// register block is shared, so an OpCast that rewrote its operand in
		// place would corrupt the row (see compileExpr's CastExpr case).
		`SELECT CAST(b AS INTEGER), b, sum(d) OVER () FROM w`,
		`SELECT upper(c), abs(a), sum(d) OVER () FROM w`,
		`SELECT rowid, a, row_number() OVER (ORDER BY a) FROM w`,
		`SELECT *, count(*) OVER () FROM w`,
		// ORDER BY terms: an EXPRESSION term joins the projection list, an
		// ordinal or an output alias does not.
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY b||a`,
		`SELECT a, sum(d) OVER (ORDER BY a) AS s FROM w ORDER BY s`,
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY 2`,
		`SELECT DISTINCT a%2, sum(d) OVER () FROM w ORDER BY 1 LIMIT 1 OFFSET 0`,
		// A bound parameter is an OpVariable read, which is why the machine
		// carries the statement's params.
		`SELECT ?1, a, sum(d) OVER () FROM w`,
		// A view is one derived scope, so it lowers like a table.
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM wv`,
		// A JOIN: two register scopes, each addressing its own slice of the
		// batch's [cols..] block and its own slot of the trailing rowid block.
		// w(a,b,c,d) and u(k,v) share no column name, so nothing is ambiguous.
		`SELECT w.a, u.v, sum(w.d) OVER () FROM w, u`,
		`SELECT w.a, sum(w.d) OVER () FROM w JOIN u ON u.k=w.a`,
		`SELECT a, v, k, b, sum(d) OVER (ORDER BY a) FROM w, u`,
		`SELECT u.rowid, w.rowid, count(*) OVER () FROM w, u`,
		// A select-list subquery CORRELATED to the window query's own row.
		// It lowers because the sub-compile resolves "w.a" against this
		// projection's REGISTER block (compileColumn's outer-regScope arm,
		// vdbe_codegen.go) -- selectWindowRewriteExprCb's p->pSubSelect lift
		// (window.c:756-771), where the same reference becomes a read of the
		// buffered ephemeral row before the sub-select is coded at all.
		//
		// It is also what keeps this out of the shape TestWindowProjectionDeclines
		// still pins: binding the outer reference makes the sub-Program
		// CORRELATED, and a correlated subquery takes no run-once cache slot
		// (compileScalarSubquery/compileExistsSubquery/compileInSubquery), so
		// compileWindowProjection's c.nSub guard is still zero here.
		`SELECT a, EXISTS(SELECT 1 FROM u WHERE k=w.a), sum(d) OVER () FROM w`,
		`SELECT a, (SELECT v FROM u WHERE k=w.a), sum(d) OVER () FROM w`,
		`SELECT a, (SELECT v FROM u WHERE k=a), sum(d) OVER () FROM w`,
		`SELECT a, a IN (SELECT k FROM u WHERE v=w.b), sum(d) OVER () FROM w`,
		// An UNCORRELATED select-list subquery, which needs the run-once cache
		// slot this machine had no room for and now has (Program.NSubCache,
		// sized by windowProjection.machine). It moved here from
		// TestWindowProjectionDeclines when the soft arm was deleted: there is
		// nothing left to decline TO.
		`SELECT a, (SELECT max(v) FROM u), sum(d) OVER () FROM w`,
		`SELECT a, a IN (SELECT k FROM u), sum(d) OVER () FROM w`,
		`SELECT a, EXISTS(SELECT 1 FROM u), sum(d) OVER () FROM w`,
	} {
		plan := windowPlanOf(t, p, sql)
		if plan.proj == nil || plan.proj.prog == nil {
			t.Errorf("%s: projection did NOT compile", sql)
			continue
		}
		if got, want := len(plan.proj.resultReg), len(plan.projExprs); got != want {
			t.Errorf("%s: %d result registers for %d projected expressions", sql, got, want)
		}
		if len(plan.projExprs) < len(plan.outs) {
			t.Errorf("%s: projection list %d shorter than the select list %d", sql, len(plan.projExprs), len(plan.outs))
		}
	}
}

// TestWindowProjectionDeclines pins the one shape compileWindowProjection still
// refuses, and pins it as a STATEMENT ERROR rather than a fallback.
//
// An AMBIGUOUS reference over a join is a correctness boundary, not a
// preference: resolveRowReg would resolve it innermost-wins where C SQLite
// raises "ambiguous column name" (resolve.c:785), and compiler.regScopeStrict is
// what makes it refuse instead. The refusal used to cost the LOWERING and the
// soft arm then raised the error at run time; that arm is deleted, so the
// refusal is now the compile's own error -- the same answer, one phase
// earlier. Asserting the error is the whole point: if the refusal ever silently
// became a compiled read of one cursor, the statement would ANSWER where 3.53.3
// errors (AGENTS.md invariant 1).
//
// The CORRELATED subquery that used to sit in this list moved to
// TestWindowProjectionLowers when it started resolving against this projection's
// register block; the UNCORRELATED one moved there when the machine got its
// run-once cache.
func TestWindowProjectionDeclines(t *testing.T) {
	p := windowProjFixture(t)
	for _, sql := range []string{
		// "a" is a column of w only, so the join above lowers; here the SECOND
		// FROM item offers it too and the reference has no single answer. A
		// bare rowid over two rowid tables is the same rule.
		`SELECT a, sum(d) OVER () FROM w, w2`,
		`SELECT rowid, sum(w.d) OVER () FROM w, u`,
	} {
		stmt, err := ParseSelect(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		switch _, cerr := compileSelectScanRow(p, stmt, nil, nil); {
		case cerr == nil:
			t.Errorf("%s: compiled, but an ambiguous reference must be an ERROR", sql)
		case !strings.Contains(cerr.Error(), "ambiguous"):
			t.Errorf("%s: error %v does not say \"ambiguous\" -- resolve.c:785's wording is "+
				"what the statement must report", sql, cerr)
		}
	}
}

// TestWindowProjScopeLowerableGuards drives windowProjScopeLowerable directly,
// over synthetic plans no SQL statement can currently produce.
//
// Two of its conditions are LAYOUT guards, not semantic ones: compileScanWindow
// builds plan.scopes and plan.nRowids from the same source list, so their
// lengths always agree and each scope's offset is always its running column
// total. A behavioural test therefore cannot reach either -- mutation-tested by
// deleting the nRowids condition, and the whole engine suite plus every window
// parity battery stayed green. They are kept because the failure they prevent is
// SILENT: compileWindowProjection sizes the rowid register block from nRowids
// and addresses scope i's rowid at base+i, so a plan with more scopes than rowid
// slots would read a register belonging to something else and answer a plausible
// wrong value rather than fail. This test is what makes them falsifiable.
func TestWindowProjScopeLowerableGuards(t *testing.T) {
	scope := func(name string, cols []string, offset int) tableScope {
		s := tableScope{name: name, offset: offset, colIndex: map[string]int{}}
		for i, c := range cols {
			s.cols = append(s.cols, columnInfo{Name: c})
			s.colIndex[c] = i
		}
		return s
	}
	for _, tc := range []struct {
		name string
		plan windowPlan
		want bool
	}{
		{"no scopes, no columns", windowPlan{}, true},
		{"no scopes but columns", windowPlan{nCols: 2}, false},
		{"one scope", windowPlan{scopes: []tableScope{scope("t", []string{"a", "b"}, 0)}, nCols: 2}, true},
		{"one scope, no rowid slot", windowPlan{scopes: []tableScope{scope("t", []string{"a"}, 0)}, nCols: 1, nRowids: 0}, true},
		{"one scope, short block", windowPlan{scopes: []tableScope{scope("t", []string{"a"}, 0)}, nCols: 2}, false},
		{"two scopes", windowPlan{
			scopes:  []tableScope{scope("t", []string{"a"}, 0), scope("u", []string{"k"}, 1)},
			nCols:   2,
			nRowids: 2,
		}, true},
		{"two scopes, one rowid slot", windowPlan{
			scopes:  []tableScope{scope("t", []string{"a"}, 0), scope("u", []string{"k"}, 1)},
			nCols:   2,
			nRowids: 1,
		}, false},
		{"two scopes, wrong offset", windowPlan{
			scopes:  []tableScope{scope("t", []string{"a"}, 0), scope("u", []string{"k"}, 5)},
			nCols:   2,
			nRowids: 2,
		}, false},
		{"two scopes, block too wide", windowPlan{
			scopes:  []tableScope{scope("t", []string{"a"}, 0), scope("u", []string{"k"}, 1)},
			nCols:   3,
			nRowids: 2,
		}, false},
	} {
		if got := windowProjScopeLowerable(&tc.plan); got != tc.want {
			t.Errorf("%s: windowProjScopeLowerable = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A coalescing scope is refused whatever the layout says: which source a
	// RIGHT/FULL JOIN's coalesced column reads is decided by the ROW's values,
	// and a register index is static.
	for _, name := range []string{"coalesced", "coalesceFallback"} {
		s := scope("t", []string{"a"}, 0)
		if name == "coalesced" {
			s.coalesced = map[string]int{"a": 0}
		} else {
			s.coalesceFallback = map[string][]int{"a": {0}}
		}
		p := windowPlan{scopes: []tableScope{s}, nCols: 1, nRowids: 1}
		if windowProjScopeLowerable(&p) {
			t.Errorf("a scope with %s must not lower", name)
		}
	}
}

// TestWindowProjListMapsOrderBy pins buildWindowProjList's index map: an ORDER
// BY term either names an output column (orderOrdinal >= 0, projOrder -1) or
// has its own slot in the projection list, never both and never neither. A
// wrong index here reads some other expression's value as the sort key, which
// sorts rows into a plausible-looking wrong order.
func TestWindowProjListMapsOrderBy(t *testing.T) {
	p := windowProjFixture(t)
	for _, sql := range []string{
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY b||a`,
		`SELECT a, sum(d) OVER (ORDER BY a) AS s FROM w ORDER BY s, b, a*3`,
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY 2, a+1, 1`,
	} {
		plan := windowPlanOf(t, p, sql)
		if len(plan.projOrder) != len(plan.orderExprs) {
			t.Fatalf("%s: projOrder %d vs orderExprs %d", sql, len(plan.projOrder), len(plan.orderExprs))
		}
		for j := range plan.orderExprs {
			ord, slot := plan.orderOrdinal[j], plan.projOrder[j]
			switch {
			case ord >= 0 && slot != -1:
				t.Errorf("%s: term %d names output %d AND slot %d", sql, j, ord, slot)
			case ord < 0 && (slot < len(plan.outs) || slot >= len(plan.projExprs)):
				t.Errorf("%s: term %d slot %d outside the appended range [%d,%d)", sql, j, slot,
					len(plan.outs), len(plan.projExprs))
			case ord < 0 && !reflect.DeepEqual(plan.projExprs[slot], plan.orderExprs[j]):
				t.Errorf("%s: term %d slot %d holds a different expression", sql, j, slot)
			}
		}
	}
}

// TestWindowAggArgsStamped gates the other half: an ordinary aggregate window
// call's argument / separator / FILTER must actually be batch columns, so
// aggItem.step reads a value. Without this the whole lowering could be dead and
// every parity case would still pass.
func TestWindowAggArgsStamped(t *testing.T) {
	p := windowProjFixture(t)
	type want struct{ arg, sep, filter bool }
	cases := []struct {
		sql string
		w   want
	}{
		{`SELECT sum(d*2) OVER () FROM w`, want{arg: true}},
		{`SELECT count(b||'x') OVER () FROM w`, want{arg: true}},
		{`SELECT min(c) OVER () FROM w`, want{arg: true}},
		{`SELECT avg(d) FILTER (WHERE a>1) OVER () FROM w`, want{arg: true, filter: true}},
		{`SELECT group_concat(b, '-') OVER () FROM w`, want{arg: true, sep: true}},
		{`SELECT group_concat(b) OVER () FROM w`, want{arg: true}},
		// count(*) has no argument at all.
		{`SELECT count(*) OVER () FROM w`, want{}},
		// The SUBTYPE aggregates are deliberately NOT lowered -- SQLite sets
		// bExprArgs for them and re-codes the argument at step time
		// (window.c:1041-1044, :1733).
		{`SELECT json_group_array(d) OVER () FROM w`, want{}},
		{`SELECT json_group_object(b, d) OVER () FROM w`, want{}},
	}
	for _, tc := range cases {
		plan := windowPlanOf(t, p, tc.sql)
		if len(plan.calls) != 1 {
			t.Fatalf("%s: %d calls", tc.sql, len(plan.calls))
		}
		call := plan.calls[0]
		tmpl := &aggItem{}
		stampWindowAggRegs(tmpl, call)
		got := want{
			arg:    tmpl.rowRegs[aggExprArg] > 0,
			sep:    tmpl.rowRegs[aggExprSep] > 0,
			filter: tmpl.rowRegs[aggExprFilter] > 0,
		}
		if got != tc.w {
			t.Errorf("%s: stamped %+v, want %+v", tc.sql, got, tc.w)
		}
	}
}

// TestWindowAggArgSlotMatchesPlan is stampWindowAggRegs' positional mapping,
// asserted rather than trusted: for every name windowAggArgsLowerable admits,
// planAggregateCallKind must put args[0] in expr and args[1] in sepExpr. If a
// future aggregate is added to that allow-list with a different shape, the
// stamped register would point at the wrong operand column -- a wrong value,
// silently.
func TestWindowAggArgSlotMatchesPlan(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		args int
	}{
		{`SELECT count(a) OVER () FROM w`, 1},
		{`SELECT sum(a) OVER () FROM w`, 1},
		{`SELECT total(a) OVER () FROM w`, 1},
		{`SELECT avg(a) OVER () FROM w`, 1},
		{`SELECT min(a) OVER () FROM w`, 1},
		{`SELECT max(a) OVER () FROM w`, 1},
		{`SELECT group_concat(a) OVER () FROM w`, 1},
		{`SELECT group_concat(a, '-') OVER () FROM w`, 2},
		{`SELECT string_agg(a, '-') OVER () FROM w`, 2},
	} {
		stmt, err := ParseSelect(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		var calls []windowCall
		rewriteWindowCalls(stmt.Columns[0].Expr, &calls)
		if len(calls) != 1 {
			t.Fatalf("%s: %d calls", tc.sql, len(calls))
		}
		call := calls[0]
		if !windowAggArgsLowerable(&call) {
			t.Errorf("%s: not admitted by windowAggArgsLowerable", tc.sql)
			continue
		}
		if len(call.args) != tc.args {
			t.Fatalf("%s: %d args", tc.sql, len(call.args))
		}
		it, err := planAggregateCall(call.funcExpr())
		if err != nil {
			t.Fatalf("%s: plan: %v", tc.sql, err)
		}
		if it.expr == nil {
			t.Errorf("%s: aggExprArg slot (aggItem.expr) is empty", tc.sql)
		}
		if (it.sepExpr != nil) != (tc.args == 2) {
			t.Errorf("%s: aggExprSep slot populated=%v for %d args", tc.sql, it.sepExpr != nil, tc.args)
		}
	}
}

// TestWindowPlanDistinctCollations pins the PLAN half of the collated DISTINCT
// dedup (compat-harness/window_distinct_collation_test.go measures the answers):
// the per-output-column collating sequence really is stored on the plan, and it
// is topExprCollation's per-column answer rather than "the query has a NOCASE
// column somewhere".
//
// A behavioural gate cannot see this on its own. Every case below whose expected
// list is nil answers identically under a BINARY dedup, so only reading the plan
// distinguishes "computed no collation" from "never computed one".
func TestWindowPlanDistinctCollations(t *testing.T) {
	p := windowProjFixture(t)
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		// c is COLLATE NOCASE; a, b, d and every window result are BINARY, and
		// nil is the all-BINARY encoding both OpDistinct's P4 and
		// keysEqualGrouping already use.
		{`SELECT DISTINCT c, count(*) OVER () FROM w`, []string{"NOCASE", ""}},
		{`SELECT DISTINCT b, count(*) OVER () FROM w`, nil},
		{`SELECT DISTINCT a, b, d, count(*) OVER () FROM w`, nil},
		// Per column INDEPENDENTLY, and an explicit COLLATE overrides.
		{`SELECT DISTINCT c, c COLLATE BINARY, count(*) OVER () FROM w`, []string{"NOCASE", "", ""}},
		{`SELECT DISTINCT c COLLATE BINARY, c, count(*) OVER () FROM w`, []string{"", "NOCASE", ""}},
		{`SELECT DISTINCT b COLLATE NOCASE, count(*) OVER () FROM w`, []string{"NOCASE", ""}},
		// CAST and unary "+" are transparent; any other expression loses it.
		{`SELECT DISTINCT CAST(c AS TEXT), count(*) OVER () FROM w`, []string{"NOCASE", ""}},
		{`SELECT DISTINCT +c, count(*) OVER () FROM w`, []string{"NOCASE", ""}},
		{`SELECT DISTINCT c||'', count(*) OVER () FROM w`, nil},
		{`SELECT DISTINCT lower(c), count(*) OVER () FROM w`, nil},
		// Through a view, whose column must carry the declared collation out.
		{`SELECT DISTINCT b, count(*) OVER () FROM wv`, nil},
		// Not DISTINCT at all: the list is still computed (it costs one schema
		// walk at compile time), and windowFinal simply never consults it.
		{`SELECT c, count(*) OVER () FROM w`, []string{"NOCASE", ""}},
	} {
		plan := windowPlanOf(t, p, tc.sql)
		if !reflect.DeepEqual(plan.distinctColls, tc.want) {
			t.Errorf("%s: distinctColls = %#v, want %#v", tc.sql, plan.distinctColls, tc.want)
		}
	}
}
