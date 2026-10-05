// This file gates fts3/fts4's "tokenize=unicode61" against C SQLite.
// A tokenizer decides which byte strings reach the index, so changes to
// tokenization are a file format question: the %_segdir dump must match.
package compat

import (
	"fmt"
	"strings"
	"testing"
)

// fts3CodepointDoc holds "a<cp>b" for thousands of codepoints to test the
// token-character test and the folding table.
func fts3CodepointDoc() string {
	ranges := [][2]int{{0x21, 0x300}, {0x300, 0x400}, {0x400, 0x530},
		{0x1e00, 0x1f00}, {0x2000, 0x2100}, {0x2100, 0x2200}, {0x3000, 0x3100},
		{0x4e00, 0x4e40}, {0xff00, 0xff70}, {0x10000, 0x10040}, {0x1d400, 0x1d420}}
	var b strings.Builder
	for _, r := range ranges {
		for c := r[0]; c < r[1]; c++ {
			if c == ' ' || c == '\'' {
				continue
			}
			fmt.Fprintf(&b, "a%cb ", rune(c))
		}
	}
	return b.String()
}

func TestFts3Unicode61Diff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"unicode61 folds case and strips diacritics", []string{
			`CREATE VIRTUAL TABLE u4 USING fts4(x, tokenize=unicode61)`,
			`CREATE VIRTUAL TABLE u3 USING fts3(x, tokenize=unicode61)`,
			`INSERT INTO u4 VALUES('café CAFÉ Ω İ ǅ ﬁ Ünïcödé')`,
			`INSERT INTO u3 VALUES('café CAFÉ Ω İ ǅ ﬁ Ünïcödé')`,
			`SELECT level, idx, quote(root) FROM u4_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM u3_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM u4_stat`,
			`SELECT docid, quote(size) FROM u4_docsize`,
			`SELECT docid FROM u4 WHERE u4 MATCH 'cafe'`,
			`SELECT docid FROM u4 WHERE u4 MATCH 'CAFE'`,
			`SELECT docid FROM u4 WHERE u4 MATCH 'café'`,
			`SELECT docid FROM u4 WHERE u4 MATCH 'unicode'`,
			`SELECT docid FROM u3 WHERE u3 MATCH 'cafe'`,
			`INSERT INTO u4(u4) VALUES('integrity-check')`,
			`INSERT INTO u3(u3) VALUES('integrity-check')`,
		}},
		{"unicode61 against the default simple tokenizer", []string{
			`CREATE VIRTUAL TABLE us USING fts4(x)`,
			`CREATE VIRTUAL TABLE uu USING fts4(x, tokenize=unicode61)`,
			`INSERT INTO us VALUES('Ünïcödé Δοκιμή 中文 a1b2')`,
			`INSERT INTO uu VALUES('Ünïcödé Δοκιμή 中文 a1b2')`,
			`SELECT quote(root) FROM us_segdir`,
			`SELECT quote(root) FROM uu_segdir`,
			`SELECT docid FROM uu WHERE uu MATCH '中'`,
			`SELECT docid FROM us WHERE us MATCH '中'`,
			`SELECT offsets(uu) FROM uu WHERE uu MATCH 'unicode'`,
			`SELECT snippet(uu) FROM uu WHERE uu MATCH 'dokime'`,
		}},
		{"remove_diacritics 0, 1 and 2", []string{
			`CREATE VIRTUAL TABLE d0 USING fts4(x, tokenize=unicode61 "remove_diacritics=0")`,
			`CREATE VIRTUAL TABLE d1 USING fts4(x, tokenize=unicode61 'remove_diacritics=1')`,
			`CREATE VIRTUAL TABLE d2 USING fts4(x, tokenize=unicode61 "remove_diacritics=2")`,
			`CREATE VIRTUAL TABLE d3 USING fts4(x, tokenize=unicode61 "remove_diacritics=3")`,
			`SELECT type, name FROM sqlite_master WHERE name IN ('d0','d1','d2','d3') ORDER BY name`,
			`INSERT INTO d0 VALUES('café CAFÉ İ ǅ ﬁ')`,
			`INSERT INTO d1 VALUES('café CAFÉ İ ǅ ﬁ')`,
			`INSERT INTO d2 VALUES('café CAFÉ İ ǅ ﬁ')`,
			`SELECT quote(root) FROM d0_segdir`,
			`SELECT quote(root) FROM d1_segdir`,
			`SELECT quote(root) FROM d2_segdir`,
			`SELECT docid FROM d0 WHERE d0 MATCH 'cafe'`,
			`SELECT docid FROM d0 WHERE d0 MATCH 'café'`,
			`SELECT docid FROM d1 WHERE d1 MATCH 'cafe'`,
			`SELECT docid FROM d1 WHERE d1 MATCH 'café'`,
		}},
		{"tokenchars and separators", []string{
			`CREATE VIRTUAL TABLE c1 USING fts4(x, tokenize=unicode61 "tokenchars=. ")`,
			`CREATE VIRTUAL TABLE c2 USING fts4(x, tokenize=unicode61 "separators=ab")`,
			`CREATE VIRTUAL TABLE c3 USING fts4(x, tokenize=unicode61 [tokenchars= .])`,
			`CREATE VIRTUAL TABLE c4 USING fts4(x, tokenize=unicode61 'tokenchars=_-')`,
			`INSERT INTO c1 VALUES('a.b c d')`,
			`INSERT INTO c2 VALUES('a.b cad BAD')`,
			`INSERT INTO c3 VALUES('a.b c d')`,
			`INSERT INTO c4 VALUES('one_two three-four five')`,
			`SELECT quote(root) FROM c1_segdir`,
			`SELECT quote(root) FROM c2_segdir`,
			`SELECT quote(root) FROM c3_segdir`,
			`SELECT quote(root) FROM c4_segdir`,
			`SELECT docid FROM c4 WHERE c4 MATCH 'one_two'`,
			`SELECT docid FROM c4 WHERE c4 MATCH 'one'`,
			`SELECT docid FROM c2 WHERE c2 MATCH 'bad'`,
			`SELECT docid FROM c2 WHERE c2 MATCH 'cad'`,
			`INSERT INTO c1(c1) VALUES('integrity-check')`,
			`INSERT INTO c4(c4) VALUES('integrity-check')`,
		}},
		{"tokenchars cannot undo separators", []string{
			`CREATE VIRTUAL TABLE e1 USING fts4(x, tokenize=unicode61 "separators=a" "tokenchars=a")`,
			`CREATE VIRTUAL TABLE e2 USING fts4(x, tokenize=unicode61 "tokenchars=a" "separators=a")`,
			`INSERT INTO e1 VALUES('cad BAD')`,
			`INSERT INTO e2 VALUES('cad BAD')`,
			`SELECT quote(root) FROM e1_segdir`,
			`SELECT quote(root) FROM e2_segdir`,
			`SELECT docid FROM e1 WHERE e1 MATCH 'cad'`,
			`SELECT docid FROM e2 WHERE e2 MATCH 'c'`,
		}},
		{"an argument asking for the default is a no-op", []string{
			`CREATE VIRTUAL TABLE n1 USING fts4(x, tokenize=unicode61 "separators=.")`,
			`CREATE VIRTUAL TABLE n2 USING fts4(x, tokenize=unicode61 "tokenchars=a")`,
			`CREATE VIRTUAL TABLE n3 USING fts4(x, tokenize=unicode61 "separators=.-" "tokenchars=-")`,
			`INSERT INTO n1 VALUES('a.b c-d')`,
			`INSERT INTO n2 VALUES('a.b c-d')`,
			`INSERT INTO n3 VALUES('a.b c-d')`,
			`SELECT quote(root) FROM n1_segdir`,
			`SELECT quote(root) FROM n2_segdir`,
			`SELECT quote(root) FROM n3_segdir`,
			`SELECT docid FROM n1 WHERE n1 MATCH 'a'`,
			`SELECT docid FROM n3 WHERE n3 MATCH 'c-d'`,
		}},
		{"refused tokenizer spellings", []string{
			`CREATE VIRTUAL TABLE r1 USING fts4(x, tokenize=unicode61 bogus)`,
			`CREATE VIRTUAL TABLE r2 USING fts4(x, tokenize=unicode61 tokenchars= .)`,
			`CREATE VIRTUAL TABLE r3 USING fts4(x, tokenize=unicode61 "categories=L*")`,
			`CREATE VIRTUAL TABLE r4 USING fts4(x, tokenize=unicode61"")`,
			`CREATE VIRTUAL TABLE r5 USING fts4(x, tokenize=nosuchtokenizer)`,
			`CREATE VIRTUAL TABLE r6 USING fts4(x, tokenize=unicode61 "remove_diacritics=")`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		{"the write path uses it everywhere", []string{
			`CREATE VIRTUAL TABLE u USING fts4(a, b, tokenize=unicode61)`,
			`INSERT INTO u VALUES('Café Noir','Ünïcödé Test')`,
			`INSERT INTO u VALUES('Δοκιμή','plain')`,
			`INSERT INTO u VALUES('third row','more text')`,
			`DELETE FROM u WHERE docid=1`,
			`SELECT level, idx, quote(root) FROM u_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM u_stat`,
			`UPDATE u SET a='Ünïcödé Again' WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM u_segdir ORDER BY level, idx`,
			`SELECT docid FROM u WHERE u MATCH 'unicode'`,
			`INSERT INTO u(u) VALUES('integrity-check')`,
			`INSERT INTO u(u) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM u_segdir ORDER BY level, idx`,
			`INSERT INTO u(u) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM u_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM u_stat`,
			`INSERT INTO u(u) VALUES('integrity-check')`,
		}},
		{"unicode61 with prefix indexes", []string{
			`CREATE VIRTUAL TABLE up USING fts4(x, tokenize=unicode61, prefix="2,3")`,
			`INSERT INTO up VALUES('Ünïcödé Café')`,
			`SELECT level, idx, quote(root) FROM up_segdir ORDER BY level, idx`,
			`SELECT docid FROM up WHERE up MATCH 'un*'`,
			`INSERT INTO up(up) VALUES('integrity-check')`,
		}},
		{"fts4aux over a unicode61 table", []string{
			`CREATE VIRTUAL TABLE u USING fts4(x, tokenize=unicode61)`,
			`INSERT INTO u VALUES('Ünïcödé CAFÉ café ΔΟΚΙΜΉ')`,
			`INSERT INTO u VALUES('café again')`,
			`CREATE VIRTUAL TABLE ua USING fts4aux(u)`,
			`SELECT term, col, documents, occurrences FROM ua`,
			`SELECT hex(term) FROM ua WHERE col='*'`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}

	doc := fts3CodepointDoc()
	differ(t, "every codepoint of a broad sample", []string{
		`CREATE VIRTUAL TABLE cp USING fts4(x, tokenize=unicode61)`,
		`CREATE VIRTUAL TABLE cp0 USING fts4(x, tokenize=unicode61 "remove_diacritics=0")`,
		`INSERT INTO cp VALUES('` + doc + `')`,
		`INSERT INTO cp0 VALUES('` + doc + `')`,
		`CREATE VIRTUAL TABLE cpa USING fts4aux(cp)`,
		`CREATE VIRTUAL TABLE cp0a USING fts4aux(cp0)`,
		`SELECT count(*) FROM cpa WHERE col='*'`,
		`SELECT count(*) FROM cp0a WHERE col='*'`,
		`SELECT group_concat(hex(term)) FROM cpa WHERE col='*'`,
		`SELECT group_concat(hex(term)) FROM cp0a WHERE col='*'`,
		`INSERT INTO cp(cp) VALUES('integrity-check')`,
		`INSERT INTO cp0(cp0) VALUES('integrity-check')`,
	})
}
