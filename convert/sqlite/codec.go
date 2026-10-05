package sqlite

// THE RECORD CODEC, VARINTS AND VALUE ORDER -- this package's own copies.
//
// The engine has the same primitives for its own format, and this package
// deliberately does not borrow them: what the SQLite format needs is fixed by
// that format, and a change the engine makes to its own record handling must
// not move a byte of what this package writes. Copied from engine/record.go,
// varint.go, utf16.go, value_compare.go and row_arena.go.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"slices"

	"github.com/samyfodil/musql/engine"
)

// Value is the engine's value: what a converter hands it and gets back.
type Value = engine.Value

const (
	Null  = engine.Null
	Int   = engine.Int
	Float = engine.Float
	Text  = engine.Text
	Blob  = engine.Blob
)
// smallRecordCols bounds the on-stack serial-type array decodeRecord uses for
// the common case, sized generously above any table this package's own tests
// or mined SQL corpora create; a record with more columns than this grows onto
// the heap the ordinary append way.
const smallRecordCols = 32

// decodeRecord parses a table b-tree leaf record payload into its column
// values, per the SQLite record format: a varint header length, followed by
// one varint serial type per column (the "header"), followed by the column
// values themselves (the "body") back to back with no padding.
func decodeRecord(payload []byte) ([]Value, error) {
	return decodeRecordInto(payload, nil)
}

// decodeRecordInto is decodeRecord with an optional caller-owned output
// buffer: when buf has room it is reused (buf[:ncols]) instead of allocating,
// so a row-at-a-time reader decodes with no per-row allocation. nil allocates.
// Reuse is safe only when the caller is done with the previous row; a caller
// retaining rows must pass nil.
func decodeRecordInto(payload []byte, buf []Value) ([]Value, error) {
	return decodeRecordMaskedInto(payload, buf, allColumns)
}

// columnMask is the set of columns a decode is asked to produce Values for:
// bit i means "column i is wanted". Bit 63 stands for column 63 AND EVERY
// COLUMN ABOVE IT, which is SQLite's own single-word column-mask convention --
// where.c:7384 builds one with `colUsed |= ((u64)1)<<(ii<63 ? ii : 63)`, and
// OP_ColumnsUsed's header (vdbe.c:4720) states it as "the first 63 bits are
// one for each of the first 63 columns ... The high-order bit is set if any
// column after the 64th is used".
//
// It is a PURE PERFORMANCE HINT and never a correctness input: see
// decodeRecordMaskedInto.
type columnMask uint64

// allColumns is the mask that wants everything -- what every caller outside
// the streaming scan passes, and what decodeRecordInto is defined as.
const allColumns columnMask = ^columnMask(0)

// has reports whether column i is wanted. Column 63 and above all answer from
// bit 63, per the convention above.
func (m columnMask) has(i int) bool {
	if i >= 63 {
		i = 63
	}
	return m&(1<<uint(i)) != 0
}

// decodeRecordMaskedInto is decodeRecordInto restricted to the columns in mask:
// a column outside it is validated and skipped, and its slot left as the zero
// Value -- OP_Column's property of decoding only the named column
// (vdbe.c:3185-3217).
//
// The mask is a performance hint, never a correctness input. Skipped columns
// are validated exactly as decoded ones (serialSkipLen returns the same
// (size, error) as decodeSerialInto; TestSerialSkipLenMatches), so a malformed
// record errors identically whatever the mask, rather than panicking or
// yielding a short row.
func decodeRecordMaskedInto(payload []byte, buf []Value, mask columnMask) ([]Value, error) {
	hdrLen, n := getVarint(payload)
	if n == 0 {
		return nil, fmt.Errorf("engine: record: truncated header-length varint")
	}
	if hdrLen > uint64(len(payload)) {
		return nil, fmt.Errorf("engine: record: header length %d exceeds payload length %d", hdrLen, len(payload))
	}

	// One header pass, recording each serial type as it is validated, like
	// OP_Column's header walk (vdbe.c:3125-3135). stackTypes avoids sizing
	// serialTypes with a second pass for any record narrower than
	// smallRecordCols.
	//
	// The one-byte fast path is vdbe.c:3126's `if( (pC->aType[i] = t =
	// zHdr[0])<0x80 )`: serial types 0..127 (NULL, integers, floats, TEXT
	// and BLOB up to 57 bytes) are one byte. payload[pos] is in range
	// because pos < hdrLen <= len(payload), C's `zHdr<zEndHdr` guard.
	var stackTypes [smallRecordCols]uint64
	serialTypes := stackTypes[:0]
	pos := n
	for pos < int(hdrLen) {
		st, sn := uint64(payload[pos]), 1
		if st >= 0x80 {
			st, sn = getVarint(payload[pos:])
			if sn == 0 {
				return nil, fmt.Errorf("engine: record: truncated serial-type varint at header offset %d", pos)
			}
		}
		serialTypes = append(serialTypes, st)
		pos += sn
	}
	if pos != int(hdrLen) {
		return nil, fmt.Errorf("engine: record: serial-type array overruns header length")
	}

	// Walk the body, one value per serial type, in header order.
	ncols := len(serialTypes)
	body := payload[hdrLen:]
	var vals []Value
	if cap(buf) >= ncols {
		vals = buf[:ncols]
	} else {
		vals = make([]Value, ncols)
	}
	off := 0
	if mask == allColumns {
		// Specialized rather than folded into the masked loop below: a mask
		// test per column per row is not free at this depth. Measured, against
		// the same tree with a single loop and the test inside it: 1.06-1.07x
		// on the scans that never mask (W4, W9), which is the overwhelming
		// majority of them.
		for i, st := range serialTypes {
			size, err := decodeSerialInto(st, body[off:], &vals[i])
			if err != nil {
				return nil, fmt.Errorf("engine: record: column %d: %w", i, err)
			}
			off += size
		}
		return vals, nil
	}
	for i, st := range serialTypes {
		var size int
		var err error
		if mask.has(i) {
			size, err = decodeSerialInto(st, body[off:], &vals[i])
		} else {
			size, err = serialSkipLen(st, body[off:])
			vals[i] = Value{}
		}
		if err != nil {
			return nil, fmt.Errorf("engine: record: column %d: %w", i, err)
		}
		off += size
	}
	return vals, nil
}

