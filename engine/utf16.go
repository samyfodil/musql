// This file implements UTF-16 text, what "PRAGMA encoding = 'UTF-16le'" (or
// 'UTF-16be') turns on.
//
// # The model
//
// The encoding is a property of the file (its catalog) and applies to every
// TEXT value the connection produces, not just stored ones: in a UTF-16
// database "SELECT hex(sqlite_version())" is 33002E00350033002E003300.
//
// Text is kept internally as UTF-8, so length, substr, upper, LIKE, GLOB,
// printf and NOCASE need no change (they behave identically across encodings).
// The encoding shows through at the byte-exposing functions -- hex(),
// CAST(text AS BLOB), CAST(blob AS TEXT), and blob arguments to text functions
// (textCoercingFuncs) -- and at BINARY/RTRIM comparison, which is memcmp over
// the encoded bytes and so orders differently. Export to C's file format
// encodes stored text the same way. The encoding is read through encoding()
// on the pager, DB, evalCtx or vdbe; a UTF-8 database pays nothing.
//
// # Comparison
//
// Over 'héllo', 'z', 'é' and '<U+1F600>ab', "SELECT a FROM t ORDER BY a" gives
//
//	UTF-8     héllo | z | é | <U+1F600>ab
//	UTF-16le  <U+1F600>ab | héllo | z | é
//	UTF-16be  héllo | z | é | <U+1F600>ab
//
// One rule explains all three: compare the encoded bytes. UTF-8 byte order is
// code point order; UTF-16BE differs only for surrogate pairs (0xD800-0xDBFF
// sorts below U+E000..U+FFFF); UTF-16LE puts the low byte first, reordering
// everything from U+0100 up. utf16CompareUTF8 compares the UTF-16 code units
// directly from the UTF-8, without transcoding.
package engine

import (
	"encoding/binary"
	"fmt"
	"os"
)

// TextEncoding is a database's text encoding, numbered as "PRAGMA encoding"'s
// three values are everywhere they are stored.
type TextEncoding uint32

const (
	UTF8    TextEncoding = 1
	UTF16LE TextEncoding = 2
	UTF16BE TextEncoding = 3
)

// isPowerOfTwo reports whether n is a power of two (a valid page size is one).
func isPowerOfTwo(n uint32) bool { return n != 0 && n&(n-1) == 0 }

// isUTF16 reports whether enc is one of the two UTF-16 encodings.
func isUTF16(enc TextEncoding) bool { return enc == UTF16LE || enc == UTF16BE }

// encodeTextBytes renders internal UTF-8 text in the database's encoding,
// returning src unchanged for UTF-8.
//
// It ports sqlite3VdbeMemTranslate's UTF-8 -> UTF-16 loop rather than using
// unicode/utf16, because they disagree on malformed input: SQLite reads with
// READ_UTF8 (sqliteUTF8Next -- swallows continuation bytes, accepts overlong
// forms, keeps a truncated sequence's bits) and writes with WRITE_UTF16, while
// []rune yields U+FFFD per bad byte. A lone 0x80 is 8000 in UTF-16le, not FDFF.
// Well-formed text, surrogate pairs included, is identical either way.
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
// An odd byte count drops the trailing byte, as C does: "SELECT
// length(CAST(x'61' AS TEXT))" is 0, and "SELECT hex(ltrim(x'6efcda'))" is
// 6EFC in UTF-16le (utf16align.test).
//
// Like encodeTextBytes it ports sqlite3VdbeMemTranslate rather than
// unicode/utf16, which differ on an unpaired surrogate: SQLite does not
// validate, so a unit in D800..DFFF combines with the next unit whatever it
// is. In UTF-16le, length(ltrim(x'00d84100')) is 1 and its hex is 00D841DC.
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
// high ones agreed, when the code points are identical. It uses the same
// decoder and unit arithmetic as encodeTextBytes (sqliteUTF8Next +
// utf16UnitsOf), so comparison order and encoded bytes cannot disagree over
// malformed text.
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

// ---- where the encoding comes from, at each of the three seams ----

