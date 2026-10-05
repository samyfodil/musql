// Aggregate function calls in correlated subquery WHERE clauses that reference
// an enclosing GROUP BY query's aggregates. Tests check which shapes now work.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

func buildAggNestedWhereRefDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aggnested_where_r40.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	exec := func(q string) {
		t.Helper()
		if err := db.Exec(q); err != nil {
			t.Fatalf("Exec(%s): %v", q, err)
		}
	}
	exec(`CREATE TABLE t1(id1, value1)`)
	exec(`INSERT INTO t1 VALUES(4469,12),(4469,11),(4470,34)`)
	exec(`CREATE INDEX t1id1 ON t1(id1)`)
	exec(`CREATE TABLE t2 (value2)`)
	exec(`INSERT INTO t2 VALUES(12),(34),(34)`)
	exec(`INSERT INTO t2 SELECT value2 FROM t2`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// TestAggNestedWhereAggRefStillDeclines checks shapes that still decline even
// after gap closure.
func TestAggNestedWhereAggRefStillDeclines(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{
			"no-group-by",
			`SELECT max(value1), (SELECT count(*) FROM t2 WHERE value2=max(value1)) FROM t1`,
		},
		{
			"qualified-outer-column",
			`SELECT max(value1), (SELECT count(*) FROM t2 WHERE value2=max(t1.value1)) FROM t1 GROUP BY id1`,
		},
	}
	for pageSize := range map[int]struct{}{512: {}, 4096: {}} {
		path := buildAggNestedWhereRefDB(t, pageSize)
		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatal(err)
		}
		p, err := engine.Open(path)
		if err != nil {
			cdb.Close()
			t.Fatal(err)
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				cCols, cRows, cErr := cgoSelect(t, cdb, c.sql, nil)
				if cErr != nil {
					t.Fatalf("expected the real oracle to ANSWER this (the point of the decline), got %v", cErr)
				}
				if len(cCols) == 0 || len(cRows) == 0 {
					t.Fatalf("real oracle returned no rows -- fixture assumption broken")
				}
				if _, _, err := p.QueryArgs(c.sql, nil); err == nil {
					t.Fatalf("QueryVDBE now ANSWERS this WHERE-clause outward-aggregate-reference shape -- "+
						"either the three-part gap this file documents was closed (move this case into a real "+
						"result-parity gate, verifying against the oracle's rows %v) or it is answering WRONG", cRows)
				}
				if _, _, err := p.Query(c.sql); err == nil {
					t.Fatalf("p.Query now ANSWERS this WHERE-clause outward-aggregate-reference shape -- "+
						"either the gap was closed (move this case into a real result-parity gate) or it is answering WRONG")
				}
			})
		}
		cdb.Close()
		p.Close()
	}
}

// TestAggNestedWhereBareColumnStillWorks checks that bare column references
// (not aggregates) in WHERE clauses still work.
func TestAggNestedWhereBareColumnStillWorks(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		path := buildAggNestedWhereRefDB(t, pageSize)
		sqlText := `SELECT max(value1), (SELECT count(*) FROM t2 WHERE value2=value1) FROM t1 GROUP BY id1`

		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatal(err)
		}
		defer cdb.Close()
		p, err := engine.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()

		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Fatalf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Fatalf("[%s] QueryVDBE regressed to a decline: %v", sqlText, vErr)
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Fatalf("[%s] QueryVDBE DIVERGES from C SQLite: %s", sqlText, reason)
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Fatalf("[%s] p.Query regressed to a decline: %v", sqlText, eErr)
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Fatalf("[%s] p.Query DIVERGES from C SQLite: %s", sqlText, reason)
		}
	}
}

