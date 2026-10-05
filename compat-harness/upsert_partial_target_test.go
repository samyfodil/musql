package compat

// Tests ON CONFLICT matching against partial UNIQUE indexes.
import "testing"

func TestUpsertPartialTarget(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"basic-do-update", []string{
			"CREATE TABLE t1(a INTEGER, b INTEGER, c TEXT)",
			"CREATE UNIQUE INDEX t1x ON t1(a) WHERE b>0",
			"INSERT INTO t1 VALUES(1,1,'one'),(1,-1,'neg'),(1,-2,'neg2')",
			"INSERT INTO t1(a,b,c) VALUES(1,5,'new') ON CONFLICT(a) WHERE b>0 DO UPDATE SET c=excluded.c",
			"SELECT * FROM t1 ORDER BY rowid",
		}},
		{"do-nothing", []string{
			"CREATE TABLE t2(a INTEGER, b INTEGER, c TEXT)",
			"CREATE UNIQUE INDEX t2x ON t2(a) WHERE b>0",
			"INSERT INTO t2 VALUES(1,1,'one')",
			"INSERT INTO t2(a,b,c) VALUES(1,9,'ignored') ON CONFLICT(a) WHERE b>0 DO NOTHING",
			"SELECT * FROM t2 ORDER BY rowid",
		}},
		{"where-less-target-does-not-match-partial", []string{
			"CREATE TABLE t3(a INTEGER, b INTEGER, c TEXT)",
			"CREATE UNIQUE INDEX t3x ON t3(a) WHERE b>0",
			"INSERT INTO t3 VALUES(1,1,'one')",
			"INSERT INTO t3(a,b,c) VALUES(1,5,'new') ON CONFLICT(a) DO UPDATE SET c=excluded.c",
		}},
		{"mismatched-where-does-not-match", []string{
			"CREATE TABLE t4(a INTEGER, b INTEGER, c TEXT)",
			"CREATE UNIQUE INDEX t4x ON t4(a) WHERE b>0",
			"INSERT INTO t4 VALUES(1,1,'one')",
			"INSERT INTO t4(a,b,c) VALUES(1,5,'new') ON CONFLICT(a) WHERE b>10 DO UPDATE SET c=excluded.c",
		}},
		{"multi-col-partial-collate", []string{
			"CREATE TABLE t5(a TEXT, b INTEGER, c INTEGER, d TEXT)",
			"CREATE UNIQUE INDEX t5x ON t5(a, b) WHERE c>0",
			"INSERT INTO t5 VALUES('X',1,1,'one')",
			"INSERT INTO t5(a,b,c,d) VALUES('x',1,5,'new') ON CONFLICT(a COLLATE nocase, b) WHERE c>0 DO UPDATE SET d=excluded.d",
			"SELECT * FROM t5 ORDER BY rowid",
		}},
		{"vdbe-fast-path-insert-only", []string{
			"CREATE TABLE t6(a INTEGER, b INTEGER)",
			"CREATE UNIQUE INDEX t6x ON t6(a) WHERE b>0",
			"INSERT INTO t6(a,b) VALUES(1,5) ON CONFLICT(a) WHERE b>0 DO UPDATE SET b=excluded.b",
			"INSERT INTO t6(a,b) VALUES(1,9) ON CONFLICT(a) WHERE b>0 DO UPDATE SET b=excluded.b",
			"SELECT * FROM t6 ORDER BY rowid",
		}},
		// upsert4.test 2.1.2.{6,7,9}: a WHERE-carrying target against a
		// NON-partial UNIQUE(d,c,b) still matches by column set alone; the
		// target's WHERE value (a!=0 vs b==45) is irrelevant to a non-partial
		// candidate, and a target naming the wrong SET of columns (d,c,c --
		// a repeat, missing b) still correctly fails to match.
		{"where-target-matches-nonpartial-index", []string{
			"CREATE TABLE xyz(a INTEGER PRIMARY KEY, b, c, d)",
			"CREATE UNIQUE INDEX xyz1 ON xyz(d, c, b COLLATE nocase)",
			"INSERT INTO xyz VALUES(10, 1, 1, 'one')",
			"INSERT INTO xyz VALUES(11, 1, 1, 'one') ON CONFLICT (b, c, d) WHERE a!=0 DO NOTHING",
			"INSERT INTO xyz VALUES(12, 1, 1, 'one') ON CONFLICT (b, c, d) WHERE b==45 DO NOTHING",
			"SELECT * FROM xyz",
		}},
		{"where-target-wrong-column-set-still-fails", []string{
			"CREATE TABLE xyz2(a INTEGER PRIMARY KEY, b, c, d)",
			"CREATE UNIQUE INDEX xyz2x ON xyz2(d, c, b COLLATE nocase)",
			"INSERT INTO xyz2 VALUES(10, 1, 1, 'one')",
			"INSERT INTO xyz2 VALUES(11, 1, 1, 'one') ON CONFLICT (d, c, c) WHERE a!=0 DO NOTHING",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
