//go:build sqlite_fts5

// Tests for the FTS5 Porter stemmer, comparing against C SQLite for correctness.
package compat

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// fts5R30PorterSpecs lists tokenize= spellings for the stemmer.
var fts5R30PorterSpecs = []string{
	`tokenize=porter`,
	`tokenize=PORTER`,
	`tokenize='porter'`,
	`tokenize="porter"`,
	`tokenize=[porter]`,
	`tokenize='porter porter'`,
	`tokenize='porter unicode61'`,
	`tokenize='porter unicode61 remove_diacritics 0'`,
	`tokenize='porter unicode61 remove_diacritics 2'`,
	`tokenize='porter unicode61 tokenchars ''_-.'''`,
	`tokenize='porter unicode61 categories ''L*'''`,
	`tokenize='porter ascii'`,
	`tokenize='porter ascii tokenchars ''_-.'''`,
	`tokenize='porter porter ascii'`,
	`tokenize='porter trigram'`,
	`tokenize=porter, prefix=2`,
	`tokenize='porter ascii', prefix='1 3'`,
	`tokenize=porter, columnsize=0`,
}

// fts5R30PorterStems is used with fts5R30PorterSuffixes to build a word corpus.
var fts5R30PorterStems = []string{
	"", "a", "b", "ab", "ba", "abo", "rel", "cons", "gener", "arch", "troubl",
	"electric", "feudal", "formal", "digit", "hesit", "sensit", "sensibil",
	"radic", "vietnam", "predic", "callous", "good", "tree", "y", "sky",
	"happ", "cri", "meet", "agree", "feed", "plaster", "bled", "motor", "siz",
	"hop", "hopp", "fall", "fil", "roll", "controll", "abcd", "xyzzy",
}

var fts5R30PorterSuffixes = []string{
	"", "s", "es", "sses", "ies", "ss", "eed", "ed", "ing", "at", "bl", "iz",
	"y", "ational", "tional", "enci", "anci", "izer", "logi", "bli", "alli",
	"entli", "eli", "ousli", "ization", "ation", "ator", "alism", "iveness",
	"fulness", "ousness", "aliti", "iviti", "biliti", "icate", "ative",
	"alize", "iciti", "ical", "ful", "ness", "al", "ance", "ence", "er", "ic",
	"able", "ible", "ant", "ement", "ment", "ent", "ion", "ou", "ism", "ate",
	"iti", "ous", "ive", "ize", "e", "ll", "sion", "tion",
}

// fts5R30PorterEdges includes edge cases for stemming tests.
var fts5R30PorterEdges = []string{
	"a", "ab", "abc", "aed", "eed", "ied", "oed", "ues", "aes", "ies", "sss",
	"ss", "ses", "eee", "yyy", "bbb", "aaa", "ing", "aing", "eing", "bling",
	"at", "bl", "iz", "ate", "ble", "ize", "ll", "all", "ball", "controlling",
	"controller", "roll", "rolling", "national", "rational", "conditional",
	"conditionally", "referenced", "referencing", "happy", "happily",
	"happier", "happiest", "skies", "sky", "dying", "die", "agreed",
	"agreement", "plastered", "bled", "motoring", "sing", "conflated",
	"troubled", "sized", "hopping", "tanned", "falling", "hissing", "fizzed",
	"failing", "filing", "matting", "mating", "meeting", "milling", "messing",
	"meetings", "feed", "feeding", "relational", "conditional", "valenci",
	"hesitanci", "digitizer", "conformabli", "radicalli", "differentli",
	"vileli", "analogousli", "vietnamization", "predication", "operator",
	"feudalism", "decisiveness", "hopefulness", "callousness", "formaliti",
	"sensitiviti", "sensibiliti", "triplicate", "formative", "formalize",
	"electriciti", "electrical", "hopeful", "goodness", "revival", "allowance",
	"inference", "airliner", "gyroscopic", "adjustable", "defensible",
	"irritant", "replacement", "adjustment", "dependent", "adoption",
	"homologou", "communism", "activate", "angulariti", "homologous",
	"effective", "bowdlerize", "probate", "rate", "cease", "controll", "roll",
	"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl",   // 64 bytes: stemmed
	"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklm", // 65 bytes: passed through
	"café", "naïve", "日本語", "ΑΒΓαβγ", "ABCDES", "AbCdEs", "HELLOS",
	"foo_bar", "foo-bar", "a1b2c3s", "123ing", "0ed",
}

