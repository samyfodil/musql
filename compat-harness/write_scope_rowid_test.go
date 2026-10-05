package compat

// Write statements cannot reference rowid on WITHOUT ROWID tables.

import "testing"

func TestWriteScopeCannotNameRowidOnWithoutRowid(t *testing.T) {
	differ(t, "write against a WITHOUT ROWID table cannot name rowid", []string{
		`CREATE TABLE wr(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`INSERT INTO wr VALUES('p',1),('q',2)`,
		`UPDATE wr SET b=99 WHERE rowid=1`,
		`DELETE FROM wr WHERE rowid=1`,
		`UPDATE wr SET b=98 WHERE wr.rowid=1`,
		`UPDATE wr SET b=97 WHERE _rowid_=1`,
		`UPDATE wr SET b=96 WHERE oid=1`,
		`SELECT a,b FROM wr ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	// An ordinary WITHOUT ROWID write is untouched by the same rule, and so is
	// a rowid reference against an ORDINARY table beside it.
	differ(t, "ordinary WITHOUT ROWID and rowid-table writes still work", []string{
		`CREATE TABLE wr(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`CREATE TABLE rt(a,b)`,
		`INSERT INTO wr VALUES('p',1),('q',2)`,
		`INSERT INTO rt VALUES(1,10),(2,20)`,
		`UPDATE wr SET b=b+10 WHERE a='p'`,
		`DELETE FROM wr WHERE a='q'`,
		`UPDATE rt SET b=b+1 WHERE rowid=1`,
		`DELETE FROM rt WHERE _rowid_=2`,
		`SELECT a,b FROM wr ORDER BY a`,
		`SELECT a,b FROM rt ORDER BY a`,
	})
}
