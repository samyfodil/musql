// This file tests TRUE/FALSE literals and IS DISTINCT FROM.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// boolLiteralScalarCases is the deterministic differential gate for both
// fixes, in their FROM-less scalar-expression form.
var boolLiteralScalarCases = []string{
	// ---- bare TRUE/FALSE literal ----
	`SELECT true`,
	`SELECT false`,
	`SELECT True`,
	`SELECT FALSE`,
	`SELECT typeof(true)`,
	`SELECT typeof(false)`,
	`SELECT true + 1`,
	`SELECT false + 1`,
	`SELECT true = 1`,
	`SELECT false = 0`,
	`SELECT true AND false`,
	`SELECT true OR false`,
	`SELECT NOT true`,
	`SELECT NOT false`,
	`SELECT -true`,
	`SELECT true || 'x'`,
	`SELECT CASE WHEN true THEN 1 ELSE 0 END`,
	`SELECT CASE WHEN false THEN 1 ELSE 0 END`,
	`SELECT iif(true, 'y', 'n')`,
	`SELECT 1 IN (true, false)`,
	`SELECT true IN (1, 2)`,

	// ---- quoted spellings: never the literal, always a plain (unresolved,
	// here -- no FROM/column in scope) column reference -- both engines must
	// reject identically (bracket/backtick quoting has no fallback-to-string
	// quirk in C SQLite either, unlike double-quoting -- see
	// boolKeywordFallback's doc comment) ----
	`SELECT [true]`,
	"SELECT `true`",
	`SELECT [false]`,
	"SELECT `false`",

	// ---- IS DISTINCT FROM / IS NOT DISTINCT FROM ----
	`SELECT 1 IS DISTINCT FROM 1`,
	`SELECT 1 IS DISTINCT FROM 2`,
	`SELECT NULL IS DISTINCT FROM NULL`,
	`SELECT NULL IS DISTINCT FROM 1`,
	`SELECT 1 IS NOT DISTINCT FROM 1`,
	`SELECT 1 IS NOT DISTINCT FROM 2`,
	`SELECT NULL IS NOT DISTINCT FROM NULL`,
	`SELECT NULL IS NOT DISTINCT FROM 1`,
	`SELECT 'a' IS DISTINCT FROM 'b'`,
	`SELECT 'a' IS DISTINCT FROM 'a'`,
	`SELECT typeof(1 IS DISTINCT FROM 2)`,
	`SELECT 1.5 IS NOT DISTINCT FROM 1.5`,
	`SELECT true IS DISTINCT FROM false`,
	`SELECT true IS NOT DISTINCT FROM 1`,

	// ---- IS [NOT] TRUE / IS [NOT] FALSE (a DIFFERENT production from an
	// ordinary "IS <expr>" comparison, even against the literal -- see
	// desugarIsBool's doc comment, engine/sql_parser.go) ----
	`SELECT 2 IS TRUE`,
	`SELECT 0 IS TRUE`,
	`SELECT NULL IS TRUE`,
	`SELECT -1 IS TRUE`,
	`SELECT 0.0 IS TRUE`,
	`SELECT 'abc' IS TRUE`,
	`SELECT '' IS TRUE`,
	`SELECT 2 IS FALSE`,
	`SELECT 0 IS FALSE`,
	`SELECT NULL IS FALSE`,
	`SELECT 2 IS NOT TRUE`,
	`SELECT 0 IS NOT TRUE`,
	`SELECT NULL IS NOT TRUE`,
	`SELECT 2 IS NOT FALSE`,
	`SELECT 0 IS NOT FALSE`,
	`SELECT NULL IS NOT FALSE`,
	`SELECT typeof(2 IS TRUE)`,
	`SELECT typeof(2 IS NOT FALSE)`,
	`SELECT (1 IN (2 IS TRUE))`, // regression case: C SQLite gives 1 (2 IS TRUE -> 1, not "2 IS (literal 1)" -> 0)
	`SELECT (1 IN (0 IS TRUE))`,
	`SELECT (1 IN (2 IS FALSE))`,
	`SELECT NOT (5 IS TRUE)`,
	`SELECT (5 IS TRUE) IS TRUE`,
	`SELECT (5 IS TRUE) IS FALSE`,
}

func TestBoolLiteralMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("boollit_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()

			sdb, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()

			for _, s := range boolLiteralScalarCases {
				t.Run(s, func(t *testing.T) {
					compareOneScalar(t, db, sdb, s)
				})
			}
		})
	}
}

