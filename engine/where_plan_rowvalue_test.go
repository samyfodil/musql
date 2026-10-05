package engine

// The WhereClause the planner builds for a desugared row value, checked term by
// term against the one exprAnalyze builds for the vector comparison it was
// written as (where_plan_rowvalue.go). compat-harness/rowvalue_plan_test.go
// checks the ORDERS these produce against the oracle; a unit's difference in
// one term's nOut shows there only where two plans happen to be that close, so
// the structure itself is pinned here, each expectation read off whereexpr.c.

import (
	"path/filepath"
	"testing"
)

// rowPlanFixture builds t(a, b, c, d), u(x, y, z) and w(a INTEGER, s TEXT
// COLLATE NOCASE, n INTEGER), and answers the planner's FROM-clause view of
// the named tables, in order.
func rowPlanFixture(t *testing.T, names ...string) ([]joinedTable, []tableScope) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rowplan.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE TABLE u(x, y, z)",
		"CREATE TABLE w(a INTEGER, s TEXT COLLATE NOCASE, n INTEGER)",
		"CREATE TABLE v(x INTEGER, y INTEGER, z INTEGER)",
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
	var jts []joinedTable
	var scopes []tableScope
	for _, n := range names {
		tbl, err := p.resolveTable(n)
		if err != nil {
			t.Fatalf("resolveTable %s: %v", n, err)
		}
		jts = append(jts, joinedTable{tbl: tbl})
		scopes = append(scopes, tableScope{name: n, tableName: n, cols: tbl.cols, colIndex: buildColIndex(tbl.cols)})
	}
	return jts, scopes
}

func rowPlanWhere(t *testing.T, sql string) Expr {
	t.Helper()
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return stmt.Where
}

func rowPlanTerms(t *testing.T, jts []joinedTable, scopes []tableScope, where Expr) []whereIdxTerm {
	t.Helper()
	terms, ok := wherePlanTermsFrom(jts, scopes, where)
	if !ok {
		t.Fatal("wherePlanTermsFrom declined")
	}
	return terms
}

// termShape is the part of a WhereTerm each case pins.
type termShape struct {
	op     uint16
	cursor int
	column int
	virt   bool
	parent int
	iField int
}

func checkTerms(t *testing.T, what string, got []whereIdxTerm, want []termShape) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d terms, want %d: %+v", what, len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		have := termShape{op: g.op, cursor: g.cursor, column: g.column, virt: g.virt, parent: g.parent, iField: g.iField}
		if have != w {
			t.Errorf("%s: term %d = %+v, want %+v", what, i, have, w)
		}
	}
}

