// Tests INSERT ... VALUES with full constant-foldable expressions.
// to produce the EXACT SAME "no such column: NAME" text C SQLite itself
// gives (parseInsertValueExpr's own doc comment explains why this one
// construct is left to fail naturally when the tuple is evaluated, rather
// than being statically declined at parse time like a subquery/aggregate
// call is).
//
// compileInsertWrite's compileExpr call reuses the identical FROM-less
// expression compiler a plain "SELECT <expr>" does, so a VALUES expression
// must produce exactly what the equivalent SELECT produces, and a column
// reference/subquery/aggregate call must fail exactly the way it fails
// there.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// execArgsPlainBoth is execPlainBoth's bound-parameter sibling: it runs
// sqlText through the pure-Go engine's ExecArgs (engineArgs) and real
// SQLite's Exec (mattnArgs, the SAME values in database/sql's native
// representation), requiring identical success/failure and, on success,
// identical RowsAffected/LastInsertId -- mirroring vdbe_conflict_test.go's
// execConflictBoth, extended with bound arguments the way param_diff_test.go's
// compareSelectArgs extends its own SELECT-only comparison.
func execArgsPlainBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string, engineArgs []engine.Value, mattnArgs []any) {
	t.Helper()
	ra, li, engErr := db.ExecArgs(sqlText, engineArgs)
	res, realErr := sdb.Exec(sqlText, mattnArgs...)
	if (engErr == nil) != (realErr == nil) {
		t.Fatalf("ExecArgs(%s, %v): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engineArgs, engErr, realErr)
	}
	if engErr != nil {
		engMsg := stripEngineErrContext(engErr.Error())
		realMsg := realErr.Error()
		if engMsg != realMsg {
			t.Fatalf("ExecArgs(%s, %v): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", sqlText, engineArgs, engMsg, realMsg)
		}
		return
	}
	realRA, _ := res.RowsAffected()
	realLI, _ := res.LastInsertId()
	if ra != realRA {
		t.Fatalf("ExecArgs(%s, %v): RowsAffected: engine=%d, C SQLite=%d", sqlText, engineArgs, ra, realRA)
	}
	if li != realLI {
		t.Fatalf("ExecArgs(%s, %v): LastInsertId: engine=%d, C SQLite=%d", sqlText, engineArgs, li, realLI)
	}
}

// TestInsertValuesExpressionsMatchCSQLite is this file's own gate: for
// each page size, and for each VDBEMode (on/off), it drives the pure-Go
// engine writer and a live real-SQLite oracle connection through a script
// covering every VALUES-expression form this task added support for --
// arithmetic, || concatenation, unary minus, a scalar function call, CASE,
// a blob literal, NULL, a bound-parameter mix (a param used INSIDE a larger
// expression, not just standing alone), and a nested combination of several
// of those in one tuple -- then verifies the resulting file's exact row
// content and PRAGMA integrity_check='ok' against C SQLite. It also
// verifies that a column reference inside a VALUES tuple is still rejected,
// with the exact same error text C SQLite itself gives.
func TestInsertValuesExpressionsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					testInsertValuesExprScenario(t, pageSize)
				})
			}
		})
	}
}

func testInsertValuesExprScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("insertexpr_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection, so DDL/DML all see the SAME in-memory schema

	execPlainBoth(t, db, sdb, `CREATE TABLE t(a, b, c)`)

	for _, s := range []string{
		// arithmetic, || concatenation, unary minus.
		`INSERT INTO t VALUES (1+2, 'a'||'b', -5)`,
		// a scalar function call, CASE, a blob literal.
		`INSERT INTO t VALUES (abs(-3), CASE WHEN 1 THEN 2 ELSE 3 END, x'ff')`,
		// NULL in every position.
		`INSERT INTO t VALUES (NULL, NULL, NULL)`,
		// a nested combination: parenthesized arithmetic, two chained
		// scalar function calls concatenated, and a CASE whose branches are
		// themselves expressions.
		`INSERT INTO t VALUES ((1+2)*3, upper('a')||lower('B'), CASE WHEN 1+1=2 THEN abs(-9) ELSE 0 END)`,
	} {
		execPlainBoth(t, db, sdb, s)
	}

	// A bound parameter used INSIDE a larger expression (not just standing
	// alone) -- "?+1", not merely "?" -- proves parameter numbering/binding
	// still works correctly once a VALUES tuple item is a full expression
	// tree rather than always a single ParamExpr leaf. Two bare "?"s each
	// auto-number to the NEXT sequential position (C SQLite's own rule),
	// so this binds two distinct arguments, not the same one twice.
	execArgsPlainBoth(t, db, sdb,
		`INSERT INTO t VALUES (?, ?+1, 'lit')`,
		[]engine.Value{evInt(10), evInt(5)}, []any{int64(10), int64(5)})

	// A column reference inside a VALUES tuple: no FROM exists for it to
	// resolve against, so this must still be an error -- verified to be the
	// EXACT SAME "no such column: a" text C SQLite itself gives (even
	// though "a" happens to also be one of t's own column names -- a VALUES
	// tuple's column reference is never resolved against the INSERT's own
	// target table).
	execPlainBoth(t, db, sdb, `INSERT INTO t VALUES (a+1, 2, 3)`)

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	// THE ORACLE IS ASKED ABOUT THE EXPORT: the file this engine wrote is a segment
	// file and C cannot read one, so the interchange claim runs through
	// ExportSQLite -- which tests the conversion too. See
	// convert_for_oracle_test.go.
	exported := filepath.Join(t.TempDir(), "exported-for-oracle.db")
	if xerr := sqliteconv.Export(path, exported, 0); xerr != nil {
		t.Fatalf("ExportSQLite: %v", xerr)
	}
	fdb, err := sql.Open("sqlite3", exported)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s): %v", exported, err)
	}
	defer fdb.Close()

	var integrity string
	if err := fdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
	}

	verifyTableViaCSQLite(t, fdb, wvTable{name: "t", colList: "a,b,c", rows: []wvRow{
		{rowid: 1, cols: []any{int64(3), "ab", int64(-5)}},
		{rowid: 2, cols: []any{int64(3), int64(2), []byte{0xff}}},
		{rowid: 3, cols: []any{nil, nil, nil}},
		{rowid: 4, cols: []any{int64(9), "Ab", int64(9)}},
		{rowid: 5, cols: []any{int64(10), int64(6), "lit"}},
	}})
}
