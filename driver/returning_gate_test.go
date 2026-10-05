package driver

// White-box (package driver, not driver_test) coverage for the gate
// Stmt.ExecContext puts in front of engine.StatementHasReturning: the full
// lex-and-parse now runs only when isPlainDMLText cannot rule a RETURNING
// clause out from the text alone. The gate is only sound in ONE direction --
// isPlainDMLText true must PROVE no RETURNING -- so both directions are pinned
// here: the statements that must still reach the RETURNING path, and the
// blanket claim over a battery that a skip never hides a real clause.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// returningGateTexts is the equivalence battery: every shape that stresses the
// difference between "the word RETURNING is in this text" and "this statement
// has a RETURNING clause", plus the leading-word cases isPlainDMLText decides
// on. It is deliberately heavy on the ways a text can MENTION returning
// without having the clause -- those are the ones a substring test gets
// pessimistically (falls back to the parse), which is the safe direction.
var returningGateTexts = []string{
	// Real RETURNING clauses, in every DML spelling and case.
	"INSERT INTO t(v) VALUES(5) RETURNING id",
	"insert into t(v) values(5) returning id",
	"InSeRt InTo t(v) VaLuEs(5) ReTuRnInG id",
	"REPLACE INTO t(id,v) VALUES(1,2) RETURNING *",
	"UPDATE t SET v = v + 1 RETURNING id, v",
	"DELETE FROM t WHERE id = 1 RETURNING id",
	"INSERT INTO t(v) VALUES(5)RETURNING id",
	"INSERT INTO t(v) VALUES(5) /* c */ RETURNING id",
	"  \n\t INSERT INTO t(v) VALUES(5) RETURNING id",
	"WITH x(a) AS (VALUES(9)) INSERT INTO t(v) SELECT a FROM x RETURNING id",
	// The word present but NOT as a clause: string literal, column name,
	// quoted identifier, comment. All must be classified by the real parse.
	"INSERT INTO t(v) VALUES('returning')",
	"INSERT INTO t(v) VALUES('RETURNING id')",
	"SELECT * FROM t WHERE v = 'returning'",
	"UPDATE t SET v = 'RETURNING' WHERE id = 1",
	"INSERT INTO t(v) VALUES(1) -- RETURNING id",
	"INSERT INTO t(v) VALUES(1) /* RETURNING id */",
	// No mention at all: the case the gate is allowed to skip.
	"INSERT INTO t(v) VALUES(5)",
	"INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)",
	"UPDATE t SET v = v + 1 WHERE k = ?",
	"DELETE FROM t WHERE k = ?",
	"SELECT id, v FROM t ORDER BY v DESC LIMIT 20",
	"WITH x AS (SELECT 1) SELECT * FROM x",
	// Non-DML leading words: isPlainDMLText says NOT plain, so the parse runs.
	"CREATE TABLE t(a)",
	"PRAGMA journal_mode",
	"BEGIN",
	"ATTACH DATABASE ':memory:' AS aux",
	// Degenerate texts, which must not panic on either side.
	"",
	"   ",
	"INSERT",
	"INSERT INTO t(v) VALUES('unterminated",
	// The case that makes the gate's soundness proof non-trivial: a keyword
	// spelled with U+0131 LATIN SMALL LETTER DOTLESS I. Go's strings.ToUpper
	// folds it onto "RETURNING" while containsFold (ASCII, byte-wise) cannot
	// see it -- so if token folding is ever made Unicode-aware again, the gate
	// would skip a real RETURNING clause and this entry catches it. Keyword
	// folding is ASCII precisely because C SQLite's is: sqlite3UpperToLower
	// (global.c:24) is a 256-byte table applied byte-wise (tokenize.c:112).
	"INSERT INTO t(v) VALUES(5) RETURN\u0131NG id",
}

// TestReturningGateNeverHidesARETURNING is the gate's soundness proof, run as
// a test rather than left as a comment: a SKIP (isPlainDMLText true) is only
// legal where engine.StatementHasReturning would have answered false, and the
// composed condition must equal the original for every text either way.
func TestReturningGateNeverHidesARETURNING(t *testing.T) {
	for _, sqlText := range returningGateTexts {
		want := engine.StatementHasReturning(sqlText)
		skipped := isPlainDMLText(sqlText)
		if skipped && want {
			t.Errorf("gate SKIPS the real check for %q, which HAS a RETURNING clause -- the write would be routed to the non-RETURNING path", sqlText)
		}
		got := !skipped && engine.StatementHasReturning(sqlText)
		if got != want {
			t.Errorf("gated verdict for %q = %v, ungated = %v", sqlText, got, want)
		}
	}
}

// TestExecContextRETURNINGTakesTheReturningPath is the end-to-end half: a
// RETURNING statement Exec'd through Stmt.ExecContext must still (a) perform
// its write and (b) report the oracle's RowsAffected, which for a statement
// that EMITS rows is the PRECEDING statement's change count (see
// ExecContext's own doc comment for the measured table). Both are observable
// only from the RETURNING branch -- the ordinary exec path reports
// sqlite3_changes() as of AFTER the statement instead.
func TestExecContextRETURNINGTakesTheReturningPath(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "returning.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		"INSERT INTO t(v) VALUES(1),(2),(3)", // leaves changes = 3
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	res, err := db.Exec("INSERT INTO t(v) VALUES(5) RETURNING id")
	if err != nil {
		t.Fatalf("RETURNING insert: %v", err)
	}
	ra, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if ra != 3 {
		t.Errorf("RowsAffected = %d, want 3 (the PRECEDING statement's change count, which only the RETURNING branch reports)", ra)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t WHERE v = 5").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the RETURNING insert wrote %d rows, want 1", n)
	}

	// The mirror case: "returning" inside a string literal is NOT a clause, so
	// this must take the ordinary exec path and report its OWN change count.
	res, err = db.Exec("INSERT INTO t(v) VALUES('returning')")
	if err != nil {
		t.Fatalf("literal-'returning' insert: %v", err)
	}
	if ra, err = res.RowsAffected(); err != nil {
		t.Fatal(err)
	}
	if ra != 1 {
		t.Errorf("RowsAffected = %d, want 1 -- a 'returning' STRING LITERAL must not route the statement to the RETURNING branch", ra)
	}
}
