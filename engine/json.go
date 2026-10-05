// This file is SQLite's JSON core (src/json.c): JSONB encoding, text translation,
// path operations, merge-patch (RFC-7396), and validation. Ported function-by-function
// for exact behavioral match (json_funcs.go holds SQL functions, vtab_json_each.go holds modules).
//     the blob back to text, which is the same text either way. So the cache
//     is a speed-up only; nBlobAlloc, the one field it sets differently
//     (json.c:3693), is read only by jsonAfterEditSizeAdjust on a malformed
//     blob, and text never parses to a malformed blob.
//   - The zSpace[100] static buffer of JsonString. It changes which appends a
//     string accepts AFTER an error has been recorded, and every such string
//     ends in an error result.
//   - OOM. Allocation failure is not modelled; an internal-consistency breach
//     that C would turn into a memory error (an edit asked to delete bytes past
//     the end of a malformed blob) is reported as "malformed JSON" instead of
//     crashing, because the C behavior there is undefined.
//
// Every buffer read goes through a bounds-checked accessor that answers 0 past
// the end. For TEXT this is exactly C, whose input is NUL-terminated and whose
// scanner stops at the NUL (an embedded NUL ends the document the same way).
// For a BLOB it replaces a read C would make past the value's allocation.
package engine

import (
	"strconv"
)

// JSONB element types, json.c:125-137.
const (
	jsonbNull    = 0
	jsonbTrue    = 1
	jsonbFalse   = 2
	jsonbInt     = 3
	jsonbInt5    = 4
	jsonbFloat   = 5
	jsonbFloat5  = 6
	jsonbText    = 7
	jsonbTextJ   = 8
	jsonbText5   = 9
	jsonbTextRaw = 10
	jsonbArray   = 11
	jsonbObject  = 12
)

// jsonbTypeName is json.c:142's jsonbType[], indexed by the low nibble.
var jsonbTypeName = [16]string{
	"null", "true", "false", "integer", "integer",
	"real", "real", "text", "text", "text",
	"text", "array", "object", "", "", "",
}

// jsonMaxDepth is JSON_MAX_DEPTH, json.c:391.
const jsonMaxDepth = 1000

// jsonSubtype is JSON_SUBTYPE, json.c:322 ("J").
const jsonSubtype = 74

// Function flags carried in sqlite3_user_data, json.c:328-335.
const (
	jsonFlagJSON   = 0x01 // result is always JSON
	jsonFlagSQL    = 0x02 // result is always SQL
	jsonFlagABPath = 0x03 // allow abbreviated JSON path specs
	jsonFlagIsSet  = 0x04 // json_set(), not json_insert()
	jsonFlagAIns   = 0x08 // json_array_insert(), not json_insert()
	jsonFlagBlob   = 0x10 // use the BLOB output format
)

// JsonString.eErr bits, json.c:314-317.
const (
	jstringOOM       = 0x01
	jstringMalformed = 0x02
	jstringTooDeep   = 0x04
	jstringErr       = 0x08
)

// JsonParse.eEdit values, json.c:377-381.
const (
	jeditDel  = 1
	jeditRepl = 2
	jeditIns  = 3
	jeditSet  = 4
	jeditAIns = 5
)

// jsonLookupStep error returns, json.c:2899-2904.
const (
	jsonLookupError     = 0xffffffff
	jsonLookupNotFound  = 0xfffffffe
	jsonLookupNotArray  = 0xfffffffd
	jsonLookupTooDeep   = 0xfffffffc
	jsonLookupPathError = 0xfffffffb
)

func jsonLookupIsError(x uint32) bool { return x >= jsonLookupPathError }

// jsonInvalidChar is JSON_INVALID_CHAR, json.c:274.
const jsonInvalidChar = 0x99999

// jsonEscU00 is the six-byte escape prefix backslash-u-0-0, spelled by
// concatenation so no tool along the way decodes it.
const jsonEscU00 = "\\" + "u00"

// ---- character classes ----------------------------------------------------

// jsonIsOk is json.c:216's jsonIsOk[]: every byte except the control
// characters, the double quote, the single quote and the backslash.
func jsonIsOk(c byte) bool {
	return c >= 0x20 && c != '"' && c != '\'' && c != '\\'
}

// jsonIsSpace is json.c:153's jsonIsSpace[]: tab, newline, return, space.
func jsonIsSpace(c byte) bool {
	return c == 0x09 || c == 0x0a || c == 0x0d || c == 0x20
}

