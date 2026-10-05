package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Generated columns (virtual, STORED, indexed, under UPDATE and ALTER) and
// WITHOUT ROWID tables (text/NOCASE/DESC/composite keys, AUTOINCREMENT errors).
func TestGeneratedAndWithoutRowidMatrix(t *testing.T) {
	cases := []struct{ name string; stmts []string; q string }{
		{"gen virtual index", []string{
			`CREATE TABLE g(a INT, b AS (a*2), c AS (a||'!') STORED)`,
			`CREATE INDEX gi ON g(b)`, `INSERT INTO g(a) VALUES(3),(1),(2)`,
		}, `SELECT a,b,c FROM g WHERE b>2 ORDER BY b`},
		{"gen update", []string{
			`CREATE TABLE g(a INT, b AS (a*2))`, `INSERT INTO g(a) VALUES(1)`, `UPDATE g SET a=5`,
		}, `SELECT a,b FROM g`},
		{"gen alter add", []string{
			`CREATE TABLE g(a INT)`, `INSERT INTO g VALUES(2)`, `ALTER TABLE g ADD COLUMN d AS (a+1)`,
		}, `SELECT a,d FROM g`},
		{"gen alter add stored", []string{
			`CREATE TABLE g(a INT)`, `INSERT INTO g VALUES(2)`, `ALTER TABLE g ADD COLUMN d AS (a+1) STORED`,
		}, `SELECT a,d FROM g`},
		{"gen drop source", []string{
			`CREATE TABLE g(a INT, b INT, c AS (a+b))`, `INSERT INTO g(a,b) VALUES(1,2)`, `ALTER TABLE g DROP COLUMN b`,
		}, `SELECT * FROM g`},
		{"gen rename source", []string{
			`CREATE TABLE g(a INT, b AS (a*2))`, `INSERT INTO g(a) VALUES(4)`, `ALTER TABLE g RENAME COLUMN a TO z`,
		}, `SELECT z,b, (SELECT sql FROM sqlite_master WHERE name='g')`},
		{"gen in check", []string{
			`CREATE TABLE g(a INT, b AS (a*2), CHECK(b<10))`, `INSERT INTO g(a) VALUES(4)`,
		}, `SELECT * FROM g`},
		{"gen insert explicit", []string{
			`CREATE TABLE g(a INT, b AS (a*2))`, `INSERT INTO g(a,b) VALUES(1,9)`,
		}, `SELECT * FROM g`},
		{"gen typeof", []string{
			`CREATE TABLE g(a, b TEXT AS (a), c INT AS (a))`, `INSERT INTO g(a) VALUES(5)`,
		}, `SELECT typeof(a), typeof(b), typeof(c), b, c FROM g`},
		{"wr text pk", []string{
			`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
			`INSERT INTO w VALUES('b',1),('a',2),('C',3)`,
		}, `SELECT k,v FROM w`},
		{"wr nocase pk", []string{
			`CREATE TABLE w(k TEXT COLLATE NOCASE PRIMARY KEY, v) WITHOUT ROWID`,
			`INSERT INTO w VALUES('b',1),('A',2)`, `INSERT OR REPLACE INTO w VALUES('a',9)`,
		}, `SELECT k,v FROM w`},
		{"wr desc pk", []string{
			`CREATE TABLE w(k INT PRIMARY KEY DESC, v) WITHOUT ROWID`,
			`INSERT INTO w VALUES(1,'a'),(3,'c'),(2,'b')`,
		}, `SELECT k,v FROM w`},
		{"wr multi pk", []string{
			`CREATE TABLE w(a,b,v,PRIMARY KEY(a,b DESC)) WITHOUT ROWID`,
			`INSERT INTO w VALUES(1,1,'x'),(1,2,'y'),(2,1,'z')`,
		}, `SELECT a,b,v FROM w`},
		{"wr blob pk", []string{
			`CREATE TABLE w(k BLOB PRIMARY KEY, v) WITHOUT ROWID`,
			`INSERT INTO w VALUES(x'02',1),(x'01',2),(x'0100',3)`,
		}, `SELECT quote(k),v FROM w`},
		{"wr rowid ref", []string{
			`CREATE TABLE w(k TEXT PRIMARY KEY) WITHOUT ROWID`, `INSERT INTO w VALUES('a')`,
		}, `SELECT rowid FROM w`},
		{"wr integrity", []string{
			`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
			`INSERT INTO w VALUES('a',1),('b',2)`, `DELETE FROM w WHERE k='a'`,
		}, `PRAGMA integrity_check`},
		{"wr autoinc", []string{
			`CREATE TABLE w(k INTEGER PRIMARY KEY AUTOINCREMENT, v) WITHOUT ROWID`,
		}, `SELECT 1`},
		{"wr null pk", []string{
			`CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `INSERT INTO w VALUES(NULL,1)`,
		}, `SELECT quote(k) FROM w`},
	}
	for _, tc := range cases {
		var out [2]string
		for i, drv := range []string{"sqlite3", "sqlite"} {
			db, _ := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
			db.SetMaxOpenConns(1)
			errs := ""
			for _, s := range tc.stmts {
				if _, err := db.Exec(s); err != nil {
					errs += "[E:" + err.Error() + "]"
				}
			}
			out[i] = errs + " || " + renderQuery(db, tc.q)
			db.Close()
		}
		if out[0] != out[1] {
			t.Errorf("[%s] %s\n  cgo: %s\n  mus: %s", tc.name, tc.q, out[0], out[1])
		}
	}
}
