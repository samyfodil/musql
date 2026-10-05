package compat

import "testing"

// TestWritableSchemaResetOverACorruptRow verifies that PRAGMA writable_schema=RESET
// enforces schema validation rules and rejects corrupt rows.
func TestWritableSchemaResetOverACorruptRow(t *testing.T) {
	differ(t, "RESET over a row of NULLs", []string{
		"CREATE TABLE keep(x)",
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_schema DEFAULT VALUES",
		"PRAGMA writable_schema=RESET",
		"SELECT type,name FROM sqlite_schema ORDER BY name",
		"PRAGMA writable_schema=ON",
		"SELECT count(*) FROM sqlite_schema",
	})
	differ(t, "RESET over an orphan automatic-index row", []string{
		"CREATE TABLE keep(x)",
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_schema VALUES('index','q','keep',0,NULL)",
		"PRAGMA writable_schema=RESET",
		"SELECT type,name FROM sqlite_schema ORDER BY name",
	})
	differ(t, "RESET over a row of NULLs in the TEMP catalog", []string{
		"CREATE TABLE keep(x)",
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_temp_schema DEFAULT VALUES",
		"PRAGMA writable_schema=RESET",
		"SELECT type,name FROM sqlite_schema ORDER BY name",
	})
	// Flag should be OFF after a successful RESET.
	differ(t, "the flag after a RESET that reloaded", []string{
		"CREATE TABLE keep(x)",
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_schema VALUES('view','vv','vv',0,'CREATE VIEW vv AS SELECT 1')",
		"PRAGMA writable_schema=RESET",
		"PRAGMA writable_schema",
		"INSERT INTO sqlite_schema VALUES('view','v2','v2',0,'CREATE VIEW v2 AS SELECT 2')",
	})
	differ(t, "RESET over a row a CREATE text accounts for", []string{
		"CREATE TABLE keep(x)",
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_schema VALUES('view','vv','vv',0,'CREATE VIEW vv AS SELECT 1')",
		"PRAGMA writable_schema=RESET",
		"SELECT type,name FROM sqlite_schema ORDER BY name",
		"SELECT * FROM vv",
	})
}
