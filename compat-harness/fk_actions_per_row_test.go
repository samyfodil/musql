package compat

import "testing"

// C runs a row's FOREIGN KEY actions right after that row is written and before
// its AFTER triggers (update.c:1107, :1118), and evaluates each UPDATE row's SET
// right-hand sides inside the row loop (update.c:954) -- so what a trigger or an
// FK action wrote for row 1 is visible to row 2. This engine applied every FK
// action at the end of the statement and read a correlated SET subquery from the
// pre-statement snapshot; every case below diverged from 3.53.3 before
// engine/fk.go's fkDrain and the widened emitSetValue test.
func TestFKActionsRunPerRowAndSetSubqueriesReadLive(t *testing.T) {
	base := []string{
		`CREATE TABLE b(id INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, n INTEGER)`,
		`INSERT INTO b VALUES(1,1),(2,2),(3,3)`,
		`INSERT INTO c VALUES(1,0),(2,0),(3,0)`,
	}
	fk := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY, n)`,
		`CREATE TABLE lg(x)`,
		`INSERT INTO p VALUES(1,0),(2,0),(3,0)`,
	}
	cases := []struct {
		name  string
		pre   []string
		stmts []string
	}{
		// A SET subquery over a table the UPDATE's own triggers write.
		{"before trigger feeds the next row's SET subquery", base, []string{
			`CREATE TRIGGER tr BEFORE UPDATE ON c BEGIN INSERT INTO b VALUES(NULL, 9); END`,
			`UPDATE c SET n = (SELECT count(*) FROM b WHERE b.id >= c.id)`, `SELECT id, n FROM c ORDER BY id`}},
		{"after trigger feeds the next row's SET subquery", base, []string{
			`CREATE TRIGGER tr AFTER UPDATE ON c BEGIN INSERT INTO b VALUES(NULL, 9); END`,
			`UPDATE c SET n = (SELECT count(*) FROM b WHERE b.id >= c.id)`, `SELECT id, n FROM c ORDER BY id`}},
		{"a trigger chain feeds the next row's SET subquery", base, []string{
			`CREATE TABLE d(x)`,
			`CREATE TRIGGER t1 BEFORE UPDATE ON c BEGIN INSERT INTO d VALUES(1); END`,
			`CREATE TRIGGER t2 AFTER INSERT ON d BEGIN INSERT INTO b VALUES(NULL, 9); END`,
			`UPDATE c SET n = (SELECT count(*) FROM b WHERE b.id >= c.id)`, `SELECT id, n FROM c ORDER BY id`}},
		{"a trigger's UPDATE feeds the next row's SET subquery", base, []string{
			`CREATE TRIGGER tr AFTER UPDATE ON c BEGIN UPDATE b SET v = v + 100 WHERE id = new.id + 1; END`,
			`UPDATE c SET n = (SELECT v FROM b WHERE b.id = c.id)`, `SELECT id, n FROM c ORDER BY id`}},
		{"EXISTS in SET", base, []string{
			`CREATE TRIGGER tr BEFORE UPDATE ON c BEGIN INSERT INTO b VALUES(new.id + 10, 9); END`,
			`UPDATE c SET n = EXISTS(SELECT 1 FROM b WHERE b.id = c.id + 9)`, `SELECT id, n FROM c ORDER BY id`}},
		{"an INSTEAD OF trigger feeds the next row's SET subquery", base, []string{
			`CREATE VIEW vc AS SELECT id, n FROM c`,
			`CREATE TRIGGER tr INSTEAD OF UPDATE ON vc BEGIN INSERT INTO b VALUES(NULL, new.n); END`,
			`UPDATE vc SET n = (SELECT count(*) FROM b WHERE b.id >= vc.id)`, `SELECT id, v FROM b ORDER BY id`}},

		// FK actions, per row.
		{"ON UPDATE CASCADE feeds the next row's SET subquery", fk, []string{
			`CREATE TABLE ch(id INTEGER PRIMARY KEY, pk REFERENCES p(k) ON UPDATE CASCADE)`,
			`INSERT INTO ch VALUES(1,1),(2,2),(3,3)`,
			`UPDATE p SET k = k + 10, n = (SELECT count(*) FROM ch WHERE ch.pk > p.k)`, `SELECT k, n FROM p ORDER BY k`}},
		{"an AFTER trigger sees its row's cascade", fk, []string{
			`CREATE TABLE ch(id INTEGER PRIMARY KEY, pk REFERENCES p(k) ON UPDATE CASCADE)`,
			`INSERT INTO ch VALUES(1,1),(2,2),(3,3)`,
			`CREATE TRIGGER tp AFTER UPDATE ON p BEGIN INSERT INTO lg SELECT count(*) FROM ch WHERE pk > 10; END`,
			`UPDATE p SET k = k + 10`, `SELECT rowid, x FROM lg ORDER BY rowid`}},
		{"an AFTER DELETE trigger sees its row's cascade", fk, []string{
			`CREATE TABLE ch(id INTEGER PRIMARY KEY, pk REFERENCES p(k) ON DELETE CASCADE)`,
			`INSERT INTO ch VALUES(1,1),(2,2),(3,3)`,
			`CREATE TRIGGER tp AFTER DELETE ON p BEGIN INSERT INTO lg SELECT count(*) FROM ch; END`,
			`DELETE FROM p`, `SELECT rowid, x FROM lg ORDER BY rowid`}},
		{"a cascade's own triggers fire before the parent's AFTER trigger", fk, []string{
			`CREATE TABLE ch(id INTEGER PRIMARY KEY, pk REFERENCES p(k) ON UPDATE CASCADE)`,
			`INSERT INTO ch VALUES(1,1),(2,2),(3,3)`,
			`CREATE TRIGGER tc AFTER UPDATE ON ch BEGIN INSERT INTO lg VALUES('ch'||new.pk); END`,
			`CREATE TRIGGER tp AFTER UPDATE ON p BEGIN INSERT INTO lg VALUES('p'||new.k); END`,
			`UPDATE p SET k = k + 10`, `SELECT rowid, x FROM lg ORDER BY rowid`}},
		// The deferred counter a drained row moved goes back with the statement
		// that failed: row 1 leaves a deferred violation, row 2 fails UNIQUE,
		// and the COMMIT must see no violation (vdbeaux.c:3258-3261).
		{"an aborted statement takes its deferred FK count with it", fk, []string{
			`CREATE TABLE ch(pk REFERENCES p(k) DEFERRABLE INITIALLY DEFERRED, u UNIQUE)`,
			`BEGIN`, `INSERT INTO ch VALUES(99, 1), (98, 1)`, `COMMIT`, `SELECT count(*) FROM ch`}},

		// A trigger BODY's UPDATE/DELETE with a correlated subquery: "no such
		// column" before liveBodyRowSubSelect.
		{"a body UPDATE's correlated SET", base, []string{
			`CREATE TABLE e(x)`,
			`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN UPDATE c SET n = (SELECT count(*) FROM b WHERE b.id >= c.id + new.x); END`,
			`INSERT INTO e VALUES(1)`, `SELECT id, n FROM c ORDER BY id`}},
		{"a body UPDATE's correlated WHERE", base, []string{
			`CREATE TABLE e(x)`,
			`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN UPDATE c SET n = 5 WHERE EXISTS(SELECT 1 FROM b WHERE b.id = c.id + 1); END`,
			`INSERT INTO e VALUES(1)`, `SELECT id, n FROM c ORDER BY id`}},
		{"a body DELETE's correlated WHERE", base, []string{
			`CREATE TABLE e(x)`,
			`CREATE TRIGGER t0 AFTER INSERT ON e BEGIN DELETE FROM c WHERE EXISTS(SELECT 1 FROM b WHERE b.id = c.id + 1); END`,
			`INSERT INTO e VALUES(1)`, `SELECT id, n FROM c ORDER BY id`}},
	}
	for _, tc := range cases {
		differ(t, tc.name, append(append([]string{}, tc.pre...), tc.stmts...))
	}
}
