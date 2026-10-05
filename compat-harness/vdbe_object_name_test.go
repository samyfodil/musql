// Tests SQLite DDL object-name rules: reserved prefix "sqlite_" and shared
// namespace for tables/views/indexes vs separate namespace for triggers
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// objectNameSetup initializes each namespace with one object
var objectNameSetup = []string{
	`CREATE TABLE t1(a,b)`,
	`CREATE TABLE t2(a,b)`,
	`CREATE INDEX i1 ON t1(a)`,
	`CREATE VIEW v1 AS SELECT * FROM t1`,
	`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT 1; END`,
}

// objectNameCases are tested for parity between engines
var objectNameCases = []string{
	// Shared namespace for tables, views and indexes
	`CREATE INDEX t1 ON t2(a)`,    // -> there is already a table named t1
	`CREATE INDEX v1 ON t2(a)`,    // a view counts as a table here
	`CREATE INDEX i1 ON t2(a)`,    // -> index i1 already exists
	`CREATE TABLE i1(x)`,          // -> there is already an index named i1
	`CREATE TABLE v1(x)`,          // -> view v1 already exists
	`CREATE VIEW t1 AS SELECT 1`,  // -> table t1 already exists
	`CREATE VIEW i1 AS SELECT 1`,  // -> there is already an index named i1
	`ALTER TABLE t2 RENAME TO t1`, // -> there is already another table or index with this name
	// IF NOT EXISTS suppresses same-kind collisions only
	`CREATE TABLE IF NOT EXISTS i1(x)`,
	`CREATE INDEX IF NOT EXISTS t1 ON t2(a)`,
	// Triggers have separate namespace from tables/views/indexes
	`CREATE TRIGGER i1 AFTER INSERT ON t1 BEGIN SELECT 1; END`,

	// Reserved "sqlite_" prefix rules
	`CREATE TABLE sqlite_x(a)`,
	`CREATE INDEX sqlite_i ON t1(a)`,
	`CREATE VIEW sqlite_v AS SELECT 1`,
	`CREATE TRIGGER sqlite_tr AFTER INSERT ON t1 BEGIN SELECT 1; END`,
	`CREATE TABLE sqlite_ctas AS SELECT 1 AS z`,
	`ALTER TABLE t2 RENAME TO sqlite_zzz`,
	`CREATE TABLE SQLITE_UPPER(a)`,            // case-insensitive
	`CREATE TABLE "sqlite_quoted"(a)`,         // quoting does not exempt
	`CREATE TABLE IF NOT EXISTS sqlite_x2(a)`, // IF NOT EXISTS does not exempt
	// Prefix only is reserved
	`CREATE TABLE sqlitex(a)`,
	`CREATE TABLE t3 AS SELECT 1 AS z`,
}

func TestObjectNameRulesParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	for _, s := range objectNameSetup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %s: %v", s, err)
		}
	}

	for _, s := range objectNameCases {
		eErr := edb.Exec(s)
		_, cErr := cdb.Exec(s)
		switch {
		case (eErr == nil) != (cErr == nil):
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
		case eErr != nil:
			got := strings.TrimPrefix(eErr.Error(), "engine: ")
			if got != cErr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", s, got, cErr.Error())
			}
		}
	}
}

// TestReservedNameAllowsInternalStat1 guards the ONE bypass: ANALYZE creates
// sqlite_stat1 through this same write path, exactly as C SQLite creates
// it internally with db->init.busy set. The reserved-name rule must not
// break that.
func TestReservedNameAllowsInternalStat1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analyze.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX ti ON t(a)`,
		`INSERT INTO t VALUES(1,2),(3,4)`,
		`ANALYZE`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	var n int
	if err := cdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='sqlite_stat1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("sqlite_stat1 rows in schema: %d, want 1", n)
	}
}
