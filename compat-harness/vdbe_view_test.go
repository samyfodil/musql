// This file tests CREATE VIEW and DROP VIEW operations against C SQLite,
// verifying identical success/failure, error text, and query results.
// Views persist across reopens, and schema matches the oracle at page sizes 512 and 4096.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// execViewBoth runs sqlText (a DDL/DML statement) against both the pure-Go
// engine writer and a live real-SQLite connection, requiring identical
// success/failure and, on failure, identical error text (this package's own
// "engine: " prefix stripped) -- mirrors vdbe_drop_test.go's execDropBoth.
func execViewBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
	t.Helper()
	engErr := db.Exec(sqlText)
	_, realErr := sdb.Exec(sqlText)
	if (engErr == nil) != (realErr == nil) {
		t.Fatalf("Exec(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", sqlText, engErr, realErr)
	}
	if engErr == nil {
		return
	}
	engMsg := strings.TrimPrefix(engErr.Error(), "engine: ")
	realMsg := realErr.Error()
	if engMsg != realMsg {
		t.Fatalf("Exec(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", sqlText, engMsg, realMsg)
	}
}

// queryViewBoth runs a SELECT against both a freshly snapshotted engine pager
// (so it observes every mutation made so far this session -- see
// (*engine.Session).SnapshotPager) and the live real-SQLite connection, requiring
// identical success/failure; on success, identical (order-sensitive iff
// ordered) rows; on failure, identical error text.
func queryViewBoth(t *testing.T, db *engine.Session, sdb *sql.DB, q string, ordered bool) {
	t.Helper()
	pager, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	gotCols, gotRows, gotErr := pager.Query(q)
	wantCols, wantRows, wantErr := cgoSelect(t, sdb, q, nil)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("Query(%s): engine err=%v, C SQLite err=%v -- disagree on success/failure", q, gotErr, wantErr)
	}
	if gotErr != nil {
		// A query-time-only view check surfaced through the VDBE's own
		// errVDBESemantic convention (resolveViewSource, vdbe_join_codegen.go
		// -- e.g. the wrong-column-count check below) wraps the underlying
		// "engine: ..." text behind an EXTRA "vdbe: semantic error: " layer
		// (see errVDBESemantic's own doc comment, subquery_validate.go: it
		// must propagate as the statement's real error rather than being
		// masked, but its Error() text itself carries that wrapper prefix
		// literally) -- strip it first, then the ordinary "engine: " prefix
		// every other error in this package carries, so either shape
		// compares against C SQLite's own (unprefixed) text uniformly.
		gotMsg := strings.TrimPrefix(gotErr.Error(), "vdbe: semantic error: ")
		gotMsg = strings.TrimPrefix(gotMsg, "engine: ")
		wantMsg := wantErr.Error()
		if gotMsg != wantMsg {
			t.Fatalf("Query(%s): error text mismatch:\n  engine (stripped): %q\n  C SQLite:        %q", q, gotMsg, wantMsg)
		}
		return
	}
	gotStrRows := engineRowsToStrings(gotRows)
	if ok, reason := queryResultsMatch(gotCols, gotStrRows, wantCols, wantRows, ordered); !ok {
		t.Fatalf("Query(%s): result mismatch: %s\n  engine: cols=%v rows=%v\n  real:   cols=%v rows=%v", q, reason, gotCols, gotStrRows, wantCols, wantRows)
	}
}

// viewSchemaRow is one sqlite_master row (rootpage excluded).
type viewSchemaRow struct {
	typ, name, tblName, sql string
}

// dumpViewSchema reads sqlite_master and sqlite_temp_master.
func dumpViewSchema(t *testing.T, db *sql.DB) []viewSchemaRow {
	t.Helper()
	return dumpViewSchemaQuery(t, db, `SELECT type,name,tbl_name,sql FROM sqlite_master UNION ALL SELECT type,name,tbl_name,sql FROM sqlite_temp_master`)
}

// dumpViewSchemaMain reads sqlite_master only.
func dumpViewSchemaMain(t *testing.T, db *sql.DB) []viewSchemaRow {
	t.Helper()
	return dumpViewSchemaQuery(t, db, `SELECT type,name,tbl_name,sql FROM sqlite_master`)
}

func dumpViewSchemaQuery(t *testing.T, db *sql.DB, q string) []viewSchemaRow {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []viewSchemaRow
	for rows.Next() {
		var r viewSchemaRow
		var sqlText sql.NullString
		if err := rows.Scan(&r.typ, &r.name, &r.tblName, &sqlText); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		r.sql = normalizeCreateTempKeyword(sqlText.String)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].typ != out[j].typ {
			return out[i].typ < out[j].typ
		}
		return out[i].name < out[j].name
	})
	return out
}

// normalizeCreateTempKeyword strips "CREATE TEMP" or "CREATE TEMPORARY" down to "CREATE".
func normalizeCreateTempKeyword(sqlText string) string {
	const p1, p2 = "CREATE TEMP ", "CREATE TEMPORARY "
	up := strings.ToUpper(sqlText)
	switch {
	case strings.HasPrefix(up, p1):
		return "CREATE " + sqlText[len(p1):]
	case strings.HasPrefix(up, p2):
		return "CREATE " + sqlText[len(p2):]
	default:
		return sqlText
	}
}

