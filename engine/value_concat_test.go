package engine

import (
	"bytes"
	"math"
	"testing"
)

// TestConcatMatchesValueToText: concatValues builds its result in one buffer;
// it must be byte for byte valueToText(l) + valueToText(r) for every class.
func TestConcatMatchesValueToText(t *testing.T) {
	vals := []Value{
		{Typ: Null}, {Typ: Int, I: 0}, {Typ: Int, I: -7}, {Typ: Int, I: math.MinInt64}, {Typ: Int, I: math.MaxInt64},
		{Typ: Float, F: 1.5}, {Typ: Float, F: -0.0}, {Typ: Float, F: 1e300}, {Typ: Float, F: math.Inf(1)}, {Typ: Float, F: 3},
		{Typ: Text, S: []byte("")}, {Typ: Text, S: []byte("row-")}, {Typ: Text, S: []byte("é\x00z")},
		{Typ: Blob, S: []byte{0, 1, 0xff}}, {Typ: Blob, S: nil},
	}
	for _, l := range vals {
		for _, r := range vals {
			got := concatValues(l, r)
			if l.Typ == Null || r.Typ == Null {
				if got.Typ != Null {
					t.Fatalf("%v || %v = %v, want NULL", l, r, got)
				}
				continue
			}
			want := []byte(valueToText(l) + valueToText(r))
			if got.Typ != Text || !bytes.Equal(got.S, want) {
				t.Fatalf("%v || %v = %q, want %q", l, r, got.S, want)
			}
		}
	}
}
