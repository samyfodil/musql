package engine

// TestFkStatementReachReadsTheConflictClauseAtItsGrammarPosition tests
// that the conflict clause is detected at its grammar position, not by
// scanning the entire statement.

import "testing"

func TestFkStatementReachReadsTheConflictClauseAtItsGrammarPosition(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want fkReach
	}{
		{"plain insert", `INSERT INTO ch VALUES(1)`, 0},
		{"insert or replace", `INSERT OR REPLACE INTO ch VALUES(1)`, fkReachDelete},
		{"insert or ignore", `INSERT OR IGNORE INTO ch VALUES(1)`, 0},
		{"insert or abort", `INSERT OR ABORT INTO ch VALUES(1)`, 0},

		// Cases with replace() function, not INSERT OR REPLACE.
		{"replace() in a SELECT source's WHERE", `INSERT INTO ch SELECT a FROM p WHERE a=1 OR replace(b,'x','y')='z'`, 0},
		{"replace() in the select list", `INSERT INTO ch SELECT replace(b,'x','y') FROM p WHERE c=1 OR d=2`, 0},
		{"a column literally named replace", `INSERT INTO ch SELECT a FROM p WHERE a=1 OR "replace"=2`, 0},

		// Same cases with leading WITH.
		{"WITH + replace() in the WHERE", `WITH c AS (SELECT 1 AS a) INSERT INTO ch SELECT a FROM p WHERE a=1 OR replace(b,'x','y')='z'`, 0},
		{"WITH + genuine insert or replace", `WITH c AS (SELECT 1 AS a) INSERT OR REPLACE INTO ch SELECT a FROM c`, fkReachDelete},

		// The DO UPDATE arm genuinely appears later and must keep scanning.
		{"upsert do update", `INSERT INTO ch VALUES(1) ON CONFLICT(a) DO UPDATE SET b=2`, fkReachUpdate},
		{"upsert do nothing", `INSERT INTO ch VALUES(1) ON CONFLICT(a) DO NOTHING`, 0},
		{"or replace AND do update", `INSERT OR REPLACE INTO ch VALUES(1) ON CONFLICT(a) DO UPDATE SET b=2`, fkReachDelete | fkReachUpdate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := lex(tc.sql)
			if err != nil {
				t.Fatalf("lex: %v", err)
			}
			kw, verbAt := writeDispatchKeywordAt(tc.sql, toks)
			got := fkStatementReach(kw, toks, verbAt)
			if got != tc.want {
				t.Errorf("fkStatementReach(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}
