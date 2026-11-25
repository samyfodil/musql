package engine

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestCompoundMergeArmPlannedUnderItsOrderBy: an arm of a UNION, INTERSECT or
// EXCEPT is planned as multiSelectByMerge codes it, under the ORDER BY of
// every result column C invents (select.c:2997, :3462-3476). With TPC-DS's
// row counts that ORDER BY makes C's planner start q75's arm from item, not
// date_dim: the date_dim-first plan probes item and catalog_sales once per
// date, and ran q75 7x slower. A UNION ALL arm keeps its own plan.
func TestCompoundMergeArmPlannedUnderItsOrderBy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.musq")
	buildDB(t, path,
		`CREATE TABLE date_dim(d_date_sk INTEGER, d_year INTEGER)`,
		`CREATE TABLE item(i_item_sk INTEGER, i_brand_id INTEGER, i_category TEXT)`,
		`CREATE TABLE catalog_sales(cs_sold_date_sk INTEGER, cs_item_sk INTEGER, cs_order_number INTEGER, cs_quantity INTEGER)`,
		`CREATE TABLE catalog_returns(cr_item_sk INTEGER, cr_order_number INTEGER, cr_return_quantity INTEGER)`,
		`INSERT INTO date_dim SELECT value, 1998 + value % 5 FROM generate_series(1, 50)`,
		`INSERT INTO item SELECT value, value % 7, CASE value % 3 WHEN 0 THEN 'Books' ELSE 'Music' END FROM generate_series(1, 30)`,
		`INSERT INTO catalog_sales SELECT value % 50 + 1, value % 30 + 1, value, value % 9 FROM generate_series(1, 400)`,
		`INSERT INTO catalog_returns SELECT value % 30 + 1, value * 7, value % 4 FROM generate_series(1, 60)`,
		`ANALYZE`,
		`DELETE FROM sqlite_stat1`,
		`INSERT INTO sqlite_stat1 VALUES ('date_dim', NULL, '73049'), ('item', NULL, '9000'),
			('catalog_sales', NULL, '720352'), ('catalog_returns', NULL, '72007')`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const arm = `SELECT d_year, i_brand_id, cs_quantity - COALESCE(cr_return_quantity, 0)
		FROM catalog_sales JOIN item ON i_item_sk = cs_item_sk JOIN date_dim ON d_date_sk = cs_sold_date_sk
		LEFT JOIN catalog_returns ON cs_order_number = cr_order_number AND cs_item_sk = cr_item_sk
		WHERE i_category = 'Books'`
	// loops lists the tables in the order their loops are entered: the
	// order of the rewinds of the cursors that read them.
	loops := func(prog *Program) (got []string) {
		tables := map[int]string{}
		for _, in := range prog.Insns {
			if rt, ok := in.P4.(*resolvedTable); ok && in.Op == OpOpenRead {
				tables[in.P1] = rt.name
			}
		}
		for _, in := range prog.Insns {
			if name, ok := tables[in.P1]; ok && in.Op == OpRewind && !slices.Contains(got, name) {
				got = append(got, name)
			}
		}
		return got
	}
	for _, c := range []struct {
		op    string
		first string
	}{
		{"UNION", "item"},
		{"INTERSECT", "item"},
		{"EXCEPT", "item"},
		{"UNION ALL", "date_dim"},
	} {
		stmt, err := ParseSelect(arm + " " + c.op + " " + arm)
		if err != nil {
			t.Fatal(err)
		}
		pop := p.pushCTEScopeOf(stmt)
		prog, err := compileSubProgram(p, stmt, nil)
		pop()
		if err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
		if prog.Compound == nil {
			t.Fatalf("%s: not a compound program", c.op)
		}
		for i, a := range prog.Compound.arms {
			if got := loops(a); len(got) == 0 || got[0] != c.first {
				t.Errorf("%s arm %d loops over %v, want %s first", c.op, i, got, c.first)
			}
		}
	}
	_, rows, err := p.QueryArgs(`SELECT count(*) FROM (`+arm+` UNION `+arm+`)`, nil)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}
