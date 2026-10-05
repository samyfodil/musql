package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// Tests that foreign key actions fire the child table's own triggers.
// FK actions are executed as DELETE/UPDATE statements, so child table
// BEFORE and AFTER triggers run once per cascaded row with appropriate
// OLD/NEW images.
func TestForeignKeyActionFiresChildTriggers(t *testing.T) {
	base := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE ch(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES par(id) ON DELETE CASCADE ON UPDATE CASCADE, note)`,
		`CREATE TABLE log(seq INTEGER PRIMARY KEY, what TEXT, a, b)`,
		`INSERT INTO par VALUES(1,'P1'),(2,'P2')`,
		`INSERT INTO ch VALUES(10,1,'c10'),(11,1,'c11'),(12,2,'c12')`,
	}
	// The one ORDERING shape that stays declined: C codes an action's own nested
	// actions BEFORE the row's AFTER trigger (sqlite3GenerateRowDelete emits
	// sqlite3FkActions ahead of the AFTER block), so a CHAIN of cascades
	// interleaves depth-first -- gc 100, ch 10, gc 101, ch 11 -- where this
	// engine drains one level at a time. The final state is identical; the
	// trigger bodies' view of it is not.
	for _, tc := range []struct {
		name          string
		trigs, run    []string
		decline       bool
	}{
		{"AFTER DELETE", []string{
			`CREATE TRIGGER ad AFTER DELETE ON ch BEGIN INSERT INTO log(what,a,b) VALUES('ad', OLD.id, OLD.pid); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"BEFORE DELETE", []string{
			`CREATE TRIGGER bd BEFORE DELETE ON ch BEGIN INSERT INTO log(what,a,b) VALUES('bd', OLD.id, OLD.pid); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"AFTER UPDATE", []string{
			`CREATE TRIGGER au AFTER UPDATE ON ch BEGIN INSERT INTO log(what,a,b) VALUES('au', OLD.pid, NEW.pid); END`},
			[]string{`UPDATE par SET id=99 WHERE id=1`}, false},
		{"BEFORE UPDATE", []string{
			`CREATE TRIGGER bu BEFORE UPDATE ON ch BEGIN INSERT INTO log(what,a,b) VALUES('bu', OLD.pid, NEW.pid); END`},
			[]string{`UPDATE par SET id=99 WHERE id=1`}, false},
		{"BEFORE DELETE that deletes the row itself", []string{
			`CREATE TRIGGER bd2 BEFORE DELETE ON ch BEGIN DELETE FROM ch WHERE id = OLD.id; INSERT INTO log(what,a,b) VALUES('bd2', OLD.id, NULL); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"BEFORE UPDATE that RAISEs", []string{
			`CREATE TRIGGER bu2 BEFORE UPDATE ON ch WHEN NEW.pid=99 BEGIN SELECT RAISE(ABORT,'no'); END`},
			[]string{`UPDATE par SET id=99 WHERE id=1`}, false},
		{"recursive_triggers OFF, AFTER DELETE writes ch", []string{
			`CREATE TRIGGER ad3 AFTER DELETE ON ch BEGIN INSERT INTO log(what,a,b) VALUES('ad3', OLD.id, (SELECT count(*) FROM ch)); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"SET NULL fires UPDATE triggers", []string{
			`CREATE TABLE ch2(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES par(id) ON DELETE SET NULL)`,
			`INSERT INTO ch2 VALUES(20,1)`,
			`CREATE TRIGGER au2 AFTER UPDATE ON ch2 BEGIN INSERT INTO log(what,a,b) VALUES('au2', OLD.pid, NEW.pid); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"SET DEFAULT fires UPDATE triggers", []string{
			`CREATE TABLE ch3(id INTEGER PRIMARY KEY, pid INTEGER DEFAULT 2 REFERENCES par(id) ON DELETE SET DEFAULT)`,
			`INSERT INTO ch3 VALUES(30,1)`,
			`CREATE TRIGGER au3 AFTER UPDATE ON ch3 BEGIN INSERT INTO log(what,a,b) VALUES('au3', OLD.pid, NEW.pid); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"a CHAIN of cascades, each with its own trigger", []string{
			`CREATE TABLE gc(id INTEGER PRIMARY KEY, cid INTEGER REFERENCES ch(id) ON DELETE CASCADE)`,
			`INSERT INTO gc VALUES(100,10),(101,11)`,
			`CREATE TRIGGER adc AFTER DELETE ON ch BEGIN INSERT INTO log(what,a,b) VALUES('ch', OLD.id, NULL); END`,
			`CREATE TRIGGER adg AFTER DELETE ON gc BEGIN INSERT INTO log(what,a,b) VALUES('gc', OLD.id, OLD.cid); END`},
			[]string{`DELETE FROM par WHERE id=1`, `SELECT count(*) FROM gc`}, true},
		{"UPDATE OF gate: a trigger naming a column the action does not write", []string{
			`CREATE TRIGGER aun AFTER UPDATE OF note ON ch BEGIN INSERT INTO log(what,a,b) VALUES('note', OLD.id, NULL); END`,
			`CREATE TRIGGER aup AFTER UPDATE OF pid ON ch BEGIN INSERT INTO log(what,a,b) VALUES('pid', OLD.id, NEW.pid); END`},
			[]string{`UPDATE par SET id=99 WHERE id=1`}, false},
		{"a WHEN clause on the action's trigger", []string{
			`CREATE TRIGGER adw AFTER DELETE ON ch WHEN OLD.id = 11 BEGIN INSERT INTO log(what,a,b) VALUES('when', OLD.id, NULL); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"the action's trigger writes the PARENT", []string{
			`CREATE TRIGGER adp AFTER DELETE ON ch BEGIN INSERT INTO log(what,a,b) VALUES('p', OLD.id, (SELECT count(*) FROM par)); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
		{"a BEFORE DELETE trigger that RAISEs IGNORE", []string{
			`CREATE TRIGGER bdi BEFORE DELETE ON ch WHEN OLD.id=10 BEGIN SELECT RAISE(IGNORE); END`},
			[]string{`DELETE FROM par WHERE id=1`}, false},
	} {
		tc := tc
		var out [2]string
		for i, drv := range []string{"sqlite3", "sqlite"} {
			db, _ := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
			db.SetMaxOpenConns(1)
			for _, s := range append(append([]string{}, base...), tc.trigs...) {
				db.Exec(s)
			}
			for _, s := range tc.run {
				out[i] += renderQuery(db, s) + " "
			}
			out[i] += "| log:" + renderQuery(db, `SELECT what,a,b FROM log ORDER BY seq`)
			out[i] += " | ch:" + renderQuery(db, `SELECT id,pid FROM ch ORDER BY id`)
			db.Close()
		}
		if tc.decline {
			if !strings.Contains(out[1], "ERR") {
				t.Errorf("%s: served now (%s) -- if the onward action is drained inline, move it to the agreeing set", tc.name, out[1])
			}
			continue
		}
		if out[0] != out[1] {
			t.Errorf("%s\n  cgo: %s\n  mus: %s", tc.name, out[0], out[1])
		}
	}
}
