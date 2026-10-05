// TestFts3ModuleArgumentsMatchCSQLite gates fts3/fts4 module argument parsing.
// Arguments are processed as tokenizer spec, fts4 options, or columns.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestFts3ModuleArgumentsMatchCSQLite(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// order=asc is default and no-op; last one wins for order=desc/asc combo.
		{"order=asc is a no-op, and the last one wins", []string{
			`CREATE VIRTUAL TABLE d USING fts4(x, order=ASC)`,
			`CREATE VIRTUAL TABLE n USING fts4(x)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`INSERT INTO d VALUES('aa bb')`,
			`INSERT INTO d VALUES('bb cc')`,
			`INSERT INTO d VALUES('cc aa')`,
			`INSERT INTO n VALUES('aa bb')`,
			`INSERT INTO n VALUES('bb cc')`,
			`INSERT INTO n VALUES('cc aa')`,
			`SELECT docid FROM d`,
			`SELECT docid FROM n`,
			`SELECT docid FROM d WHERE d MATCH 'aa'`,
			`SELECT docid FROM n WHERE n MATCH 'aa'`,
			`SELECT level, idx, quote(root) FROM d_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM n_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM d_stat ORDER BY id`,
			`SELECT id, quote(value) FROM n_stat ORDER BY id`,
			`INSERT INTO d(d) VALUES('integrity-check')`,
			// Accepted spellings: case-insensitive, dequoted, last-one-wins.
			`CREATE VIRTUAL TABLE a1 USING fts4(x, order=Asc)`,
			`CREATE VIRTUAL TABLE a2 USING fts4(x, order="asc")`,
			`CREATE VIRTUAL TABLE a3 USING fts4(x, order=[asc])`,
			`CREATE VIRTUAL TABLE a4 USING fts4(x, order=desc, order=asc)`,
			`INSERT INTO a4 VALUES('p'),('q')`,
			`SELECT docid FROM a4`,
			// Refused spellings; value is not trimmed.
			`CREATE VIRTUAL TABLE b1 USING fts4(x, order=xyz)`,
			`CREATE VIRTUAL TABLE b2 USING fts4(x, order=)`,
			`CREATE VIRTUAL TABLE b3 USING fts4(x, order=ascx)`,
			`CREATE VIRTUAL TABLE b4 USING fts4(x, order=' asc')`,
			`CREATE VIRTUAL TABLE b5 USING fts4(x, order= asc)`,
			`CREATE VIRTUAL TABLE b6 USING fts4(x, order=xyz, order=asc)`,
			`CREATE VIRTUAL TABLE b7 USING fts4(x, order =asc)`,
			// fts3 has no options; this is a column.
			`CREATE VIRTUAL TABLE b8 USING fts3(x, order=asc)`,
			`SELECT sql FROM sqlite_master WHERE name='b8_content'`,
			`SELECT type, name FROM sqlite_master WHERE type='table' ORDER BY name`,
		}},
		// Option value is dequoted, not trimmed; leading space fails.
		{"an option value is dequoted, not trimmed", []string{
			`CREATE VIRTUAL TABLE p1 USING fts4(x, prefix= 2)`,
			`CREATE VIRTUAL TABLE p2 USING fts4(x, matchinfo= fts3)`,
			`CREATE VIRTUAL TABLE p3 USING fts4(x, notindexed= x)`,
			`SELECT type, name FROM sqlite_master WHERE type='table' ORDER BY name`,
			// Same values without space work fine.
			`CREATE VIRTUAL TABLE q1 USING fts4(x, prefix=2)`,
			`CREATE VIRTUAL TABLE q2 USING fts4(x, matchinfo=fts3)`,
			`CREATE VIRTUAL TABLE q3 USING fts4(x, notindexed=x)`,
			`SELECT type, name FROM sqlite_master WHERE type='table' ORDER BY name`,
			`INSERT INTO q1 VALUES('alpha')`,
			`SELECT level, idx, quote(root) FROM q1_segdir ORDER BY level, idx`,
		}},
		// Declared TYPE is discarded; columns are untyped.
		{"a column's declared type is discarded", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a TEXT, b INTEGER)`,
			`PRAGMA table_info(t)`,
			`INSERT INTO t VALUES('x y', 3)`,
			`SELECT a, b, typeof(a), typeof(b) FROM t`,
			`SELECT docid FROM t WHERE t MATCH 'x'`,
			`SELECT sql FROM sqlite_master WHERE name='t'`,
			`SELECT docid, quote(c0a), quote(c1b) FROM t_content`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
		}},
		{"only the first token of a column argument survives", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a VARCHAR(10), b)`,
			`PRAGMA table_info(t)`,
			`CREATE VIRTUAL TABLE u USING fts3(c TEXT NOT NULL DEFAULT 'x', d)`,
			`PRAGMA table_info(u)`,
			`CREATE VIRTUAL TABLE w USING fts3("a b" TEXT, c)`,
			`PRAGMA table_info(w)`,
			`INSERT INTO w VALUES('hello','there')`,
			`SELECT "a b", c FROM w`,
			`SELECT level, idx, quote(root) FROM w_segdir`,
		}},
		// fts3 has no module options; = argument is a column.
		{"fts3 treats an = argument as a column", []string{
			`CREATE VIRTUAL TABLE t USING fts3(xyz=abc)`,
			`PRAGMA table_info(t)`,
			`CREATE VIRTUAL TABLE u USING fts3(a, notindexed=a)`,
			`PRAGMA table_info(u)`,
			`CREATE VIRTUAL TABLE w USING fts3(tokenizer=simple)`,
			`PRAGMA table_info(w)`,
			`INSERT INTO u VALUES('one','two')`,
			`SELECT docid FROM u WHERE u MATCH 'two'`,
			`SELECT level, idx, quote(root) FROM u_segdir`,
		}},
		{"the simple tokenizer is the default and is accepted", []string{
			`CREATE VIRTUAL TABLE t USING fts3(tokenize=simple)`,
			`PRAGMA table_info(t)`,
			`INSERT INTO t VALUES('a b')`,
			`SELECT * FROM t`,
			`SELECT level, idx, quote(root) FROM t_segdir`,
			`CREATE VIRTUAL TABLE u USING fts3(a, tokenize=simple)`,
			`PRAGMA table_info(u)`,
			`INSERT INTO u VALUES('one two')`,
			`SELECT docid FROM u WHERE u MATCH 'two'`,
			`CREATE VIRTUAL TABLE w USING fts3(tokenize simple)`,
			`PRAGMA table_info(w)`,
			`CREATE VIRTUAL TABLE x USING fts4(tokenize=simple, a, b)`,
			`PRAGMA table_info(x)`,
			`INSERT INTO x VALUES('p q','r')`,
			`SELECT level, idx, quote(root) FROM x_segdir`,
			`SELECT id, quote(value) FROM x_stat`,
		}},
		// Unrecognized options and duplicate tokenizer are errors.
		{"an unrecognized fts4 option and a duplicate tokenizer are errors", []string{
			`CREATE VIRTUAL TABLE t USING fts4(xyz=abc)`,
			`CREATE VIRTUAL TABLE u USING fts4(xyz = abc)`,
			`CREATE VIRTUAL TABLE w USING fts4(tokenize=simple, a, tokenize=simple)`,
			`CREATE VIRTUAL TABLE x USING fts4(a, b, notindexed=nosuch)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "ftsmodargs/"+tc.name, tc.stmts) })
	}
}

// Options that stay out: content=, compress=, uncompress=, and unimplemented tokenizers.
func TestFts3ModuleOptionsDeclined(t *testing.T) {
	for _, stmt := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(content=other)`,
		`CREATE VIRTUAL TABLE t USING fts4(x, compress=zip)`,
		`CREATE VIRTUAL TABLE t USING fts4(x, uncompress=unzip)`,
		// Unimplemented tokenizers.
		`CREATE VIRTUAL TABLE t USING fts4(words, tokenize icu)`,
		`CREATE VIRTUAL TABLE t USING fts3(a, tokenize=icu)`,
		`CREATE VIRTUAL TABLE t USING fts4(tokenize=icu, x)`,
	} {
		res := run(t, "musql", []string{stmt, `SELECT count(*) FROM sqlite_master`})
		if res[0]["kind"] != "error" {
			t.Errorf("%s: expected a clean decline, got %v", stmt, res[0])
		}
		// Must not leave behind a half-created table.
		if got := res[1]; got["kind"] == "error" {
			t.Errorf("%s: the declined CREATE broke the schema: %v", stmt, got)
		}
	}
}
