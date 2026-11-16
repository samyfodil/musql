package engine

import (
	"reflect"
	"testing"
)

// TestCreateTableParseLeavesLexTokens is a single-quoted column name, which
// both CREATE TABLE parsers rewrite into an identifier, leaving the tokens lex
// hands out for the same text as they were.
func TestCreateTableParseLeavesLexTokens(t *testing.T) {
	sql := `CREATE TABLE t('a' INTEGER, b TEXT UNIQUE, CHECK(b <> 'x'))`
	before, err := lex(sql)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]token(nil), before...)
	def, err := parsePragmaTableDef(sql)
	if err != nil {
		t.Fatal(err)
	}
	cols, _, _, err := parseCreateTableColumnsAndAutoIndexes(sql, false)
	if err != nil {
		t.Fatal(err)
	}
	if def.cols[0].name != "a" || cols[0].Name != "a" {
		t.Fatalf("column names %q and %q, want a", def.cols[0].name, cols[0].Name)
	}
	after, err := lex(sql)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("lex's tokens changed: %v, were %v", after, want)
	}
}

// TestAutoIndexNumbers is the automatic indexes a CREATE text names, asked
// twice so the second answer comes from the memo.
func TestAutoIndexNumbers(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want []int
	}{
		{`CREATE TABLE r(a INTEGER PRIMARY KEY, b UNIQUE, c, UNIQUE(c))`, []int{1, 2}},
		{`CREATE TABLE r2(a TEXT PRIMARY KEY, b UNIQUE)`, []int{1, 2}},
		// A WITHOUT ROWID table's PRIMARY KEY gets no row, but keeps its number.
		{`CREATE TABLE w(a PRIMARY KEY, b UNIQUE) WITHOUT ROWID`, []int{2}},
		{`CREATE TABLE p(a, b)`, nil},
		{`CREATE TABLE`, nil},
	} {
		for range 2 {
			if got := autoIndexNumbers(c.sql); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%s: %v, want %v", c.sql, got, c.want)
			}
		}
	}
}