// decodeSerialInto decodes a single value of the given serial type from the
// start of b into *out, returning the number of body bytes it occupies.
//
// Writes through a pointer rather than returning a Value because this is the
// innermost statement of every table scan -- once per column per row -- and a
// returned Value is 48 bytes the caller then copies a second time into its
// output slice. On an error return *out is left untouched; every caller
// abandons the whole record on error, so a stale slot is never observable.
func decodeSerialInto(serialType uint64, b []byte, out *Value) (int, error) {
	switch serialType {
	case 0: // NULL
		*out = Value{Typ: Null}
		return 0, nil
	case 1, 2, 3, 4, 5, 6: // signed big-endian ints of 1,2,3,4,6,8 bytes
		size := serialIntSize[serialType]
		if len(b) < size {
			return 0, fmt.Errorf("truncated %d-byte integer", size)
		}
		*out = Value{Typ: Int, I: decodeSignedBE(b[:size])}
		return size, nil
	case 7: // 8-byte IEEE float
		if len(b) < 8 {
			return 0, fmt.Errorf("truncated float")
		}
		bits := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
			uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
		*out = Value{Typ: Float, F: math.Float64frombits(bits)}
		return 8, nil
	case 8: // integer constant 0
		*out = Value{Typ: Int, I: 0}
		return 0, nil
	case 9: // integer constant 1
		*out = Value{Typ: Int, I: 1}
		return 0, nil
	case 10, 11: // reserved for internal use; not expected on disk
		return 0, fmt.Errorf("reserved serial type %d", serialType)
	default:
		if serialType >= 12 && serialType%2 == 0 { // BLOB
			size := int((serialType - 12) / 2)
			if len(b) < size {
				return 0, fmt.Errorf("truncated blob of length %d", size)
			}
			// Zero-copy: S sub-slices the payload (which sub-slices the
			// pager-owned page) rather than copying its bytes out. This kills
			// the dominant per-row allocation of a table scan (~one heap copy
			// per TEXT/BLOB column). SAFE because a pager's page bytes are
			// never mutated in place (nothing here or in the engine writes to
			// a Value's S), and a segment build copies TEXT/BLOB into a
			// fresh string/[]byte before any Value can outlive the pager. The
			// page slice is heap-allocated (readPage) so the GC keeps it alive
			// as long as any Value references it, even after the pager closes.
			*out = Value{Typ: Blob, S: b[:size:size]}
			return size, nil
		}
		if serialType >= 13 { // TEXT (odd, >=13)
			size := int((serialType - 13) / 2)
			if len(b) < size {
				return 0, fmt.Errorf("truncated text of length %d", size)
			}
			*out = Value{Typ: Text, S: b[:size:size]} // zero-copy sub-slice; see BLOB above
			return size, nil
		}
		return 0, fmt.Errorf("invalid serial type %d", serialType)
	}
}

