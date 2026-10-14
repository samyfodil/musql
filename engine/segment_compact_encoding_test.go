package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"testing"
)

// The segment encodings version 6 added -- consecutive rowids kept as one,
// no width block for full rows, TEXT and BLOB columns as heap bounds, and the
// packed TEXT index -- each read back value for value, at their edges: NULLs,
// empty values, values that did not fit (exceptions), BLOB alignment after an
// odd-length value, several TEXT columns sharing the heap, short rows, and
// rowids that are consecutive only across the signed/unsigned wrap.
func TestSegmentCompactEncodingsRoundTrip(t *testing.T) {
	cols := []columnInfo{{Name: "id"}, {Name: "t"}, {Name: "b"}, {Name: "u"}, {Name: "i"}}
	rng := rand.New(rand.NewPCG(1, 2))
	value := func(c, i int) Value {
		switch rng.IntN(9) {
		case 0:
			return Value{Typ: Null}
		case 1:
			return Value{Typ: Int, I: int64(i)} // an exception in a TEXT/BLOB column
		}
		switch c {
		case 0:
			return Value{Typ: Null}
		case 1, 3:
			return Value{Typ: Text, S: []byte(fmt.Sprintf("%s%d", "xyz"[:rng.IntN(4)], rng.IntN(5)))}
		case 2:
			n := rng.IntN(7) // odd lengths, so the next BLOB needs aligning
			return Value{Typ: Blob, S: bytes.Repeat([]byte{byte(i)}, n)}
		}
		return Value{Typ: Int, I: int64(rng.IntN(100))}
	}
	for _, tc := range []struct {
		name  string
		first uint64
		gap   bool
		short bool
	}{
		{"dense", 1, false, false},
		{"gaps", 1, true, false},
		{"short rows", 100, false, true},
		{"dense across zero", ^uint64(0) - 5, false, false}, // -6 ..
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 300
			rowids := make([]uint64, n)
			rows := make([][]Value, n)
			next := tc.first
			for i := range rows {
				rowids[i] = next
				next++
				if tc.gap && i%7 == 0 {
					next += 3
				}
				w := len(cols)
				if tc.short && i%5 == 0 {
					w = 3
				}
				rows[i] = make([]Value, w)
				for c := range rows[i] {
					rows[i][c] = value(c, i)
				}
			}
			raw, err := buildSegment(cols, rowids, rows)
			if err != nil {
				t.Fatal(err)
			}
			s, err := openSegment(raw)
			if err != nil {
				t.Fatal(err)
			}
			if s.dense != !tc.gap || s.fullWidth != !tc.short {
				t.Errorf("dense=%v fullWidth=%v", s.dense, s.fullWidth)
			}
			for i := range rows {
				if got := s.Rowid(i); got != rowids[i] {
					t.Fatalf("row %d: rowid %d, want %d", i, got, rowids[i])
				}
				if got := s.Width(i); got != len(rows[i]) {
					t.Fatalf("row %d: width %d, want %d", i, got, len(rows[i]))
				}
				for c := range cols {
					want := Value{Typ: Null}
					if c < len(rows[i]) {
						want = rows[i][c]
					}
					got := s.Value(c, i)
					if got.Typ != want.Typ || got.I != want.I || !bytes.Equal(got.S, want.S) {
						t.Fatalf("row %d col %d: %v, want %v", i, c, got, want)
					}
				}
				if j, ok := s.findRowid(int64(rowids[i])); !ok || j != i {
					t.Fatalf("findRowid(%d) = %d, %v", int64(rowids[i]), j, ok)
				}
			}
		})
	}
}

// BytesColumn hands out 8-byte cells for a column stored as bounds; they must
// name exactly the bytes Value reads.
func TestSegmentBoundsCellsMatchValues(t *testing.T) {
	cols := []columnInfo{{Name: "t"}, {Name: "b"}}
	const n = 200
	rowids := make([]uint64, n)
	rows := make([][]Value, n)
	for i := range rows {
		rowids[i] = uint64(i + 1)
		rows[i] = []Value{
			{Typ: Text, S: []byte(fmt.Sprint("v", i*i%37))},
			{Typ: Blob, S: bytes.Repeat([]byte{7}, i%5)},
		}
	}
	raw, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openSegment(raw)
	if err != nil {
		t.Fatal(err)
	}
	for c := range cols {
		if !s.cols[c].bounds {
			t.Fatalf("column %d is not stored as bounds", c)
		}
		cells, heap, ok := s.BytesColumn(c)
		if !ok {
			t.Fatalf("column %d: BytesColumn declined", c)
		}
		for i, x := range cells {
			o, l := uint32(x), uint32(x>>32)
			if got := heap[o : o+l]; !bytes.Equal(got, rows[i][c].S) {
				t.Fatalf("column %d row %d: cell names %q, want %q", c, i, got, rows[i][c].S)
			}
			if c == 1 && l > 0 && o%4 != 0 {
				t.Fatalf("row %d: BLOB at %d is not 4-aligned", i, o)
			}
		}
	}
}

