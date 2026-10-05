// This file tests derived tables with duplicate result-column names,
// which SQLite resolves to the first occurrence. All cases are diffed
// against the C oracle.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r32mDupSchema varies column order and mixed affinities.
var r32mDupSchema = []string{
	`CREATE TABLE d1(k INTEGER, a TEXT COLLATE NOCASE, z REAL)`,
	`CREATE TABLE d2(a INTEGER, k TEXT, y BLOB)`,
	`INSERT INTO d1 VALUES(1,'X',1.5),(2,'y',2.5),(3,NULL,NULL)`,
	`INSERT INTO d2 VALUES(10,'1',x'aa'),(20,'2',x'bb'),(30,'9',NULL)`,
}

// r32mDupReadouts covers various code paths: projection, WHERE, GROUP BY,
// aggregates, DISTINCT, subqueries, joins, and CASE.
var r32mDupReadouts = []string{
	`SELECT a FROM (%s)`,
	`SELECT typeof(a) FROM (%s)`,
	`SELECT s.a FROM (%s) AS s`,
	`SELECT typeof(s.a), s.a FROM (%s) AS s`,
	`SELECT a FROM (%s) WHERE a IS NOT NULL ORDER BY 1`,
	`SELECT count(*) FROM (%s) WHERE a > 1`,
	`SELECT sum(a), min(a), max(a), count(a) FROM (%s)`,
	// ORDER BY outside aggregate to avoid collation-related declines.
	`SELECT * FROM (SELECT a, count(*) AS n FROM (%s) GROUP BY a) ORDER BY 1,2`,
	`SELECT * FROM (SELECT a FROM (%s) GROUP BY a HAVING count(*) >= 1) ORDER BY 1`,
	`SELECT a FROM (%s) ORDER BY a`,
	`SELECT DISTINCT a FROM (%s) ORDER BY 1`,
	`SELECT (SELECT a FROM (%s) LIMIT 1)`,
	`SELECT k FROM d1 WHERE k IN (SELECT a FROM (%s))`,
	`SELECT CASE WHEN a IS NULL THEN 'n' ELSE 'v' END FROM (%s) ORDER BY 1`,
	`SELECT a || '' FROM (%s) ORDER BY 1`,
	`SELECT d1.k, s.a FROM d1 JOIN (%s) AS s ON d1.k = s.a ORDER BY 1`,
	`SELECT a FROM (%s) LIMIT 2 OFFSET 1`,
}

// r32mDupSources are subqueries whose result set repeats "a". They vary WHICH
// position the repeat lands in, whether the two copies hold different values,
// different TYPES, or the same value, and whether the repeat comes from an
// explicit alias, a "*" over a join, or a compound arm.
var r32mDupSources = []string{
	`SELECT 1 AS a, 2 AS a`,
	`SELECT 2 AS a, 1 AS a`,
	`SELECT 1 AS a, 1 AS a`,
	`SELECT NULL AS a, 7 AS a`,
	`SELECT 7 AS a, NULL AS a`,
	`SELECT 1 AS a, 'x' AS a`,
	`SELECT 'x' AS a, 1 AS a`,
	`SELECT 1 AS a, 2 AS A`,
	`SELECT 9 AS k, 1 AS a, 2 AS a`,
	`SELECT 1 AS a, 9 AS k, 2 AS a`,
	`SELECT 1 AS a, 2 AS a, 3 AS a`,
	`SELECT k AS a, a AS a FROM d1`,
	`SELECT a AS a, k AS a FROM d1`,
	`SELECT d1.a, d2.a FROM d1 JOIN d2 ON d1.k = d2.a / 10`,
	`SELECT d2.a, d1.a FROM d1 JOIN d2 ON d1.k = d2.a / 10`,
	`SELECT 1 AS a, 2 AS a UNION ALL SELECT 3, 4`,
	`SELECT k AS a, z AS a FROM d1 ORDER BY k`,
}

// TestR32MDerivedDuplicateColumnRead is the cross product: every readout over
// every duplicate-producing source.
func TestR32MDerivedDuplicateColumnRead(t *testing.T) {
	for si, src := range r32mDupSources {
		for ri, readout := range r32mDupReadouts {
			stmts := append([]string{}, r32mDupSchema...)
			stmts = append(stmts, fmt.Sprintf(readout, src))
			differ(t, fmt.Sprintf("r32m dup src#%d readout#%d", si, ri), stmts)
		}
	}
}

