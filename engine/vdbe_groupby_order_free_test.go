package engine

import (
	"fmt"
	"testing"
)

// TestGroupByOrderFreeSkipsAutoIndexSort: a GROUP BY whose result cannot see
// the order rows arrive in within a group -- counts only, grouped by the
// outermost table -- drops the automatic index's sort of an inner table, and
// answers as the sorted form does. A grouping term on the inner table, or an
// order-sensitive aggregate, keeps the sort.
func TestGroupByOrderFreeSkipsAutoIndexSort(t *testing.T) {
	stmts := []string{
		`CREATE TABLE c (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE o (id INTEGER PRIMARY KEY, cid INTEGER, k INTEGER, s TEXT)`,
	}
	for i := 0; i < 200; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO c VALUES (%d, 'c%d')`, i, i%17))
	}
	for i := 0; i < 2000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO o VALUES (%d, %d, %d, 's%d')`, 5000-i, (i*7)%230, i%3, i%11))
	}
	p := newSegPair(t, stmts...)
	sorts := func(q string) bool {
		t.Helper()
		_, plan, err := p.fast(`EXPLAIN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range plan {
			if string(r[1].S) == "AutoIndexOrder" {
				return true
			}
		}
		return false
	}
	const join = ` FROM c LEFT JOIN o ON c.id = o.cid AND o.s NOT LIKE '%1%'`
	free := `SELECT c.id, count(o.id), count(*)` + join + ` GROUP BY c.id`
	sorted := `SELECT c.id, count(o.id), count(*), group_concat(o.id)` + join + ` GROUP BY c.id`
	if sorts(free) {
		t.Errorf("%s still sorts its inner table", free)
	}
	for _, q := range []string{sorted, `SELECT c.id, o.k, count(*)` + join + ` GROUP BY c.id, o.k`} {
		if !sorts(q) {
			t.Fatalf("%s does not sort its inner table, so this test compares nothing", q)
		}
	}
	got, want := p.mustFast(free), p.mustFast(sorted)
	if len(got) != len(want) {
		t.Fatalf("%d groups, sorted form %d", len(got), len(want))
	}
	for i := range got {
		if fmt.Sprint(typedRows(got[i:i+1])) != fmt.Sprint(typedRows([][]Value{want[i][:3]})) {
			t.Fatalf("group %d: %v, sorted form %v", i, typedRows(got[i:i+1]), typedRows(want[i:i+1]))
		}
	}
}
