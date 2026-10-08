package engine

import (
	"math/rand"
	"testing"
)

// TestDistinctSetMatchesTheScan: the hashed set must decide every key exactly
// as the linear scan over every seen key does, under each collation and class.
func TestDistinctSetMatchesTheScan(t *testing.T) {
	vals := []Value{
		{Typ: Null}, {Typ: Int, I: 0}, {Typ: Int, I: 1}, {Typ: Int, I: -1}, {Typ: Int, I: 1 << 53}, {Typ: Int, I: 1<<53 + 1},
		{Typ: Int, I: 9223372036854775807}, {Typ: Int, I: -9223372036854775808},
		{Typ: Float, F: 0}, {Typ: Float, F: 1}, {Typ: Float, F: -1}, {Typ: Float, F: 1.5}, {Typ: Float, F: 1 << 53},
		{Typ: Float, F: 9223372036854775807}, {Typ: Float, F: -9223372036854775808}, {Typ: Float, F: 1e300},
		{Typ: Text, S: []byte("a")}, {Typ: Text, S: []byte("A")}, {Typ: Text, S: []byte("a  ")}, {Typ: Text, S: []byte("A ")},
		{Typ: Text, S: []byte("")}, {Typ: Text, S: []byte(" ")}, {Typ: Text, S: []byte("1")},
		{Typ: Text, S: []byte("é")}, {Typ: Text, S: []byte("É")}, {Typ: Text, S: []byte("\xff")}, {Typ: Text, S: []byte("\xfe")},
		{Typ: Text, S: []byte("\xffa")}, {Typ: Text, S: []byte("\xef\xbf\xbd")},
		{Typ: Blob, S: []byte("a")}, {Typ: Blob, S: []byte("")}, {Typ: Blob, S: []byte("A")},
	}
	rng := rand.New(rand.NewSource(1))
	for _, colls := range [][]string{nil, {"NOCASE"}, {"RTRIM"}, {"BINARY", "NOCASE"}, {"RTRIM", "NOCASE"}} {
		for _, enc := range []TextEncoding{UTF8, UTF16LE} {
			width := len(colls)
			if width == 0 {
				width = 2
			}
			set := newDistinctSet(colls, enc)
			var seen [][]Value
			for n := 0; n < 4000; n++ {
				key := make([]Value, width)
				for i := range key {
					key[i] = vals[rng.Intn(len(vals))]
				}
				want := false
				for _, k := range seen {
					if keysEqualGrouping(k, key, colls, enc) {
						want = true
						break
					}
				}
				if !want {
					seen = append(seen, key)
				}
				if got := set.checkAndAdd(key); got != want {
					t.Fatalf("colls %v enc %v key %v: hashed %v, scan %v", colls, enc, key, got, want)
				}
			}
		}
	}
}