// TestAggNestedWhereAggRefUnqualifiedNowWorks checks unqualified aggregate
// references in correlated subquery WHERE clauses.
func TestAggNestedWhereAggRefUnqualifiedNowWorks(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{
			"mined-verbatim-groupby-max",
			`SELECT max(value1), (SELECT count(*) FROM t2 WHERE value2=max(value1)) FROM t1 GROUP BY id1`,
		},
		{
			"different-aggregate-sum",
			`SELECT sum(value1), (SELECT count(*) FROM t2 WHERE value2=sum(value1)) FROM t1 GROUP BY id1`,
		},
	}
	for _, pageSize := range []int{512, 4096} {
		path := buildAggNestedWhereRefDB(t, pageSize)

		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatal(err)
		}
		defer cdb.Close()
		p, err := engine.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				cCols, cRows, cErr := cgoSelect(t, cdb, c.sql, nil)
				if cErr != nil {
					t.Fatalf("[%s] C SQLite unexpectedly errored: %v", c.sql, cErr)
				}
				vCols, vVals, vErr := p.QueryArgs(c.sql, nil)
				if vErr != nil {
					t.Fatalf("[%s] QueryVDBE regressed to a decline: %v", c.sql, vErr)
				}
				if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
					t.Fatalf("[%s] QueryVDBE DIVERGES from C SQLite: %s", c.sql, reason)
				}
				eCols, eVals, eErr := p.Query(c.sql)
				if eErr != nil {
					t.Fatalf("[%s] p.Query regressed to a decline: %v", c.sql, eErr)
				}
				if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
					t.Fatalf("[%s] p.Query DIVERGES from C SQLite: %s", c.sql, reason)
				}
			})
		}
	}
}

// TestAggNestedWhereAggRefQualifiedDuplicateNowWorks checks qualified
// duplicate aggregate references.
func TestAggNestedWhereAggRefQualifiedDuplicateNowWorks(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{
			"group-by",
			`SELECT max(t1.value1), (SELECT count(*) FROM t2 WHERE value2=max(t1.value1)) FROM t1 GROUP BY id1`,
		},
	}
	for _, pageSize := range []int{512, 4096} {
		path := buildAggNestedWhereRefDB(t, pageSize)

		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatal(err)
		}
		defer cdb.Close()
		p, err := engine.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				cCols, cRows, cErr := cgoSelect(t, cdb, c.sql, nil)
				if cErr != nil {
					t.Fatalf("[%s] C SQLite unexpectedly errored: %v", c.sql, cErr)
				}
				vCols, vVals, vErr := p.QueryArgs(c.sql, nil)
				if vErr != nil {
					t.Fatalf("[%s] QueryVDBE regressed to a decline: %v", c.sql, vErr)
				}
				if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
					t.Fatalf("[%s] QueryVDBE DIVERGES from C SQLite: %s", c.sql, reason)
				}
				eCols, eVals, eErr := p.Query(c.sql)
				if eErr != nil {
					t.Fatalf("[%s] p.Query regressed to a decline: %v", c.sql, eErr)
				}
				if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
					t.Fatalf("[%s] p.Query DIVERGES from C SQLite: %s", c.sql, reason)
				}
			})
		}
	}
}

// TestAggNestedWhereAggRefDuplicateHoistDoesNotDisturbAnchorRow checks that
// hoisting duplicates does not affect min/max anchor row resolution.
func TestAggNestedWhereAggRefDuplicateHoistDoesNotDisturbAnchorRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggnested_where_r40_anchor.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE g1(k, v)`,
		`INSERT INTO g1 VALUES(1,10),(1,20),(2,30),(2,NULL),(3,40)`,
		`CREATE TABLE g2(w)`,
		`INSERT INTO g2 VALUES(1),(2)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatalf("Exec(%s): %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sqlText := `SELECT k, max(g1.v), v, (SELECT count(*) FROM g2 WHERE w=max(g1.v)) FROM g1 GROUP BY k`

	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
	if cErr != nil {
		t.Fatalf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
	}
	vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
	if vErr != nil {
		t.Fatalf("[%s] QueryVDBE regressed to a decline: %v", sqlText, vErr)
	}
	if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
		t.Fatalf("[%s] QueryVDBE DIVERGES from C SQLite: %s", sqlText, reason)
	}
}