// The packed TEXT index answers what a scan answers, for values present once,
// many times, and not at all.
func TestSegmentPackedTextIndexMatchesScan(t *testing.T) {
	cols := []columnInfo{{Name: "t"}}
	const n = 1000
	rowids := make([]uint64, n)
	rows := make([][]Value, n)
	for i := range rows {
		rowids[i] = uint64(i + 1)
		v := fmt.Sprint("k", i)
		if i%3 == 0 {
			v = fmt.Sprint("dup", i%10)
		}
		rows[i] = []Value{{Typ: Text, S: []byte(v)}}
	}
	raw, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openSegment(raw)
	if err != nil {
		t.Fatal(err)
	}
	x, ok := s.diskIndexFor(0)
	if !ok || x.codec != segIdxCodecBytesPerm {
		t.Fatalf("no packed index: %v %+v", ok, x)
	}
	for _, probe := range []string{"k1", "k998", "dup0", "dup3", "dup9", "", "k", "zzz", "k1000"} {
		var want []int32
		for i, r := range rows {
			if string(r[0].S) == probe {
				want = append(want, int32(i))
			}
		}
		got, ok := s.segEqualityRowsBytes(0, []byte(probe))
		if !ok {
			t.Fatalf("%q: declined", probe)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q: %v, want %v", probe, got, want)
		}
	}
}

// A damaged packed index declines -- never panics, never answers.
func TestSegmentPackedTextIndexRefusesCorruption(t *testing.T) {
	cols := []columnInfo{{Name: "t"}}
	const n = 64
	rowids := make([]uint64, n)
	rows := make([][]Value, n)
	for i := range rows {
		rowids[i] = uint64(i + 1)
		rows[i] = []Value{{Typ: Text, S: []byte(fmt.Sprint("v", i%9))}}
	}
	good, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := openSegment(good)
	if err != nil {
		t.Fatal(err)
	}
	off := int(sg.cols[0].idxOff)
	rowBytes := func(s *segment) func(int) ([]byte, bool) {
		return func(i int) ([]byte, bool) { return s.cellBytes(0, i) }
	}
	if _, ok := openSegColumnIndex(good, uint32(off), n, sg.heap, rowBytes(sg)); !ok {
		t.Fatal("the undamaged record is refused")
	}
	posts := off + segIdxHdr
	for _, tc := range []struct {
		name   string
		break_ func(b []byte)
	}{
		{"a position out of range", func(b []byte) { putUint24(b[posts:], n) }},
		{"a position named twice", func(b []byte) { copy(b[posts+3:posts+6], b[posts:posts+3]) }},
		{"two positions swapped", func(b []byte) {
			var tmp [3]byte
			copy(tmp[:], b[posts:])
			copy(b[posts:posts+3], b[posts+3*(n-1):])
			copy(b[posts+3*(n-1):], tmp[:])
		}},
		{"a wrong distinct count", func(b []byte) { binary.LittleEndian.PutUint32(b[off+4:], 3) }},
		{"a short byte length", func(b []byte) { binary.LittleEndian.PutUint32(b[off+12:], segIdxHdr+3) }},
	} {
		bad := append([]byte(nil), good...)
		tc.break_(bad)
		s, err := openSegment(bad)
		if err != nil {
			continue // refused even earlier
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panicked: %v", tc.name, r)
				}
			}()
			if _, ok := openSegColumnIndex(bad, uint32(off), n, s.heap, rowBytes(s)); ok {
				t.Errorf("%s: accepted", tc.name)
			}
		}()
	}
}

// Damaged bounds read as a decline, never a slice out of range.
func TestSegmentBoundsRefuseCorruption(t *testing.T) {
	cols := []columnInfo{{Name: "t"}}
	rows := [][]Value{{{Typ: Text, S: []byte("abc")}}, {{Typ: Text, S: []byte("de")}}}
	good, err := buildSegment(cols, []uint64{1, 2}, rows)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := openSegment(good)
	if err != nil {
		t.Fatal(err)
	}
	blk := int(sg.cols[0].blockOff)
	for _, bound := range []int{0, 1, 2} {
		for _, v := range []uint32{1 << 30, 4} {
			bad := append([]byte(nil), good...)
			binary.LittleEndian.PutUint32(bad[blk+4*bound:], v)
			s, err := openSegment(bad)
			if err != nil {
				continue
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("bound %d = %d: panicked: %v", bound, v, r)
					}
				}()
				for i := 0; i < 2; i++ {
					s.Value(0, i)
				}
				s.BytesColumn(0)
			}()
		}
	}
}