// encoding is this write session's text encoding, defaulting to UTF-8 for a
// zero-valued DB (TextEncoding's zero is not UTF8, so this cannot be a bare
// field read).
func (db *DB) encoding() TextEncoding {
	if isUTF16(db.textEncoding) {
		return db.textEncoding
	}
	return UTF8
}

// encoding is the read side's counterpart: the encoding the database declares,
// which is what every value decoded from it is in.
func (p *ReadOnlyPager) encoding() TextEncoding {
	if p == nil || !isUTF16(p.meta.encoding) {
		return UTF8
	}
	return p.meta.encoding
}

// encoding is the expression evaluator's counterpart. An evalCtx with no pager
// (the write path evaluates CHECK constraints and trigger WHEN clauses against
// one) falls back to UTF-8, which is correct for every database this engine
// writes unless PRAGMA encoding switched it -- and a switched session always
// has a pager by the time a value is compared, because the write path's own
// comparisons go through DB/vdbe below.
func (ctx *evalCtx) encoding() TextEncoding {
	if ctx == nil {
		return UTF8
	}
	if ctx.pager != nil {
		return ctx.pager.encoding()
	}
	if ctx.outer != nil {
		return ctx.outer.encoding()
	}
	return UTF8
}

// encoding is the VM's counterpart: a write program carries the write session,
// a read program its pager.
func (m *vdbe) encoding() TextEncoding {
	if m == nil {
		return UTF8
	}
	if m.wctx != nil && m.wctx.db != nil {
		return m.wctx.db.encoding()
	}
	return m.pager.encoding()
}

// ---- seam 1: the record codec ----
//
// Transcoding is done AROUND the codec rather than inside it, which keeps the
// hot path (record.go's serial-type arithmetic and its stack-allocated
// scratch) byte-for-byte unchanged and costs a UTF-8 database nothing: both
// wrappers return immediately.

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

