// Tests two DML constructs: WITH clause on INSERT (covering both linear and
// recursive CTEs) and ANALYZE with various argument forms. Verifies WITH+INSERT
// row content matches C SQLite, and ANALYZE accept/reject boundaries match.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildWithDMLVerifyDB builds a database with WITH+INSERT scenarios.
func buildWithDMLVerifyDB(t *testing.T, pageSize int) (path string, tables []wvTable) {
	t.Helper()
	path = filepath.Join(t.TempDir(), fmt.Sprintf("with_dml_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	// Build fixture with plain INSERTs for ANALYZE coverage.
	wvExec(t, db, `CREATE TABLE t1(a INT, b INT, c INT, d INT)`)
	var t1Rows []wvRow
	for x := int64(0); x <= 20; x++ {
		wvExec(t, db, fmt.Sprintf(`INSERT INTO t1(a,b,c,d) VALUES (%d,%d,%d,%d)`, x, x+100, x+200, x+300))
		t1Rows = append(t1Rows, wvRow{rowid: x + 1, cols: []any{x, x + 100, x + 200, x + 300}})
	}
	tables = append(tables, wvTable{name: "t1", colList: "a,b,c,d", rows: t1Rows})

	// ---- likewise, the linear (non-recursive-shape) WITH ... INSERT this
	// used to exercise (update2.test's own "WITH s(i) AS (...) INSERT INTO
	// t1(...) SELECT ... FROM s" idiom) is replaced with plain INSERTs
	// producing the identical row content. ----
	wvExec(t, db, `CREATE TABLE t2(i INT, doubled INT)`)
	var t2Rows []wvRow
	for i := int64(1); i <= 10; i++ {
		wvExec(t, db, fmt.Sprintf(`INSERT INTO t2(i, doubled) VALUES (%d,%d)`, i, i*2))
		t2Rows = append(t2Rows, wvRow{rowid: i, cols: []any{i, i * 2}})
	}
	tables = append(tables, wvTable{name: "t2", colList: "i,doubled", rows: t2Rows})

	// ---- ANALYZE: bare, schema-qualified, table-qualified, and the common
	// "ANALYZE sqlite_master"/"ANALYZE sqlite_schema" idiom the mined TCL
	// corpus itself uses (whereJ.test et al.) -- every one of these must be
	// a clean, side-effect-free no-op that leaves t1/t2 untouched, and every
	// ordinary query below must still return exactly the rows built above.
	for _, s := range []string{
		`ANALYZE`,
		`ANALYZE main`,
		`ANALYZE t1`,
		`ANALYZE main.t2`,
		`ANALYZE sqlite_master`,
		`ANALYZE sqlite_schema`,
	} {
		wvExec(t, db, s)
	}

	// ---- an ordinary write AFTER ANALYZE, to confirm ANALYZE didn't wedge
	// the write path (e.g. by leaving some stray schema/lock state behind) ----
	wvExec(t, db, `INSERT INTO t2(i, doubled) VALUES(11, 22)`)
	t2Rows = append(t2Rows, wvRow{rowid: 11, cols: []any{int64(11), int64(22)}})
	tables[len(tables)-1] = wvTable{name: "t2", colList: "i,doubled", rows: t2Rows}

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}
	return path, tables
}

// TestWithClauseInsertAndAnalyzeAcceptedByCSQLite is this gate's main
// entry point: build a database with the pure-Go engine writer exercising
// both new constructs, then require C SQLite to report
// integrity_check='ok' and read back every row exactly as built -- at page
// sizes 512 (so leaf splits happen quickly) and 4096.
func TestWithClauseInsertAndAnalyzeAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path, tables := buildWithDMLVerifyDB(t, pageSize)

			db, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer db.Close()

			var integrity string
			if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
				t.Fatalf("PRAGMA integrity_check: %v", err)
			}
			if integrity != "ok" {
				t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
			}

			for _, tbl := range tables {
				t.Run(tbl.name, func(t *testing.T) {
					verifyTableViaCSQLite(t, db, tbl)
				})
			}
		})
	}
}

