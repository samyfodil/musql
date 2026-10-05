//go:build sqlite_fts5

// Tests fts5 tokenizers: token stream agreement, raw segment bytes,
// and interchange. Each tokenizer is proved via vocab, shadow bytes, and oracle integrity checks.
package compat

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// Tokenizer spellings this engine accepts, in forms from the mined corpus.
var fts5TokSpecs = []string{
	`tokenize=unicode61`,
	`tokenize=UNICODE61`,
	`tokenize=''`, // an empty value is fts5's default tokenizer, verified
	`tokenize='unicode61 remove_diacritics 0'`,
	`tokenize='unicode61 remove_diacritics 1'`,
	`tokenize='unicode61 remove_diacritics 2'`,
	`tokenize="unicode61 remove_diacritics 0"`,
	`tokenize=[unicode61 remove_diacritics 2]`,
	`tokenize='unicode61 tokenchars ''_-.'''`,
	`tokenize='unicode61 separators ''aoE'''`,
	`tokenize='unicode61 separators ''a'' tokenchars ''a'''`,
	`tokenize='unicode61 tokenchars ''a'' separators ''a'''`,
	`tokenize='unicode61 tokenchars ''-'' separators ''o'' remove_diacritics 0'`,
	`tokenize='unicode61 categories ''L* N* Co'''`,
	`tokenize='unicode61 categories ''L*'''`,
	`tokenize='unicode61 categories ''L* N* Co Mn'''`,
	`tokenize='unicode61 categories ''Nd Zs'''`,
	`tokenize='unicode61 categories ''C* P* S* Z* M*'''`,
	`tokenize='unicode61 categories ''Ll Lu Nd'''`,
	`tokenize='unicode61 categories ''Nd'' separators ''a'''`,
	`tokenize='unicode61 categories ''Nd'' tokenchars ''a'''`,
	`tokenize='unicode61 categories ''L* N* Co Cc'''`,
	`tokenize=ascii`,
	`tokenize=ASCII`,
	`tokenize='ascii tokenchars ''_-.'''`,
	`tokenize='ascii separators ''aoE0'''`,
	`tokenize='ascii separators ''-'' tokenchars ''-'''`,
	`tokenize=trigram`,
	`tokenize='trigram'`,
	`tokenize=[trigram]`,
	`tokenize='trigram case_sensitive 0'`,
	`tokenize='trigram case_sensitive 1'`,
	`tokenize='trigram remove_diacritics 1'`,
	`tokenize='trigram remove_diacritics 2'`,
	// Combined with the other configuration options this engine accepts, since
	// a prefix index is a second term space over the SAME token stream.
	`tokenize=ascii, prefix=2`,
	`tokenize='trigram', prefix='1 3'`,
	`tokenize='unicode61 remove_diacritics 0', columnsize=0`,
}

// Tokenizer test corpus covering ASCII, mixed case, accents, CJK, ligatures, UTF-8 variants.
var fts5TokDocs = []string{
	`('Hello World hello WORLD', 'second COLUMN text')`,
	`('foo_bar foo-bar foo.bar 123 abc123 4.5 AT&T e.g. O''Brien don''t', 'a b c ab abc abcd abcde')`,
	"('café CAFÉ café naïve Ünïcödé ÀÉÎÕÜ', 'åäö ÅÄÖ đ ø ł')",
	"('日本語 テスト 中文 ΑΒΓ αβγ ΣΤΙΓΜΑΣ σ ς', 'mixed 日 script')",
	"('İ K Å ẞ ǅ ǆ Ǆ ＡＢＣ ａｂｃ ⅰ Ⅰ', 'ﬀ ﬁ ǰ ß')",
	"('private  use  here', 'combining áêĩ')",
	`('', 'empty first column')`,
	`('   ' || char(9) || char(10) || char(13) || '  ', 'whitespace only')`,
	`(NULL, 'null first column')`,
	`(CAST(x'6162FF6364' AS TEXT), 'bad utf8: stray FF')`,
	`(CAST(x'61C26263' AS TEXT), 'bad utf8: truncated two-byte')`,
	`(CAST(x'61EDA08063' AS TEXT), 'bad utf8: encoded surrogate')`,
	`(CAST(x'61F4908080' AS TEXT), 'bad utf8: above U+10FFFF')`,
	`(CAST(x'61C0806263' AS TEXT), 'bad utf8: overlong NUL')`,
	`(CAST(x'61EFBFBE62' AS TEXT), 'bad utf8: noncharacter U+FFFE')`,
	`(CAST(x'618081826263' AS TEXT), 'bad utf8: stray continuation bytes')`,
	`('ab', 'a')`,
	`('abc', 'ab')`,
	`('abc' || char(0) || 'def', 'nul between two words')`,
}

