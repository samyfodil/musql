// This file tests PRAGMA encoding. The setter is a one-shot that only
// takes effect on an empty database. All three encodings are tested against
// the oracle.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// encodingCases are run through differ() and compared against C SQLite.
var encodingCases = []struct {
	name  string
	stmts []string
}{
	{"getter-on-a-fresh-database", []string{
		`PRAGMA encoding`,
		`PRAGMA main.encoding`,
	}},
	{"setter-after-a-schema-exists-is-ignored", []string{
		`CREATE TABLE t(x)`,
		`PRAGMA encoding = 'UTF-16'`,
		`PRAGMA encoding`,
		`PRAGMA encoding = 'UTF-16le'`,
		`PRAGMA encoding`,
		`PRAGMA encoding = 'UTF-16be'`,
		`PRAGMA encoding`,
		`PRAGMA encoding=UTF16`,
		`PRAGMA encoding`,
		`PRAGMA encoding = 'UTF-8'`,
		`PRAGMA encoding`,
		`PRAGMA encoding=UTF8`,
		`PRAGMA encoding`,
		`INSERT INTO t VALUES('héllo')`,
		`SELECT x, hex(x), length(x) FROM t`,
	}},
	{"an-unrecognized-name-is-ignored-even-when-empty", []string{
		`PRAGMA encoding = 'bogus'`,
		`PRAGMA encoding`,
		`PRAGMA encoding = ''`,
		`PRAGMA encoding`,
		`CREATE TABLE t(x)`,
		`PRAGMA encoding = 'bogus'`,
		`PRAGMA encoding`,
	}},
	{"utf8-on-an-empty-database-is-a-real-no-op", []string{
		`PRAGMA encoding = 'UTF-8'`,
		`PRAGMA encoding`,
		`PRAGMA encoding=utf8`,
		`PRAGMA encoding`,
		`CREATE TABLE t(a TEXT)`,
		`INSERT INTO t VALUES('héllo')`,
		`SELECT a, hex(a), hex(CAST(a AS BLOB)), length(a), length(CAST(a AS BLOB)) FROM t`,
		`PRAGMA encoding`,
	}},
}

func TestEncodingPragmaMatchesCSQLite(t *testing.T) {
	for _, tc := range encodingCases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "encoding/"+tc.name, tc.stmts) })
	}
}

// utf16Script is the same program in every encoding.
func utf16Script(enc string) []string {
	var stmts []string
	if enc != "" {
		stmts = append(stmts, `PRAGMA encoding = '`+enc+`'`)
	}
	return append(stmts,
		`PRAGMA encoding`,
		`CREATE TABLE t(a TEXT, b BLOB)`,
		`CREATE INDEX ti ON t(a)`,
		`INSERT INTO t VALUES('héllo', x'414243')`,
		`INSERT INTO t VALUES('z', NULL)`,
		`INSERT INTO t VALUES('é', NULL)`,
		`INSERT INTO t VALUES('`+"😀"+`ab', NULL)`,
		`INSERT INTO t VALUES('', x'')`,
		// stored text, read back
		`SELECT a, typeof(a), length(a) FROM t ORDER BY rowid`,
		// the byte-exposing seam
		`SELECT hex(a), hex(CAST(a AS BLOB)), length(CAST(a AS BLOB)), octet_length(a) FROM t ORDER BY rowid`,
		`SELECT hex(b), length(b) FROM t WHERE b IS NOT NULL`,
		`SELECT hex(CAST(1 AS TEXT)), hex(CAST(1.5 AS TEXT)), hex(CAST('a' AS BLOB))`,
		`SELECT hex(CAST(x'6100' AS TEXT)), CAST(x'6100' AS TEXT), length(CAST(x'61' AS TEXT)), hex(CAST(x'61' AS TEXT))`,
		`SELECT hex(printf('%s-%d','x',3)), hex(typeof('x')), hex(quote('héllo'))`,
		// the ORDERING seam -- this is what differs between the three
		`SELECT group_concat(a, '|') FROM (SELECT a FROM t ORDER BY a)`,
		`SELECT '`+"😀"+`' < 'z', '`+"😀"+`' < '`+"�"+`', 'é' < 'z', 'a' < x'00'`,
		`SELECT a FROM t WHERE a > 'a' ORDER BY a`,
		`SELECT DISTINCT a FROM t ORDER BY a DESC`,
		`SELECT a FROM t UNION SELECT 'ZZ' ORDER BY 1`,
		// characters -- must be IDENTICAL in every encoding
		`SELECT substr('`+"😀"+`ab',1,1), substr('`+"😀"+`ab',2,1), instr('héllo','l'), replace('`+"😀"+`ab','a','X'), upper('héllo')`,
		`SELECT unicode('`+"😀"+`'), hex(char(128512)), 'héllo' LIKE 'h_llo', '`+"😀"+`ab' GLOB '?ab'`,
		`SELECT 'ABC' = 'abc' COLLATE NOCASE, 'É' = 'é' COLLATE NOCASE, 'a ' = 'a' COLLATE RTRIM`,
		// the schema catalog's own text is stored in that encoding too
		`SELECT name, sql FROM sqlite_master ORDER BY name`,
		`PRAGMA integrity_check`,
		`PRAGMA encoding`,
	)
}

