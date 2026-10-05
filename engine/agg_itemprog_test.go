package engine

// Tests compilation of aggregate result expressions and query shapes.

import (
	"errors"
	"path/filepath"
	"testing"
)

func itemProgs(plan *aggPlan) []bool {
	var out []bool
	for _, it := range plan.outPlans {
		out = append(out, it != nil && it.prog != nil)
	}
	if plan.havingPlan != nil {
		out = append(out, plan.havingPlan.prog != nil)
	}
	for _, op := range plan.orderPlans {
		if op.item != nil {
			out = append(out, op.item.prog != nil)
		}
	}
	return out
}

func TestAggItemProgramsCompiled(t *testing.T) {
	p := argRegsFixture(t)
	cases := []struct {
		sql  string
		want []bool
		why  string
	}{
		{`SELECT count(*) FROM t`, []bool{true},
			"a whole-table aggregate's item is a bare groupAggExpr"},
		{`SELECT sum(v) * 2 + 1 FROM t`, []bool{true},
			"arithmetic around the placeholder is ordinary expression codegen"},
		{`SELECT k, count(*) FROM t GROUP BY k`, []bool{true, true},
			"a bare reference to a GROUP BY key column is a groupBareColExpr (the GROUP-BY-KEY REFERENCE RULE, sql_group.go), so this pair reads the anchor block and the accumulator block"},
		{`SELECT k+1, count(*) FROM t GROUP BY k+1`, []bool{true, true},
			"...and a NON-column GROUP BY expression is the groupKeyExpr that reads the KEY block"},
		{`SELECT max(v) + (k+1) FROM t GROUP BY k+1`, []bool{true},
			"one item over both blocks at once"},
		{`SELECT k, max(v), v FROM t GROUP BY k`, []bool{true, true, true},
			"a bare column reads the anchor-row register block"},
		{`SELECT k, rowid FROM t GROUP BY k`, []bool{true, true},
			"...and the rowid pseudo-column reads the anchor rowid block"},
		{`SELECT count(*) FROM t HAVING count(*) > 1`, []bool{true, true},
			"HAVING is compiled exactly like a select-list item (select.c:8909)"},
		{`SELECT k, count(*) FROM t GROUP BY k ORDER BY sum(v)`, []bool{true, true, true},
			"a non-ordinal ORDER BY term is one too"},
		{`SELECT k, CASE WHEN sum(v) > 0 THEN 'p' ELSE 'n' END FROM t GROUP BY k`, []bool{true, true},
			"CASE/comparison/literals all lower"},
		{`SELECT k, sum(v) IN (1,2,3) FROM t GROUP BY k`, []bool{true, true},
			"an IN over a value LIST lowers; only an IN (SELECT ...) does not"},
		{`SELECT k, cast(sum(v) AS TEXT) || 'x' FROM t GROUP BY k`, []bool{true, true},
			"CAST and a function-free concatenation lower"},
		{`SELECT k, abs(sum(v)) FROM t GROUP BY k`, []bool{true, true},
			"a scalar function call compiles to OpFunction"},

		{`SELECT count(*), (SELECT 5) FROM t`, []bool{true, true},
			"an UNCORRELATED subquery item compiles as an ordinary sub-Program with a run-once cache slot (aggItemVM sizes one)"},
		{`SELECT count(*) FROM t WHERE (SELECT 1)`, []bool{true},
			"...and a subquery OUTSIDE the item does not disturb it"},
		{`SELECT k, count(*) FROM t GROUP BY k HAVING count(*) > (SELECT 0)`, []bool{true, true, true},
			"HAVING is compiled exactly the same way"},
		{`SELECT k, count(*), (SELECT max(x.v) FROM t AS x WHERE x.v = t.v) FROM t GROUP BY k`, []bool{true, true, true},
			"a CORRELATED body: t.v is rewritten to the anchor-row placeholder and read across the frame by OpOuterAggReg (rewriteAggItemBodies)"},
		{`SELECT k, count(*) FROM t GROUP BY k HAVING EXISTS (SELECT 1 FROM t AS x WHERE x.v = t.v)`, []bool{true, true, true},
			"...and so is an EXISTS body in HAVING"},
		{`SELECT k, count(*) FROM t GROUP BY k HAVING t.v IN (SELECT x.v FROM t AS x WHERE x.k = t.k)`, []bool{true, true, true},
			"...and an IN (SELECT ...) body"},

		{`SELECT d.k, count(*), (SELECT count(*) FROM t AS u WHERE u.v = d.e) FROM (SELECT k, v+1 AS e FROM t) AS d GROUP BY d.k`, []bool{true, true, true},
			"a COMPUTED derived-table column (NoAffinity) used to be DECLINED here, because groupBareColExpr answered isMaterializedRef unconditionally instead of from the column's own NoAffinity bit. The bit now travels ON the placeholder (groupBareColExpr.noAff), so there is one answer and it is the column's own"},
		{`SELECT count(*), changes() FROM t`, []bool{true, true},
			"a connection-state function compiles: connStateValue now walks the same outer chain evalCtx.connState does, and aggItemVM carries it"},
	}
	for _, tc := range cases {
		got := itemProgs(aggPlanOf(t, p, tc.sql))
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d items, want %d (%s)", tc.sql, len(got), len(tc.want), tc.why)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: item %d compiled=%v, want %v (%s)", tc.sql, i, got[i], tc.want[i], tc.why)
			}
		}
	}
}

