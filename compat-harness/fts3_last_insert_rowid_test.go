// fts3/fts4 write: last_insert_rowid() reflects docid for INSERT only.
package compat

import "testing"

// Interleave ordinary-table inserts with fts to detect counter stoppage.
func TestFts3LastInsertRowidDiff(t *testing.T) {
	for _, mod := range []string{"fts3", "fts4"} {
		cases := []struct {
			name  string
			stmts []string
		}{
			// fts4lastrowid.test 1.0-1.6, which is the corpus's own statement
			// of this rule: implicit docids, an explicit NEGATIVE one, a
			// multi-row VALUES list read inside the transaction that wrote it,
			// and both "INSERT ... SELECT" spellings.
			{"fts4lastrowid.test", []string{
				`CREATE VIRTUAL TABLE t1 USING ` + mod + `(str)`,
				`INSERT INTO t1 VALUES('one string')`,
				`INSERT INTO t1 VALUES('two string')`,
				`INSERT INTO t1 VALUES('three string')`,
				`SELECT last_insert_rowid()`,
				`BEGIN`,
				`INSERT INTO t1 VALUES('one string')`,
				`INSERT INTO t1 VALUES('two string')`,
				`INSERT INTO t1 VALUES('three string')`,
				`COMMIT`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO t1(rowid, str) VALUES(-22, 'some more text')`,
				`SELECT last_insert_rowid()`,
				`BEGIN`,
				`INSERT INTO t1(rowid, str) VALUES(45, 'some more text')`,
				`INSERT INTO t1(rowid, str) VALUES(46, 'some more text')`,
				`INSERT INTO t1(rowid, str) VALUES(222, 'some more text')`,
				`SELECT last_insert_rowid()`,
				`COMMIT`,
				`SELECT last_insert_rowid()`,
				`CREATE TABLE x1(x)`,
				`INSERT INTO x1 VALUES('john'), ('paul'), ('george'), ('ringo')`,
				`INSERT INTO t1 SELECT x FROM x1`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO t1(rowid, str) SELECT rowid+10, x FROM x1`,
				`SELECT last_insert_rowid()`,
				`SELECT docid, str FROM t1 ORDER BY docid`,
			}},
			// fts3e.test's idiom, and the reason CREATE VIRTUAL TABLE stopped
			// marking the counter opaque for this module family: the docid IS
			// last_insert_rowid(), read across an fts table's creation and out
			// of an ORDINARY table's insert.
			{"docid is last_insert_rowid()", []string{
				`CREATE VIRTUAL TABLE t1 USING ` + mod + `(c)`,
				`CREATE TABLE t2(id INTEGER PRIMARY KEY AUTOINCREMENT, weight INTEGER UNIQUE)`,
				`INSERT INTO t2 VALUES (null, 10)`,
				`INSERT INTO t1 (docid, c) VALUES (last_insert_rowid(), 'This is a test')`,
				`INSERT INTO t2 VALUES (null, 5)`,
				`INSERT INTO t1 (docid, c) VALUES (last_insert_rowid(), 'That was a test')`,
				`INSERT INTO t2 VALUES (null, 20)`,
				`INSERT INTO t1 (docid, c) VALUES (last_insert_rowid(), 'This is a test')`,
				`SELECT docid FROM t1 ORDER BY docid`,
				`SELECT docid, weight FROM t1, t2 WHERE t2.id = t1.docid ORDER BY weight`,
				`SELECT docid FROM t1 WHERE t1 MATCH 'this' ORDER BY docid`,
				`SELECT last_insert_rowid()`,
			}},
			// The counter is CONNECTION state, so a ROLLBACK does not restore
			// it -- and an fts insert re-opens it after an ordinary one, and
			// vice versa, in either order.
			{"survives rollback and interleaves with ordinary writes", []string{
				`CREATE TABLE base(x)`,
				`CREATE VIRTUAL TABLE t1 USING ` + mod + `(a)`,
				`INSERT INTO base VALUES(1),(2),(3)`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO t1(docid,a) VALUES(9,'x')`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO base VALUES(4)`,
				`SELECT last_insert_rowid()`,
				`BEGIN`,
				`INSERT INTO t1 VALUES('two')`,
				`SELECT last_insert_rowid()`,
				`ROLLBACK`,
				`SELECT last_insert_rowid()`,
				`SELECT docid FROM t1 ORDER BY docid`,
			}},
			// A statement that stored NO row leaves the counter exactly as it
			// was: C SQLite's OP_VUpdate never runs, so there is nothing to
			// publish -- not a zero, and not a decline either.
			{"a zero-row INSERT..SELECT leaves it alone", []string{
				`CREATE TABLE s(x)`,
				`INSERT INTO s VALUES('a')`,
				`CREATE VIRTUAL TABLE t1 USING ` + mod + `(a)`,
				`INSERT INTO t1 SELECT x FROM s WHERE 0`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO t1 SELECT x FROM s`,
				`SELECT last_insert_rowid()`,
			}},
			// The COMMAND CHANNEL stores no row either, but it does run
			// OP_VUpdate -- which publishes the zero it initialized *pRowid to
			// (sqlite3-binding.c, "sqlite_int64 rowid = 0"). So a command
			// RESETS the counter rather than leaving it.
			{"a command resets it to zero", []string{
				`CREATE VIRTUAL TABLE t1 USING ` + mod + `(a)`,
				`INSERT INTO t1 VALUES('one two')`,
				`INSERT INTO t1 VALUES('three four')`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO t1(t1) VALUES('optimize')`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO t1 VALUES('five')`,
				`SELECT last_insert_rowid()`,
				`INSERT INTO t1(t1) VALUES('rebuild')`,
				`SELECT last_insert_rowid()`,
				`SELECT docid FROM t1 WHERE t1 MATCH 'five'`,
			}},
			// Creating (and dropping) an fts3-family table moves NEITHER
			// counter: its xCreate writes only DDL. fts4aux and fts3tokenize
			// are in the family for the same reason.
			{"creating one moves neither counter", []string{
				`CREATE TABLE base(x)`,
				`INSERT INTO base VALUES(1),(2),(3),(4),(5),(6),(7)`,
				`CREATE VIRTUAL TABLE f USING ` + mod + `(a)`,
				`CREATE VIRTUAL TABLE fx USING fts4aux('f')`,
				`CREATE VIRTUAL TABLE ftk USING fts3tokenize('simple')`,
				`SELECT last_insert_rowid(), total_changes()`,
				`DROP TABLE fx`,
				`SELECT last_insert_rowid(), total_changes()`,
			}},
			// A schema-qualified target is the same statement.
			{"schema-qualified", []string{
				`CREATE TABLE base(x)`,
				`INSERT INTO base VALUES(1),(2)`,
				`CREATE VIRTUAL TABLE main.t1 USING ` + mod + `(a)`,
				`INSERT INTO main.t1(docid,a) VALUES(33,'hi')`,
				`SELECT last_insert_rowid()`,
			}},
		}
		for _, c := range cases {
			t.Run(mod+"/"+c.name, func(t *testing.T) { differ(t, mod+"/"+c.name, c.stmts) })
		}
	}
}

// TestFts3LastInsertRowidDeclined pins the writes whose effect on the counter
// is still module internals this engine does not reproduce. Each case first
// asserts its SETUP ran -- a decline gate whose setup failed would "pass" for
// the wrong reason -- and then that the read is refused.
//
// The oracle values these declines stand in for, all measured on
// mattn/go-sqlite3 3.53.3 over a session whose counter read 14 beforehand:
//
//	fts3 DELETE                 -> 14 (unchanged)
//	fts4 DELETE                 ->  0 (its %_stat row has rowid 0)
//	fts3 UPDATE                 -> the updated docid
//	fts4 UPDATE                 ->  0
//	a part-way failed INSERT    -> the last row it managed to store
//
// -- five different answers for four spellings of "the module's own shadow SQL
// was the last thing to insert something", which is exactly the guesswork
// conn_state.go exists to refuse.
func TestFts3LastInsertRowidDeclined(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		read  string
	}{
		{"after an fts3 DELETE", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(docid,a) VALUES(9,'x')`,
			`DELETE FROM t WHERE docid=9`,
		}, `SELECT last_insert_rowid()`},
		{"after an fts4 DELETE", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(9,'x')`,
			`DELETE FROM t WHERE docid=9`,
		}, `SELECT last_insert_rowid()`},
		{"after an fts4 UPDATE", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(9,'x')`,
			`UPDATE t SET a='y' WHERE docid=9`,
		}, `SELECT last_insert_rowid()`},
		// total_changes() is the counter that stays opaque either way: one
		// fts3 insert moves it by 3 and one fts4 insert by 5, both pure
		// shadow-table bookkeeping.
		{"total_changes() after an fts4 INSERT", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(9,'x')`,
		}, `SELECT total_changes()`},
		// rtree's xCreate ALONE moves both counters (last_insert_rowid()==1,
		// total_changes()+1) before a single row is written.
		{"after creating an rtree table", []string{
			`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
		}, `SELECT last_insert_rowid()`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openMusqlFts(t)
			for _, s := range c.setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q failed -- this gate would otherwise pass vacuously: %v", s, err)
				}
			}
			if err := db.QueryRow(c.read).Scan(new(any)); err == nil {
				t.Fatalf("engine ANSWERED %q after %v; C SQLite reports its module's own shadow-table rowid there, which this engine does not reproduce", c.read, c.setup)
			}
		})
	}
}
