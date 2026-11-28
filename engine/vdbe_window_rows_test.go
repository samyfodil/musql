package engine

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// TestWindowRowsRoundTrip checks that every Value the window batch buffers
// reads back exactly: each field, nil versus empty bytes, values that need
// the escape list, bytes larger than an arena chunk, and rows across many
// cell and row chunks.
func TestWindowRowsRoundTrip(t *testing.T) {
	big := []byte(strings.Repeat("y", winChunkMax+17))
	vals := []Value{
		{},
		{Typ: Int, I: -1},
		{Typ: Int, I: math.MinInt64},
		{Typ: Float, F: math.Copysign(0, -1)},
		{Typ: Float, F: math.NaN()},
		{Typ: Float, F: 3, intReal: true},
		{Typ: Float, F: 2.5, intReal: true},
		{Typ: Text, S: []byte("abc"), Subtype: 74},
		{Typ: Text, S: []byte{}},
		{Typ: Blob, S: []byte{}},
		{Typ: Blob, S: []byte{0, 1, 2}},
		{Typ: Text, S: []byte("i"), I: 7},
		{Typ: Text, S: []byte("f"), F: 1.25},
		{Typ: Text, S: []byte("both"), I: 7, F: 1.25},
		{Typ: Float, F: 1e300, I: 9},
	}
	want := func(v Value) Value { return recordValue(v) }
	var r windowRows
	r.appendRow([]Value{{Typ: Blob, S: big}, {Typ: Text, S: []byte("after")}})
	if g := r.at(0, 0); !bytes.Equal(g.S, big) || g.Typ != Blob {
		t.Fatalf("big blob read back as %d bytes of type %v", len(g.S), g.Typ)
	}
	if g := r.at(0, 1); string(g.S) != "after" {
		t.Fatalf("value after the big blob: %q", g.S)
	}
	r = windowRows{}
	const n = 3 * (1 << winRowChunkLg)
	for i := range n {
		row := make([]Value, i%len(vals)+1)
		for j := range row {
			row[j] = vals[(i+j)%len(vals)]
		}
		r.appendRow(row)
	}
	if r.len() != n {
		t.Fatalf("len %d, want %d", r.len(), n)
	}
	var buf []Value
	for i := range n {
		w := i%len(vals) + 1
		if r.width(i) != w {
			t.Fatalf("row %d: width %d, want %d", i, r.width(i), w)
		}
		got := r.rowInto(i, 0, &buf)
		for j := range w {
			exp := want(vals[(i+j)%len(vals)])
			g := got[j]
			if g.Typ != exp.Typ || g.Subtype != exp.Subtype || g.intReal != exp.intReal || g.I != exp.I ||
				math.Float64bits(g.F) != math.Float64bits(exp.F) || (g.S == nil) != (exp.S == nil) || !bytes.Equal(g.S, exp.S) {
				t.Fatalf("row %d col %d: got %+v, want %+v", i, j, g, exp)
			}
			if cap(g.S) != len(g.S) {
				t.Fatalf("row %d col %d: bytes have spare capacity %d > %d", i, j, cap(g.S), len(g.S))
			}
			if a := r.at(i, j); !valuesIdentical(a, g) {
				t.Fatalf("row %d col %d: at %+v, rowInto %+v", i, j, a, g)
			}
		}
		if tail := r.rowInto(i, w-1, &buf); len(tail) != 1 {
			t.Fatalf("row %d: tail of %d values", i, len(tail))
		}
	}
	// A value handed out keeps its bytes however much more is appended.
	held := r.at(7, 0)
	before := append([]byte(nil), held.S...)
	for range 1000 {
		r.appendRow([]Value{{Typ: Text, S: []byte("later")}})
	}
	if !bytes.Equal(held.S, before) {
		t.Fatalf("held bytes changed: %q, was %q", held.S, before)
	}
}
