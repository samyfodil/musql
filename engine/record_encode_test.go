package engine

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

// appendRecordReference is appendRecord as it was written first -- a byte at a
// time through the per-value helpers -- kept as the oracle the in-place encoder
// is checked against.
func appendRecordReference(dst []byte, vals []Value) []byte {
	serialTypes := make([]uint64, len(vals))
	bodyLen, typesLen := 0, 0
	for i, v := range vals {
		st := serialTypeOf(v)
		serialTypes[i] = st
		bodyLen += serialBodyLen(st)
		typesLen += varintLen(st)
	}
	hdrLen := 1 + typesLen
	for {
		n := varintLen(uint64(hdrLen))
		if n == hdrLen-typesLen {
			break
		}
		hdrLen = n + typesLen
	}
	var tmp [9]byte
	n := putVarint(tmp[:], uint64(hdrLen))
	dst = append(dst, tmp[:n]...)
	for _, st := range serialTypes {
		n = putVarint(tmp[:], st)
		dst = append(dst, tmp[:n]...)
	}
	for _, v := range vals {
		dst = appendValueBody(dst, v)
	}
	return dst
}

// TestAppendRecordMatchesReference: random rows -- every integer width and
// both edges of each, the header-only 0 and 1, REALs including -0, NaN and the
// infinities, TEXT and BLOB across the lengths where a serial type needs a
// second varint byte, NULLs, and rows wide enough that the header length does
// too -- encode to the same bytes as the reference, appended after a prefix,
// and decode back to the row.
func TestAppendRecordMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	ints := []int64{0, 1, -1, 2, 127, 128, -128, -129, 32767, 32768, -32768, -32769,
		1<<23 - 1, 1 << 23, -(1 << 23), -(1 << 23) - 1, 1<<31 - 1, 1 << 31, -(1 << 31), -(1 << 31) - 1,
		1<<47 - 1, 1 << 47, -(1 << 47), -(1 << 47) - 1, math.MaxInt64, math.MinInt64}
	floats := []float64{0, math.Copysign(0, -1), 1.5, -2.25, math.MaxFloat64, math.SmallestNonzeroFloat64,
		math.Inf(1), math.Inf(-1), math.NaN()}
	text := func() []byte {
		n := []int{0, 1, 5, 56, 57, 58, 59, 63, 64, 65, 200, 8191, 8192}[rng.Intn(13)]
		b := make([]byte, n)
		rng.Read(b)
		return b
	}
	val := func() Value {
		switch rng.Intn(6) {
		case 0:
			return Value{Typ: Null}
		case 1:
			return Value{Typ: Float, F: floats[rng.Intn(len(floats))]}
		case 2:
			return Value{Typ: Text, S: text()}
		case 3:
			return Value{Typ: Blob, S: text()}
		default:
			if rng.Intn(3) == 0 {
				return Value{Typ: Int, I: int64(rng.Uint64())}
			}
			return Value{Typ: Int, I: ints[rng.Intn(len(ints))]}
		}
	}
	for iter := range 20000 {
		n := rng.Intn(8)
		if rng.Intn(20) == 0 {
			n = 30 + rng.Intn(120) // past smallRecordCols, and a two-byte header length
		}
		row := make([]Value, n)
		for i := range row {
			row[i] = val()
		}
		prefix := []byte{0xAB, 0xCD}[:rng.Intn(3)]
		want := appendRecordReference(append([]byte(nil), prefix...), row)
		got := appendRecord(append([]byte(nil), prefix...), row)
		if !bytes.Equal(got, want) {
			t.Fatalf("iter %d: row %v\ngot  %x\nwant %x", iter, row, got, want)
		}
		if sz := recordSize(row); sz != len(want)-len(prefix) {
			t.Fatalf("iter %d: recordSize %d, encoded %d", iter, sz, len(want)-len(prefix))
		}
		back, err := decodeRecord(got[len(prefix):])
		if err != nil || len(back) != len(row) {
			t.Fatalf("iter %d: decode: %v (%d of %d values)", iter, err, len(back), len(row))
		}
		for i := range row {
			a, b := row[i], back[i]
			same := a.Typ == b.Typ && a.I == b.I && bytes.Equal(a.S, b.S) &&
				(a.F == b.F || (math.IsNaN(a.F) && math.IsNaN(b.F))) && math.Signbit(a.F) == math.Signbit(b.F)
			if !same {
				t.Fatalf("iter %d col %d: %+v decoded as %+v", iter, i, a, b)
			}
		}
	}
}
