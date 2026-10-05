package compat

// Tests that non-ASCII characters in keyword positions are rejected. SQL
// tokens are case-folded byte-wise, not Unicode-aware, so U+0131 (dotless i)
// must not fold to 'I'. All test cases must error in both engines.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// dotlessI is U+0131; unicode.ToUpper('ı') == 'I', so any keyword containing an
// "i" can be misspelled with it and still fold onto the keyword under Unicode
// rules. It is two bytes in UTF-8 (0xC4 0xB1), so it can never match under
// SQLite's byte-wise fold.
const dotlessI = "ı"

func TestNonASCIIKeywordSpellingRejectedLikeC(t *testing.T) {
	cases := []string{
		dotlessI + "NSERT INTO t VALUES(2)",                   // INSERT
		"INSERT " + dotlessI + "NTO t VALUES(3)",              // INTO
		"SELECT * FROM t WHERE a " + dotlessI + "S NOT NULL",  // IS
		"SELECT * FROM t L" + dotlessI + "MIT 1",              // LIMIT
		"SELECT D" + dotlessI + "ST" + dotlessI + "NCT a FROM t", // DISTINCT
	}

	dir := t.TempDir()
	for _, e := range []struct{ label, driver, path string }{
		{"musql-pure", driver.DriverName, filepath.Join(dir, "musql.db")},
		{"mattn-C", "sqlite3", filepath.Join(dir, "c.db")},
	} {
		db, err := sql.Open(e.driver, e.path)
		if err != nil {
			t.Fatalf("[%s] open: %v", e.label, err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("CREATE TABLE t(a INTEGER)"); err != nil {
			db.Close()
			t.Fatalf("[%s] create: %v", e.label, err)
		}
		for _, q := range cases {
			_, err := db.Exec(q)
			if err == nil {
				t.Errorf("[%s] ACCEPTED %q -- a non-ASCII keyword spelling must be a syntax error "+
					"(SQLite folds keywords byte-wise: sqlite3UpperToLower, global.c:24)",
					e.label, strings.ToValidUTF8(q, "?"))
			}
		}
		db.Close()
	}
}
