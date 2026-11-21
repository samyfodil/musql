// sqlean's crypto family (src/crypto/extension.c): hash functions returning
// BLOBs, and encode/decode over base32, base64, base85, hex and url. The
// *_agg aggregates are not provided.
package engine

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

func init() {
	hashes := []struct {
		name string
		sum  func([]byte) []byte
	}{
		{"blake3", blake3Sum},
		{"md5", func(b []byte) []byte { s := md5.Sum(b); return s[:] }},
		{"sha1", func(b []byte) []byte { s := sha1.Sum(b); return s[:] }},
		{"sha256", func(b []byte) []byte { s := sha256.Sum256(b); return s[:] }},
		{"sha384", func(b []byte) []byte { s := sha512.Sum384(b); return s[:] }},
		{"sha512", func(b []byte) []byte { s := sha512.Sum512(b); return s[:] }},
		{"xxh32", xxh32Sum},
		{"xxh64", xxh64Sum},
		{"xxh3_64", xxh3_64Sum},
		{"xxh3_128", xxh3_128Sum},
	}
	var scalars []sqleanScalar
	for _, h := range hashes {
		f := sqleanScalar{name: h.name, lo: 1, hi: 1, fn: func(args []Value) (Value, error) {
			if args[0].Typ == Null {
				return sqleanNull, nil
			}
			return sqleanBlob(h.sum(sqleanBytes(args[0]))), nil
		}}
		scalars = append(scalars, f, sqleanAlias("crypto_"+h.name, f))
	}
	enc := sqleanScalar{name: "encode", lo: 2, hi: 2, fn: cryptoEncode}
	dec := sqleanScalar{name: "decode", lo: 2, hi: 2, fn: cryptoDecode}
	scalars = append(scalars, enc, sqleanAlias("crypto_encode", enc), dec, sqleanAlias("crypto_decode", dec))
	sqleanFamilies["crypto"] = &sqleanFamily{scalars: scalars}
}

type cryptoCodec struct {
	encode func([]byte) []byte
	decode func([]byte) ([]byte, bool)
}

var cryptoCodecs = map[string]cryptoCodec{
	"base32": {
		func(b []byte) []byte { return []byte(base32.StdEncoding.EncodeToString(b)) },
		base32Decode,
	},
	"base64": {
		func(b []byte) []byte { return []byte(base64.StdEncoding.EncodeToString(b)) },
		base64Decode,
	},
	"base85": {base85Encode, base85Decode},
	"hex": {
		func(b []byte) []byte { return []byte(hex.EncodeToString(b)) },
		hexDecode,
	},
	"url": {urlEncode, urlDecode},
}

func cryptoArgCodec(v Value) (cryptoCodec, error) {
	if v.Typ == Blob {
		return cryptoCodec{}, fmt.Errorf("encoding name should be text")
	}
	name := string(sqleanBytes(v))
	c, ok := cryptoCodecs[name]
	if !ok {
		return cryptoCodec{}, fmt.Errorf("unknown encoding: %s", name)
	}
	return c, nil
}

func cryptoEncode(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	c, err := cryptoArgCodec(args[1])
	if err != nil {
		return sqleanNull, err
	}
	return sqleanText(string(c.encode(sqleanBytes(args[0])))), nil
}

