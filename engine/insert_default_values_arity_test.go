package engine

// Column list with DEFAULT VALUES is an arity error; wrong answer now fixed.

import "testing"

func TestInsertDefaultValuesWithAColumnListIsAnArityError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  string
	}{
		{"ordinary table", []string{`CREATE TABLE t(a,b)`},
			`INSERT INTO t(a) DEFAULT VALUES`, `engine: 0 values for 1 columns`},
		{"ordinary table, two named columns", []string{`CREATE TABLE t(a,b)`},
			`INSERT INTO t(a,b) DEFAULT VALUES`, `engine: 0 values for 2 columns`},
		{"view with an INSTEAD OF INSERT trigger",
			[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
				`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`},
			`INSERT INTO v(a) DEFAULT VALUES`, `engine: 0 values for 1 columns`},
		{"virtual table", []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			`INSERT INTO f(x) DEFAULT VALUES`, `engine: 0 values for 1 columns`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			err = db.Exec(tc.stmt)
			if err == nil {
				t.Fatalf("%q SUCCEEDED. The 3.53.3 oracle rejects it at prepare time (insert.c:1255-1258);\n"+
					"accepting it stores a row C SQLite never stores.", tc.stmt)
			}
			if err.Error() != tc.want {
				t.Fatalf("%q: got %q, want %q", tc.stmt, err.Error(), tc.want)
			}
		})
	}
}

// TestInsertDefaultValuesWithNoColumnListStillWorks is the other half: the
// check must be on a NON-EMPTY list only, because the C compares against
// pColumn->nId and an empty IDLIST agrees with nColumn==0. Without this, the
// obvious "stmt.cols != nil" spelling would reject every DEFAULT VALUES
// statement in the corpus -- parseInsertStmt sets stmt.cols to a non-nil empty
// slice for the shape itself.
func TestInsertDefaultValuesWithNoColumnListStillWorks(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{`CREATE TABLE t(a, b DEFAULT 7)`, `INSERT INTO t DEFAULT VALUES`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	got := rvdRowStrings(rvdQuery(t, db, `SELECT a IS NULL, b FROM t`))
	if len(got) != 1 || got[0] != "1,7" {
		t.Fatalf("got %v, want [1,7]", got)
	}
}
