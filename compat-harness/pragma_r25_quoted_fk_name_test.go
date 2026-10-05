// This file gates foreign-key name positions with single-quoted spellings.
package compat

import "testing"

// pragmaR25QuotedFKCases test foreign keys with quoted names.
var pragmaR25QuotedFKCases = []struct {
	name  string
	stmts []string
}{
	// The reported shape: a single-quoted parent, self-referencing.
	{"single-quoted-parent-self-reference", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE 'pone'(a REFERENCES 'pone', b, PRIMARY KEY(b))`,
		`INSERT INTO 'pone' VALUES(1,1)`,
		`SELECT * FROM pragma_foreign_key_list('pone')`,
		`INSERT INTO 'pone' VALUES(99,2)`,
		`INSERT INTO 'pone' VALUES(1,3)`,
		`SELECT a, b FROM 'pone' ORDER BY b`,
	}},

	// The COLUMN position, all four spellings, column-level.
	{"quoted-parent-column-single", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(x PRIMARY KEY)`,
		`INSERT INTO par VALUES(1)`,
		`CREATE TABLE k1(y REFERENCES par('x'))`,
		`SELECT * FROM pragma_foreign_key_list('k1')`,
		`INSERT INTO k1 VALUES(99)`,
		`INSERT INTO k1 VALUES(1)`,
		`SELECT y FROM k1`,
	}},
	{"quoted-parent-column-double", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(x PRIMARY KEY)`,
		`INSERT INTO par VALUES(1)`,
		`CREATE TABLE k2(y REFERENCES par("x"))`,
		`SELECT * FROM pragma_foreign_key_list('k2')`,
		`INSERT INTO k2 VALUES(99)`,
		`INSERT INTO k2 VALUES(1)`,
		`SELECT y FROM k2`,
	}},
	{"quoted-parent-column-bracket", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(x PRIMARY KEY)`,
		`INSERT INTO par VALUES(1)`,
		`CREATE TABLE k3(y REFERENCES par([x]))`,
		`SELECT * FROM pragma_foreign_key_list('k3')`,
		`INSERT INTO k3 VALUES(99)`,
		`INSERT INTO k3 VALUES(1)`,
		`SELECT y FROM k3`,
	}},

	// The table-level form, with EVERY name quoted at once: the child columns,
	// the parent, and the parent columns.
	{"table-level-every-name-quoted", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(x PRIMARY KEY)`,
		`INSERT INTO par VALUES(1)`,
		`CREATE TABLE k5(y, FOREIGN KEY('y') REFERENCES 'par'('x'))`,
		`SELECT * FROM pragma_foreign_key_list('k5')`,
		`INSERT INTO k5 VALUES(99)`,
		`INSERT INTO k5 VALUES(1)`,
		`SELECT y FROM k5`,
	}},

	// A multi-column key mixing all four spellings -- which also pins the ORDER
	// foreign_key_list reports its pairs in.
	{"multi-column-mixed-spellings", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p2(a, b, PRIMARY KEY(a,b))`,
		`INSERT INTO p2 VALUES(1,2)`,
		`CREATE TABLE c2(u, v, FOREIGN KEY('u','v') REFERENCES "p2"('a',[b]))`,
		`SELECT * FROM pragma_foreign_key_list('c2')`,
		`INSERT INTO c2 VALUES(9,9)`,
		`INSERT INTO c2 VALUES(1,2)`,
		`SELECT u, v FROM c2`,
	}},

	// The silent-non-enforcement one: a quoted CONSTRAINT name.
	{"quoted-constraint-name-single", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(x PRIMARY KEY)`,
		`INSERT INTO par VALUES(1)`,
		`CREATE TABLE kA(y, CONSTRAINT 'fk1' FOREIGN KEY(y) REFERENCES par(x))`,
		`SELECT * FROM pragma_foreign_key_list('kA')`,
		`INSERT INTO kA VALUES(99)`,
		`INSERT INTO kA VALUES(1)`,
		`SELECT y FROM kA`,
	}},
	{"quoted-constraint-name-bracket", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(x PRIMARY KEY)`,
		`INSERT INTO par VALUES(1)`,
		`CREATE TABLE kC(y, CONSTRAINT [fk3] FOREIGN KEY(y) REFERENCES par(x))`,
		`SELECT * FROM pragma_foreign_key_list('kC')`,
		`INSERT INTO kC VALUES(99)`,
		`INSERT INTO kC VALUES(1)`,
		`SELECT y FROM kC`,
	}},

	// The trailing clauses must still parse after a quoted name -- and CASCADE
	// must really cascade, which is a side effect no "accepted" can fake.
	{"quoted-names-then-on-delete-cascade", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE par(x PRIMARY KEY)`,
		`INSERT INTO par VALUES(1)`,
		`CREATE TABLE k6(y REFERENCES 'par'('x') ON DELETE CASCADE)`,
		`SELECT * FROM pragma_foreign_key_list('k6')`,
		`INSERT INTO k6 VALUES(1)`,
		`DELETE FROM par WHERE x=1`,
		`SELECT count(*) FROM k6`,
	}},

	// A parent that genuinely does not exist keeps its own name in the error,
	// dequoted -- "no such table: main.no such parent".
	{"quoted-parent-that-does-not-exist", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE k9(y REFERENCES 'no such parent')`,
		`SELECT * FROM pragma_foreign_key_list('k9')`,
		`INSERT INTO k9 VALUES(1)`,
		`SELECT count(*) FROM k9`,
	}},

	// The quoted CONSTRAINT name in front of the OTHER table-level kinds, whose
	// PRAGMA-side reporting the same skip governs: the automatic index and its
	// origin ("pk"/"u") come back under the quoted name too.
	//
	// Their ENFORCEMENT is a separate parser and is still wrong -- see
	// TestPragmaR25QuotedConstraintNameIsDroppedByTheWritePath below.
	{"quoted-constraint-name-on-pk-and-unique", []string{
		`CREATE TABLE t1(a, b, CONSTRAINT 'pk' PRIMARY KEY(a,b))`,
		`SELECT * FROM pragma_index_list('t1')`,
		`CREATE TABLE t2(a, CONSTRAINT 'u' UNIQUE(a))`,
		`SELECT * FROM pragma_index_list('t2')`,
	}},
	// These two were a KNOWN WRONG ANSWER in their own test until the write
	// path's three CONSTRAINT-name skips learned objectNameToken (SQLite's
	// "nm" production) as well. Silent non-enforcement: the CREATE was
	// accepted, the schema text round-tripped, the PRAGMA reported the
	// constraint, and only the row that should have been refused said
	// otherwise. Now compared against the oracle, which is the stronger
	// assertion -- and the four spellings are exercised, not just the one that
	// broke.
	{"quoted-constraint-name-enforces-unique", []string{
		`CREATE TABLE t2(a, CONSTRAINT 'u' UNIQUE(a))`,
		`INSERT INTO t2 VALUES(1)`,
		`INSERT INTO t2 VALUES(1)`,
		`SELECT count(*) FROM t2`,
	}},
	{"quoted-constraint-name-enforces-check", []string{
		`CREATE TABLE t3(a, CONSTRAINT 'c' CHECK(a>10))`,
		`INSERT INTO t3 VALUES(1)`,
		`INSERT INTO t3 VALUES(11)`,
		`SELECT a FROM t3`,
	}},
	{"every-constraint-quote-form-enforces", []string{
		`CREATE TABLE q1(a, CONSTRAINT "u1" UNIQUE(a))`,
		`INSERT INTO q1 VALUES(1)`,
		`INSERT INTO q1 VALUES(1)`,
		`CREATE TABLE q2(a, CONSTRAINT [u2] UNIQUE(a))`,
		`INSERT INTO q2 VALUES(1)`,
		`INSERT INTO q2 VALUES(1)`,
		"CREATE TABLE q3(a, CONSTRAINT `u3` UNIQUE(a))",
		`INSERT INTO q3 VALUES(1)`,
		`INSERT INTO q3 VALUES(1)`,
		`SELECT (SELECT count(*) FROM q1), (SELECT count(*) FROM q2), (SELECT count(*) FROM q3)`,
	}},
	{"quoted-constraint-name-then-more-constraints", []string{
		`CREATE TABLE m1(a, b, CONSTRAINT 'u' UNIQUE(a), CONSTRAINT 'c' CHECK(b>0), UNIQUE(b))`,
		`INSERT INTO m1 VALUES(1,1)`,
		`INSERT INTO m1 VALUES(1,2)`,
		`INSERT INTO m1 VALUES(2,0)`,
		`INSERT INTO m1 VALUES(2,1)`,
		`SELECT a,b FROM m1`,
		`SELECT * FROM pragma_index_list('m1')`,
	}},
}

func TestPragmaR25QuotedForeignKeyNames(t *testing.T) {
	for _, tc := range pragmaR25QuotedFKCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, tc.name, tc.stmts) })
	}
}