// TestAggItemProgramNoVMState is compileSelfRowExpr's own guard, restated for
// this compile: the machine an item's program runs on carries a register file,
// the pager and the bound parameters and NOTHING else, so a program that
// allocated a cursor, a record register, a sorter, a subquery cache slot or a
// distinct set would index a nil slice at run time -- a panic, which AGENTS.md
// invariant 3 forbids without exception.
//
// Every shape TestAggItemProgramsCompiled expects to compile is re-checked
// here through the compiler's OWN allocators, which is what cannot drift from
// what the opcodes actually need.
func TestAggItemProgramNoVMState(t *testing.T) {
	p := argRegsFixture(t)
	for _, sql := range []string{
		`SELECT count(*) FROM t`,
		`SELECT k, max(v), v FROM t GROUP BY k`,
		`SELECT k, abs(sum(v)) FROM t GROUP BY k`,
		`SELECT k, sum(v) IN (1,2,3) FROM t GROUP BY k`,
		`SELECT k, count(*) FROM t GROUP BY k ORDER BY sum(v)`,
	} {
		plan := aggPlanOf(t, p, sql)
		items := append([]*itemPlan{}, plan.outPlans...)
		if plan.havingPlan != nil {
			items = append(items, plan.havingPlan)
		}
		for _, op := range plan.orderPlans {
			items = append(items, op.item)
		}
		for i, it := range items {
			if it == nil || it.prog == nil {
				continue
			}
			pr := it.prog.prog
			if pr.NCursors != 0 || pr.NRecRegs != 0 || pr.NSorters != 0 || pr.NSubCache != 0 || pr.NDistinct != 0 {
				t.Errorf("%s: item %d compiled to a program wanting VM state the item machine does not have", sql, i)
			}
		}
	}
}

