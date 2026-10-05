package engine

import (
	"strings"
	"testing"
)

// TestParseAttachStmtLiteralForms tests ATTACH parsing of literal paths and
// names, including keywords, NULL, and numeric forms
func TestParseAttachStmtLiteralForms(t *testing.T) {
	cases := []struct {
		sql      string
		wantPath string
		wantName string
	}{
		{"ATTACH DATABASE x AS y2", "x", "y2"}, // bare identifier: its own spelling, not a column
		{`ATTACH "x" AS "y2"`, "x", "y2"},      // double-quoted: identical AST node to bare
		{"ATTACH `x` AS `y2`", "x", "y2"},      // backtick-quoted: same
		{"ATTACH [x] AS [y2]", "x", "y2"},      // bracket-quoted: same
		{"ATTACH 'ON' AS 'ON'", "ON", "ON"},    // quoted reserved keyword: fine
		{"ATTACH NULL AS x", "", "x"},          // NULL path -> "" (anonymous private db)
		{"ATTACH 'f' AS NULL", "f", ""},        // NULL name -> ""
		{"ATTACH NULL AS NULL", "", ""},        // both
		{"ATTACH 456 AS x", "456", "x"},        // numeric path: value text
		{"ATTACH 0123 AS x", "123", "x"},       // value text, not source spelling
		{"ATTACH 4.5 AS x", "4.5", "x"},        // float path
		{"ATTACH 'f' AS 123", "f", "123"},      // numeric name (attach_numeric_name_test.go's own case, mirrored here for the path side)
		{"ATTACH DATABASE ? AS ?", "", ""},     // unbound parameter -> NULL -> ""
		{"ATTACH DATABASE '' AS ?", "", ""},    // path already '', name unbound
	}
	for _, tc := range cases {
		path, name, ok, err := ParseAttachStmt(tc.sql)
		if !ok {
			t.Errorf("%s: ParseAttachStmt reported ok=false, want a recognized ATTACH", tc.sql)
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.sql, err)
			continue
		}
		if path != tc.wantPath || name != tc.wantName {
			t.Errorf("%s: got path=%q name=%q, want path=%q name=%q", tc.sql, path, name, tc.wantPath, tc.wantName)
		}
	}
}

// TestParseAttachStmtBareKeywordPathIsSyntaxError verifies unquoted reserved
// keywords in path position cause syntax errors
func TestParseAttachStmtBareKeywordPathIsSyntaxError(t *testing.T) {
	_, _, ok, err := ParseAttachStmt("ATTACH ON AS x")
	if !ok || err == nil || !strings.Contains(err.Error(), `near "ON": syntax error`) {
		t.Errorf(`ATTACH ON AS x: want ok=true err containing near "ON": syntax error, got ok=%v err=%v`, ok, err)
	}
}

// TestParseAttachStmtPathExpression tests ATTACH path expressions including
// concatenation, functions, and literals
func TestParseAttachStmtPathExpression(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want string
	}{
		{"ATTACH 'a' || 'b' AS x", "ab"},
		{"ATTACH printf('%s.db', 'x') AS y", "x.db"},
		{"ATTACH DATABASE ('a')||('.db') AS y", "a.db"},
		{"ATTACH ':mem' || 'ory:' AS test1", ":memory:"}, // auth.test's own case
		{"ATTACH 1+1 AS y", "2"},
		{"ATTACH NULL||'x' AS y", ""}, // NULL propagates, then TEXT-affinity NULL -> ""
		{"ATTACH lower('AB.DB') AS y", "ab.db"},
		{"ATTACH CAST(7 AS TEXT) AS y", "7"},
		{"ATTACH x'6162' AS y", "ab"}, // a BLOB path is its raw bytes as text
		{"ATTACH ('a') AS y", "a"},
		// FROM-less subquery
		{"ATTACH (SELECT 'q.db') AS y", "q.db"},
		// Literal tokens
		{"ATTACH x AS y", "x"},
		{"ATTACH 0123 AS y", "123"},
	} {
		path, _, ok, err := ParseAttachStmt(tc.sql)
		if !ok || err != nil {
			t.Errorf("%s: want it accepted, got ok=%v err=%v", tc.sql, ok, err)
			continue
		}
		if path != tc.want {
			t.Errorf("%s: got path=%q, want %q", tc.sql, path, tc.want)
		}
	}
}

