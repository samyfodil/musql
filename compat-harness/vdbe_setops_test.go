// Differential tests of INTERSECT and EXCEPT compound set operators,
// including page-size variations and column-count mismatch checks.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildSetOpsDB builds test tables for INTERSECT/EXCEPT with overlapping rows,
// duplicates, NULLs, column mismatches, and empty sets.
func buildSetOpsDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("setops_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	exec := func(sqlText string) {
		t.Helper()
		if err := db.Exec(sqlText); err != nil {
			t.Fatalf("Exec(%s): %v", sqlText, err)
		}
	}

	exec(`CREATE TABLE seta (a INTEGER, b TEXT)`)
	exec(`INSERT INTO seta (a,b) VALUES (1,'x')`)
	exec(`INSERT INTO seta (a,b) VALUES (1,'x')`) // exact duplicate within seta
	exec(`INSERT INTO seta (a,b) VALUES (2,NULL)`)
	exec(`INSERT INTO seta (a,b) VALUES (NULL,NULL)`)
	exec(`INSERT INTO seta (a,b) VALUES (NULL,NULL)`) // all-NULL duplicate within seta
	exec(`INSERT INTO seta (a,b) VALUES (3,'y')`)
	exec(`INSERT INTO seta (a,b) VALUES (4,'z')`) // only in seta, not setb

	exec(`CREATE TABLE setb (a INTEGER, b TEXT)`)
	exec(`INSERT INTO setb (a,b) VALUES (1,'x')`)
	exec(`INSERT INTO setb (a,b) VALUES (2,NULL)`)
	exec(`INSERT INTO setb (a,b) VALUES (5,'w')`) // only in setb, not seta
	exec(`INSERT INTO setb (a,b) VALUES (NULL,NULL)`)
	exec(`INSERT INTO setb (a,b) VALUES (3,'y')`)
	exec(`INSERT INTO setb (a,b) VALUES (3,'y')`) // exact duplicate within setb

	exec(`CREATE TABLE setc (a INTEGER, b TEXT)`)
	exec(`INSERT INTO setc (a,b) VALUES (4,'z')`)
	exec(`INSERT INTO setc (a,b) VALUES (6,'v')`)

	exec(`CREATE TABLE wide_x (a INTEGER)`)
	exec(`INSERT INTO wide_x (a) VALUES (1)`)
	exec(`CREATE TABLE wide_y (a INTEGER, b INTEGER)`)
	exec(`INSERT INTO wide_y (a,b) VALUES (1,2)`)

	exec(`CREATE TABLE empty_set (a INTEGER, b TEXT)`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// setOpsUnorderedCorpus are unordered result queries: INTERSECT/EXCEPT membership,
// dedup, chained mixed operators, and empty-arm edge cases.
var setOpsUnorderedCorpus = []string{
	"SELECT a, b FROM seta INTERSECT SELECT a, b FROM setb",
	"SELECT b FROM seta INTERSECT SELECT b FROM setb",

	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb",
	"SELECT a, b FROM setb EXCEPT SELECT a, b FROM seta",
	"SELECT b FROM seta EXCEPT SELECT b FROM setb",

	"SELECT a, b FROM seta WHERE a > 1 INTERSECT SELECT a, b FROM setb WHERE a > 1",
	"SELECT a, b FROM seta WHERE a > 1 EXCEPT SELECT a, b FROM setb WHERE a > 1",

	"SELECT a, b FROM seta UNION ALL SELECT a, b FROM setb EXCEPT SELECT a, b FROM setc",
	"SELECT a, b FROM seta UNION SELECT a, b FROM setb EXCEPT SELECT a, b FROM setc",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb UNION SELECT a, b FROM setc",
	"SELECT a, b FROM seta INTERSECT SELECT a, b FROM setb UNION ALL SELECT a, b FROM setc",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb INTERSECT SELECT a, b FROM setc",
	"SELECT a, b FROM seta INTERSECT SELECT a, b FROM setb EXCEPT SELECT a, b FROM setc",

	"SELECT a, b FROM seta INTERSECT SELECT a, b FROM empty_set",
	"SELECT a, b FROM empty_set INTERSECT SELECT a, b FROM seta",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM empty_set",
	"SELECT a, b FROM empty_set EXCEPT SELECT a, b FROM seta",

	"SELECT count(*) FROM seta INTERSECT SELECT count(*) FROM setb",
	"SELECT count(*) FROM seta EXCEPT SELECT count(*) FROM setb",
}

// setOpsOrderedCorpus are ordered result queries with ORDER BY, LIMIT, OFFSET, and chaining.
var setOpsOrderedCorpus = []string{
	"SELECT a, b FROM seta INTERSECT SELECT a, b FROM setb ORDER BY a, b",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb ORDER BY a, b",
	"SELECT a, b FROM setb EXCEPT SELECT a, b FROM seta ORDER BY a, b",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb ORDER BY a DESC, b ASC",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb ORDER BY 1, 2",
	"SELECT a AS x, b FROM seta INTERSECT SELECT a, b FROM setb ORDER BY x, b",

	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM empty_set ORDER BY a, b",

	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb ORDER BY a, b LIMIT 1",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb ORDER BY a, b LIMIT 10 OFFSET 1",

	"SELECT a, b FROM seta UNION ALL SELECT a, b FROM setb EXCEPT SELECT a, b FROM setc ORDER BY a, b",
	"SELECT a, b FROM seta UNION SELECT a, b FROM setb EXCEPT SELECT a, b FROM setc ORDER BY a, b",
	"SELECT a, b FROM seta EXCEPT SELECT a, b FROM setb UNION SELECT a, b FROM setc ORDER BY a, b",
	"SELECT a, b FROM seta INTERSECT SELECT a, b FROM setb UNION ALL SELECT a, b FROM setc ORDER BY a, b",
}

// TestSetOpsTableParity verifies corpus queries match between engine and C SQLite at both page sizes.
func TestSetOpsTableParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildSetOpsDB(t, pageSize)

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

			wrong := 0
			total := 0
			runCase := func(sqlText string, orderSensitive bool) {
				total++
				eCols, eVals, eErr := p.Query(sqlText)
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

				if eErr != nil || cErr != nil {
					wrong++
					t.Errorf("[%s] unexpected error\n  engine=%v\n  cgo=%v", sqlText, eErr, cErr)
					return
				}

				eRows := engineRowsToStrings(eVals)

				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, orderSensitive); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}

				if vCols, vVals, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
					vRows := engineRowsToStrings(vVals)
					if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, orderSensitive); !ok {
						wrong++
						t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
							sqlText, reason, vCols, vRows, cCols, cRows)
					}
				}
			}

			for _, sqlText := range setOpsUnorderedCorpus {
				runCase(sqlText, false)
			}
			for _, sqlText := range setOpsOrderedCorpus {
				runCase(sqlText, true)
			}

			t.Logf("INTERSECT/EXCEPT table parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("INTERSECT/EXCEPT parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}

// TestSetOpsColumnCountMismatch verifies column-count mismatches are rejected at both page sizes.
func TestSetOpsColumnCountMismatch(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildSetOpsDB(t, pageSize)
			p, err := engine.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			cases := []string{
				`SELECT a FROM wide_x INTERSECT SELECT a, b FROM wide_y`,
				`SELECT a, b FROM wide_y INTERSECT SELECT a FROM wide_x`,
				`SELECT a FROM wide_x EXCEPT SELECT a, b FROM wide_y`,
				`SELECT a, b FROM wide_y EXCEPT SELECT a FROM wide_x`,
			}
			for _, sqlText := range cases {
				if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
					t.Errorf("[%s] QueryVDBE: expected column-count-mismatch error, got nil", sqlText)
				}
				if _, _, err := p.Query(sqlText); err == nil {
					t.Errorf("[%s] Query: expected column-count-mismatch error, got nil", sqlText)
				}
			}
		})
	}
}
