package engine

// Tests that REPLACE INTO and INSERT OR REPLACE compile to byte-identical programs.
// Both spellings must use the same execution path.

import (
	"regexp"
	"testing"
)

// ptrRE normalizes heap addresses in disassembly output.
var ptrRE = regexp.MustCompile(`0x[0-9a-f]+`)

func TestReplaceSpellingCompilesLikeInsertOrReplace(t *testing.T) {
	for _, tc := range []struct {
		setup []string
		short string
		long  string
	}{
		{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`},
			`REPLACE INTO t VALUES(1,'x')`,
			`INSERT OR REPLACE INTO t VALUES(1,'x')`},
		{[]string{`CREATE TABLE t(a UNIQUE, b)`},
			`REPLACE INTO t(a,b) VALUES(1,'x')`,
			`INSERT OR REPLACE INTO t(a,b) VALUES(1,'x')`},
		{[]string{`CREATE TABLE t(a UNIQUE, b)`, `CREATE TABLE s(a,b)`},
			`REPLACE INTO t SELECT a,b FROM s`,
			`INSERT OR REPLACE INTO t SELECT a,b FROM s`},
		{[]string{`CREATE TABLE t(a UNIQUE, b)`},
			`REPLACE INTO t VALUES(1,'x') RETURNING a,b`,
			`INSERT OR REPLACE INTO t VALUES(1,'x') RETURNING a,b`},
		{[]string{`CREATE TABLE t(a UNIQUE, b)`},
			`REPLACE INTO main.t VALUES(1,'x')`,
			`INSERT OR REPLACE INTO main.t VALUES(1,'x')`},
	} {
		t.Run(tc.short, func(t *testing.T) {
			// ONE database for both compiles: a program bakes *tableMeta
			// pointers into its P4 operands, so two databases would differ in
			// every payload for reasons that say nothing about the programs.
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
			disasm := func(sql string) string {
				prog, cerr := db.compileWrite(sql)
				if cerr != nil {
					t.Fatalf("compile %q: %v", sql, cerr)
				}
				return ptrRE.ReplaceAllString(prog.Disassemble(), "0xPTR")
			}
			if got, want := disasm(tc.short), disasm(tc.long); got != want {
				t.Errorf("the two spellings of one insert_cmd compile differently.\n"+
					"REPLACE INTO:\n%s\nINSERT OR REPLACE INTO:\n%s", got, want)
			}
		})
	}
}