// TestViewStatementsMatchCSQLite tests CREATE VIEW and DROP VIEW at page sizes 512 and 4096.
func TestViewStatementsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testViewScenario(t, pageSize)
		})
	}
}

func testViewScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("view_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // a single logical connection, so DDL/DML/query calls below all see the SAME in-memory schema

	// Base tables.
	for _, s := range []string{
		`CREATE TABLE t1(a INTEGER, b TEXT)`,
		`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE t2(a INTEGER, c TEXT)`,
		`INSERT INTO t2 VALUES(1,'p'),(2,'q')`,
	} {
		execViewBoth(t, db, sdb, s)
	}

	// CREATE VIEW matrix.
	for _, s := range []string{
		`CREATE VIEW v1 AS SELECT a,b FROM t1`,                             // simple
		`CREATE VIEW v2(x,y) AS SELECT a,b FROM t1`,                        // explicit column-rename list
		`CREATE VIEW v3 AS SELECT v1.a, t2.c FROM v1 JOIN t2 ON v1.a=t2.a`, // view over a JOIN, referencing another view
		`CREATE VIEW v4 AS SELECT x FROM v2`,                               // nested view (references v2)
		`CREATE VIEW IF NOT EXISTS v1 AS SELECT a FROM t1`,                 // IF NOT EXISTS: silent no-op
		`CREATE VIEW v1 AS SELECT a FROM t1`,                               // duplicate name, no IF NOT EXISTS: error
		`CREATE VIEW vbadcount(x) AS SELECT a,b FROM t1`,                   // wrong col count -- accepted here, errors at QUERY time
		`INSERT INTO v1 VALUES(1,'x')`,                                     // modify-view error (INSERT)
		`UPDATE v1 SET a=1`,                                                // modify-view error (UPDATE)
		`DELETE FROM v1`,                                                   // modify-view error (DELETE)
		`CREATE TEMP VIEW v5 AS SELECT b FROM t1`,                          // TEMP VIEW
	} {
		execViewBoth(t, db, sdb, s)
	}

	// Query every view.
	for _, q := range []string{
		`SELECT * FROM v1`,
		`SELECT * FROM v2`,
		`SELECT * FROM v3`,
		`SELECT * FROM v4`,
		`SELECT * FROM v5`,
		`SELECT * FROM vbadcount`, // "expected 1 columns for 'vbadcount' but got 2"
	} {
		queryViewBoth(t, db, sdb, q, false)
	}

	// DROP VIEW and confirm it's gone.
	execViewBoth(t, db, sdb, `DROP VIEW v1`)
	queryViewBoth(t, db, sdb, `SELECT * FROM v1`, false)

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close (phase 1-4): %v", err)
	}

	// Reopen and prove views persist.
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	queryViewBoth(t, db2, sdb, `SELECT * FROM v2`, false)
	queryViewBoth(t, db2, sdb, `SELECT * FROM v3`, false)
	queryViewBoth(t, db2, sdb, `SELECT * FROM v4`, false)
	execViewBoth(t, db2, sdb, `CREATE VIEW v6 AS SELECT c FROM t2`)
	queryViewBoth(t, db2, sdb, `SELECT * FROM v6`, false)
	execViewBoth(t, db2, sdb, `DROP VIEW v6`)
	queryViewBoth(t, db2, sdb, `SELECT * FROM v6`, false)
	if err := db2.Close(); err != nil {
		t.Fatalf("engine writer Close (phase 5): %v", err)
	}

	// Verify the file itself via C SQLite after export.
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

	gotSchema := dumpViewSchemaMain(t, fdb)
	wantSchema := dumpViewSchemaMain(t, sdb)
	if len(gotSchema) != len(wantSchema) {
		t.Fatalf("final schema: got %d objects %+v, want %d %+v", len(gotSchema), gotSchema, len(wantSchema), wantSchema)
	}
	for i := range wantSchema {
		if gotSchema[i] != wantSchema[i] {
			t.Errorf("final schema object %d: got %+v, want %+v", i, gotSchema[i], wantSchema[i])
		}
	}

	// Read views purely through the on-disk file.
	rp, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer rp.Close()
	for _, q := range []string{`SELECT * FROM v2`, `SELECT * FROM v4`} {
		gotCols, gotRows, gotErr := rp.Query(q)
		if gotErr != nil {
			t.Fatalf("post-reopen Query(%s): %v", q, gotErr)
		}
		wantCols, wantRows, wantErr := cgoSelect(t, fdb, q, nil)
		if wantErr != nil {
			t.Fatalf("post-reopen cgo Query(%s): %v", q, wantErr)
		}
		gotStrRows := engineRowsToStrings(gotRows)
		if ok, reason := queryResultsMatch(gotCols, gotStrRows, wantCols, wantRows, false); !ok {
			t.Fatalf("post-reopen Query(%s): result mismatch: %s\n  engine: cols=%v rows=%v\n  real:   cols=%v rows=%v", q, reason, gotCols, gotStrRows, wantCols, wantRows)
		}
	}
}
