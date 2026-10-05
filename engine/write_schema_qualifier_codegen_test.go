package engine

import (
	"strings"
	"testing"
)

// Tests schema-qualified column references in write statements.
func TestWriteSchemaQualifierCompiles(t *testing.T) {
	// Each case tests whether the qualifier is accepted (bytecode) or rejected (error).
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  string
	}{
		{"update-where", []string{"CREATE TABLE t(a,b)"}, "UPDATE t SET a=9 WHERE main.t.b=2", "bytecode"},
		{"update-set", []string{"CREATE TABLE t(a,b)"}, "UPDATE t SET a=main.t.b+1", "bytecode"},
		{"delete-where", []string{"CREATE TABLE t(a,b)"}, "DELETE FROM t WHERE main.t.b=2", "bytecode"},
		{"insert-select", []string{"CREATE TABLE t(a,b)", "CREATE TABLE u(a,b)"}, "INSERT INTO u SELECT main.t.a, main.t.b FROM t", "bytecode"},
		{"temp-target", []string{"CREATE TEMP TABLE tt(a,b)"}, "UPDATE tt SET a=9 WHERE temp.tt.b=2", "bytecode"},

		// An ALIAS hides the table's own name.
		{"update-alias", []string{"CREATE TABLE t(a,b)"}, "UPDATE t AS q SET a=9 WHERE main.t.b=2", "error"},
		// Right schema, wrong table.
		{"wrong-table", []string{"CREATE TABLE t(a,b)", "CREATE TABLE u(a,b)"}, "UPDATE t SET a=9 WHERE main.u.b=2", "error"},

		// The qualifier names a different schema than the target.
		{"temp-qual-main-target", []string{"CREATE TABLE t(a,b)"}, "UPDATE t SET a=9 WHERE temp.t.b=2", "error"},
		{"main-qual-temp-target", []string{"CREATE TEMP TABLE tt(a,b)"}, "UPDATE tt SET a=9 WHERE main.tt.b=2", "error"},
		{"unknown-qual", []string{"CREATE TABLE t(a,b)"}, "UPDATE t SET a=9 WHERE zzz.t.b=2", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			got := "bytecode"
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				got = "error"
			}
			if got != tc.want {
				t.Errorf("%q: got %s, want %s", tc.stmt, got, tc.want)
			}
		})
	}
}

// TestWriteSchemaQualifierAnswers verifies the results of qualifying columns in writes.
func TestWriteSchemaQualifierAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		{"update-where", []string{"CREATE TABLE t(a,b)", "INSERT INTO t VALUES(1,2)"},
			"UPDATE t SET a=9 WHERE main.t.b=2", "SELECT a,b FROM t", []string{"9,2"}},
		{"update-set", []string{"CREATE TABLE t(a,b)", "INSERT INTO t VALUES(1,2)"},
			"UPDATE t SET a=main.t.b+1", "SELECT a,b FROM t", []string{"3,2"}},
		{"delete-where", []string{"CREATE TABLE t(a,b)", "INSERT INTO t VALUES(1,2),(3,4)"},
			"DELETE FROM t WHERE main.t.b=2", "SELECT a,b FROM t", []string{"3,4"}},
		// A temp table shadowing a main one: the write must land in main.
		{"temp-shadow", []string{"CREATE TABLE t(a,b)", "INSERT INTO t VALUES(1,2)", "CREATE TEMP TABLE t(a,b)", "INSERT INTO temp.t VALUES(7,8)"},
			"UPDATE main.t SET a=9 WHERE main.t.b=2", "SELECT a,b FROM main.t", []string{"9,2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if err := db.Exec(tc.stmt); err != nil {
				t.Fatalf("%q: %v", tc.stmt, err)
			}
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("%q then %q: got %v, want %v", tc.stmt, tc.query, got, tc.want)
			}
		})
	}
}