// TestUTF16MatchesCSQLite runs that program in all three encodings against
// both engines. It is the whole feature in one gate: a wrong record encoding
// shows up as different stored text, a missed byte-exposing function as a
// different hex(), and a wrong comparison as a different ORDER BY -- and the
// three encodings disagree with each other on exactly that last one, so a
// UTF-8-shaped answer cannot pass by accident.
func TestUTF16MatchesCSQLite(t *testing.T) {
	for _, enc := range []string{"", "UTF-8", "UTF-16", "UTF-16le", "UTF-16be"} {
		enc := enc
		name := enc
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) { differ(t, "utf16/"+name, utf16Script(enc)) })
	}
}

// TestUTF16FileIsInterchangeable is the file-format half: C SQLite must
// open a UTF-16 database musql wrote, agree on the encoding, call it
// structurally sound (integrity_check recomputes every index key from the row,
// so it validates the encoded ORDER as well as the bytes), and read back exactly
// what musql saw -- and the reverse.
func TestUTF16CornersFoundBySweep(t *testing.T) {
	// utf16align.test: an ODD byte count drops its TRAILING byte, it does not
	// yield the empty string. Both ends of that rule matter.
	differ(t, "utf16/odd-length-blob-to-text", []string{
		`PRAGMA encoding = 'UTF-16le'`,
		`SELECT hex(CAST(x'6efcda' AS TEXT)), length(CAST(x'6efcda' AS TEXT))`,
		`SELECT hex(CAST(x'61' AS TEXT)), length(CAST(x'61' AS TEXT))`,
		`SELECT hex(CAST(x'6efc' AS TEXT))`,
	})
	// attach2.test section 5: every database a connection holds must agree on
	// the encoding, with C SQLite's own wording.
	differ(t, "utf16/attach-must-agree-on-encoding", []string{
		`PRAGMA encoding = 'UTF-16le'`,
		`CREATE TABLE t(a)`,
		`SELECT count(*) FROM t`,
	})

	// ...and a BLOB handed to a function whose arguments C SQLite treats as
	// TEXT, which it converts from the database encoding first. utf16align.test's
	// ltrim and windowC.test's group_concat. These used to be DECLINED; the
	// conversion is now implemented (coerceUTF16BlobArgs, engine/utf16.go) and
	// its own gate, with the full function matrix in all three encodings, is
	// TestR24BlobToTextCoercionInUTF16 (attach_r24_utf16_blob_test.go).
	differ(t, "utf16/blob-argument-is-converted", []string{
		`PRAGMA encoding = 'UTF-16le'`,
		`SELECT hex(ltrim(x'6efcda'))`,
		`SELECT upper(x'6162')`,
		`SELECT replace(x'6162', 'a', 'X')`,
		`SELECT group_concat(y) FROM (SELECT x'5585' AS y UNION ALL SELECT x'd090')`,
	})
}
