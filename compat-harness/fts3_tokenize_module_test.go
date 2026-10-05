// FTS3 tokenizer selection tests: simple delimiter argument and fts3tokenize
// virtual table against SQLite 3.53.3.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

var fts3DelimDump = []string{
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(c0x) FROM t_content ORDER BY docid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
}

func TestFts3SimpleDelimiterDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"one argument is ignored", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple 'a')`,
			`INSERT INTO t(x) VALUES('abcab')`,
			`SELECT docid FROM t WHERE t MATCH 'abcab'`,
			`SELECT docid FROM t WHERE t MATCH 'bc'`,
		}, fts3DelimDump...)},
		// The UNQUOTED spelling of the same thing. This one used to be on
		// fts3_module_args_test.go's deliberately-declined list, described as
		// "an argument to the one tokenizer that takes none" -- a premise the
		// argv[1] rule shows was false. C SQLite accepts it and indexes
		// 'a-b c' with the DEFAULT table, as two terms.
		{"an unquoted single argument is ignored too", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple extra)`,
			`INSERT INTO t(x) VALUES('a-b c')`,
			`SELECT docid FROM t WHERE t MATCH 'a'`,
			`SELECT docid FROM t WHERE t MATCH 'c'`,
			`SELECT docid FROM t WHERE t MATCH 'a-b'`,
		}, fts3DelimDump...)},
		{"argv[1] is the delimiter list", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple 'q' 'a')`,
			`INSERT INTO t(x) VALUES('abcab')`,
			`SELECT docid FROM t WHERE t MATCH 'abcab'`,
			`SELECT docid FROM t WHERE t MATCH 'bc'`,
			`SELECT docid FROM t WHERE t MATCH 'b'`,
			`SELECT offsets(t) FROM t WHERE t MATCH 'bc'`,
		}, fts3DelimDump...)},
		// The listed bytes REPLACE the alphanumeric test rather than extending
		// it: with 'xyz ' listed, a digit still starts a token and '-' (which
		// the default table splits on) no longer does.
		{"the default alphanumeric test is replaced wholesale", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple '' 'xyz ')`,
			`INSERT INTO t(x) VALUES('1x2x3x')`,
			`INSERT INTO t(x) VALUES('a-b.c d')`,
			`SELECT docid FROM t WHERE t MATCH '1'`,
			`SELECT docid FROM t WHERE t MATCH 'a-b.c'`,
			`SELECT docid FROM t WHERE t MATCH 'a'`,
			`SELECT offsets(t) FROM t WHERE t MATCH '2'`,
			`SELECT snippet(t) FROM t WHERE t MATCH '3'`,
		}, fts3DelimDump...)},
		{"an empty delimiter list makes one term", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple 'q' '')`,
			`INSERT INTO t(x) VALUES('one two three')`,
			`SELECT docid FROM t WHERE t MATCH '"one two three"'`,
			`SELECT docid FROM t WHERE t MATCH 'one'`,
		}, fts3DelimDump...)},
		{"a non-ASCII delimiter fails the CREATE", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple '' 'é')`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`INSERT INTO t(x) VALUES('anything')`,
		}},
		{"a UTF-8 sequence is not split by an explicit list", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple 'q' ' ')`,
			`INSERT INTO t(x) VALUES('héllo wörld')`,
			`SELECT docid FROM t WHERE t MATCH 'héllo'`,
			`SELECT docid FROM t WHERE t MATCH 'h'`,
		}, fts3DelimDump...)},
		{"the default tokenizer is unchanged alongside one", append([]string{
			`CREATE VIRTUAL TABLE t USING fts4(x, tokenize=simple 'q' 'a')`,
			`CREATE VIRTUAL TABLE u USING fts4(x)`,
			`INSERT INTO t(x) VALUES('abcab')`,
			`INSERT INTO u(x) VALUES('abcab a-b')`,
			`SELECT docid FROM u WHERE u MATCH 'abcab'`,
			`SELECT docid FROM u WHERE u MATCH 'b'`,
			`SELECT docid FROM t WHERE t MATCH 'bc'`,
			`SELECT quote(root) FROM u_segdir`,
		}, fts3DelimDump...)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}

func fts3DelimInterchangeWrite(spec string) []string {
	stmts := []string{`CREATE VIRTUAL TABLE t USING fts4(x, ` + spec + `)`}
	for i := 0; i < 200; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t(x) VALUES('word%03dx common%03d-tail')`, i, i))
	}
	stmts = append(stmts, `DELETE FROM t WHERE docid %  7 = 0`, `INSERT INTO t(t) VALUES('optimize')`)
	return stmts
}

var fts3DelimInterchangeRead = []string{
	`SELECT docid, x FROM t ORDER BY docid`,
	`SELECT docid, quote(c0x) FROM t_content ORDER BY docid`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
}

