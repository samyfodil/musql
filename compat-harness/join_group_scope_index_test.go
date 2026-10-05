package compat

// This file tests join group scope naming with parenthesized groups.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

func buildJoinGroupScopeIndexDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/join_group_scope_index.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE gg1(a INTEGER, b TEXT)`,
		`INSERT INTO gg1 VALUES(1,'one')`,
		`INSERT INTO gg1 VALUES(2,'two')`,
		`CREATE TABLE gg2(a2 INTEGER, c TEXT)`,
		`INSERT INTO gg2 VALUES(1,'C1')`,
		`CREATE TABLE gg2b(w TEXT)`,
		`INSERT INTO gg2b VALUES('W')`,
		`CREATE TABLE gg3(x INTEGER, y TEXT)`,
		`INSERT INTO gg3 VALUES(7,'Y7')`,
		`INSERT INTO gg3 VALUES(8,'Y8')`,
		`CREATE TABLE gg4(x INTEGER, z TEXT)`,
		`INSERT INTO gg4 VALUES(7,'Z7')`,
		`INSERT INTO gg4 VALUES(9,'Z9')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// TestJoinGroupScopeIndexParity is the gate. Row COUNT is what it turns on: a
// representative resolved to the wrong item makes the last join's condition
// compare a column with itself, so the correct 2 rows become the unfiltered
// 4 (gg3 x gg4) -- the columns and their names look right either way.
func TestJoinGroupScopeIndexParity(t *testing.T) {
	path := buildJoinGroupScopeIndexDB(t)

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

	corpus := []joinCase{
		// A TWO-member group: one skipped position.
		{"SELECT * FROM (gg1 LEFT JOIN gg2 ON gg1.a=gg2.a2) JOIN gg3 ON 1 JOIN gg4 USING(x) ORDER BY 1,3,5", true},
		{"SELECT * FROM (gg1 LEFT JOIN gg2 ON gg1.a=gg2.a2) JOIN gg3 ON 1 NATURAL JOIN gg4 ORDER BY 1,3,5", true},
		{"SELECT a, y, z FROM (gg1 LEFT JOIN gg2 ON gg1.a=gg2.a2) JOIN gg3 ON 1 JOIN gg4 USING(x) ORDER BY 1,2,3", true},

		// A THREE-member group: two skipped positions -- the spelling that
		// used to panic.
		{"SELECT * FROM (gg1 LEFT JOIN gg2 ON gg1.a=gg2.a2 JOIN gg2b ON 1) JOIN gg3 ON 1 JOIN gg4 USING(x) ORDER BY 1,3,6", true},
		{"SELECT * FROM (gg1 LEFT JOIN gg2 ON gg1.a=gg2.a2 JOIN gg2b ON 1) JOIN gg3 ON 1 NATURAL JOIN gg4 ORDER BY 1,3,6", true},
		{"SELECT a, y, z FROM (gg1 LEFT JOIN gg2 ON gg1.a=gg2.a2 JOIN gg2b ON 1) JOIN gg3 ON 1 JOIN gg4 USING(x) ORDER BY 1,2,3", true},

		// The same chains with no group at all: unaffected, and here to show
		// the expected 2-row answer is not an artifact of the group.
		{"SELECT a, y, z FROM gg1 JOIN gg3 ON 1 JOIN gg4 USING(x) ORDER BY 1,2,3", true},
		{"SELECT a, y, z FROM gg1 LEFT JOIN gg2 ON gg1.a=gg2.a2 JOIN gg3 ON 1 JOIN gg4 USING(x) ORDER BY 1,2,3", true},
	}

	wrong := 0
	for _, tc := range corpus {
		vCols, vVals, vErr := p.QueryArgs(tc.sql, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error\n  musql=%v\n  cgo=%v", tc.sql, vErr, cErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] DIVERGES from C SQLite: %s\n  musql: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
				tc.sql, reason, vCols, vRows, cCols, cRows)
		}
		if len(vRows) != 2 {
			wrong++
			t.Errorf("[%s] expected exactly 2 rows (a wrong representative makes it 4, the unfiltered gg3 x gg4), got %d", tc.sql, len(vRows))
		}
	}
	t.Logf("join-group scope-index gate: %d statements, wrong=%d", len(corpus), wrong)
	if wrong != 0 {
		t.Fatalf("join-group scope-index gate FAILED: wrong=%d (must be 0)", wrong)
	}
}
