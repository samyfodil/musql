package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestPlanShapeWrites: INSERT, UPDATE and DELETE statements that differ only
// in their VALUES, SET and WHERE literals run one program, each changing the
// database as its own compile does (#164).
func TestPlanShapeWrites(t *testing.T) {
	schema := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b TEXT, c REAL)`,
		`CREATE INDEX ta ON t(a)`,
		`CREATE TABLE u(k TEXT UNIQUE, v INTEGER, w TEXT COLLATE NOCASE)`,
		`CREATE TABLE p(x INTEGER, y INTEGER)`,
		`CREATE INDEX px ON p(x) WHERE y > 5`,
		`CREATE TABLE log(n INTEGER, what TEXT)`,
		`CREATE TRIGGER tl AFTER UPDATE ON t WHEN NEW.a > 40 BEGIN INSERT INTO log VALUES(NEW.id, 'big'); END`,
	}
	stmts := []struct {
		sql  string
		args []Value
	}{
		{`INSERT INTO t VALUES(10, 20, 'x', 2.5)`, nil},
		{`INSERT INTO t VALUES(11, 30, 'y', 3.5)`, nil},
		{`INSERT INTO t VALUES(12, -7, 'z', -0.5)`, nil},
		{`INSERT INTO t VALUES(13, 0x10, 'w', 1e3)`, nil},
		{`INSERT INTO t VALUES(14, 9223372036854775807, '', 7)`, nil},
		{`INSERT INTO t VALUES(15, '42', 33, '4.5')`, nil},
		{`INSERT INTO t VALUES(16, 1, 'one', 0)`, nil},
		{`INSERT INTO t VALUES(17, 2 * 3, 'six', 1.5 + 2)`, nil},
		{`INSERT INTO t VALUES(18, 4 * 3, 'twelve', 2.5 + 2)`, nil},
		{`INSERT INTO t(id, a) VALUES(20, 5), (21, 6), (22, 7)`, nil},
		{`INSERT INTO t(id, a) VALUES(23, 8), (24, 9), (25, 10)`, nil},
		{`INSERT INTO t(id, a) VALUES(26, 11) UNION ALL SELECT 27, 12`, nil},
		{`INSERT INTO t(id, a) VALUES(28, 13) UNION ALL SELECT 29, 14`, nil},
		{`INSERT INTO t VALUES(?, 50, 'p', 5.5)`, []Value{{Typ: Int, I: 30}}},
		{`INSERT INTO t VALUES(?, 60, 'q', 6.5)`, []Value{{Typ: Int, I: 31}}},
		{`INSERT INTO t VALUES(:id, 70, :b, 7.5)`, []Value{{Typ: Int, I: 32}, {Typ: Text, S: []byte("r")}}},
		{`INSERT INTO t VALUES(:id, 80, :b, 8.5)`, []Value{{Typ: Int, I: 33}, {Typ: Text, S: []byte("s")}}},
		{`INSERT INTO t VALUES(10, 99, 'dup', 0.5)`, nil},
		{`INSERT INTO t VALUES(11, 99, 'dup', 0.5)`, nil},
		{`INSERT OR REPLACE INTO t VALUES(10, 21, 'xx', 2.25)`, nil},
		{`INSERT OR REPLACE INTO t VALUES(11, 31, 'yy', 3.25)`, nil},
		{`INSERT INTO u VALUES('k1', 5, 'Abc')`, nil},
		{`INSERT INTO u VALUES('k2', 6, 'abc')`, nil},
		{`INSERT INTO u VALUES('k1', 7, 'x') ON CONFLICT(k) DO UPDATE SET v = v + excluded.v`, nil},
		{`INSERT INTO u VALUES('k2', 8, 'x') ON CONFLICT(k) DO UPDATE SET v = v + excluded.v`, nil},
		{`INSERT INTO p VALUES(3, 9)`, nil},
		{`INSERT INTO p VALUES(4, 2)`, nil},
		{`UPDATE t SET b = 'u1' WHERE id = 12`, nil},
		{`UPDATE t SET b = 'u2' WHERE id = 13`, nil},
		{`UPDATE t SET a = a + 25 WHERE a = 20`, nil},
		{`UPDATE t SET a = a + 15 WHERE a = 30`, nil},
		{`UPDATE t SET c = c * 2 WHERE a > 5 AND a < 9`, nil},
		{`UPDATE t SET c = c * 3 WHERE a > 8 AND a < 12`, nil},
		{`UPDATE t SET b = 'in' WHERE a IN (5, 7, 9)`, nil},
		{`UPDATE t SET b = 'in2' WHERE a IN (6, 8, 10)`, nil},
		{`UPDATE t SET b = 'between' WHERE a BETWEEN 11 AND 13`, nil},
		{`UPDATE t SET b = 'between2' WHERE a BETWEEN 12 AND 14`, nil},
		{`UPDATE t SET b = 'or' WHERE a = 50 OR a = 60`, nil},
		{`UPDATE t SET b = 'or2' WHERE a = 70 OR a = 70`, nil},
		{`UPDATE t SET b = 'str' WHERE b = 'p'`, nil},
		{`UPDATE t SET b = 'str2' WHERE b = 'q'`, nil},
		{`UPDATE t SET a = 1 WHERE id = 30`, nil},
		{`UPDATE t SET a = 0 WHERE id = 31`, nil},
		{`UPDATE t SET a = 2 WHERE id = ?`, []Value{{Typ: Int, I: 32}}},
		{`UPDATE t SET a = 3 WHERE id = ?`, []Value{{Typ: Int, I: 33}}},
		{`UPDATE u SET v = 50 WHERE w = 'ABC'`, nil},
		{`UPDATE u SET v = 60 WHERE w = 'abc'`, nil},
		{`UPDATE p SET x = 10 WHERE y > 6`, nil},
		{`UPDATE p SET x = 11 WHERE y > 3`, nil},
		{`UPDATE t SET (a, b) = (77, 'tuple') WHERE id = 20`, nil},
		{`UPDATE t SET (a, b) = (78, 'tuple2') WHERE id = 21`, nil},
		{`UPDATE t SET b = 'ret' WHERE id = 22 RETURNING id, b`, nil},
		{`DELETE FROM t WHERE id = 23`, nil},
		{`DELETE FROM t WHERE id = 24`, nil},
		{`DELETE FROM t WHERE a > 70 AND b <> 'zz'`, nil},
		{`DELETE FROM t WHERE a > 60 AND b <> 'yy'`, nil},
		{`DELETE FROM t WHERE a IN (SELECT x FROM p WHERE x > 9)`, nil},
		{`DELETE FROM t WHERE a IN (SELECT x FROM p WHERE x > 3)`, nil},
		{`DELETE FROM p WHERE x = 10`, nil},
		{`DELETE FROM p WHERE x = 11`, nil},
		{`DELETE FROM u WHERE k = 'k1'`, nil},
		{`DELETE FROM u WHERE k = 'k2'`, nil},
	}
	type result struct {
		affected, last int64
		err            string
		dump           string
	}
	run := func(shapes bool) []result {
		planShapesOffForTest = !shapes
		defer func() { planShapesOffForTest = false }()
		db, err := OpenOrCreate(filepath.Join(t.TempDir(), "w.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range schema {
			if _, _, err := db.ExecArgs(s, nil); err != nil {
				t.Fatal(err)
			}
		}
		var out []result
		for _, s := range stmts {
			var r result
			var err error
			r.affected, r.last, err = db.ExecArgs(s.sql, s.args)
			if err != nil {
				r.err = err.Error()
			}
			for _, tbl := range []string{"t", "u", "p", "log"} {
				_, rows, err := db.Query(`SELECT * FROM `+tbl+` ORDER BY rowid`, nil)
				if err != nil {
					t.Fatal(err)
				}
				r.dump += fmt.Sprintf("%s=%v;", tbl, rows)
			}
			out = append(out, r)
		}
		return out
	}
	off := run(false)
	before := planShapeHits.Load()
	on := run(true)
	hits := planShapeHits.Load() - before
	for i := range stmts {
		if off[i] != on[i] {
			t.Errorf("%s:\n shaped   %+v\n unshaped %+v", stmts[i].sql, on[i], off[i])
		}
	}
	if hits < 20 {
		t.Errorf("shaped writes ran %d programs of a shape, want at least 20", hits)
	}
}

// TestPlanShapeWritePattern: which literals a write's shape makes parameters.
func TestPlanShapeWritePattern(t *testing.T) {
	db, err := OpenOrCreate(filepath.Join(t.TempDir(), "w.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b TEXT)`,
		`CREATE TABLE c(k TEXT COLLATE NOCASE, v INTEGER)`,
		`CREATE TABLE p(x INTEGER, y INTEGER)`,
		`CREATE INDEX px ON p(x) WHERE y > 5`,
		`CREATE VIEW vt AS SELECT * FROM t`,
	} {
		if _, _, err := db.ExecArgs(s, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ sql, pattern string }{
		{`INSERT INTO t VALUES(5, 20, 'x')`, "111"},
		{`INSERT INTO t VALUES(5, 1, 'x')`, "11"},
		{`INSERT INTO t VALUES(5, 20 + 3, 'x' || 'y')`, "11100"},
		{`INSERT INTO t VALUES(5, -20, (7))`, "110"},
		{`INSERT INTO t SELECT 5, 20, 'x'`, ""},
		{`INSERT INTO t VALUES(5, 20, 'x') RETURNING id`, ""},
		{`WITH w AS (SELECT 2) INSERT INTO t VALUES(5, 20, 'x')`, ""},
		{`INSERT INTO t VALUES(5, 20, 'x') ON CONFLICT(id) DO UPDATE SET a = 30`, "1110"},
		{`INSERT INTO vt VALUES(5, 20, 'x')`, ""},
		{`UPDATE t SET a = 20, b = 'x' WHERE id = 5 AND b LIKE 'y%'`, "1110"},
		{`UPDATE t SET a = (SELECT 20) WHERE a IN (5, 6) AND b IN ('p')`, "0110"},
		{`UPDATE t SET (a, b) = (20, 'x') WHERE id = 5`, "001"},
		{`UPDATE t SET a = 20 FROM p WHERE p.x = 5`, ""},
		{`UPDATE c SET v = 20 WHERE k = 'x'`, ""},
		{`UPDATE c SET k = 'x' WHERE v = 20`, "11"},
		{`UPDATE p SET y = 20 WHERE x = 5`, ""},
		{`DELETE FROM t WHERE id = 5 OR a BETWEEN 20 AND 30`, "111"},
		{`DELETE FROM t WHERE a = 5 RETURNING *`, ""},
		{`DELETE FROM t WHERE (SELECT 5) = a`, ""},
	} {
		toks, err := lex(tc.sql)
		if err != nil {
			t.Fatal(err)
		}
		lits := literalTokens(toks)
		pattern := ""
		if _, sh := db.parseWriteShape(tc.sql, toks, lits); sh != nil {
			pattern = sh.pattern
		}
		if pattern != tc.pattern {
			t.Errorf("%s: pattern %q, want %q", tc.sql, pattern, tc.pattern)
		}
	}
}
