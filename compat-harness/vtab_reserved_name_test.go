// Virtual table names must not use the "sqlite_" prefix, which is reserved
// for internal schema objects. fts3/fts4 is used rather than fts5 to run in the default build.
package compat

import "testing"

func TestVtabReservedObjectName(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"reserved name is rejected", []string{
			// First CREATE avoids driver session state issues.
			`CREATE TABLE keep(x)`,
			`CREATE VIRTUAL TABLE sqlite_stat1 USING fts4(a)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		{"the check is case-insensitive and survives IF NOT EXISTS", []string{
			`CREATE TABLE keep(x)`,
			`CREATE VIRTUAL TABLE SQLITE_Reserved USING fts4(a)`,
			`CREATE VIRTUAL TABLE IF NOT EXISTS sqlite_reserved2 USING fts3(a)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		{"a quoted reserved name is still reserved", []string{
			`CREATE TABLE keep(x)`,
			`CREATE VIRTUAL TABLE "sqlite_quoted" USING fts4(a)`,
			`SELECT count(*) FROM sqlite_master`,
		}},
		{"only the prefix is reserved", []string{
			`CREATE VIRTUAL TABLE sqlitex USING fts4(a)`,
			`INSERT INTO sqlitex VALUES('hello world')`,
			`SELECT docid, a FROM sqlitex`,
			`SELECT type, name FROM sqlite_master WHERE name='sqlitex'`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}
