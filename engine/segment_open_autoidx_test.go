package engine

import (
	"slices"
	"testing"
)

// TestAutoIndexNumbersWithoutConstraint: a CREATE text naming no PRIMARY KEY
// and no UNIQUE is not parsed for its automatic indexes, and answers what the
// parse answers, in any case and any spelling of the words.
func TestAutoIndexNumbersWithoutConstraint(t *testing.T) {
	for _, sql := range []string{
		`CREATE TABLE t(a INT, b TEXT)`,
		`CREATE TABLE t(a INT PRIMARY KEY, b TEXT)`,
		`create table t(a int primary key, b text unique)`,
		`CREATE TABLE t(a, b, PrImArY KeY(a, b))`,
		`CREATE TABLE t(a, b, CONSTRAINT c UNIQUE(b))`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UnIqUe)`,
		`CREATE TABLE t(a PRIMARY KEY, b) WITHOUT ROWID`,
		`CREATE TABLE "primary"(a, "unique")`,
		`CREATE TABLE t(a)`,
	} {
		var want []int
		if def, err := parsePragmaTableDef(sql); err == nil {
			for i, spec := range def.autoIdx {
				if !(def.withoutRowid && spec.kind == "pk") {
					want = append(want, i+1)
				}
			}
		}
		if got := autoIndexNumbers(sql); !slices.Equal(got, want) {
			t.Errorf("%s: automatic indexes %v, the parse says %v", sql, got, want)
		}
	}
	for _, c := range []struct {
		s    string
		want bool
	}{{"x primary y", true}, {"PRIMAR", false}, {"pRiMaRy", true}, {"", false}, {"PRIMARX", false}, {"primary", true}} {
		if got := hasWordFold(c.s, "PRIMARY"); got != c.want {
			t.Errorf("hasWordFold(%q) = %v", c.s, got)
		}
	}
}

// TestCatalogTextWordChecks: a CREATE text that does not name VIRTUAL is no
// virtual table, and one that does not name WITHOUT is no WITHOUT ROWID table,
// without being lexed; either word, in any case or inside a name or a comment,
// still goes to the lexer, which answers.
func TestCatalogTextWordChecks(t *testing.T) {
	for _, c := range []struct {
		sql              string
		virtual, without bool
	}{
		{`CREATE TABLE t(a INT)`, false, false},
		{`CREATE VIRTUAL TABLE v USING fts4(a)`, true, false},
		{`create virtual table v using rtree(id, x0, x1)`, true, false},
		{`CREATE TEMP VIRTUAL TABLE v USING fts4(a)`, true, false},
		{`CREATE TABLE "virtual"(a)`, false, false},
		{`CREATE TABLE t(a PRIMARY KEY, b) WITHOUT ROWID`, false, true},
		{`CREATE TABLE t(a PRIMARY KEY, b) without rowid`, false, true},
		{`CREATE TABLE t(a PRIMARY KEY, b) WiThOuT RoWiD, STRICT`, false, true},
		{`CREATE TABLE t(a, "without rowid")`, false, false},
		{`CREATE TABLE t(a) /* WITHOUT ROWID */`, false, false},
	} {
		if got := isCreateVirtualTableSQL(c.sql); got != c.virtual || got != isCreateVirtualTableSQLUncached(c.sql) {
			t.Errorf("%s: virtual %v, want %v", c.sql, got, c.virtual)
		}
		if got := sqlTextTableIsWithoutRowid(c.sql); got != c.without {
			t.Errorf("%s: WITHOUT ROWID %v, want %v", c.sql, got, c.without)
		}
	}
}
