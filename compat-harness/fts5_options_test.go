//go:build sqlite_fts5

// This file tests fts5 CREATE VIRTUAL TABLE options: argument spelling
// acceptance, shadow layout, interchange, and vocab queries.
package compat

import (
	"fmt"
	"path/filepath"
	"testing"
)

// fts5OptionAcceptance lists CREATE VIRTUAL TABLE spellings and whether this engine accepts them.
var fts5OptionAcceptance = []struct {
	sql    string
	accept bool
	why    string
}{
	// ---- the UNINDEXED column modifier ----
	{`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)`, true, "the modifier itself"},
	{`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b)`, true, "on the first column"},
	{`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b UNINDEXED)`, true, "on every column: an index with no terms at all"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b unindexed)`, true, "matched case-insensitively"},
	{`CREATE VIRTUAL TABLE t USING fts5(a    UNINDEXED  , b)`, true, "extra spaces around it"},
	{`CREATE VIRTUAL TABLE t USING fts5("a b" UNINDEXED, c)`, true, "after a quoted name"},
	{`CREATE VIRTUAL TABLE t USING fts5("a UNINDEXED", b)`, true, "INSIDE quotes it is part of the NAME, not a modifier"},
	{`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXEDX)`, false, "oracle: unrecognized column option: UNINDEXEDX"},
	{`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED UNINDEXED)`, false, "oracle: parse error"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b NOT NULL)`, false, "oracle: parse error -- UNINDEXED is the only column option"},
	{"CREATE VIRTUAL TABLE t USING fts5(a\tUNINDEXED, b)", false, "oracle: parse error -- a TAB does not separate the modifier"},
	{"CREATE VIRTUAL TABLE t USING fts5(a\nUNINDEXED, b)", false, "oracle: parse error -- nor does a newline"},
	{`CREATE VIRTUAL TABLE t USING fts5(rank UNINDEXED)`, false, "oracle: reserved fts5 column name: rank (checked AFTER the modifier is stripped)"},

	// ---- options spelled with their DEFAULT value: exactly the plain table ----
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=1)`, true, "the default, byte-identical to no option"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize='1')`, true, "quoted value"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize="1")`, true, "double-quoted value"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, COLUMNSIZE = 1)`, true, "key case-insensitive, spaces around ="},
	{`CREATE VIRTUAL TABLE t USING fts5(columnsize=1, a, b)`, true, "an option may precede the columns"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=1, columnsize=1)`, true, "a repeated columnsize is legal there, last wins"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=full)`, true, "the default detail"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, detail='full')`, true, "quoted"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=FULL)`, true, "value case-insensitive"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=unicode61)`, true, "the default tokenizer, named explicitly"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize='unicode61')`, true, "quoted"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize="unicode61")`, true, "double-quoted"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=[unicode61])`, true, "bracket-quoted"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=UNICODE61)`, true, "value case-insensitive"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize='  unicode61  ')`, true, "space-padded inside the quotes"},
	// The other tokenizers and their arguments; every one of these has its
	// token stream, its raw %_data bytes and C SQLite's integrity_check
	// over a file this engine wrote gated in fts5_tokenizer_test.go.
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=ascii)`, true, "the ascii tokenizer"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=trigram)`, true, "the trigram tokenizer"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize='unicode61 remove_diacritics 0')`, true, "a tokenizer argument"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize='unicode61 remove_diacritics 1')`, true, "spelled at its default value"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize='unicode61 tokenchars ''-''')`, true, "a quoted tokenizer-argument value"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize='unicode61 categories ''L* N*''')`, true, "a categories list"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize='trigram case_sensitive 1')`, true, "trigram's own argument"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, detail=full, columnsize=1)`, true, "together, and with UNINDEXED"},

	// ---- columnsize=0: the one non-default value that is exact here ----
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)`, true, "drops %_docsize; everything else unchanged"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize='0')`, true, "quoted"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=1, columnsize=0)`, true, "last wins: this one IS columnsize=0"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0, columnsize=1)`, true, "last wins: this one IS columnsize=1"},
	{`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b, columnsize=0)`, true, "with UNINDEXED"},

	// ---- prefix=: an extra term space in the same segment ----
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=2)`, true, "one prefix index"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='2 3')`, true, "space-separated list"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='3 2')`, true, "the ORDER is the encoding, so this is a different file"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1, 2, 3')`, true, "comma-separated"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1 ,2')`, true, "space AND comma"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=[1 2])`, true, "bracket-quoted"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='2, 2')`, true, "a repeated LENGTH is two identical prefix indexes, not one"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=2, prefix=3)`, true, "a repeated prefix= APPENDS to the list"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='01')`, true, "leading zeros parse (unlike columnsize=01)"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='')`, true, "an empty list: exactly a table with no prefix index"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=' ')`, true, "all-space is the empty list too"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=999)`, true, "the largest legal length"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31')`, true, "31 is the limit"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=0)`, false, "oracle: prefix length out of range (max 999)"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=1000)`, false, "oracle: same"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=bogus)`, false, "oracle: malformed prefix=... directive"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1 x')`, false, "oracle: same"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1,')`, false, "oracle: same -- a comma must be followed by a length"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=',')`, false, "oracle: same"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='+1')`, false, "oracle: same"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1.0')`, false, "oracle: same"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix=-1)`, false, "oracle: parse error in the whole argument"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32')`, false, "oracle: too many prefix indexes (max 31)"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16', prefix='17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32')`, false, "oracle: the limit counts ACROSS directives"},

	// ---- tokenize=porter: PORTED, not probed (engine/fts5_tokenizers.go) ----
	{`CREATE VIRTUAL TABLE t USING fts5(a, tokenize=porter)`, true, "the stemmer, over the default unicode61 base"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, tokenize='porter ascii')`, true, "a named base tokenizer"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, tokenize='porter porter')`, true, "leading 'porter' args are skipped, so this is porter over unicode61"},

	// ---- an option KEY is a case-insensitive PREFIX of its full name ----
	// fts5ConfigParseSpecial tests strnicmp(<name>, zCmd, strlen(zCmd)) down a
	// fixed chain, so an abbreviation resolves to the FIRST name it prefixes.
	{`CREATE VIRTUAL TABLE t USING fts5(a, t='trigram')`, true, "t= is tokenize=, reached before tokendata="},
	{`CREATE VIRTUAL TABLE t USING fts5(a, TOKEN='ascii')`, true, "matched case-insensitively"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, p=2)`, true, "p= is prefix=, the first arm of the chain"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, =2)`, false, `oracle: parse error in "=2" -- an empty key would prefix every name, but the bareword gobble rejects it first`},
	{`CREATE VIRTUAL TABLE t USING fts5(a, column=0)`, true, "column= is columnsize="},
	{`CREATE VIRTUAL TABLE t USING fts5(a, d=full)`, true, "d= is detail="},
	{`CREATE VIRTUAL TABLE t USING fts5(a, c=ext)`, true, "c= is content=, reached before contentless_delete=, and external content is served"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, l=1)`, true, "l= is locale=, gated in fts5_r37b_locale_test.go/fts5_r38b_locale_func_test.go"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, prefixes=2)`, false, "oracle: unrecognized option -- a key LONGER than the name is not a prefix of it"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, x=1)`, false, "oracle: unrecognized option: no name starts with x"},

	// ---- rejected spellings: the two engines must agree to reject ----
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=2)`, false, "oracle: malformed columnsize=... directive"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=01)`, false, "oracle: malformed -- the value is matched literally, not parsed as a number"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=bogus)`, false, "oracle: malformed detail=... directive"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=bogus)`, false, "oracle: no such tokenizer: bogus"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=unicode61, tokenize=unicode61)`, false, "oracle: multiple tokenize=... directives, even when both are legal alone"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, nosuchopt=1)`, false, `oracle: unrecognized option: "nosuchopt"`},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b=1)`, false, `oracle: unrecognized option: "b" -- ANY key=value is an option, never a column`},
	{`CREATE VIRTUAL TABLE t USING fts5(a, "detail"=full)`, false, "oracle: parse error -- a QUOTED key is not an option key"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, [detail]=full)`, false, "oracle: parse error"},
	{`CREATE VIRTUAL TABLE t USING fts5(columnsize=1)`, false, "oracle: vtable constructor failed -- an fts5 table needs at least one column"},

	// detail=none/columns are now IMPLEMENTED (engine/fts5_detail.go); their
	// bytes and their query refusals are gated by fts5_r31_detail_test.go,
	// which is where the interesting spellings live. "column" and the
	// last-wins repeat stay here because they are spellings that test the
	// PARSE rather than the mode: fts5ConfigSetEnum resolves an enum value by
	// prefix, so "column" is FTS5_DETAIL_COLUMNS.
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=none)`, true, "detail=none"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=column)`, true, "a prefix of 'columns' resolves to it"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=full, detail=none)`, true, "a repeat is last-wins, so this one IS detail=none"},

	// content=<table> (EXTERNAL content) is now IMPLEMENTED
	// (engine/fts5_extcontent.go); its shadow bytes, its answers and its
	// writes are gated by fts5_r32p_extcontent_test.go, which is where the
	// interesting spellings live.
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content=ext, content_rowid=id)`, true, "external content"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content=ext)`, true, "external content, content_rowid defaults to the content table's rowid"},

	// CONTENTLESS. Its rows are stored NOWHERE, so both its scan and its MATCH
	// are answered by reading %_data's index back into documents
	// (engine/fts5_contentless.go); the whole subject is gated by
	// fts5_r33t_contentless_test.go.
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content='')`, true, "contentless"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content='', contentless_delete=0)`, true, "contentless, with the inert spelling of the flag"},

	// ---- declined here, ACCEPTED there: a clean decline, never a wrong answer ----
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content='', contentless_delete=1)`, true, "contentless with deletes: the V2 structure record and the %_docsize origin column, gated in fts5_r34w_contentless_delete_test.go"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content='', columnsize=0)`, true, "contentless with no %_docsize: the rowid set and every column's token count come from the POSTINGS, and fts5_main.c:1623's \"table does not support scanning\" is reproduced -- gated in fts5_r38b_columnsize0_test.go"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content='', detail=none)`, true, "contentless AND detail=none: the CREATE is accepted, because C fts5's own restriction is not on the CREATE -- fts5ConfigParse stores both and every READ then has no positions to rebuild the documents from, which is where this engine declines too (fts5_contentless.go's fts5ContentlessDocsOf)"},
	{`CREATE VIRTUAL TABLE t USING fts5(a, b, content_rowid=id)`, false, "declined: C fts5 stores it and then fails every read, since %_content has no such column"},
}

// TestFts5OptionAcceptance is the load-bearing safety claim: this engine never
// ACCEPTS an option spelling C fts5 rejects (which would leave a table the
// oracle does not have, desynchronizing every later statement), and every
// spelling it does accept the oracle accepts too.
func TestFts5OptionAcceptance(t *testing.T) {
	for _, c := range fts5OptionAcceptance {
		c := c
		t.Run(c.sql, func(t *testing.T) {
			dir := t.TempDir()
			// content=ext needs its content table to exist first.
			setup := []string{`CREATE TABLE ext(id INTEGER PRIMARY KEY, a, b)`}
			goErr := fts5Exec(t, "sqlite", filepath.Join(dir, "musql.db"), append(setup, c.sql))
			cgoErr := fts5Exec(t, "sqlite3", filepath.Join(dir, "cgo.db"), append(setup, c.sql))
			switch {
			case goErr == nil && cgoErr != nil:
				t.Errorf("this engine ACCEPTED a CREATE C fts5 rejects (%s)\n  sql: %s\n  cgo err: %v", c.why, c.sql, cgoErr)
			case goErr != nil && c.accept:
				t.Errorf("this engine declined a spelling it is supposed to accept (%s)\n  sql: %s\n  err: %v", c.why, c.sql, goErr)
			case goErr == nil && !c.accept:
				t.Errorf("this engine now ACCEPTS a spelling recorded as declined (%s) -- if that is deliberate, move it to accept:true so its bytes get gated\n  sql: %s", c.why, c.sql)
			case goErr != nil && cgoErr == nil:
				t.Logf("declined (tracked, not wrong): %s\n  %s\n  err: %v", c.sql, c.why, goErr)
			}
		})
	}
}

// fts5OptionLayoutCases build a table with ONE statement, where this engine's
// whole-index rebuild and C fts5's single flushed segment agree byte for
// byte -- the same scope TestFts5ShadowLayoutDiff works in, and the only scope
// in which %_data/%_idx can be compared at all.
var fts5OptionLayoutCases = []struct {
	name  string
	stmts []string
}{
	{"UNINDEXED, empty table", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)`,
	}},
	{"UNINDEXED trailing column", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"UNINDEXED leading column: the poslist keeps the real column number", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"every column UNINDEXED: rows, but no segment page at all", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b UNINDEXED)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"UNINDEXED middle column of three, several rows", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, c)`,
		`INSERT INTO t(rowid,a,b,c) VALUES(1,'x y','y z','z x'),(2,'y','x','y'),(9,'shared','shared','shared')`,
	}},
	{"UNINDEXED with a quoted name", []string{
		`CREATE VIRTUAL TABLE t USING fts5("a b" UNINDEXED, c)`,
		`INSERT INTO t VALUES('one two','three four')`,
	}},
	{`a column literally NAMED "a UNINDEXED" is indexed`, []string{
		`CREATE VIRTUAL TABLE t USING fts5("a UNINDEXED", b)`,
		`INSERT INTO t VALUES('one two','three four')`,
	}},
	{"a multi-page segment with an UNINDEXED column", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 3000, fts5OptionRow),
	}},
	{"columnsize=1 spelled explicitly", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=1)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"detail=full spelled explicitly", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=full)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"tokenize=unicode61 spelled explicitly", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=unicode61)`,
		`INSERT INTO t(a,b) VALUES('hello world','café NAÏVE')`,
	}},
	{"an option before the columns", []string{
		`CREATE VIRTUAL TABLE t USING fts5(columnsize=1, a, b)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"every default-valued option at once, over an UNINDEXED column", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, columnsize=1, detail=full, tokenize='unicode61')`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar'),(2,'foo','hello')`,
	}},
	{"columnsize=0, empty table", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)`,
	}},
	{"columnsize=0 drops %_docsize and nothing else", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"columnsize=0, several rowids", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, columnsize=0)`,
		`INSERT INTO t(rowid,a) VALUES(1,'x y'),(2,'x'),(5,'y x'),(100,'x')`,
	}},
	{"columnsize=0 and UNINDEXED together", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b, columnsize=0)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'hello','world'),(2,'world','hello')`,
	}},
	{"a repeated columnsize: last wins", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=1, columnsize=0)`,
		`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
	}},
	{"a multi-page segment with columnsize=0", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 3000, fts5OptionRow),
	}},
	{"prefix=2", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, prefix=2)`,
		`INSERT INTO t(rowid,a) VALUES(1,'hello world')`,
	}},
	{"prefix list order is the encoding", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, prefix='3 2')`,
		`INSERT INTO t(rowid,a) VALUES(1,'abcd')`,
	}},
	{"prefix merges truncated tokens into one poslist", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, prefix=2)`,
		`INSERT INTO t(rowid,a) VALUES(1,'abx aby abz'),(2,'abq'),(3,'zz')`,
	}},
	{"prefix across columns and rows", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, prefix='1, 2, 3')`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'abx q','aby'),(2,'ab','abcdef')`,
	}},
	{"prefix counts CHARACTERS, not bytes", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1 3 4')`,
		`INSERT INTO t(rowid,a) VALUES(1,'日本語 ab'),(2,'café NAÏVE')`,
	}},
	{"a token shorter than the prefix contributes nothing", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, prefix=999)`,
		`INSERT INTO t(rowid,a) VALUES(1,'abcd')`,
	}},
	{"prefix with UNINDEXED and columnsize=0", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, prefix=2, columnsize=0)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'abcd efgh','ignored')`,
	}},
	{"31 prefix indexes", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, prefix='1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31')`,
		`INSERT INTO t(rowid,a) VALUES(1,'abcdefghijklmnopqrstuvwxyzabcdefgh')`,
	}},
	{"a multi-page segment with a prefix index", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, prefix='2 4')`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 3000, fts5OptionRow),
	}},
}