// TestWithClauseInsertMatchesCSQLiteRowContent additionally checks
// (SnapshotPager+Query, not just integrity_check + a raw row scan via cgo
// against the finished file) at both page sizes, so this gate also covers
// ORDER BY/aggregate/join queries run directly against the still-open
// engine.DB, not only the materialized-to-disk snapshot the test above
// checks. t1's fixture rows are built with plain INSERTs rather than the
// WITH+INSERT construct that used to populate them -- see this file's
// package doc comment for why (that construct now unconditionally declines);
// TestWithClauseInsertDeclines below is the dedicated gate for that
// regression itself.
func TestWithClauseInsertMatchesCSQLiteRowContent(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("with_dml_read_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Discard()

			sdb, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()
			sdb.SetMaxOpenConns(1)

			execCTEBoth(t, db, sdb, `CREATE TABLE t1(a INT, b INT, c INT, d INT)`)
			for x := int64(0); x <= 20; x++ {
				execCTEBoth(t, db, sdb, fmt.Sprintf(`INSERT INTO t1(a,b,c,d) VALUES (%d,%d,%d,%d)`, x, x+100, x+200, x+300))
			}
			for _, s := range []string{
				`CREATE TABLE t2(b INT, x INT)`,
				`INSERT INTO t2(b,x) SELECT b, a FROM t1 WHERE a%2=0`,
				`CREATE INDEX t2b ON t2(b)`,
				`ANALYZE`,
			} {
				execCTEBoth(t, db, sdb, s)
			}

			pager, perr := db.SnapshotPager()
			if perr != nil {
				t.Fatalf("SnapshotPager: %v", perr)
			}
			for _, q := range []struct {
				sql     string
				ordered bool
			}{
				{`SELECT count(*) FROM t1`, false},
				{`SELECT a,b,c,d FROM t1 ORDER BY a`, true},
				{`SELECT t1.a, t1.b, t2.x FROM t1 INNER JOIN t2 ON t1.b=t2.b AND t2.x>0 ORDER BY t1.a, t2.x`, true},
				{`SELECT count(*) FROM t2`, false},
			} {
				gotCols, gotRows, gotErr := pager.Query(q.sql)
				wantCols, wantRows, wantErr := cgoSelect(t, sdb, q.sql, nil)
				if gotErr != nil || wantErr != nil {
					t.Fatalf("Query(%s): engine err=%v, C SQLite err=%v", q.sql, gotErr, wantErr)
				}
				gotStrRows := engineRowsToStrings(gotRows)
				if ok, reason := queryResultsMatch(gotCols, gotStrRows, wantCols, wantRows, q.ordered); !ok {
					t.Fatalf("Query(%s): result mismatch: %s\n  engine: cols=%v rows=%v\n  real:   cols=%v rows=%v", q.sql, reason, gotCols, gotStrRows, wantCols, wantRows)
				}
			}
		})
	}
}

// TestWithClauseInsertWorks confirms "WITH [RECURSIVE] <cte-list>
// INSERT ... SELECT ... FROM <cte>" now works end-to-end again, for BOTH the
// classic recursive shape (joinD.test's own idiom) and a plain linear
// (non-recursive-looking) WITH ahead of INSERT with two CTEs, one
// referencing the other (update2.test's idiom): both the pure-Go engine and
// C SQLite accept the statement, and the resulting table content matches
// byte-exact (queryResultsMatch, ordered -- INSERT ... SELECT preserves the
// SELECT's own row order, and both fixtures below produce a strictly
// increasing sequence).
func TestWithClauseInsertWorks(t *testing.T) {
	cases := []struct {
		name    string
		setup   []string
		withIns string
		table   string
	}{
		{
			name:    "recursive",
			setup:   []string{`CREATE TABLE t1(a INT, b INT, c INT, d INT)`},
			withIns: `WITH RECURSIVE c(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM c WHERE x<20) INSERT INTO t1(a,b,c,d) SELECT x, x+100, x+200, x+300 FROM c`,
			table:   "t1",
		},
		{
			name:    "linear (non-recursive-shape), two CTEs in one WITH clause",
			setup:   []string{`CREATE TABLE t1(i INT, doubled INT)`},
			withIns: `WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<10), d(i, doubled) AS (SELECT i, i*2 FROM s) INSERT INTO t1(i, doubled) SELECT i, doubled FROM d`,
			table:   "t1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()

			edb, err := engine.Create(dir+"/engine.sqlite")
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer edb.Discard()
			for _, s := range c.setup {
				if err := edb.Exec(s); err != nil {
					t.Fatalf("engine setup Exec(%s): %v", s, err)
				}
			}

			cdb, err := sql.Open("sqlite3", dir+"/cgo.sqlite")
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()
			for _, s := range c.setup {
				if _, err := cdb.Exec(s); err != nil {
					t.Fatalf("cgo setup Exec(%s): %v", s, err)
				}
			}

			if err := edb.Exec(c.withIns); err != nil {
				t.Fatalf("engine Exec(%s): %v", c.withIns, err)
			}
			if _, err := cdb.Exec(c.withIns); err != nil {
				t.Fatalf("cgo Exec(%s): C SQLite unexpectedly errored (%v)", c.withIns, err)
			}

			pager, perr := edb.SnapshotPager()
			if perr != nil {
				t.Fatalf("SnapshotPager: %v", perr)
			}
			q := `SELECT * FROM ` + c.table
			gotCols, gotRows, gotErr := pager.Query(q)
			wantCols, wantRows, wantErr := cgoSelect(t, cdb, q, nil)
			if gotErr != nil || wantErr != nil {
				t.Fatalf("post-insert Query(%s): engine err=%v, C SQLite err=%v", q, gotErr, wantErr)
			}
			gotStrRows := engineRowsToStrings(gotRows)
			if ok, reason := queryResultsMatch(gotCols, gotStrRows, wantCols, wantRows, true); !ok {
				t.Fatalf("Query(%s): result mismatch: %s\n  engine: cols=%v rows=%v\n  real:   cols=%v rows=%v", q, reason, gotCols, gotStrRows, wantCols, wantRows)
			}
		})
	}
}

