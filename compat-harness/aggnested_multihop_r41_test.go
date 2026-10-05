// Multi-hop aggregate hoisting tests verify that aggregate calls buried
// multiple subquery levels deep are correctly escalated to their actual owners.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildAggNestedMultihopDB creates the test fixture.
func buildAggNestedMultihopDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aggnested_multihop_r41.sqlite")
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
	exec(`CREATE TABLE t1(a INTEGER)`)
	exec(`INSERT INTO t1 VALUES(1),(2),(3),(4)`)
	exec(`CREATE TABLE t2(b INTEGER)`)
	exec(`INSERT INTO t2 VALUES(10),(20),(30)`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// checkAggNestedMultihop runs the query through both oracle and engine and asserts a byte-exact match.
func checkAggNestedMultihop(t *testing.T, path, sqlText string) {
	t.Helper()
	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
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
	eCols, eVals, eErr := p.Query(sqlText)
	if eErr != nil {
		t.Fatalf("[%s] p.Query regressed to a decline: %v", sqlText, eErr)
	}
	if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
		t.Fatalf("[%s] p.Query DIVERGES from C SQLite: %s", sqlText, reason)
	}
}

// TestAggNestedMultihopMinedStatement checks deeply nested aggregate escapes.
func TestAggNestedMultihopMinedStatement(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	checkAggNestedMultihop(t, path,
		`WITH out(i,j,k) AS (VALUES(1234,5678,9012))
		 SELECT (SELECT (SELECT min(abc)=(SELECT (SELECT 1234 FROM (SELECT abc)))
		                  FROM (SELECT sum(out.i)+(SELECT sum(out.i)) AS abc
		                          FROM (SELECT out.j))))
		   FROM out`)
}

// TestAggNestedMultihopOneHopAlreadyWorked checks the 1-hop case that already worked.
func TestAggNestedMultihopOneHopAlreadyWorked(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	checkAggNestedMultihop(t, path,
		`WITH out(i) AS (VALUES(1),(2)) SELECT (SELECT sum(out.i) FROM (SELECT 1)) FROM out`)
}

// TestAggNestedMultihopTwoHop checks the 2-hop escape case.
func TestAggNestedMultihopTwoHop(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	checkAggNestedMultihop(t, path,
		`WITH out(i) AS (VALUES(1),(2)) SELECT (SELECT sum(out.i) + (SELECT sum(out.i)) FROM (SELECT 1)) FROM out`)
}

// TestAggNestedMultihopThreeHop checks the 3-hop escape case.
func TestAggNestedMultihopThreeHop(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	checkAggNestedMultihop(t, path,
		`WITH out(i) AS (VALUES(1),(2),(3))
		 SELECT (SELECT (SELECT (SELECT sum(out.i)) FROM (SELECT 1)) FROM (SELECT 1))
		   FROM out`)
}

// TestAggNestedMultihopOverRealTable checks 2-hop escapes over real tables.
func TestAggNestedMultihopOverRealTable(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	checkAggNestedMultihop(t, path,
		`SELECT (SELECT sum(t1.a) + (SELECT sum(t1.a)) FROM (SELECT 1)) FROM t1`)
}

// TestAggNestedMultihopMinMaxStillDeclines checks that min/max two-hop
// escapes remain declined (known limitation).
func TestAggNestedMultihopMinMaxStillDeclines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggnested_multihop_r41_minmax.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE t1(a INTEGER)`,
		`INSERT INTO t1 VALUES(5),(3),(9)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatalf("Exec(%s): %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sqlText := `SELECT (SELECT max(t1.a) + (SELECT max(t1.a)) FROM (SELECT 1)) FROM t1`

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
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
		t.Fatalf("expected the real oracle to ANSWER this, got %v", cErr)
	}
	if len(cCols) == 0 || len(cRows) == 0 {
		t.Fatalf("real oracle returned no rows -- fixture assumption broken")
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Fatalf("QueryVDBE now ANSWERS this min/max two-hop shape -- either it was closed " +
			"(move this case into a real result-parity gate, verifying against the oracle) " +
			"or it is answering WRONG")
	}
	if _, eVals, err := p.Query(sqlText); err == nil {
		t.Fatalf("p.Query now ANSWERS this min/max two-hop shape (%v) -- either it was closed "+
			"(move this case into a real result-parity gate) or it is answering WRONG", eVals)
	}
}

// TestAggNestedMultihopGroupBy checks escapes in GROUP BY queries.
func TestAggNestedMultihopGroupBy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggnested_multihop_r41_groupby.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE t1(g INTEGER, a INTEGER)`,
		`INSERT INTO t1 VALUES(1,10),(1,20),(2,30),(2,40)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatalf("Exec(%s): %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkAggNestedMultihop(t, path,
		`SELECT g, (SELECT sum(t1.a) + (SELECT sum(t1.a)) FROM (SELECT 1)) FROM t1 GROUP BY g`)
}

// TestAggNestedMultihopFilterClause checks escapes with FILTER clauses.
func TestAggNestedMultihopFilterClause(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	checkAggNestedMultihop(t, path,
		`SELECT (SELECT sum(1) FILTER (WHERE t1.a>1) + (SELECT sum(1) FILTER (WHERE t1.a>1)) FROM (SELECT 1)) FROM t1`)
}

// TestAggNestedMultihopCombinedWithWhereGap checks escapes in WHERE clauses
// combined with GROUP BY.
func TestAggNestedMultihopCombinedWithWhereGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggnested_multihop_r41_combined.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE t1(id1 INTEGER, value1 INTEGER)`,
		`INSERT INTO t1 VALUES(1,10),(1,20),(2,30)`,
		`CREATE TABLE t2(value2 INTEGER)`,
		`INSERT INTO t2 VALUES(30)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatalf("Exec(%s): %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkAggNestedMultihop(t, path,
		`SELECT max(value1), (SELECT (SELECT count(*) FROM t2 WHERE value2=max(value1)) FROM (SELECT 1)) FROM t1 GROUP BY id1`)
}

// TestAggNestedMultihopCompoundMember checks escapes in compound query members.
func TestAggNestedMultihopCompoundMember(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	checkAggNestedMultihop(t, path,
		`SELECT (SELECT 1 WHERE 0 UNION SELECT (SELECT sum(t1.a)) FROM (SELECT 1)) FROM t1 LIMIT 1`)
}

// TestAggNestedMultihopTwoMoversDifferentOwners checks escapes to different owners.
func TestAggNestedMultihopTwoMoversDifferentOwners(t *testing.T) {
	path := buildAggNestedMultihopDB(t, 4096)
	sqlText := `SELECT (SELECT sum(t2.b) + (SELECT sum(t1.a)) FROM t2) FROM t1`

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
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
		t.Fatalf("expected the real oracle to ANSWER this, got %v", cErr)
	}
	if len(cCols) == 0 || len(cRows) == 0 {
		t.Fatalf("real oracle returned no rows -- fixture assumption broken")
	}
	vCols, vRows, vErr := p.QueryArgs(sqlText, nil)
	if vErr != nil {
		t.Fatalf("QueryVDBE declined a shape C SQLite answers: %v -- this shape is expected to "+
			"compile, so a decline here is a regression, not the status quo", vErr)
	}
	if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vRows), cCols, cRows, true); !ok {
		t.Fatalf("[%s] QueryVDBE DIVERGES from C SQLite: %s", sqlText, reason)
	}
}