// The sqlite3CtypeMap predicates (global.c:118, sqliteInt.h:4684-4692).
func sqlIsSpace(c byte) bool  { return c == ' ' || (c >= 0x09 && c <= 0x0d) }
func sqlIsDigit(c byte) bool  { return c >= '0' && c <= '9' }
func sqlIsXdigit(c byte) bool { return isHexDigit(c) }
func sqlIsAlpha(c byte) bool  { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func sqlIsAlnum(c byte) bool  { return sqlIsAlpha(c) || sqlIsDigit(c) }

// sqlJSONID1/sqlJSONID2 are sqlite3JsonId1/2 (sqliteInt.h:4691): ctype bits
// 0x42 and 0x46 -- a letter (plus a digit for Id2), '$', '_', or any byte
// with the high bit set.
func sqlJSONID1(c byte) bool { return sqlIsAlpha(c) || c == '$' || c == '_' || c >= 0x80 }
func sqlJSONID2(c byte) bool { return sqlJSONID1(c) || sqlIsDigit(c) }


func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// jsonHexToInt is json.c:947 (and sqlite3HexToInt): only meaningful for a
// real hex digit, never asserting on anything else.
func jsonHexToInt(h byte) uint32 {
	x := uint32(h)
	x += 9 * (1 & (x >> 6))
	return x & 0xf
}

// byteAt reads z[i], answering 0 past the end -- the NUL terminator a C
// string would present there.
func byteAt(z []byte, i int) byte {
	if i < 0 || i >= len(z) {
		return 0
	}
	return z[i]
}

func jsonHexToInt4(z []byte, i int) uint32 {
	return jsonHexToInt(byteAt(z, i))<<12 | jsonHexToInt(byteAt(z, i+1))<<8 |
		jsonHexToInt(byteAt(z, i+2))<<4 | jsonHexToInt(byteAt(z, i+3))
}

func jsonIs2Hex(z []byte, i int) bool {
	return sqlIsXdigit(byteAt(z, i)) && sqlIsXdigit(byteAt(z, i+1))
}

func jsonIs4Hex(z []byte, i int) bool { return jsonIs2Hex(z, i) && jsonIs2Hex(z, i+2) }

// sqlStrNICmpPrefix is "sqlite3StrNICmp(&z[i], match, n)==0" for an ASCII
// match of length n.
func sqlStrNICmpPrefix(z []byte, i int, match string) bool {
	for k := 0; k < len(match); k++ {
		c := byteAt(z, i+k)
		m := match[k]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		if m >= 'A' && m <= 'Z' {
			m += 32
		}
		if c != m {
			return false
		}
	}
	return true
}

// ---- JsonString (json.c:303-897) -----------------------------------------

// jsonString is JsonString: a growing output buffer with sticky error bits.
// Errors that C pushes straight into the function context (too deep, a BLOB
// that is not JSONB) are pushed into ctx here.
type jsonString struct {
	ctx  *jsonCtx
	buf  []byte
	eErr uint8
}

func (p *jsonString) reset() { p.buf = p.buf[:0] }

func (p *jsonString) tooDeep() {
	p.eErr |= jstringTooDeep
	p.ctx.resultError("JSON nested too deep")
	p.reset()
}

func (p *jsonString) appendRaw(b []byte)    { p.buf = append(p.buf, b...) }
func (p *jsonString) appendRawStr(s string) { p.buf = append(p.buf, s...) }
func (p *jsonString) appendChar(c byte)     { p.buf = append(p.buf, c) }

// trimOneChar is jsonStringTrimOneChar, json.c:660.
func (p *jsonString) trimOneChar() {
	if p.eErr == 0 && len(p.buf) > 0 {
		p.buf = p.buf[:len(p.buf)-1]
	}
}

// appendSeparator is jsonAppendSeparator, json.c:682.
func (p *jsonString) appendSeparator() {
	if len(p.buf) == 0 {
		return
	}
	c := p.buf[len(p.buf)-1]
	if c == '[' || c == '{' {
		return
	}
	p.appendChar(',')
}

// appendControlChar is jsonAppendControlChar, json.c:696.
func (p *jsonString) appendControlChar(c byte) {
	var special byte
	switch c {
	case '\b':
		special = 'b'
	case '\t':
		special = 't'
	case '\n':
		special = 'n'
	case '\f':
		special = 'f'
	case '\r':
		special = 'r'
	}
	if special != 0 {
		p.buf = append(p.buf, '\\', special)
		return
	}
	const hex = "0123456789abcdef"
	p.buf = append(p.buf, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
}

// appendString is jsonAppendString, json.c:732: z quoted and escaped. A
// single quote is copied through (it is special only to JSON5 input).
func (p *jsonString) appendString(z []byte) {
	p.buf = append(p.buf, '"')
	for _, c := range z {
		switch {
		case jsonIsOk(c) || c == '\'':
			p.buf = append(p.buf, c)
		case c == '"' || c == '\\':
			p.buf = append(p.buf, '\\', c)
		default:
			p.appendControlChar(c)
		}
	}
	p.buf = append(p.buf, '"')
}

// appendSQLValue is jsonAppendSqlValue, json.c:803.
func (p *jsonString) appendSQLValue(v Value) {
	switch v.Typ {
	case Null:
		p.appendRawStr("null")
	case Float:
		p.appendRawStr(formatFloatLiteral(v.F))
	case Int:
		p.buf = strconv.AppendInt(p.buf, v.I, 10)
	case Text:
		if v.Subtype == jsonSubtype {
			p.appendRaw(v.S)
		} else {
			p.appendString(v.S)
		}
	default:
		var px jsonParse
		if jsonArgIsJsonb(v, &px) {
			px.translateBlobToText(0, p)
		} else if p.eErr == 0 {
			p.ctx.resultError("JSON cannot hold BLOB values")
			p.eErr = jstringErr
			p.reset()
		}
	}
}

// returnString is jsonReturnString, json.c:856, without the cache insert.
func (p *jsonString) returnString() {
	switch {
	case p.eErr == 0:
		if p.ctx.flags&jsonFlagBlob != 0 {
			p.returnStringAsBlob()
		} else {
			p.ctx.resultText(append([]byte(nil), p.buf...))
		}
	case p.eErr&jstringOOM != 0:
		p.ctx.resultError("out of memory")
	case p.eErr&jstringTooDeep != 0:
		// error already in ctx
	case p.eErr&jstringMalformed != 0:
		p.ctx.resultError("malformed JSON")
	}
	p.reset()
}

// returnStringAsBlob is jsonReturnStringAsBlob, json.c:2100: the text is
// well-formed JSON by construction, and a parse error is ignored.
func (p *jsonString) returnStringAsBlob() {
	var px jsonParse
	px.zJson = p.buf
	px.translateTextToBlob(0)
	p.ctx.resultBlob(append([]byte(nil), px.aBlob[:px.nBlob]...))
}

// ---- JsonParse (json.c:353-374) ------------------------------------------

// jsonParse is JsonParse. aBlob is the JSONB; when nBlobAlloc is non-zero the
// slice is owned and len(aBlob)==nBlobAlloc, otherwise it aliases an input
// value's bytes and must never be written (jsonBlobMakeEditable copies it).
type jsonParse struct {
	aBlob      []byte
	nBlob      uint32
	nBlobAlloc uint32
	zJson      []byte // text being parsed, NUL-less; byteAt supplies the terminator
	isText     bool   // zJson!=0 in C: the input was text (json_each's JSON column)
	iErr       uint32
	iDepth     uint16
	nErr       uint8
	oom        bool
	hasNonstd  bool
	bReadOnly  bool
	eEdit      uint8
	delta      int32
	nIns       uint32
	iLabel     uint32
	aIns       []byte
}

// at reads aBlob[i], 0 past the slice.
func (p *jsonParse) at(i uint32) byte {
	if uint64(i) >= uint64(len(p.aBlob)) {
		return 0
	}
	return p.aBlob[i]
}

// span is aBlob[from:to] clamped to the slice, for the payload reads C makes
// through a pointer whose bounds a malformed size may have stretched.
func (p *jsonParse) span(from, to uint32) []byte {
	n := uint32(len(p.aBlob))
	if to > n {
		to = n
	}
	if from > to {
		from = to
	}
	return p.aBlob[from:to]
}

// z reads the text being parsed, 0 at and past its end.
func (p *jsonParse) z(i uint32) byte {
	if uint64(i) >= uint64(len(p.zJson)) {
		return 0
	}
	return p.zJson[i]
}

// blobExpand is jsonBlobExpand, json.c:1153, with C's growth policy kept
// exactly because nBlobAlloc is itself observable (jsonAfterEditSizeAdjust).
func (p *jsonParse) blobExpand(n uint64) bool {
	var t uint64
	if p.nBlobAlloc == 0 {
		t = 100
	} else {
		t = uint64(p.nBlobAlloc) * 2
	}
	if t < n {
		t = n + 100
	}
	if t >= 0x7fffffff {
		p.oom = true
		return true
	}
	aNew := make([]byte, t)
	copy(aNew, p.aBlob)
	p.aBlob = aNew
	p.nBlobAlloc = uint32(t)
	return false
}

// blobMakeEditable is jsonBlobMakeEditable, json.c:1179.
func (p *jsonParse) blobMakeEditable(nExtra uint32) bool {
	if p.oom {
		return false
	}
	if p.nBlobAlloc > 0 {
		return true
	}
	aOld := p.aBlob
	nSize := uint64(p.nBlob) + uint64(nExtra)
	p.aBlob = nil
	if p.blobExpand(nSize) {
		return false
	}
	copy(p.aBlob, aOld[:min(uint32(len(aOld)), p.nBlob)])
	return true
}

// blobAppendOneByte is jsonBlobAppendOneByte, json.c:1211.
func (p *jsonParse) blobAppendOneByte(c byte) {
	if p.nBlob >= p.nBlobAlloc {
		if p.blobExpand(uint64(p.nBlob) + 1) {
			return
		}
	}
	p.aBlob[p.nBlob] = c
	p.nBlob++
}

// blobAppendNode is jsonBlobAppendNode, json.c:1243. withPayload false is C's
// aPayload==0: the header is written and room is reserved, but nBlob stays at
// the first payload byte.
func (p *jsonParse) blobAppendNode(eType byte, szPayload uint64, payload []byte, withPayload bool) {
	if uint64(p.nBlob)+szPayload+9 > uint64(p.nBlobAlloc) {
		if p.blobExpand(uint64(p.nBlob) + szPayload + 9) {
			return
		}
	}
	a := p.aBlob[p.nBlob:]
	switch {
	case szPayload <= 11:
		a[0] = eType | byte(szPayload<<4)
		p.nBlob++
	case szPayload <= 0xff:
		a[0] = eType | 0xc0
		a[1] = byte(szPayload)
		p.nBlob += 2
	case szPayload <= 0xffff:
		a[0] = eType | 0xd0
		a[1] = byte(szPayload >> 8)
		a[2] = byte(szPayload)
		p.nBlob += 3
	default:
		a[0] = eType | 0xe0
		a[1] = byte(szPayload >> 24)
		a[2] = byte(szPayload >> 16)
		a[3] = byte(szPayload >> 8)
		a[4] = byte(szPayload)
		p.nBlob += 5
	}
	if withPayload {
		copy(p.aBlob[p.nBlob:], payload[:szPayload])
		p.nBlob += uint32(szPayload)
	}
}

// blobChangePayloadSize is jsonBlobChangePayloadSize, json.c:1284.
func (p *jsonParse) blobChangePayloadSize(i uint32, szPayload uint32) int32 {
	if p.oom || i >= p.nBlob {
		return 0
	}
	szType := p.aBlob[i] >> 4
	var nExtra, nNeeded int32
	switch {
	case szType <= 11:
		nExtra = 0
	case szType == 12:
		nExtra = 1
	case szType == 13:
		nExtra = 2
	case szType == 14:
		nExtra = 4
	default:
		nExtra = 8
	}
	switch {
	case szPayload <= 11:
		nNeeded = 0
	case szPayload <= 0xff:
		nNeeded = 1
	case szPayload <= 0xffff:
		nNeeded = 2
	default:
		nNeeded = 4
	}
	delta := nNeeded - nExtra
	if delta != 0 {
		newSize := uint32(int64(p.nBlob) + int64(delta))
		if delta > 0 {
			if newSize > p.nBlobAlloc && p.blobExpand(uint64(newSize)) {
				return 0
			}
			copy(p.aBlob[int(i)+1+int(delta):], p.aBlob[i+1:p.nBlob])
		} else {
			copy(p.aBlob[i+1:], p.aBlob[int(i)+1-int(delta):p.nBlob])
		}
		p.nBlob = newSize
	}
	a := p.aBlob[i:]
	switch nNeeded {
	case 0:
		a[0] = (a[0] & 0x0f) | byte(szPayload<<4)
	case 1:
		a[0] = (a[0] & 0x0f) | 0xc0
		a[1] = byte(szPayload)
	case 2:
		a[0] = (a[0] & 0x0f) | 0xd0
		a[1] = byte(szPayload >> 8)
		a[2] = byte(szPayload)
	default:
		a[0] = (a[0] & 0x0f) | 0xe0
		a[1] = byte(szPayload >> 24)
		a[2] = byte(szPayload >> 16)
		a[3] = byte(szPayload >> 8)
		a[4] = byte(szPayload)
	}
	return delta
}

// payloadSize is jsonbPayloadSize, json.c:2123: the header size of the node
// at i (0 on any error) and its payload size.
func (p *jsonParse) payloadSize(i uint32) (n uint32, sz uint32) {
	if i >= p.nBlob {
		return 0, 0
	}
	x := p.at(i) >> 4
	switch {
	case x <= 11:
		sz = uint32(x)
		n = 1
	case x == 12:
		if i+1 >= p.nBlob {
			return 0, 0
		}
		sz = uint32(p.at(i + 1))
		n = 2
	case x == 13:
		if i+2 >= p.nBlob {
			return 0, 0
		}
		sz = uint32(p.at(i+1))<<8 + uint32(p.at(i+2))
		n = 3
	case x == 14:
		if i+4 >= p.nBlob {
			return 0, 0
		}
		sz = uint32(p.at(i+1))<<24 + uint32(p.at(i+2))<<16 + uint32(p.at(i+3))<<8 + uint32(p.at(i+4))
		n = 5
	default:
		if i+8 >= p.nBlob || p.at(i+1) != 0 || p.at(i+2) != 0 || p.at(i+3) != 0 || p.at(i+4) != 0 {
			return 0, 0
		}
		sz = uint32(p.at(i+5))<<24 + uint32(p.at(i+6))<<16 + uint32(p.at(i+7))<<8 + uint32(p.at(i+8))
		n = 9
	}
	if int64(i)+int64(sz)+int64(n) > int64(p.nBlob) &&
		int64(i)+int64(sz)+int64(n) > int64(p.nBlob)-int64(p.delta) {
		return 0, 0
	}
	return n, sz
}

// jsonIs4HexB is json.c:1355: z[i]=='u' followed by four hex digits.
func jsonIs4HexB(z []byte, i int, op *byte) bool {
	if byteAt(z, i) != 'u' || !jsonIs4Hex(z, i+1) {
		return false
	}
	*op = jsonbTextJ
	return true
}

// validityCheck is jsonbValidityCheck, json.c:1372: 0 when the element at i
// ending at iEnd is well-formed, else the 1-based offset of the problem.
func (p *jsonParse) validityCheck(i, iEnd, iDepth uint32) uint32 {
	if iDepth > jsonMaxDepth {
		return i + 1
	}
	n, sz := p.payloadSize(i)
	if n == 0 {
		return i + 1
	}
	if i+n+sz != iEnd {
		return i + 1
	}
	z := p.aBlob
	x := p.at(i) & 0x0f
	switch x {
	case jsonbNull, jsonbTrue, jsonbFalse:
		if n+sz == 1 {
			return 0
		}
		return i + 1
	case jsonbInt:
		if sz < 1 {
			return i + 1
		}
		j := i + n
		if p.at(j) == '-' {
			j++
			if sz < 2 {
				return i + 1
			}
		}
		k := i + n + sz
		for j < k {
			if !sqlIsDigit(p.at(j)) {
				return j + 1
			}
			j++
		}
		return 0
	case jsonbInt5:
		if sz < 3 {
			return i + 1
		}
		j := i + n
		if p.at(j) == '-' {
			if sz < 4 {
				return i + 1
			}
			j++
		}
		if p.at(j) != '0' {
			return i + 1
		}
		if p.at(j+1) != 'x' && p.at(j+1) != 'X' {
			return j + 2
		}
		j += 2
		k := i + n + sz
		for j < k {
			if !sqlIsXdigit(p.at(j)) {
				return j + 1
			}
			j++
		}
		return 0
	case jsonbFloat, jsonbFloat5:
		seen := 0 // 0: initial.  1: '.' seen  2: 'e' seen
		if sz < 2 {
			return i + 1
		}
		j := i + n
		k := j + sz
		if p.at(j) == '-' {
			j++
			if sz < 3 {
				return i + 1
			}
		}
		if p.at(j) == '.' {
			if x == jsonbFloat {
				return j + 1
			}
			if !sqlIsDigit(p.at(j + 1)) {
				return j + 1
			}
			j += 2
			seen = 1
		} else if p.at(j) == '0' && x == jsonbFloat {
			if j+3 > k {
				return j + 1
			}
			if p.at(j+1) != '.' && p.at(j+1) != 'e' && p.at(j+1) != 'E' {
				return j + 1
			}
			j++
		}
		for ; j < k; j++ {
			c := p.at(j)
			if sqlIsDigit(c) {
				continue
			}
			if c == '.' {
				if seen > 0 {
					return j + 1
				}
				if x == jsonbFloat && (j == k-1 || !sqlIsDigit(p.at(j+1))) {
					return j + 1
				}
				seen = 1
				continue
			}
			if c == 'e' || c == 'E' {
				if seen == 2 {
					return j + 1
				}
				if j == k-1 {
					return j + 1
				}
				if p.at(j+1) == '+' || p.at(j+1) == '-' {
					j++
					if j == k-1 {
						return j + 1
					}
				}
				seen = 2
				continue
			}
			return j + 1
		}
		if seen == 0 {
			return i + 1
		}
		return 0
	case jsonbText:
		j := i + n
		k := j + sz
		for j < k {
			if c := p.at(j); !jsonIsOk(c) && c != '\'' {
				return j + 1
			}
			j++
		}
		return 0
	case jsonbTextJ, jsonbText5:
		j := i + n
		k := j + sz
		for j < k {
			c := p.at(j)
			if !jsonIsOk(c) && c != '\'' {
				switch {
				case c == '"':
					if x == jsonbTextJ {
						return j + 1
					}
				case c <= 0x1f:
					// Control characters in JSON5 string literals are ok.
					if x == jsonbTextJ {
						return j + 1
					}
				case c != '\\' || j+1 >= k:
					return j + 1
				case jsonStrchr("\"\\/bfnrt", p.at(j+1)):
					j++
				case p.at(j+1) == 'u':
					if j+5 >= k {
						return j + 1
					}
					if !jsonIs4Hex(z, int(j)+2) {
						return j + 1
					}
					j++
				case x != jsonbText5:
					return j + 1
				default:
					var ch uint32
					szC := jsonUnescapeOneChar(z[j:], k-j, &ch)
					if ch == jsonInvalidChar {
						return j + 1
					}
					j += szC - 1
				}
			}
			j++
		}
		return 0
	case jsonbTextRaw:
		return 0
	case jsonbArray:
		j := i + n
		k := j + sz
		for j < k {
			n, sz = p.payloadSize(j)
			if n == 0 {
				return j + 1
			}
			if j+n+sz > k {
				return j + 1
			}
			if sub := p.validityCheck(j, j+n+sz, iDepth+1); sub != 0 {
				return sub
			}
			j += n + sz
		}
		return 0
	case jsonbObject:
		cnt := uint32(0)
		j := i + n
		k := j + sz
		for j < k {
			n, sz = p.payloadSize(j)
			if n == 0 {
				return j + 1
			}
			if j+n+sz > k {
				return j + 1
			}
			if cnt&1 == 0 {
				x = p.at(j) & 0x0f
				if x < jsonbText || x > jsonbTextRaw {
					return j + 1
				}
			}
			if sub := p.validityCheck(j, j+n+sz, iDepth+1); sub != 0 {
				return sub
			}
			cnt++
			j += n + sz
		}
		if cnt&1 != 0 {
			return j + 1
		}
		return 0
	}
	return i + 1
}

// jsonStrchr is "strchr(set, c)!=0": a NUL c matches the terminator.
func jsonStrchr(set string, c byte) bool {
	if c == 0 {
		return true
	}
	for i := 0; i < len(set); i++ {
		if set[i] == c {
			return true
		}
	}
	return false
}

// json5Whitespace is json.c:1019: the bytes of JSON5 whitespace (including
// comments) at the start of z[i:].
func json5Whitespace(z []byte, i int) int {
	n := 0
	for {
		switch byteAt(z, i+n) {
		case 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20:
			n++
		case '/':
			if byteAt(z, i+n+1) == '*' && byteAt(z, i+n+2) != 0 {
				j := n + 3
				for byteAt(z, i+j) != '/' || byteAt(z, i+j-1) != '*' {
					if byteAt(z, i+j) == 0 {
						return n
					}
					j++
				}
				n = j + 1
			} else if byteAt(z, i+n+1) == '/' {
				j := n + 2
				for ; byteAt(z, i+j) != 0; j++ {
					c := byteAt(z, i+j)
					if c == '\n' || c == '\r' {
						break
					}
					if c == 0xe2 && byteAt(z, i+j+1) == 0x80 &&
						(byteAt(z, i+j+2) == 0xa8 || byteAt(z, i+j+2) == 0xa9) {
						j += 2
						break
					}
				}
				n = j
				if byteAt(z, i+n) != 0 {
					n++
				}
			} else {
				return n
			}
		case 0xc2:
			if byteAt(z, i+n+1) == 0xa0 {
				n += 2
			} else {
				return n
			}
		case 0xe1:
			if byteAt(z, i+n+1) == 0x9a && byteAt(z, i+n+2) == 0x80 {
				n += 3
			} else {
				return n
			}
		case 0xe2:
			if byteAt(z, i+n+1) == 0x80 {
				c := byteAt(z, i+n+2)
				if c < 0x80 {
					return n
				}
				if c <= 0x8a || c == 0xa8 || c == 0xa9 || c == 0xaf {
					n += 3
				} else {
					return n
				}
			} else if byteAt(z, i+n+1) == 0x81 && byteAt(z, i+n+2) == 0x9f {
				n += 3
			} else {
				return n
			}
		case 0xe3:
			if byteAt(z, i+n+1) == 0x80 && byteAt(z, i+n+2) == 0x80 {
				n += 3
			} else {
				return n
			}
		case 0xef:
			if byteAt(z, i+n+1) == 0xbb && byteAt(z, i+n+2) == 0xbf {
				n += 3
			} else {
				return n
			}
		default:
			return n
		}
	}
}

// jsonNanInfName is json.c:1113's aNanInfName[].
var jsonNanInfName = []struct {
	c1, c2 byte
	match  string
	eType  byte
}{
	{'i', 'I', "inf", jsonbFloat},
	{'i', 'I', "infinity", jsonbFloat},
	{'n', 'N', "NaN", jsonbNull},
	{'q', 'Q', "QNaN", jsonbNull},
	{'s', 'S', "SNaN", jsonbNull},
}

// translateTextToBlob is jsonTranslateTextToBlob, json.c:1581: translate the
// element at zJson[i], returning the index just past it, or 0 at end of
// input, -1 on a syntax error, and -2/-3/-4/-5 for '}' ']' ',' ':' (with
// iErr set to that character's index).
func (p *jsonParse) translateTextToBlob(i uint32) int64 {
restart:
	switch c := p.z(i); c {
	case '{':
		iThis := p.nBlob
		p.blobAppendNode(jsonbObject, uint64(uint32(len(p.zJson))-i), nil, false)
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			p.iErr = i
			return -1
		}
		iStart := p.nBlob
		j := i + 1
		for ; ; j++ {
			iBlob := p.nBlob
			x := p.translateTextToBlob(j)
			if x <= 0 {
				if x == -2 {
					j = p.iErr
					if p.nBlob != iStart {
						p.hasNonstd = true
					}
					break
				}
				j += uint32(json5Whitespace(p.zJson, int(j)))
				op := byte(jsonbText)
				if sqlJSONID1(p.z(j)) || (p.z(j) == '\\' && jsonIs4HexB(p.zJson, int(j)+1, &op)) {
					k := j + 1
					for (sqlJSONID2(p.z(k)) && json5Whitespace(p.zJson, int(k)) == 0) ||
						(p.z(k) == '\\' && jsonIs4HexB(p.zJson, int(k)+1, &op)) {
						k++
					}
					p.blobAppendNode(op, uint64(k-j), p.zJson[j:], true)
					p.hasNonstd = true
					x = int64(k)
				} else {
					if x != -1 {
						p.iErr = j
					}
					return -1
				}
			}
			if p.oom {
				return -1
			}
			t := p.at(iBlob) & 0x0f
			if t < jsonbText || t > jsonbTextRaw {
				p.iErr = j
				return -1
			}
			j = uint32(x)
			if p.z(j) == ':' {
				j++
			} else {
				gotColon := false
				if jsonIsSpace(p.z(j)) {
					for {
						j++
						if !jsonIsSpace(p.z(j)) {
							break
						}
					}
					if p.z(j) == ':' {
						j++
						gotColon = true
					}
				}
				if !gotColon {
					x = p.translateTextToBlob(j)
					if x != -5 {
						if x != -1 {
							p.iErr = j
						}
						return -1
					}
					j = p.iErr + 1
				}
			}
			x = p.translateTextToBlob(j)
			if x <= 0 {
				if x != -1 {
					p.iErr = j
				}
				return -1
			}
			j = uint32(x)
			if p.z(j) == ',' {
				continue
			} else if p.z(j) == '}' {
				break
			}
			if jsonIsSpace(p.z(j)) {
				j++
				for jsonIsSpace(p.z(j)) {
					j++
				}
				if p.z(j) == ',' {
					continue
				} else if p.z(j) == '}' {
					break
				}
			}
			x = p.translateTextToBlob(j)
			if x == -4 {
				j = p.iErr
				continue
			}
			if x == -2 {
				j = p.iErr
				break
			}
			p.iErr = j
			return -1
		}
		p.blobChangePayloadSize(iThis, p.nBlob-iStart)
		p.iDepth--
		return int64(j) + 1
	case '[':
		iThis := p.nBlob
		p.blobAppendNode(jsonbArray, uint64(uint32(len(p.zJson))-i), nil, false)
		iStart := p.nBlob
		if p.oom {
			return -1
		}
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			p.iErr = i
			return -1
		}
		j := i + 1
		for ; ; j++ {
			x := p.translateTextToBlob(j)
			if x <= 0 {
				if x == -3 {
					j = p.iErr
					if p.nBlob != iStart {
						p.hasNonstd = true
					}
					break
				}
				if x != -1 {
					p.iErr = j
				}
				return -1
			}
			j = uint32(x)
			if p.z(j) == ',' {
				continue
			} else if p.z(j) == ']' {
				break
			}
			if jsonIsSpace(p.z(j)) {
				j++
				for jsonIsSpace(p.z(j)) {
					j++
				}
				if p.z(j) == ',' {
					continue
				} else if p.z(j) == ']' {
					break
				}
			}
			x = p.translateTextToBlob(j)
			if x == -4 {
				j = p.iErr
				continue
			}
			if x == -3 {
				j = p.iErr
				break
			}
			p.iErr = j
			return -1
		}
		p.blobChangePayloadSize(iThis, p.nBlob-iStart)
		p.iDepth--
		return int64(j) + 1
	case '\'', '"':
		if c == '\'' {
			p.hasNonstd = true
		}
		opcode := byte(jsonbText)
		cDelim := c
		j := i + 1
		for {
			if jsonIsOk(p.z(j)) {
				if !jsonIsOk(p.z(j + 1)) {
					j++
				} else if !jsonIsOk(p.z(j + 2)) {
					j += 2
				} else {
					j += 3
					continue
				}
			}
			ch := p.z(j)
			if ch == cDelim {
				break
			} else if ch == '\\' {
				j++
				ch = p.z(j)
				switch {
				case ch == '"' || ch == '\\' || ch == '/' || ch == 'b' || ch == 'f' ||
					ch == 'n' || ch == 'r' || ch == 't' || (ch == 'u' && jsonIs4Hex(p.zJson, int(j)+1)):
					if opcode == jsonbText {
						opcode = jsonbTextJ
					}
				case ch == '\'' || ch == 'v' || ch == '\n' ||
					(ch == '0' && !sqlIsDigit(p.z(j+1))) ||
					(ch == 0xe2 && p.z(j+1) == 0x80 && (p.z(j+2) == 0xa8 || p.z(j+2) == 0xa9)) ||
					(ch == 'x' && jsonIs2Hex(p.zJson, int(j)+1)):
					opcode = jsonbText5
					p.hasNonstd = true
				case ch == '\r':
					if p.z(j+1) == '\n' {
						j++
					}
					opcode = jsonbText5
					p.hasNonstd = true
				default:
					p.iErr = j
					return -1
				}
			} else if ch <= 0x1f {
				if ch == 0 {
					p.iErr = j
					return -1
				}
				// Control characters are not allowed in canonical JSON string
				// literals, but are allowed in JSON5 string literals.
				opcode = jsonbText5
				p.hasNonstd = true
			} else if ch == '"' {
				opcode = jsonbText5
			}
			j++
		}
		p.blobAppendNode(opcode, uint64(j-1-i), p.zJson[i+1:], true)
		return int64(j) + 1
	case 't':
		if jsonHasPrefix(p.zJson, i, "true") && !sqlIsAlnum(p.z(i+4)) {
			p.blobAppendOneByte(jsonbTrue)
			return int64(i) + 4
		}
		p.iErr = i
		return -1
	case 'f':
		if jsonHasPrefix(p.zJson, i, "false") && !sqlIsAlnum(p.z(i+5)) {
			p.blobAppendOneByte(jsonbFalse)
			return int64(i) + 5
		}
		p.iErr = i
		return -1
	case '+', '.', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.translateNumber(i)
	case '}':
		p.iErr = i
		return -2
	case ']':
		p.iErr = i
		return -3
	case ',':
		p.iErr = i
		return -4
	case ':':
		p.iErr = i
		return -5
	case 0:
		return 0
	case 0x09, 0x0a, 0x0d, 0x20:
		i++
		for jsonIsSpace(p.z(i)) {
			i++
		}
		goto restart
	case 0x0b, 0x0c, '/', 0xc2, 0xe1, 0xe2, 0xe3, 0xef:
		if j := json5Whitespace(p.zJson, int(i)); j > 0 {
			i += uint32(j)
			p.hasNonstd = true
			goto restart
		}
		p.iErr = i
		return -1
	default:
		if c == 'n' && jsonHasPrefix(p.zJson, i, "null") && !sqlIsAlnum(p.z(i+4)) {
			p.blobAppendOneByte(jsonbNull)
			return int64(i) + 4
		}
		for _, nm := range jsonNanInfName {
			if c != nm.c1 && c != nm.c2 {
				continue
			}
			if !sqlStrNICmpPrefix(p.zJson, int(i), nm.match) {
				continue
			}
			if sqlIsAlnum(p.z(i + uint32(len(nm.match)))) {
				continue
			}
			if nm.eType == jsonbFloat {
				p.blobAppendNode(jsonbFloat, 5, []byte("9e999"), true)
			} else {
				p.blobAppendOneByte(jsonbNull)
			}
			p.hasNonstd = true
			return int64(i) + int64(len(nm.match))
		}
		p.iErr = i
		return -1
	}
}