func cryptoDecode(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	c, err := cryptoArgCodec(args[1])
	if err != nil {
		return sqleanNull, err
	}
	out, ok := c.decode(sqleanBytes(args[0]))
	if !ok {
		return sqleanNull, nil
	}
	if out == nil {
		out = []byte{}
	}
	return sqleanBlob(out), nil
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

func hexDecode(src []byte) ([]byte, bool) {
	if len(src)%2 != 0 {
		return nil, false
	}
	dst := make([]byte, len(src)/2)
	for i := range dst {
		hi, ok1 := hexValue(src[2*i])
		lo, ok2 := hexValue(src[2*i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		dst[i] = hi<<4 | lo
	}
	return dst, true
}

func base32Decode(src []byte) ([]byte, bool) {
	for len(src) > 0 && src[len(src)-1] == '=' {
		src = src[:len(src)-1]
	}
	var dst []byte
	var value uint32
	nbits := 0
	for _, c := range src {
		switch {
		case c >= 'A' && c <= 'Z':
			c -= 'A'
		case c >= '2' && c <= '7':
			c -= '2' - 26
		default:
			return nil, false
		}
		value = (value<<5 | uint32(c)) & 0xFFF
		nbits += 5
		if nbits >= 8 {
			dst = append(dst, byte(value>>(nbits-8)))
			nbits -= 8
		}
	}
	if nbits >= 5 || value&(1<<nbits-1) != 0 {
		return nil, false
	}
	return dst, true
}

func base64Decode(src []byte) ([]byte, bool) {
	if len(src)%4 != 0 {
		return nil, false
	}
	padding := 0
	if len(src) > 0 && src[len(src)-1] == '=' {
		padding++
		if src[len(src)-2] == '=' {
			padding++
		}
	}
	dataLen := len(src) - padding
	var dst []byte
	for i := 0; i < len(src); i += 4 {
		var block uint32
		for k := 0; k < 4; k++ {
			block <<= 6
			if i+k >= dataLen {
				continue
			}
			v := base64Index(src[i+k])
			if v < 0 {
				return nil, false
			}
			block |= uint32(v)
		}
		n := 3
		if i+4 > dataLen {
			n = 3 - padding
		}
		dst = append(dst, byte(block>>16))
		if n > 1 {
			dst = append(dst, byte(block>>8))
		}
		if n > 2 {
			dst = append(dst, byte(block))
		}
	}
	return dst, true
}

func base64Index(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '+':
		return 62
	case c == '/':
		return 63
	}
	return -1
}

func base85Encode(src []byte) []byte {
	var dst []byte
	for i := 0; i < len(src); i += 4 {
		group := min(len(src)-i, 4)
		var tuple uint32
		for j := 0; j < group; j++ {
			tuple |= uint32(src[i+j]) << (24 - 8*j)
		}
		if tuple == 0 && group == 4 {
			dst = append(dst, 'z')
			continue
		}
		var digits [5]byte
		for j := 4; j >= 0; j-- {
			digits[j] = byte(tuple % 85)
			tuple /= 85
		}
		for j := 0; j < group+1; j++ {
			dst = append(dst, digits[j]+33)
		}
	}
	return dst
}

func base85Decode(src []byte) ([]byte, bool) {
	var dst []byte
	for i := 0; i < len(src); {
		if src[i] == 'z' {
			dst = append(dst, 0, 0, 0, 0)
			i++
			continue
		}
		group := min(len(src)-i, 5)
		if group == 1 {
			return nil, false
		}
		var digits [5]uint64
		for k := range digits {
			if k >= group {
				digits[k] = 84
				continue
			}
			d := src[i+k] - 33
			if d > 84 {
				return nil, false
			}
			digits[k] = uint64(d)
		}
		tuple := digits[0]*52200625 + digits[1]*614125 + digits[2]*7225 + digits[3]*85 + digits[4]
		if tuple > 0xFFFFFFFF {
			return nil, false
		}
		tb := [4]byte{byte(tuple >> 24), byte(tuple >> 16), byte(tuple >> 8), byte(tuple)}
		dst = append(dst, tb[:group-1]...)
		i += 5
	}
	return dst, true
}

func urlEncode(src []byte) []byte {
	const hexChars = "0123456789ABCDEF"
	var dst []byte
	for _, c := range src {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			dst = append(dst, c)
		} else {
			dst = append(dst, '%', hexChars[c>>4], hexChars[c&15])
		}
	}
	return dst
}

func urlDecode(src []byte) ([]byte, bool) {
	var dst []byte
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '%':
			if len(src)-i < 3 {
				return nil, false
			}
			hi, ok1 := hexValue(src[i+1])
			lo, ok2 := hexValue(src[i+2])
			if !ok1 || !ok2 {
				return nil, false
			}
			dst = append(dst, hi<<4|lo)
			i += 2
		case '+':
			dst = append(dst, ' ')
		default:
			dst = append(dst, src[i])
		}
	}
	return dst, true
}
