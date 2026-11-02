package engine

import (
	"math"
	"math/rand"
	"testing"
)

// TestAggDistinctHashFollowsTheComparison checks aggDistinctHash's contract:
// values compareValuesCollatedEnc calls equal hash alike, in every encoding
// and under every collation it dispatches on.
func TestAggDistinctHashFollowsTheComparison(t *testing.T) {
	alphabet := []string{"a", "A", "b", "B", " ", "\x00", "é", "É", "\xff", "\xed\xa0\xbd", "😀", "😁"}
	r := rand.New(rand.NewSource(1))
	vals := []Value{
		{Typ: Null}, {Typ: Int, I: 2}, {Typ: Float, F: 2}, {Typ: Int, I: 0}, {Typ: Float, F: math.Copysign(0, -1)},
		{Typ: Blob, S: []byte("2")}, {Typ: Text, S: []byte("2")}, {Typ: Int, I: 1<<53 + 1}, {Typ: Float, F: 1 << 53},
		{Typ: Int, I: math.MinInt64}, {Typ: Float, F: -9223372036854775808}, {Typ: Float, F: 9223372036854775808}, {Typ: Float, F: 0.5},
	}
	for range 400 {
		var s []byte
		for n := r.Intn(5); n > 0; n-- {
			s = append(s, alphabet[r.Intn(len(alphabet))]...)
		}
		vals = append(vals, Value{Typ: Text, S: s})
	}
	equalPairs := 0
	for _, enc := range []TextEncoding{UTF8, UTF16LE, UTF16BE} {
		for _, coll := range []string{"", "BINARY", "NOCASE", "nocase", "RTRIM", "custom"} {
			for _, a := range vals {
				for _, b := range vals {
					if compareValuesCollatedEnc(a, b, coll, enc) != 0 {
						continue
					}
					equalPairs++
					if aggDistinctHash(a, coll, enc) != aggDistinctHash(b, coll, enc) {
						t.Fatalf("enc %d coll %q: %v and %v compare equal but hash apart", enc, coll, a, b)
					}
				}
			}
		}
	}
	if equalPairs < 10000 {
		t.Fatalf("only %d equal pairs: the values no longer exercise the hash", equalPairs)
	}
	nul := func(s string) Value { return Value{Typ: Text, S: []byte(s)} }
	if a, b := nul("a\x00b"), nul("A\x00c"); compareValuesCollatedEnc(a, b, "NOCASE", UTF8) != 0 ||
		aggDistinctHash(a, "NOCASE", UTF8) != aggDistinctHash(b, "NOCASE", UTF8) {
		t.Fatal("NOCASE stops at the first NUL")
	}
}

// TestAggDistinctSeenProbesItsBucket checks that the probe returns the first
// spelling of a value and keeps values that only share a hash apart.
func TestAggDistinctSeenProbesItsBucket(t *testing.T) {
	it := &aggItem{distinct: true}
	if _, dup := it.distinctSeen(Value{Typ: Int, I: 1}, "", UTF8); dup {
		t.Fatal("first value reported as seen")
	}
	if s, dup := it.distinctSeen(Value{Typ: Float, F: 1}, "", UTF8); !dup || s.Typ != Int {
		t.Fatalf("REAL 1.0 after INTEGER 1: %v %v", s, dup)
	}
	for _, s := range []string{"é", "É", "ü"} {
		if _, dup := it.distinctSeen(Value{Typ: Text, S: []byte(s)}, "NOCASE", UTF8); dup {
			t.Fatalf("%q reported as seen", s)
		}
	}
	// A different value chained into x's bucket must not match x.
	h := aggDistinctHash(Value{Typ: Text, S: []byte("x")}, "", UTF8)
	it.seen = append(it.seen, Value{Typ: Text, S: []byte("y")})
	it.seenNext = append(it.seenNext, it.seenHead.swap(h, len(it.seen)))
	if _, dup := it.distinctSeen(Value{Typ: Text, S: []byte("x")}, "", UTF8); dup {
		t.Fatal("a different value in the same bucket reported as seen")
	}
	if len(it.seen) != 6 {
		t.Fatalf("seen has %d values, want 6", len(it.seen))
	}
}