// aggItemCollFixture is the collation/affinity half of the differential. The
// placeholders CARRY the affinity and collating sequence of what they replaced
// (groupKeyExpr.aff/.coll, groupAggExpr.coll, groupBareColExpr.aff/.coll), and
// a comparison in the same item is resolved from the AST node -- so this
// fixture feeds the tkt3493 family, which is precisely where dropping one of
// those fields shows up as a wrong answer.
func aggItemCollFixture(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ic.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t2(a TEXT COLLATE NOCASE, b TEXT COLLATE BINARY, n INTEGER)`,
		`INSERT INTO t2 VALUES('aBc','DeF',1)`,
		`INSERT INTO t2 VALUES('ABC','def',2)`,
		`INSERT INTO t2 VALUES('xyz','xyz',3)`,
		`CREATE TABLE t3(c0 TEXT COLLATE NOCASE, c1 TEXT COLLATE RTRIM)`,
		`INSERT INTO t3 VALUES('a','b ')`,
		`INSERT INTO t3 VALUES('A','B')`,
		`INSERT INTO t3 VALUES('aB','ab')`,
	} {
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

// TestAggItemProgramAnswers is what remains of the differential this battery
// was written as. It ran each statement twice -- once with the item programs
// on, once with them turned off through the aggItemProgramsDisabledForTest
// seam -- and compared the two byte for byte. There is no second answer any
// more (an item that does not lower is refused by compileAggItemPrograms at
// COMPILE time), so the seam is gone and with it the comparison.
//
// The BATTERY is kept exactly as it was, because what it now asserts is
// stronger than what it asserted before: every one of these statements must
// COMPILE and ANSWER. Each entry was chosen for a mismatch a compiled item
// could produce -- a value in the wrong storage class, a comparison resolved
// under the wrong collating sequence or affinity (the placeholders carry both),
// a NULL where a value belongs (a mis-sized register block), a JSON subtype
// kept where a grouped row loses it -- and each is now a DECLINE gate as well.
// The values themselves are pinned against the C SQLite oracle in
// compat-harness, which is where an oracle-checked value belongs.
func TestAggItemProgramAnswers(t *testing.T) {
	both := func(t *testing.T, p *ReadOnlyPager, sql string) {
		t.Helper()
		p.planCache = nil // the compile is memoized on SQL TEXT alone
		// A DECLINE fails; an ordinary SQL error does not. Two of these
		// statements genuinely raise "integer overflow" over this fixture,
		// which holds -9223372036854775808 so the stamped argument sees every
		// storage class -- C SQLite raises there too, and the pair the old
		// differential compared simply agreed on the error.
		if _, _, err := p.QueryArgs(sql, nil); errors.Is(err, errVDBEUnsupported) {
			t.Errorf("%s: %v", sql, err)
		}
	}

	p := argRegsFixture(t)
	for _, sql := range []string{
		`SELECT count(*) FROM t`,
		`SELECT count(*), sum(v), total(v), avg(v), min(v), max(v) FROM t`,
		`SELECT sum(v) * 2 + 1, -min(v), NOT (max(v) > 0) FROM t`,
		`SELECT count(DISTINCT v), max(s) FROM t`,
		`SELECT k, count(*), sum(v) FROM t GROUP BY k`,
		// The groupKeyExpr block: a NON-column GROUP BY expression, which is
		// the only spelling that reads it (a bare key column resolves to the
		// anchor block -- the GROUP-BY-KEY REFERENCE RULE, sql_group.go).
		`SELECT k+1, count(*) FROM t GROUP BY k+1`,
		`SELECT max(v) + (k+1), (k+1) * 10, (k+1) || 'x' FROM t GROUP BY k+1`,
		`SELECT abs(k), count(*) FROM t GROUP BY abs(k) HAVING abs(k) >= 0`,
		`SELECT k+1, count(*) FROM t GROUP BY k+1 ORDER BY k+1 DESC`,
		`SELECT k, max(v), v, s, rowid, id FROM t GROUP BY k`,
		`SELECT k, min(v), v FROM t GROUP BY k`,
		`SELECT k || 'x', sum(v) IS NULL, sum(v) IN (1,2,3) FROM t GROUP BY k`,
		`SELECT k, CASE WHEN sum(v) > 0 THEN 'p' WHEN sum(v) = 0 THEN 'z' ELSE 'n' END FROM t GROUP BY k`,
		`SELECT k, cast(sum(v) AS TEXT) || '!', abs(min(v)), length(max(s)) FROM t GROUP BY k`,
		`SELECT k, sum(v) BETWEEN -10 AND 10, max(s) LIKE 's%', max(s) GLOB 's?' FROM t GROUP BY k`,
		`SELECT count(*) FROM t HAVING count(*) > 1`,
		`SELECT count(*) FROM t HAVING count(*) > 1000`,
		`SELECT k, count(*) FROM t GROUP BY k HAVING sum(v) > 0`,
		`SELECT k, count(*) FROM t GROUP BY k ORDER BY sum(v)`,
		`SELECT k, count(*) FROM t GROUP BY k ORDER BY max(s) DESC`,
		// The whole-table aggregate over ZERO rows: anchorRow's all-NULL row of
		// the query's own width, which the register block must reproduce.
		`SELECT count(*), max(v), v, s, rowid FROM t WHERE 0`,
		// A CORRELATED bare column left standing in a rewritten item (bound at
		// run time by bindAggOuterRefs, vdbe_agg_codegen.go): still the DECLINE
		// path, exercised so it is covered rather than assumed.
		`SELECT (SELECT max(v) + t.id FROM t AS u) FROM t`,

		// SUBQUERY BODIES, the shape rewriteAggItemBodies now compiles
		// (vdbe_agg_item_subst.go). Every one of these is a correlated read of
		// the group's ANCHOR ROW through a placeholder the body resolves with
		// OpOuterAggReg, and the anchor is the thing most easily got wrong: it
		// MOVES with the query's min()/max() census (aggResult's own oracle
		// table), so a body reading a different row than the bare columns
		// beside it shows up here as a different value, not an error.
		`SELECT k, count(*), (SELECT count(*) FROM t AS u WHERE u.k = t.k) FROM t GROUP BY k`,
		`SELECT k, max(v), (SELECT u.s FROM t AS u WHERE u.v = t.v) FROM t GROUP BY k`,
		`SELECT k, min(v), (SELECT u.s FROM t AS u WHERE u.v = t.v) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT u.s FROM t AS u WHERE u.v = t.v) FROM t GROUP BY k`,
		`SELECT k, max(NULL), (SELECT u.s FROM t AS u WHERE u.v = t.v) FROM t GROUP BY k`,
		`SELECT k, max(v) FILTER (WHERE s='zzz'), (SELECT u.s FROM t AS u WHERE u.v = t.v) FROM t GROUP BY k`,
		// The same reference reached through the item's ROWID and through a
		// non-key bare column beside the subquery.
		`SELECT k, count(*), (SELECT u.k FROM t AS u WHERE u.id = t.id) FROM t GROUP BY k`,
		// EXISTS / NOT EXISTS / IN (SELECT ...) bodies, in the select list and
		// in HAVING.
		`SELECT k, count(*), EXISTS (SELECT 1 FROM t AS u WHERE u.v = t.v) FROM t GROUP BY k`,
		`SELECT k, count(*), NOT EXISTS (SELECT 1 FROM t AS u WHERE u.v > t.v) FROM t GROUP BY k`,
		`SELECT k, count(*) FROM t GROUP BY k HAVING EXISTS (SELECT 1 FROM t AS u WHERE u.v = t.v)`,
		`SELECT k, count(*) FROM t GROUP BY k HAVING t.v IN (SELECT u.v FROM t AS u WHERE u.k = t.k)`,
		`SELECT k, count(*), t.v IN (SELECT u.v FROM t AS u WHERE u.id > t.id) FROM t GROUP BY k`,
		`SELECT k, count(*) FROM t GROUP BY k ORDER BY (SELECT count(*) FROM t AS u WHERE u.k = t.k)`,
		// A body carrying a WINDOW function -- the region that used to be
		// refused wholesale and is now served by lifting the reference into
		// the batch (slotPlaceholder). Both halves must still agree, and the
		// two placeholder KINDS are separated: the anchor row (t.v) and the
		// finalized accumulator (the hoisted max(t.v)). The oracle gate for
		// the VALUES is compat-harness/agg_item_window_region_test.go; this
		// one is the seam's own differential, which is what the
		// aggItemProgramsDisabledForTest seam exists for.
		`SELECT k, count(*), (SELECT max(u.v + t.v) OVER () FROM t AS u LIMIT 1) FROM t GROUP BY k`,
		// NOT `SELECT k, count(*), (SELECT max(t.v) + count(*) OVER () FROM t
		// AS u LIMIT 1) FROM t GROUP BY k`: a PRE-EXISTING decline, unrelated
		// to the item compile ("aggregate inside a select-list subquery whose
		// argument belongs to the enclosing query"), which the old differential
		// admitted only because both halves raised it identically.
		`SELECT k, count(*), (SELECT sum(u.v) OVER (ORDER BY u.v + t.v) FROM t AS u LIMIT 1) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT count(*) OVER () FROM t AS u WHERE u.v = t.v LIMIT 1) FROM t GROUP BY k`,
		// ...and the pseudo-ROWID of the aggregate query's own table, the
		// other shape this round promoted. groupBareColExpr's isRowid form
		// reads the anchor row's trailing rowid block, not its column block.
		`SELECT k, count(*), (SELECT count(*) FROM t AS u WHERE u.id = t.rowid) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT t.rowid) FROM t GROUP BY k`,
		// An UNCORRELATED body: it takes a run-once cache slot, which the item
		// machine has to size (aggItemVM) rather than index off a nil slice.
		`SELECT count(*), (SELECT 5) FROM t`,
		`SELECT k, count(*), (SELECT max(v) FROM t AS u) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT 1) + (SELECT 2) FROM t GROUP BY k`,
		// A body whose OWN qualifier shadows the aggregate query's: "t.v"
		// inside "FROM t" is LOCAL and must NOT be substituted. The second
		// spelling is the one that MEASURES the rule -- the first puts the
		// reference inside an aggregate's argument, where the refused-region
		// guard would decline it anyway, so only this one fails if bodyColumn
		// stops honouring the local FROM-scope set.
		`SELECT k, count(*), (SELECT max(t.v) FROM t) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT t.v FROM t ORDER BY t.id LIMIT 1) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT t.s FROM t WHERE t.id = 3) FROM t GROUP BY k`,
		// Nested one level deeper: the inner body's reference to the aggregate
		// query is TWO frames out, so its OpOuterAggReg carries P5=2.
		`SELECT k, count(*), (SELECT (SELECT count(*) FROM t AS w WHERE w.v = t.v) FROM t AS u LIMIT 1) FROM t GROUP BY k`,
		// A body with its own FROM-clause derived table, and a compound body:
		// both restart the local-scope set, which is where a wrong rule would
		// substitute a name that is actually local.
		`SELECT k, count(*), (SELECT y FROM (SELECT u.v AS y FROM t AS u WHERE u.v = t.v) LIMIT 1) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT count(*) FROM t AS u WHERE u.v = t.v UNION ALL SELECT 9) FROM t GROUP BY k`,
		// A HOISTED aggregate: the call is written inside the body but belongs
		// to the ENCLOSING query, and is substituted by the placeholder
		// standing for its already-finalized value.
		`SELECT (SELECT count(v)) FROM t`,
		`SELECT (SELECT count(v) + 1) FROM t`,
		`SELECT k, (SELECT sum(v)) FROM t GROUP BY k`,
		// TWO bodies in one item, one carrying the hoist and one whose
		// textually identical call is its OWN. hoistsFor matches by AST
		// POINTER for exactly this: matching by shape would substitute the
		// enclosing query's finalized count into the inner query too.
		`SELECT (SELECT count(v)) + (SELECT count(v) FROM t AS u) FROM t`,
		`SELECT k, (SELECT sum(v)) - (SELECT sum(v) FROM t AS u) FROM t GROUP BY k`,
		// The one shape whose UNQUALIFIED name this compile declines
		// (selectStmt's emptyArmScope): window4.test 12.3's compound arm with
		// no scope of its own.
		`SELECT (SELECT avg(v) UNION SELECT min(v) OVER()) FROM t`,
		`SELECT k, (SELECT avg(v) UNION SELECT min(v) OVER()) FROM t GROUP BY k`,
		// The substituted placeholder carries the column's AFFINITY and
		// DECLARED collation: without them these comparisons rank differently.
		`SELECT k, count(*), (SELECT count(*) FROM t AS u WHERE u.s = t.v) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT count(*) FROM t AS u WHERE t.v = '2.5') FROM t GROUP BY k`,
		// The REFUSED REGIONS (vdbe_agg_item_subst.go's header): regions of a
		// body where a placeholder would be read against no group at all. The
		// first of these PANICKED before the guard existed -- window1.test#25,
		// "index out of range [0] with length 0" -- so it is pinned here as
		// well as in TestAggItemProgramRefusedRegions.
		`SELECT (0, 0) IN (SELECT MIN(v), NTILE(1) OVER()) FROM t`,
		`SELECT (0, 1) IN (SELECT MIN(v), NTILE(1) OVER()) FROM t`,
		`SELECT k, count(*), (SELECT sum(t.v + u.v) FROM t AS u) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT group_concat(u.s, t.s) FROM t AS u) FROM t GROUP BY k`,
		`SELECT k, avg(v) AS av FROM t GROUP BY k HAVING NOT EXISTS (SELECT x.k, avg(x.v) FROM t AS x GROUP BY x.k HAVING EXISTS (SELECT 1 WHERE av > 4.2))`,
		`SELECT k, count(*), (SELECT count(*) FROM t AS u GROUP BY u.k HAVING u.k = t.k) FROM t GROUP BY k`,
		`SELECT k, count(*), (SELECT count(*) FROM t AS u GROUP BY u.k HAVING count(*) > t.v) FROM t GROUP BY k`,
		// ...and the connection-state functions, which now compile because
		// connStateValue resolves them through the same chain evalCtx.connState
		// does (conn_state.go).
		`SELECT count(*), sqlite_version() FROM t`,
		`SELECT k, count(*), changes() FROM t GROUP BY k`,
		// A JSON aggregate's result subtype: kept for a whole-table aggregate,
		// lost for a grouped one, and produced INSIDE the item either way.
		`SELECT json_quote(max(value)) FROM json_each('[[7],[8]]')`,
		`SELECT json_quote(min(value)) FROM json_each('[[7]]') GROUP BY key`,
		`SELECT key, json_quote(json_array(key)) FROM json_each('[[7],[8]]') GROUP BY key`,
	} {
		both(t, p, sql)
	}

	q := aggItemCollFixture(t)
	for _, sql := range []string{
		// tkt3493: the key's declared collation and affinity must survive the
		// placeholder, or these compare BINARY.
		`SELECT a='abc', count(*) FROM t2 GROUP BY a`,
		`SELECT a>b, count(*) FROM t2 GROUP BY a, b`,
		`SELECT b>a, count(*) FROM t2 GROUP BY a, b`,
		`SELECT n='2', count(*) FROM t2 GROUP BY n`,
		// A bare column's DECLARED collation, the groupBareColExpr half.
		`SELECT c1 > c0, count(*) FROM t3 GROUP BY c0`,
		// An EXPLICIT COLLATE inside the aggregate's argument propagates out of
		// the call (sqlite3ExprCollSeq, expr.c:248) and onto groupAggExpr.coll.
		`SELECT max(a COLLATE nocase) = 'ABC' FROM t2`,
		`SELECT max(b COLLATE nocase) = 'DEF' FROM t2`,
		`SELECT max(b) = 'DEF' FROM t2`,
		// The SUBSTITUTED reference's own declared collation and affinity, the
		// half rewriteExprOuterRefs records two live wrong answers for when
		// either is dropped. Here the substitution happens inside a body.
		`SELECT a, count(*), (SELECT count(*) FROM t3 WHERE t3.c0 = t2.a) FROM t2 GROUP BY a`,
		`SELECT a, count(*), EXISTS (SELECT 1 FROM t3 WHERE t2.a = 'ABC') FROM t2 GROUP BY a`,
		`SELECT n, count(*), EXISTS (SELECT 1 FROM t3 WHERE t2.n = '2') FROM t2 GROUP BY n`,
	} {
		both(t, q, sql)
	}
}

