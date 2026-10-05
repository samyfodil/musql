package compat

import "testing"

// This file tests that table constraints close the column list.
// After a table constraint, no more columns may be defined.
func TestCreateTableConstraintClosesColumnList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		// Rejected by C SQLite -- one per table-constraint spelling.
		{"check then column", []string{`CREATE TABLE t(a, CHECK(a > 0), b DEFAULT (1+2))`}},
		{"primary key then column", []string{`CREATE TABLE t(a, PRIMARY KEY(a), b)`}},
		{"unique then column", []string{`CREATE TABLE t(a, UNIQUE(a), b)`}},
		{"foreign key then column", []string{`CREATE TABLE t(a, b, FOREIGN KEY(a) REFERENCES x(a), c)`}},
		{"named constraint then column", []string{`CREATE TABLE t(a, CONSTRAINT one UNIQUE(a), b)`}},
		// A bare "CONSTRAINT nm" is itself a tcons (parse.y's
		// "tcons ::= CONSTRAINT nm"), so it closes the column list too.
		{"dangling constraint name then column", []string{`CREATE TABLE t(a, CONSTRAINT one, b)`}},

		// Still legal, and the reason this cannot simply forbid everything
		// after the first constraint.
		{"two constraints in a row", []string{
			`CREATE TABLE t(a, CHECK(a>0), CHECK(a<9))`,
			`INSERT INTO t VALUES(5)`,
			`SELECT a FROM t`,
		}},
		{"trailing dangling constraint name", []string{
			`CREATE TABLE t(a, b, c, CONSTRAINT one)`,
			`INSERT INTO t VALUES(1,2,3)`,
			`SELECT * FROM t`,
		}},
		{"a column constraint does not close anything", []string{
			`CREATE TABLE t(a PRIMARY KEY, b UNIQUE, c CHECK(c>0), d)`,
			`INSERT INTO t VALUES(1,2,3,4)`,
			`SELECT * FROM t`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "createtabletail/"+tc.name, tc.stmts) })
	}
}
