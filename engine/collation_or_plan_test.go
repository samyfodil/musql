package engine

import (
	"strings"
	"testing"
)

// TestCollationOrPlanBoundary pins applyRiskyCollationOrPlan's DECLINE
// boundary (collation_or_plan.go): each shape here is one the oracle runs
// but whose answer depends on a planner/analysis this engine does not
// reproduce, so it must keep the historical decline -- never be served with
// a guess. The SERVED side of the boundary is pinned against the live oracle
// in compat-harness/collation_or_multiindex_test.go, which a decline cannot
// be (its oracle runs these statements fine).
func TestCollationOrPlanBoundary(t *testing.T) {
	mustDecline := func(t *testing.T, setup []string, query string) {
		t.Helper()
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		pg, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		defer pg.Close()
		_, _, qerr := pg.QueryArgs(query, nil)
		if qerr == nil || !strings.Contains(qerr.Error(), "cross-table declared-collation equality combined with OR") {
			t.Errorf("%q: want the declared-collation OR decline, got %v", query, qerr)
		}
	}

	base := []string{
		"CREATE TABLE ta(a TEXT COLLATE NOCASE, b TEXT COLLATE NOCASE)",
		"INSERT INTO ta VALUES('AAA','BBB')",
	}
	tb := []string{
		"CREATE TABLE tb(x,y,c TEXT)",
		"INSERT INTO tb(c) VALUES('aaa'),('bbb')",
		"CREATE INDEX tb_c ON tb(c)",
	}

	t.Run("partial-index", func(t *testing.T) {
		// A partial index CAN be chosen for the OR-to-IN probe when the
		// disjunct implies its predicate (measured: "WHERE c IS NOT NULL" is
		// probed, flipping the answer to c's collation); reproducing that
		// implication analysis is out of scope.
		mustDecline(t, append(base,
			"CREATE TABLE te(x,y,c TEXT)",
			"INSERT INTO te(c) VALUES('aaa'),('bbb')",
			"CREATE INDEX te_c ON te(c) WHERE c IS NOT NULL"),
			"SELECT c FROM ta, te WHERE a=c OR b=c")
	})
	t.Run("stat1-present", func(t *testing.T) {
		// Statistics put the transform under C SQLite's cost model.
		mustDecline(t, append(append(base, tb...), "ANALYZE"),
			"SELECT c FROM ta, tb WHERE a=c OR b=c")
	})
	t.Run("and-context", func(t *testing.T) {
		// Another conjunct referencing the probed table could hand the
		// planner a competing index driver.
		mustDecline(t, append(base, tb...),
			"SELECT c FROM ta, tb WHERE (a=c OR b=c) AND c<>'zzz'")
	})
	t.Run("both-sides-shared", func(t *testing.T) {
		// Every disjunct the same column pair: either table could host the
		// transform.
		mustDecline(t, append(base, tb...),
			"SELECT c FROM ta, tb WHERE a=c OR a=c")
	})
	t.Run("three-tables", func(t *testing.T) {
		mustDecline(t, append(append(base, tb...),
			"CREATE TABLE tz(z)"),
			"SELECT c FROM ta, tb, tz WHERE a=c OR b=c")
	})
	// "view-source" used to be here: a view over tb declined because it might
	// flatten into a shape this analysis could not see. The flattening is
	// ported now (flatten_projection.go), the view becomes tb before planning,
	// and the answer is compared against C in
	// compat-harness/collation_or_view_test.go.
	t.Run("automatic-index", func(t *testing.T) {
		// A UNIQUE constraint's automatic index: its column list is not
		// recoverable from the sqlite_schema index row.
		mustDecline(t, append(base,
			"CREATE TABLE tu(x,y,c TEXT UNIQUE)",
			"INSERT INTO tu(c) VALUES('aaa'),('bbb')"),
			"SELECT c FROM ta, tu WHERE a=c OR b=c")
	})
	t.Run("without-rowid-pk-leading", func(t *testing.T) {
		// The table's own b-tree is an index on the shared column -- a probe
		// target with no recorded evidence.
		mustDecline(t, append(base,
			"CREATE TABLE tw(c TEXT PRIMARY KEY, z TEXT) WITHOUT ROWID",
			"INSERT INTO tw VALUES('aaa','1'),('bbb','2')"),
			"SELECT c FROM ta, tw WHERE a=c OR b=c")
	})
	t.Run("risky-where-plus-on", func(t *testing.T) {
		// The risky OR must be the statement's sole constraint.
		mustDecline(t, append(base, tb...),
			"SELECT c FROM ta JOIN tb ON (ta.a=tb.x) WHERE a=c OR b=c")
	})
	t.Run("risky-where-over-left-join", func(t *testing.T) {
		// The transform mixed with NULL-completed rows was not probed.
		mustDecline(t, append(base, tb...),
			"SELECT c FROM ta LEFT JOIN tb WHERE a=c OR b=c")
	})
	t.Run("non-text-affinity", func(t *testing.T) {
		// The rule was pinned over TEXT-affinity columns only; an affinity
		// conflict is its own transform-disabling condition in SQLite.
		mustDecline(t, append(base,
			"CREATE TABLE ti(x,y,c INTEGER)",
			"INSERT INTO ti(c) VALUES(1),(2)",
			"CREATE INDEX ti_c ON ti(c)"),
			"SELECT c FROM ta, ti WHERE a=c OR b=c")
	})
}
