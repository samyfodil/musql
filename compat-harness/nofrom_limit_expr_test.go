// This file gates LIMIT/OFFSET expression compilation and resolution. Parameters
// and subqueries in LIMIT/OFFSET must be handled correctly.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// TestNoFromLimitExprParamParity gates the pager-free half (RunNoFrom): a bound
// parameter as the whole LIMIT, as the whole OFFSET, and inside a function call
// -- against C SQLite. The LIMIT-0 case is deliberate: an off-by-one in
// the fold turns a zero-row answer into a one-row one.
func TestNoFromLimitExprParamParity(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := []struct {
		sql        string
		engineArgs []engine.Value
		cgoArgs    []any
	}{
		{"SELECT 42 LIMIT ?1", []engine.Value{evInt(1)}, []any{int64(1)}},
		{"SELECT 42 LIMIT ?1", []engine.Value{evInt(0)}, []any{int64(0)}},
		{"SELECT 42 LIMIT ?1 OFFSET ?2", []engine.Value{evInt(1), evInt(0)}, []any{int64(1), int64(0)}},
		{"SELECT 42 LIMIT ?1 OFFSET ?2", []engine.Value{evInt(1), evInt(1)}, []any{int64(1), int64(1)}},
		{"SELECT 42 LIMIT abs(?1)", []engine.Value{evInt(-1)}, []any{int64(-1)}},
	}
	for _, c := range cases {
		vCols, vVals, vErr := engine.RunNoFrom(c.sql, c.engineArgs)
		cCols, cRows, cErr := cgoSelect(t, db, c.sql, c.cgoArgs)
		if vErr != nil || cErr != nil {
			t.Errorf("[%s %v] unexpected error vdbe=%v cgo=%v", c.sql, c.cgoArgs, vErr, cErr)
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s %v] VDBE DIVERGES from C SQLite: %s\n  vdbe=%v cgo=%v",
				c.sql, c.cgoArgs, reason, engineRowsToStrings(vVals), cRows)
		}
	}
}

// TestQueryVDBELimitExprParity gates the half that has a pager (QueryVDBE),
// where the LIMIT/OFFSET expression may read the database. A fold that lost the
// pager still ANSWERS every literal-arithmetic case here and declines only the
// subquery ones, so the subquery cases are the load-bearing half.
func TestQueryVDBELimitExprParity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nofrom_limit_expr.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE t(a INTEGER)`,
		`INSERT INTO t VALUES(1),(2),(3),(4)`,
		`CREATE TABLE t5(k INTEGER)`,
		`INSERT INTO t5 VALUES(2)`,
	} {
		if err := edb.Exec(q); err != nil {
			t.Fatalf("Exec(%s): %v", q, err)
		}
	}
	if err := edb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

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

	for _, sqlText := range []string{
		`SELECT a FROM t LIMIT (SELECT k FROM t5)`,
		`SELECT a FROM t LIMIT (SELECT max(k) FROM t5)`,
		`SELECT a FROM t LIMIT 2 OFFSET (SELECT k FROM t5)`,
		`SELECT a FROM t LIMIT (SELECT k FROM t5) OFFSET (SELECT k-1 FROM t5)`,
		`SELECT a FROM t LIMIT 1+1`,
		`SELECT a FROM t LIMIT abs(-2)`,
	} {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Fatalf("oracle %q: %v", sqlText, cErr)
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("QueryVDBE %q declined: %v", sqlText, vErr)
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("QueryVDBE %q DIVERGES from C SQLite: %s\n  vdbe=%v cgo=%v",
				sqlText, reason, engineRowsToStrings(vVals), cRows)
		}
	}
}