// jsonHasPrefix is "strncmp(z+i, lit, len)==0" (json.c:1813).
func jsonHasPrefix(z []byte, i uint32, lit string) bool {
	for k := 0; k < len(lit); k++ {
		if byteAt(z, int(i)+k) != lit[k] {
			return false
		}
	}
	return true
}

// translateNumber is the number arms of jsonTranslateTextToBlob,
// json.c:1828-1969 ('+', '.', '-', and the digits).
func (p *jsonParse) translateNumber(i uint32) int64 {
	var t byte // bit 0x01: JSON5.  bit 0x02: FLOAT
	var seenE bool
	var j uint32
	c := p.z(i)
	switch c {
	case '+':
		p.hasNonstd = true
		t = 0x00
	case '.':
		if sqlIsDigit(p.z(i + 1)) {
			p.hasNonstd = true
			t = 0x03
			goto parseNumber2
		}
		p.iErr = i
		return -1
	}
	// parse_number:
	if c <= '0' {
		if c == '0' {
			if (p.z(i+1) == 'x' || p.z(i+1) == 'X') && sqlIsXdigit(p.z(i+2)) {
				p.hasNonstd = true
				t = 0x01
				for j = i + 3; sqlIsXdigit(p.z(j)); j++ {
				}
				goto parseNumberFinish
			} else if sqlIsDigit(p.z(i + 1)) {
				p.iErr = i + 1
				return -1
			}
		} else {
			if !sqlIsDigit(p.z(i + 1)) {
				// JSON5 allows for "+Infinity" and "-Infinity" using exactly
				// that case.  SQLite also allows these in any case and it
				// allows "+inf" and "-inf".
				if (p.z(i+1) == 'I' || p.z(i+1) == 'i') && sqlStrNICmpPrefix(p.zJson, int(i)+1, "inf") {
					p.hasNonstd = true
					if p.z(i) == '-' {
						p.blobAppendNode(jsonbFloat, 6, []byte("-9e999"), true)
					} else {
						p.blobAppendNode(jsonbFloat, 5, []byte("9e999"), true)
					}
					if sqlStrNICmpPrefix(p.zJson, int(i)+4, "inity") {
						return int64(i) + 9
					}
					return int64(i) + 4
				}
				if p.z(i+1) == '.' {
					p.hasNonstd = true
					t |= 0x01
					goto parseNumber2
				}
				p.iErr = i
				return -1
			}
			if p.z(i+1) == '0' {
				if sqlIsDigit(p.z(i + 2)) {
					p.iErr = i + 1
					return -1
				} else if (p.z(i+2) == 'x' || p.z(i+2) == 'X') && sqlIsXdigit(p.z(i+3)) {
					p.hasNonstd = true
					t |= 0x01
					for j = i + 4; sqlIsXdigit(p.z(j)); j++ {
					}
					goto parseNumberFinish
				}
			}
		}
	}
parseNumber2:
	for j = i + 1; ; j++ {
		c = p.z(j)
		if sqlIsDigit(c) {
			continue
		}
		if c == '.' {
			if t&0x02 != 0 {
				p.iErr = j
				return -1
			}
			t |= 0x02
			continue
		}
		if c == 'e' || c == 'E' {
			if p.z(j-1) < '0' {
				if p.z(j-1) == '.' && j-2 >= i && sqlIsDigit(p.z(j-2)) {
					p.hasNonstd = true
					t |= 0x01
				} else {
					p.iErr = j
					return -1
				}
			}
			if seenE {
				p.iErr = j
				return -1
			}
			t |= 0x02
			seenE = true
			c = p.z(j + 1)
			if c == '+' || c == '-' {
				j++
				c = p.z(j + 1)
			}
			if c < '0' || c > '9' {
				p.iErr = j
				return -1
			}
			continue
		}
		break
	}
	if p.z(j-1) < '0' {
		if p.z(j-1) == '.' && j-2 >= i && sqlIsDigit(p.z(j-2)) {
			p.hasNonstd = true
			t |= 0x01
		} else {
			p.iErr = j
			return -1
		}
	}
parseNumberFinish:
	if p.z(i) == '+' {
		i++
	}
	p.blobAppendNode(jsonbInt+t, uint64(j-i), p.zJson[i:], true)
	return int64(j)
}