// fts5OptionRow is one bulk row: enough distinct terms to force several leaf
// pages, plus a shared one so the doclists span page boundaries. The second
// column's text is deliberately DIFFERENT from the first's, so a column wrongly
// indexed (or wrongly skipped) shows up as extra or missing terms.
func fts5OptionRow(i int) string {
	return fmt.Sprintf("(%d,'qqq word%04d','ignored%d text')", i+1, i, i)
}

// TestFts5OptionShadowLayout pins the raw shadow bytes of every ACCEPTED
// option against the oracle's. Each dump is compared for PRESENCE as well as
// content, because an option can remove a shadow table outright and the two
// files must then agree that it is gone.
func TestFts5OptionShadowLayout(t *testing.T) {
	for _, c := range fts5OptionLayoutCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], c.stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5ShadowDump {
				// Both dumps go through the CGo driver, over the two engines'
				// FILES -- see fts5ShadowDump's comment for why.
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if (goErr == nil) != (cgoErr == nil) {
					t.Errorf("%s: only one engine's file answers %s\n  this engine's: %v\n  C SQLite's: %v", c.name, q, goErr, cgoErr)
					continue
				}
				if goErr != nil {
					// Both refuse: the shadow table is absent from both files,
					// which is itself part of the claim.
					continue
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES\n  sql: %s\n%s", c.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// fts5OptionInterchangeCases cover the shapes the byte comparison cannot --
// several statements, deletes, updates -- for the option-bearing tables.
var fts5OptionInterchangeCases = []struct {
	name  string
	stmts []string
}{
	{"UNINDEXED across several statements, with a delete and an update", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
		`INSERT INTO t(rowid,a,b) VALUES(2,'beta shared','hello')`,
		`INSERT INTO t(rowid,a,b) VALUES(3,'alpha1 beta','world')`,
		`DELETE FROM t WHERE rowid=3`,
		`UPDATE t SET b='rewritten' WHERE rowid=1`,
		`INSERT INTO t(rowid,a,b) VALUES(4,'rewritten alpha1','beta')`,
	}},
	{"every column UNINDEXED, then emptied", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b UNINDEXED)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
		`INSERT INTO t(rowid,a,b) VALUES(2,'beta','gamma')`,
		`DELETE FROM t`,
	}},
	{"columnsize=0 across several statements", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
		`INSERT INTO t(rowid,a,b) VALUES(2,'beta shared','ignored1 gamma')`,
		`DELETE FROM t WHERE rowid=1`,
		`INSERT INTO t(rowid,a,b) VALUES(3,'hello world','beta')`,
		`UPDATE t SET a='rewritten' WHERE rowid=2`,
	}},
	{"columnsize=0 with optimize and rebuild", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 200, fts5OptionRow),
		`DELETE FROM t WHERE rowid % 7 = 0`,
		`INSERT INTO t(t) VALUES('optimize')`,
		`INSERT INTO t(rowid,a,b) VALUES(9001,'hello world','beta')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"prefix= across several statements, with a delete and an update", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, prefix='2 3')`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
		`INSERT INTO t(rowid,a,b) VALUES(2,'beta shared','ignored1 gamma')`,
		`INSERT INTO t(rowid,a,b) VALUES(3,'hello there','beta')`,
		`DELETE FROM t WHERE rowid=2`,
		`UPDATE t SET a='rewritten hello' WHERE rowid=3`,
		`INSERT INTO t(t) VALUES('optimize')`,
	}},
	{"UNINDEXED with optimize and rebuild", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)`,
		fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 200, fts5OptionRow),
		`DELETE FROM t WHERE rowid % 7 = 0`,
		`INSERT INTO t(t) VALUES('optimize')`,
		`INSERT INTO t(rowid,a,b) VALUES(9001,'hello world','beta')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
}

