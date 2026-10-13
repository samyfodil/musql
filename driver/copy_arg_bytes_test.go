package driver

import (
	"bytes"
	"strings"
	"testing"
)

// A carved argument is a copy capped at its own length: appending to one
// cannot reach the next, and a long one is not carved at all.
func TestCopyArgBytesCarvesCappedCopies(t *testing.T) {
	var arena []byte
	a := copyArgBytes(&arena, "abc")
	b := copyArgBytes(&arena, []byte("def"))
	if string(a) != "abc" || string(b) != "def" || cap(a) != 3 {
		t.Fatalf("a=%q cap %d, b=%q", a, cap(a), b)
	}
	a = append(a, 'X')
	if string(b) != "def" || string(a) != "abcX" {
		t.Fatalf("appending to one carved slice changed the next: %q", b)
	}
	if e := copyArgBytes(&arena, ""); e == nil || len(e) != 0 {
		t.Fatalf("empty string: %v", e)
	}
	long := strings.Repeat("x", argArenaMax+1)
	before := len(arena)
	if l := copyArgBytes(&arena, long); !bytes.Equal(l, []byte(long)) || len(arena) != before {
		t.Fatal("a long argument was carved from the arena")
	}
}