func TestRowValueWhereTerms(t *testing.T) {
	jts, scopes := rowPlanFixture(t, "t")
	const b, c, d = 1, 2, 3

	// tag-20220128a: the equality stays ONE base term, disabled (TERM_VIRTUAL,
	// WO_ROWVAL) once its slices are appended -- after every term the
	// backwards walk analysed before it, which is c > 5 here -- and the
	// slices are neither virtual nor children.
	checkTerms(t, "vector ==", rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (b, d) = (2, 3) AND c > 5")),
		[]termShape{
			{op: woRowval, cursor: -1, column: xnRowid, virt: true, parent: -1},
			{op: woGT, cursor: 0, column: c, parent: -1},
			{op: woEq, cursor: 0, column: b, parent: -1},
			{op: woEq, cursor: 0, column: d, parent: -1},
		})

	// whereexpr.c:1491-1520: the IN term indexes nothing itself; one virtual
	// child per field, each its child, u.x.iField its 1-based position.
	inTerms := rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (b, d) IN ((1, 1), (2, 2))"))
	checkTerms(t, "vector IN", inTerms, []termShape{
		{op: 0, cursor: -1, column: xnRowid, parent: -1},
		{op: woIn, cursor: 0, column: b, virt: true, parent: 0, iField: 1},
		{op: woIn, cursor: 0, column: d, virt: true, parent: 0, iField: 2},
	})
	if inTerms[1].inCount != -1 {
		t.Errorf("vector IN field inCount = %d, want -1 (priced as IN (SELECT ...))", inTerms[1].inCount)
	}
	// A one-row list is still "IN (VALUES ...)" (parse.y:1531).
	checkTerms(t, "vector IN, one row", rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (b, d) IN ((1, 1))")),
		[]termShape{
			{op: 0, cursor: -1, column: xnRowid, parent: -1},
			{op: woIn, cursor: 0, column: b, virt: true, parent: 0, iField: 1},
			{op: woIn, cursor: 0, column: d, virt: true, parent: 0, iField: 2},
		})

	// A vector range is a range on its FIRST element's column; commuted in
	// place when that element is on the right.
	checkTerms(t, "vector range", rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (30, 2) < (c, d)")),
		[]termShape{{op: woGT, cursor: 0, column: c, parent: -1}})
	// ... and "(c COLLATE nocase, d)" is on no column (whereexpr.c:1080).
	checkTerms(t, "vector range, COLLATE first", rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (c COLLATE nocase, d) > (30, 2)")),
		[]termShape{{op: 0, cursor: -1, column: xnRowid, parent: -1}})

	// BETWEEN's two children are vector ranges (whereexpr.c:1302).
	checkTerms(t, "vector BETWEEN", rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (c, d) BETWEEN (10, 0) AND (20, 9)")),
		[]termShape{
			{op: 0, cursor: -1, column: xnRowid, parent: -1},
			{op: woGE, cursor: 0, column: c, virt: true, parent: 0},
			{op: woLE, cursor: 0, column: c, virt: true, parent: 0},
		})

	// A vector equality defines no constant (findConstInWhere reads TK_EQ over
	// a COLUMN only): "c = b" keeps b a column.
	cw, _, ok := wherePlanPropagateConstants(jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (b, d) = (2, 3) AND c = b"))
	if !ok {
		t.Fatal("constant propagation declined")
	}
	cs := whereSplitTopAnd(cw)
	if len(cs) != 2 {
		t.Fatalf("propagated WHERE split into %d conjuncts, want 2", len(cs))
	}
	if be, isBin := cs[1].(BinaryExpr); !isBin {
		t.Errorf("c = b became %T", cs[1])
	} else if _, fixed := be.R.(whereFixedCol); fixed {
		t.Errorf("c = b: b was fixed from a row-value equality")
	}

	// A WO_AND disjunct's nBase reaches its last slice (whereexpr.c:79), and
	// the sliced original -- TERM_VIRTUAL -- is in no pAndExpr.
	or1 := rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (b, a) = (1, 6) OR c = 5"))
	if or1[0].orInfo == nil || or1[0].orInfo.and[0] == nil {
		t.Fatalf("(b, a) = (1, 6) OR c = 5: no WO_AND disjunct: %+v", or1[0])
	}
	if n := or1[0].orInfo.and[0].nBase; n != 3 {
		t.Errorf("WO_AND disjunct nBase = %d, want 3 (virtual original, two slices)", n)
	}
	or2 := rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE (c < 3 OR d = 6) AND (a, b) = (5, 0)"))
	if or2[0].orInfo == nil {
		t.Fatalf("(c < 3 OR d = 6): not a WHERE_MULTI_OR term: %+v", or2[0])
	}
	if n := len(or2[0].orInfo.others); n != 0 {
		t.Errorf("pAndExpr holds %d terms, want 0 (the sliced original is TERM_VIRTUAL)", n)
	}
}

// TestRowValueFixedColumns: propagateConstants walks into a TK_VECTOR and fixes
// a column there as anywhere -- with bIgnoreAffBlob, since no comparison arm
// names a vector (select.c propagateConstantExprRewrite), so only a column of
// real affinity is fixed once a BLOB one defined a constant. A fixed column
// then costs its term no prerequisite (sqlite3WhereExprUsageNN), and heading
// the RIGHT vector it still commutes: EP_FixedCol is tested on pRight, the
// TK_VECTOR (whereexpr.c:1223).
func TestRowValueFixedColumns(t *testing.T) {
	jts, scopes := rowPlanFixture(t, "w")
	fixedWhere, _, ok := wherePlanPropagateConstants(jts, scopes, rowPlanWhere(t, "SELECT * FROM w WHERE n = 2 AND (a, s) > (n, 'x')"))
	if !ok {
		t.Fatal("constant propagation declined")
	}
	checkTerms(t, "fixed first element", rowPlanTerms(t, jts, scopes, fixedWhere), []termShape{
		{op: woEq, cursor: 0, column: 2, parent: -1},
		{op: woGT, cursor: 0, column: 0, parent: -1},
		{op: woLT, cursor: 0, column: 2, virt: true, parent: 1},
	})

	// BLOB-affinity t.b defines the constant, so the BLOB column inside the
	// vector is left alone -- and the term, reading t on both sides, gets
	// only WO_EQUIV's mask: no operator.
	jts, scopes = rowPlanFixture(t, "t")
	blobWhere, _, ok := wherePlanPropagateConstants(jts, scopes, rowPlanWhere(t, "SELECT * FROM t WHERE b = 2 AND (c, d) > (b, 1)"))
	if !ok {
		t.Fatal("constant propagation declined")
	}
	checkTerms(t, "BLOB first element", rowPlanTerms(t, jts, scopes, blobWhere), []termShape{
		{op: woEq, cursor: 0, column: 1, parent: -1},
		{op: 0, cursor: 0, column: 2, parent: -1},
		{op: 0, cursor: 0, column: 1, virt: true, parent: 1},
	})

	// Across tables the difference is a prerequisite: u.x stays u's inside the
	// vector, though the desugared tree -- whose comparisons the bHasAffBlob
	// arm does rewrite -- has it fixed.
	jts, scopes = rowPlanFixture(t, "t", "u")
	blobJoin, _, ok := wherePlanPropagateConstants(jts, scopes, rowPlanWhere(t,
		"SELECT * FROM t, u WHERE u.x = 2 AND (t.c, t.d) > (u.x, 1)"))
	if !ok {
		t.Fatal("constant propagation declined")
	}
	bj := rowPlanTerms(t, jts, scopes, blobJoin)
	if len(bj) < 2 || bj[1].prereqAll != 3 || bj[1].prereqRight != 2 {
		t.Errorf("BLOB vector across tables: want term 1 prereqAll 11, prereqRight 10, got %+v", bj)
	}

	jts, scopes = rowPlanFixture(t, "w", "v")
	where, _, ok := wherePlanPropagateConstants(jts, scopes, rowPlanWhere(t,
		"SELECT * FROM w, v WHERE v.x = 2 AND (w.a, w.n) > (v.x, 1)"))
	if !ok {
		t.Fatal("constant propagation declined")
	}
	terms := rowPlanTerms(t, jts, scopes, where)
	if len(terms) < 2 || terms[1].cursor != 0 || terms[1].column != 0 {
		t.Fatalf("want the vector range on w.a as term 1, got %+v", terms)
	}
	if terms[1].prereqRight != 0 {
		t.Errorf("vector range prereqRight = %b, want 0 (v.x is fixed)", terms[1].prereqRight)
	}
}

// TestRowValueCollation: sqlite3BinaryCompareCollSeq over two vectors (expr.c:
// 424) tests EP_Collate on each whole vector but reads the collation off its
// first element only.
func TestRowValueCollation(t *testing.T) {
	jts, scopes := rowPlanFixture(t, "w")
	for _, c := range []struct{ where, want string }{
		{"('b', n) < (s, 5)", "NOCASE"},                // no COLLATE: s's declared
		{"('b', n COLLATE binary) < (s, 5)", "BINARY"}, // the left vector answers, from 'b'
		{"(s, n) > ('b', 3)", "NOCASE"},
		{"(s, n) > ('b', 3 COLLATE binary)", "BINARY"}, // the right vector answers, from 'b': none
	} {
		terms := rowPlanTerms(t, jts, scopes, rowPlanWhere(t, "SELECT * FROM w WHERE "+c.where))
		if !equalFoldName(terms[0].coll, c.want) {
			t.Errorf("%s: collation %q, want %q", c.where, terms[0].coll, c.want)
		}
	}
}
