package engine

import (
	"reflect"
	"testing"
)

// TestSchemaParseKeepsNoScratchToken: what the CREATE TABLE parsers return
// from a lent token buffer holds none of its tokens, so the buffer can be
// overwritten by the next table's text: the results equal a parse over tokens
// of their own after the lent buffer is filled with another text's tokens.
func TestSchemaParseKeepsNoScratchToken(t *testing.T) {
	texts := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL DEFAULT 'x' COLLATE NOCASE, c REAL CHECK (c > 0), UNIQUE(b, c))`,
		`CREATE TABLE "q t"([a b] INT, "c" VARCHAR(10) DEFAULT (1 + 2), d BLOB, PRIMARY KEY(d DESC, "c")) WITHOUT ROWID`,
		`CREATE TABLE g(a INT, b INT GENERATED ALWAYS AS (a * 2) STORED, c AS (b || 'x') VIRTUAL, d TEXT DEFAULT CURRENT_TIME)`,
		`CREATE TABLE f(id INTEGER PRIMARY KEY AUTOINCREMENT, p INT REFERENCES parent(id) ON DELETE CASCADE, q INT, CONSTRAINT ck CHECK (q <> p), FOREIGN KEY(q) REFERENCES other(x))`,
		`CREATE TABLE s(a INTEGER, b TEXT, c ANY) STRICT`,
		`CREATE TABLE store_sales(ss_sold_date_sk INTEGER, ss_item_sk INTEGER NOT NULL, ss_ext_discount_amt DECIMAL(7,2), PRIMARY KEY(ss_item_sk, ss_sold_date_sk) ON CONFLICT REPLACE)`,
	}
	filler, err := lexUncached(`SELECT zzz, 'garbage', 999, x'00', "y" FROM w WHERE (((1))) AND v = ? ORDER BY 1, 2, 3, 4, 5, 6, 7, 8, 9`)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range texts {
		own, err := lexUncached(sql)
		if err != nil {
			t.Fatal(err)
		}
		wantCols, wantChecks, wantSpecs, wantErr := parseCreateTableToks(sql, own, false)
		wantDef, wantDefErr := parsePragmaTableDefToks(sql, own)

		toks, b, err := lexScratch(sql)
		if err != nil {
			t.Fatal(err)
		}
		cols, checks, specs, perr := parseCreateTableToks(sql, toks, false)
		def, derr := parsePragmaTableDefToks(sql, toks)
		for i := range toks {
			toks[i] = filler[i%len(filler)]
		}
		releaseScratch(b, toks)

		if (perr != nil) != (wantErr != nil) || (derr != nil) != (wantDefErr != nil) {
			t.Fatalf("%s: errors %v %v, want %v %v", sql, perr, derr, wantErr, wantDefErr)
		}
		if !reflect.DeepEqual(cols, wantCols) || !reflect.DeepEqual(checks, wantChecks) || !reflect.DeepEqual(specs, wantSpecs) {
			t.Errorf("%s: the parse changed with its lent tokens:\n cols %+v\n want %+v\n checks %+v\n want %+v", sql, cols, wantCols, checks, wantChecks)
		}
		if !reflect.DeepEqual(def, wantDef) {
			t.Errorf("%s: the table definition changed with its lent tokens:\n %+v\n want %+v", sql, def, wantDef)
		}
	}
}

// TestOpenKeepsNoCatalogTokens: opening a database parses each table's CREATE
// text from a lent buffer, so none of those texts' tokens are left in lexKept.
// Lexing one at open through lex (an AUTOINCREMENT probe did) keeps every
// table's tokens for the life of the process.
func TestOpenKeepsNoCatalogTokens(t *testing.T) {
	path := t.TempDir() + "/a.musq"
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{
		`CREATE TABLE a(x INTEGER PRIMARY KEY, y TEXT UNIQUE, z REAL DEFAULT 1.5)`,
		`CREATE TABLE b(p INT, q TEXT COLLATE NOCASE, CHECK (p > 0), UNIQUE(p, q))`,
		`CREATE TABLE c(k INT, v BLOB)`,
	}
	for _, s := range texts {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lexKept.mu.Lock()
	lexKept.m = nil
	lexKept.mu.Unlock()
	autoIndexMemo.mu.Lock()
	autoIndexMemo.m = nil
	autoIndexMemo.mu.Unlock()
	s, err := OpenOrCreateWait(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lexKept.mu.Lock()
	defer lexKept.mu.Unlock()
	for _, sql := range texts {
		if _, ok := lexKept.m[sql]; ok {
			t.Errorf("opening kept the tokens of %s", sql)
		}
	}
}