// serialSkipLen is decodeSerialInto without the store: the identical (size,
// error) for the identical input, used for a column a masked decode skips. A
// skipped column must be validated exactly as a decoded one, or a truncated
// record becomes an out-of-range slice on the next column -- a panic.
// TestSerialSkipLenMatches checks every serial type against every truncation.
func serialSkipLen(serialType uint64, b []byte) (int, error) {
	switch serialType {
	case 0: // NULL
		return 0, nil
	case 1, 2, 3, 4, 5, 6: // signed big-endian ints of 1,2,3,4,6,8 bytes
		size := serialIntSize[serialType]
		if len(b) < size {
			return 0, fmt.Errorf("truncated %d-byte integer", size)
		}
		return size, nil
	case 7: // 8-byte IEEE float
		if len(b) < 8 {
			return 0, fmt.Errorf("truncated float")
		}
		return 8, nil
	case 8, 9: // integer constants 0 and 1; no body bytes
		return 0, nil
	case 10, 11: // reserved for internal use; not expected on disk
		return 0, fmt.Errorf("reserved serial type %d", serialType)
	default:
		if serialType >= 12 && serialType%2 == 0 { // BLOB
			size := int((serialType - 12) / 2)
			if len(b) < size {
				return 0, fmt.Errorf("truncated blob of length %d", size)
			}
			return size, nil
		}
		if serialType >= 13 { // TEXT (odd, >=13)
			size := int((serialType - 13) / 2)
			if len(b) < size {
				return 0, fmt.Errorf("truncated text of length %d", size)
			}
			return size, nil
		}
		return 0, fmt.Errorf("invalid serial type %d", serialType)
	}
}

// serialIntSize maps the six fixed-width integer serial types to their
// on-disk byte width, indexed by the serial type itself. An ARRAY, not a map:
// this is read once per integer column per decoded row -- the innermost loop of
// every table scan -- where a map's hash+probe cost dominates the two-byte load
// it is guarding. Index 0 is unused padding so the six real types index
// directly; decodeSerialInto's only lookup sits inside a `case 1,2,3,4,5,6`,
// so the index is always in range.
var serialIntSize = [7]int{0, 1, 2, 3, 4, 6, 8}

// decodeSignedBE sign-extends and decodes a big-endian two's-complement
// integer of 1, 2, 3, 4, 6, or 8 bytes.
func decodeSignedBE(b []byte) int64 {
	var v int64
	if b[0]&0x80 != 0 {
		v = -1 // all-ones sign extension
	}
	for _, c := range b {
		v = (v << 8) | int64(c)
	}
	return v
}

// ---- record encoding (the write side) ----

// intSerialWidths lists, in the order SQLite tries them, the (serialType,
// byteWidth) pairs for the six fixed-width signed-integer encodings, used to
// pick the smallest representation that round-trips a given int64 (matching
// C SQLite's own record-encoding behavior, which prefers small serial
// types for small values).
var intSerialWidths = []struct {
	serialType uint64
	bytes      int
}{
	{1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 6}, {6, 8},
}

// fitsSignedWidth reports whether i fits in the two's-complement range of an
// n-byte signed integer (n one of 1,2,3,4,6,8).
func fitsSignedWidth(i int64, n int) bool {
	if n >= 8 {
		return true
	}
	bits := uint(n * 8)
	min := -(int64(1) << (bits - 1))
	max := (int64(1) << (bits - 1)) - 1
	return i >= min && i <= max
}

// serialTypeOf returns v's serial type WITHOUT producing its body bytes -- the
// half of the old encodeValue that encodeRecord needs first, split out so the
// record's total size can be computed before anything is written.
//
// The Int case picks the narrowest width that holds the value, walking
// intSerialWidths exactly as appendValueBody below does -- the two share that
// one table rather than keeping separate copies, so they cannot drift.
func serialTypeOf(v Value) uint64 {
	switch v.Typ {
	case Null:
		return 0
	case Int:
		switch v.I {
		case 0:
			return 8
		case 1:
			return 9
		}
		for _, w := range intSerialWidths {
			if fitsSignedWidth(v.I, w.bytes) {
				return w.serialType
			}
		}
		return 6 // unreachable: every int64 fits in 8 bytes
	case Float:
		return 7
	case Text:
		return uint64(13 + 2*len(v.S))
	case Blob:
		return uint64(12 + 2*len(v.S))
	default:
		panic(fmt.Sprintf("engine: serialTypeOf: invalid ValueType %d", v.Typ))
	}
}

// serialBodyLen returns how many BODY bytes serial type st occupies, so
// encodeRecord can size its buffer exactly without materializing any body.
// Mirrors decodeSerialInto's own sizing: types 0/8/9 are stored entirely in the
// header, 1..6 are the fixed integer widths, 7 is an 8-byte float, and >=12 is
// a TEXT/BLOB length -- for which (st-12)/2 is correct for BOTH parities,
// because integer division makes the odd TEXT form agree with (st-13)/2.
func serialBodyLen(st uint64) int {
	switch {
	case st == 0 || st == 8 || st == 9:
		return 0
	case st <= 6:
		return serialIntSize[st]
	case st == 7:
		return 8
	case st >= 12:
		return int((st - 12) / 2)
	default:
		return 0 // 10/11 are reserved and never produced by serialTypeOf
	}
}

