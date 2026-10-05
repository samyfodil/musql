//go:build sqlite_fts5

package compat

import "testing"

// TestCreateVirtualTableStoredTextStartsAtCreate verifies that CREATE VIRTUAL
// TABLE stores the correct SQL text without leading comments in sqlite_schema.
func TestCreateVirtualTableStoredTextStartsAtCreate(t *testing.T) {
	differ(t, "leading comment before CREATE VIRTUAL TABLE", []string{
		"-- lead\nCREATE VIRTUAL TABLE v USING fts5(o)",
		"/* block */ CREATE VIRTUAL TABLE w USING fts5(o)",
		"\n\t  CREATE VIRTUAL TABLE y USING fts5(o)",
		"CREATE VIRTUAL TABLE x USING fts5(o)",
		"SELECT name, sql FROM sqlite_schema WHERE name IN ('v','w','x','y') ORDER BY name",
		"INSERT INTO v(o) VALUES('a b c')",
		"SELECT rowid FROM v WHERE v MATCH 'b'",
	})
	differ(t, "leading comment and a schema qualifier", []string{
		"-- lead\nCREATE VIRTUAL TABLE main.q USING fts5(o)",
		"SELECT name, sql FROM sqlite_schema WHERE name='q'",
	})
}