// TestAggItemProgramGuards covers runnable's four decline arms. A guard nothing
// exercises is indistinguishable from one that does not work.
func TestAggItemProgramGuards(t *testing.T) {
	p := argRegsFixture(t)
	// This statement is chosen so the ONE item reads all four blocks: a
	// groupAggExpr (nAgg 1), a groupKeyExpr (nKey 1 -- a non-column GROUP BY
	// expression, since a bare key COLUMN resolves to the anchor block
	// instead), and the plan's anchor row and rowids. A fixture whose nKey was
	// 0 would skip the key guard silently -- which is exactly what the first
	// version of this test did, and what mutation-testing the guard found.
	plan := aggPlanOf(t, p, `SELECT max(v) + (k+1) FROM t GROUP BY k+1`)
	prog := plan.outPlans[0].prog
	if prog == nil || prog.prog == nil {
		t.Fatal("fixture item did not compile -- the rest of this test proves nothing")
	}
	if prog.nAgg == 0 || prog.nKey == 0 || prog.nCols == 0 || prog.nRowid == 0 {
		t.Fatalf("fixture must exercise all four blocks: nAgg=%d nKey=%d nCols=%d nRowid=%d",
			prog.nAgg, prog.nKey, prog.nCols, prog.nRowid)
	}
	agg := make([]Value, prog.nAgg)
	key := make([]Value, prog.nKey)
	col := make([]Value, prog.nCols)
	rid := make([]Value, prog.nRowid)
	m := &vdbe{pager: p}
	if !prog.runnable(m, agg, key, col, rid) {
		t.Fatal("the exact widths must be runnable")
	}
	var nilProg *aggItemProgram
	if nilProg.runnable(m, agg, key, col, rid) {
		t.Error("a nil program must not be runnable")
	}
	if (&aggItemProgram{}).runnable(m, agg, key, col, rid) {
		t.Error("a program that did not compile must not be runnable")
	}
	// A machine with NO PAGER used to be refused here, because it was the one
	// environment read where two evaluators of the same item could disagree
	// (evalCtx.encoding walks OUTER, vdbe.encoding does not). There is only one
	// evaluator now, so there is no disagreement to avoid and the program is
	// the only answer -- see runnable.
	if !prog.runnable(&vdbe{}, agg, key, col, rid) {
		t.Error("a machine with no pager must still be runnable")
	}
	// Each block one short: the program addresses them BY POSITION, so a short
	// input would answer from a cleared register -- a wrong value, not a panic.
	if prog.nAgg > 0 && prog.runnable(m, agg[:prog.nAgg-1], key, col, rid) {
		t.Error("a short accumulator slice must decline")
	}
	if prog.nKey > 0 && prog.runnable(m, agg, key[:prog.nKey-1], col, rid) {
		t.Error("a short group key must decline")
	}
	if prog.nCols > 0 && prog.runnable(m, agg, key, col[:prog.nCols-1], rid) {
		t.Error("a short anchor row must decline")
	}
	if prog.nRowid > 0 && prog.runnable(m, agg, key, col, rid[:prog.nRowid-1]) {
		t.Error("a short anchor rowid slice must decline")
	}
}