// convertTextToBlob is jsonConvertTextToBlob, json.c:2055, reporting any
// error into ctx when ctx is non-nil. true means failure.
func (p *jsonParse) convertTextToBlob(ctx *jsonCtx) bool {
	i := p.translateTextToBlob(0)
	if p.oom {
		i = -1
	}
	if i > 0 {
		k := uint32(i)
		for jsonIsSpace(p.z(k)) {
			k++
		}
		if p.z(k) != 0 {
			k += uint32(json5Whitespace(p.zJson, int(k)))
			if p.z(k) != 0 {
				if ctx != nil {
					ctx.resultError("malformed JSON")
				}
				p.resetBlob()
				return true
			}
			p.hasNonstd = true
		}
	}
	if i <= 0 {
		if ctx != nil {
			if p.oom {
				ctx.resultError("out of memory")
			} else {
				ctx.resultError("malformed JSON")
			}
		}
		p.resetBlob()
		return true
	}
	return false
}

// resetBlob is the aBlob half of jsonParseReset, json.c:906.
func (p *jsonParse) resetBlob() {
	if p.nBlobAlloc > 0 {
		p.aBlob = nil
		p.nBlob = 0
		p.nBlobAlloc = 0
	}
}

// translateBlobToText is jsonTranslateBlobToText, json.c:2193.
func (p *jsonParse) translateBlobToText(i uint32, pOut *jsonString) uint32 {
	n, sz := p.payloadSize(i)
	if n == 0 {
		pOut.eErr |= jstringMalformed
		return p.nBlob + 1
	}
	switch p.at(i) & 0x0f {
	case jsonbNull:
		pOut.appendRawStr("null")
		return i + 1
	case jsonbTrue:
		pOut.appendRawStr("true")
		return i + 1
	case jsonbFalse:
		pOut.appendRawStr("false")
		return i + 1
	case jsonbInt, jsonbFloat:
		if sz == 0 {
			pOut.eErr |= jstringMalformed
			break
		}
		pOut.appendRaw(p.span(i+n, i+n+sz))
	case jsonbInt5:
		// Integer literal in hexadecimal notation.
		k := uint32(2)
		var u uint64
		zIn := p.span(i+n, i+n+sz)
		bOverflow := false
		if sz == 0 {
			pOut.eErr |= jstringMalformed
			break
		}
		if byteAt(zIn, 0) == '-' {
			pOut.appendChar('-')
			k++
		} else if byteAt(zIn, 0) == '+' {
			k++
		}
		for ; k < sz; k++ {
			if c := byteAt(zIn, int(k)); !sqlIsXdigit(c) {
				pOut.eErr |= jstringMalformed
				break
			} else if u>>60 != 0 {
				bOverflow = true
			} else {
				u = u*16 + uint64(jsonHexToInt(c))
			}
		}
		if bOverflow {
			pOut.appendRawStr("9.0e999")
		} else {
			pOut.buf = strconv.AppendUint(pOut.buf, u, 10)
		}
	case jsonbFloat5:
		// Float literal missing digits beside ".".
		k := uint32(0)
		zIn := p.span(i+n, i+n+sz)
		if sz == 0 {
			pOut.eErr |= jstringMalformed
			break
		}
		if byteAt(zIn, 0) == '-' {
			pOut.appendChar('-')
			k++
		}
		if byteAt(zIn, int(k)) == '.' {
			pOut.appendChar('0')
		}
		for ; k < sz; k++ {
			c := byteAt(zIn, int(k))
			pOut.appendChar(c)
			if c == '.' && (k+1 == sz || !sqlIsDigit(byteAt(zIn, int(k)+1))) {
				pOut.appendChar('0')
			}
		}
	case jsonbText, jsonbTextJ:
		pOut.appendChar('"')
		pOut.appendRaw(p.span(i+n, i+n+sz))
		pOut.appendChar('"')
	case jsonbText5:
		zIn := p.span(i+n, i+n+sz)
		pOut.appendChar('"')
		for len(zIn) > 0 {
			k := 0
			for k < len(zIn) && (jsonIsOk(zIn[k]) || zIn[k] == '\'') {
				k++
			}
			if k > 0 {
				pOut.appendRaw(zIn[:k])
				if k >= len(zIn) {
					break
				}
				zIn = zIn[k:]
			}
			if zIn[0] == '"' {
				pOut.appendRawStr(`\"`)
				zIn = zIn[1:]
				continue
			}
			if zIn[0] <= 0x1f {
				pOut.appendControlChar(zIn[0])
				zIn = zIn[1:]
				continue
			}
			if len(zIn) < 2 {
				pOut.eErr |= jstringMalformed
				break
			}
			switch zIn[1] {
			case '\'':
				pOut.appendChar('\'')
			case 'v':
				pOut.appendRawStr(jsonEscU00 + "0b")
			case 'x':
				if len(zIn) < 4 {
					pOut.eErr |= jstringMalformed
					zIn = zIn[:2]
					break
				}
				pOut.appendRawStr(jsonEscU00)
				pOut.appendRaw(zIn[2:4])
				zIn = zIn[2:]
			case '0':
				pOut.appendRawStr(jsonEscU00 + "00")
			case '\r':
				if len(zIn) > 2 && zIn[2] == '\n' {
					zIn = zIn[1:]
				}
			case '\n':
			case 0xe2:
				// '\' followed by either U+2028 or U+2029 is ignored as
				// whitespace.
				if len(zIn) < 4 || zIn[2] != 0x80 || (zIn[3] != 0xa8 && zIn[3] != 0xa9) {
					pOut.eErr |= jstringMalformed
					zIn = zIn[:2]
					break
				}
				zIn = zIn[2:]
			default:
				pOut.appendRaw(zIn[:2])
			}
			zIn = zIn[2:]
		}
		pOut.appendChar('"')
	case jsonbTextRaw:
		pOut.appendString(p.span(i+n, i+n+sz))
	case jsonbArray:
		pOut.appendChar('[')
		j := i + n
		iEnd := j + sz
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			pOut.tooDeep()
		}
		for j < iEnd && pOut.eErr == 0 {
			j = p.translateBlobToText(j, pOut)
			pOut.appendChar(',')
		}
		p.iDepth--
		if j > iEnd {
			pOut.eErr |= jstringMalformed
		}
		if sz > 0 {
			pOut.trimOneChar()
		}
		pOut.appendChar(']')
	case jsonbObject:
		x := 0
		pOut.appendChar('{')
		j := i + n
		iEnd := j + sz
		p.iDepth++
		if p.iDepth > jsonMaxDepth {
			pOut.tooDeep()
		}
		for j < iEnd && pOut.eErr == 0 {
			j = p.translateBlobToText(j, pOut)
			if x&1 != 0 {
				pOut.appendChar(',')
			} else {
				pOut.appendChar(':')
			}
			x++
		}
		p.iDepth--
		if x&1 != 0 || j > iEnd {
			pOut.eErr |= jstringMalformed
		}
		if sz > 0 {
			pOut.trimOneChar()
		}
		pOut.appendChar('}')
	default:
		pOut.eErr |= jstringMalformed
	}
	return i + n + sz
}