// TestParseAttachStmtStillDeclinesNonConstantPaths pins the boundary that
// remains: "no row context" is not a limitation of this parser but SQLite's own
// rule for ATTACH, so an expression that needs a row -- a column reference, a
// subquery -- has nothing to resolve against and is declined rather than
// guessed at.
func TestParseAttachStmtStillDeclinesNonConstantPaths(t *testing.T) {
	// The last two are the never-panic half: a subquery over a real table, and
	// an unknown function, both reach the evaluator with no pager at all --
	// exactly the state a nil dereference would show up in. C SQLite rejects
	// both too ("no such column: f", "no such function: nosuchfunc").
	cases := []string{
		"ATTACH t.c AS y",
		"ATTACH (SELECT f FROM t) AS y",
		"ATTACH nosuchfunc('x') AS y",
	}
	for _, sql := range cases {
		_, _, ok, err := ParseAttachStmt(sql)
		if !ok || err == nil {
			t.Errorf("%s: want a declined ATTACH (ok=true, non-nil err), got ok=%v err=%v", sql, ok, err)
		}
	}
}

// TestParseAttachStmtNeverPanics runs a whole expression PARSER and EVALUATOR
// over ATTACH's path now, with no pager, no row and no bound parameters -- the
// state in which a missing nil check shows up. AGENTS.md's never-panic
// invariant is the point; the RESULT does not matter, only that each of these
// returns rather than crashing.
func TestParseAttachStmtNeverPanics(t *testing.T) {
	for _, sql := range []string{
		"ATTACH", "ATTACH DATABASE", "ATTACH AS", "ATTACH DATABASE AS x",
		"ATTACH ( AS x", "ATTACH ) AS x", "ATTACH ((((( AS x", "ATTACH , AS x",
		"ATTACH ? AS y", "ATTACH ?||'x' AS y", "ATTACH :n || 'x' AS y",
		"ATTACH CASE WHEN 1 THEN 'a' END AS y",
		"ATTACH (SELECT) AS y", "ATTACH (SELECT * FROM) AS y",
		"ATTACH EXISTS(SELECT 1) AS y", "ATTACH 1 IN (SELECT 1) AS y",
		"ATTACH raise(ignore) AS y", "ATTACH sum(1) AS y", "ATTACH x'zz' AS y",
		"ATTACH 'a' COLLATE nosuchcoll AS y", "ATTACH -'a' AS y", "ATTACH ~NULL AS y",
		"ATTACH 1/0 AS y", "ATTACH CAST('x' AS NOSUCHTYPE) AS y",
		"ATTACH 'a' AS", "ATTACH 'a' AS (", "ATTACH 'a' AS 'b' KEY",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: PANIC %v", sql, r)
				}
			}()
			ParseAttachStmt(sql)
		}()
	}
}

// TestExecAttachUnboundParamMatchesOracle runs the EXEC-level round trip for
// attach3.test's own "?"-parameterized shapes (section 12), which real
// SQLite runs through Exec (never Query -- see attach.go's package comment
// for why that distinction matters here) with the parameters left unbound:
// an unbound host parameter's value is SQL NULL by SQLite's own contract, so
// each of these attaches/detaches a database named "" -- exactly the
// sequence attach3-12.2/12.4/12.7 pins, and exactly what
// TestParseAttachStmtLiteralForms already confirmed the PARSER resolves;
// this confirms execAttach/execDetach honor it end to end (the "" path
// triggers execAttach's own isMem branch, so nothing lands outside
// os.TempDir() -- see AGENTS.md's no-stray-dbs rule).
func TestExecAttachUnboundParamMatchesOracle(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(dir+"/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Exec("ATTACH DATABASE ? AS ?"); err != nil {
		t.Fatalf("ATTACH DATABASE ? AS ?: %v", err)
	}
	if err := db.Exec("DETACH ?"); err != nil {
		t.Fatalf("DETACH ? (undoing the attach above): %v", err)
	}
	if err := db.Exec("ATTACH DATABASE '' AS ?"); err != nil {
		t.Fatalf("ATTACH DATABASE '' AS ?: %v", err)
	}
	if err := db.Exec("DETACH ''"); err != nil {
		t.Fatalf("DETACH '' (undoing the attach above): %v", err)
	}
	// A SECOND unbound-name attach while the first is still live collides,
	// exactly like two explicit ATTACH '' AS '' statements would (attach3-
	// 12.12's own case) -- proving the resolved name really is the constant
	// "" and not, say, a fresh value each time.
	if err := db.Exec("ATTACH null AS null"); err != nil {
		t.Fatalf("ATTACH null AS null: %v", err)
	}
	if err := db.Exec("ATTACH DATABASE ? AS ?"); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf(`ATTACH DATABASE ? AS ? while "" is already attached: want "already in use", got %v`, err)
	}
	if err := db.Exec("DETACH ''"); err != nil {
		t.Fatalf("DETACH '' (final cleanup): %v", err)
	}
}
