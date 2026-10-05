// This file gates which statements clear the defer_foreign_keys flag when
// executed outside a transaction. The flag is cleared by statements that read
// or write database files; certain statements like BEGIN or SELECT 1 keep it.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// pragmaR25DeferFKCase is one statement and whether it leaves the flag on.
type pragmaR25DeferFKCase struct {
	setup []string
	stmt  string
	kept  bool
}

var pragmaR25DeferFKCases = []pragmaR25DeferFKCase{
	// ---- KEPT: the VM never reads or writes a database file (bIsReader==0) ----
	{nil, `BEGIN`, true},
	{nil, `BEGIN DEFERRED`, true},
	{nil, `BEGIN IMMEDIATE`, true},
	{nil, `BEGIN EXCLUSIVE`, true},
	{nil, `SAVEPOINT s1`, true},
	{nil, `SELECT 1`, true},
	{nil, `SELECT abs(-1)`, true},
	{nil, `VALUES(1)`, true},
	{nil, `SELECT * FROM (SELECT 1)`, true},
	{nil, `PRAGMA defer_foreign_keys=ON`, true},
	{nil, `PRAGMA defer_foreign_keys`, true},
	{nil, `PRAGMA foreign_keys`, true},
	{nil, `PRAGMA locking_mode`, true},
	{nil, `PRAGMA synchronous`, true},
	{nil, `PRAGMA cache_size`, true},
	{nil, `PRAGMA cache_spill`, true},
	{nil, `PRAGMA database_list`, true},
	{nil, `PRAGMA collation_list`, true},
	{nil, `PRAGMA recursive_triggers`, true},
	{nil, `PRAGMA case_sensitive_like=ON`, true},
	{nil, `PRAGMA writable_schema=1`, true},
	{nil, `PRAGMA query_only`, true},
	{nil, `PRAGMA temp_store`, true},
	{nil, `PRAGMA busy_timeout`, true},
	{nil, `PRAGMA auto_vacuum`, true},
	{nil, `PRAGMA encoding`, true},
	{nil, `PRAGMA page_size`, true},
	{nil, `PRAGMA wal_autocheckpoint`, true},
	{nil, `PRAGMA journal_size_limit`, true},
	{nil, `PRAGMA mmap_size`, true},
	{nil, `PRAGMA threads`, true},
	{nil, `PRAGMA shrink_memory`, true},
	{nil, `PRAGMA secure_delete`, true},
	{nil, `PRAGMA analysis_limit`, true},
	{nil, `PRAGMA trusted_schema`, true},
	{nil, `PRAGMA lock_status`, true},
	{nil, `PRAGMA no_such_pragma_at_all`, true},

	// ---- CLEARED: the statement really opened the database ----
	{nil, `PRAGMA journal_mode`, false},
	{nil, `PRAGMA user_version`, false},
	{nil, `PRAGMA max_page_count`, false},
	{nil, `PRAGMA page_count`, false},
	{nil, `PRAGMA freelist_count`, false},
	{nil, `PRAGMA schema_version`, false},
	{nil, `PRAGMA application_id`, false},
	{nil, `PRAGMA data_version`, false},
	{nil, `PRAGMA integrity_check`, false},
	{nil, `PRAGMA quick_check`, false},
	{nil, `PRAGMA table_info(t1)`, false},
	{nil, `PRAGMA table_xinfo(t1)`, false},
	{nil, `PRAGMA table_list`, false},
	{nil, `PRAGMA index_list(plain)`, false},
	{nil, `PRAGMA index_info(pi)`, false},
	{nil, `PRAGMA index_xinfo(pi)`, false},
	{nil, `PRAGMA wal_checkpoint`, false},
	{nil, `PRAGMA incremental_vacuum`, false},
	{nil, `PRAGMA foreign_key_list(t3)`, false},
	{nil, `PRAGMA foreign_key_check`, false},
	{nil, `SELECT * FROM plain`, false},
	{nil, `SELECT * FROM sqlite_master`, false},
	{nil, `INSERT INTO plain VALUES(2)`, false},
	{nil, `UPDATE plain SET a=3`, false},
	{nil, `DELETE FROM plain WHERE a=99`, false},
	{nil, `CREATE TABLE zz(a)`, false},
	{nil, `DROP TABLE IF EXISTS nosuchtable`, false},
	{nil, `ANALYZE`, false},
	{nil, `VACUUM`, false},
	{nil, `ATTACH ':memory:' AS aux`, false},
	{nil, `CREATE TEMP TABLE tt2(a)`, false},
	// A TEMP-database statement CLEARS here, unlike the wal-index table where
	// every one of them is a NO.
	{[]string{`CREATE TEMP TABLE tt(a)`, `INSERT INTO tt VALUES(1)`}, `INSERT INTO tt VALUES(2)`, false},
	{[]string{`CREATE TEMP TABLE tt(a)`, `INSERT INTO tt VALUES(1)`}, `SELECT * FROM tt`, false},
	// Transaction control with nothing to end errors AND clears.
	{nil, `COMMIT`, false},
	{nil, `ROLLBACK`, false},

	// ---- the context-dependent ones, both sides ----
	{nil, `PRAGMA optimize(0)`, true},
	{nil, `PRAGMA optimize`, false},
	// REINDEX with an index present rebuilds it; the setup below always has one.
	{nil, `REINDEX`, false},
	// A statement that fails at PREPARE never got a VM...
	{nil, `SELECT nosuchcolumn FROM plain`, true},
	{nil, `INSERT INTO plain VALUES(1,2)`, true},
	{nil, `not even sql`, true},
	// ...while one that fails at RUN has already cleared it.
	{nil, `INSERT INTO t3 VALUES(9,99)`, false},
}

