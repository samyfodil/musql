// This file implements SQLite's record format: the encoding of a table row's
// payload (its non-key columns) as stored in a table b-tree leaf cell. See
// https://www.sqlite.org/fileformat2.html#record_format
package engine

import (
	"fmt"
	"math"
	"slices"
)

// ValueType identifies the decoded type of a record column.
type ValueType byte

const (
	Null ValueType = iota
	Int
	Float
	Text
	Blob
)

// Value is a single decoded record column. Only the field matching Typ is
// meaningful: I for Int, F for Float, S for Text/Blob (Text and Blob are
// distinguished only by Typ; both carry their bytes in S).
type Value struct {
	Typ ValueType
	// Subtype is SQLite's function SUBTYPE (0 for none; 74 for JSON text, 73 for
	// fts5_insttoken). It propagates by ordinary value copy and is ignored by
	// functions that don't explicitly read it. Declared here, after Typ, to fit
	// in its padding without growing Value's size.
	Subtype uint8
	I       int64
	F       float64
	S       []byte
}

// smallRecordCols bounds the on-stack serial-type array; records larger than
// this grow onto the heap.
const smallRecordCols = 32

// decodeRecord parses a table b-tree leaf record payload into its column
// values, per the SQLite record format: a varint header length, followed by
// one varint serial type per column (the "header"), followed by the column
// values themselves (the "body") back to back with no padding.
func decodeRecord(payload []byte) ([]Value, error) {
	return decodeRecordInto(payload, nil)
}

// decodeRecordInto is decodeRecord with an optional caller-owned output buffer
// for zero-allocation decodes in streaming scans. Reuse is safe only if the
// caller has finished with the previous row before decoding the next. A
// materialized cursor must pass nil.
func decodeRecordInto(payload []byte, buf []Value) ([]Value, error) {
	return decodeRecordMaskedInto(payload, buf, allColumns)
}

// columnMask is the set of columns to decode: bit i means "column i is wanted".
// Bit 63 stands for column 63 and every column above it. This is a performance
// hint, not a correctness input (see decodeRecordMaskedInto).
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

// widenIfTotal collapses a mask that already wants every column back to
// allColumns, so a "SELECT *" scan takes the plain decode path instead of
// the masked one: identical output, but skips the per-column mask test.
func (m columnMask) widenIfTotal(n int) columnMask {
	if n < 0 || n > 63 {
		return m
	}
	if total := columnMask(1)<<uint(n) - 1; m&total == total {
		return allColumns
	}
	return m
}

// decodeRecordMaskedInto is decodeRecordInto restricted to the columns in mask:
// a column outside it is validated and skipped, its slot left as zero Value.
// The mask is a performance hint, not a correctness input. Columns are
// validated identically whether decoded or skipped. Skipped columns are
// re-decoded if needed later rather than trusting the mask.
func decodeRecordMaskedInto(payload []byte, buf []Value, mask columnMask) ([]Value, error) {
	hdrLen, n := getVarint(payload)
	if n == 0 {
		return nil, fmt.Errorf("engine: record: truncated header-length varint")
	}
	if hdrLen > uint64(len(payload)) {
		return nil, fmt.Errorf("engine: record: header length %d exceeds payload length %d", hdrLen, len(payload))
	}

	// One header pass, recording each serial type. Serial types 0..127 are
	// single bytes (NULL, integer, float, and TEXT/BLOB up to 57 bytes), so the
	// general varint decoder is only entered for genuinely multi-byte types.
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
		// Specialized path for unmasked decodes: no per-column mask test.
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
			// Zero-copy sub-slice of the payload. Safe because the pager is
			// immutable and the driver boundary copies before any Value can
			// outlive the pager.
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

// serialSkipLen is decodeSerialInto without the store: returns the identical
// (size, error) pair for a skipped column. Validates exactly like decodeSerialInto.
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

// serialIntSize maps the six fixed-width integer serial types to their byte
// width, indexed directly by serial type. Index 0 is unused padding.
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

// serialTypeOf returns v's serial type without producing body bytes.
// The Int case picks the narrowest width that holds the value.
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

// serialBodyLen returns the body bytes for serial type st. Mirrors decodeSerialInto:
// types 0/8/9 are in the header; 1..6 are fixed integer widths; 7 is 8-byte float;
// >=12 is TEXT/BLOB length.
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

// appendValueBody appends v's body bytes onto dst.
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

// encodeRecord builds a table b-tree leaf record payload from column values:
// varint header length, then one varint serial type per column (the header),
// then column bodies back to back (the body).
// The header-length field is self-referential (its size counts toward the
// length it encodes), found by fixed-point iteration.
func checkRowRecordLength(vals []Value) error {
	total := int64(len(vals)) + 9
	for _, v := range vals {
		switch v.Typ {
		case Text, Blob:
			total += int64(len(v.S))
		default:
			total += 8
		}
		if total > sqliteMaxLength {
			return fmt.Errorf("engine: string or blob too big")
		}
	}
	return nil
}

func encodeRecord(vals []Value) []byte { return appendRecord(nil, vals) }

// recordSize is len(appendRecord(nil, vals)), computed without encoding.
func recordSize(vals []Value) int {
	bodyLen, typesLen := 0, 0
	for _, v := range vals {
		st := serialTypeOf(v)
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
	return hdrLen + bodyLen
}

// appendRecord appends encodeRecord onto the end of dst, which it grows once to
// the exact size. Multiple records into one buffer pay no allocation per record.
func appendRecord(dst []byte, vals []Value) []byte {
	// serialTypes come off the stack for the common handful-of-columns case.
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
