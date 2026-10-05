// This file gates that the SQLite file musql exports is byte-identical to the
// file C SQLite writes for the same database. Exported files are compared against
// C's VACUUM INTO, with the schema cookie blanked. Known unequal cases are reported.
package compat

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fbeBuild(t *testing.T, driver, path string, stmts []string) {
	t.Helper()
	db, err := sql.Open(driver, path)
	if err != nil {
		t.Fatalf("%s open: %v", driver, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range stmts {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s %q: %v", driver, s, eerr)
		}
	}
}

// fbeSeries is a recursive CTE for generating test data.
func fbeSeries(n int, insert, projection string) string {
	return fmt.Sprintf(
		"WITH RECURSIVE s(value) AS (SELECT 1 UNION ALL SELECT value+1 FROM s WHERE value < %d) "+
			"%s SELECT %s FROM s", n, insert, projection)
}

// fbeCompare builds the same SQL with both engines and returns the two files,
// with the schema cookie blanked.
func fbeCompare(t *testing.T, stmts []string) (musql, cgo []byte) {
	t.Helper()
	dir := t.TempDir()
	mp, cp := filepath.Join(dir, "m.db"), filepath.Join(dir, "c.db")
	fbeBuild(t, "sqlite", mp, stmts)
	cv := filepath.Join(dir, "c-vacuum-into.db")
	fbeBuild(t, "sqlite3", cp, append(append([]string{}, stmts...), `VACUUM INTO '`+cv+`'`))
	cp = cv
	mb, err := os.ReadFile(exportedForOracle(t, mp))
	if err != nil {
		t.Fatalf("read musql file: %v", err)
	}
	cb, err := os.ReadFile(cp)
	if err != nil {
		t.Fatalf("read cgo file: %v", err)
	}
	// Blank the schema cookie (VACUUM INTO increments it).
	for _, b := range [][]byte{mb, cb} {
		if len(b) >= 44 {
			copy(b[40:44], []byte{0, 0, 0, 0})
		}
	}
	return mb, cb
}

// fbeFirstDiff locates the first differing byte and names the page it is on,
// so a failure says WHERE rather than just "not equal".
func fbeFirstDiff(m, c []byte) string {
	if len(m) != len(c) {
		return fmt.Sprintf("sizes differ: musql=%d cgo=%d", len(m), len(c))
	}
	for i := range m {
		if m[i] != c[i] {
			where := "header"
			if i >= 100 {
				where = fmt.Sprintf("page %d offset %d", i/4096+1, i%4096)
			}
			return fmt.Sprintf("first difference at byte %d (%s): cgo=0x%02x musql=0x%02x", i, where, c[i], m[i])
		}
	}
	return "identical"
}

// fbeEqual are the shapes whose files MUST be byte-identical.
var fbeEqual = []struct {
	name  string
	stmts []string
}{
	{"empty-schema", []string{`CREATE TABLE a(x)`}},
	{"one-row", []string{`CREATE TABLE a(x)`, `INSERT INTO a VALUES(1)`}},
	{"one-page", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(30, "INSERT INTO a", "value,'r'||value")}},
	{"several-tables", []string{
		`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		`CREATE TABLE b(z)`,
		`CREATE INDEX ay ON a(y)`,
		`CREATE VIEW v AS SELECT x FROM a`,
	}},
	{"trigger-fires", []string{
		`CREATE TABLE a(x)`, `CREATE TABLE l(v)`,
		`CREATE TRIGGER t AFTER INSERT ON a BEGIN INSERT INTO l VALUES(new.x); END`,
		`INSERT INTO a VALUES(1)`,
	}},
	{"transaction", []string{`CREATE TABLE a(x)`, `BEGIN`,
		`INSERT INTO a VALUES(1)`, `INSERT INTO a VALUES(2)`, `COMMIT`}},
	{"no-op-update", []string{`CREATE TABLE a(x)`, `INSERT INTO a VALUES(1)`,
		`UPDATE a SET x=1 WHERE x=99`}},
	{"no-op-delete", []string{`CREATE TABLE a(x)`, `INSERT INTO a VALUES(1)`,
		`DELETE FROM a WHERE x=99`}},
	{"vacuumed", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(100, "INSERT INTO a", "value,value"),
		`DELETE FROM a WHERE x>50`, `VACUUM`}},
	{"autoincrement-append", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY AUTOINCREMENT, y)`,
		fbeSeries(300, "INSERT INTO a(y)", "value")}},
	{"descending", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(300, "INSERT INTO a", "1000-value,'r'||value")}},
	{"secondary-index", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		`CREATE INDEX ay ON a(y)`, fbeSeries(200, "INSERT INTO a", "value,'y'||value")}},
	{"multi-page-ascending", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(400, "INSERT INTO a", "value,'row'||value")}},
	{"big-rows", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(300, "INSERT INTO a", "value,hex(zeroblob(100))")}},
	{"insert-100", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(100, "INSERT INTO a", "value,value")}},
	{"delete-tail", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(100, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE x>50`}},
	{"delete-scattered", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(100, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE x%3=0`}},
	{"delete-head", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(100, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE x<=50`}},
	{"delete-alternating", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(80, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE x%2=0`}},
	{"delete-then-insert", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(100, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE x%4=0`,
		`INSERT INTO a VALUES(500,500)`}},
	{"delete-then-many-inserts", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(100, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE x%3=0`,
		fbeSeries(20, "INSERT INTO a", "500+value,value")}},
	{"reuse-freeblock", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(60, "INSERT INTO a", "value,hex(zeroblob(20))"),
		`DELETE FROM a WHERE x%5=0`, `INSERT INTO a VALUES(500,'z')`}},
	{"delete-insert-cycles", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(60, "INSERT INTO a", "value,value"),
		`DELETE FROM a WHERE x%2=0`, `INSERT INTO a VALUES(200,200)`,
		`DELETE FROM a WHERE x%3=0`, `INSERT INTO a VALUES(300,300)`}},
	{"delete-all-then-insert", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(50, "INSERT INTO a", "value,value"), `DELETE FROM a`,
		`INSERT INTO a VALUES(1,1)`}},
	{"truncate-only", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(50, "INSERT INTO a", "value,value"), `DELETE FROM a`}},
	{"where-covers-all", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(50, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE x<=50`}},
	{"where-always-true", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(50, "INSERT INTO a", "value,value"), `DELETE FROM a WHERE 1`}},
	{"truncate-twice", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(40, "INSERT INTO a", "value,value"), `DELETE FROM a`,
		fbeSeries(10, "INSERT INTO a", "value,value"), `DELETE FROM a`}},
	{"truncate-then-refill", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y)`,
		fbeSeries(50, "INSERT INTO a", "value,value"), `DELETE FROM a`,
		fbeSeries(30, "INSERT INTO a", "value,value")}},
	{"shuffled", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(200, "INSERT INTO a", "(value*7919)%1009,'r'||value")}},
	{"reverse-index", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y INT)`,
		`CREATE INDEX ay ON a(y)`, fbeSeries(200, "INSERT INTO a", "value,1000-value")}},
	{"case-b-middle-leaf-unlink", []string{`PRAGMA page_size=512`, `CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(500, "INSERT INTO a", "value,'x0123456789'"), `DELETE FROM a WHERE x<=250`}},
	{"case-d-root-collapse", []string{`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT)`,
		fbeSeries(222, "INSERT INTO a", "value,'x0123456789'"), `DELETE FROM a WHERE x<=221`}},
}

