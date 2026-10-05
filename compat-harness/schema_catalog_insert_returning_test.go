package compat

import "testing"

// TestCatalogInsertDefaultValuesReturning pins a direct catalog INSERT's two
// shapes from returning1.test 21.0-21.1: "DEFAULT VALUES" writes a row of
// NULLs, and RETURNING reports that row -- reachable by the target's own
// spelling and by every other name of the catalog (resolve.c:528-532). The
// TEMP catalog takes the same write into its own file.
func TestCatalogInsertDefaultValuesReturning(t *testing.T) {
	differ(t, "catalog INSERT DEFAULT VALUES RETURNING", []string{
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_schema DEFAULT VALUES RETURNING sqlite_schema.name",
		"SELECT count(*) FROM sqlite_schema",
		"SELECT type, name, rootpage, sql FROM sqlite_schema",
	})
	differ(t, "temp catalog INSERT DEFAULT VALUES RETURNING", []string{
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_temp_schema DEFAULT VALUES RETURNING sqlite_temp_schema.name",
		"SELECT count(*) FROM sqlite_temp_schema",
		"SELECT count(*) FROM sqlite_schema",
		"SELECT type, name FROM sqlite_temp_schema",
	})
	differ(t, "catalog INSERT RETURNING over given columns", []string{
		"CREATE TABLE keep(x)",
		"PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_schema(type,name,tbl_name,rootpage,sql) VALUES('table','zz','zz',0,'CREATE VIRTUAL TABLE zz USING nosuchmodule(a)') RETURNING name, sqlite_master.type, rootpage, sql IS NULL",
		"SELECT type, name FROM sqlite_schema ORDER BY name",
	})
}