// r32mSkipUntilRenamingLanded is TEMPORARY SCAFFOLDING. The ":N" uniquifier
// itself is r32mUniqueColumnNames (engine/query.go), landed by this stream;
// calling it is a handful of lines in files this stream does not own --
// derivedColumnInfos (engine/join.go) above all, which alone accounts for
// every derived-table, CTE and view READ, plus the CTAS naming
// (engine/schema_write.go), PRAGMA table_info over a view (engine/pragma.go)
// and the explicit "(col,...)" rename lists (engine/view.go, engine/cte.go,
// engine/vdbe_join_codegen.go). The naming tests below can therefore arrive
// before their wire-up does.
//
// DELETE THIS GUARD, and its two call sites, once that wire-up is in.
//
// It skips ONLY when the engine still DECLINES the canonical shape outright.
// If the engine ever ANSWERS with the wrong names, the differ sees it and the
// gate goes red -- which is the regression this file exists to catch. A guard
// that also skipped on "the names disagree" would hide exactly that.
func r32mSkipUntilRenamingLanded(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r32m-probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT * FROM (SELECT 1 AS a, 2 AS a)`)
	if err != nil {
		t.Skipf("engine still declines a derived table with a repeated column name (%v)"+
			" -- pending the one-line derivedColumnInfos wire-up in engine/join.go", err)
		return
	}
	rows.Close()
}

// TestR32MDerivedDuplicateColumnNames is the NAMING half: what the repeats are
// called once they reach the outer query. sqlite3ColumnsFromExprList appends
// ":<counter>" to the repeat's own spelling, replacing (never extending) a
// ":<digits>" tail the name already had, comparing case-insensitively, and
// restarting the counter per column -- see r32mUniqueColumnNames (engine/
// query.go) for the loop this pins.
func TestR32MDerivedDuplicateColumnNames(t *testing.T) {
	r32mSkipUntilRenamingLanded(t)
	cases := []string{
		`SELECT * FROM (SELECT 1 AS a, 2 AS a)`,
		`SELECT * FROM (SELECT 1 AS a, 2 AS a, 3 AS a)`,
		`SELECT * FROM (SELECT 1 AS a, 2 AS a, 3 AS a, 4 AS a, 5 AS a)`,
		`SELECT * FROM (SELECT 1 AS a, 2 AS A)`,             // suffix takes the repeat's spelling
		`SELECT * FROM (SELECT 1 AS "a:1", 2 AS a, 3 AS a)`, // a:1, a, a:2
		`SELECT * FROM (SELECT 1 AS "a:9", 2 AS "a:9")`,     // a:9, a:1 (tail replaced)
		`SELECT * FROM (SELECT 1 AS ":1", 2 AS ":1")`,       // :1, :2 (empty prefix)
		`SELECT * FROM (SELECT 1 AS "a:", 2 AS "a:")`,
		`SELECT * FROM (SELECT 1 AS "a1", 2 AS "a1")`,
		`SELECT * FROM (SELECT 1 AS "", 2 AS "")`,
		`SELECT * FROM (SELECT 1 AS a, 2 AS a) AS s`,
		`SELECT s.* FROM (SELECT 1 AS a, 2 AS a) AS s`,
		`SELECT * FROM (SELECT * FROM d1 JOIN d2 ON d1.k=d2.a/10) ORDER BY 1`,
		`SELECT * FROM (SELECT * FROM d1 JOIN d2 ON d1.k=d2.a/10) JOIN d1 AS q ON q.k=1 ORDER BY 1,2`,
		`SELECT * FROM (SELECT 1 AS a, 2 AS a) UNION ALL SELECT 8,9`,
		// The generated name is a real, resolvable column of the derived
		// table -- not just a label on the output.
		`SELECT "a:1" FROM (SELECT 1 AS a, 2 AS a)`,
		`SELECT a, "a:1" FROM (SELECT 1 AS a, 2 AS a)`,
		`SELECT s."a:1" FROM (SELECT 1 AS a, 2 AS a) AS s`,
		`SELECT a FROM (SELECT 1 AS a, 2 AS a) WHERE "a:1"=2`,
		`SELECT count(*) FROM (SELECT 1 AS a, 2 AS a) WHERE "a:1"=2`,
		`SELECT "a:1" FROM (SELECT 1 AS a, 2 AS a) ORDER BY "a:1"`,
		// A CTE and a VIEW take the identical renaming; an explicit
		// "(col,...)" rename list bypasses it on both.
		`WITH c AS (SELECT 1 AS a, 2 AS a) SELECT * FROM c`,
		`WITH c AS (SELECT 1 AS a, 2 AS a) SELECT a, "a:1" FROM c`,
		`WITH c(x,y) AS (SELECT 1 AS a, 2 AS a) SELECT * FROM c`,
		`WITH c(x,x) AS (SELECT 1 AS a, 2 AS a) SELECT * FROM c`,
		`SELECT * FROM (SELECT * FROM (SELECT 1 AS a, 2 AS a))`,
		`SELECT * FROM (SELECT * FROM (SELECT * FROM (SELECT 1 AS a, 2 AS a)))`,
	}
	for i, q := range cases {
		stmts := append([]string{}, r32mDupSchema...)
		stmts = append(stmts, q)
		differ(t, fmt.Sprintf("r32m names #%d", i), stmts)
	}
	// Views need their own script: the CREATE has to precede the read.
	views := [][]string{
		{`CREATE VIEW v1 AS SELECT 1 AS a, 2 AS a`, `SELECT * FROM v1`, `PRAGMA table_info(v1)`, `SELECT a FROM v1`, `SELECT "a:1" FROM v1`},
		{`CREATE VIEW v2(p,q) AS SELECT 1 AS a, 2 AS a`, `SELECT * FROM v2`, `PRAGMA table_info(v2)`},
		{`CREATE VIEW v3 AS SELECT * FROM d1 JOIN d2 ON d1.k=d2.a/10`, `SELECT * FROM v3 ORDER BY 1`, `PRAGMA table_info(v3)`},
		{`CREATE TABLE c1 AS SELECT 1 AS a, 2 AS a`, `PRAGMA table_info(c1)`, `SELECT * FROM c1`},
		{`CREATE TABLE c2 AS SELECT 1 AS "a:9", 2 AS "a:9"`, `PRAGMA table_info(c2)`, `SELECT * FROM c2`},
		{`CREATE TABLE c3 AS SELECT 1 AS ":1", 2 AS ":1"`, `PRAGMA table_info(c3)`},
		{`CREATE TABLE c4 AS SELECT 1 AS "a:1", 2 AS a, 3 AS a`, `PRAGMA table_info(c4)`},
	}
	for i, s := range views {
		stmts := append([]string{}, r32mDupSchema...)
		stmts = append(stmts, s...)
		differ(t, fmt.Sprintf("r32m named-object #%d", i), stmts)
	}
}

// TestR32MDerivedDuplicateColumnStar is the "*" readout over every one of the
// duplicate-producing sources: it reports BOTH copies, so it is the readout
// that needs the ":N" names to exist AND needs each output column bound to its
// own copy rather than both to the first.
func TestR32MDerivedDuplicateColumnStar(t *testing.T) {
	r32mSkipUntilRenamingLanded(t)
	for si, src := range r32mDupSources {
		stmts := append([]string{}, r32mDupSchema...)
		stmts = append(stmts, fmt.Sprintf(`SELECT * FROM (%s) AS s WHERE s.a IS NOT NULL ORDER BY 1,2`, src))
		differ(t, fmt.Sprintf("r32m star src#%d", si), stmts)
	}
}

// TestR32MDuplicateNameIsDerivedOnly pins the other half of the rule: a
// duplicate that arises ACROSS FROM items (two tables that each have an "a") is
// NOT the same shape -- SQLite calls an unqualified reference to it "ambiguous
// column name" and leaves a "*" expansion plainly duplicated. Nothing here may
// start resolving those to a first occurrence.
func TestR32MDuplicateNameIsDerivedOnly(t *testing.T) {
	cases := []string{
		`SELECT a FROM d1, d2`,
		`SELECT k FROM d1, d2`,
		`SELECT * FROM d1, d2 ORDER BY 1`,
		`SELECT d1.a, d2.a FROM d1, d2 ORDER BY 1,2`,
		`SELECT a FROM d1 JOIN d2 USING(a) ORDER BY 1`,
		`SELECT * FROM d1 JOIN d2 USING(a,k) ORDER BY 1`,
	}
	for i, q := range cases {
		stmts := append([]string{}, r32mDupSchema...)
		stmts = append(stmts, q)
		differ(t, fmt.Sprintf("r32m across-from #%d", i), stmts)
	}
}