// appendValueBody appends v's body bytes straight onto dst.
//
// Writing through the caller's buffer is the point: the old encodeValue
// returned a freshly allocated []byte for every Int and every Float, which
// encodeRecord copied into its output and dropped. Over one corpus chunk that
// was 1.57M allocations for the integer bodies alone, plus the [][]byte
// holding them -- pure garbage, since a body is only ever consumed by being
// copied.
// TEXT/BLOB already avoided it by handing back v.S directly, and still do.
func appendValueBody(dst []byte, v Value) []byte {
	switch v.Typ {
	case Null:
		return dst
	case Int:
		switch v.I {
		case 0, 1:
			return dst // serial types 8 and 9 carry the value in the header
		}
		u := uint64(v.I)
		for _, w := range intSerialWidths {
			if fitsSignedWidth(v.I, w.bytes) {
				for k := w.bytes - 1; k >= 0; k-- {
					dst = append(dst, byte(u>>uint(8*k)))
				}
				return dst
			}
		}
		return dst // unreachable, see serialTypeOf
	case Float:
		bits := math.Float64bits(v.F)
		for k := 0; k < 8; k++ {
			dst = append(dst, byte(bits>>uint(8*(7-k))))
		}
		return dst
	default: // Text, Blob
		return append(dst, v.S...)
	}
}

func encodeRecord(vals []Value) []byte { return appendRecord(nil, vals) }

// appendRecord is encodeRecord written onto the end of dst, which it grows once to
// the record's exact size. A caller building many records into one buffer -- a
// delta batch (appendSegmentDeltaAt) -- pays no allocation per record.
func appendRecord(dst []byte, vals []Value) []byte {
	// serialTypes and bodies are pure scratch -- neither is returned, and the
	// body slices are COPIED into buf below rather than retained -- so for the
	// common handful-of-columns record they come off the stack, exactly as
	// decodeRecordInto's own stackTypes does on the read side. These were two of
	// the three allocations every encoded record used to cost, and a commit
	// encodes one record per row plus one per index entry.
	var stackTypes [smallRecordCols]uint64
	var serialTypes []uint64
	if len(vals) <= smallRecordCols {
		serialTypes = stackTypes[:len(vals)]
	} else {
		serialTypes = make([]uint64, len(vals))
	}
	bodyLen := 0
	typesLen := 0
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

	buf := slices.Grow(dst, hdrLen+bodyLen)
	var tmp [9]byte
	n := putVarint(tmp[:], uint64(hdrLen))
	buf = append(buf, tmp[:n]...)
	for _, st := range serialTypes {
		n = putVarint(tmp[:], st)
		buf = append(buf, tmp[:n]...)
	}
	for _, v := range vals {
		buf = appendValueBody(buf, v)
	}
	return buf
}
// getVarint decodes a SQLite varint from the start of b, returning the
// decoded value and the number of bytes consumed (1..9). A varint is a
// big-endian encoding using 7 bits of each of the first 8 bytes (the high bit
// of each byte, if set, signals "more bytes follow"); a 9th byte, if present,
// contributes all 8 of its bits. Every varint is at most 9 bytes long. If b is
// too short to contain a complete varint, n is 0.
func getVarint(b []byte) (val uint64, n int) {
	var v uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0
		}
		c := b[i]
		v = (v << 7) | uint64(c&0x7f)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	// 9th byte: all 8 bits are used.
	if len(b) < 9 {
		return 0, 0
	}
	v = (v << 8) | uint64(b[8])
	return v, 9
}

// varintLen returns the number of bytes putVarint would write for v, without
// writing anything.
func varintLen(v uint64) int {
	switch {
	case v <= 0x7f:
		return 1
	case v < 1<<14:
		return 2
	case v < 1<<21:
		return 3
	case v < 1<<28:
		return 4
	case v < 1<<35:
		return 5
	case v < 1<<42:
		return 6
	case v < 1<<49:
		return 7
	case v < 1<<56:
		return 8
	default:
		return 9
	}
}

