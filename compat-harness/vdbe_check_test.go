// Tests CHECK constraint enforcement. A CHECK fails on a definite false but
// passes when true or NULL.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

func TestCheckConstraintsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					testCheckScenario(t, pageSize)
				})
			}
		})
	}
}

func testCheckScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("check_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)

	script := []string{
		// column-level + table-level + named CHECK on one table
		`CREATE TABLE t (a INTEGER CHECK(a > 0), b INTEGER, CONSTRAINT bpos CHECK(b >= a))`,
		`INSERT INTO t VALUES (1, 5)`,            // passes both
		`INSERT INTO t VALUES (0, 5)`,            // column CHECK a>0 fails
		`INSERT INTO t VALUES (3, 1)`,            // named table CHECK b>=a fails
		`INSERT INTO t VALUES (2, NULL)`,         // b>=a is NULL -> CHECK PASSES
		`UPDATE t SET a = -1 WHERE a = 1`,        // update violates a>0
		`UPDATE t SET b = 10 WHERE a = 1`,        // update passes
		`INSERT OR IGNORE INTO t VALUES (-9, 9)`, // failing row skipped, no error
		`INSERT OR IGNORE INTO t VALUES (7, 8)`,  // passes
		// second table: CHECK with a scalar function + string
		`CREATE TABLE u (s TEXT CHECK(length(s) <= 3))`,
		`INSERT INTO u VALUES ('ok')`,      // passes
		`INSERT INTO u VALUES ('toolong')`, // length>3 fails
		`INSERT INTO u VALUES (NULL)`,      // length(NULL) IS NULL -> CHECK passes
	}
	for _, stmt := range script {
		execPlainBoth(t, db, sdb, stmt)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen from the on-disk file and confirm CHECK still enforces on a
	// table recovered from its stored CREATE TABLE sql (not one built fresh).
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if err := db2.Exec(`INSERT INTO t VALUES (100, 200)`); err != nil {
		t.Fatalf("post-reopen valid insert rejected: %v", err)
	}
	if err := db2.Exec(`INSERT INTO t VALUES (-5, 1)`); err == nil {
		t.Fatalf("post-reopen CHECK not enforced: a=-5 was accepted")
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close2: %v", err)
	}

	// integrity_check against C SQLite.
	vdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open final: %v", err)
	}
	defer vdb.Close()
	var ic string
	if err := vdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "ok" {
		t.Fatalf("integrity_check = %q, want ok", ic)
	}
}