func TestFts3SimpleDelimiterInterchange(t *testing.T) {
	for _, spec := range []string{
		`tokenize=simple 'q' 'a'`,
		`tokenize=simple '' 'xyz -'`,
		`tokenize=simple 'a'`, // the ignored-argument form: the DEFAULT table
	} {
		spec := spec
		t.Run(spec, func(t *testing.T) {
			for _, writer := range engineOrder {
				dsn := filepath.Join(t.TempDir(), "fts.db")
				runWithDSN(t, writer, dsn, fts3DelimInterchangeWrite(spec))
				var baseline string
				// Each engine over its OWN format, with the converter in between where the
				// writer was the other one (convert_for_oracle_test.go).
				goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
				for _, reader := range engineOrder {
					readPath := goPath
					if reader == "cgo" {
						readPath = cgoPath
					}
					got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, fts3DelimInterchangeRead))
					if baseline == "" {
						baseline = got
						continue
					}
					if got != baseline {
						t.Errorf("[%s writes] readers disagree\n  %s\n  %s reads: %s", writer, baseline, reader, got)
					}
				}
				sdb, err := sql.Open("sqlite3", cgoPath)
				if err != nil {
					t.Fatalf("sql.Open(sqlite3): %v", err)
				}
				var ic string
				if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
					sdb.Close()
					t.Fatalf("[%s writes] integrity_check: %v", writer, err)
				}
				if ic != "ok" {
					t.Errorf("[%s writes] C SQLite reports integrity_check = %q", writer, ic)
				}
				sdb.Close()
				// fts3's own index-vs-content check, run by BOTH engines over
				// the same file. A delimiter table that disagreed with the one
				// the writer used would fail exactly here.
				for _, drv := range []string{"sqlite", "sqlite3"} {
					path := goPath // each engine over its own format (RULE #3)
					if drv == "sqlite3" {
						path = cgoPath
					}
					if err := fts3TokExec(t, drv, path, `INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
						t.Errorf("[%s writes] %s integrity-check: %v", writer, drv, err)
					}
				}
			}
		})
	}
}

func fts3TokExec(t *testing.T, drv, dsn, stmt string) error {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	_, err = db.Exec(stmt)
	return err
}

// ---- 2. the fts3tokenize virtual table ----

func TestFts3TokenizeVtabDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// The module's whole surface: the declared schema (input, token,
		// start, end, position -- none hidden), the rowid, and the fact that a
		// query with no "input =" constraint is an ERROR rather than a scan.
		{"the simple tokenizer", []string{
			`CREATE VIRTUAL TABLE t1 USING fts3tokenize(simple)`,
			`CREATE VIRTUAL TABLE t2 USING fts3tokenize()`,
			`CREATE VIRTUAL TABLE t3 USING fts3tokenize`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`SELECT * FROM t1 WHERE input = 'one two three'`,
			`SELECT * FROM t2 WHERE input = 'OnE tWo tHrEe'`,
			`SELECT * FROM t3 WHERE input = 'a b c'`,
			`SELECT rowid, token FROM t1 WHERE input = 'a b c'`,
			`SELECT token FROM t1 WHERE input = '1x2x3x'`,
			`SELECT token FROM t1 WHERE input = ''`,
			`SELECT token FROM t1 WHERE input = NULL`,
			`SELECT count(*) FROM t1 WHERE input = 'a b'`,
			`SELECT * FROM t1 WHERE input = 'a b c' AND token = 'b'`,
			`SELECT * FROM t1 WHERE token = 'b' AND input = 'a b c'`,
			`SELECT * FROM t1 WHERE input < 'b' AND input = 'a b c'`,
			`SELECT * FROM t1 WHERE input = 'a' AND input = 'b c'`,
			`SELECT typeof(input), typeof(token), typeof(start), typeof(end), typeof(position) FROM t1 WHERE input = 'ab'`,
			`SELECT * FROM t1`,
			`SELECT * FROM t1 WHERE token = 'a'`,
			`INSERT INTO t1(input) VALUES('x')`,
			`DELETE FROM t1`,
			`DROP TABLE t2`,
			`SELECT name FROM sqlite_master ORDER BY name`,
		}},
		// The tokenizer NAME is argument 0 and everything after it is that
		// tokenizer's own argument list -- the same handoff a tokenize=
		// specification makes.
		{"every named tokenizer", []string{
			`CREATE VIRTUAL TABLE u1 USING fts3tokenize(unicode61)`,
			`CREATE VIRTUAL TABLE u2 USING fts3tokenize( "unicode61", "tokenchars=@.", "separators=1234567890" )`,
			`CREATE VIRTUAL TABLE p1 USING fts3tokenize(porter)`,
			`CREATE VIRTUAL TABLE s1 USING fts3tokenize(simple, '', 'xyz ')`,
			`CREATE VIRTUAL TABLE s2 USING fts3tokenize(simple, 'a')`,
			`SELECT * FROM u1 WHERE input = 'Hello WÖrld.'`,
			`SELECT * FROM u2 WHERE input = 'abc@def.ghi 123 jkl'`,
			`SELECT * FROM p1 WHERE input = 'running gardens'`,
			`SELECT token FROM s1 WHERE input = '1x2x3x'`,
			`SELECT token FROM s1 WHERE input = '1''2x3x'`,
			`SELECT token FROM s1 WHERE input = ''`,
			`SELECT * FROM s2 WHERE input = 'abcab'`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		}},
		// Error cases: an unknown tokenizer fails the CREATE, and a tokenizer
		// this build does not carry (icu) fails it too.
		{"an unknown tokenizer fails the CREATE", []string{
			`CREATE VIRTUAL TABLE tX USING fts3tokenize(nosuchtokenizer)`,
			`CREATE VIRTUAL TABLE tY USING fts3tokenize(icu)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`SELECT * FROM tX WHERE input = 'a b'`,
		}},
		// A DEFAULT-tokenizer fts3tokenize alongside an ordinary fts4 table:
		// the two must not disturb each other, and the vtab must survive being
		// written to the file and read back by a later statement.
		{"alongside an fts4 table", []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`CREATE VIRTUAL TABLE tk USING fts3tokenize(simple)`,
			`INSERT INTO ft(a) VALUES('one two')`,
			`SELECT docid FROM ft WHERE ft MATCH 'two'`,
			`SELECT token, start, end, position FROM tk WHERE input = 'one two'`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}

// TestFts3TokenizeVtabDeclined pins what is LEFT of the shapes real
// fts3tokenize answers and this engine will not.
//
// The two NON-TEXT spellings that stood here -- "input = 123" and
// "input = 12.5" -- are ANSWERED now, and correctly: the consumed conjunct is
// removed from the residual WHERE at compile time (vtab_omit.go), which is
// fts3tokBestIndexMethod's own "aConstraintUsage[i].omit = 1"
// (fts3_tokenize_vtab.c:248) landing somewhere at last. compat-harness/
// fts3tokenize_omit_test.go carries them, with every other storage class and
// with the residual-WHERE cases that prove exactly ONE conjunct was dropped.
//
// What still declines is the NUL, and it is a different problem entirely --
// a TOKENIZER difference, not a WHERE one. simpleCreate builds its default
// delimiter table with "for(i=1; i<0x80; i++){ t->delim[i] = !fts3_isalnum(i)
// ? -1 : 0; }" (fts3_tokenizer1.c:89-92): the loop starts at ONE, so delim[0]
// is left zero and NUL is a TOKEN CHARACTER in C. x'6120620063' therefore
// yields TWO tokens there -- 'a', then 'b<NUL>c' spanning 2..5, while the
// reported "input" stops at the NUL because xColumn passes -1
// (fts3_tokenize_vtab.c:388) -- where this engine's fts3TokenizeSpans makes
// every non-alphanumeric byte a separator and yields three. Answering would be
// a row too many, so it declines.
//
// The difference is invisible everywhere else: every INDEXING call site
// truncates at the first NUL before tokenizing (fts3TokenizerInput,
// engine/fts3_tokenizer.go, with its own oracle evidence), and only this module
// opens the tokenizer over the full byte count. porter and unicode61 have not
// been measured on the same question, which is the other half of why this is
// still a decline.
//
// The second spelling is the same value reached through "||", which is ALSO
// declined for a second, independent reason: a non-literal right-hand side
// cannot be proved to be the constraint BestIndex consumes, so no conjunct is
// dropped for it at all (fts3TokModule.OmittedConjunct, engine/vtab_fts3tok.go).
func TestFts3TokenizeVtabDeclined(t *testing.T) {
	for _, bad := range []string{
		`SELECT * FROM tk WHERE input = x'6120620063'`,
		`SELECT * FROM tk WHERE input = 'a' || char(0) || 'b'`,
	} {
		bad := bad
		t.Run(bad, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "tok.db")
			if err := fts3TokExec(t, "sqlite", dsn, `CREATE VIRTUAL TABLE tk USING fts3tokenize(simple)`); err != nil {
				t.Fatalf("setup: %v", err)
			}
			db, err := sql.Open("sqlite", importedForMusql(t, dsn))
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			defer db.Close()
			if _, err := db.Query(bad); err == nil {
				t.Fatalf("engine ANSWERED a shape whose row C fts3tokenize reports differently: %s", bad)
			}
		})
	}
}
