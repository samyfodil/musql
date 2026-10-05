package engine

import (
	"path/filepath"
	"testing"
)

// TestR41WherePlanIndexListPureExprIndexMultiTable tests pure expression indexes
// with multi-table queries.
func TestR41WherePlanIndexListPureExprIndexMultiTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r41.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t1(a INT, b INT, c INT)",
		"CREATE INDEX t1x ON t1(a+0, c)", // pure expression index: a+0, plain c
		"CREATE TABLE t2(y)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	tbl, err := p.resolveTable("t1")
	if err != nil {
		t.Fatal(err)
	}

	multi, err := ParseSelect("SELECT t2.y, SUM(t1.b) FROM t1 CROSS JOIN t2")
	if err != nil {
		t.Fatal(err)
	}
	idxs, _, ok := wherePlanIndexList(p, tbl, "t1", "", false, multi)
	if !ok {
		t.Fatal("wherePlanIndexList declined a pure-expression index for a multi-table stmt -- round 41's fix regressed")
	}
	found := false
	for _, ix := range idxs {
		if ix.name == "t1x" {
			found = true
			if len(ix.exprs) == 0 || ix.exprs[0] == nil {
				t.Errorf("t1x's leading key column lost its XN_EXPR representation: exprs=%v", ix.exprs)
			}
		}
	}
	if !found {
		t.Error("t1x missing from the multi-table chain entirely")
	}

	// The SAME query, single-table (just t1), for comparison: this path
	// already worked before round 41 and must still.
	single, err := ParseSelect("SELECT SUM(t1.b) FROM t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := wherePlanIndexList(p, tbl, "t1", "", false, single); !ok {
		t.Fatal("wherePlanIndexList declined a pure-expression index for a single-table stmt -- pre-existing behavior broke")
	}
}

// TestR41CoveringFallbackStaysSingleTableOnly pins the SEPARATE, narrower
// restriction round 41 deliberately did NOT lift: stmtCoveredByExprIndex
// (where_plan_exprindex_cover.go), the query-aware isCovering fallback for a
// PURE expression index. Its own colIndexed closure below matches a bare
// ColumnExpr by NAME ALONE against t1's own columns -- it never reads
// x.Qualifier at all (see exprCoveredForIndexScan, where_plan_exprindex_cover.go)
// -- so with a SECOND FROM item present whose column happens to share t1's
// name, an unqualified (or differently-qualified) reference to THAT column
// would be misread as "covered by t1's own index": a wrong isCovering=true a
// solver could act on, not a missed optimization. Verified end-to-end against
// the real oracle (compat-harness/multitable_exprindex_r41_test.go's
// TestR41MultiTablePartialIndexOnClauseStaysDeclined proves the SIBLING
// partial-index hazard this same len(stmt.From)==1 gating pattern exists
// for); this test pins the mechanism directly since the covering fallback's
// own cost-tie interaction is not reliably forced through an end-to-end
// query.
//
// t1 and t2 BOTH declare a column named "m", and t1x's key list DOES include
// t1's own "m" as a plain (non-expression) column -- so colIndexed("m")
// legitimately answers true for a reference to t1's OWN m. The hazard is a
// DIFFERENT table's column of the identical name: colIndexed never inspects
// which table the reference actually named, so a bare (or t2-qualified)
// reference to t2.m is indistinguishable, from inside that closure, from one
// to t1.m.
func TestR41CoveringFallbackStaysSingleTableOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r41cover.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t1(a INT, m INT)",
		"CREATE INDEX t1x ON t1(a+0, m)", // pure expression index; its SECOND key column plainly covers t1's own "m"
		"CREATE TABLE t2(m INT)",         // a DIFFERENT table's column, same name
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	tbl, err := p.resolveTable("t1")
	if err != nil {
		t.Fatal(err)
	}

	// t2.m, not t1.m: this must NOT make t1x look covering.
	stmt, err := ParseSelect("SELECT t2.m FROM t1 CROSS JOIN t2")
	if err != nil {
		t.Fatal(err)
	}
	idxs, _, ok := wherePlanIndexList(p, tbl, "t1", "", false, stmt)
	if !ok {
		t.Fatal("wherePlanIndexList declined outright -- expected it to represent t1x, just not as covering")
	}
	for _, ix := range idxs {
		if ix.name != "t1x" {
			continue
		}
		if ix.isCovering {
			t.Fatalf("t1x wrongly marked isCovering=true for a MULTI-table stmt: colIndexed matched t2.m's bare name %q against t1's OWN unrelated column of the same name, ignoring that the reference names a different table entirely", "m")
		}
	}

	// Positive control: the SAME name, single-table (t2 not in the FROM
	// clause at all, so "m" can only mean t1.m, and t1x's second key column
	// genuinely IS t1's own m) -- t1x MUST be marked covering here. Without
	// this the negative assertion above would be trivially true for the
	// wrong reason (a fallback that never marks anything covering, rather
	// than one that correctly distinguishes t1.m from t2.m).
	single, err := ParseSelect("SELECT m FROM t1")
	if err != nil {
		t.Fatal(err)
	}
	idxs2, _, ok := wherePlanIndexList(p, tbl, "t1", "", false, single)
	if !ok {
		t.Fatal("wherePlanIndexList declined outright on the single-table control")
	}
	singleCovering := false
	for _, ix := range idxs2 {
		if ix.name == "t1x" {
			singleCovering = ix.isCovering
		}
	}
	if !singleCovering {
		t.Fatal("t1x NOT marked isCovering for the single-table control, where t1's own \"m\" genuinely is t1x's second key column -- the fallback mechanism itself is not firing, so the negative assertion above proves nothing")
	}
}