// putVarint encodes v into dst (which must have at least 9 bytes of room) as
// a SQLite varint, the exact inverse of getVarint, and returns the number of
// bytes written (1..9).
//
// Encoding: values below 2^56 are packed into the minimum number of 7-bit
// big-endian groups (1..8 bytes), with the continuation bit (0x80) set on
// every byte but the last. Values >= 2^56 always take the full 9 bytes: the
// first 8 bytes each carry 7 bits of v>>8 (56 bits total) with the
// continuation bit forced on all 8 regardless of value (that's how getVarint
// knows to read a 9th byte at all), and the 9th byte carries all 8 low bits
// of v verbatim.
func putVarint(dst []byte, v uint64) int {
	if v <= 0x7f {
		dst[0] = byte(v)
		return 1
	}
	if v < 1<<56 {
		var tmp [8]byte
		n := 0
		for v != 0 {
			tmp[n] = byte(v & 0x7f)
			v >>= 7
			n++
		}
		for i := 0; i < n; i++ {
			b := tmp[n-1-i]
			if i != n-1 {
				b |= 0x80
			}
			dst[i] = b
		}
		return n
	}
	top56 := v >> 8
	for i := 0; i < 8; i++ {
		shift := uint(7 * (7 - i))
		dst[i] = byte((top56>>shift)&0x7f) | 0x80
	}
	dst[8] = byte(v)
	return 9
}
// isUTF16 reports whether enc is one of the two UTF-16 encodings.
func isUTF16(enc TextEncoding) bool { return enc == UTF16LE || enc == UTF16BE }

// encodeTextBytes renders internal UTF-8 text in the database's encoding,
// returning src unchanged for UTF-8.
//
// It ports sqlite3VdbeMemTranslate's UTF-8 -> UTF-16 loop rather than using
// unicode/utf16, because they disagree on malformed input: SQLite reads with
// READ_UTF8 (swallows continuation bytes, accepts overlong forms, keeps a
// truncated sequence's bits) and writes with WRITE_UTF16, while []rune yields
// U+FFFD per bad byte. A lone 0x80 is 8000 in UTF-16le, not FDFF.
func encodeTextBytes(enc TextEncoding, src []byte) []byte {
	if !isUTF16(enc) {
		return src
	}
	out := make([]byte, 0, 2*len(src)+2)
	for i := 0; i < len(src); {
		cp, n := sqliteUTF8Next(src[i:])
		i += n
		u0, u1, pair := utf16UnitsOf(uint32(cp))
		out = appendUTF16Unit(out, enc, u0)
		if pair {
			out = appendUTF16Unit(out, enc, u1)
		}
	}
	return out
}

// decodeTextBytes renders text in the database's encoding as internal UTF-8,
// returning src unchanged for UTF-8.
//
// An odd byte count drops the trailing byte, as C does ("SELECT
// length(CAST(x'61' AS TEXT))" is 0). It ports sqlite3VdbeMemTranslate rather
// than unicode/utf16, which differ on an unpaired surrogate: SQLite combines a
// D800..DFFF unit with the next unit whatever it is, so in UTF-16le
// length(ltrim(x'00d84100')) is 1 and its hex is 00D841DC.
func decodeTextBytes(enc TextEncoding, src []byte) []byte {
	if !isUTF16(enc) {
		return src
	}
	n := len(src) &^ 1 // sqlite3VdbeMemTranslate's own "pMem->n &= ~1"
	get := binary.LittleEndian.Uint16
	if enc == UTF16BE {
		get = binary.BigEndian.Uint16
	}
	// sqlite3VdbeMemTranslate's own bound for this direction: a 2-byte code
	// unit can grow to a 3-byte UTF-8 character, so 2n is the ceiling.
	out := make([]byte, 0, 2*n)
	for i := 0; i < n; i += 2 {
		c := uint32(get(src[i:]))
		if c >= 0xD800 && c < 0xE000 && i+2 < n {
			// The surrogate branch, verbatim: the next unit is consumed
			// unconditionally -- SQLite never checks that it is a LOW surrogate.
			i += 2
			c2 := uint32(get(src[i:]))
			c = (c2 & 0x03FF) + ((c & 0x003F) << 10) + (((c & 0x03C0) + 0x0040) << 10)
		}
		out = appendUTF8(out, c)
	}
	return out
}

// utf16UnitsOf returns the one or two code units C SQLite's WRITE_UTF16LE /
// WRITE_UTF16BE macros emit for c, which for c > 0xFFFF is NOT quite the
// textbook surrogate-pair formula: the macros mask the high bits (c>>18 & 3,
// c>>8 & 3), so a value above U+10FFFF -- which READ_UTF8 can produce from a
// malformed 4-byte sequence -- wraps rather than saturating. Reproduced as
// written so the two engines agree on those too.
func utf16UnitsOf(c uint32) (u0, u1 uint16, pair bool) {
	if c <= 0xFFFF {
		return uint16(c), 0, false
	}
	u := c - 0x10000
	u0 = uint16(0x00D8+((u>>18)&0x03))<<8 | uint16(((c>>10)&0x003F)+((u>>10)&0x00C0))
	u1 = uint16(0x00DC+((c>>8)&0x03))<<8 | uint16(c&0x00FF)
	return u0, u1, true
}

// appendUTF16Unit writes one code unit in the database's byte order.
func appendUTF16Unit(dst []byte, enc TextEncoding, u uint16) []byte {
	if enc == UTF16BE {
		return append(dst, byte(u>>8), byte(u))
	}
	return append(dst, byte(u), byte(u>>8))
}