func TestFileBytesEqual(t *testing.T) {
	for _, c := range fbeEqual {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m, g := fbeCompare(t, c.stmts)
			if !bytes.Equal(m, g) {
				t.Errorf("musql's file is not byte-identical to SQLite's: %s", fbeFirstDiff(m, g))
			}
		})
	}
}

// fbeKnownUnequal are shapes still differing from C SQLite, with the cause named.
// They are REPORTED rather than asserted.
var fbeKnownUnequal = []struct {
	name, cause string
	stmts       []string
}{
	{"without-rowid", "export restores arrival order; VACUUM INTO copies in key order",
		[]string{`CREATE TABLE a(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
			fbeSeries(200, "INSERT INTO a", "'k'||value,value")}},
}

// TestFileBytesKnownUnequal reports still-unequal cases; it fails when one becomes equal.
func TestFileBytesKnownUnequal(t *testing.T) {
	for _, c := range fbeKnownUnequal {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m, g := fbeCompare(t, c.stmts)
			if bytes.Equal(m, g) {
				t.Errorf("this file is now byte-identical -- move %q into fbeEqual "+
					"(recorded cause was: %s)", c.name, c.cause)
				return
			}
			t.Logf("still differs (%s): %s", c.cause, fbeFirstDiff(m, g))
		})
	}
}
