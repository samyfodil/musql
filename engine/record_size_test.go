package engine

import (
	"bytes"
	"math"
	"testing"
)

// TestRecordSizeMatchesAppendRecord holds recordSize to the encoder it predicts,
// across every serial type and the header-length boundary (a header whose own
// varint length grows it past 127 bytes).
func TestRecordSizeMatchesAppendRecord(t *testing.T) {
	wide := make([]Value, 200)
	for i := range wide {
		wide[i] = Value{Typ: Int, I: int64(i) << (i % 60)}
	}
	cases := [][]Value{
		nil,
		{{Typ: Null}},
		{{Typ: Int, I: 0}, {Typ: Int, I: 1}, {Typ: Int, I: -1}, {Typ: Int, I: 127}, {Typ: Int, I: 1 << 40}, {Typ: Int, I: math.MinInt64}},
		{{Typ: Float, F: 1.5}, {Typ: Text, S: []byte("hello")}, {Typ: Blob, S: bytes.Repeat([]byte{7}, 10000)}},
		{{Typ: Text, S: nil}, {Typ: Blob, S: []byte{}}},
		wide,
	}
	for i, vals := range cases {
		if got, want := recordSize(vals), len(appendRecord(nil, vals)); got != want {
			t.Errorf("case %d: recordSize = %d, appendRecord wrote %d", i, got, want)
		}
	}
}