// appendUTF8 is SQLite's WRITE_UTF8 macro: the plain UTF-8 encoder, with no
// validity check at all (a surrogate value, or anything above U+10FFFF that
// still fits the 4-byte form, is written out as-is -- which is what makes
// decodeTextBytes's surrogate branch round-trip through sqliteUTF8Next).
func appendUTF8(dst []byte, c uint32) []byte {
	switch {
	case c < 0x80:
		return append(dst, byte(c))
	case c < 0x800:
		return append(dst, 0xC0|byte((c>>6)&0x1F), 0x80|byte(c&0x3F))
	case c < 0x10000:
		return append(dst, 0xE0|byte((c>>12)&0x0F), 0x80|byte((c>>6)&0x3F), 0x80|byte(c&0x3F))
	default:
		return append(dst, 0xF0|byte((c>>18)&0x07), 0x80|byte((c>>12)&0x3F), 0x80|byte((c>>6)&0x3F), 0x80|byte(c&0x3F))
	}
}

// utf16CompareUTF8 compares two internal (UTF-8) strings by the order their
// UTF-16 encodings would memcmp in -- see this file's doc comment for why that
// is not code point order. It reads the UTF-8 directly, so it allocates
// nothing.
func utf16CompareUTF8(enc TextEncoding, a, b []byte) int {
	bigEndian := enc == UTF16BE
	for len(a) > 0 && len(b) > 0 {
		ua, na := nextUTF16Unit(a)
		ub, nb := nextUTF16Unit(b)
		if ua != ub {
			if utf16UnitLess(bigEndian, ua, ub) {
				return -1
			}
			return 1
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case len(a) > 0:
		return 1
	case len(b) > 0:
		return -1
	}
	return 0
}

// nextUTF16Unit returns the first UTF-16 code unit s encodes to and how many
// UTF-8 bytes it consumed. A non-BMP code point yields its high surrogate and
// consumes the whole code point: the low surrogate is only reached after the
// high ones agreed. It shares encodeTextBytes' decoder and unit arithmetic,
// so comparison order and encoded bytes cannot disagree over malformed text --
// a disagreement would be an index C SQLite calls corrupt.
func nextUTF16Unit(s []byte) (unit uint16, advance int) {
	cp, n := sqliteUTF8Next(s)
	u0, _, _ := utf16UnitsOf(uint32(cp))
	return u0, n
}

// utf16UnitLess orders two differing code units the way memcmp over their
// encoded bytes does: by the high byte first for big-endian, by the LOW byte
// first for little-endian (which is exactly why UTF-16le reorders everything
// from U+0100 up).
func utf16UnitLess(bigEndian bool, x, y uint16) bool {
	if bigEndian {
		return x < y
	}
	if lx, ly := x&0xFF, y&0xFF; lx != ly {
		return lx < ly
	}
	return x>>8 < y>>8
}

// encodeRecordEnc is encodeRecord for a database whose text encoding may not be
// UTF-8: TEXT bodies are written in that encoding, which is what makes the FILE
// interchangeable with C SQLite's.
func encodeRecordEnc(vals []Value, enc TextEncoding) []byte {
	if !isUTF16(enc) {
		return encodeRecord(vals)
	}
	out := make([]Value, len(vals))
	copy(out, vals)
	for i := range out {
		if out[i].Typ == Text {
			out[i].S = encodeTextBytes(enc, out[i].S)
		}
	}
	return encodeRecord(out)
}

// decodeRecordEnc is decodeRecord's counterpart: TEXT read out of the file is
// converted back to the internal UTF-8 every other part of the engine works in.
func decodeRecordEnc(payload []byte, enc TextEncoding) ([]Value, error) {
	vals, err := decodeRecord(payload)
	decodeTextValues(enc, vals)
	return vals, err
}

// decodeRecordMaskedIntoEnc is decodeRecordIntoEnc restricted to mask's columns
// (see decodeRecordMaskedInto). A skipped column decodes to the zero Value,
// which is not TEXT, so the encoding conversion below simply never sees it.
func decodeRecordMaskedIntoEnc(payload []byte, buf []Value, enc TextEncoding, mask columnMask) ([]Value, error) {
	vals, err := decodeRecordMaskedInto(payload, buf, mask)
	decodeTextValues(enc, vals)
	return vals, err
}

// decodeTextValues converts every TEXT value in vals from the file's encoding
// to internal UTF-8, in place. A no-op for UTF-8.
//
// This BREAKS the zero-copy sub-slice decodeSerialInto hands back for a UTF-8
// database (record.go), which is correct and unavoidable: the internal form of
// UTF-16 text is a different byte string, so it has to be its own allocation.
// Only a UTF-16 database pays for it.
func decodeTextValues(enc TextEncoding, vals []Value) {
	if !isUTF16(enc) {
		return
	}
	for i := range vals {
		if vals[i].Typ == Text {
			vals[i].S = decodeTextBytes(enc, vals[i].S)
		}
	}
}
// classOrder implements SQLite's storage-class sort order: NULL < INTEGER/
// REAL (as one numeric class) < TEXT < BLOB.
func classOrder(v Value) int {
	switch v.Typ {
	case Null:
		return 0
	case Int, Float:
		return 1
	case Text:
		return 2
	case Blob:
		return 3
	}
	return 0
}

// compareValues compares two values per SQLite's ordering rules. Callers
// handle NULL specially where NULL propagation (rather than ordering) is
// required (e.g. inside comparison operators); this function is also used
// directly for ORDER BY, where NULL does participate in the ordering.
func compareValues(a, b Value) int {
	ca, cb := classOrder(a), classOrder(b)
	if ca != cb {
		if ca < cb {
			return -1
		}
		return 1
	}
	switch ca {
	case 0:
		return 0
	case 1:
		return compareNumeric(a, b)
	case 2, 3:
		return bytes.Compare(a.S, b.S) // BINARY collation: memcmp
	}
	return 0
}

// compareValuesCollatedEnc compares two values under a collation, in a
// database whose TEXT encoding may not be UTF-8. BINARY (and RTRIM) comparison is memcmp over the
// ENCODED bytes, so the encoding changes the order.
// Every caller that can reach a UTF-16 database passes its encoding; the
// UTF-8 shim above is kept for the callers that provably cannot (a fixed
// collation over ASCII-only internal keys).
func compareValuesCollatedEnc(a, b Value, collation string, enc TextEncoding) int {
	ca, cb := classOrder(a), classOrder(b)
	if ca != cb {
		if ca < cb {
			return -1
		}
		return 1
	}
	switch ca {
	case 0:
		return 0
	case 1:
		return compareNumeric(a, b)
	case 2:
		return collatedTextCompareEnc(collation, a.S, b.S, enc)
	case 3:
		return bytes.Compare(a.S, b.S)
	}
	return 0
}

// collatedTextCompareEnc compares two TEXT values under a collation, in a
// database whose text encoding may not be UTF-8. NOCASE is unaffected -- it folds ASCII case and
// C SQLite gives the identical answers in all three encodings (verified:
// "'ABC' = 'abc' COLLATE NOCASE" is 1 and "'É' = 'é' COLLATE NOCASE" is 0
// everywhere) -- while BINARY and RTRIM are memcmp over the ENCODED bytes and
// therefore order differently. utf16CompareUTF8 computes that order straight
// from the internal UTF-8 bytes; see utf16.go.
func collatedTextCompareEnc(collation string, a, b []byte, enc TextEncoding) int {
	// A collation name is an identifier: sqlite3FindCollSeq looks it up in a
	// hash.c table folding with sqlite3UpperToLower and comparing with
	// sqlite3StrICmp -- ASCII-only. This runs per comparison (a comparison
	// carries its collation by name, not as C's resolved CollSeq* in P4,
	// vdbe.c:2383), so it matches in place with asciiEqualFold rather than
	// building a folded copy. An unknown name falls through to BINARY.
	switch {
	case asciiEqualFold(collation, "NOCASE"):
		return nocaseCompare(a, b)
	case asciiEqualFold(collation, "RTRIM"):
		a, b = rtrimTrailingSpaces(a), rtrimTrailingSpaces(b)
	}
	if isUTF16(enc) {
		return utf16CompareUTF8(enc, a, b)
	}
	return bytes.Compare(a, b)
}

// nocaseCompare implements NOCASE: memcmp with ASCII case folded to lowercase,
// as C's sqlite3UpperToLower does. The direction is observable: ']' (0x5D)
// sits between 'B' (0x42) and 'b' (0x62), so "a]b" sorts before "ABC"/"abc"
// only when folding down. A byte-wise fold is safe for UTF-8, since every
// non-ASCII byte is >= 0x80.
func nocaseCompare(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := asciiLowerByte(a[i]), asciiLowerByte(b[i])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func asciiLowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// rtrimTrailingSpaces returns b with only trailing ASCII 0x20 space bytes
// removed (not a general whitespace trim -- verified directly that a
// trailing TAB is left alone by the RTRIM collation).
func rtrimTrailingSpaces(b []byte) []byte {
	i := len(b)
	for i > 0 && b[i-1] == ' ' {
		i--
	}
	return b[:i]
}

func compareNumeric(a, b Value) int {
	if a.Typ == Int && b.Typ == Int {
		switch {
		case a.I < b.I:
			return -1
		case a.I > b.I:
			return 1
		default:
			return 0
		}
	}
	if a.Typ == Int && b.Typ == Float {
		return compareIntFloat(a.I, b.F)
	}
	if a.Typ == Float && b.Typ == Int {
		return -compareIntFloat(b.I, a.F)
	}
	af, bf := valueAsFloat(a), valueAsFloat(b)
	switch {
	case af < bf:
		return -1
	case af > bf:
		return 1
	default:
		return 0
	}
}

func valueAsFloat(v Value) float64 {
	if v.Typ == Int {
		return float64(v.I)
	}
	return v.F
}

// compareIntFloat compares INTEGER i with REAL r exactly, as
// sqlite3IntFloatCompare (vdbe.c) does, without converting i to float64 --
// which rounds for |i| >= 2^53 and can make distinct integers compare equal
// to one REAL (affinity2.test: a REAL column holding 3175546974276630385 must
// compare greater than that literal as an INTEGER).
//
// Any float64 of magnitude >= 2^53 has no fractional part, so int64(r) is exact
// whenever r is in int64's range; smaller magnitudes convert exactly both ways.
func compareIntFloat(i int64, r float64) int {
	switch {
	case math.IsNaN(r):
		// SQLite has no NULL-free NaN value to compare against (a REAL
		// column can't actually store one via ordinary SQL), so this is
		// unreachable in practice; treat it as "greater than everything"
		// defensively rather than panicking or picking an arbitrary side.
		return -1
	case r < -9223372036854775808.0: // less than any possible int64
		return 1
	case r >= 9223372036854775808.0: // greater than any possible int64 (2^63)
		return -1
	}
	ri := int64(r) // exact: r is now known to be within int64's range
	switch {
	case i < ri:
		return -1
	case i > ri:
		return 1
	}
	// i == ri (equal integer parts): whatever's left of r beyond that
	// integer part (r's fractional remainder, exactly recoverable -- see
	// doc comment above) decides the tie.
	switch frac := r - float64(ri); {
	case frac > 0:
		return -1
	case frac < 0:
		return 1
	default:
		return 0
	}
}
// bump hands out byte blocks. A nil *bump allocates per block, which is what
// every caller outside a scan wants.
type bump struct{ buf []byte }

func (a *bump) take(n int) []byte {
	if a == nil {
		return make([]byte, 0, n)
	}
	if n > cap(a.buf)-len(a.buf) {
		a.buf = make([]byte, 0, max(n, bumpChunk))
	}
	off := len(a.buf)
	a.buf = a.buf[:off+n]
	return a.buf[off:off:off+n]
}

// bumpChunk is bump's chunk size. A block bigger than a chunk gets a chunk of
// its own, so an overflowing row costs one allocation, not a loop.
const bumpChunk = 32 << 10

// sqliteUTF8Next is C SQLite's READ_UTF8 macro: the codepoint of the
// character at the front of s, plus the byte count sqliteUTF8Skip would report.
// A sequence that decodes to something UTF-8 may not represent -- an overlong
// form, a surrogate half, or U+FFFE/U+FFFF -- reads as U+FFFD, but a merely
// TRUNCATED sequence keeps whatever bits it did carry, which is why
// unicode(cast(x'e282' as text)) is 130 and unicode(cast(x'f09f92' as text))
// is 2002 rather than either 65533 or an error (both verified against 3.53.3,
// as is unicode(cast(x'80' as text)) == 128 for a bare continuation byte).
func sqliteUTF8Next(s []byte) (cp int64, n int) {
	if len(s) == 0 {
		return 0, 0
	}
	c := int64(s[0])
	n = 1
	if s[0] >= 0xc0 {
		c = int64(sqlite3Utf8Trans1[s[0]-0xc0])
		for n < len(s) && s[n]&0xc0 == 0x80 {
			c = (c << 6) + int64(s[n]&0x3f)
			n++
		}
		if c < 0x80 || c&0xFFFFF800 == 0xD800 || c&0xFFFFFFFE == 0xFFFE {
			c = 0xFFFD
		}
	}
	return c, n
}

// asciiEqualFold reports whether s and want are equal ignoring ASCII case,
// without allocating -- sqlite3StrICmp (util.c). ASCII-only on purpose:
// strings.EqualFold applies Unicode folding (the Kelvin sign U+212A equals
// "k"), which would accept a collation name C rejects. want is always an ASCII
// literal.
func asciiEqualFold(s, want string) bool {
	if len(s) != len(want) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// sqlite3Utf8Trans1 is C SQLite's own table of the same name: the initial
// codepoint bits a lead byte 0xC0..0xFF contributes. It is reproduced verbatim
// rather than computed, because the last two entries (0xFE and 0xFF, which are
// not lead bytes in any UTF-8 revision) are 0 by fiat, not by masking.
var sqlite3Utf8Trans1 = [64]byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x00, 0x01, 0x02, 0x03, 0x00, 0x01, 0x00, 0x00,
}