// Builds CREATE statement for a tokenizer spec.
func fts5TokCreate(spec string) string {
	return `CREATE VIRTUAL TABLE t USING fts5(a, b, ` + spec + `)`
}

// Inserts the whole corpus as one statement for single-segment layout.
func fts5TokInsert() string {
	rows := make([]string, len(fts5TokDocs))
	for i, d := range fts5TokDocs {
		rows[i] = fmt.Sprintf("(%d,%s", i+1, d[1:])
	}
	return `INSERT INTO t(rowid,a,b) VALUES` + strings.Join(rows, ",")
}

// Reads index as tokens via fts5vocab kinds (instance, row, col).
var fts5TokVocabDump = []string{
	`SELECT '#'||hex(term), doc, col, "offset" FROM v_inst ORDER BY hex(term), doc, col, "offset"`,
	`SELECT '#'||hex(term), cnt FROM v_row ORDER BY hex(term)`,
	`SELECT '#'||hex(term), col, doc, cnt FROM v_col ORDER BY hex(term), col`,
}

var fts5TokVocabCreate = []string{
	`CREATE VIRTUAL TABLE v_inst USING fts5vocab(t, 'instance')`,
	`CREATE VIRTUAL TABLE v_row USING fts5vocab(t, 'row')`,
	`CREATE VIRTUAL TABLE v_col USING fts5vocab(t, 'col')`,
}

// Verifies token streams match via fts5vocab, testing all tokenizer spellings.
func TestFts5TokenizerVocab(t *testing.T) {
	for _, spec := range fts5TokSpecs {
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

// Verifies raw segment bytes match: %_data, %_idx, %_docsize, %_config.
func TestFts5TokenizerShadowBytes(t *testing.T) {
	for _, spec := range fts5TokSpecs {
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
				// columnsize=0 creates no %_docsize at all, so the dump's
				// query against it fails over BOTH files -- agreeing to have
				// no such table is agreement, and the shadow-table SET itself
				// is compared through sqlite_master below.
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

// Interchange queries answered identically by both engines over either file.
var fts5TokInterchangeProbes = []string{
	`SELECT count(*) FROM t`,
	`SELECT group_concat(rowid) FROM (SELECT rowid FROM t ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'ell' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'wor*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '"hello world"' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello AND world' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello OR abc' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'a:hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'NEAR(hello world)' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'cafe' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'caf' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'nosuchtermatall')`,
	// A query shorter than one trigram tokenizes to nothing there, which fts5
	// answers with no rows rather than an error (verified) -- this engine has
	// to agree, since silently matching everything would be the wrong answer.
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'ab' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'z')`,
	// LIKE/GLOB are what trigram exists to accelerate in C fts5. They are
	// answered from the content here, so agreement proves that accepting the
	// tokenizer changed which rows a query returns nowhere.
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE a LIKE '%ell%' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE b GLOB '*col*' ORDER BY rowid)`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// Tokenizer subset for full write/read interchange with multi-page and mutation cases.
var fts5TokInterchangeSpecs = []string{
	`tokenize=unicode61`,
	`tokenize='unicode61 remove_diacritics 0'`,
	`tokenize='unicode61 tokenchars ''_-.'''`,
	`tokenize='unicode61 categories ''L*'''`,
	`tokenize='unicode61 categories ''L* N* Co Cc'''`,
	`tokenize=ascii`,
	`tokenize='ascii tokenchars ''_-.'''`,
	`tokenize=trigram`,
	`tokenize='trigram case_sensitive 1'`,
}

// Verifies write/read interchange: both engines read the same way, integrity_check passes.
func TestFts5TokenizerInterchange(t *testing.T) {
	for _, spec := range fts5TokInterchangeSpecs {
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
					`UPDATE t SET a='rewritten hello text here' WHERE rowid % 3 = 0`,
					`DELETE FROM t WHERE rowid % 5 = 0`,
					`INSERT INTO t(rowid,a,b) VALUES(9001,'hello world again','tail')`,
				}},
				{"a multi-page segment", []string{
					fts5TokCreate(spec),
					fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 3000, func(i int) string {
						return fmt.Sprintf("(%d,'word%04d hello qqq','col%d shared')", i+1, i, i)
					}),
				}},
				{"a multi-page segment, then deletes and optimize", []string{
					fts5TokCreate(spec),
					fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 2000, func(i int) string {
						return fmt.Sprintf("(%d,'word%04d hello qqq','col%d shared')", i+1, i, i)
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
						for _, q := range fts5TokInterchangeProbes {
							goOut, goErr := fts5Query(t, "sqlite", musqlPathFor(t, writer, dsn), q)
							cgoOut, cgoErr := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), q)
							switch {
							case goErr != nil:
								t.Errorf("[%s wrote] this engine cannot read it back\n  sql: %s\n  err: %v", writer, q, goErr)
							case cgoErr != nil:
								t.Errorf("[%s wrote] C SQLite cannot read it\n  sql: %s\n  err: %v", writer, q, cgoErr)
							case goOut != cgoOut:
								t.Errorf("[%s wrote] the two engines read it differently\n  sql: %s\n  go:  %q\n  cgo: %q", writer, q, goOut, cgoOut)
							}
						}
						ic, err := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), `PRAGMA integrity_check`)
						if err != nil {
							t.Fatalf("[%s wrote] integrity_check: %v", writer, err)
						}
						if ic != "integrity_check\nT:ok" {
							t.Errorf("[%s wrote] C SQLite reports integrity_check = %q", writer, ic)
						}
						// fts5's OWN integrity check, which hashes every
						// posting derivable from %_content against every one
						// the segments decode to -- run on both sides, since
						// each engine implements it over its own reader.
						// ...each over its OWN format, with the converter in between
						// where the writer was the other one
						// (convert_for_oracle_test.go).
						for _, drv := range []string{"sqlite", "sqlite3"} {
							path := musqlPathFor(t, writer, dsn)
							if drv == "sqlite3" {
								path = oraclePathFor(t, writer, dsn)
							}
							if err := fts5Exec(t, drv, path, []string{`INSERT INTO t(t) VALUES('integrity-check')`}); err != nil {
								t.Errorf("[%s wrote] %s integrity-check: %v", writer, drv, err)
							}
						}
					}
				})
			}
		})
	}
}

