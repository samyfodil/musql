// Verify which statements cause the TEMP database to open.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// r35aTempDBIsOpen reports whether the connection's TEMP database is OPEN.
func r35aTempDBIsOpen(t *testing.T, db *sql.DB) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatalf("database_list: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name string
		var file any
		if serr := rows.Scan(&seq, &name, &file); serr != nil {
			t.Fatalf("scan: %v", serr)
		}
		if name == "temp" {
			return true
		}
	}
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("database_list step: %v", rerr)
	}
	return false
}

func TestR35ATempDatabaseOpeners(t *testing.T) {
	cases := []struct {
		name  string
		open  bool
		stmts []string
	}{
		// sqlite3CodeVerifySchemaAtToplevel for iDb==1 (build.c:5366): a
		// statement whose codegen names the TEMP schema.
		{"create temp table", true, []string{`CREATE TEMP TABLE tt(a)`}},
		{"create temporary table", true, []string{`CREATE TEMPORARY TABLE tt(a)`}},
		{"create temp table then drop", true, []string{`CREATE TEMP TABLE tt(a)`, `DROP TABLE tt`}},
		{"create table temp.q", true, []string{`CREATE TABLE temp.q(a)`}},
		{"select from sqlite_temp_master", true, []string{`SELECT * FROM sqlite_temp_master`}},
		{"select from sqlite_temp_schema", true, []string{`SELECT * FROM sqlite_temp_schema`}},
		{"select count from sqlite_temp_master", true, []string{`SELECT count(*) FROM sqlite_temp_master`}},
		// pragma.c:457, the "temp."-qualified pragma.
		{"pragma temp.page_count", true, []string{`PRAGMA temp.page_count`}},
		{"pragma temp.locking_mode", true, []string{`PRAGMA temp.locking_mode`}},
		// pragma.c:1741, the integrity-check loop over EVERY database. The
		// "main."-qualified spelling skips i==1 before the verify.
		{"pragma integrity_check", true, []string{`CREATE TABLE t(a)`, `PRAGMA integrity_check`}},
		{"pragma quick_check", true, []string{`CREATE TABLE t(a)`, `PRAGMA quick_check`}},
		{"pragma integrity_check(t)", true, []string{`CREATE TABLE t(a)`, `PRAGMA integrity_check(t)`}},
		{"pragma main.integrity_check", false, []string{`CREATE TABLE t(a)`, `PRAGMA main.integrity_check`}},

		// Everything else leaves it shut. The transient temp material a
		// sorter/DISTINCT/GROUP BY uses is an EPHEMERAL table, not the temp
		// DATABASE -- the distinction the whole bit rests on.
		{"nothing", false, nil},
		{"create+insert+select", false, []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `SELECT * FROM t`}},
		{"begin+insert", false, []string{`CREATE TABLE t(a)`, `BEGIN`, `INSERT INTO t VALUES(1)`}},
		{"order by (sorter)", false, []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(2),(1)`, `SELECT * FROM t ORDER BY a`}},
		{"group by", false, []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(2),(1)`, `SELECT a,count(*) FROM t GROUP BY a`}},
		{"distinct", false, []string{`CREATE TABLE t(a)`, `SELECT DISTINCT a FROM t`}},
		{"pragma temp_store getter", false, []string{`PRAGMA temp_store`}},
		{"pragma temp_store=1", false, []string{`PRAGMA temp_store=1`}},
		{"reindex bare", false, []string{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(a)`, `REINDEX`}},
		{"analyze bare", false, []string{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(a)`, `INSERT INTO t VALUES(1)`, `ANALYZE`}},
		{"vacuum", false, []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `VACUUM`}},
		{"pragma table_list", false, []string{`CREATE TABLE t(a)`, `PRAGMA table_list`}},
		{"pragma optimize", false, []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `PRAGMA optimize`}},
		{"pragma foreign_key_check", false, []string{`CREATE TABLE t(a)`, `PRAGMA foreign_key_check`}},
		{"pragma schema_version", false, []string{`PRAGMA schema_version`}},
		{"pragma writable_schema=ON", false, []string{`PRAGMA writable_schema=ON`}},
		{"drop table if exists absent", false, []string{`DROP TABLE IF EXISTS nosuch`}},
		{"cte materialized", false, []string{`WITH x(a) AS (VALUES(1),(2)) SELECT * FROM x`}},
		{"subquery in from", false, []string{`CREATE TABLE t(a)`, `SELECT * FROM (SELECT a FROM t) ORDER BY 1`}},
		{"compound union", false, []string{`CREATE TABLE t(a)`, `SELECT a FROM t UNION SELECT 1`}},
		{"trigger on main", false, []string{`CREATE TABLE t(a)`, `CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`, `INSERT INTO t VALUES(1)`}},
		{"view", false, []string{`CREATE TABLE t(a)`, `CREATE VIEW v AS SELECT a FROM t`, `SELECT * FROM v`}},
		{"attach", false, []string{`ATTACH ':memory:' AS aux1`}},
		{"savepoint", false, []string{`SAVEPOINT one`, `RELEASE one`}},
		{"upsert", false, []string{`CREATE TABLE t(a PRIMARY KEY)`, `INSERT INTO t VALUES(1) ON CONFLICT DO NOTHING`}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := sql.Open("sqlite3", filepath.Join(dir, "x.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			for _, s := range tc.stmts {
				if _, eerr := db.Exec(s); eerr != nil {
					t.Logf("  (stmt %q: %v)", s, eerr)
				}
			}
			if got := r35aTempDBIsOpen(t, db); got != tc.open {
				t.Errorf("temp database open=%v, want %v -- the opener table engine/temp_schema.go's r35aStatementOpensTempDatabase ports is stale; re-read sqlite3OpenTempDatabase's call sites (build.c:5366, pragma.c:457, pragma.c:1741) and update BOTH", got, tc.open)
			}
		})
	}
}

// TestR35ATempStoreInvalidatesTempSchema pins the SECOND half of
// invalidateTempStorage -- the one this engine declines rather than reproduces.
// TestR35ATempStoreDiscardsTempObjects is the gate that acts on it; this is the
// oracle measurement it rests on, kept separate so a change in the oracle's
// behaviour is reported here rather than as a confusing engine failure.
func TestR35ATempStoreInvalidatesTempSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range []string{`CREATE TEMP TABLE tt(a)`, `INSERT INTO tt VALUES(1)`, `PRAGMA temp_store=2`} {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}
	var n int
	if qerr := db.QueryRow(`SELECT count(*) FROM tt`).Scan(&n); qerr == nil {
		t.Errorf("SELECT count(*) FROM tt answered %d after PRAGMA temp_store=2; the oracle is expected to have closed the temp database out from under it (pragma.c's invalidateTempStorage)", n)
	}
	// ...and the temp database is OPEN again immediately, because the very next
	// statement that names it re-opens an empty one.
	var m int
	if qerr := db.QueryRow(`SELECT count(*) FROM sqlite_temp_master`).Scan(&m); qerr != nil || m != 0 {
		t.Errorf("sqlite_temp_master after temp_store=2: rows=%d err=%v, want 0 rows and no error", m, qerr)
	}
}
