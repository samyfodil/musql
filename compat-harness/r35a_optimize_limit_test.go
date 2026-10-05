// This file is the differential gate for "PRAGMA optimize"'s own analysis
// limit. engine/pragma.go used to decline the pragma for ANY table above 100
// rows, reading pragma.c's "nLimit = 100*nLimit/nBtree" scaling as
// unconditional. It is not: the scaling only runs when nBtree>100, so the limit
// on an ordinary schema is SQLITE_DEFAULT_OPTIMIZE_LIMIT itself (2000) -- which
// is exactly where the round-24 measurement recorded in that file's own comment
// put the boundary (1999 rows agrees, 2500 does not).
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// r35aOptimizeScript builds a schema with an index, ANALYZEs it, and then runs
// PRAGMA optimize -- the shape busy.test 3.x uses. rows is the table's size.
func r35aOptimizeScript(rows int) []string {
	return []string{
		`CREATE TABLE t1(x)`,
		`CREATE TABLE t2(y)`,
		`CREATE TABLE t3(z)`,
		`CREATE INDEX i1 ON t1(x)`,
		`CREATE INDEX i2 ON t2(y)`,
		fmt.Sprintf(`WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<%d) INSERT INTO t1 SELECT i FROM s`, rows),
		`INSERT INTO t2 VALUES(1)`,
		`ANALYZE`,
		`PRAGMA optimize`,
	}
}

// TestR35AOptimizeWithinAnalysisLimit is the window the fix opens: a table
// between the old floor of 100 and the real limit of 2000, whose sqlite_stat1
// is already current, so optimize's ANALYZE -- whichever tables it picks --
// rewrites exactly what is already stored.
func TestR35AOptimizeWithinAnalysisLimit(t *testing.T) {
	for _, n := range []int{150, 500, 1999} {
		n := n
		t.Run(fmt.Sprintf("%d rows", n), func(t *testing.T) {
			flLockstep(t, fmt.Sprintf("optimize/%d", n), r35aOptimizeScript(n),
				`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
				`PRAGMA schema_version`,
				`SELECT count(*) FROM t1`)
		})
	}
}

// TestR35AOptimizeAboveAnalysisLimit is the other side of the boundary. Above
// the limit optimize's ANALYZE SAMPLES, and this engine has no sampling
// ANALYZE, so the pragma is declined -- but a decline is not what this asserts.
// It asserts that if the engine ever ACCEPTS it, sqlite_stat1 must then agree
// with the oracle's, which is the instruction for whoever implements sampling.
func TestR35AOptimizeAboveAnalysisLimit(t *testing.T) {
	const n = 2500
	script := r35aOptimizeScript(n)

	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)

	edb, eerr := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if eerr != nil {
		t.Fatalf("engine.Create: %v", eerr)
	}
	defer edb.Close()

	for i, s := range script {
		_, cerr := cdb.Exec(s)
		if cerr != nil {
			t.Fatalf("cgo stmt #%d %s: %v", i, s, cerr)
		}
		if err := edb.Exec(s); err != nil {
			if i == len(script)-1 {
				return // the honest decline of PRAGMA optimize itself
			}
			t.Fatalf("engine stmt #%d %s: %v", i, s, err)
		}
	}
	flCompareQuery(t, "optimize/2500", edb, cdb, `SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`)
}
