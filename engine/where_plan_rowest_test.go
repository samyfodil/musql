package engine

// Tests row count estimation for WHERE plan selection.

import (
	"path/filepath"
	"testing"
)

// r39Build creates a one-table database ("t": INTEGER PRIMARY KEY a, plus b
// and c) with whatever extra DDL the caller supplies (e.g. a UNIQUE INDEX),
// and returns a read pager over it.
func r39Build(t *testing.T, extraDDL ...string) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "r39.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := append([]string{"CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)"}, extraDDL...)
	for _, s := range stmts {
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

// r39Plan mirrors wherePlanSingleTableIndexOrder's own construction of a
// wherePlanInput (where_plan_gate.go) up to the point of calling
// wherePlanSingleIndexKey, but calls (*wherePlanIdxBuild).plan() directly so
// the test can read back wherePlanResult.nRow -- a field
// wherePlanSingleIndexKey's own (*autoIndexKey, bool) return cannot carry.
func r39Plan(t *testing.T, p *ReadOnlyPager, sql string, wantDistinct bool) *wherePlanResult {
	t.Helper()
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	tbl, err := p.resolveTable("t")
	if err != nil {
		t.Fatalf("resolveTable: %v", err)
	}
	indexedBy := stmt.From[0].IndexedBy
	notIndexed := stmt.From[0].NotIndexed
	idxs, szTabRow, ok := wherePlanIndexList(p, tbl, "t", indexedBy, notIndexed, stmt)
	if !ok {
		t.Fatalf("%s: wherePlanIndexList declined", sql)
	}
	scope := tableScope{name: "t", tableName: "t", cols: tbl.cols, colIndex: buildColIndex(tbl.cols)}
	jts := []joinedTable{{tbl: tbl}}
	terms, ok := wherePlanTermsFrom(jts, []tableScope{scope}, stmt.Where)
	if !ok {
		t.Fatalf("%s: wherePlanTermsFrom declined", sql)
	}
	item := whereItem{
		cols: tbl.cols, tabName: "t", colUsed: ^uint64(0), colUsedOK: true,
		ipkIndex: tbl.ipkIndex, szTabRow: szTabRow, idxs: idxs,
		indexedBy: indexedBy != "", notIndexed: notIndexed,
	}
	b := &wherePlanIdxBuild{
		items: []whereItem{item}, terms: terms,
		sort:        &whereSortCtl{wantDistinct: wantDistinct},
		noAutoIndex: !p.AutomaticIndex(),
	}
	res, ok := b.plan()
	if !ok {
		t.Fatalf("%s: plan() declined", sql)
	}
	return res
}

// TestR39RowidEqualityIsExactlyOneRow: a rowid equality is whereShortCut's
// first shape, and it reports "pWInfo->nRowOut = 1" (where.c:6424) -- LogEst 1,
// nRowOut being a LogEst (whereInt.h:497), NOT the general solver's LogEst 0
// that a one-row loop's nOut telescopes to. This used to assert 0, from the
// solver, while this package ran no whereShortCut; it runs it now (shortCut).
func TestR39RowidEqualityIsExactlyOneRow(t *testing.T) {
	p := r39Build(t)
	res := r39Plan(t, p, "SELECT * FROM t WHERE a = 5", false)
	if len(res.loops) != 1 {
		t.Fatalf("loops = %v, want 1 level", res.loops)
	}
	if res.nRow != 1 {
		t.Errorf("nRow = %d, want 1 (whereShortCut's LogEst)", res.nRow)
	}
}

// TestR39UniqueIndexEqualityIsExactlyOneRow: whereShortCut's second shape, an
// equality on every key column of a UNIQUE index, reports the same LogEst 1.
func TestR39UniqueIndexEqualityIsExactlyOneRow(t *testing.T) {
	p := r39Build(t, "CREATE UNIQUE INDEX ib ON t(b)")
	res := r39Plan(t, p, "SELECT * FROM t WHERE b = 'x'", false)
	if len(res.loops) != 1 {
		t.Fatalf("loops = %v, want 1 level", res.loops)
	}
	if res.nRow != 1 {
		t.Errorf("nRow = %d, want 1 (whereShortCut's LogEst)", res.nRow)
	}
}

// TestR39DistinctReductionOnFullScan checks the plain where.c:7118-7121 case:
// no WHERE clause at all, so the winning loop is the ordinary sPk full scan
// (nOut == whereDefaultRowLogEst == 200, NOT one-row), and DISTINCT with no
// ORDER BY reduces the reported estimate by 30.
func TestR39DistinctReductionOnFullScan(t *testing.T) {
	p := r39Build(t)
	plain := r39Plan(t, p, "SELECT b FROM t", false)
	if plain.nRow != whereDefaultRowLogEst {
		t.Fatalf("plain scan nRow = %d, want %d", plain.nRow, whereDefaultRowLogEst)
	}
	distinct := r39Plan(t, p, "SELECT DISTINCT b FROM t", true)
	if want := whereDefaultRowLogEst - 30; distinct.nRow != want {
		t.Errorf("DISTINCT full-scan nRow = %d, want %d (== %d - 30)",
			distinct.nRow, want, whereDefaultRowLogEst)
	}
}

// TestR39DistinctSkipsReductionWhenShortCutWouldFire is the where.c:6425-6428
// gate: whereShortCut sets eDistinct = WHERE_DISTINCT_UNIQUE and never
// reaches the "-30" site at all when its own preconditions hold (single
// table, a full rowid or unique-index equality match, no INDEXED BY/NOT
// INDEXED). reportedRowEstimate must reproduce that -- NOT simply "the
// winning loop happens to be one-row" -- or a plain "WHERE pk = ? " DISTINCT
// query would under-report by 30 where C SQLite reports exactly 1.
func TestR39DistinctSkipsReductionWhenShortCutWouldFire(t *testing.T) {
	p := r39Build(t)
	res := r39Plan(t, p, "SELECT DISTINCT b FROM t WHERE a = 5", true)
	if res.nRow != 1 {
		t.Errorf("nRow = %d, want 1 -- whereShortCut takes the statement, so"+
			" the -30 reduction must not apply", res.nRow)
	}
}

// TestR39DistinctReductionAppliesUnderNotIndexed is the negative half of the
// gate above: NOT INDEXED disables whereShortCut outright (where.c:6367-6370,
// "if( pItem->fg.isIndexedBy || pItem->fg.notIndexed ) return 0;") even
// though the resulting loop -- the rowid full scan is the ONLY candidate left
// -- would itself still measure one-row for a rowid equality. C SQLite
// therefore takes the general solver for this statement and DOES apply the
// -30 reduction; a version of reportedRowEstimate that gated purely on "is
// the winning loop WHERE_ONEROW" would wrongly skip it here.
func TestR39DistinctReductionAppliesUnderNotIndexed(t *testing.T) {
	p := r39Build(t)
	res := r39Plan(t, p, "SELECT DISTINCT b FROM t NOT INDEXED WHERE a = 5", true)
	if res.nRow != -30 {
		t.Errorf("nRow = %d, want -30 -- NOT INDEXED disables whereShortCut,"+
			" so the general solver's DISTINCT reduction must still apply"+
			" on top of the still-one-row rowid scan", res.nRow)
	}
}

// TestR39DistinctReductionAppliesUnderIndexedBy is the INDEXED BY half of the
// same gate: an explicit INDEXED BY disables whereShortCut too
// (where.c:6367-6370), even naming the very index that fully matches.
func TestR39DistinctReductionAppliesUnderIndexedBy(t *testing.T) {
	p := r39Build(t, "CREATE UNIQUE INDEX ib ON t(b)")
	res := r39Plan(t, p, "SELECT DISTINCT b FROM t INDEXED BY ib WHERE b = 'x'", true)
	if res.nRow != -30 {
		t.Errorf("nRow = %d, want -30 -- INDEXED BY disables whereShortCut,"+
			" so the general solver's DISTINCT reduction must still apply"+
			" on top of the still-one-row indexed scan", res.nRow)
	}
}
