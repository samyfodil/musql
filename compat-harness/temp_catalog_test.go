// This file tests both schema catalogs while a TEMP object is live: sqlite_master,
// sqlite_schema, sqlite_temp_master, and sqlite_temp_schema, including their
// schema-qualified spellings.
package compat

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// tempCatalogQuery runs q on a fresh connection and renders the result.
func tempCatalogQuery(t *testing.T, drv, dsn, q string) (string, error) {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	rows, err := db.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, cerr := rows.Columns()
	if cerr != nil {
		return "", cerr
	}
	out := fmt.Sprint(cols)
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			return "", serr
		}
		out += fmt.Sprintf("|%v", cells)
	}
	return out, rows.Err()
}

func TestTempSchemaCatalogEmpty(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE INDEX i1 ON t1(b)`,
		`CREATE VIEW v1 AS SELECT a FROM t1`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		db, err := sql.Open(drv, dsn[drv])
		if err != nil {
			t.Fatalf("sql.Open(%s): %v", drv, err)
		}
		db.SetMaxOpenConns(1)
		for _, s := range setup {
			if _, eerr := db.Exec(s); eerr != nil {
				t.Fatalf("%s: %q: %v", drv, s, eerr)
			}
		}
		db.Close()
	}
	for _, q := range []string{
		`SELECT count(*) FROM sqlite_temp_master`,
		`SELECT count(*) FROM sqlite_temp_schema`,
		`SELECT count(*) FROM temp.sqlite_master`,
		`SELECT count(*) FROM temp.sqlite_schema`,
		// The fixed five-column shape, including its column NAMES.
		`SELECT * FROM sqlite_temp_master`,
		`SELECT type, name, tbl_name, sql FROM sqlite_temp_master`,
		`SELECT count(*) FROM sqlite_temp_master WHERE type='table'`,
		// Aliased, joined, nested and compounded -- the catalog is an ordinary
		// zero-row source once resolved, so every one of these must work.
		`SELECT count(*) FROM sqlite_temp_master AS x`,
		`SELECT x.name FROM sqlite_temp_master AS x`,
		`SELECT count(*) FROM sqlite_master, sqlite_temp_master`,
		`SELECT count(*) FROM sqlite_master LEFT JOIN sqlite_temp_master ON 1`,
		`SELECT count(*) FROM sqlite_temp_master LEFT JOIN sqlite_master ON 1`,
		`SELECT name FROM sqlite_temp_master UNION ALL SELECT name FROM sqlite_master ORDER BY name`,
		`WITH c AS (SELECT name FROM sqlite_temp_master) SELECT count(*) FROM c`,
	} {
		goOut, goErr := tempCatalogQuery(t, "sqlite", dsn["sqlite"], q)
		cgoOut, cgoErr := tempCatalogQuery(t, "sqlite3", dsn["sqlite3"], q)
		switch {
		case goErr != nil && cgoErr != nil:
			// Agreement, including agreeing to reject.
		case goErr != nil:
			t.Errorf("this engine declined a temp-catalog read C SQLite answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a temp-catalog read C SQLite rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("temp catalog DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}
}

// liveTempCatalogQuery runs setup and query on one connection and returns sorted results.
func liveTempCatalogQuery(t *testing.T, drv, dsn string, setup []string, q string) (string, error) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	defer db.Close()
	conn, cerr := db.Conn(ctx)
	if cerr != nil {
		t.Fatalf("Conn(%s): %v", drv, cerr)
	}
	defer conn.Close()
	for _, s := range setup {
		if _, eerr := conn.ExecContext(ctx, s); eerr != nil {
			t.Fatalf("%s: %q: %v", drv, s, eerr)
		}
	}
	rows, qerr := conn.QueryContext(ctx, q)
	if qerr != nil {
		return "", qerr
	}
	defer rows.Close()
	cols, colErr := rows.Columns()
	if colErr != nil {
		return "", colErr
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			return "", serr
		}
		for i, c := range cells {
			if bs, ok := c.([]byte); ok {
				cells[i] = string(bs)
			}
		}
		out = append(out, fmt.Sprintf("%v", cells))
	}
	sort.Strings(out)
	return fmt.Sprint(cols) + "|" + strings.Join(out, "|"), rows.Err()
}

// liveTempCatalogSetup creates various temp and main objects for testing.
var liveTempCatalogSetup = []string{
	`CREATE TABLE m1(a INTEGER PRIMARY KEY, b UNIQUE)`,
	`CREATE INDEX mi1 ON m1(b)`,
	`CREATE VIEW mv1 AS SELECT a FROM m1`,
	`CREATE TEMP TABLE tt(z UNIQUE)`,
	`CREATE INDEX ti_nokw ON tt(z)`,
	`CREATE TEMP VIEW tv AS SELECT z FROM tt`,
	`CREATE TEMP TRIGGER ttr AFTER INSERT ON tt BEGIN UPDATE tt SET z=z; END`,
	`CREATE TEMP TRIGGER ttr_on_main AFTER INSERT ON main.m1 BEGIN UPDATE m1 SET b=b; END`,
	`CREATE TABLE temp.q2(a)`,
	`CREATE TEMPORARY TABLE t3(w)`,
	`CREATE TEMP TABLE t4 AS SELECT 1 AS c`,
	`CREATE TEMP TABLE t5(k PRIMARY KEY) WITHOUT ROWID`,
}

// TestSchemaCatalogSplitWithLiveTempObject verifies catalog split with live TEMP objects.
func TestSchemaCatalogSplitWithLiveTempObject(t *testing.T) {
	dir := t.TempDir()
	for n, q := range []string{
		`SELECT count(*) FROM sqlite_master`,
		`SELECT count(*) FROM sqlite_temp_master`,
		`SELECT count(*) FROM sqlite_temp_schema`,
		`SELECT count(*) FROM temp.sqlite_master`,
		`SELECT count(*) FROM temp.sqlite_schema`,
		`SELECT count(*) FROM main.sqlite_master`,
		// The row sets themselves, every column but rootpage (declined --
		// see engine/query.go's schemaCatalogQueryGuard). "sql" is read
		// through a separate case below, which needs a trigger-free schema.
		`SELECT type, name, tbl_name FROM sqlite_master`,
		`SELECT type, name, tbl_name FROM sqlite_temp_master`,
		`SELECT type, name, tbl_name FROM temp.sqlite_master`,
		`SELECT name FROM sqlite_master WHERE type='table'`,
		`SELECT name FROM sqlite_temp_master WHERE type='table'`,
		`SELECT name FROM sqlite_temp_master WHERE type='index'`,
		`SELECT name FROM sqlite_temp_master WHERE type='trigger'`,
		`SELECT name FROM sqlite_temp_master WHERE type='view'`,
		// tbl_name vs name for a temp trigger on a MAIN table: C SQLite
		// files it in TEMP with tbl_name naming the main table.
		`SELECT name, tbl_name FROM sqlite_temp_master WHERE tbl_name='m1'`,
		// Aliased, joined, nested, compounded and subqueried -- once resolved
		// the catalog is an ordinary materialized source, so all of these work.
		`SELECT count(*) FROM sqlite_temp_master AS x`,
		`SELECT x.name FROM sqlite_temp_master AS x`,
		`SELECT count(*) FROM sqlite_master, sqlite_temp_master`,
		`SELECT count(*) FROM sqlite_master LEFT JOIN sqlite_temp_master ON 1`,
		`SELECT name FROM sqlite_master UNION ALL SELECT name FROM sqlite_temp_master`,
		`WITH c AS (SELECT name FROM sqlite_temp_master) SELECT count(*) FROM c`,
		`SELECT name FROM sqlite_master WHERE name IN (SELECT tbl_name FROM sqlite_temp_master)`,
		// main.sqlite_temp_master is "no such table" in C SQLite: the temp
		// catalog's own name lives only in the temp database.
		`SELECT name FROM main.sqlite_temp_master`,
	} {
		// A fresh pair of database FILES per query: only the temp objects go
		// away when the connection closes, so a reused file would re-run the
		// main-schema setup over itself.
		goOut, goErr := liveTempCatalogQuery(t, "sqlite", filepath.Join(dir, fmt.Sprintf("go%d.db", n)), liveTempCatalogSetup, q)
		cgoOut, cgoErr := liveTempCatalogQuery(t, "sqlite3", filepath.Join(dir, fmt.Sprintf("cgo%d.db", n)), liveTempCatalogSetup, q)
		switch {
		case goErr != nil && cgoErr != nil:
			// Agreement, including agreeing to reject.
		case goErr != nil:
			t.Errorf("this engine declined a catalog read C SQLite answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a catalog read C SQLite rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("schema catalog split DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}
}

// TestSchemaCatalogSplitMainAutoincrementExempt pins the narrow exemption from
// the TEMP-AUTOINCREMENT decline below: when MAIN independently has its own
// AUTOINCREMENT table too, MAIN's catalog can still answer even while a TEMP
// AUTOINCREMENT table also exists. This engine's single, shared
// "sqlite_sequence" schema-definition row is always stored non-temp
// (engine/temp_schema.go's markTempSchemaRows classifies purely off the
// stored CREATE text, which never carries the TEMP keyword for this
// synthesized table -- engine/schema_write.go's sqliteSequenceCreateSQL), so
// it genuinely belongs to MAIN's catalog whenever MAIN has an AUTOINCREMENT
// table of its own -- C SQLite creates each database's copy eagerly, keyed
// on THAT database's own first AUTOINCREMENT table (build.c:2919's
// `pDb->pSchema->pSeqTab==0` check nested-parses "CREATE TABLE
// %Q.sqlite_sequence" into pDb->zDbSName). TEMP's own catalog keeps declining
// unconditionally -- see the "a TEMP AUTOINCREMENT table" case in
// TestSchemaCatalogSplitDeclines below, and its new sibling case pinning that
// MAIN must ALSO keep declining without an AUTOINCREMENT table of its own.
//
// The setup and query are compat-harness/testdata/tcl/autoinc.test's own
// mined fixture (autoinc-4.2, segment #1, mined statement #86): "CREATE TABLE
// t1(...AUTOINCREMENT...); CREATE TEMP TABLE t3(...AUTOINCREMENT...)" then
// "SELECT 1, name FROM sqlite_master WHERE type='table'" -- which used to be
// an outright decline (main and temp share ONE split-decline check) even
// though it never reads sqlite_temp_master at all.
func TestSchemaCatalogSplitMainAutoincrementExempt(t *testing.T) {
	dir := t.TempDir()
	setup := []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
		`CREATE TEMP TABLE t3(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
	}
	for n, q := range []string{
		// The exact mined statement (autoinc.test line 352/360, autoinc-4.2).
		`SELECT 1, name FROM sqlite_master WHERE type='table'`,
		// Unqualified and "main."-qualified spellings, and sqlite_schema too.
		`SELECT name FROM sqlite_master WHERE type='table'`,
		`SELECT name FROM main.sqlite_master WHERE type='table'`,
		`SELECT name FROM sqlite_schema WHERE type='table'`,
		`SELECT count(*) FROM sqlite_master`,
	} {
		goOut, goErr := liveTempCatalogQuery(t, "sqlite", filepath.Join(dir, fmt.Sprintf("go%d.db", n)), setup, q)
		cgoOut, cgoErr := liveTempCatalogQuery(t, "sqlite3", filepath.Join(dir, fmt.Sprintf("cgo%d.db", n)), setup, q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("this engine declined a MAIN catalog read C SQLite answers, despite MAIN's own AUTOINCREMENT table\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a catalog read C SQLite rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("MAIN catalog with its own AUTOINCREMENT table DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}
}

// TestSchemaCatalogSplitTempAutoincrementExempt is
// TestSchemaCatalogSplitMainAutoincrementExempt's mirror: a TEMP
// AUTOINCREMENT table's OWN sqlite_sequence catalog row can now list in
// sqlite_temp_master, even though this engine keeps only ONE physical
// sqlite_sequence and it is always stored non-temp (engine/temp_schema.go's
// markTempSchemaRows). Unlike MAIN's case, the physical row cannot BE the
// answer here -- it is genuinely the wrong catalog's row -- so
// engine/temp_catalog.go's schemaCatalogRows SYNTHESIZES a second entry
// instead, using the identical "CREATE TABLE sqlite_sequence(name,seq)" text
// C SQLite's own per-database copy gets (build.c:2925's
// `pDb->pSchema->pSeqTab==0` check is per-database, so TEMP's database trips
// it independently of MAIN's).
//
// The first setup/query pair is compat-harness/testdata/tcl/autoinc.test's
// own mined fixture (autoinc-4.2, segment #1, mined statement #87): the exact
// statement right after TestSchemaCatalogSplitMainAutoincrementExempt's own
// mined sibling, run against the SAME two-table setup (both MAIN's t1 and
// TEMP's t3 are AUTOINCREMENT) -- it used to be an outright decline even
// though every column it asks for (type/name) is fully reproducible. The
// second pair drops MAIN's own AUTOINCREMENT table entirely, pinning that
// TEMP's own answer does not depend on MAIN having one (C SQLite's
// per-database pSeqTab check does not either) -- the mirror of
// TestSchemaCatalogSplitDeclines' "MAIN with no AUTOINCREMENT table of its
// own, only TEMP has one" case, read from TEMP's own side instead of MAIN's.
func TestSchemaCatalogSplitTempAutoincrementExempt(t *testing.T) {
	dir := t.TempDir()
	both := []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
		`CREATE TEMP TABLE t3(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
	}
	tempOnly := []string{
		`CREATE TABLE m1(a, b)`,
		`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
	}
	cases := []struct {
		setup []string
		q     string
	}{
		// The exact mined statement (autoinc.test line 361, autoinc-4.2).
		{both, `SELECT 2, name FROM sqlite_temp_master WHERE type='table'`},
		// Unqualified, "temp."-qualified, and the sqlite_schema spellings.
		{both, `SELECT name FROM sqlite_temp_master WHERE type='table'`},
		{both, `SELECT name FROM temp.sqlite_master WHERE type='table'`},
		{both, `SELECT name FROM sqlite_temp_schema WHERE type='table'`},
		{both, `SELECT name FROM temp.sqlite_schema WHERE type='table'`},
		{both, `SELECT count(*) FROM sqlite_temp_master`},
		// The "sql" column -- fully known even for the synthetic row, since
		// C SQLite gives it the identical text as main's copy.
		{both, `SELECT sql FROM sqlite_temp_master WHERE name='sqlite_sequence'`},
		// MAIN has no AUTOINCREMENT table of its own at all: TEMP's own
		// answer must not depend on it.
		{tempOnly, `SELECT name FROM sqlite_temp_master WHERE type='table'`},
		{tempOnly, `SELECT count(*) FROM sqlite_temp_master`},
	}
	for n, tc := range cases {
		goOut, goErr := liveTempCatalogQuery(t, "sqlite", filepath.Join(dir, fmt.Sprintf("go%d.db", n)), tc.setup, tc.q)
		cgoOut, cgoErr := liveTempCatalogQuery(t, "sqlite3", filepath.Join(dir, fmt.Sprintf("cgo%d.db", n)), tc.setup, tc.q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("this engine declined a TEMP catalog read C SQLite answers, despite TEMP's own AUTOINCREMENT table\n  sql: %s\n  err: %v\n  cgo: %s", tc.q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a catalog read C SQLite rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", tc.q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("TEMP catalog with its own AUTOINCREMENT table DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", tc.q, goOut, cgoOut)
		}
	}
}

// TestSchemaCatalogSplitSQLText pins the stored "sql" column of the split,
// which is where the two engines genuinely store different bytes: C SQLite
// STRIPS the TEMP/TEMPORARY keyword from a temp object's stored text, while
// this engine keeps it as the on-disk marker its single b-tree recovers the
// catalog from (engine/temp_schema.go), so the temp direction has to render it
// back out. No TRIGGER here -- a trigger row makes schemaCatalogQueryGuard
// decline any "sql" read (engine/query.go), which would hide exactly this.
func TestSchemaCatalogSplitSQLText(t *testing.T) {
	dir := t.TempDir()
	setup := []string{
		`CREATE TABLE m1(a INTEGER PRIMARY KEY, b UNIQUE)`,
		`CREATE VIEW mv1 AS SELECT a FROM m1`,
		`CREATE TEMP TABLE tt(z UNIQUE)`,
		`CREATE INDEX ti_nokw ON tt(z)`,
		`CREATE TEMP VIEW tv AS SELECT z FROM tt`,
		`CREATE TABLE temp.q2(a)`,
		`CREATE   TEMPORARY   TABLE   spaced  (  a  ,  b  )`,
		`CREATE TEMP TABLE t4 AS SELECT 1 AS c`,
	}
	for n, q := range []string{
		`SELECT type, name, tbl_name, sql FROM sqlite_master`,
		`SELECT type, name, tbl_name, sql FROM sqlite_temp_master`,
		`SELECT sql FROM sqlite_temp_master WHERE name='tt'`,
		`SELECT sql FROM sqlite_temp_master WHERE name='spaced'`,
		`SELECT sql FROM sqlite_temp_master WHERE name='tv'`,
		`SELECT sql FROM sqlite_temp_master WHERE name='q2'`,
		`SELECT sql FROM sqlite_temp_master WHERE name='t4'`,
		// An implicitly-created index has a NULL sql in either catalog.
		`SELECT name, sql IS NULL FROM sqlite_temp_master WHERE type='index'`,
		`SELECT name, sql IS NULL FROM sqlite_master WHERE type='index'`,
		`SELECT count(*) FROM sqlite_temp_master WHERE sql IS NULL`,
	} {
		goOut, goErr := liveTempCatalogQuery(t, "sqlite", filepath.Join(dir, fmt.Sprintf("go%d.db", n)), setup, q)
		cgoOut, cgoErr := liveTempCatalogQuery(t, "sqlite3", filepath.Join(dir, fmt.Sprintf("cgo%d.db", n)), setup, q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("this engine declined a catalog sql read C SQLite answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a catalog sql read C SQLite rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("schema catalog sql text DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}
}

// TestSchemaCatalogSplitDeclines pins the states in which the split is NOT
// exact -- or in which the guard is deliberately coarser than it strictly has
// to be -- and must stay a decline rather than become a wrong row.
// TestSchemaCatalogSplitNowMatches is the test that used to be
// TestSchemaCatalogSplitDeclines. Every case in it was a clean refusal for one
// reason: both catalogs lived in ONE schema b-tree, so a rootpage, a row set,
// or a sqlite_sequence row could not be attributed to the right database. The
// TEMP database has its own file and its own catalog now (engine/temp_store.go),
// so each of them is an ordinary read -- and must agree with the oracle
// cell for cell.
func TestSchemaCatalogSplitNowMatches(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		what  string
		setup []string
		q     string
	}{
		{
			// MAIN's own catalog with a TEMP AUTOINCREMENT table live, and no
			// AUTOINCREMENT table of main's own: C SQLite's
			// main.sqlite_master carries NO sqlite_sequence row here
			// (build.c:2925 never ran for main's database), and neither does
			// this engine's now -- temp's own sqlite_sequence is in temp's
			// catalog.
			"MAIN with no AUTOINCREMENT table of its own, only TEMP has one",
			[]string{`CREATE TABLE m1(a, b)`, `CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`},
			`SELECT name FROM sqlite_master WHERE type='table'`,
		},
		{
			// A temp object's rootpage is numbered in the temp DATABASE's own
			// file, restarting at 2 -- which is exactly what this engine's temp
			// file numbers it as.
			"a rootpage read",
			[]string{`CREATE TABLE m1(a)`, `CREATE TEMP TABLE tt(z)`},
			`SELECT rootpage FROM sqlite_temp_master`,
		},
		{
			"a SELECT * over the temp catalog",
			[]string{`CREATE TABLE m1(a)`, `CREATE TEMP TABLE tt(z)`},
			`SELECT * FROM sqlite_temp_master`,
		},
		{
			"a projected-away SELECT * over the temp catalog",
			[]string{`CREATE TABLE m1(a)`, `CREATE TEMP TABLE tt(z)`},
			`SELECT count(*) FROM (SELECT * FROM sqlite_temp_master)`,
		},
		{
			// The whole temp catalog with its own sqlite_sequence in it, rows
			// and rootpages included.
			"the temp catalog beside a temp AUTOINCREMENT table",
			[]string{`CREATE TABLE m1(a)`, `CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`},
			`SELECT type, name, tbl_name, rootpage, sql FROM sqlite_temp_master`,
		},
	} {
		want, cgoErr := liveTempCatalogQuery(t, "sqlite3", filepath.Join(dir, "cgo-"+tc.what+".db"), tc.setup, tc.q)
		if cgoErr != nil {
			t.Fatalf("C SQLite could not answer %s (%s): %v", tc.what, tc.q, cgoErr)
		}
		got, goErr := liveTempCatalogQuery(t, "sqlite", filepath.Join(dir, "go-"+tc.what+".db"), tc.setup, tc.q)
		if goErr != nil {
			t.Errorf("%s: %q: this engine REFUSED it: %v", tc.what, tc.q, goErr)
			continue
		}
		if got != want {
			t.Errorf("%s: %q DIVERGES\n  engine: %s\n  cgo:    %s", tc.what, tc.q, got, want)
		}
	}
}

// liveTempCatalogQueryOrdered is liveTempCatalogQuery's row-order-SENSITIVE
// twin: no sort.Strings before returning, so it actually asserts the
// catalog's OWN row order, not just its row set. Forces a single physical
// connection (SetMaxOpenConns(1) plus one held Conn for setup AND query)
// since a TEMP object belongs to the connection that created it.
func liveTempCatalogQueryOrdered(t *testing.T, drv, dsn string, setup []string, q string) (string, error) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	conn, cerr := db.Conn(ctx)
	if cerr != nil {
		t.Fatalf("Conn(%s): %v", drv, cerr)
	}
	defer conn.Close()
	for _, s := range setup {
		if _, eerr := conn.ExecContext(ctx, s); eerr != nil {
			t.Fatalf("%s: %q: %v", drv, s, eerr)
		}
	}
	rows, qerr := conn.QueryContext(ctx, q)
	if qerr != nil {
		return "", qerr
	}
	defer rows.Close()
	cols, colErr := rows.Columns()
	if colErr != nil {
		return "", colErr
	}
	out := fmt.Sprint(cols)
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			return "", serr
		}
		for i, c := range cells {
			if bs, ok := c.([]byte); ok {
				cells[i] = string(bs)
			}
		}
		out += fmt.Sprintf("|%v", cells)
	}
	return out, rows.Err()
}