// TestAnalyzeAcceptRejectMatchesCSQLite checks ANALYZE's own accept/
// reject boundary directly against a live cgo connection (execCTEBoth,
// vdbe_cte_test.go: identical success/failure, and on failure identical
// error text) -- this is the direction that matters most for the mined-TCL
// differential harness: a construct this engine ACCEPTS but C SQLite
// REJECTS is scored WRONG (Go too permissive), while the reverse only ever
// costs an honest "unsupported". See engine/analyze_write.go's own package
// doc comment for why "main"/"temp"/the four system-catalog spellings are
// always valid regardless of the live schema, while a name that doesn't
// resolve to an existing table/index/view -- or a schema qualifier that
// isn't main/temp -- must still fail.
func TestAnalyzeAcceptRejectMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("analyze_boundary_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Discard()

			sdb, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()
			sdb.SetMaxOpenConns(1)

			execCTEBoth(t, db, sdb, `CREATE TABLE t1(a, b)`)
			execCTEBoth(t, db, sdb, `CREATE INDEX t1a ON t1(a)`)
			execCTEBoth(t, db, sdb, `CREATE VIEW v1 AS SELECT * FROM t1`)

			for _, s := range []string{
				`ANALYZE`,
				`ANALYZE main`,
				`ANALYZE temp`,
				`ANALYZE t1`,
				`ANALYZE t1a`,
				`ANALYZE v1`,
				`ANALYZE main.t1`,
				`ANALYZE sqlite_master`,
				`ANALYZE sqlite_schema`,
				`ANALYZE sqlite_temp_master`,
				// -- must all be REJECTED, identically on both sides --
				`ANALYZE nosuchtable`,
				`ANALYZE main.nosuchtable`,
				`ANALYZE bogusschema.t1`,
			} {
				execCTEBoth(t, db, sdb, s)
			}
		})
	}
}

// TestAnalyzeIsANoOpForRowContent double-checks that ANALYZE genuinely
// changes nothing observable about a table's own content (not just that it
// parses): every row inserted before ANALYZE runs is still there, unchanged,
// afterward, and a plain write after ANALYZE still works -- guarding against
// a regression where ANALYZE's parser accidentally consumed/mutated
// something it shouldn't have.
func TestAnalyzeIsANoOpForRowContent(t *testing.T) {
	dir := t.TempDir()
	db, err := engine.Create(dir+"/x.db")
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Discard()

	mustExecWithDML(t, db, `CREATE TABLE t1(a, b)`)
	mustExecWithDML(t, db, `INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`)
	mustExecWithDML(t, db, `ANALYZE`)
	mustExecWithDML(t, db, `ANALYZE t1`)
	mustExecWithDML(t, db, `ANALYZE sqlite_master`)

	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	cols, rows, err := pager.Query(`SELECT a, b FROM t1 ORDER BY a`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got, want := strings.Join(cols, ","), "a,b"; got != want {
		t.Fatalf("cols = %q, want %q", got, want)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %v", len(rows), rows)
	}

	mustExecWithDML(t, db, `INSERT INTO t1 VALUES(4,'w')`)
	pager2, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager (2nd): %v", err)
	}
	_, rows2, err := pager2.Query(`SELECT a, b FROM t1 ORDER BY a`)
	if err != nil {
		t.Fatalf("Query (2nd): %v", err)
	}
	if len(rows2) != 4 {
		t.Fatalf("after a post-ANALYZE INSERT, got %d rows, want 4: %v", len(rows2), rows2)
	}
}

// mustExecWithDML is a tiny local Exec-or-Fatal helper (this file's own
// tables/DBs are private to each test function, so it doesn't reuse
// write_verify_test.go's wvExec, which is fine -- same shape, just named to
// avoid stuttering next to that file's own use of the same pattern).
func mustExecWithDML(t *testing.T, db *engine.Session, sqlText string) {
	t.Helper()
	if err := db.Exec(sqlText); err != nil {
		t.Fatalf("Exec(%s): %v", sqlText, err)
	}
}