// fts5R30PorterWords returns the deduplicated word corpus.
func fts5R30PorterWords() []string {
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, s := range fts5R30PorterStems {
		for _, suf := range fts5R30PorterSuffixes {
			add(s + suf)
		}
	}
	for _, w := range fts5R30PorterEdges {
		add(w)
	}
	return out
}

// fts5R30PorterQuote quotes a word as an SQL string.
func fts5R30PorterQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// TestFts5R30PorterStemTable verifies stemmer correctness by comparing FTS5 vocabulary.
func TestFts5R30PorterStemTable(t *testing.T) {
	words := fts5R30PorterWords()
	rows := make([]string, len(words))
	for i, w := range words {
		rows[i] = fmt.Sprintf("(%d,%s)", i+1, fts5R30PorterQuote(w))
	}
	stmts := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, tokenize=porter)`,
		`INSERT INTO t(rowid,a) VALUES` + strings.Join(rows, ","),
		`CREATE VIRTUAL TABLE v_inst USING fts5vocab(t, 'instance')`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
	}
	const q = `SELECT doc, '#'||hex(term), "offset" FROM v_inst ORDER BY doc, "offset", hex(term)`
	goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
	cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
	if goErr != nil || cgoErr != nil {
		t.Fatalf("%s\n  this engine: %v\n  C SQLite: %v", q, goErr, cgoErr)
	}
	if goOut == cgoOut {
		return
	}
	// Name the words: the doc column is the rowid, which is the word's index.
	g, c := strings.Split(goOut, "\n"), strings.Split(cgoOut, "\n")
	shown := 0
	for i := 0; i < len(g) || i < len(c); i++ {
		var gl, cl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(c) {
			cl = c[i]
		}
		if gl == cl {
			continue
		}
		word := "?"
		if f := strings.Fields(cl + " " + gl); len(f) > 0 {
			n := 0
			fmt.Sscanf(f[0], "%d", &n)
			if n >= 1 && n <= len(words) {
				word = words[n-1]
			}
		}
		t.Errorf("porter stem DIVERGES for %q\n  this engine: %s\n  C SQLite: %s", word, gl, cl)
		if shown++; shown >= 25 {
			t.Fatalf("... %d differing lines in all", len(g))
		}
	}
}

// TestFts5R30PorterVocab verifies tokenization with the Porter stemmer.
func TestFts5R30PorterVocab(t *testing.T) {
	for _, spec := range fts5R30PorterSpecs {
		spec := spec
		t.Run(spec, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := append([]string{fts5TokCreate(spec), fts5TokInsert()}, fts5TokVocabCreate...)
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5TokVocabDump {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  this engine: %v\n  C SQLite: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("token stream DIVERGES\n  sql: %s\n%s", q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// TestFts5R30PorterShadowBytes verifies file format correctness.
func TestFts5R30PorterShadowBytes(t *testing.T) {
	for _, spec := range fts5R30PorterSpecs {
		spec := spec
		t.Run(spec, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{fts5TokCreate(spec), fts5TokInsert()}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5ShadowDump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				// columnsize=0 creates no %_docsize at all: agreeing to have
				// no such table is agreement, and the shadow-table SET itself
				// is compared through sqlite_master in the same dump.
				if goErr != nil && cgoErr != nil && goErr.Error() == cgoErr.Error() {
					continue
				}
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}

				if goOut != cgoOut {
					t.Errorf("shadow bytes DIVERGE\n  sql: %s\n%s", q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// fts5R30PorterProbes are MATCH queries to test stemming.
var fts5R30PorterProbes = []string{
	`SELECT count(*) FROM t`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'running' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'runs' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'run' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'wor*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'runn*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '"hello world"' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello AND world' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello OR abc' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'a:hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'NEAR(hello world)' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'nosuchtermatall')`,
	`SELECT ifnull(group_concat(k),'') FROM (SELECT rowid || ':' || snippet(t,-1,'[',']','...',6) AS k FROM t WHERE t MATCH 'hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(k),'') FROM (SELECT rowid || ':' || highlight(t,0,'<','>') AS k FROM t WHERE t MATCH 'hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(k),'') FROM (SELECT rowid || ':' || highlight(t,0,'<','>') AS k FROM t WHERE t MATCH 'runs' ORDER BY rowid)`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts5R30PorterInterchange verifies that databases are interchangeable between engines.
