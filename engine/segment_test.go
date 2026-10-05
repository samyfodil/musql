package engine

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// A segment must hand back EXACTLY the values it was given -- every storage
// class, NULLs, short rows, and the values that did not fit their column's
// physical type and went to the exception list. That last case is the one the
// whole format rests on: a value which is neither in its block nor in the side
// list is not slow, it is GONE.
func TestSegmentRoundTrip(t *testing.T) {
	cols := make([]columnInfo, 6)
	for i := range cols {
		cols[i].Name = fmt.Sprintf("c%d", i)
	}
	rng := rand.New(rand.NewSource(0x5eed))
	rows := make([][]Value, 0, 500)
	for i := 0; i < 500; i++ {
		r := []Value{
			{Typ: Int, I: int64(i)},                                  // cleanly int
			{Typ: Text, S: []byte(fmt.Sprintf("t%d", i))},            // cleanly text
			{Typ: Float, F: float64(i) + 0.5},                        // cleanly real
			{Typ: Null},                                              // always NULL
			{Typ: Int, I: int64(rng.Intn(100))},                      // int with strays below
			{Typ: Blob, S: []byte{byte(i), byte(i >> 8)}},            // cleanly blob
		}
		switch {
		case i == 7: // a stray TEXT in an int column -> exception list
			r[4] = Value{Typ: Text, S: []byte("stray")}
		case i == 11: // a stray REAL in the same column -> exception list
			r[4] = Value{Typ: Float, F: math.Pi}
		case i == 23: // a NULL in an otherwise-populated column
			r[0] = Value{Typ: Null}
		case i == 31: // a row that predates an ADD COLUMN: short
			r = r[:3]
		}
		rows = append(rows, r)
	}

	buf, err := buildSegment(cols, segTestRowids(len(rows)), rows)
	if err != nil {
		t.Fatalf("buildSegment: %v", err)
	}
	s, err := openSegment(buf)
	if err != nil {
		t.Fatalf("openSegment: %v", err)
	}
	if s.nRows != len(rows) {
		t.Fatalf("segment holds %d rows, want %d", s.nRows, len(rows))
	}
	if len(s.exc) == 0 {
		t.Fatal("no exceptions were recorded; this test would not be exercising the side list")
	}

	for i, r := range rows {
		for c := range cols {
			want := Value{Typ: Null}
			if c < len(r) {
				want = r[c]
			}
			got := s.Value(c, i)
			if got.Typ != want.Typ {
				t.Fatalf("row %d col %d: got type %v, want %v", i, c, got.Typ, want.Typ)
			}
			switch want.Typ {
			case Int:
				if got.I != want.I {
					t.Fatalf("row %d col %d: got %d, want %d", i, c, got.I, want.I)
				}
			case Float:
				if got.F != want.F {
					t.Fatalf("row %d col %d: got %v, want %v", i, c, got.F, want.F)
				}
			case Text, Blob:
				if !bytes.Equal(got.S, want.S) {
					t.Fatalf("row %d col %d: got %q, want %q", i, c, got.S, want.S)
				}
			}
		}
	}
}

// The zero-copy path is the format's entire reason to exist: a cleanly-typed
// int64 column must come back as a Go slice that aliases the segment buffer,
// with no decode and no copy, and must agree value for value with the general
// accessor.
func TestSegmentInt64ColumnIsZeroCopy(t *testing.T) {
	cols := []columnInfo{{Name: "a"}, {Name: "b"}}
	rows := make([][]Value, 1000)
	for i := range rows {
		rows[i] = []Value{{Typ: Int, I: int64(i * 7)}, {Typ: Text, S: []byte("x")}}
	}
	buf, err := buildSegment(cols, segTestRowids(len(rows)), rows)
	if err != nil {
		t.Fatalf("buildSegment: %v", err)
	}
	s, err := openSegment(buf)
	if err != nil {
		t.Fatalf("openSegment: %v", err)
	}
	col, ok := s.Int64Column(0)
	if !ok {
		t.Fatal("a cleanly-typed int column did not hand back a slice")
	}
	if len(col) != len(rows) {
		t.Fatalf("slice holds %d, want %d", len(col), len(rows))
	}
	for i := range rows {
		if col[i] != int64(i*7) {
			t.Fatalf("row %d: got %d, want %d", i, col[i], i*7)
		}
		if v := s.Value(0, i); v.I != col[i] {
			t.Fatalf("row %d: slice says %d, accessor says %d", i, col[i], v.I)
		}
	}
	// It ALIASES the buffer: proving that is what proves there was no copy.
	col[0] = -12345
	if got := s.Value(0, 0); got.I != -12345 {
		t.Fatalf("writing through the slice did not change the segment: accessor says %d", got.I)
	}
	// ...and a TEXT column is not an int64 column.
	if _, ok := s.Int64Column(1); ok {
		t.Fatal("a text column handed back an int64 slice")
	}
}

// A column too mixed to type falls back rather than losing values: every value
// still reads back, which is the promise that makes the fast path safe to take.
func TestSegmentTaggedFallbackKeepsEveryValue(t *testing.T) {
	cols := []columnInfo{{Name: "a"}}
	var rows [][]Value
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			rows = append(rows, []Value{{Typ: Int, I: int64(i)}})
		} else {
			rows = append(rows, []Value{{Typ: Text, S: []byte(fmt.Sprintf("s%d", i))}})
		}
	}
	buf, err := buildSegment(cols, segTestRowids(len(rows)), rows)
	if err != nil {
		t.Fatalf("buildSegment: %v", err)
	}
	s, err := openSegment(buf)
	if err != nil {
		t.Fatalf("openSegment: %v", err)
	}
	if _, ok := s.Int64Column(0); ok {
		t.Fatal("an evenly-mixed column claimed to be a fixed-width int64 block")
	}
	for i, r := range rows {
		got, want := s.Value(0, i), r[0]
		if got.Typ != want.Typ || got.I != want.I || !bytes.Equal(got.S, want.S) {
			t.Fatalf("row %d: got %+v, want %+v", i, got, want)
		}
	}
}

// A truncated or corrupt segment is an ERROR at open, never a panic in a scan:
// the accessors are branch-light precisely because open validated the offsets.
func TestSegmentRejectsMalformed(t *testing.T) {
	cols := []columnInfo{{Name: "a"}}
	rows := [][]Value{{{Typ: Int, I: 1}}, {{Typ: Int, I: 2}}}
	buf, err := buildSegment(cols, segTestRowids(len(rows)), rows)
	if err != nil {
		t.Fatalf("buildSegment: %v", err)
	}
	if _, err := openSegment(buf[:len(buf)-1]); err == nil {
		t.Error("a truncated segment opened cleanly")
	}
	if _, err := openSegment([]byte("nope")); err == nil {
		t.Error("a non-segment opened cleanly")
	}
	bad := append([]byte(nil), buf...)
	bad[segHeaderSize+4] = 0xff // a block offset far past the buffer
	if _, err := openSegment(bad); err == nil {
		t.Error("a segment whose block overruns the buffer opened cleanly")
	}
}

// segTestRowids is 1..n, the keys a freshly loaded table would carry.
func segTestRowids(n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(i + 1)
	}
	return out
}
