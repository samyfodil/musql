package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRoundTrip exports a database and imports the file back, and asks both
// the same questions. The generated-column table is the case the old importer
// lost a column on: it named the columns from PRAGMA table_info, which leaves
// generated columns out, so a STORED one's value took the next column's slot
// and the last column came back NULL.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a, db, b := filepath.Join(dir, "a.musq"), filepath.Join(dir, "a.db"), filepath.Join(dir, "b.musq")
	buildDB(t, a,
		`CREATE TABLE g(a INT, b AS (a*2), c AS (a+1) STORED, d)`,
		`INSERT INTO g(a, d) VALUES(1, 'x'), (2, 'y')`,
		`CREATE TABLE w(k TEXT, v AS (k || '!'), n INT, PRIMARY KEY(n DESC, k)) WITHOUT ROWID`,
		`INSERT INTO w(k, n) VALUES('a', 1), ('b', 2), ('c', 2)`,
		`ALTER TABLE w ADD COLUMN z DEFAULT 9`,
		`INSERT INTO w(k, n, z) VALUES('d', 3, 4)`,
		`CREATE TABLE r(id INTEGER PRIMARY KEY AUTOINCREMENT, t TEXT COLLATE NOCASE UNIQUE, f REAL)`,
		`INSERT INTO r(t, f) VALUES('One', 1), ('two', 2.5), ('Three', NULL)`,
		`DELETE FROM r WHERE id = 3`,
		`CREATE INDEX r_f ON r(f DESC)`,
		`CREATE INDEX r_e ON r(lower(t)) WHERE f > 1`,
		`CREATE VIEW v AS SELECT * FROM r`,
		`CREATE TRIGGER tr AFTER INSERT ON r BEGIN UPDATE r SET f = 0 WHERE id = new.id; END`,
		`PRAGMA user_version = 7`,
	)
	if err := Export(a, db, 0); err != nil {
		t.Fatal(err)
	}
	if err := Import(db, b, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT * FROM g ORDER BY a`,
		`SELECT * FROM w ORDER BY k`,
		`SELECT rowid, * FROM r ORDER BY id`,
		`SELECT * FROM v ORDER BY id`,
		`SELECT * FROM sqlite_sequence`,
		`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY rowid`,
		`SELECT t FROM r WHERE t = 'ONE'`,
		`SELECT id FROM r INDEXED BY r_f WHERE f > 0`,
		`PRAGMA user_version`,
		`PRAGMA integrity_check`,
	} {
		x, xerr := queryDB(t, a, q)
		y, yerr := queryDB(t, b, q)
		if xerr != nil || yerr != nil {
			t.Fatalf("%s: %v / %v", q, xerr, yerr)
		}
		if x != y {
			t.Errorf("%s\n  before: %s\n  after:  %s", q, x, y)
		}
	}
	if got, _ := queryDB(t, b, `SELECT d FROM g ORDER BY a`); !strings.Contains(got, `"x"`) {
		t.Errorf("g.d after the round trip: %s", got)
	}
}