// jsonPretty is JsonPretty, json.c:2420.
type jsonPretty struct {
	pParse  *jsonParse
	pOut    *jsonString
	zIndent []byte
	nIndent uint32
}

func (pp *jsonPretty) indent() {
	for jj := uint32(0); jj < pp.nIndent; jj++ {
		pp.pOut.appendRaw(pp.zIndent)
	}
}

// translate is jsonTranslateBlobToPrettyText, json.c:2453.
func (pp *jsonPretty) translate(i uint32) uint32 {
	p := pp.pParse
	pOut := pp.pOut
	n, sz := p.payloadSize(i)
	if n == 0 {
		pOut.eErr |= jstringMalformed
		return p.nBlob + 1
	}
	switch p.at(i) & 0x0f {
	case jsonbArray:
		j := i + n
		iEnd := j + sz
		pOut.appendChar('[')
		if j < iEnd {
			pOut.appendChar('\n')
			pp.nIndent++
			if pp.nIndent >= jsonMaxDepth {
				pOut.tooDeep()
			}
			for pOut.eErr == 0 {
				pp.indent()
				j = pp.translate(j)
				if j >= iEnd {
					break
				}
				pOut.appendRawStr(",\n")
			}
			pOut.appendChar('\n')
			pp.nIndent--
			pp.indent()
		}
		pOut.appendChar(']')
		i = iEnd
	case jsonbObject:
		j := i + n
		iEnd := j + sz
		pOut.appendChar('{')
		if j < iEnd {
			pOut.appendChar('\n')
			pp.nIndent++
			if pp.nIndent >= jsonMaxDepth {
				pOut.tooDeep()
			}
			p.iDepth = uint16(pp.nIndent)
			for pOut.eErr == 0 {
				pp.indent()
				j = p.translateBlobToText(j, pOut)
				if j > iEnd {
					pOut.eErr |= jstringMalformed
					break
				}
				pOut.appendRawStr(": ")
				j = pp.translate(j)
				if j >= iEnd {
					break
				}
				pOut.appendRawStr(",\n")
			}
			pOut.appendChar('\n')
			pp.nIndent--
			pp.indent()
		}
		pOut.appendChar('}')
		i = iEnd
	default:
		i = p.translateBlobToText(i, pOut)
	}
	return i
}

