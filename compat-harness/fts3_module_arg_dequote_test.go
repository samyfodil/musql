// This file tests fts3/fts4 module-argument parsing of empty column names.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestFts3ModuleArgumentEmptyColumnName(t *testing.T) {
	cases := []struct {
		name      string
		stmts     []string
		nAccepted int
	}{
		// The mined shape itself: a quoted-empty-string argument in the middle
		// of the column list dequotes to "", C fts3's "Fill in the
		// azColumn array" loop just writes that as the column's name.
		//
		// (PRAGMA table_xinfo on an fts3/4 table is served now and gated
		// against the oracle in TestVirtualTableInfoMatchesCSQLite; it is
		// not exercised here because this case is about the COLUMN NAMES the
		// module argument parser produces, which table_info already shows.)
		{"a quoted empty string is a valid, empty column name", []string{
			`CREATE VIRTUAL TABLE t9 USING fts4(a, "", '---')`,
			`SELECT type, name, sql FROM sqlite_master WHERE name LIKE 't9%' ORDER BY name`,
			`INSERT INTO t9(a, "", "---") VALUES('hello', 'middle', 'world')`,
			`SELECT a, "", "---" FROM t9`,
		}, 4},

		// "<" starts no token at all (not an identifier character, not a
		// quote) -- sqlite3Fts3NextToken finds nothing, which is the SAME
		// "n stays 0" empty-name case, not a decline.
		{"a character that starts no token is also an empty column name", []string{
			`CREATE VIRTUAL TABLE t10 USING fts3(<, b, c)`,
			`SELECT type, name, sql FROM sqlite_master WHERE name LIKE 't10%' ORDER BY name`,
			`INSERT INTO t10("", b, c) VALUES('one', 'two', 'three')`,
			`SELECT "", b, c FROM t10`,
		}, 4},

		// Excluded class this fix's soundness depends on: TWO columns that
		// both dequote to "" still collide, through the EXISTING, unchanged
		// duplicate-column-name check (vtab_fts3.go's parseSchemaWith, "seen"
		// map) -- this fix only stops treating a SINGLE empty name as an
		// error, it does not touch that check at all. Both engines reject
		// this (cgo: "vtable constructor failed"), so plain agreement is the
		// right assertion here.
		{"two empty column names still collide", []string{
			`CREATE VIRTUAL TABLE t11 USING fts4(a, "", "")`,
		}, 0},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, c.stmts, c.nAccepted)
		})
	}
}

func TestFts3ModuleArgumentDequoteDoubling(t *testing.T) {
	// fts3aux1.test 5.1/5.2's own shape: a table whose real name embeds a
	// single quote, created with ONE quoting style and referenced by fts4aux
	// with ANOTHER -- the doubled-single-quote spelling -- both dequoting to
	// the identical target name. This exercises fts3DequoteArg's doubling
	// rule end-to-end (not just "CREATE succeeds"): the fts4aux read has to
	// actually resolve the SAME target table both ways to answer identically.
	stmts := []string{
		`CREATE VIRTUAL TABLE "abc '!' def" USING fts4(x, y)`,
		`INSERT INTO "abc '!' def" VALUES('XX', 'YY')`,
		// Double-quoted argument: no doubling needed, the embedded ' is a
		// different quote character from the delimiter.
		`CREATE VIRTUAL TABLE terms3 USING fts4aux("abc '!' def")`,
		`SELECT * FROM terms3`,
		// Single-quoted argument: the embedded ' must be DOUBLED to stay
		// inside the literal, and fts3DequoteArg's doubling-absorption rule
		// (fts3.c:464's "z[iIn+1]!=quote" check) is what collapses it back to
		// one -- resolving to the SAME target table as terms3 above.
		`CREATE VIRTUAL TABLE "%%^^%%" USING fts4aux('abc ''!'' def')`,
		`SELECT * FROM "%%^^%%"`,
	}
	differAllAccepted(t, "fts4aux target dequoted with doubled single quotes", stmts, len(stmts))
}

// TestFts4CompressOptionMatchesCSQLite compares an fts4 table's
// compress=/uncompress= functions with C SQLite: every %_content write goes
// through compress= and every read through uncompress= (fts3WriteExprList /
// fts3ReadExprList, fts3.c:867-955), including the errors a function that does
// not exist, takes the wrong number of arguments or is an aggregate raises, and
// WHERE those errors are raised -- a MATCH that reads no content never prepares
// the read.
func TestFts4CompressOptionMatchesCSQLite(t *testing.T) {
	for _, pair := range [][2]string{{"lower", "upper"}, {"hex", "unhex"}, {"quote", "trim"}, {"zip", "unzip"}, {"lower", "nosuch"}, {"substr", "upper"}, {"count", "upper"}, {"", ""}} {
		differ(t, "fts4 compress="+pair[0]+" uncompress="+pair[1], []string{
			`CREATE VIRTUAL TABLE tt USING fts4(a, b, compress='` + pair[0] + `', uncompress='` + pair[1] + `')`,
			`INSERT INTO tt VALUES('Hello World', 'Second Col')`,
			`INSERT INTO tt(docid, a, b) VALUES(7, 'Another row', NULL)`,
			`SELECT docid, a, b FROM tt ORDER BY docid`,
			`SELECT docid FROM tt WHERE tt MATCH 'hello'`,
			`SELECT a FROM tt WHERE tt MATCH 'row'`,
			`SELECT quote(c0a), quote(c1b) FROM tt_content ORDER BY docid`,
			`SELECT snippet(tt), offsets(tt) FROM tt WHERE tt MATCH 'world'`,
			`UPDATE tt SET b='changed Text' WHERE docid=7`,
			`SELECT docid, a, b FROM tt ORDER BY docid`,
			`SELECT quote(c1b) FROM tt_content ORDER BY docid`,
			`DELETE FROM tt WHERE a LIKE 'hello%'`,
			`SELECT docid, a, b FROM tt ORDER BY docid`,
			`INSERT INTO tt(tt) VALUES('rebuild')`,
			`INSERT INTO tt(tt) VALUES('integrity-check')`,
			`SELECT docid FROM tt WHERE tt MATCH 'changed'`,
			`SELECT * FROM tt_docsize`,
			`SELECT * FROM tt WHERE docid=7`,
			`INSERT OR REPLACE INTO tt(docid, a) VALUES(7, 'replaced')`,
			`SELECT docid, a, b FROM tt ORDER BY docid`,
		})
	}
}
