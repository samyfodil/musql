package engine

import "testing"

// TestNoFromReadIsPlanCached: a FROM-less read keeps its plan for its text;
// one with GROUP BY does not.
func TestNoFromReadIsPlanCached(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	if err := db.Exec(`CREATE TABLE t(k INTEGER)`); err != nil {
		t.Fatal(err)
	}
	query := func(q string, args ...Value) [][]Value {
		t.Helper()
		p, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		_, rows, err := p.QueryArgs(q, args)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return rows
	}
	cached := func(q string) bool { return db.plans != nil && db.plans.m[q] != nil }

	for _, q := range []string{"SELECT 1, 'x'", "SELECT ?1 + 1, ?2", "SELECT (SELECT count(*) FROM t), random()"} {
		query(q, Value{Typ: Int, I: 1}, Value{Typ: Text, S: []byte("a")})
		if !cached(q) {
			t.Errorf("%s: no plan kept", q)
		}
	}
	for _, n := range []int64{5, -3, 40} {
		rows := query("SELECT ?1 + 1, ?2", Value{Typ: Int, I: n}, Value{Typ: Text, S: []byte("b")})
		if len(rows) != 1 || rows[0][0].I != n+1 || string(rows[0][1].S) != "b" {
			t.Fatalf("SELECT ?1 + 1, ?2 bound %d: %v", n, rows)
		}
	}
	const grouped = "SELECT 1 GROUP BY abs(?1)"
	query(grouped, Value{Typ: Int, I: 1})
	if cached(grouped) {
		t.Errorf("%s: a plan was kept", grouped)
	}
}
