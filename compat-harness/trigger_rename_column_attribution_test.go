package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestTriggerRenameColumnAttribution tests ALTER TABLE RENAME COLUMN with
// trigger body references against C SQLite.
func TestTriggerRenameColumnAttribution(t *testing.T) {
	for ci, c := range [][]string{
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT a FROM t; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT a FROM o; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE VIEW a AS SELECT 1 AS z`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT a FROM t; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg VALUES(new.a); END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER UPDATE OF a ON t BEGIN INSERT INTO lg VALUES(old.a); END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE u(a,c)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER UPDATE OF a ON u BEGIN INSERT INTO lg SELECT t.a FROM t; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT o.a FROM o; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// a step's SET name belongs to that step's TARGET, not to the trigger's table
		{`CREATE TABLE t(a,b)`, `CREATE TABLE lg(a)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN UPDATE lg SET a=1; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// ...and when the target IS the renamed table, both SET name and value
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN UPDATE t SET a=a+1 WHERE b=1; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// an INSERT step's column list
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE u(c)`,
			`CREATE TRIGGER tg AFTER INSERT ON u BEGIN INSERT INTO t(a) VALUES(1); END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// both tables carry "a" but the reference is qualified
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT t.a FROM t,o; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// an ALIAS on the renamed table
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT x.a FROM t AS x; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// INSTEAD OF on a view whose own column shares the name
		{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT b AS a FROM t`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg INSTEAD OF INSERT ON v BEGIN INSERT INTO lg VALUES(new.a); END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// a trigger on another table reading the renamed one
		{`CREATE TABLE t(a,b)`, `CREATE TABLE u(c)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON u BEGIN INSERT INTO lg SELECT a FROM t; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// a WHEN clause
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t WHEN new.a>0 BEGIN INSERT INTO lg VALUES(1); END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// a QUOTED, case-different reference
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT "A" FROM t; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// a subquery inside a DELETE step's WHERE -- a scope reached only by
		// walking the step's own expressions, not its target
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN DELETE FROM lg WHERE m IN (SELECT a FROM t); END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// The oracle itself refuses these two, so this engine must as well:
		// a subquery FROM item (whose output column shadows the table's)...
		{`CREATE TABLE t(a,b)`, `CREATE TABLE a(z)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg SELECT a FROM (SELECT a FROM t); END`, `ALTER TABLE t RENAME COLUMN a TO q`},
		// ...and a body whose bare reference is genuinely ambiguous.
		{`CREATE TABLE t(a,b)`, `CREATE TABLE o(a)`, `CREATE TABLE lg(m)`,
			`CREATE TRIGGER t1 AFTER INSERT ON t BEGIN INSERT INTO lg SELECT a FROM t; END`,
			`CREATE TRIGGER t2 AFTER INSERT ON t BEGIN INSERT INTO lg SELECT a FROM t,o; END`, `ALTER TABLE t RENAME COLUMN a TO q`},
	} {
		ci, c := ci, c
		t.Run(fmt.Sprintf("%02d", ci), func(t *testing.T) {
			var out [2]string
			var failed [2]bool
			for i, drv := range []string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range c {
					if _, e := db.Exec(s); e != nil {
						failed[i] = true
					}
				}
				out[i] = renderQuery(db, `SELECT sql FROM sqlite_schema WHERE type='trigger' ORDER BY name`)
				db.Close()
			}
			if failed[0] != failed[1] {
				t.Errorf("%v: cgo failed=%v, musql failed=%v\n  cgo: %s\n  mus: %s", c, failed[0], failed[1], out[0], out[1])
			}
			if out[0] != out[1] {
				t.Errorf("%v\n  cgo: %s\n  mus: %s", c, out[0], out[1])
			}
		})
	}
}
