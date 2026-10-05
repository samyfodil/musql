// Gate for DROP TABLE / DROP VIEW / DROP INDEX, comparing outcomes against C SQLite.
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

// execDropBoth runs the statement against both engines and requires matching outcomes.
func execDropBoth(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
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

// dropSchemaRow is one sqlite_master row (rootpage deliberately excluded --
// the pure-Go writer's and C SQLite's own page layouts differ, but the
// SET of surviving objects and their verbatim CREATE ... text must not).
type dropSchemaRow struct {
	typ, name, tblName, sql string
}

// dumpSchema reads db's sqlite_master (type,name,tbl_name,sql), sorted by
// (type,name) so two independently-built schemas compare order-independent.
func dumpSchema(t *testing.T, db *sql.DB) []dropSchemaRow {
	t.Helper()
	rows, err := db.Query(`SELECT type,name,tbl_name,sql FROM sqlite_master`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []dropSchemaRow
	for rows.Next() {
		var r dropSchemaRow
		var sqlText sql.NullString
		if err := rows.Scan(&r.typ, &r.name, &r.tblName, &sqlText); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		r.sql = sqlText.String
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

// TestDropStatementsMatchCSQLite is the DROP TABLE/VIEW/INDEX conformance
// gate: it drives the pure-Go engine writer AND a live real-SQLite oracle
// connection through the identical statement script (execDropBoth), requiring
// every step's success/failure and exact error text to agree; then, after a
// Close+OpenWrite+Close round-trip (proving drops persist across a reopen,
// not merely within one in-memory session), it requires the resulting file's
// own schema and surviving rows to be byte-faithful against C SQLite.
func TestDropStatementsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testDropScenario(t, pageSize)
		})
	}
}

func testDropScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("drop_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // a single logical connection, so DDL/DML/schema queries below all see the SAME in-memory schema (see execDropBoth's doc comment)

	// ---- phase 1: build a schema with plenty for DROP to chew on ----
	// t: a plain table with an explicit index (idx1) -- surviving the whole
	// script, so its row content AND its now-index-less schema can be
	// checked at the very end.
	// u: a UNIQUE column, so it carries an automatic index
	// (sqlite_autoindex_u_1) -- dropped via DROP TABLE later, cascading.
	// w: a UNIQUE column PLUS an explicit index (idxw) -- both must vanish
	// when w itself is dropped.
	// leftover: untouched by any DROP in this script, verifying an
	// unrelated table's rows survive every drop below unharmed.
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER, b TEXT)`,
		`CREATE INDEX idx1 ON t(b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE u(a INTEGER UNIQUE, c INTEGER)`,
		`INSERT INTO u VALUES(1,10),(2,20)`,
		`CREATE TABLE w(a INTEGER UNIQUE, b INTEGER)`,
		`CREATE INDEX idxw ON w(b)`,
		`INSERT INTO w VALUES(1,100),(2,200)`,
		`CREATE TABLE leftover(x TEXT)`,
		`INSERT INTO leftover VALUES('keep-me')`,
	} {
		execDropBoth(t, db, sdb, s)
	}

	// ---- phase 2: the DROP error/edge-case matrix, each checked against
	// C SQLite's own live outcome (see execDropBoth) ----
	for _, s := range []string{
		`DROP TABLE nope`,                    // no such table
		`DROP TABLE IF EXISTS nope`,          // IF EXISTS swallows it
		`DROP VIEW nope`,                     // no such view
		`DROP VIEW IF EXISTS nope`,           // IF EXISTS swallows it
		`DROP INDEX nope`,                    // no such index
		`DROP INDEX IF EXISTS nope`,          // IF EXISTS swallows it
		`DROP VIEW t`,                        // wrong DDL verb: t is a table
		`DROP VIEW IF EXISTS t`,              // IF EXISTS does NOT swallow the wrong-DDL-verb error (only "not found")
		`DROP INDEX t`,                       // wrong namespace: t is not an index
		`DROP INDEX sqlite_autoindex_u_1`,    // automatic index: cannot be dropped
		`DROP TABLE sqlite_master`,           // reserved
		`DROP TABLE IF EXISTS sqlite_master`, // IF EXISTS does NOT swallow the reserved-name error either
		`DROP VIEW sqlite_master`,            // reserved (same wording as DROP TABLE)
		`DROP TABLE main.nope2`,              // schema-qualified not-found (message repeats the qualifier)
		`DROP TABLE bogusdb.t`,               // t exists, but under an unresolvable schema qualifier -- still not found
		`DROP INDEX bogusdb.idx1`,            // idx1 exists, but under an unresolvable schema qualifier -- still not found
		`DROP INDEX main.idx1`,               // ordinary explicit index, schema-qualified: succeeds
		`DROP INDEX idx1`,                    // already gone: no such index
		`DROP TABLE w`,                       // cascades idxw + sqlite_autoindex_w_1
		`DROP TABLE u`,                       // cascades sqlite_autoindex_u_1
	} {
		execDropBoth(t, db, sdb, s)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close (phase 1): %v", err)
	}

	// ---- phase 3: reopen the file and prove drops persist, and that a
	// freshly reopened *DB can still both create and drop correctly ----
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE post(a INTEGER)`,
		`INSERT INTO post VALUES(42)`,
		`DROP TABLE post`, // drop something created THIS reopened session
		`DROP TABLE nope3`,
	} {
		execDropBoth(t, db2, sdb, s)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("engine writer Close (phase 3): %v", err)
	}

	// ---- final verification: the file itself, via C SQLite ----
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

	gotSchema := dumpSchema(t, fdb)
	wantSchema := dumpSchema(t, sdb)
	if len(gotSchema) != len(wantSchema) {
		t.Fatalf("final schema: got %d objects %+v, want %d %+v", len(gotSchema), gotSchema, len(wantSchema), wantSchema)
	}
	for i := range wantSchema {
		if gotSchema[i] != wantSchema[i] {
			t.Errorf("final schema object %d: got %+v, want %+v", i, gotSchema[i], wantSchema[i])
		}
	}

	verifyTableViaCSQLite(t, fdb, wvTable{name: "t", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "x"}},
		{rowid: 2, cols: []any{int64(2), "y"}},
		{rowid: 3, cols: []any{int64(3), "z"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "leftover", colList: "x", rows: []wvRow{
		{rowid: 1, cols: []any{"keep-me"}},
	}})
}
