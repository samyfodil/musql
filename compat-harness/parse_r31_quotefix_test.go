// This file tests renameFixQuotes: ALTER TABLE RENAME's quoting rules.
package compat

import "testing"

// TestParseR31Quotefix tests renameFixQuotes behavior in ALTER TABLE RENAME.
func TestParseR31Quotefix(t *testing.T) {
	differ(t, "r31-quotefix-view", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT "a", "b", "notacolumn!", "c" FROM t1`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-quotefix-view-embedded-quotes", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT "a", "b", "not'a'column!", "c" FROM t1`,
		`CREATE VIEW v2 AS SELECT "a", "b", "not""a""column!", "c" FROM t1`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-quotefix-view-groupby", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT "val", count("b") FROM t1 GROUP BY "abc"`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-quotefix-check-and-generated", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE TABLE xyz(a CHECK (a!="str"), b AS (a||"str"))`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	// alterqf.test 6's shape, but note it is NOT a quotefix case end to end:
	// through a REAL rename the oracle rejects it outright, "error in index
	// i1: no such column: val". A bare "val" in the KEY LIST is an index
	// COLUMN, and renameParseSql's re-resolve of it fails where the original
	// CREATE INDEX succeeded -- CREATE ran under db->init/DQS, the re-parse
	// does not (areDoubleQuotedStringsEnabled, resolve.c:161). An EXPRESSION
	// key ("a || "str"") is fine, which is the second sequence below.
	// musql renames the column and keeps the index text.
	differ(t, "r31-quotefix-index-bare-dqs-key", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1(a || "str", "b", "val")`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-quotefix-index", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1(a || "str", "b")`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-quotefix-index-partial", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1(a) WHERE "b"="bb"`,
		`ALTER TABLE t1 RENAME c TO ccc`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	differ(t, "r31-quotefix-string-alias", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT "string"'alias' FROM t1`,
		`ALTER TABLE t1 RENAME a TO aaa`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	// alterqf.test 2.0/2.1, whole. Its trigger is a TEMP trigger there; kept
	// as a main-schema one so a single sqlite_master read sees everything.
	differ(t, "r31-quotefix-alterqf-2", []string{
		`CREATE TABLE x1(one, two, three, PRIMARY KEY(one), CHECK (three!="xyz"), CHECK (two!="one")) WITHOUT ROWID`,
		`CREATE INDEX x1i ON x1(one+"two"+"four") WHERE "five"`,
		`ALTER TABLE x1 RENAME two TO 'four'`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	// DROP COLUMN runs the same pass (alter.c:2309).
	differ(t, "r31-quotefix-drop-column", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT "b", "notacolumn!" FROM t1`,
		`ALTER TABLE t1 DROP COLUMN a`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
	// ... and RENAME TO does NOT: alter.c:282 calls renameTestSchema only,
	// never renameFixQuotes, so the double quotes survive a table rename.
	differ(t, "r31-quotefix-not-on-rename-to", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE VIEW v1 AS SELECT "a", "notacolumn!" FROM t1`,
		`ALTER TABLE t1 RENAME TO t2`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
}