// arrayCount is jsonbArrayCount, json.c:2533.
func (p *jsonParse) arrayCount(iRoot uint32) uint32 {
	k := uint32(0)
	n, sz := p.payloadSize(iRoot)
	iEnd := iRoot + n + sz
	for i := iRoot + n; n > 0 && i < iEnd; i, k = i+sz+n, k+1 {
		n, sz = p.payloadSize(i)
	}
	return k
}

// afterEditSizeAdjust is jsonAfterEditSizeAdjust, json.c:2548.
func (p *jsonParse) afterEditSizeAdjust(iRoot uint32) {
	nBlob := p.nBlob
	p.nBlob = p.nBlobAlloc
	_, sz := p.payloadSize(iRoot)
	p.nBlob = nBlob
	sz = uint32(int64(sz) + int64(p.delta))
	p.delta += p.blobChangePayloadSize(iRoot, sz)
}

// jsonBlobOverwrite is json.c:2577: rewrite aIns into aOut with its header
// widened by d bytes, so an overwrite need not move the rest of the blob.
// This optimization is observable in a jsonb_* result's bytes.
func jsonBlobOverwrite(aOut []byte, aIns []byte, nIns uint32, d uint32) bool {
	aType := [8]byte{0xc0, 0xd0, 0, 0xe0, 0, 0, 0, 0xf0}
	if nIns == 0 || len(aIns) == 0 || aIns[0]&0x0f <= 2 {
		return false // cannot enlarge NULL, true, false
	}
	var i uint32
	var szHdr uint32
	switch aIns[0] >> 4 {
	default: // header size 1
		if (1<<d)&0x116 == 0 {
			return false
		}
		i = d + 1
		szHdr = 1
	case 12: // header size 2
		if (1<<d)&0x8a == 0 {
			return false
		}
		i = d + 2
		szHdr = 2
	case 13: // header size 3
		if d != 2 && d != 6 {
			return false
		}
		i = d + 3
		szHdr = 3
	case 14: // header size 5
		if d != 4 {
			return false
		}
		i = 9
		szHdr = 5
	case 15:
		return false
	}
	if nIns < szHdr || uint64(nIns) > uint64(len(aIns)) || uint64(i)+uint64(nIns-szHdr) > uint64(len(aOut)) {
		return false
	}
	aOut[0] = (aIns[0] & 0x0f) | aType[i-2]
	copy(aOut[i:], aIns[szHdr:nIns])
	szPayload := nIns - szHdr
	for {
		i--
		aOut[i] = byte(szPayload)
		if i == 1 {
			break
		}
		szPayload >>= 8
	}
	return true
}

// blobEdit is jsonBlobEdit, json.c:2650: replace nDel bytes at iDel with
// nIns bytes of aIns (or uninitialized room when aIns is nil).
func (p *jsonParse) blobEdit(iDel, nDel uint32, aIns []byte, nIns uint32) {
	if uint64(iDel)+uint64(nDel) > uint64(p.nBlob) || p.nBlobAlloc == 0 {
		// A malformed blob's sizes pointed an edit past its end, which C
		// asserts cannot happen; report it rather than corrupting memory.
		p.oom = true
		return
	}
	d := int64(nIns) - int64(nDel)
	if d < 0 && d >= -8 && aIns != nil &&
		jsonBlobOverwrite(p.aBlob[iDel:], aIns, nIns, uint32(-d)) {
		return
	}
	if d != 0 {
		if int64(p.nBlob)+d > int64(p.nBlobAlloc) {
			if p.blobExpand(uint64(int64(p.nBlob) + d)) {
				return
			}
		}
		copy(p.aBlob[iDel+nIns:], p.aBlob[iDel+nDel:p.nBlob])
		p.nBlob = uint32(int64(p.nBlob) + d)
		p.delta += int32(d)
	}
	if nIns > 0 && aIns != nil {
		copy(p.aBlob[iDel:iDel+nIns], aIns)
	}
}

// jsonBytesToBypass is json.c:2690: the escaped newlines at the start of z.
func jsonBytesToBypass(z []byte, n uint32) uint32 {
	i := uint32(0)
	for i+1 < n {
		if byteAt(z, int(i)) != '\\' {
			return i
		}
		if byteAt(z, int(i)+1) == '\n' {
			i += 2
			continue
		}
		if byteAt(z, int(i)+1) == '\r' {
			if i+2 < n && byteAt(z, int(i)+2) == '\n' {
				i += 3
			} else {
				i += 2
			}
			continue
		}
		if byteAt(z, int(i)+1) == 0xe2 && i+3 < n && byteAt(z, int(i)+2) == 0x80 &&
			(byteAt(z, int(i)+3) == 0xa8 || byteAt(z, int(i)+3) == 0xa9) {
			i += 4
			continue
		}
		break
	}
	return i
}

// jsonUnescapeOneChar is json.c:2728: decode the escape at z[0] ('\\'),
// limited to n bytes, into *piOut; returns the bytes consumed.
func jsonUnescapeOneChar(z []byte, n uint32, piOut *uint32) uint32 {
	if n < 2 {
		*piOut = jsonInvalidChar
		return n
	}
	switch byteAt(z, 1) {
	case 'u':
		if n < 6 {
			*piOut = jsonInvalidChar
			return n
		}
		v := jsonHexToInt4(z, 2)
		if v&0xfc00 == 0xd800 && n >= 12 && byteAt(z, 6) == '\\' && byteAt(z, 7) == 'u' {
			if vlo := jsonHexToInt4(z, 8); vlo&0xfc00 == 0xdc00 {
				*piOut = ((v & 0x3ff) << 10) + (vlo & 0x3ff) + 0x10000
				return 12
			}
		}
		*piOut = v
		return 6
	case 'b':
		*piOut = '\b'
		return 2
	case 'f':
		*piOut = '\f'
		return 2
	case 'n':
		*piOut = '\n'
		return 2
	case 'r':
		*piOut = '\r'
		return 2
	case 't':
		*piOut = '\t'
		return 2
	case 'v':
		*piOut = '\v'
		return 2
	case '0':
		// JSON5 requires that the \0 escape not be followed by a digit.
		if n > 2 && sqlIsDigit(byteAt(z, 2)) {
			*piOut = jsonInvalidChar
		} else {
			*piOut = 0
		}
		return 2
	case '\'', '"', '/', '\\':
		*piOut = uint32(byteAt(z, 1))
		return 2
	case 'x':
		if n < 4 {
			*piOut = jsonInvalidChar
			return n
		}
		*piOut = jsonHexToInt(byteAt(z, 2))<<4 | jsonHexToInt(byteAt(z, 3))
		return 4
	case 0xe2, '\r', '\n':
		nSkip := jsonBytesToBypass(z, n)
		if nSkip == 0 {
			*piOut = jsonInvalidChar
			return n
		} else if nSkip == n {
			*piOut = 0
			return n
		} else if byteAt(z, int(nSkip)) == '\\' {
			return nSkip + jsonUnescapeOneChar(z[nSkip:], n-nSkip, piOut)
		}
		sz := sqliteUtf8ReadLimited(z[nSkip:], n-nSkip, piOut)
		return nSkip + sz
	default:
		*piOut = jsonInvalidChar
		return 2
	}
}

