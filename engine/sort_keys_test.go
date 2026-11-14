package engine

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

// TestSortKeyOrdersAsBinary: two values' sort keys compare as the values do
// under BINARY in a UTF-8 database, ascending and descending.
func TestSortKeyOrdersAsBinary(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	strs := [][]byte{nil, {}, {0}, {0, 0}, {0, 1}, {1}, {'a'}, {'a', 0}, {'a', 0, 'b'}, {'a', 'b'}, {'a', 0xff}, {0xff}, {0xff, 0}}
	ints := []int64{math.MinInt64, math.MinInt64 + 1, -256, -1, 0, 1, 255, 256, math.MaxInt64}
	gen := func() Value {
		switch r.Intn(4) {
		case 0:
			return Value{}
		case 1:
			return Value{Typ: Int, I: ints[r.Intn(len(ints))]}
		case 2:
			return Value{Typ: Text, S: strs[r.Intn(len(strs))]}
		default:
			return Value{Typ: Blob, S: strs[r.Intn(len(strs))]}
		}
	}
	for range 20000 {
		a, b := []Value{gen(), gen()}, []Value{gen(), gen()}
		for _, desc := range []bool{false, true} {
			var ka, kb []byte
			for i := range a {
				ka, _ = appendSortKey(ka, a[i], desc && i == 1)
				kb, _ = appendSortKey(kb, b[i], desc && i == 1)
			}
			want := 0
			for i := range a {
				if c := compareValuesCollatedEnc(a[i], b[i], "", UTF8); c != 0 {
					if desc && i == 1 {
						c = -c
					}
					want = c
					break
				}
			}
			if got := bytes.Compare(ka, kb); got != want {
				t.Fatalf("%v vs %v (desc %v): keys compare %d, values %d", a, b, desc, got, want)
			}
		}
	}
	if _, ok := appendSortKey(nil, Value{Typ: Float, F: 1}, false); ok {
		t.Fatal("a REAL has a sort key")
	}
}
