package engine

import (
	"strings"
	"testing"
)

// TestSplitStatements pins the boundaries a ";"-separated script has. The
// cases that matter are the ones a text-level split gets wrong: a semicolon
// inside a string, a comment or a trigger body.
func TestSplitStatements(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want []string
	}{
		{"single", `SELECT 1`, []string{`SELECT 1`}},
		{"single-trailing-semi", `SELECT 1;`, []string{`SELECT 1`}},
		{"two", `CREATE TABLE u(x); INSERT INTO u VALUES(1)`,
			[]string{`CREATE TABLE u(x)`, `INSERT INTO u VALUES(1)`}},
		{"three", `CREATE TABLE u(x); INSERT INTO u VALUES(1); INSERT INTO u VALUES(2)`,
			[]string{`CREATE TABLE u(x)`, `INSERT INTO u VALUES(1)`, `INSERT INTO u VALUES(2)`}},
		{"leading-semi", `; CREATE TABLE u(x)`, []string{`CREATE TABLE u(x)`}},
		{"doubled-semi", `CREATE TABLE u(x);; INSERT INTO u VALUES(1)`,
			[]string{`CREATE TABLE u(x)`, `INSERT INTO u VALUES(1)`}},
		{"only-semis", `;;;`, nil},
		{"empty", ``, nil},
		{"whitespace", "  \n\t ", nil},
		// a ";" that is NOT a boundary
		{"semi-in-string", `CREATE TABLE u(x); INSERT INTO u VALUES('a;b')`,
			[]string{`CREATE TABLE u(x)`, `INSERT INTO u VALUES('a;b')`}},
		{"semi-in-quoted-ident", `CREATE TABLE "a;b"(x); SELECT 1`,
			[]string{`CREATE TABLE "a;b"(x)`, `SELECT 1`}},
		{"semi-in-line-comment", "CREATE TABLE u(x); -- a; comment\nINSERT INTO u VALUES(1)",
			[]string{`CREATE TABLE u(x)`, `INSERT INTO u VALUES(1)`}},
		{"semi-in-block-comment", "CREATE TABLE u(x); /* a; b */ INSERT INTO u VALUES(1)",
			[]string{`CREATE TABLE u(x)`, `INSERT INTO u VALUES(1)`}},
		// TRIGGER BODIES: the inner ";" must not split
		{"trigger", `CREATE TRIGGER tr AFTER INSERT ON u BEGIN INSERT INTO l VALUES(new.x); END; SELECT 1`,
			[]string{`CREATE TRIGGER tr AFTER INSERT ON u BEGIN INSERT INTO l VALUES(new.x); END`, `SELECT 1`}},
		{"trigger-two-steps", `CREATE TRIGGER tr AFTER INSERT ON u BEGIN INSERT INTO l VALUES(1); INSERT INTO l VALUES(2); END; SELECT 1`,
			[]string{`CREATE TRIGGER tr AFTER INSERT ON u BEGIN INSERT INTO l VALUES(1); INSERT INTO l VALUES(2); END`, `SELECT 1`}},
		{"trigger-temp", `CREATE TEMP TRIGGER tr AFTER INSERT ON u BEGIN SELECT 1; END; SELECT 2`,
			[]string{`CREATE TEMP TRIGGER tr AFTER INSERT ON u BEGIN SELECT 1; END`, `SELECT 2`}},
		{"trigger-no-trailing-semi", `CREATE TRIGGER tr AFTER INSERT ON u BEGIN SELECT 1; END`,
			[]string{`CREATE TRIGGER tr AFTER INSERT ON u BEGIN SELECT 1; END`}},
		// a CASE ... END INSIDE the body must not close it early
		{"trigger-case", `CREATE TRIGGER tr AFTER INSERT ON u BEGIN SELECT CASE WHEN 1 THEN 2 ELSE 3 END; INSERT INTO l VALUES(1); END; SELECT 9`,
			[]string{`CREATE TRIGGER tr AFTER INSERT ON u BEGIN SELECT CASE WHEN 1 THEN 2 ELSE 3 END; INSERT INTO l VALUES(1); END`, `SELECT 9`}},
		// ...and one in the WHEN clause, BEFORE the body opens
		{"trigger-case-in-when", `CREATE TRIGGER tr AFTER INSERT ON u WHEN CASE WHEN new.x THEN 1 ELSE 0 END BEGIN SELECT 1; END; SELECT 9`,
			[]string{`CREATE TRIGGER tr AFTER INSERT ON u WHEN CASE WHEN new.x THEN 1 ELSE 0 END BEGIN SELECT 1; END`, `SELECT 9`}},
		// a table named "trigger" must NOT be treated as one
		{"create-table-not-trigger", `CREATE TABLE trigger_log(x); INSERT INTO trigger_log VALUES(1)`,
			[]string{`CREATE TABLE trigger_log(x)`, `INSERT INTO trigger_log VALUES(1)`}},
		{"quoted-trigger-ident", `CREATE TABLE "trigger"(x); SELECT 1`,
			[]string{`CREATE TABLE "trigger"(x)`, `SELECT 1`}},
		// a plain BEGIN outside a trigger is its own statement
		{"begin-commit", `BEGIN; INSERT INTO u VALUES(1); COMMIT`,
			[]string{`BEGIN`, `INSERT INTO u VALUES(1)`, `COMMIT`}},
		// a CASE ... END outside any trigger is not a body
		{"case-outside", `SELECT CASE WHEN 1 THEN 2 END; SELECT 3`,
			[]string{`SELECT CASE WHEN 1 THEN 2 END`, `SELECT 3`}},
	} {
		got, err := SplitStatements(c.in)
		if err != nil {
			t.Errorf("[%s] SplitStatements(%q): %v", c.name, c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("[%s] SplitStatements(%q)\n  got  %d: %q\n  want %d: %q",
				c.name, c.in, len(got), got, len(c.want), c.want)
			continue
		}
		for i := range got {
			if strings.TrimSpace(got[i]) != strings.TrimSpace(c.want[i]) {
				t.Errorf("[%s] statement %d\n  got  %q\n  want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// TestSplitStatementsLexError surfaces a lex failure rather than silently
// returning a partial split -- an unterminated string must not be reported as
// a valid one-statement script.
func TestSplitStatementsLexError(t *testing.T) {
	for _, in := range []string{`SELECT 'unterminated`, `SELECT "unterminated`} {
		if _, err := SplitStatements(in); err == nil {
			t.Errorf("SplitStatements(%q) returned no error", in)
		}
	}
}
