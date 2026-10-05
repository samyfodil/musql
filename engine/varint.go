// Package engine implements SQLite's variable-length integer encoding.
package engine

// getVarint decodes a SQLite varint, returning value and bytes consumed (1..9).
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
	// 9th byte uses all 8 bits.
	if len(b) < 9 {
		return 0, 0
	}
	v = (v << 8) | uint64(b[8])
	return v, 9
}

// varintLen returns the number of bytes needed to encode v as a varint.
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
