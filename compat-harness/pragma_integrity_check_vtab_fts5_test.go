//go:build sqlite_fts5

// Pragma_integrity_check_vtab_fts5_test verifies PRAGMA integrity_check with
// FTS5 virtual tables.
package compat

import "testing"

func TestPragmaIntegrityCheckExecPathVtabFts5(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)`,
		`INSERT INTO t1(b) VALUES('the quick fox jumps over the lazy brown dog')`,
		`CREATE VIRTUAL TABLE t2 USING fts5(content="t1", b)`,
		`INSERT INTO t2(t2) VALUES('rebuild')`,
	}
	for _, q := range []string{
		"PRAGMA integrity_check(t2)",
		"PRAGMA quick_check(t2)",
	} {
		pragmaIntegrityCheckVtabCase(t, setup, q)
	}
}
