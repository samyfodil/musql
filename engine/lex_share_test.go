package engine

import (
	"reflect"
	"strings"
	"testing"
)

// TestLexSharedTokens holds lex's one-entry memory of the last long text to
// lexing it afresh: the same tokens for the same text, others for another, and
// a slice that cannot be appended into the memory's own.
func TestLexSharedTokens(t *testing.T) {
	long := "INSERT INTO t VALUES " + strings.Repeat("(1, 'abcdefghijklmnop'), ", lexShareMin/20) + "(2, 'z')"
	other := strings.Replace(long, "'z'", "'y'", 1)
	want, err := lexUncached(long)
	if err != nil {
		t.Fatal(err)
	}
	first, err := lex(long)
	if err != nil {
		t.Fatal(err)
	}
	second, err := lex(long)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatal("shared tokens differ from a fresh lex")
	}
	if cap(second) != len(second) {
		t.Fatalf("spare capacity %d: an append would write into the shared slice", cap(second)-len(second))
	}
	extended := append(second, token{kind: tkEOF})
	extended[0].text = "changed"
	again, _ := lex(long)
	if !reflect.DeepEqual(again, want) {
		t.Fatal("an append changed the shared tokens")
	}
	got, err := lex(other)
	if err != nil {
		t.Fatal(err)
	}
	wantOther, _ := lexUncached(other)
	if !reflect.DeepEqual(got, wantOther) || reflect.DeepEqual(got, want) {
		t.Fatal("a different text got the last text's tokens")
	}
	if _, err := lex(long + " 'unterminated"); err == nil {
		t.Fatal("an unterminated string lexed")
	}
}

// TestLexMidTextShared: a statement too long for lexKept and up to
// lexLongMax, lexed as given and trimmed by turns, as the classifiers and the
// parser lex it, is lexed once in each spelling: every later lex hands back
// the first one's tokens, which are a fresh lex's, with no spare capacity.
func TestLexMidTextShared(t *testing.T) {
	for _, n := range []int{lexKeepMax + 1, 8 << 10, lexShareMin - 1, lexShareMin, 64 << 10, lexLongMax} {
		body := "SELECT " + strings.Repeat("a1 + b2, ", n/9)
		body = body[:n-len("x FROM t \n")] + "x FROM t"
		given := body + " \n"
		if len(given) != n {
			t.Fatalf("text of %d bytes, want %d", len(given), n)
		}
		texts := []string{given, strings.TrimSpace(given)}
		first := make([][]token, 2)
		for round := range 3 {
			for i, s := range texts {
				toks, err := lex(s)
				if err != nil {
					t.Fatal(err)
				}
				if cap(toks) != len(toks) {
					t.Fatalf("%d bytes: spare capacity %d", len(s), cap(toks)-len(toks))
				}
				if round == 0 {
					want, _ := lexUncached(s)
					if !reflect.DeepEqual(toks, want) {
						t.Fatalf("%d bytes: shared tokens differ from a fresh lex", len(s))
					}
					first[i] = toks
				} else if &toks[0] != &first[i][0] {
					t.Errorf("%d bytes, round %d: lexed again", len(s), round)
				}
			}
		}
	}
}
