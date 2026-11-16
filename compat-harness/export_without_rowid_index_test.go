package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestExportWithoutRowidIndexKeepsTheKeyOrder: an index on a WITHOUT ROWID
// table carries the PRIMARY KEY's columns after its own, in the PRIMARY
// KEY's sort order (sqlite3CreateIndex). The export sorted them ascending, so
// entries with equal keys sat in an order C's lookups do not search: C's
// integrity_check reported the rows missing from the index, and a write that
// had to find one could not.
func TestExportWithoutRowidIndexKeepsTheKeyOrder(t *testing.T) {
	for _, pk := range []string{"n DESC, k", "n, k DESC", "n DESC, k COLLATE NOCASE DESC"} {
		t.Run(pk, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "w.musq")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			stmts := []string{
				fmt.Sprintf(`CREATE TABLE w(k TEXT, n INT, v, PRIMARY KEY(%s)) WITHOUT ROWID`, pk),
				`INSERT INTO w SELECT 'k'||value, value%13, value%7 FROM generate_series(1, 2000)`,
				`CREATE INDEX w_v ON w(v)`,
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
			}
			db.Close()
			c, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			check := func(when string) {
				t.Helper()
				var got string
				if err := c.QueryRow(`PRAGMA integrity_check`).Scan(&got); err != nil || got != "ok" {
					t.Fatalf("C %s: %q %v", when, got, err)
				}
			}
			check("over the export")
			if _, err := c.Exec(`DELETE FROM w WHERE v = 3 AND n < 6`); err != nil {
				t.Fatalf("C deleting through the index: %v", err)
			}
			check("after its delete")
		})
	}
}