// sqliteUtf8ReadLimited is sqlite3Utf8ReadLimited, utf.c:208.
func sqliteUtf8ReadLimited(z []byte, n uint32, piOut *uint32) uint32 {
	i := uint32(1)
	c := uint32(byteAt(z, 0))
	if c >= 0xc0 {
		c = uint32(sqlite3Utf8Trans1[c-0xc0])
		if n > 4 {
			n = 4
		}
		for i < n && byteAt(z, int(i))&0xc0 == 0x80 {
			c = (c << 6) + uint32(0x3f&byteAt(z, int(i)))
			i++
		}
	}
	*piOut = c
	return i
}

// jsonLabelCompareEscaped is json.c:2821.
func jsonLabelCompareEscaped(zLeft []byte, rawLeft bool, zRight []byte, rawRight bool) bool {
	for {
		var cLeft, cRight uint32
		if len(zLeft) == 0 {
			cLeft = 0
		} else if rawLeft || zLeft[0] != '\\' {
			cLeft = uint32(zLeft[0])
			if cLeft >= 0xc0 {
				sz := sqliteUtf8ReadLimited(zLeft, uint32(len(zLeft)), &cLeft)
				zLeft = zLeft[sz:]
			} else {
				zLeft = zLeft[1:]
			}
		} else {
			n := jsonUnescapeOneChar(zLeft, uint32(len(zLeft)), &cLeft)
			zLeft = zLeft[min(int(n), len(zLeft)):]
		}
		if len(zRight) == 0 {
			cRight = 0
		} else if rawRight || zRight[0] != '\\' {
			cRight = uint32(zRight[0])
			if cRight >= 0xc0 {
				sz := sqliteUtf8ReadLimited(zRight, uint32(len(zRight)), &cRight)
				zRight = zRight[sz:]
			} else {
				zRight = zRight[1:]
			}
		} else {
			n := jsonUnescapeOneChar(zRight, uint32(len(zRight)), &cRight)
			zRight = zRight[min(int(n), len(zRight)):]
		}
		if cLeft != cRight {
			return false
		}
		if cLeft == 0 {
			return true
		}
	}
}

// jsonLabelCompare is json.c:2877.
func jsonLabelCompare(zLeft []byte, rawLeft bool, zRight []byte, rawRight bool) bool {
	if rawLeft && rawRight {
		return string(zLeft) == string(zRight)
	}
	return jsonLabelCompareEscaped(zLeft, rawLeft, zRight, rawRight)
}

// jsonCreateEditSubstructure is json.c:2929.
func (p *jsonParse) createEditSubstructure(pIns *jsonParse, path []byte, tail int) uint32 {
	*pIns = jsonParse{}
	if byteAt(path, tail) == 0 {
		// No substructure.  Just insert what is given in pParse.
		pIns.aBlob = p.aIns
		pIns.nBlob = p.nIns
		return 0
	}
	// Construct the binary substructure.
	pIns.nBlob = 1
	if byteAt(path, tail) == '.' {
		pIns.aBlob = []byte{jsonbObject}
	} else {
		pIns.aBlob = []byte{jsonbArray}
	}
	pIns.eEdit = p.eEdit
	pIns.nIns = p.nIns
	pIns.aIns = p.aIns
	pIns.iDepth = p.iDepth + 1
	if pIns.iDepth >= jsonMaxDepth {
		return jsonLookupTooDeep
	}
	rc := pIns.lookupStep(0, path, tail, 0)
	p.iDepth--
	p.oom = p.oom || pIns.oom
	return rc
}

// lookupStep is jsonLookupStep, json.c:2978. path[pos:] is the remaining
// path; path itself is kept so JEDIT_AINS can look at the byte BEFORE pos.
func (p *jsonParse) lookupStep(iRoot uint32, path []byte, pos int, iLabel uint32) uint32 {
	zp := func(k int) byte { return byteAt(path, pos+k) }
	if zp(0) == 0 {
		if p.eEdit != 0 && p.blobMakeEditable(p.nIns) {
			n, sz := p.payloadSize(iRoot)
			sz += n
			switch p.eEdit {
			case jeditDel:
				if iLabel > 0 {
					sz += iRoot - iLabel
					iRoot = iLabel
				}
				p.blobEdit(iRoot, sz, nil, 0)
			case jeditIns:
				// Already exists, so json_insert() is a no-op.
			case jeditAIns:
				if byteAt(path, pos-1) != ']' {
					return jsonLookupNotArray
				}
				p.blobEdit(iRoot, 0, p.aIns, p.nIns)
			default:
				// json_set() or json_replace()
				p.blobEdit(iRoot, sz, p.aIns, p.nIns)
			}
		}
		p.iLabel = iLabel
		return iRoot
	}
	if zp(0) == '.' {
		rawKey := true
		x := p.at(iRoot)
		pos++
		var zKey []byte
		var nKey, i int
		if zp(0) == '"' {
			i = 1
			for zp(i) != 0 && zp(i) != '"' {
				if zp(i) == '\\' && zp(i+1) != 0 {
					i++
				}
				i++
			}
			nKey = i - 1
			if zp(i) != 0 {
				i++
			} else {
				return jsonLookupPathError
			}
			zKey = path[pos+1 : pos+1+nKey]
			for _, c := range zKey {
				if c == '\\' {
					rawKey = false
					break
				}
			}
		} else {
			for i = 0; zp(i) != 0 && zp(i) != '.' && zp(i) != '['; i++ {
			}
			nKey = i
			if nKey == 0 {
				return jsonLookupPathError
			}
			zKey = path[pos : pos+nKey]
		}
		if x&0x0f != jsonbObject {
			return jsonLookupNotFound
		}
		n, sz := p.payloadSize(iRoot)
		j := iRoot + n // j is the index of a label
		iEnd := j + sz
		for j < iEnd {
			x = p.at(j) & 0x0f
			if x < jsonbText || x > jsonbTextRaw {
				return jsonLookupError
			}
			n, sz = p.payloadSize(j)
			if n == 0 {
				return jsonLookupError
			}
			k := j + n // k is the index of the label text
			if k+sz >= iEnd {
				return jsonLookupError
			}
			zLabel := p.span(k, k+sz)
			rawLabel := x == jsonbText || x == jsonbTextRaw
			if jsonLabelCompare(zKey, rawKey, zLabel, rawLabel) {
				v := k + sz // v is the index of the value
				if p.at(v)&0x0f > jsonbObject {
					return jsonLookupError
				}
				n, sz = p.payloadSize(v)
				if n == 0 || v+n+sz > iEnd {
					return jsonLookupError
				}
				p.iDepth++
				if p.iDepth >= jsonMaxDepth {
					return jsonLookupTooDeep
				}
				rc := p.lookupStep(v, path, pos+i, j)
				p.iDepth--
				if p.delta != 0 {
					p.afterEditSizeAdjust(iRoot)
				}
				return rc
			}
			j = k + sz
			if p.at(j)&0x0f > jsonbObject {
				return jsonLookupError
			}
			n, sz = p.payloadSize(j)
			if n == 0 {
				return jsonLookupError
			}
			j += n + sz
		}
		if j > iEnd {
			return jsonLookupError
		}
		if p.eEdit >= jeditIns {
			if p.eEdit == jeditAIns && !jsonGlobEndsBracket(path, pos+i) {
				return jsonLookupNotArray
			}
			var ix jsonParse
			if rawKey {
				ix.blobAppendNode(jsonbTextRaw, uint64(nKey), nil, false)
			} else {
				ix.blobAppendNode(jsonbText5, uint64(nKey), nil, false)
			}
			p.oom = p.oom || ix.oom
			var v jsonParse
			rc := p.createEditSubstructure(&v, path, pos+i)
			if !jsonLookupIsError(rc) && p.blobMakeEditable(ix.nBlob+uint32(nKey)+v.nBlob) {
				nIns := ix.nBlob + uint32(nKey) + v.nBlob
				p.blobEdit(j, 0, nil, nIns)
				if !p.oom {
					copy(p.aBlob[j:], ix.aBlob[:ix.nBlob])
					k := j + ix.nBlob
					copy(p.aBlob[k:], zKey)
					k += uint32(nKey)
					copy(p.aBlob[k:], v.aBlob[:v.nBlob])
					if p.delta != 0 {
						p.afterEditSizeAdjust(iRoot)
					}
				}
			}
			return rc
		}
	} else if zp(0) == '[' {
		var kk uint64
		x := p.at(iRoot) & 0x0f
		if x != jsonbArray {
			return jsonLookupNotFound
		}
		n, sz := p.payloadSize(iRoot)
		i := 1
		for sqlIsDigit(zp(i)) {
			if kk < 0xffffffff {
				kk = kk*10 + uint64(zp(i)-'0')
			}
			i++
		}
		if i < 2 || zp(i) != ']' {
			if zp(1) == '#' {
				kk = uint64(p.arrayCount(iRoot))
				i = 2
				if zp(2) == '-' && sqlIsDigit(zp(3)) {
					var nn uint64
					i = 3
					for {
						if nn < 0xffffffff {
							nn = nn*10 + uint64(zp(i)-'0')
						}
						i++
						if !sqlIsDigit(zp(i)) {
							break
						}
					}
					if nn > kk {
						return jsonLookupNotFound
					}
					kk -= nn
				}
				if zp(i) != ']' {
					return jsonLookupPathError
				}
			} else {
				return jsonLookupPathError
			}
		}
		j := iRoot + n
		iEnd := j + sz
		for j < iEnd {
			if kk == 0 {
				p.iDepth++
				if p.iDepth >= jsonMaxDepth {
					return jsonLookupTooDeep
				}
				rc := p.lookupStep(j, path, pos+i+1, 0)
				p.iDepth--
				if p.delta != 0 {
					p.afterEditSizeAdjust(iRoot)
				}
				return rc
			}
			kk--
			n, sz = p.payloadSize(j)
			if n == 0 {
				return jsonLookupError
			}
			j += n + sz
		}
		if j > iEnd {
			return jsonLookupError
		}
		if kk > 0 {
			return jsonLookupNotFound
		}
		if p.eEdit >= jeditIns {
			var v jsonParse
			rc := p.createEditSubstructure(&v, path, pos+i+1)
			if !jsonLookupIsError(rc) && p.blobMakeEditable(v.nBlob) {
				p.blobEdit(j, 0, v.aBlob, v.nBlob)
			}
			if p.delta != 0 {
				p.afterEditSizeAdjust(iRoot)
			}
			return rc
		}
	} else {
		return jsonLookupPathError
	}
	return jsonLookupNotFound
}

