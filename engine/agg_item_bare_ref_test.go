package engine

// Tests aggregate item bare column reference resolution.

import (
	"path/filepath"
	"testing"
)

// bareRefFixture is a schema with the four scope shapes the rule turns on: a
// single-table aggregate source (t), a second table for the bodies to select
// from (b), a pair that can be joined plainly or with USING, and a name (n)
// that belongs to the BODY's table and to nothing else.
func bareRefFixture(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "br.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(k, v, s)`,
		`INSERT INTO t VALUES(1,10,'a'),(1,11,'b'),(2,12,'c')`,
		`CREATE TABLE b(n)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
		`CREATE TABLE j1(g, m)`,
		`INSERT INTO j1 VALUES(1,10)`,
		`CREATE TABLE j2(g, q)`,
		`INSERT INTO j2 VALUES(1,20)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
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

// TestAggItemBareRefCompiles pins which bodies compile once the aggregate
// query's FROM items are published as register scopes over the anchor row
// (aggAnchorRegScopesUsable / compileAggItemProgram), and which still decline.
//
// Before those scopes existed EVERY case here declined, including the ones a
// bare name has nothing to do with: aggItemSubst.bodyColumn's test was
// "err != nil || col != nil" against a probe that never returns a nil col
// without an error, so it was true for every unqualified reference in every
// body.
func TestAggItemBareRefCompiles(t *testing.T) {
	p := bareRefFixture(t)
	for _, tc := range []struct {
		sql  string
		want []bool
		why  string
	}{
		{`SELECT k, count(*), (SELECT count(*) FROM b WHERE n > v) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"a bare name the AGGREGATE query owns: resolved against the anchor-row register scope, one query level out, and read with OpOuterAggReg"},
		{`SELECT k, count(*), (SELECT count(*) FROM b WHERE n > 0) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"a bare name the BODY owns: never this pass's business at all, and it used to decline the item anyway"},
		{`SELECT k, count(*), (SELECT sum(n) FROM b) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"...including inside the body's own AGGREGATE argument, once a refused region: the region only matters for a name naming the GROUP, and this one cannot"},
		{`SELECT k, count(*), (SELECT count(*) FROM b WHERE n > rowid) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"the anchor row's ROWID, spelled bare"},
		{`SELECT k, count(*), (SELECT (SELECT count(*) FROM b WHERE n > v)) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"two frames out: the walk is per query level, so the level count comes out of the chain rather than being assumed"},

		{`SELECT k, count(*), (SELECT sum(v + n) FROM b) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"CLOSED REGION: a bare name naming the GROUP inside an AGGREGATE's argument. It used to decline because aggItem.rowValue walks that expression per row whenever the scan body stamped no register for it (sql_agg.go). The body's scan DOES stamp one -- planAggArgRegs/emitAggArgRegs compile it, and the reference resolves against the item's anchor-row register scope one level out -- and the one route that did not, the sorted drain, now admits the read for what it is: a REGISTER, never a cursor (aggDrainRow.lowerable). The oracle gate for the value is compat-harness/agg_item_inner_agg_region_test.go's TestAggItemAggArgRegion"},

		// ...and the exclusions, each for a stated reason.
		{`SELECT k, count(*), (SELECT max(n) OVER () FROM b LIMIT 1) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"CONTROL for the next case: the same window body with no reference to the group compiles"},
		{`SELECT k, count(*), (SELECT max(n + v) OVER () FROM b LIMIT 1) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"CLOSED REGION, and the LAST one an aggregate argument had: a bare name naming the group inside a WINDOW aggregate's argument. It declined while aggItem.rowValue still walked an operand the scan body stamped no register for; with that arm deleted the operand is either lowered into a batch column here (it is: planWindowOperands takes \"n + v\" and emitWindowOperands compiles it in the window query's own chain, which reaches the item's register block) or the whole item compile fails at windowAggOperandsServed. The oracle gate for the value is compat-harness/agg_item_window_agg_arg_test.go"},
		{`SELECT k, count(*), (SELECT max(n) OVER () + v FROM b LIMIT 1) FROM t GROUP BY k`,
			[]bool{true, true, true},
			"...and this is the window region's actual win: the same reference in the PROJECTION, outside the aggregate call, is lifted into a batch column by windowBufCols and resolved down in the scan body -- C's own lift (selectWindowRewriteExprCb, window.c:756-771)"},
		{`SELECT g, count(*), (SELECT count(*) FROM b WHERE n > g) FROM j1 JOIN j2 USING(g) GROUP BY g`,
			[]bool{true, true, true},
			"a USING join, which used to decline TWICE over: aggAnchorRegScopesUsable refused any scope carrying a coalesced map, and resolveRowReg then counted BOTH copies of the common column. Neither is C. coalesced is not a coalesce() -- coalesceFallback is, for RIGHT/FULL -- it is the USING/NATURAL left-most-wins bookkeeping, and the register block represents it exactly because every column keeps its own offset and only NAME resolution differs. C counts a second match only when the FROM item is not a USING join or the column is not in its USING list (resolve.c:438-446); an INNER or LEFT join \"continue\"s and keeps the left-most, leaving cnt at 1 (resolve.c:447-449). The oracle answers this statement [1 1 2]; before the fix this engine answered nothing at all"},
		{`SELECT j1.g, count(*), (SELECT count(*) FROM b WHERE n > j1.m) FROM j1, j2 GROUP BY j1.g`,
			[]bool{true, true, true},
			"CONTROL: the same plain join with an UNambiguous reference still compiles, so the case above measures the ambiguity and not the join"},
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

// TestAggItemBareRefAmbiguousDeclines is the USING case's sibling, split out of
// the table above because it no longer produces a plan at all.
//
// A plain join whose two tables both declare g makes the bare name genuinely
// AMBIGUOUS: there is no USING clause pairing the two columns, so C lets cnt go
// above 1 (resolve.c:438-443) and reports "ambiguous column name"
// (resolve.c:784). Verified against 3.53.3 -- C SQLite errors on this
// statement, so this engine erroring is agreement, not a gap.
//
// It used to be a row in the table above with want=false, meaning "this ITEM
// declines to an item program". That distinction is gone: a declined item used
// to be answered another way, and a decline is now a hard error
// for the whole statement, which is what RULE #1 asks for. So the assertion
// moved from "this item does not compile" to "this statement does not compile",
// which is the honest form of it.
func TestAggItemBareRefAmbiguousDeclines(t *testing.T) {
	p := bareRefFixture(t)
	const sql = `SELECT j1.g, count(*), (SELECT count(*) FROM b WHERE n > g) FROM j1, j2 GROUP BY j1.g`
	if _, _, err := p.QueryArgs(sql, nil); err == nil {
		t.Fatalf("%s: compiled and answered, but C SQLite reports \"ambiguous column name: g\" "+
			"(resolve.c:784) -- answering here would be a wrong answer, not a wider capability", sql)
	}
	// ...and the control, so the case above measures the AMBIGUITY, not the join.
	const ok = `SELECT j1.g, count(*), (SELECT count(*) FROM b WHERE n > j1.m) FROM j1, j2 GROUP BY j1.g`
	if _, _, err := p.QueryArgs(ok, nil); err != nil {
		t.Fatalf("%s: %v -- the same plain join with an UNambiguous reference must still compile", ok, err)
	}
}