// fts5OptionProbes are the same MATCH-through-the-index probes
// fts5InterchangeProbes runs, plus the ones that only mean something with an
// UNINDEXED column: a term that appears ONLY there must select no row, and a
// column filter naming it must select no row -- on either engine's file.
var fts5OptionProbes = []string{
	`SELECT count(*) FROM t`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'beta' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'ignored1*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'beta AND shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '"hello world"' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'rewritten' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'a:hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'b:hello' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'nosuchterm')`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts5OptionInterchange is the load-bearing claim for the accepted
// options: whichever engine wrote the file, both must answer every probe
// identically, and C SQLite must report integrity_check "ok" -- which is
// what checks the index AGAINST the content, so an UNINDEXED column silently
// indexed anyway fails here even when every query above happens to miss it.
func TestFts5OptionInterchange(t *testing.T) {
	for _, c := range fts5OptionInterchangeCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			for _, writer := range []string{"sqlite", "sqlite3"} {
				dsn := filepath.Join(t.TempDir(), "fts5.db")
				if err := fts5Exec(t, writer, dsn, c.stmts); err != nil {
					t.Fatalf("%s writes: %v", writer, err)
				}
				// Each engine reads its OWN format, with the converter in between
				// where the writer was the other one -- see
				// convert_for_oracle_test.go. The claim is unchanged.
				goPath, cgoPath := pathsForBothEngines(t, writer, dsn)
				for _, q := range fts5OptionProbes {
					goOut, goErr := fts5Query(t, "sqlite", goPath, q)
					cgoOut, cgoErr := fts5Query(t, "sqlite3", cgoPath, q)
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
			}
		})
	}
}

