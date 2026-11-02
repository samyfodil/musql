package engine

import (
	"bytes"
	"math/rand"
	"testing"
)

// substrByStarts is substr over TEXT as it was computed before the walk: every
// character start found first, then the range sliced out of them.
func substrByStarts(s []byte, y int64, hasZ bool, z int64) []byte {
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	starts := []int{}
	for i := 0; i < len(s); i += sqliteUTF8Skip(s[i:]) {
		starts = append(starts, i)
	}
	starts = append(starts, len(s))
	start, count := substrRange(len(starts)-1, y, hasZ, z)
	return s[starts[start]:starts[start+count]]
}

// TestSubstrWalkMatchesTheStarts checks substrFunc's walk against slicing
// between precomputed character starts, over text with malformed UTF-8 and
// NULs and over positions and lengths of either sign.
func TestSubstrWalkMatchesTheStarts(t *testing.T) {
	pieces := []string{"a", "é", "日", "😀", "\xff", "\xc3", "\x80", "\x00", "z"}
	r := rand.New(rand.NewSource(1))
	for range 20000 {
		var s []byte
		for n := r.Intn(8); n > 0; n-- {
			s = append(s, pieces[r.Intn(len(pieces))]...)
		}
		y := int64(r.Intn(21) - 10)
		z := int64(r.Intn(21) - 10)
		hasZ := r.Intn(4) != 0
		args := []Value{{Typ: Text, S: s}, {Typ: Int, I: y}}
		if hasZ {
			args = append(args, Value{Typ: Int, I: z})
		}
		got := fnSubstr(args)
		want := substrByStarts(s, y, hasZ, z)
		if got.Typ != Text || !bytes.Equal(got.S, want) {
			t.Fatalf("substr(%q, %d, %v %d) = %q, want %q", s, y, hasZ, z, got.S, want)
		}
	}
	if got := fnSubstr([]Value{{Typ: Int, I: 12345}, {Typ: Int, I: -2}}); string(got.S) != "45" {
		t.Fatalf("substr(12345, -2) = %q", got.S)
	}
	if got := fnSubstr([]Value{{Typ: Text, S: []byte("abcdefghij")}, {Typ: Int, I: -1 << 62}}); len(got.S) != 0 {
		t.Fatalf("a hugely negative start: %q", got.S)
	}
}