// jsonGlobEndsBracket is "sqlite3_strglob("*]", &zPath[i])==0": the rest of
// the path (up to its NUL) ends in ']'.
func jsonGlobEndsBracket(path []byte, pos int) bool {
	end := pos
	for byteAt(path, end) != 0 {
		end++
	}
	return end > pos && path[end-1] == ']'
}

// jsonArgIsJsonb is json.c:3620: does v look like a JSONB blob? A full
// validity check is made only for small blobs, which might be text JSON cast
// to a BLOB.
func jsonArgIsJsonb(v Value, p *jsonParse) bool {
	if v.Typ != Blob {
		return false
	}
	p.aBlob = v.S
	p.nBlob = uint32(len(v.S))
	if p.nBlob > 0 {
		c := p.aBlob[0]
		if c&0x0f <= jsonbObject {
			n, sz := p.payloadSize(0)
			if n > 0 && sz+n == p.nBlob && (c&0x0f > jsonbFalse || sz == 0) &&
				(sz > 7 || (c != 0x7b && c != 0x5b && !sqlIsDigit(c)) ||
					p.validityCheck(0, p.nBlob, 1) == 0) {
				return true
			}
		}
	}
	p.aBlob = nil
	p.nBlob = 0
	return false
}

// mergePatch is jsonMergePatch, json.c:4236 (RFC 7396). Return codes:
const (
	jsonMergeOK        = 0
	jsonMergeBadTarget = 1
	jsonMergeBadPatch  = 2
	jsonMergeOOM       = 3
	jsonMergeTooDeep   = 4
)

func (pTarget *jsonParse) mergePatch(iTarget uint32, pPatch *jsonParse, iPatch uint32, iDepth uint32) int {
	x := pPatch.at(iPatch) & 0x0f
	if x != jsonbObject { // Algorithm line 02
		n, sz := pPatch.payloadSize(iPatch)
		szPatch := n + sz
		n, sz = pTarget.payloadSize(iTarget)
		szTarget := n + sz
		pTarget.blobEdit(iTarget, szTarget, pPatch.span(iPatch, iPatch+szPatch), szPatch)
		if pTarget.oom {
			return jsonMergeOOM
		}
		return jsonMergeOK // Line 03
	}
	x = pTarget.at(iTarget) & 0x0f
	if x != jsonbObject { // Algorithm line 05
		n, sz := pTarget.payloadSize(iTarget)
		pTarget.blobEdit(iTarget+n, sz, nil, 0)
		if pTarget.oom {
			return jsonMergeOOM
		}
		x = pTarget.aBlob[iTarget]
		pTarget.aBlob[iTarget] = (x & 0xf0) | jsonbObject
	}
	n, sz := pPatch.payloadSize(iPatch)
	if n == 0 {
		return jsonMergeBadPatch
	}
	iPCursor := iPatch + n
	iPEnd := iPCursor + sz
	n, sz = pTarget.payloadSize(iTarget)
	if n == 0 {
		return jsonMergeBadTarget
	}
	iTStart := iTarget + n
	iTEndBE := iTStart + sz

	for iPCursor < iPEnd { // Algorithm line 07
		iPLabel := iPCursor
		ePLabel := pPatch.at(iPCursor) & 0x0f
		if ePLabel < jsonbText || ePLabel > jsonbTextRaw {
			return jsonMergeBadPatch
		}
		nPLabel, szPLabel := pPatch.payloadSize(iPCursor)
		if nPLabel == 0 {
			return jsonMergeBadPatch
		}
		iPValue := iPCursor + nPLabel + szPLabel
		if iPValue >= iPEnd {
			return jsonMergeBadPatch
		}
		nPValue, szPValue := pPatch.payloadSize(iPValue)
		if nPValue == 0 {
			return jsonMergeBadPatch
		}
		iPCursor = iPValue + nPValue + szPValue
		if iPCursor > iPEnd {
			return jsonMergeBadPatch
		}

		iTCursor := iTStart
		iTEnd := uint32(int64(iTEndBE) + int64(pTarget.delta))
		var iTLabel, nTLabel, szTLabel, iTValue, nTValue, szTValue uint32
		for iTCursor < iTEnd {
			iTLabel = iTCursor
			eTLabel := pTarget.at(iTCursor) & 0x0f
			if eTLabel < jsonbText || eTLabel > jsonbTextRaw {
				return jsonMergeBadTarget
			}
			nTLabel, szTLabel = pTarget.payloadSize(iTCursor)
			if nTLabel == 0 {
				return jsonMergeBadTarget
			}
			iTValue = iTLabel + nTLabel + szTLabel
			if iTValue >= iTEnd {
				return jsonMergeBadTarget
			}
			nTValue, szTValue = pTarget.payloadSize(iTValue)
			if nTValue == 0 {
				return jsonMergeBadTarget
			}
			if iTValue+nTValue+szTValue > iTEnd {
				return jsonMergeBadTarget
			}
			isEqual := jsonLabelCompare(
				pPatch.span(iPLabel+nPLabel, iPLabel+nPLabel+szPLabel),
				ePLabel == jsonbText || ePLabel == jsonbTextRaw,
				pTarget.span(iTLabel+nTLabel, iTLabel+nTLabel+szTLabel),
				eTLabel == jsonbText || eTLabel == jsonbTextRaw)
			if isEqual {
				break
			}
			iTCursor = iTValue + nTValue + szTValue
		}
		x = pPatch.at(iPValue) & 0x0f
		if iTCursor < iTEnd {
			// A match was found.  Algorithm line 08
			if x == 0 {
				// Patch value is NULL.  Algorithm line 09
				pTarget.blobEdit(iTLabel, nTLabel+szTLabel+nTValue+szTValue, nil, 0)
				if pTarget.oom {
					return jsonMergeOOM
				}
			} else {
				// Algorithm line 12
				savedDelta := pTarget.delta
				pTarget.delta = 0
				if iDepth >= jsonMaxDepth {
					return jsonMergeTooDeep
				}
				if rc := pTarget.mergePatch(iTValue, pPatch, iPValue, iDepth+1); rc != 0 {
					return rc
				}
				pTarget.delta += savedDelta
			}
		} else if x > 0 { // Algorithm line 13
			// No match and patch value is not NULL
			szNew := szPLabel + nPLabel
			if pPatch.at(iPValue)&0x0f != jsonbObject { // Line 14
				pTarget.blobEdit(iTEnd, 0, nil, szPValue+nPValue+szNew)
				if pTarget.oom {
					return jsonMergeOOM
				}
				copy(pTarget.aBlob[iTEnd:], pPatch.span(iPLabel, iPLabel+szNew))
				copy(pTarget.aBlob[iTEnd+szNew:], pPatch.span(iPValue, iPValue+szPValue+nPValue))
			} else {
				pTarget.blobEdit(iTEnd, 0, nil, szNew+1)
				if pTarget.oom {
					return jsonMergeOOM
				}
				copy(pTarget.aBlob[iTEnd:], pPatch.span(iPLabel, iPLabel+szNew))
				pTarget.aBlob[iTEnd+szNew] = 0x00
				savedDelta := pTarget.delta
				pTarget.delta = 0
				if iDepth >= jsonMaxDepth {
					return jsonMergeTooDeep
				}
				if rc := pTarget.mergePatch(iTEnd+szNew, pPatch, iPValue, iDepth+1); rc != 0 {
					return rc
				}
				pTarget.delta += savedDelta
			}
		}
	}
	if pTarget.delta != 0 {
		pTarget.afterEditSizeAdjust(iTarget)
	}
	if pTarget.oom {
		return jsonMergeOOM
	}
	return jsonMergeOK
}
