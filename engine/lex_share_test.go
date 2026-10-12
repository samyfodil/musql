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
