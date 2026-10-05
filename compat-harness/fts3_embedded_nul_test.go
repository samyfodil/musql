// Tests how fts3/fts4 handle embedded NUL bytes: they stop tokenization at
// the NUL but keep the full value in content.
package compat

import "testing"

func TestFts3EmbeddedNulDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"the simple tokenizer stops at the NUL", []string{
			`CREATE VIRTUAL TABLE n USING fts4(a, b)`,
			`INSERT INTO n VALUES('abc' || char(0) || 'def', 'xx' || char(0) || 'yy zz')`,
			`SELECT hex(CAST(a AS blob)), hex(CAST(b AS blob)) FROM n`,
			`SELECT level, idx, quote(root) FROM n_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM n_stat`,
			`SELECT docid, quote(size) FROM n_docsize`,
			`SELECT docid FROM n WHERE n MATCH 'abc'`,
			`SELECT docid FROM n WHERE n MATCH 'def'`,
			`SELECT docid FROM n WHERE n MATCH 'zz'`,
			`INSERT INTO n(n) VALUES('integrity-check')`,
		}},
		{"fts3 too, and through an fts4aux listing", []string{
			`CREATE VIRTUAL TABLE n USING fts3(a)`,
			`INSERT INTO n VALUES('abc' || char(0) || 'def')`,
			`CREATE VIRTUAL TABLE na USING fts4aux(n)`,
			`SELECT * FROM na`,
			`SELECT level, idx, quote(root) FROM n_segdir`,
			`INSERT INTO n(n) VALUES('integrity-check')`,
		}},
		{"unicode61 and porter stop there too", []string{
			`CREATE VIRTUAL TABLE u USING fts4(a, tokenize=unicode61)`,
			`CREATE VIRTUAL TABLE p USING fts4(a, tokenize=porter)`,
			`INSERT INTO u VALUES('abc' || char(0) || 'def')`,
			`INSERT INTO p VALUES('running' || char(0) || 'gardens')`,
			`SELECT level, idx, quote(root) FROM u_segdir`,
			`SELECT level, idx, quote(root) FROM p_segdir`,
			`SELECT id, quote(value) FROM u_stat`,
			`SELECT id, quote(value) FROM p_stat`,
			`SELECT docid, quote(size) FROM u_docsize`,
			`CREATE VIRTUAL TABLE ua USING fts4aux(u)`,
			`SELECT * FROM ua`,
			`INSERT INTO u(u) VALUES('integrity-check')`,
			`INSERT INTO p(p) VALUES('integrity-check')`,
		}},
		// A leading NUL indexes nothing, and DELETE must retire matching terms.
		{"a leading NUL, and the DELETE markers", []string{
			`CREATE VIRTUAL TABLE n USING fts4(a)`,
			`INSERT INTO n VALUES(char(0) || 'nothing here')`,
			`INSERT INTO n VALUES('one two' || char(0) || 'three four')`,
			`INSERT INTO n VALUES('keeper')`,
			`SELECT level, idx, quote(root) FROM n_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM n_stat`,
			`SELECT docid, quote(size) FROM n_docsize ORDER BY docid`,
			`DELETE FROM n WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM n_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM n_stat`,
			`INSERT INTO n(n) VALUES('integrity-check')`,
			`INSERT INTO n(n) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM n_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM n_stat`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}
