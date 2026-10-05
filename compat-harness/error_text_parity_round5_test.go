package compat

// Error message parity for DDL, transactions, constraints, ATTACH, and
// compound SELECT statements.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// errorRound5Setup: schema covering constraints, STRICT, WITHOUT ROWID, etc.
var errorRound5Setup = []string{
	`PRAGMA foreign_keys = ON`,
	`CREATE TABLE par(id INTEGER PRIMARY KEY, u TEXT UNIQUE)`,
	`INSERT INTO par VALUES(1,'a')`,
	`CREATE TABLE ch(id INTEGER PRIMARY KEY, pid INTEGER, CONSTRAINT ch_par_fk FOREIGN KEY(pid) REFERENCES par(id))`,
	`CREATE TABLE ck(a INTEGER, CONSTRAINT ck_pos CHECK(a > 0))`,
	`CREATE TABLE st(x INTEGER, y TEXT NOT NULL) STRICT`,
	`CREATE TABLE wr(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
	`CREATE TABLE gc(a INTEGER, v AS (a*2), s INTEGER GENERATED ALWAYS AS (a+1) STORED)`,
	`CREATE TABLE plain(a, b)`,
	`INSERT INTO plain VALUES(1,'x'),(2,'y')`,
	`CREATE VIEW pv AS SELECT a FROM plain`,
	`CREATE TABLE nopar(id INTEGER, ref INTEGER REFERENCES ghost(id))`,
	`CREATE TABLE npk(a, b)`,
	`CREATE TABLE fkbad(a, b REFERENCES plain(a))`,
	// Rows that make the constraint cases below actually FIRE: a CHECK needs a
	// row to update, and an FK needs a child row before the parent's delete is
	// refused.
	`INSERT INTO ck VALUES(5)`,
	`INSERT INTO ch VALUES(1,1)`,
}

// TestErrorTextParityRound5 sweeps the remaining error classes.
func TestErrorTextParityRound5(t *testing.T) {
	for _, q := range []string{
		// NAMED constraints: C reports the constraint's own name for a CHECK and
		// the table.column for a FK, and neither is the generic sentence.
		`INSERT INTO ck VALUES(0)`,
		`INSERT INTO ck VALUES(-1)`,
		`UPDATE ck SET a = 0 WHERE a = 5`,
		`INSERT INTO ch VALUES(1, 99)`,
		`UPDATE ch SET pid = 99 WHERE id = 1`,
		`DELETE FROM par WHERE id = 1`,
		`DROP TABLE par`,
		`INSERT INTO par VALUES(2,'a')`,
		`INSERT INTO nopar VALUES(1,1)`,
		`INSERT INTO fkbad VALUES(1,1)`,
		// STRICT's own rules: an unknown type name at CREATE, and a value whose
		// type does not match at INSERT.
		`CREATE TABLE st2(a FUNKY) STRICT`,
		`CREATE TABLE st2(a) STRICT`,
		`INSERT INTO st VALUES('nope','y')`,
		`INSERT INTO st VALUES(1, x'00')`,
		`INSERT INTO st(x) VALUES(1)`,
		`INSERT INTO st VALUES(1.5,'y')`,
		`INSERT INTO st VALUES(x'00','y')`,
		// WITHOUT ROWID: the missing PRIMARY KEY, the rowid reference, and a NULL
		// in the key.
		`CREATE TABLE wr2(a, b) WITHOUT ROWID`,
		`SELECT rowid FROM wr`,
		`INSERT INTO wr VALUES(NULL, 1)`,
		`CREATE TABLE wr3(a PRIMARY KEY AUTOINCREMENT) WITHOUT ROWID`,
		// AUTOINCREMENT's placement rules.
		`CREATE TABLE ai(a TEXT PRIMARY KEY AUTOINCREMENT)`,
		`CREATE TABLE ai(a INTEGER, PRIMARY KEY(a AUTOINCREMENT))`,
		`CREATE TABLE ai(a INTEGER AUTOINCREMENT)`,
		`CREATE TABLE ai(a INTEGER PRIMARY KEY, b INTEGER PRIMARY KEY AUTOINCREMENT)`,
		// Generated columns: what cannot be written, and what cannot be declared.
		`INSERT INTO gc(a,v) VALUES(1,1)`,
		`INSERT INTO gc(a,s) VALUES(1,1)`,
		`UPDATE gc SET v = 1`,
		`UPDATE gc SET s = 1`,
		`ALTER TABLE gc ADD COLUMN w AS (a*3) STORED`,
		`CREATE TABLE gc2(a, b AS (nosuch))`,
		`CREATE TABLE gc2(a, b AS ((SELECT 1)))`,
		`CREATE TABLE gc2(a, b AS (random()))`,
		`CREATE TABLE gc2(a, b AS (a) PRIMARY KEY)`,
		// Duplicate and reserved names.
		`CREATE TABLE dup(a, a)`,
		`CREATE TABLE dup(a, A)`,
		`CREATE TABLE sqlite_reserved(a)`,
		`CREATE INDEX sqlite_idx ON plain(a)`,
		`CREATE VIEW pv AS SELECT 1`,
		`CREATE TRIGGER tg AFTER INSERT ON plain BEGIN SELECT 1; END`,
		`CREATE TRIGGER tg AFTER INSERT ON plain BEGIN SELECT 1; END`,
		// DEFAULT and CHECK expression rules.
		`CREATE TABLE dv(a DEFAULT (nosuch))`,
		`CREATE TABLE dv(a DEFAULT ((SELECT 1)))`,
		`CREATE TABLE cx(a CHECK((SELECT 1)))`,
		`CREATE TABLE cx(a CHECK(nosuch > 0))`,
		// Trigger rules: the event, the target, and a view without INSTEAD OF.
		`CREATE TRIGGER tg2 AFTER INSERT ON pv BEGIN SELECT 1; END`,
		`CREATE TRIGGER tg2 INSTEAD OF INSERT ON plain BEGIN SELECT 1; END`,
		`CREATE TRIGGER tg2 AFTER INSERT ON nosuch BEGIN SELECT 1; END`,
		`CREATE TRIGGER tg2 AFTER UPDATE OF nosuch ON plain BEGIN SELECT 1; END`,
		`CREATE TRIGGER tg2 BEFORE INSERT ON plain BEGIN SELECT nosuch; END`,
		`CREATE TRIGGER tg2 AFTER DELETE ON plain BEGIN SELECT NEW.a; END`,
		`CREATE TRIGGER tg2 AFTER INSERT ON plain BEGIN SELECT OLD.a; END`,
		`INSERT INTO pv VALUES(1)`,
		`UPDATE pv SET a = 1`,
		`DELETE FROM pv`,
		// Compound SELECT rules.
		`SELECT a FROM plain ORDER BY a UNION SELECT 1`,
		`SELECT a FROM plain LIMIT 1 UNION SELECT 1`,
		`SELECT a FROM plain UNION SELECT 1 ORDER BY nosuch`,
		`SELECT a FROM plain UNION SELECT 1 ORDER BY 2`,
		`SELECT a,b FROM plain INTERSECT SELECT 1`,
		`SELECT a FROM plain EXCEPT SELECT 1,2`,
		// Recursive CTE rules, each of which C spells its own way.
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r, r) SELECT * FROM r`,
		`WITH RECURSIVE r(n) AS (SELECT n FROM r) SELECT * FROM r`,
		`WITH r(a,b) AS (SELECT 1) SELECT * FROM r`,
		`WITH r AS (SELECT 1) SELECT * FROM nosuch`,
		`WITH r AS (SELECT 1), r AS (SELECT 2) SELECT * FROM r`,
		// Window misuse beyond what round 4 covered.
		`SELECT sum(a) OVER (ORDER BY a ROWS BETWEEN CURRENT ROW AND 1 PRECEDING) FROM plain`,
		`SELECT sum(a) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED FOLLOWING AND CURRENT ROW) FROM plain`,
		`SELECT sum(a) OVER (ORDER BY a ROWS -1 PRECEDING) FROM plain`,
		`SELECT sum(a) OVER (RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM plain`,
		`SELECT sum(a) OVER (ORDER BY a,b RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM plain`,
		`SELECT sum(a) OVER w FROM plain`,
		`SELECT sum(a) OVER (nosuchwin) FROM plain`,
		`SELECT lag(a) FROM plain`,
		`SELECT ntile(0) OVER (ORDER BY a) FROM plain`,
		`SELECT sum(a) FILTER (WHERE a>0) FROM plain WHERE sum(a) FILTER (WHERE a>0) > 1`,
		`SELECT abs(a) FILTER (WHERE a>0) FROM plain`,
		// Row values.
		`SELECT (1,2) FROM plain`,
		`SELECT * FROM plain WHERE (a,b) = 1`,
		`SELECT * FROM plain WHERE (a,b) IN (SELECT a FROM plain)`,
		`SELECT * FROM plain ORDER BY (a,b)`,
		// The transaction verbs.
		`COMMIT`,
		`ROLLBACK`,
		`END`,
		`ROLLBACK TO nosuch`,
		`RELEASE nosuchsp`,
		// ATTACH / DETACH.
		`DETACH nosuch`,
		`DETACH main`,
		`ATTACH DATABASE '/nonexistent-dir-xyz/q.db' AS aux`,
		`SELECT * FROM nosuchdb.t`,
		`CREATE TABLE nosuchdb.t(a)`,
		`PRAGMA nosuchdb.page_size`,
		`ALTER TABLE nosuchdb.plain RENAME TO q`,
		// The maintenance verbs.
		`ANALYZE nosuch`,
		`REINDEX nosuch`,
		`DROP TRIGGER nosuch`,
		`VACUUM nosuchdb`,
		// Qualified-name rules.
		`SELECT plain.nosuch FROM plain`,
		`SELECT nosuch.a FROM plain`,
		`SELECT main.plain.nosuch FROM plain`,
		`INSERT INTO plain(nosuch) VALUES(1)`,
		`INSERT INTO plain SELECT 1`,
		`INSERT INTO plain(a) SELECT 1,2`,
		`INSERT INTO plain VALUES(1),(1,2)`,
		`UPDATE plain SET (a,b) = (1,2,3)`,
		// Index hints.
		`SELECT * FROM plain NOT INDEXED INDEXED BY nosuch`,
		`SELECT * FROM pv INDEXED BY nosuch`,
		// A few more parse errors, away from end of input.
		`SELECT CASE END`,
		`SELECT * FROM plain GROUP BY`,
		`SELECT DISTINCT * FROM`,
		`UPDATE plain SET`,
		`DELETE plain`,
		`CREATE`,
		`CREATE TABLE`,
		`PRAGMA`,
		`WITH`,
		`VALUES`,
		// fts5 / vtab syntax, whose messages come from the module.
		`CREATE VIRTUAL TABLE ftbad USING fts5()`,
		`CREATE VIRTUAL TABLE ftbad USING nosuchmodule(a)`,
		`CREATE VIRTUAL TABLE ftbad USING fts5(a, nosuchopt=1)`,
		`CREATE VIRTUAL TABLE ftbad USING fts4(a, nosuchopt=1)`,
		`CREATE VIRTUAL TABLE ftbad USING rtree(id)`,
		`SELECT * FROM plain WHERE a MATCH 'x'`,
	} {
		q := q
		t.Run(strings.NewReplacer(" ", "_", "(", "", ")", "", "'", "", "*", "star", "/", "-").Replace(q), func(t *testing.T) {
			var msg [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range errorRound5Setup {
					db.Exec(s)
				}
				_, e := db.Exec(q)
				if e == nil {
					if rows, qerr := db.Query(q); qerr != nil {
						e = qerr
					} else if rows != nil {
						rows.Close()
					}
				}
				if e != nil {
					msg[i] = e.Error()
				}
				db.Close()
			}
			if msg[0] == "" {
				t.Fatalf("the oracle did not reject %q -- the case no longer tests an error", q)
			}
			if msg[0] != msg[1] {
				t.Errorf("%s\n  cgo: %q\n  mus: %q", q, msg[0], msg[1])
			}
		})
	}
}