// decodeRecordIntoEnc is decodeRecordInto's counterpart (the buffer-reusing
// form the table-scan hot path uses).
func decodeRecordIntoEnc(payload []byte, buf []Value, enc TextEncoding) ([]Value, error) {
	return decodeRecordMaskedIntoEnc(payload, buf, enc, allColumns)
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

// ---- seam 2: the byte-exposing scalar functions ----

// encodedTextValue is v with a TEXT value's bytes replaced by their form in the
// database's encoding -- what hex() and octet_length() must see, since those are
// the only two functions that expose a text value's BYTES rather than its
// characters. A BLOB (or anything else) is returned unchanged: a blob's bytes
// are its own, whatever the database's text encoding is (verified: with
// encoding=UTF-16le, "hex(b)" over x'414243' is still 414243).
func encodedTextValue(enc TextEncoding, v Value) Value {
	if !isUTF16(enc) || v.Typ != Text {
		return v
	}
	v.S = encodeTextBytes(enc, v.S)
	return v
}

// textCoercingFuncs is the scalar functions whose arguments C treats as TEXT,
// so a BLOB argument is converted from the database's encoding first -- the
// identity in UTF-8, a real decode (decodeTextBytes) in UTF-16
// (utf16align.test's "hex(ltrim(x'6efcda'))", windowC.test's group_concat).
// Over x'2000410042002000', ltrim() strips a leading space in UTF-16le and
// nothing in UTF-16be, where the bytes are U+2000 U+4100 U+4200 U+2000.
//
// Absent because their blob argument stays a blob: length/octet_length,
// hex/quote/typeof, substr, unhex/zeroblob/randomblob and instr.
// concat/concat_ws are separate; see utf16ByteCountedConcat.
var textCoercingFuncs = map[string]bool{
	"ltrim": true, "rtrim": true, "trim": true, "upper": true, "lower": true,
	"replace": true, "printf": true, "format": true,
	"unicode": true, "like": true, "glob": true, "group_concat": true, "string_agg": true,
}

// utf16ByteCountedConcat is the two functions that coerce a blob like the set
// above but get their own answer wrong, so UTF-16 databases decline them.
// concatFuncCore sizes its buffer with sqlite3_value_bytes() before
// sqlite3_value_text(), so for a blob it copies the blob's byte count out of
// the converted text:
//
//	SELECT hex(concat(x'610062006300','!'))
//	  -> 6100620063000000000000002100     "abc", then three NUL code units
//
// In UTF-16le that over-reads past the conversion buffer; in UTF-16be it
// truncates. Reproducing a rule that reads uninitialized memory is guessing.
var utf16ByteCountedConcat = map[string]bool{"concat": true, "concat_ws": true}

// coerceUTF16BlobArgs applies that conversion in place to a call's evaluated
// arguments; OpFunction passes a copy (fnArgBuf), so no register is touched.
// Every listed function reaches text through this one call. Other coercion
// sites are separate: CAST(blob AS TEXT) converts, while "||" does not yet
// (concatValues is still wrong in a UTF-16 database).
func coerceUTF16BlobArgs(name string, args []Value, enc TextEncoding) error {
	if !isUTF16(enc) {
		return nil
	}
	if utf16ByteCountedConcat[name] {
		for _, a := range args {
			if a.Typ == Blob {
				return fmt.Errorf("%w: %s() over a BLOB in a UTF-16 database (C SQLite sizes its result buffer from the BLOB's byte count but fills it from the CONVERTED text, reading past its own allocation when the conversion is shorter; see utf16ByteCountedConcat)", errVDBEUnsupported, name)
			}
		}
		return nil
	}
	if !textCoercingFuncs[name] {
		return nil
	}
	for i := range args {
		if args[i].Typ == Blob {
			args[i] = Value{Typ: Text, S: decodeTextBytes(enc, args[i].S)}
		}
	}
	return nil
}

// coerceUTF16BlobValue is coerceUTF16BlobArgs for one value, which is the shape
// the aggregate step functions need (sql_agg.go holds group_concat's value and
// separator as plain locals, not as an argument slice).
func coerceUTF16BlobValue(name string, v Value, enc TextEncoding) (Value, error) {
	args := [1]Value{v}
	if err := coerceUTF16BlobArgs(name, args[:], enc); err != nil {
		return v, err
	}
	return args[0], nil
}

// FileTextEncoding reports the text encoding recorded in the database file at
// path, and whether the file is initialized (not absent or zero-length).
//
// The driver uses it for ATTACH's same-encoding rule (execAttach applies the
// same rule for engine-direct sessions). C raises "attached databases must use
// the same text encoding as main database" (attach.c:207-211), guarded by
//
//	}else if( pNew->pSchema->file_format && pNew->pSchema->enc!=ENC(db) ){
//
// so only an initialized database is checked; a new or empty file adopts the
// connection's encoding:
//
//	UTF-8  main (with a table) + UTF-16 aux (with a table) -> error
//	UTF-16 main (with a table) + UTF-8  aux (with a table) -> error
//	EMPTY  main (reports UTF-8)+ UTF-16 aux (with a table) -> error
//	UTF-16 main (with a table) + BRAND-NEW aux             -> ok, aux becomes UTF-16le
func FileTextEncoding(path string) (enc TextEncoding, initialized bool, err error) {
	fi, serr := os.Stat(path)
	if serr != nil {
		if os.IsNotExist(serr) {
			return UTF8, false, nil
		}
		return UTF8, false, serr
	}
	if fi.Size() == 0 {
		return UTF8, false, nil
	}
	// The engine's own format keeps the encoding in its catalog, stamped once
	// the database holds a schema object -- C's file_format condition. Reading
	// the file as SQLite failed for every musql database, and the driver's
	// ATTACH check, which treats an unreadable file as "nothing to check", let
	// a mismatched encoding through.
	f, oerr := OpenSegmentFile(path)
	if oerr != nil {
		return UTF8, false, oerr
	}
	defer f.Close()
	if enc := TextEncoding(f.Catalog().Encoding); enc != 0 {
		return enc, true, nil
	}
	return UTF8, false, nil
}
