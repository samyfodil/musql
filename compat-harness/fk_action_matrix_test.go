package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestFKActionMatrix tests foreign key actions (NO ACTION, RESTRICT, CASCADE,
// SET NULL, SET DEFAULT) on both DELETE and UPDATE events against C SQLite.
func TestFKActionMatrix(t *testing.T) {
	type tc struct {
		name    string
		stmts   []string
		q       string
		decline bool
	}
	var cases []tc
	for _, act := range []string{"NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"} {
		for _, ev := range []string{"ON DELETE", "ON UPDATE"} {
			base := []string{
				`PRAGMA foreign_keys=ON`,
				`CREATE TABLE p(k INTEGER PRIMARY KEY, v)`,
				`CREATE TABLE c(f DEFAULT 7 REFERENCES p(k) ` + ev + ` ` + act + `, d)`,
				`INSERT INTO p VALUES(1,'a'),(7,'g')`,
				`INSERT INTO c VALUES(1,'x')`,
			}
			del := append(append([]string(nil), base...), `DELETE FROM p WHERE k=1`)
			upd := append(append([]string(nil), base...), `UPDATE p SET k=9 WHERE k=1`)
			cases = append(cases,
				tc{ev + " " + act + " / delete", del, `SELECT (SELECT count(*) FROM p), (SELECT quote(f)||'/'||d FROM c)`, false},
				tc{ev + " " + act + " / update", upd, `SELECT (SELECT group_concat(k) FROM p), (SELECT quote(f)||'/'||d FROM c)`, false},
			)
		}
	}
	cases = append(cases,
		tc{"deferred violation in txn", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(f REFERENCES p(k) DEFERRABLE INITIALLY DEFERRED)`,
			`BEGIN`, `INSERT INTO c VALUES(5)`, `INSERT INTO p VALUES(5)`, `COMMIT`,
		}, `SELECT (SELECT count(*) FROM c), (SELECT count(*) FROM p)`, false},
		tc{"deferred unresolved", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(f REFERENCES p(k) DEFERRABLE INITIALLY DEFERRED)`,
			`BEGIN`, `INSERT INTO c VALUES(5)`, `COMMIT`,
		}, `SELECT count(*) FROM c`, false},
		tc{"self-referential cascade", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE s(id INTEGER PRIMARY KEY, parent REFERENCES s(id) ON DELETE CASCADE)`,
			`INSERT INTO s VALUES(1,NULL),(2,1),(3,2)`, `DELETE FROM s WHERE id=1`,
		}, `SELECT count(*) FROM s`, false},
		tc{"fk to unique non-pk", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INT UNIQUE, z)`, `CREATE TABLE c(f REFERENCES p(k) ON DELETE CASCADE)`,
			`INSERT INTO p VALUES(1,'a')`, `INSERT INTO c VALUES(1)`, `DELETE FROM p`,
		}, `SELECT (SELECT count(*) FROM c)`, false},
		tc{"fk missing parent index", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INT, z)`, `CREATE TABLE c(f REFERENCES p(k))`,
			`INSERT INTO p VALUES(1,'a')`, `INSERT INTO c VALUES(1)`,
		}, `SELECT count(*) FROM c`, false},
		tc{"fk composite", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(a,b,PRIMARY KEY(a,b))`,
			`CREATE TABLE c(x,y,FOREIGN KEY(x,y) REFERENCES p(a,b) ON DELETE CASCADE)`,
			`INSERT INTO p VALUES(1,2)`, `INSERT INTO c VALUES(1,2)`, `DELETE FROM p`,
		}, `SELECT count(*) FROM c`, false},
		tc{"fk null child", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INTEGER PRIMARY KEY)`, `CREATE TABLE c(f REFERENCES p(k))`,
			`INSERT INTO c VALUES(NULL)`,
		}, `SELECT count(*) FROM c`, false},
		tc{"fk off then on", []string{
			`PRAGMA foreign_keys=OFF`,
			`CREATE TABLE p(k INTEGER PRIMARY KEY)`, `CREATE TABLE c(f REFERENCES p(k))`,
			`INSERT INTO c VALUES(5)`, `PRAGMA foreign_keys=ON`,
		}, `PRAGMA foreign_key_check`, false},
		tc{"fk drop parent table", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INTEGER PRIMARY KEY)`, `CREATE TABLE c(f REFERENCES p(k))`,
			`INSERT INTO p VALUES(1)`, `INSERT INTO c VALUES(1)`, `DROP TABLE p`,
		}, `SELECT (SELECT count(*) FROM c)`, false},
		tc{"fk cascade with trigger", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INTEGER PRIMARY KEY)`, `CREATE TABLE c(f REFERENCES p(k) ON DELETE CASCADE)`,
			`CREATE TABLE log(m)`,
			`CREATE TRIGGER tg AFTER DELETE ON c BEGIN INSERT INTO log VALUES(old.f); END`,
			`INSERT INTO p VALUES(1)`, `INSERT INTO c VALUES(1)`, `DELETE FROM p`,
		}, `SELECT (SELECT count(*) FROM c), (SELECT group_concat(m) FROM log)`, false},
		// The cascade above was DECLINED until the action learned to fire the
		// child's own triggers: in C an action IS a trigger program
		// (fkActionTrigger, fkey.c), so the implicit DELETE is ordinary and c's
		// AFTER DELETE runs. fk_action_child_triggers_test.go carries the full
		// fourteen shapes; this row keeps the matrix's own coverage of it.
		//
		// What the matrix now declines instead is the one ORDERING shape: C codes
		// an action's own NESTED actions ahead of the row's AFTER trigger
		// (sqlite3GenerateRowDelete emits sqlite3FkActions before the AFTER
		// block), so a chain interleaves depth-first where this engine drains one
		// level at a time.
		tc{"fk cascade CHAIN with a trigger at each level", []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
			`CREATE TABLE c(f INTEGER PRIMARY KEY REFERENCES p(k) ON DELETE CASCADE)`,
			`CREATE TABLE g(h REFERENCES c(f) ON DELETE CASCADE)`,
			`CREATE TABLE log(m)`,
			`CREATE TRIGGER tgc AFTER DELETE ON c BEGIN INSERT INTO log VALUES('c'||old.f); END`,
			`CREATE TRIGGER tgg AFTER DELETE ON g BEGIN INSERT INTO log VALUES('g'||old.h); END`,
			`INSERT INTO p VALUES(1)`, `INSERT INTO c VALUES(1)`, `INSERT INTO g VALUES(1)`,
			`DELETE FROM p`,
		}, `SELECT (SELECT count(*) FROM c), (SELECT group_concat(m) FROM log)`, true},
	)
	for _, c := range cases {
		var out [2]string
		for i, drv := range []string{"sqlite3", "sqlite"} {
			db, _ := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
			db.SetMaxOpenConns(1)
			errs := ""
			for _, s := range c.stmts {
				if _, err := db.Exec(s); err != nil {
					errs += "[E:" + err.Error() + "]"
				}
			}
			out[i] = errs + " || " + renderQuery(db, c.q)
			db.Close()
		}
		if c.decline {
			if out[0] == out[1] {
				t.Errorf("[%s] this engine now agrees with the oracle -- move it out of the declining set: %s", c.name, out[1])
			}
			continue
		}
		if out[0] != out[1] {
			t.Errorf("[%s]\n  cgo: %s\n  mus: %s", c.name, out[0], out[1])
		}
	}
}