// TestBoolLiteralSchemaScenarios covers the column-priority disambiguation
// (a real column/table-alias literally named "true"/"false" always wins
// over the keyword-literal fallback -- ColumnExpr.FallbackLiteral's doc
// comment, engine/sql_ast.go) and JOIN ON usage (the "SELECT: engine: JOIN
// ... ON: engine: no such column: true" bucket, ~187 TCL-corpus statements,
// almost entirely bare "ON true"/"ON false" join conditions) -- both need an
// actual schema, so can't be expressed as a scalar boolLiteralScalarCases
// entry. Also re-covers, end-to-end through a JOIN's WHERE-pushdown
// planning (join.go), the exact whereG.test-derived shape ("(0 <
// LIKELY(v0.c2)) IS TRUE" filtering a comma join) that caught the first
// (reverted) implementation of the IS TRUE/FALSE fix silently dropping the
// filter when it introduced a new Expr kind join.go's WHERE-pushdown
// table-dependency walkers didn't know about.
func TestBoolLiteralSchemaScenarios(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testBoolLiteralSchemaScenario(t, pageSize)
		})
	}
}

func testBoolLiteralSchemaScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("boollitschema_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	cgodb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()

	schema := []string{
		"CREATE TABLE t1(`true` INT, x INT)",
		"INSERT INTO t1(`true`, x) VALUES (5, 9)",
		"CREATE TABLE t2(a INT)",
		"INSERT INTO t2 VALUES (1)",
		"CREATE TABLE ja(a INT, b INT)",
		"INSERT INTO ja VALUES (1,2),(3,4)",
		"CREATE TABLE jb(b INT, x INT)",
		"INSERT INTO jb VALUES (2, 20),(4,40)",
		// The whereG.test-derived shape: a VIEW column that is itself a
		// CAST(... IS TRUE ...), joined via a comma-list, filtered by an
		// "IS TRUE"-wrapped condition over a LIKELY()-wrapped view column
		// reference.
		"CREATE TABLE wg(c0 INT)",
		"INSERT INTO wg(c0) VALUES (NULL)",
		"CREATE VIEW wgv(c2) AS SELECT CAST((c0 IS TRUE) AS TEXT) FROM wg",
	}
	for _, s := range schema {
		if err := db.Exec(s); err != nil {
			t.Fatalf("engine schema setup %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo schema setup %q: %v", s, err)
		}
	}

	cases := []string{
		// Column-priority: a real column/alias literally named true/false
		// always wins over the keyword literal.
		"SELECT `true` FROM t1",
		"SELECT t1.`true` FROM t1",
		"SELECT x FROM t1 WHERE `true` = 5",
		"SELECT x FROM t1 WHERE `true`",
		"SELECT a FROM t2 AS `true`",
		"SELECT `true`.a FROM t2 AS `true`",
		// No matching column anywhere: falls back to the literal even though
		// OTHER tables are in scope.
		"SELECT a FROM t2 WHERE true",
		"SELECT a FROM t2 WHERE false",

		// JOIN ON true/false (the ~187-statement corpus bucket).
		"SELECT ja.a, jb.x FROM ja INNER JOIN jb ON true WHERE ja.b=jb.b",
		"SELECT ja.a, jb.x FROM ja INNER JOIN jb ON false",
		"SELECT ja.a, jb.x FROM ja LEFT JOIN jb ON true",
		"SELECT ja.a, jb.x FROM ja LEFT JOIN jb ON false",

		// The whereG.test regression shape.
		"SELECT quote(c0), quote(c2) FROM wg, wgv WHERE (0 < LIKELY(wgv.c2))",
		"SELECT quote(c0), quote(c2) FROM wg, wgv WHERE (0 < LIKELY(wgv.c2)) IS TRUE",
		"SELECT wgv.c2 FROM wg, wgv WHERE 1 IS TRUE",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			gotCols, gotRows, qerr, panicked, panicVal := tclSafeGoQuery(db, s)
			if panicked {
				t.Fatalf("engine panicked: %v", panicVal)
			}
			cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, s)
			if (qerr == nil) != (cerr == nil) {
				t.Fatalf("engine err=%v (cols=%v rows=%v), cgo err=%v (cols=%v rows=%v)", qerr, gotCols, gotRows, cerr, cgoCols, cgoRows)
			}
			if qerr != nil {
				return // both declined identically -- never counted wrong
			}
			ok, reason := queryResultsMatch(gotCols, gotRows, cgoCols, cgoRows, false)
			if !ok {
				t.Errorf("mismatch: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", reason, gotCols, gotRows, cgoCols, cgoRows)
			}
		})
	}
}
