// sqlean's uuid family (src/uuid): random version 4 and time-ordered version
// 7 UUIDs, and conversions between the text and blob forms.
package engine

import (
	"crypto/rand"
	"time"
)

func init() {
	v4 := sqleanScalar{name: "uuid4", volatile: true, fn: func([]Value) (Value, error) {
		return sqleanText(uuidFmt(uuidV4())), nil
	}}
	sqleanFamilies["uuid"] = &sqleanFamily{scalars: []sqleanScalar{
		v4,
		{name: "uuid7", lo: 0, hi: 1, volatile: true, fn: uuid7Func},
		{name: "uuid7_timestamp_ms", lo: 1, hi: 1, fn: func(args []Value) (Value, error) {
			u, ok := uuidArg(args[0])
			if !ok || u[6]>>4 != 7 {
				return sqleanNull, nil
			}
			var ms int64
			for _, b := range u[:6] {
				ms = ms<<8 | int64(b)
			}
			return sqleanInt(ms), nil
		}},
		{name: "uuid_str", lo: 1, hi: 1, fn: func(args []Value) (Value, error) {
			u, ok := uuidArg(args[0])
			if !ok {
				return sqleanNull, nil
			}
			return sqleanText(uuidFmt(u)), nil
		}},
		{name: "uuid_blob", lo: 1, hi: 1, fn: func(args []Value) (Value, error) {
			u, ok := uuidArg(args[0])
			if !ok {
				return sqleanNull, nil
			}
			return sqleanBlob(u[:]), nil
		}},
		sqleanAlias("gen_random_uuid", v4),
	}}
}

// uuidV7MaxMs is UUID_V7_MAX_MS, the largest 48-bit Unix time.
const uuidV7MaxMs = 1<<48 - 1

func uuidRandom() [16]byte {
	var u [16]byte
	rand.Read(u[:])
	return u
}

// uuidSetVersion is set_version: the version nibble and the RFC 9562 variant.
func uuidSetVersion(u *[16]byte, version byte) {
	u[6] = u[6]&0x0f | version<<4
	u[8] = u[8]&0x3f | 0x80
}

func uuidV4() [16]byte {
	u := uuidRandom()
	uuidSetVersion(&u, 4)
	return u
}

// uuidV7 is make_v7: the 48-bit time in ms, rand_a, and random rand_b.
func uuidV7(ms int64, randA uint16) [16]byte {
	u := uuidRandom()
	t := uint64(ms)
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(t>>40), byte(t>>32), byte(t>>24), byte(t>>16), byte(t>>8), byte(t)
	u[6], u[7] = byte(randA>>8), byte(randA)
	uuidSetVersion(&u, 7)
	return u
}

// uuid7Func is fn_v7: the current time with the sub-millisecond fraction in
// rand_a (uuid_v7_time), or the given Unix seconds with a random rand_a.
func uuid7Func(args []Value) (Value, error) {
	if len(args) == 0 {
		now := time.Now()
		sec, nsec := now.Unix(), int64(now.Nanosecond())
		if sec < 0 || sec > uuidV7MaxMs/1000 {
			return sqleanNull, nil
		}
		ms := sec*1000 + nsec/1e6
		if ms > uuidV7MaxMs {
			return sqleanNull, nil
		}
		return sqleanText(uuidFmt(uuidV7(ms, uint16(nsec%1e6*4096/1e6)))), nil
	}
	isInt, sec, _, ok := mathNumericType(args[0])
	if !ok || !isInt || sec < 0 || sec > uuidV7MaxMs/1000 {
		return sqleanNull, nil
	}
	r := uuidRandom()
	return sqleanText(uuidFmt(uuidV7(sec*1000, uint16(r[6])<<8|uint16(r[7])))), nil
}

// uuidFmt is uuid_fmt, the canonical lowercase form.
func uuidFmt(u [16]byte) string {
	const digits = "0123456789abcdef"
	buf := make([]byte, 0, 36)
	for i, b := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf = append(buf, '-')
		}
		buf = append(buf, digits[b>>4], digits[b&0x0f])
	}
	return string(buf)
}

// uuidArg is arg_uuid: a 16-byte blob, or text uuid_parse accepts, a number
// read as its text.
func uuidArg(v Value) ([16]byte, bool) {
	var u [16]byte
	switch v.Typ {
	case Null:
		return u, false
	case Blob:
		if len(v.S) != 16 {
			return u, false
		}
		copy(u[:], v.S)
		return u, true
	}
	return uuidParse(sqleanBytes(v))
}

// uuidParse is uuid_parse: the canonical form, in braces, behind
// "urn:uuid:", or 32 bare hex digits, in any case.
func uuidParse(s []byte) ([16]byte, bool) {
	var u [16]byte
	switch len(s) {
	case 36:
	case 36 + 9:
		for i, c := range []byte("urn:uuid:") {
			b := s[i]
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if b != c {
				return u, false
			}
		}
		s = s[9:]
	case 36 + 2:
		if s[0] != '{' || s[37] != '}' {
			return u, false
		}
		s = s[1:]
	case 32:
		for i := range u {
			b, ok := uuidXtob(s[2*i], s[2*i+1])
			if !ok {
				return u, false
			}
			u[i] = b
		}
		return u, true
	default:
		return u, false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, false
	}
	for i, off := range [16]int{0, 2, 4, 6, 9, 11, 14, 16, 19, 21, 24, 26, 28, 30, 32, 34} {
		b, ok := uuidXtob(s[off], s[off+1])
		if !ok {
			return u, false
		}
		u[i] = b
	}
	return u, true
}

func uuidXtob(x1, x2 byte) (byte, bool) {
	hi, lo := uuidHexValue(x1), uuidHexValue(x2)
	if hi < 0 || lo < 0 {
		return 0, false
	}
	return byte(hi<<4 | lo), true
}

func uuidHexValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
