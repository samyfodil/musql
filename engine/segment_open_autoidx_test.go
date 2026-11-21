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