// TestFts5OptionVocab pins fts5vocab over an UNINDEXED column: the index does
// not hold it, so neither does any view of the index.
func TestFts5OptionVocab(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, c)`,
		`INSERT INTO t(rowid,a,b,c) VALUES(1,'hello world','foo bar','world')`,
		`INSERT INTO t(rowid,a,b,c) VALUES(2,'foo','hello','bar hello')`,
		`CREATE VIRTUAL TABLE vr USING fts5vocab(t, row)`,
		`CREATE VIRTUAL TABLE vc USING fts5vocab(t, col)`,
		`CREATE VIRTUAL TABLE vi USING fts5vocab(t, instance)`,
	}
	probes := []string{
		`SELECT * FROM vr ORDER BY term`,
		`SELECT * FROM vc ORDER BY term, col`,
		`SELECT * FROM vi ORDER BY term, doc, col, offset`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("%s setup: %v", drv, err)
		}
	}
	for _, p := range probes {
		goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], p)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], p)
		switch {
		case goErr != nil:
			t.Errorf("this engine declined %s: %v", p, goErr)
		case cgoErr != nil:
			t.Errorf("C fts5 declined %s: %v", p, cgoErr)
		case goOut != cgoOut:
			t.Errorf("fts5vocab over an UNINDEXED column DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", p, goOut, cgoOut)
		}
	}
}