// TestAggResultRegOutsideAggCompile pins the other half of compiler.aggRegs'
// contract: the three placeholders are an ERROR in any compile that is not an
// aggregate-result compile, rather than a silent read of register 0. That is
// what compiler.winRegs states for windowResultExpr, and it is what keeps a
// placeholder from leaking into (say) a scan body's select list.
func TestAggResultRegOutsideAggCompile(t *testing.T) {
	c := &compiler{}
	for _, e := range []Expr{
		groupAggExpr{accIdx: 0},
		groupKeyExpr{idx: 0},
		groupBareColExpr{idx: 0},
		groupBareColExpr{isRowid: true, rowidTableIdx: 0},
	} {
		if _, err := c.compileExpr(e); err == nil {
			t.Errorf("%T compiled outside an aggregate result compile", e)
		}
	}
	// ...and an index past the block this program was built for is the same
	// answer, not a read of a neighbouring block.
	c.aggRegs = &aggResultRegs{agg: []int{3}, key: []int{4}, col: []int{5}, rowid: []int{6}}
	for _, e := range []Expr{
		groupAggExpr{accIdx: 1},
		groupKeyExpr{idx: 1},
		groupBareColExpr{idx: 1},
		groupBareColExpr{isRowid: true, rowidTableIdx: 1},
		groupKeyExpr{idx: -1},
	} {
		if _, err := c.compileExpr(e); err == nil {
			t.Errorf("%v: an out-of-range placeholder index must decline", e)
		}
	}
	for _, tc := range []struct {
		e    Expr
		want int
	}{
		{groupAggExpr{accIdx: 0}, 3},
		{groupKeyExpr{idx: 0}, 4},
		{groupBareColExpr{idx: 0}, 5},
		{groupBareColExpr{isRowid: true, rowidTableIdx: 0}, 6},
	} {
		r, err := c.compileExpr(tc.e)
		if err != nil || r != tc.want {
			t.Errorf("%v: got reg %d err %v, want reg %d", tc.e, r, err, tc.want)
		}
	}
}

