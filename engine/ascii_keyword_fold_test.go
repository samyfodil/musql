// Keyword and identifier folding is ASCII-only, matching C SQLite.
package engine

import "testing"

const dotlessI = "ı"

func TestASCIIUpperMatchesSQLiteFold(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"insert", "INSERT"},
		{"INSERT", "INSERT"},
		{"InSeRt", "INSERT"},
		{"t1", "T1"},
		// Non-ASCII bytes must survive folding unchanged.
		{"RETURN" + dotlessI + "NG", "RETURN" + dotlessI + "NG"},
		{dotlessI + "nsert", dotlessI + "NSERT"},
		// Other non-ASCII characters stay unchanged.
		{"K", "K"},
		{"İ", "İ"},
		// A quoted identifier's payload may be arbitrary UTF-8.
		{"café", "CAFé"},
	} {
		if got := asciiUpper(tc.in); got != tc.want {
			t.Errorf("asciiUpper(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNonASCIIKeywordSpellingIsNotAKeyword verifies that non-ASCII spellings
// of keywords are not recognized as keywords.
func TestNonASCIIKeywordSpellingIsNotAKeyword(t *testing.T) {
	toks, err := lex(dotlessI + "NSERT INTO t VALUES(1)")
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	if got := toks[0].upper(); got == "INSERT" {
		t.Fatalf("token %q folded to %q -- it must not match the INSERT keyword "+
			"(SQLite: sqlite3UpperToLower, global.c:24, is ASCII-only)", toks[0].text, got)
	}

	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE t(a INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		dotlessI + "NSERT INTO t VALUES(2)",
		"INSERT " + dotlessI + "NTO t VALUES(3)",
	} {
		if err := db.Exec(q); err == nil {
			t.Errorf("%q was ACCEPTED; C SQLite reports a syntax error", q)
		}
	}
}