func TestPragmaR25DeferFKAutocommitTable(t *testing.T) {
	for _, tc := range pragmaR25DeferFKCases {
		tc := tc
		t.Run(tc.stmt, func(t *testing.T) {
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "o.db"))
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			setup := append([]string{
				`PRAGMA foreign_keys=ON`,
				`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
				`CREATE TABLE t3(u INTEGER PRIMARY KEY, v INTEGER REFERENCES t1(x))`,
				`CREATE TABLE plain(a)`,
				`CREATE INDEX pi ON plain(a)`,
				`INSERT INTO t1 VALUES(1),(2),(3)`,
				`INSERT INTO t3 VALUES(3,3)`,
				`INSERT INTO plain VALUES(1)`,
			}, tc.setup...)
			setup = append(setup, `PRAGMA defer_foreign_keys=ON`)
			for _, s := range setup {
				if _, eerr := db.Exec(s); eerr != nil {
					t.Fatalf("setup %q: %v", s, eerr)
				}
			}
			db.Exec(tc.stmt) // its error, if any, is part of the case
			var v int
			if err := db.QueryRow(`PRAGMA defer_foreign_keys`).Scan(&v); err != nil {
				t.Fatalf("getter: %v", err)
			}
			if (v == 1) != tc.kept {
				t.Errorf("%q left defer_foreign_keys=%d, this table says kept=%v -- the oracle's rule moved, re-measure before relying on it", tc.stmt, v, tc.kept)
			}
			db.Exec(`ROLLBACK`)
		})
	}
}

// TestPragmaR25DeferFKAutocommitIsObservable is the other half: what an
// implementation actually buys. The setter in autocommit, then a BEGIN (which
// KEEPS it), then a DELETE that violates an IMMEDIATE foreign key -- which
// SUCCEEDS, and the COMMIT fails instead. That is fkey6.test's own fkey6-1.8
// sequence, the mined statement this bucket is about.
func TestPragmaR25DeferFKAutocommitIsObservable(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t1(x INTEGER PRIMARY KEY)`,
		`CREATE TABLE t3(u INTEGER PRIMARY KEY, v INTEGER REFERENCES t1(x))`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`INSERT INTO t3 VALUES(3,3)`,
		`PRAGMA defer_foreign_keys=ON`,
		`BEGIN`,
	} {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%q: %v", s, eerr)
		}
	}
	if _, eerr := db.Exec(`DELETE FROM t1 WHERE x=3`); eerr != nil {
		t.Fatalf("the DELETE must be DEFERRED past the statement, not refused: %v", eerr)
	}
	var v int
	if err := db.QueryRow(`PRAGMA defer_foreign_keys`).Scan(&v); err != nil {
		t.Fatalf("getter: %v", err)
	}
	if v != 1 {
		t.Fatalf("the flag did not survive the BEGIN: %d", v)
	}
	if _, eerr := db.Exec(`COMMIT`); eerr == nil {
		t.Fatal("the COMMIT must fail the deferred check")
	}
	// ...and a FAILED commit does NOT switch the flag off, unlike a successful
	// one -- sqlite3VdbeHalt clears it only after vdbeCommit succeeds.
	if err := db.QueryRow(`PRAGMA defer_foreign_keys`).Scan(&v); err != nil {
		t.Fatalf("getter: %v", err)
	}
	if v != 1 {
		t.Fatalf("a FAILED commit switched the flag off: %d", v)
	}
	db.Exec(`ROLLBACK`)
}
