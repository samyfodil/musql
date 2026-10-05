// This file tests PRAGMA secure_delete per-database and per-connection behavior,
// verifying qualified setters affect only one database while bare setters
// affect all databases and later-attached ones.
package compat

import "testing"

// TestSecureDeletePerDatabase tests qualified and bare setters.
func TestSecureDeletePerDatabase(t *testing.T) {
	differ(t, "secure_delete per database", []string{
		`ATTACH ':memory:' AS db2`,
		`PRAGMA main.secure_delete`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA secure_delete`,
		`PRAGMA main.secure_delete=ON`,
		`PRAGMA main.secure_delete`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA secure_delete`,
		`PRAGMA secure_delete=ON`,
		`PRAGMA main.secure_delete`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA db2.secure_delete=OFF`,
		`PRAGMA main.secure_delete`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA secure_delete`,
		`PRAGMA db2.secure_delete=fast`,
		`PRAGMA db2.secure_delete`,
		`PRAGMA main.secure_delete`,
	})
}

// TestSecureDeleteInheritedByLaterAttach tests that later-attached databases
// inherit the bare setter's value.
func TestSecureDeleteInheritedByLaterAttach(t *testing.T) {
	differ(t, "secure_delete inherited by a later attach", []string{
		`ATTACH ':memory:' AS one`,
		`PRAGMA secure_delete=ON`,
		`ATTACH ':memory:' AS two`,
		`PRAGMA one.secure_delete`,
		`PRAGMA two.secure_delete`,
		`PRAGMA main.secure_delete`,
	})
	differ(t, "secure_delete main-qualified", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`PRAGMA main.secure_delete=ON`,
		`PRAGMA main.secure_delete`,
		`DELETE FROM t WHERE a=2`,
		`PRAGMA main.secure_delete=OFF`,
		`PRAGMA main.secure_delete`,
		`SELECT a FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
		`PRAGMA page_count`,
		`PRAGMA freelist_count`,
	})
}

// TestSecureDeleteAcceptsEveryValueSpelling tests various value spellings.
func TestSecureDeleteAcceptsEveryValueSpelling(t *testing.T) {
	differ(t, "secure_delete value spellings", []string{
		`PRAGMA secure_delete=2`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=3`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=-1`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=xyzzy`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=0x10`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=256`, `PRAGMA secure_delete`,
	})
	differ(t, "secure_delete accepted spellings", []string{
		`PRAGMA secure_delete=on`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=off`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=1`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=0`, `PRAGMA secure_delete`,
		`PRAGMA secure_delete=fast`, `PRAGMA secure_delete`,
		`PRAGMA main.secure_delete=true`, `PRAGMA main.secure_delete`,
	})
}