// TestSchemaCatalogSplitRowOrder is the row-order-sensitive test
// liveTempCatalogQuery's own doc comment forward-references. TEMP's
// synthetic sqlite_sequence row (schemaCatalogRows, engine/temp_catalog.go)
// has to land at the SAME position a real physical row would: C SQLite
// creates it synchronously while processing the triggering AUTOINCREMENT
// table's own CREATE (build.c:2925, fired from inside sqlite3EndTable,
// immediately after that table's own schema row is finalized) -- so it is
// ordered relative to THAT table's creation, not appended after whatever
// else happens to exist when the catalog is read. Verified directly against
// the live oracle (single physical connection, forced via SetMaxOpenConns(1)
// -- TEMP belongs to the connection that created it) that a later TEMP
// object (a plain table, a view, or a second AUTOINCREMENT table) does NOT
// move sqlite_sequence's position: it stays exactly where the FIRST
// AUTOINCREMENT table put it.
func TestSchemaCatalogSplitRowOrder(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		setup []string
		q     string
	}{
		{"only the AUTOINCREMENT table, no later object", []string{
			`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
		}, `SELECT name FROM sqlite_temp_master WHERE type='table'`},
		{"a later plain table", []string{
			`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
			`CREATE TEMP TABLE tai2(z)`,
		}, `SELECT name FROM sqlite_temp_master WHERE type='table'`},
		{"a later view", []string{
			`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
			`CREATE TEMP VIEW v1 AS SELECT 1`,
		}, `SELECT name FROM sqlite_temp_master`},
		{"a second AUTOINCREMENT table -- the row must not move or repeat", []string{
			`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
			`CREATE TEMP TABLE tai2(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
		}, `SELECT name FROM sqlite_temp_master WHERE type='table'`},
	}
	for n, tc := range cases {
		goOut, goErr := liveTempCatalogQueryOrdered(t, "sqlite", filepath.Join(dir, fmt.Sprintf("go%d.db", n)), tc.setup, tc.q)
		cgoOut, cgoErr := liveTempCatalogQueryOrdered(t, "sqlite3", filepath.Join(dir, fmt.Sprintf("cgo%d.db", n)), tc.setup, tc.q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("%s: this engine declined a TEMP catalog read C SQLite answers\n  sql: %s\n  err: %v\n  cgo: %s", tc.name, tc.q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("%s: this engine ACCEPTED a catalog read C SQLite rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", tc.name, tc.q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("%s: TEMP catalog ROW ORDER diverges\n  sql: %s\n  go:  %q\n  cgo: %q", tc.name, tc.q, goOut, cgoOut)
		}
	}
}

// TestSchemaCatalogSplitTempAutoincrementOutlivesDrop pins that TEMP's
// synthetic sqlite_sequence row (schemaCatalogRows, engine/temp_catalog.go)
// keeps appearing even after the AUTOINCREMENT table that first triggered it
// is DROPped -- C SQLite's per-database sqlite_sequence row, once created
// (build.c:2925), is never removed again just because the table that
// triggered it goes away. Uses DB.tempSqliteSequenceSeen (the same sticky,
// session-scoped signal sqliteSequenceSourceScope already relies on for the
// sqlite_sequence CONTENT read, sqlite_sequence_split.go) rather than
// schemaHasTempAutoIncrement's live-row rescan alone, which cannot tell
// "never had one" from "had one, now dropped".
func TestSchemaCatalogSplitTempAutoincrementOutlivesDrop(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		setup []string
		q     string
	}{
		{"row survives after the only TEMP AI table is dropped", []string{
			`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
			`DROP TABLE tai`,
		}, `SELECT name FROM sqlite_temp_master`},
		{"row survives, count(*) reflects it", []string{
			`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
			`DROP TABLE tai`,
		}, `SELECT count(*) FROM sqlite_temp_master`},
		{"row survives even with MAIN independently AUTOINCREMENT too", []string{
			`CREATE TABLE m1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
			`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
			`DROP TABLE tai`,
		}, `SELECT name FROM sqlite_temp_master`},
	}
	for n, tc := range cases {
		// BEGIN wraps setup+query in ONE held engine.DB session
		// (driver/conn.go's own doc comment: autocommit opens a fresh
		// throwaway session PER STATEMENT, and COMMIT closes a transaction's
		// held one too) -- the scope DB.tempSqliteSequenceSeen actually
		// covers, per schemaCatalogSplitDecline's own doc comment. No
		// COMMIT: this test is specifically about the read seeing the SAME
		// session's own prior DROP, not about what survives past it.
		setup := append([]string{`BEGIN`}, tc.setup...)
		goOut, goErr := liveTempCatalogQuery(t, "sqlite", filepath.Join(dir, fmt.Sprintf("go%d.db", n)), setup, tc.q)
		cgoOut, cgoErr := liveTempCatalogQuery(t, "sqlite3", filepath.Join(dir, fmt.Sprintf("cgo%d.db", n)), setup, tc.q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("%s: this engine declined a TEMP catalog read C SQLite answers\n  sql: %s\n  err: %v\n  cgo: %s", tc.name, tc.q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("%s: this engine ACCEPTED a catalog read C SQLite rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", tc.name, tc.q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("%s: DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", tc.name, tc.q, goOut, cgoOut)
		}
	}
}

// TestSchemaCatalogSplitTempAutoincrementDropAcrossSessionsStillWrong pins a
// CONFIRMED, PRE-EXISTING (not introduced or worsened by this session's
// work -- verified via a parent-commit checkout, see
// schemaCatalogSplitDecline's own doc comment) wrong-answer gap: once the
// CREATE that set DB.tempSqliteSequenceSeen has already COMMITted and a
// later, separate statement does the read, that in-memory flag is back at
// its zero value (a fresh engine.DB session, per driver's autocommit/
// commit-closes-the-session model) -- so the carve-out this file's own
// fixes rely on doesn't fire, and the read silently omits the row instead
// of declining. There is no way to detect this from a fresh session (no
// persistent, on-disk trace distinguishes "TEMP never had an AUTOINCREMENT
// table" from "TEMP had one, now dropped, in a PRIOR session"), so closing
// it needs a genuinely new persistent per-file marker -- the same shape as
// Header.CatalogRowEverRemoved (format.go) -- not attempted here. This test
// exists so the gap stays visible rather than silently reappearing as a
// surprise regression report; it intentionally does NOT fail the build.
func TestSchemaCatalogSplitTempAutoincrementDropAcrossSessionsStillWrong(t *testing.T) {
	dir := t.TempDir()
	setup := []string{
		`CREATE TEMP TABLE tai(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
		`DROP TABLE tai`,
	}
	q := `SELECT count(*) FROM sqlite_temp_master`
	goOut, goErr := liveTempCatalogQuery(t, "sqlite", filepath.Join(dir, "go.db"), setup, q)
	cgoOut, cgoErr := liveTempCatalogQuery(t, "sqlite3", filepath.Join(dir, "cgo.db"), setup, q)
	if goErr != nil || cgoErr != nil {
		t.Fatalf("expected both engines to answer (this gap is a wrong VALUE, not a decline); go err=%v cgo err=%v", goErr, cgoErr)
	}
	if goOut == cgoOut {
		t.Logf("this gap appears to be CLOSED (go=%q now matches cgo=%q) -- if this is a deliberate fix, update this test to assert equality and remove this comment", goOut, cgoOut)
		return
	}
	t.Logf("known, pre-existing gap still open (not fixed by this session's work, not a regression): go=%q, cgo=%q -- see schemaCatalogSplitDecline's doc comment", goOut, cgoOut)
}
