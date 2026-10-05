package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestViewReturning tests RETURNING on writes to views via INSTEAD OF triggers:
// INSERT, UPDATE, DELETE, and mixed deletion modes. Covers RAISE(IGNORE),
// subqueries with different lifetime rules, and affinity handling per verb.
func TestViewReturning(t *testing.T) {
	triggers := []string{
		`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		`CREATE TRIGGER tu INSTEAD OF UPDATE ON v BEGIN UPDATE t SET b=new.b WHERE a=old.a; END`,
		`CREATE TRIGGER td INSTEAD OF DELETE ON v BEGIN DELETE FROM t WHERE a=old.a; END`,
	}
	plain := append([]string{
		`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'z')`,
		`CREATE VIEW v AS SELECT a,b FROM t`,
	}, triggers...)
	cases := []struct {
		name    string
		setup   []string
		q       string
		decline bool
	}{
		{"update returning cols", plain, `UPDATE v SET b='y' WHERE a=1 RETURNING a,b`, false},
		{"update returning star", plain, `UPDATE v SET b='y' WHERE a=1 RETURNING *`, false},
		{"update returning rowid errors in both", plain, `UPDATE v SET b='y' WHERE a=1 RETURNING rowid`, false},
		{"update returning no rows", plain, `UPDATE v SET b='y' WHERE a=99 RETURNING a`, false},
		{"update returning every row", plain, `UPDATE v SET b=b||'!' RETURNING a,b`, false},
		{"update returning expressions", plain, `UPDATE v SET b='Z' WHERE a=1 RETURNING a+100 AS k, upper(b), b IS NULL`, false},
		{"insert returning cols", plain, `INSERT INTO v VALUES(9,'n') RETURNING a,b`, false},
		{"insert returning star", plain, `INSERT INTO v VALUES(9,'n') RETURNING *`, false},
		{"insert returning several tuples", plain, `INSERT INTO v VALUES(9,'n'),(10,'o') RETURNING a,b`, false},
		{"delete returning cols", plain, `DELETE FROM v WHERE a=2 RETURNING a,b`, false},
		{"delete returning star", plain, `DELETE FROM v RETURNING *`, false},
		{"ignore suppresses the returned row", []string{
			`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER tu INSTEAD OF UPDATE ON v BEGIN
			   SELECT RAISE(IGNORE) WHERE old.a=1;
			   UPDATE t SET b=new.b WHERE a=old.a; END`,
		}, `UPDATE v SET b='Z' RETURNING a,b`, false},
		{"returns the NEW image, not what the body wrote", []string{
			`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER tu INSTEAD OF UPDATE ON v BEGIN UPDATE t SET b='OTHER' WHERE a=old.a; END`,
		}, `UPDATE v SET b='Z' RETURNING a,b`, false},
		{"a body that writes nothing still returns", []string{
			`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER tu INSTEAD OF UPDATE ON v BEGIN SELECT 1; END`,
		}, `UPDATE v SET b='Z' RETURNING a,b`, false},
		{"insert skips the view's affinity", []string{
			`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT CAST(a AS TEXT) AS a, b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v VALUES(5,'q') RETURNING a, typeof(a)`, false},
		{"update applies the view's affinity", []string{
			`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x')`,
			`CREATE VIEW v AS SELECT a, CAST(b AS TEXT) AS b FROM t`,
			`CREATE TRIGGER tu INSTEAD OF UPDATE ON v BEGIN UPDATE t SET b=new.b WHERE a=old.a; END`,
		}, `UPDATE v SET b=5 RETURNING b, typeof(b)`, false},
		{"an unspecified column is NULL in the image", []string{
			`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v(a) VALUES(5) RETURNING a,b`, false},
		{"subquery over an unrelated table", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE s(n)`, `INSERT INTO s VALUES(7)`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v VALUES(1,'p'),(2,'q') RETURNING a,(SELECT n FROM s)`, false},
		{"subquery over the base table the body writes", []string{
			`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM t)`, false},
		{"subquery over the view itself", []string{
			`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM v)`, false},
		{"subquery correlated to the new row", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE s(n)`, `INSERT INTO s VALUES(1),(2),(3)`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s WHERE n<=a)`, false},
		{"subquery in an UPDATE scan", []string{
			`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`, `CREATE TABLE l(m)`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER tu INSTEAD OF UPDATE ON v BEGIN INSERT INTO l VALUES(old.a); END`,
		}, `UPDATE v SET b='Z' RETURNING a,(SELECT count(*) FROM l)`, false},
		{"subquery in a DELETE scan", []string{
			`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER td INSTEAD OF DELETE ON v BEGIN DELETE FROM t WHERE a=old.a; END`,
		}, `DELETE FROM v RETURNING a,(SELECT count(*) FROM t)`, false},
		{"insert select returning", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE src(x,y)`, `INSERT INTO src VALUES(1,'p'),(2,'q')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v SELECT x,y FROM src RETURNING a,b`, false},
		{"insert select returning star", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE src(x,y)`, `INSERT INTO src VALUES(1,'p'),(2,'q')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v SELECT x,y FROM src RETURNING *`, false},
		{"insert select returning expressions", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE src(x,y)`, `INSERT INTO src VALUES(1,'p'),(2,'q')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v SELECT x,y FROM src RETURNING a*10, upper(b)`, false},
		{"insert select, a body that ignores one row", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE src(x,y)`, `INSERT INTO src VALUES(1,'p'),(2,'q')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN
			   SELECT RAISE(IGNORE) WHERE new.a=1;
			   INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v SELECT x,y FROM src RETURNING a,b`, false},
		{"insert select, subquery over the view", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE src(x,y)`, `INSERT INTO src VALUES(1,'p'),(2,'q')`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v SELECT x,y FROM src RETURNING a,(SELECT count(*) FROM v)`, false},
		{"insert select, an empty source", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE src(x,y)`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v SELECT x,y FROM src RETURNING a,b`, false},
		{"mixed lifetimes, insert select", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE src(x,y)`, `INSERT INTO src VALUES(1,'p'),(2,'q')`,
			`CREATE TABLE s(n)`, `INSERT INTO s VALUES(4)`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v SELECT x,y FROM src RETURNING a,(SELECT count(*) FROM v),(SELECT n FROM s)`, true},
		{"mixed lifetimes, VALUES", []string{
			`CREATE TABLE t(a,b)`, `CREATE TABLE s(n)`, `INSERT INTO s VALUES(4)`,
			`CREATE VIEW v AS SELECT a,b FROM t`,
			`CREATE TRIGGER ti INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a,new.b); END`,
		}, `INSERT INTO v VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM v),(SELECT n FROM s)`, true},
	}
	for ci, c := range cases {
		ci, c := ci, c
		t.Run(fmt.Sprintf("%02d-%s", ci, c.name), func(t *testing.T) {
			var out [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range c.setup {
					if _, err := db.Exec(s); err != nil {
						t.Fatalf("%s: %v", s, err)
					}
				}
				out[i] = renderQuery(db, c.q) + " || " + renderQuery(db, `SELECT a,b FROM t ORDER BY a`)
				db.Close()
			}
			if c.decline {
				if out[0] == out[1] {
					t.Errorf("%s: this engine now agrees with the oracle -- move it out of the declining set: %s", c.q, out[1])
				}
				return
			}
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", c.q, out[0], out[1])
			}
		})
	}
}
