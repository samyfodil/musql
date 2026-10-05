package engine

// DB.leadingVerb memoizes the statement verb so mainReadTxnOf avoids re-lexing
// prepared statements, matching the oracle's classification of read vs write transactions.

import (
	"fmt"
	"path/filepath"
	"testing"
)

// leadingVerbTexts covers statement verbs and edge cases like comments and CTEs.
var leadingVerbTexts = []string{
	// Recognized statement verbs.
	"SELECT * FROM t", "SELECT 1", "VALUES(1)",
	"INSERT INTO t VALUES(1)", "REPLACE INTO t VALUES(1)",
	"UPDATE t SET a = 1", "DELETE FROM t",
	"CREATE TABLE u(a)", "CREATE TEMP TABLE tt(a)", "DROP TABLE u", "ALTER TABLE t RENAME TO t2",
	"VACUUM", "ANALYZE", "REINDEX",
	"BEGIN", "BEGIN DEFERRED", "BEGIN IMMEDIATE", "BEGIN EXCLUSIVE",
	"COMMIT", "END", "ROLLBACK", "SAVEPOINT s1", "RELEASE s1",
	"ATTACH DATABASE ':memory:' AS aux", "DETACH DATABASE aux",
	"PRAGMA page_count", "PRAGMA user_version = 3", "PRAGMA locking_mode = normal",
	"PRAGMA writable_schema=1", "PRAGMA nosuchpragma",
	// Leading whitespace and comments -- a lexer skips them, so a scan must.
	"   SELECT * FROM t", "\n\t\r\v\f SELECT * FROM t",
	"-- a line comment\nSELECT * FROM t",
	"/* a block comment */ INSERT INTO t VALUES(1)",
	"/* one */ -- two\n /* three */ DELETE FROM t",
	// Quoted, bracketed and backticked leading identifiers. These lex to
	// tkIdent, so LeadingStatementVerb DOES report a verb for them.
	`"SELECT" * FROM t`, "[SELECT] * FROM t", "`SELECT` * FROM t",
	`"insert" INTO t VALUES(1)`, `"sel""ect" * FROM t`,
	// WITH: the verb is what FOLLOWS the CTE list, which needs a real parse.
	"WITH x AS (SELECT 1) SELECT * FROM x",
	"WITH RECURSIVE x(a) AS (VALUES(1)) SELECT * FROM x",
	"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x",
	"WITH x AS (SELECT 1) UPDATE t SET a = 1",
	"WITH x AS (SELECT 1) DELETE FROM t",
	"WITH", "WITH x AS (", // a WITH whose CTE list does not parse
	// Text that does not lex, or lexes to no verb at all. LeadingStatementVerb
	// reports ok=false for these and mainReadTxnOf must stay UNKNOWN.
	"SELECT 'unterminated", "INSERT INTO t VALUES('unterminated",
	"INSERT INTO t VALUES(x'0g')", "SELECT 123abc", "SELECT 0x",
	`SELECT "unterminated`, "SELECT [unterminated",
	"", "   ", "-- only a comment", "/* only a comment */",
	"1 + 1", ";", "(SELECT 1)",
}

// TestLeadingVerbMemoMatchesLeadingStatementVerb runs the battery twice on ONE
// session -- so every call but the first sees a populated memo, and the second
// pass sees a memo holding the LAST text rather than this one -- and demands
// the uncached answer every time.
func TestLeadingVerbMemoMatchesLeadingStatementVerb(t *testing.T) {
	db := &DB{}
	for pass := 0; pass < 2; pass++ {
		for _, sqlText := range leadingVerbTexts {
			wantVerb, wantOK := LeadingStatementVerb(sqlText)
			gotVerb, gotOK := db.leadingVerb(sqlText)
			if gotVerb != wantVerb || gotOK != wantOK {
				t.Errorf("pass %d: leadingVerb(%q) = (%q, %v), want (%q, %v)", pass, sqlText, gotVerb, gotOK, wantVerb, wantOK)
			}
		}
	}
	// The specific failure a one-entry memo can produce: two texts alternating,
	// so a hit is never the one just stored. Distinct verbs make a stale answer
	// visible rather than accidentally right.
	for i := 0; i < 4; i++ {
		for _, sqlText := range []string{"SELECT * FROM t", "INSERT INTO t VALUES(1)", "DROP TABLE t", "SELECT * FROM t"} {
			wantVerb, wantOK := LeadingStatementVerb(sqlText)
			if gotVerb, gotOK := db.leadingVerb(sqlText); gotVerb != wantVerb || gotOK != wantOK {
				t.Fatalf("alternating round %d: leadingVerb(%q) = (%q, %v), want (%q, %v)", i, sqlText, gotVerb, gotOK, wantVerb, wantOK)
			}
		}
	}
	// The empty string is the memo's ZERO VALUE key, so it is the one text
	// whose answer can be served from a memo that was never written.
	if verb, ok := (&DB{}).leadingVerb(""); verb != "" || ok {
		t.Errorf(`cold leadingVerb("") = (%q, %v), want ("", false)`, verb, ok)
	}
}

// TestMainReadTxnOfClassificationUnchanged is the end of the chain the memo
// feeds: for every shape, the answer a session that has already classified
// other statements gives must equal the answer a COLD session gives. That
// needs no hand-written expectations to be a real gate -- a stale verb turns
// one statement's classification into another's, which is exactly the
// difference this compares.
func TestMainReadTxnOfClassificationUnchanged(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *Session {
		db, err := Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, _, err := db.ExecArgs("CREATE TABLE t(a)", nil); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		return db
	}
	warm := open("warm.musq")
	defer warm.Close()
	for i, sqlText := range leadingVerbTexts {
		// A cold session per text: same schema, empty memo, nothing else run.
		cold := open(fmt.Sprintf("cold%d.musq", i))
		want := cold.mainReadTxnOf(sqlText)
		cold.Close()
		if got := warm.mainReadTxnOf(sqlText); got != want {
			t.Errorf("mainReadTxnOf(%q) = %v on a warm session, %v on a cold one", sqlText, got, want)
		}
	}

	// A handful of the measured memberships from mainReadTxnOf's own doc
	// comment, spelled out so the comparison above cannot pass by classifying
	// everything UNKNOWN on both sides.
	fresh := open("pinned.musq")
	defer fresh.Close()
	for _, tc := range []struct {
		sqlText string
		want    mainReadTxn
	}{
		{"SELECT * FROM t", mainReadTxnYes},
		{"SELECT 1", mainReadTxnUnknown}, // no FROM: not vacuously YES
		{"INSERT INTO t VALUES(1)", mainReadTxnYes},
		{"PRAGMA page_count", mainReadTxnYes},
		{"BEGIN", mainReadTxnNo},
		{"BEGIN IMMEDIATE", mainReadTxnYes},
		{"COMMIT", mainReadTxnNo},
		{"PRAGMA locking_mode = normal", mainReadTxnNo},
		{"INSERT INTO t VALUES('unterminated", mainReadTxnUnknown},
		{"", mainReadTxnUnknown},
	} {
		if got := fresh.mainReadTxnOf(tc.sqlText); got != tc.want {
			t.Errorf("mainReadTxnOf(%q) = %v, want %v", tc.sqlText, got, tc.want)
		}
	}
}