// Verifies MATCH on empty phrase (tokenizes to no terms) returns no rows.
func TestFts5TokenizerEmptyPhrase(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t USING fts5(x, y)`,
		`CREATE VIRTUAL TABLE g USING fts5(x, y, tokenize=trigram)`,
		`INSERT INTO t(rowid,x,y) VALUES(1,'hello world','col2 hello world'),(2,'ab','col2 ab'),(3,'abc def','col2 abc def')`,
		`INSERT INTO g(rowid,x,y) VALUES(1,'hello world','col2 hello world'),(2,'ab','col2 ab'),(3,'abc def','col2 abc def')`,
	}
	answered := []string{
		`'""'`, `'"+++"'`, `'""*'`, `'"+++"*'`, `'x:"+++"'`, `'^"+++"'`, `'{x y}:"+++"'`,
		`'"+++" AND hello'`, `'"+++" OR hello'`, `'hello NOT "+++"'`, `'"+++" NOT hello'`,
		`'"+++" + hello'`, `'hello + "+++"'`,
		`'hello "+++"'`, `'NEAR("+++" hello)'`,
		// Under trigram these are the same shape reached by an ordinary query.
		`'ab'`, `'a'`, `'ab*'`, `'^ab'`, `'a*'`, `'NEAR(ab cd)'`,
		`'ab OR hello'`, `'ab AND hello'`, `'hello NOT ab'`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
	}
	for _, tab := range []string{"t", "g"} {
		for _, q := range answered {
			stmt := fmt.Sprintf("SELECT ifnull(group_concat(rowid),'-') FROM (SELECT rowid FROM %s WHERE %s MATCH %s ORDER BY rowid)", tab, tab, q)
			goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], stmt)
			cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], stmt)
			switch {
			case cgoErr != nil:
				t.Fatalf("C SQLite errors on a query listed as answered: %s: %v", stmt, cgoErr)
			case goErr != nil:
				t.Errorf("this engine declines %s: %v", stmt, goErr)
			case goOut != cgoOut:
				t.Errorf("%s\n  go:  %q\n  cgo: %q", stmt, goOut, cgoOut)
			}
		}
	}
}

// Tokenizer spellings this engine refuses, with oracle behavior.
var fts5TokDeclined = []struct {
	spec        string
	oracleTakes bool // true when C fts5 accepts it and this engine declines
}{
	// Invalid spellings: unknown tokenizer, malformed/invalid arguments.
	{`tokenize=blah`, false},
	{`tokenize='unicode61 error'`, false},
	{`tokenize='unicode61 tokenchars'`, false},
	{`tokenize='unicode61 remove_diacritics 3'`, false},
	{`tokenize='unicode61 remove_diacritics 10'`, false},
	{`tokenize='unicode61 remove_diacritics 00'`, false},
	{`tokenize='unicode61 remove_diacritics x'`, false},
	{`tokenize='unicode61 categories ''XYZ'''`, false},
	{`tokenize='unicode61 categories ''N* MYZ'''`, false},
	{`tokenize='unicode61 categories ''l*'''`, false},
	{`tokenize='unicode61 categories ''*'''`, false},
	{`tokenize='unicode61 tokenchars ''-'' bogus 1'`, false},
	{`tokenize='unicode61 tokenchars -_'`, false},
	{`tokenize='unicode61 tokenchars "-"'`, false},
	{`tokenize='unicode61 tokenchars [-]'`, false},
	{`tokenize='ascii remove_diacritics 1'`, false},
	{`tokenize='ascii categories ''L*'''`, false},
	{`tokenize='ascii porter'`, false},
	{`tokenize='trigram tokenchars ''ab'''`, false},
	{`tokenize='trigram separators ''a'''`, false},
	{`tokenize='trigram case_sensitive 2'`, false},
	{`tokenize='trigram case_sensitive x'`, false},
	{`tokenize='trigram case_sensitive 01'`, false},
	{`tokenize='trigram remove_diacritics 3'`, false},
	{`tokenize='trigram case_sensitive 1 remove_diacritics 1'`, false},
	{`tokenize='trigram remove_diacritics 1 case_sensitive 1'`, false},
	{`tokenize='   '`, false},
	{`tokenize=unicode61, tokenize=ascii`, false},
}

// Verifies declined tokenizers are rejected at CREATE.
func TestFts5TokenizerDeclined(t *testing.T) {
	for _, c := range fts5TokDeclined {
		c := c
		t.Run(c.spec, func(t *testing.T) {
			dir := t.TempDir()
			goErr := fts5Exec(t, "sqlite", filepath.Join(dir, "musql.db"), []string{fts5TokCreate(c.spec)})
			cgoErr := fts5Exec(t, "sqlite3", filepath.Join(dir, "cgo.db"), []string{fts5TokCreate(c.spec)})
			if goErr == nil {
				t.Fatalf("this engine ACCEPTED a tokenizer it does not implement (C SQLite: %v)", cgoErr)
			}
			if c.oracleTakes {
				if cgoErr != nil {
					t.Errorf("expected C SQLite to accept this; it said %v", cgoErr)
				}
				return
			}
			if cgoErr == nil {
				t.Errorf("C SQLite ACCEPTS this and this engine declines it -- a decline, but reclassify the case")
			}
		})
	}
}

// Verifies reading a database C SQLite wrote with tokenizers respects the declared tokenizer.
func TestFts5TokenizerForeign(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "porter.db")
	if err := fts5Exec(t, "sqlite3", dsn, []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, tokenize=porter)`,
		`INSERT INTO t(rowid,a) VALUES(1,'running happily'),(2,'connection relates')`,
		`CREATE VIRTUAL TABLE v USING fts5vocab(t, 'row')`,
	}); err != nil {
		t.Fatalf("C SQLite writes: %v", err)
	}
	for _, q := range []string{
		`SELECT rowid FROM t WHERE t MATCH 'run'`,
		`SELECT rowid FROM t WHERE t MATCH 'running'`,
		`SELECT rowid FROM t WHERE t MATCH 'happili'`,
		`SELECT rowid FROM t WHERE t MATCH 'happily'`,
		`SELECT term FROM v ORDER BY term`,
		`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'run'`,
		`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'run' ORDER BY rowid`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", importedForMusql(t, dsn), q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn, q)
		if goErr != nil && cgoErr != nil {
			continue
		}
		if goErr != nil || cgoErr != nil {
			t.Errorf("one engine answered a porter-tokenized table C SQLite wrote and the other did not\n  sql: %s\n  this engine: %v\n  C SQLite: %v", q, goErr, cgoErr)
			continue
		}
		if goOut != cgoOut {
			t.Errorf("DIVERGES over a porter-tokenized table C SQLite wrote\n  sql: %s\n%s", q, fts5DiffLines(goOut, cgoOut))
		}
	}
}
