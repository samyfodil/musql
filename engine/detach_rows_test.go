package engine

import (
	"bytes"
	"testing"
)

// TestDetachResultRowsCopiesIntoChunks: detached bytes equal what the rows
// held and no longer point at it, an empty value comes back nil, a value
// longer than a chunk is copied alone, and each copy is capped, so an append
// to one leaves the values beside it in its chunk as they were.
func TestDetachResultRowsCopiesIntoChunks(t *testing.T) {
	src := [][]byte{
		[]byte("abc"), {}, nil, bytes.Repeat([]byte{7}, detachChunk+1), []byte("defgh"),
		bytes.Repeat([]byte{1}, detachChunk-2), []byte("ij"),
	}
	want := make([][]byte, len(src))
	rows := make([][]Value, len(src))
	for i, b := range src {
		want[i] = bytes.Clone(b)
		rows[i] = []Value{{Typ: Int, I: int64(i)}, {Typ: Blob, S: b}}
	}
	check := func(stage string) {
		t.Helper()
		for i, r := range rows {
			got := r[1].S
			if len(want[i]) == 0 {
				if got != nil {
					t.Fatalf("%s: value %d is %v, want nil", stage, i, got)
				}
				continue
			}
			if !bytes.Equal(got[:len(want[i])], want[i]) || r[0].I != int64(i) {
				t.Fatalf("%s: value %d changed", stage, i)
			}
		}
	}
	detachResultRows(rows)
	check("detached")
	for i, r := range rows {
		if len(src[i]) > 0 && (&r[1].S[0] == &src[i][0] || cap(r[1].S) != len(r[1].S)) {
			t.Fatalf("value %d: aliases its source or has room past its end", i)
		}
	}
	for _, b := range src {
		for j := range b {
			b[j] ^= 0xff
		}
	}
	check("sources overwritten")
	for i := range rows {
		if len(rows[i][1].S) > 0 {
			rows[i][1].S = append(rows[i][1].S, 0xee, 0xee)
		}
	}
	check("appended to")
}
