//go:build sqlite_fts5

// FTS5 vtable tests for PRAGMA legacy_alter_table.
package compat

import "testing"

// TestPragmaR25LegacyAlterVtabFts5Rename tests FTS5 vtable rename with legacy mode.
func TestPragmaR25LegacyAlterVtabFts5Rename(t *testing.T) {
	flLockstep(t, "legacy fts5 vtab rename, no referencing objects",
		[]string{
			`PRAGMA legacy_alter_table=ON`,
			`CREATE VIRTUAL TABLE fff USING fts5(x, y, z)`,
			`BEGIN`,
			`INSERT INTO fff VALUES('a', 'b', 'c')`,
			`ALTER TABLE fff RENAME TO ggg`,
			`COMMIT`,
		},
		`SELECT * FROM ggg`,
		`SELECT name FROM sqlite_master ORDER BY name`,
	)
}

// TestPragmaR25LegacyAlterVtabTriggerBodyStaysStale tests trigger body updates
// when legacy mode renames an FTS5 vtable.
func TestPragmaR25LegacyAlterVtabTriggerBodyStaysStale(t *testing.T) {
	flLockstep(t, "legacy fts5 vtab rename, trigger body references old name",
		[]string{
			`PRAGMA legacy_alter_table=ON`,
			`CREATE VIRTUAL TABLE fff USING fts5(x, y, z)`,
			`CREATE TRIGGER trg1 AFTER INSERT ON fff BEGIN INSERT INTO fff(x) VALUES('body-ref'); END`,
			`ALTER TABLE fff RENAME TO ggg`,
			`INSERT INTO ggg(x) VALUES('outer')`,
		},
		`SELECT sql FROM sqlite_master WHERE name='trg1'`,
		`SELECT x FROM ggg`,
	)
}
