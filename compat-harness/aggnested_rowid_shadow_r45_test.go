// Regression pin for unqualified "rowid" in aggregate arguments in subquery WHERE clauses.
// Tests that rowid binds to the subquery's implicit rowid, not an outer query's.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

func buildAggNestedRowidShadowDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aggnested_rowid_shadow_r45.sqlite")
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
	exec(`CREATE TABLE t1(id1 INTEGER, value1 INTEGER)`)
	exec(`INSERT INTO t1 VALUES(1,10),(1,20),(2,30)`)
	exec(`CREATE TABLE t2(x INTEGER)`)
	exec(`INSERT INTO t2 VALUES(2),(2),(3)`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// TestAggNestedRowidShadowStillDeclines pins the exact counter-example (and
// its "_rowid_"/sum() siblings) that regressed a silent WRONG ANSWER: the
// real oracle REFUSES each one ("misuse of aggregate", a self-reference, not
// the outward-escape this file's sibling gap-2 fix targets), so this engine
// must decline cleanly (never compute a shaped-but-wrong answer) on both the
// VDBE path and the integrated p.Query path.
func TestAggNestedRowidShadowStillDeclines(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{
			// The exact mined counter-example: aggnested-3.11's own shape
			// with "value1" replaced by the implicit rowid alias "rowid".
			// C SQLite: "misuse of aggregate: max()".
			"unqualified-rowid-max",
			`SELECT max(rowid), (SELECT count(*) FROM t2 WHERE x=max(rowid)) FROM t1 GROUP BY id1`,
		},
		{
			// "_rowid_" is the identical pseudo-column under a different
			// spelling (sqlite3IsRowid, expr.c:3042, treats all three names
			// alike). C SQLite: "misuse of aggregate: max()".
			"unqualified-rowid-alias-max",
			`SELECT max(_rowid_), (SELECT count(*) FROM t2 WHERE x=max(_rowid_)) FROM t1 GROUP BY id1`,
		},
		{
			// "oid" is the third reserved spelling.
			"unqualified-oid-max",
			`SELECT max(oid), (SELECT count(*) FROM t2 WHERE x=max(oid)) FROM t1 GROUP BY id1`,
		},
		{
			// sum() rather than max()/min(): a different aggregate, same
			// self-reference misuse ("misuse of aggregate: sum()"), proving
			// this is not specific to the min/max census-exception path
			// (aggnested_where_agg_ref_r40_test.go's gap 3).
			"unqualified-rowid-sum",
			`SELECT sum(rowid), (SELECT count(*) FROM t2 WHERE x=sum(rowid)) FROM t1 GROUP BY id1`,
		},
	}
	for pageSize := range map[int]struct{}{512: {}, 4096: {}} {
		path := buildAggNestedRowidShadowDB(t, pageSize)
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
				_, _, cErr := cgoSelect(t, cdb, c.sql, nil)
				if cErr == nil {
					t.Fatalf("expected the real oracle to REFUSE this self-referencing WHERE-clause aggregate; it answered instead -- fixture assumption broken")
				}
				if cols, rows, err := p.QueryArgs(c.sql, nil); err == nil {
					t.Fatalf("QueryVDBE ANSWERED %q where the oracle errors (%v) -- got cols=%v rows=%v; this is the exact regression class this file pins (a shaped-but-wrong answer, not a decline)", c.sql, cErr, cols, rows)
				}
				if cols, rows, err := p.Query(c.sql); err == nil {
					t.Fatalf("p.Query ANSWERED %q where the oracle errors (%v) -- got cols=%v rows=%v", c.sql, cErr, cols, rows)
				}
			})
		}
		cdb.Close()
		p.Close()
	}
}

// TestAggNestedRowidShadowQualifiedStillWorks is the baseline regression
// guard: a QUALIFIED rowid reference ("t1.rowid") was never part of this
// gap -- aggCallHasLocalColumnArg's own qualifier-matching path (inner map,
// keyed by FROM-item NAME) already handled it correctly before and after
// this fix -- so it must keep escalating to t1's own GROUP BY aggregate
// exactly as aggnested-3.11's own qualified sibling does.
func TestAggNestedRowidShadowQualifiedStillWorks(t *testing.T) {
	path := buildAggNestedRowidShadowDB(t, 4096)
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

	sqlText := `SELECT max(t1.rowid), (SELECT count(*) FROM t2 WHERE x=max(t1.rowid)) FROM t1 GROUP BY id1`
	cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
	if cErr != nil {
		t.Fatalf("oracle: %v", cErr)
	}
	vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
	if vErr != nil {
		t.Fatalf("QueryVDBE regressed to a decline: %v", vErr)
	}
	if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
		t.Fatalf("QueryVDBE DIVERGES from C SQLite: %s", reason)
	}
	eCols, eVals, eErr := p.Query(sqlText)
	if eErr != nil {
		t.Fatalf("p.Query regressed to a decline: %v", eErr)
	}
	if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
		t.Fatalf("p.Query DIVERGES from C SQLite: %s", reason)
	}
}