func TestFts5R30PorterInterchange(t *testing.T) {
	words := fts5R30PorterWords()
	for _, spec := range []string{
		`tokenize=porter`,
		`tokenize='porter ascii'`,
		`tokenize='porter unicode61 remove_diacritics 0'`,
		`tokenize=porter, prefix=2`,
	} {
		spec := spec
		t.Run(spec, func(t *testing.T) {
			cases := []struct {
				name  string
				stmts []string
			}{
				{"the whole corpus", []string{fts5TokCreate(spec), fts5TokInsert()}},
				{"mutations rewrite postings", []string{
					fts5TokCreate(spec),
					fts5TokInsert(),
					`UPDATE t SET a='rewritten hello texts running here' WHERE rowid % 3 = 0`,
					`DELETE FROM t WHERE rowid % 5 = 0`,
					`INSERT INTO t(rowid,a,b) VALUES(9001,'hello worlds again','tail')`,
				}},
				{"a multi-page segment of stemmable words", []string{
					fts5TokCreate(spec),
					fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 3000, func(i int) string {
						w := words[i%len(words)]
						return fmt.Sprintf("(%d,'word%04ding %s hello','col%d shared runs')", i+1, i, w, i)
					}),
				}},
				{"a multi-page segment, then deletes and optimize", []string{
					fts5TokCreate(spec),
					fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 2000, func(i int) string {
						w := words[i%len(words)]
						return fmt.Sprintf("(%d,'word%04dness %s hello','col%d shared')", i+1, i, w, i)
					}),
					`DELETE FROM t WHERE rowid % 7 = 0`,
					`INSERT INTO t(t) VALUES('optimize')`,
					`INSERT INTO t(t) VALUES('rebuild')`,
				}},
			}
			for _, c := range cases {
				c := c
				t.Run(c.name, func(t *testing.T) {
					for _, writer := range []string{"sqlite", "sqlite3"} {
						dsn := filepath.Join(t.TempDir(), "fts5.db")
						if err := fts5Exec(t, writer, dsn, c.stmts); err != nil {
							t.Fatalf("%s writes: %v", writer, err)
						}
						for _, q := range fts5R30PorterProbes {
							goOut, goErr := fts5Query(t, "sqlite", musqlPathFor(t, writer, dsn), q)
							cgoOut, cgoErr := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), q)
							if goErr != nil && cgoErr != nil && goErr.Error() == cgoErr.Error() {
								continue
							}
							if goErr != nil || cgoErr != nil {
								t.Fatalf("written by %s\n  sql: %s\n  this engine: %v\n  C SQLite: %v", writer, q, goErr, cgoErr)
							}
							if goOut != cgoOut {
								t.Errorf("written by %s: DIVERGES\n  sql: %s\n%s", writer, q, fts5DiffLines(goOut, cgoOut))
							}
						}
						ok, err := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), `PRAGMA integrity_check`)
						if err != nil {
							t.Fatalf("written by %s: C SQLite integrity_check: %v", writer, err)
						}
						if _, body, _ := strings.Cut(ok, "\n"); strings.TrimSpace(body) != "T:ok" {
							t.Errorf("written by %s: C SQLite reports the file CORRUPT: %s", writer, ok)
						}
						if _, err := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), `INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
							t.Errorf("written by %s: C fts5 integrity-check: %v", writer, err)
						}
					}
				})
			}
		})
	}
}
