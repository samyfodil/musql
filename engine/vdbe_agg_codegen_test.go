package engine

import "testing"

// aggUnstampedFor compiles sql and returns the aggregate expressions that
// were not lowered to compiled code.
func aggUnstampedFor(t *testing.T, db *Session, sql string) []string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, plan := compiledAgg(t, p, sql)
	return unstampedOperands(plan)
}

// TestAggDrainServesKeywordLiteral tests that bare TRUE/FALSE aggregate
// arguments are handled correctly without requiring an unlowered expression path.
func TestAggDrainServesKeywordLiteral(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ddl, ins      string
		query         string
		wantUnlowered bool
	}{
		{"max TRUE", "CREATE TABLE t(k,v)", "INSERT INTO t VALUES(1,10),(1,20),(2,30)",
			"SELECT k, max(TRUE) FROM t GROUP BY k ORDER BY k", false},
		{"sum FALSE", "CREATE TABLE t(k,v)", "INSERT INTO t VALUES(1,10),(1,20),(2,30)",
			"SELECT k, sum(FALSE) FROM t GROUP BY k ORDER BY k", false},
		{"TRUE inside an expression", "CREATE TABLE t(k,v)", "INSERT INTO t VALUES(1,10),(1,20),(2,30)",
			"SELECT k, min(TRUE+v) FROM t GROUP BY k ORDER BY k", false},
		// Control: a real column named "true" shadows the keyword.
		{"real column shadows", `CREATE TABLE t("true" INT, k)`, "INSERT INTO t VALUES(7,1),(8,1)",
			"SELECT k, max(true) FROM t GROUP BY k ORDER BY k", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range []string{tc.ddl, tc.ins} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
			}
			rvdQuery(t, db, tc.query)
			if got := aggUnstampedFor(t, db, tc.query); (len(got) != 0) != tc.wantUnlowered {
				t.Errorf("%q left %v unlowered, want unlowered=%v -- a bare TRUE/FALSE needs no "+
					"row-block slot at all; compileColumn serves it from FallbackLiteral", tc.query, got, tc.wantUnlowered)
			}
		})
	}
}

// TestAggDrainServesCoalescedColumn tests aggregate operations on coalesced
// columns from RIGHT/FULL JOINs, which require special handling.
func TestAggDrainServesCoalescedColumn(t *testing.T) {
	for _, tc := range []struct {
		name, query   string
		wantUnlowered bool
	}{
		{"full join", "SELECT k, sum(k) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k", false},
		{"right join", "SELECT k, sum(k) FROM a RIGHT JOIN b USING(k) GROUP BY k ORDER BY k", false},
		{"natural full join", "SELECT k, sum(k) FROM a NATURAL FULL JOIN b GROUP BY k ORDER BY k", false},
		{"three-way chain", "SELECT k, sum(k), count(*) FROM a FULL JOIN b USING(k) FULL JOIN c USING(k) GROUP BY k ORDER BY k", false},
		{"coalesced beside plain columns", "SELECT k, sum(k), sum(x), sum(y) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k", false},
		{"distinct over the coalesced column", "SELECT k, count(DISTINCT k) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k", false},
		// Qualified column references in full joins.
		{"qualified names one arm", "SELECT a.k, sum(b.k) FROM a FULL JOIN b USING(k) GROUP BY a.k ORDER BY a.k", false},
		// LEFT and INNER joins with USING columns.
		{"left join is not coalesced", "SELECT k, sum(k) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k", false},
		{"inner join is not coalesced", "SELECT k, sum(k) FROM a JOIN b USING(k) GROUP BY k ORDER BY k", false},
		{"natural inner join", "SELECT k, sum(k) FROM a NATURAL JOIN b GROUP BY k ORDER BY k", false},
		{"rowid through the drain", "SELECT k, sum(a.rowid) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range []string{
				"CREATE TABLE a(k,x)", "CREATE TABLE b(k,y)", "CREATE TABLE c(k,z)",
				"INSERT INTO a VALUES(1,10),(2,20),(NULL,90)",
				"INSERT INTO b VALUES(2,200),(3,300),(NULL,900)",
				"INSERT INTO c VALUES(3,3000),(4,4000)",
			} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
			}
			rvdQuery(t, db, tc.query)
			if got := aggUnstampedFor(t, db, tc.query); (len(got) != 0) != tc.wantUnlowered {
				t.Errorf("%q left %v unlowered, want unlowered=%v", tc.query, got, tc.wantUnlowered)
			}
		})
	}
}