// TestAggItemProgramRefusedRegions pins the guard that makes
// rewriteAggItemBodies decline in the regions of a subquery body where a
// placeholder could not be read at all -- the regions vdbe_agg_item_subst.go's
// header enumerates, where nothing supplies the group's register block. Only
// the fts5 auxiliary one is left; every case below is a region that CLOSED,
// kept as a case that must keep compiling.
//
// It is written from a real failure, not from caution. Without the guard,
// window1.test#25's "SELECT (0,0) IN (SELECT MIN(c0), NTILE(1) OVER()) FROM t0"
// PANICKED -- the hoisted MIN(c0) became a groupAggExpr, the body took the
// window path, and projectWindowRow read ctx.groupAggVals[0] out of
// an empty slice. Three panics in a whole-corpus sweep, in a
// tree whose fast gates were all green. A panic is the conformance gate's
// hardest failure (AGENTS.md invariant 2), so each region gets a statement
// here: delete its arm and this test panics or diverges rather than merely
// getting slower.
//
// A CLOSED region's statements are kept as want=true rather than deleted: this
// table is the record of which regions exist, so a region that stops existing
// is worth more here as a case that must keep compiling than as a case that is
// gone. Each closed the way a region is supposed to close -- the ARM it named
// was deleted first, and only then was the placeholder served:
// projectWindowRow's arm and the projection's buffering seam for the window
// PROJECTION, and aggItem.rowValue's arm and the buffered operand column for a
// window AGGREGATE's argument. Never by relaxing this pass on judgement.
func TestAggItemProgramRefusedRegions(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql  string
		want []bool
		why  string
	}{
		{`SELECT (0, 0) IN (SELECT MIN(v), NTILE(1) OVER()) FROM t`, []bool{true},
			"CLOSED REGION, and window1.test#25's own shape: a body whose SELECT level carries a WINDOW used to be a fourth region here, because projectWindowRow did not compile its select list at all. That arm is deleted, and the hoisted MIN(v)'s placeholder is now LIFTED into a batch column the scan body fills (slotPlaceholder / aggResultRegOrBuffer) -- C's own answer for a projection reference naming something outside the window query (selectWindowRewriteExprCb, window.c:756-771). The oracle gate for the value is compat-harness/agg_item_window_region_test.go"},
		{`SELECT k, count(*), (SELECT max(u.v) OVER () FROM t AS u WHERE u.v = t.v LIMIT 1) FROM t GROUP BY k`, []bool{true, true, true},
			"...and so does the WHERE of that same level, which the per-LEVEL flag used to cover even though a WHERE is compiled by the body's own scan compiler and was never at risk at all"},
		{`SELECT k, count(*), (SELECT sum(t.v + u.v) FROM t AS u) FROM t GROUP BY k`, []bool{true, true, true},
			"CLOSED REGION: a placeholder inside an AGGREGATE's own argument. aggItem.rowValue only walks that expression when the scan body stamped no register for it, and the body's scan stamps one (planAggArgRegs/emitAggArgRegs); the sorted drain, the one route that refused, now admits a placeholder for what it compiles to -- a REGISTER read, never a cursor read (aggDrainRow.lowerable). Oracle gate: compat-harness/agg_item_inner_agg_region_test.go's TestAggItemAggArgRegion"},
		{`SELECT k, count(*), (SELECT group_concat(u.s, t.s) FROM t AS u) FROM t GROUP BY k`, []bool{true, true, true},
			"...and its SEPARATOR, stamped as its own register by the same pass"},
		{`SELECT k, count(*), (SELECT count(*) FROM t AS u GROUP BY u.k HAVING u.k = t.k) FROM t GROUP BY k`, []bool{true, true, true},
			"CLOSED REGION: a placeholder inside the RESULT clauses of a body that is ITSELF an aggregate query. Those clauses become the INNER query's own itemPlans, so the placeholder is compiled against a block whose indices count the INNER query's accumulators -- which is why it used to decline. It now NAMES ITS OWNER (groupBareColExpr.owner, C's pExpr->pAggInfo) and aggResultReg walks past a block that is not it, so the read lands on the outer block one frame further out. The oracle gate for the value is compat-harness/agg_item_inner_agg_region_test.go"},
		{`SELECT k, count(*), (SELECT count(*) FROM t AS u WHERE u.v = t.v) FROM t GROUP BY k`, []bool{true, true, true},
			"CONTROL: the SAME aggregate body with the reference in its WHERE compiles -- a WHERE is the body's own scan compiler's, never an itemPlan, so the guard is per CLAUSE and not per body"},
		{`SELECT k, count(*), (SELECT json_group_array(t.v) OVER () FROM t AS u LIMIT 1) FROM t GROUP BY k`, []bool{true, true, true},
			"the LAST region is CLOSED and so is the last hole beneath it. json_group_array/json_group_object are not buffered at all (pWin->bExprArgs, window.c:1041-1044) -- their arguments are re-coded at STEP time -- so the placeholder never gets a batch column; compileWindowStepArgs now compiles that step-argument program WITH the scan's compiler as its outer chain, so the placeholder is served by the same cross-frame OpOuterAggReg every other one in the same body uses (compileWindowRowProgram's chainOuter). It had to be: there is no other arm left for an item that does not lower -- a decline is a hard error. Oracle gate: compat-harness/agg_item_window_agg_arg_test.go"},
	} {
		got := itemProgs(aggPlanOf(t, p, tc.sql))
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d items, want %d (%s)", tc.sql, len(got), len(tc.want), tc.why)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: item %d compiled=%v, want %v (%s)", tc.sql, i, got[i], tc.want[i], tc.why)
			}
		}
	}
}
