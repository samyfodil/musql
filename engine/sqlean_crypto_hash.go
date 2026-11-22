// Ports of sqlean's blake3.c, xxhash.c and xxh3.c (src/crypto): one-shot
// digests, with xxh results in big-endian canonical form.
package engine

import (
	"encoding/binary"
	"math/bits"
)

var (
	xp32_1 uint32 = 0x9E3779B1
	xp32_2 uint32 = 0x85EBCA77
	xp32_3 uint32 = 0xC2B2AE3D
	xp32_4 uint32 = 0x27D4EB2F
	xp32_5 uint32 = 0x165667B1

	xp64_1 uint64 = 0x9E3779B185EBCA87
	xp64_2 uint64 = 0xC2B2AE3D27D4EB4F
	xp64_3 uint64 = 0x165667B19E3779F9
	xp64_4 uint64 = 0x85EBCA77C2B2AE63
	xp64_5 uint64 = 0x27D4EB2F165667C5
)

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

func xxh32Sum(p []byte) []byte {
	round := func(acc, lane uint32) uint32 {
		return bits.RotateLeft32(acc+lane*xp32_2, 13) * xp32_1
	}
	n := len(p)
	var h uint32
	if n >= 16 {
		v0, v1, v2, v3 := xp32_1+xp32_2, xp32_2, uint32(0), 0-xp32_1
		for ; len(p) >= 16; p = p[16:] {
			v0 = round(v0, le32(p))
			v1 = round(v1, le32(p[4:]))
			v2 = round(v2, le32(p[8:]))
			v3 = round(v3, le32(p[12:]))
		}
		h = bits.RotateLeft32(v0, 1) + bits.RotateLeft32(v1, 7) + bits.RotateLeft32(v2, 12) + bits.RotateLeft32(v3, 18)
	} else {
		h = xp32_5
	}
	h += uint32(n)
	for ; len(p) >= 4; p = p[4:] {
		h += le32(p) * xp32_3
		h = bits.RotateLeft32(h, 17) * xp32_4
	}
	for _, c := range p {
		h += uint32(c) * xp32_5
		h = bits.RotateLeft32(h, 11) * xp32_1
	}
	h ^= h >> 15
	h *= xp32_2
	h ^= h >> 13
	h *= xp32_3
	h ^= h >> 16
	return binary.BigEndian.AppendUint32(nil, h)
}

func xxh64Round(acc, lane uint64) uint64 {
	return bits.RotateLeft64(acc+lane*xp64_2, 31) * xp64_1
}

func xxh64Sum(p []byte) []byte {
	n := len(p)
	var h uint64
	if n >= 32 {
		v := [4]uint64{xp64_1 + xp64_2, xp64_2, 0, 0 - xp64_1}
		for ; len(p) >= 32; p = p[32:] {
			for i := range v {
				v[i] = xxh64Round(v[i], le64(p[8*i:]))
			}
		}
		h = bits.RotateLeft64(v[0], 1) + bits.RotateLeft64(v[1], 7) + bits.RotateLeft64(v[2], 12) + bits.RotateLeft64(v[3], 18)
		for _, x := range v {
			h ^= xxh64Round(0, x)
			h = h*xp64_1 + xp64_4
		}
	} else {
		h = xp64_5
	}
	h += uint64(n)
	for ; len(p) >= 8; p = p[8:] {
		h ^= xxh64Round(0, le64(p))
		h = bits.RotateLeft64(h, 27)*xp64_1 + xp64_4
	}
	if len(p) >= 4 {
		h ^= uint64(le32(p)) * xp64_1
		h = bits.RotateLeft64(h, 23)*xp64_2 + xp64_3
		p = p[4:]
	}
	for _, c := range p {
		h ^= uint64(c) * xp64_5
		h = bits.RotateLeft64(h, 11) * xp64_1
	}
	return binary.BigEndian.AppendUint64(nil, xxh64Avalanche(h))
}

func xxh64Avalanche(h uint64) uint64 {
	h ^= h >> 33
	h *= xp64_2
	h ^= h >> 29
	h *= xp64_3
	h ^= h >> 32
	return h
}

var xxh3Secret = [192]byte{
	0xb8, 0xfe, 0x6c, 0x39, 0x23, 0xa4, 0x4b, 0xbe, 0x7c, 0x01, 0x81, 0x2c, 0xf7, 0x21, 0xad, 0x1c,
	0xde, 0xd4, 0x6d, 0xe9, 0x83, 0x90, 0x97, 0xdb, 0x72, 0x40, 0xa4, 0xa4, 0xb7, 0xb3, 0x67, 0x1f,
	0xcb, 0x79, 0xe6, 0x4e, 0xcc, 0xc0, 0xe5, 0x78, 0x82, 0x5a, 0xd0, 0x7d, 0xcc, 0xff, 0x72, 0x21,
	0xb8, 0x08, 0x46, 0x74, 0xf7, 0x43, 0x24, 0x8e, 0xe0, 0x35, 0x90, 0xe6, 0x81, 0x3a, 0x26, 0x4c,
	0x3c, 0x28, 0x52, 0xbb, 0x91, 0xc3, 0x00, 0xcb, 0x88, 0xd0, 0x65, 0x8b, 0x1b, 0x53, 0x2e, 0xa3,
	0x71, 0x64, 0x48, 0x97, 0xa2, 0x0d, 0xf9, 0x4e, 0x38, 0x19, 0xef, 0x46, 0xa9, 0xde, 0xac, 0xd8,
	0xa8, 0xfa, 0x76, 0x3f, 0xe3, 0x9c, 0x34, 0x3f, 0xf9, 0xdc, 0xbb, 0xc7, 0xc7, 0x0b, 0x4f, 0x1d,
	0x8a, 0x51, 0xe0, 0x4b, 0xcd, 0xb4, 0x59, 0x31, 0xc8, 0x9f, 0x7e, 0xc9, 0xd9, 0x78, 0x73, 0x64,
	0xea, 0xc5, 0xac, 0x83, 0x34, 0xd3, 0xeb, 0xc3, 0xc5, 0x81, 0xa0, 0xff, 0xfa, 0x13, 0x63, 0xeb,
	0x17, 0x0d, 0xdd, 0x51, 0xb7, 0xf0, 0xda, 0x49, 0xd3, 0x16, 0x55, 0x26, 0x29, 0xd4, 0x68, 0x9e,
	0x2b, 0x16, 0xbe, 0x58, 0x7d, 0x47, 0xa1, 0xfc, 0x8f, 0xf8, 0xb8, 0xd1, 0x7a, 0xd0, 0x31, 0xce,
	0x45, 0xcb, 0x3a, 0x8f, 0x95, 0x16, 0x04, 0x28, 0xaf, 0xd7, 0xfb, 0xca, 0xbb, 0x4b, 0x40, 0x7e,
}

type u128 struct{ lo, hi uint64 }

func mul128(x, y uint64) u128 {
	hi, lo := bits.Mul64(x, y)
	return u128{lo, hi}
}

func mulFold64(x, y uint64) uint64 {
	m := mul128(x, y)
	return m.lo ^ m.hi
}

func xxh3Avalanche(h uint64) uint64 {
	h ^= h >> 37
	h *= 0x165667919E3779F9
	h ^= h >> 32
	return h
}

func rrmxmx(h, n uint64) uint64 {
	h ^= bits.RotateLeft64(h, 49) ^ bits.RotateLeft64(h, 24)
	h *= 0x9FB21C651E98DF25
	h ^= (h >> 35) + n
	h *= 0x9FB21C651E98DF25
	h ^= h >> 28
	return h
}

func xxh3Mix16(p, s []byte) uint64 {
	return mulFold64(le64(p)^le64(s), le64(p[8:])^le64(s[8:]))
}

func xxh3Mix32(acc u128, p1, p2, s []byte) u128 {
	acc.lo += xxh3Mix16(p1, s)
	acc.lo ^= le64(p2) + le64(p2[8:])
	acc.hi += xxh3Mix16(p2, s[16:])
	acc.hi ^= le64(p1) + le64(p1[8:])
	return acc
}

func xxh3Hash64Short(p []byte) uint64 {
	s := xxh3Secret[:]
	n := len(p)
	switch {
	case n == 0:
		return xxh64Avalanche(le64(s[56:]) ^ le64(s[64:]))
	case n <= 3:
		combined := uint32(p[0])<<16 | uint32(p[n>>1])<<24 | uint32(p[n-1]) | uint32(n)<<8
		bitflip := uint64(le32(s) ^ le32(s[4:]))
		return xxh64Avalanche(uint64(combined) ^ bitflip)
	case n <= 8:
		input := uint64(le32(p[n-4:])) + uint64(le32(p))<<32
		bitflip := le64(s[8:]) ^ le64(s[16:])
		return rrmxmx(input^bitflip, uint64(n))
	case n <= 16:
		lo := le64(p) ^ (le64(s[24:]) ^ le64(s[32:]))
		hi := le64(p[n-8:]) ^ (le64(s[40:]) ^ le64(s[48:]))
		return xxh3Avalanche(uint64(n) + bits.ReverseBytes64(lo) + hi + mulFold64(lo, hi))
	case n <= 128:
		acc := uint64(n) * xp64_1
		for i := 0; i <= (n-1)/32; i++ {
			acc += xxh3Mix16(p[16*i:], s[32*i:])
			acc += xxh3Mix16(p[n-16*(i+1):], s[32*i+16:])
		}
		return xxh3Avalanche(acc)
	}
	acc := uint64(n) * xp64_1
	for i := 0; i < 8; i++ {
		acc += xxh3Mix16(p[16*i:], s[16*i:])
	}
	acc = xxh3Avalanche(acc)
	for i := 8; i < n/16; i++ {
		acc += xxh3Mix16(p[16*i:], s[16*(i-8)+3:])
	}
	acc += xxh3Mix16(p[n-16:], s[119:])
	return xxh3Avalanche(acc)
}

func xxh3Mid128Final(acc u128, n int) u128 {
	return u128{
		lo: xxh3Avalanche(acc.lo + acc.hi),
		hi: -xxh3Avalanche(acc.lo*xp64_1 + acc.hi*xp64_4 + uint64(n)*xp64_2),
	}
}

func xxh3Hash128Short(p []byte) u128 {
	s := xxh3Secret[:]
	n := len(p)
	switch {
	case n == 0:
		return u128{
			lo: xxh64Avalanche(le64(s[64:]) ^ le64(s[72:])),
			hi: xxh64Avalanche(le64(s[80:]) ^ le64(s[88:])),
		}
	case n <= 3:
		combinedLo := uint32(p[0])<<16 | uint32(p[n>>1])<<24 | uint32(p[n-1]) | uint32(n)<<8
		combinedHi := bits.RotateLeft32(bits.ReverseBytes32(combinedLo), 13)
		bitflipLo := uint64(le32(s) ^ le32(s[4:]))
		bitflipHi := uint64(le32(s[8:]) ^ le32(s[12:]))
		return u128{
			lo: xxh64Avalanche(uint64(combinedLo) ^ bitflipLo),
			hi: xxh64Avalanche(uint64(combinedHi) ^ bitflipHi),
		}
	case n <= 8:
		input := uint64(le32(p)) + uint64(le32(p[n-4:]))<<32
		bitflip := le64(s[16:]) ^ le64(s[24:])
		m := mul128(input^bitflip, xp64_1+uint64(n)<<2)
		m.hi += m.lo << 1
		m.lo ^= m.hi >> 3
		m.lo ^= m.lo >> 35
		m.lo *= 0x9FB21C651E98DF25
		m.lo ^= m.lo >> 28
		m.hi = xxh3Avalanche(m.hi)
		return m
	case n <= 16:
		bitflipLo := le64(s[32:]) ^ le64(s[40:])
		bitflipHi := le64(s[48:]) ^ le64(s[56:])
		inputLo := le64(p)
		inputHi := le64(p[n-8:])
		m := mul128(inputLo^inputHi^bitflipLo, xp64_1)
		m.lo += uint64(n-1) << 54
		inputHi ^= bitflipHi
		m.hi += inputHi + uint64(uint32(inputHi))*uint64(xp32_2-1)
		m.lo ^= bits.ReverseBytes64(m.hi)
		h := mul128(m.lo, xp64_2)
		h.hi += m.hi * xp64_2
		return u128{lo: xxh3Avalanche(h.lo), hi: xxh3Avalanche(h.hi)}
	case n <= 128:
		acc := u128{lo: uint64(n) * xp64_1}
		for i := (n-1)/32 + 1 - 1; i >= 0; i-- {
			acc = xxh3Mix32(acc, p[16*i:], p[n-16*(i+1):], s[32*i:])
		}
		return xxh3Mid128Final(acc, n)
	}
	acc := u128{lo: uint64(n) * xp64_1}
	for i := 0; i < 4; i++ {
		acc = xxh3Mix32(acc, p[32*i:], p[32*i+16:], s[32*i:])
	}
	acc.lo = xxh3Avalanche(acc.lo)
	acc.hi = xxh3Avalanche(acc.hi)
	for i := 4; i < n/32; i++ {
		acc = xxh3Mix32(acc, p[32*i:], p[32*i+16:], s[32*(i-4)+3:])
	}
	acc = xxh3Mix32(acc, p[n-16:], p[n-32:], s[103:])
	return xxh3Mid128Final(acc, n)
}

func xxh3AccumulateStripe(acc *[8]uint64, p, s []byte) {
	for i := 0; i < 8; i++ {
		v := le64(p[8*i:])
		k := v ^ le64(s[8*i:])
		acc[i^1] += v
		acc[i] += uint64(uint32(k)) * (k >> 32)
	}
}

func xxh3Accumulate(acc *[8]uint64, p []byte, stripes int) {
	for i := 0; i < stripes; i++ {
		xxh3AccumulateStripe(acc, p[64*i:], xxh3Secret[8*i:])
	}
}

func xxh3Scramble(acc *[8]uint64) {
	s := xxh3Secret[192-64:]
	for i := range acc {
		acc[i] ^= acc[i] >> 47
		acc[i] ^= le64(s[8*i:])
		acc[i] *= uint64(xp32_1)
	}
}

func xxh3Long(p []byte) [8]uint64 {
	acc := [8]uint64{uint64(xp32_3), xp64_1, xp64_2, xp64_3, xp64_4, uint64(xp32_2), xp64_5, uint64(xp32_1)}
	n := len(p)
	blocks := (n - 1) / 1024
	for i := 0; i < blocks; i++ {
		xxh3Accumulate(&acc, p[1024*i:], 16)
		xxh3Scramble(&acc)
	}
	p = p[1024*blocks:]
	xxh3Accumulate(&acc, p, (len(p)-1)/64)
	xxh3AccumulateStripe(&acc, p[len(p)-64:], xxh3Secret[192-64-7:])
	return acc
}

func xxh3MergeAccs(acc *[8]uint64, s []byte, start uint64) uint64 {
	h := start
	for i := 0; i < 4; i++ {
		h += mulFold64(acc[2*i]^le64(s[16*i:]), acc[2*i+1]^le64(s[16*i+8:]))
	}
	return xxh3Avalanche(h)
}

func xxh3_64Sum(p []byte) []byte {
	if len(p) <= 240 {
		return binary.BigEndian.AppendUint64(nil, xxh3Hash64Short(p))
	}
	acc := xxh3Long(p)
	return binary.BigEndian.AppendUint64(nil, xxh3MergeAccs(&acc, xxh3Secret[11:], uint64(len(p))*xp64_1))
}

func xxh3_128Sum(p []byte) []byte {
	var h u128
	if len(p) <= 240 {
		h = xxh3Hash128Short(p)
	} else {
		acc := xxh3Long(p)
		n := uint64(len(p))
		h.lo = xxh3MergeAccs(&acc, xxh3Secret[11:], n*xp64_1)
		h.hi = xxh3MergeAccs(&acc, xxh3Secret[192-64-11:], ^(n * xp64_2))
	}
	return binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, h.hi), h.lo)
}

var blake3IV = [8]uint32{0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A, 0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19}

var blake3Perm = [16]int{2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8}

const (
	blake3ChunkStart = 1
	blake3ChunkEnd   = 2
	blake3ParentFlag = 4
	blake3Root       = 8
)

func blake3Compress(cv [8]uint32, m [16]uint32, counter uint64, blockLen, flags uint32) [8]uint32 {
	var st [16]uint32
	copy(st[:8], cv[:])
	copy(st[8:12], blake3IV[:4])
	st[12], st[13], st[14], st[15] = uint32(counter), uint32(counter>>32), blockLen, flags
	g := func(a, b, c, d int, mx, my uint32) {
		st[a] = st[a] + st[b] + mx
		st[d] = bits.RotateLeft32(st[d]^st[a], -16)
		st[c] += st[d]
		st[b] = bits.RotateLeft32(st[b]^st[c], -12)
		st[a] = st[a] + st[b] + my
		st[d] = bits.RotateLeft32(st[d]^st[a], -8)
		st[c] += st[d]
		st[b] = bits.RotateLeft32(st[b]^st[c], -7)
	}
	for r := 0; r < 7; r++ {
		g(0, 4, 8, 12, m[0], m[1])
		g(1, 5, 9, 13, m[2], m[3])
		g(2, 6, 10, 14, m[4], m[5])
		g(3, 7, 11, 15, m[6], m[7])
		g(0, 5, 10, 15, m[8], m[9])
		g(1, 6, 11, 12, m[10], m[11])
		g(2, 7, 8, 13, m[12], m[13])
		g(3, 4, 9, 14, m[14], m[15])
		if r < 6 {
			var pm [16]uint32
			for i, j := range blake3Perm {
				pm[i] = m[j]
			}
			m = pm
		}
	}
	var out [8]uint32
	for i := range out {
		out[i] = st[i] ^ st[i+8]
	}
	return out
}

type blake3Output struct {
	cv       [8]uint32
	block    [16]uint32
	counter  uint64
	blockLen uint32
	flags    uint32
}

func (o blake3Output) chainingValue() [8]uint32 {
	return blake3Compress(o.cv, o.block, o.counter, o.blockLen, o.flags)
}

func blake3Words(b []byte) [16]uint32 {
	var w [16]uint32
	for i := 0; i < len(b)/4; i++ {
		w[i] = le32(b[4*i:])
	}
	if r := len(b) % 4; r != 0 {
		var t [4]byte
		copy(t[:], b[len(b)-r:])
		w[len(b)/4] = le32(t[:])
	}
	return w
}

func blake3Parent(l, r [8]uint32) blake3Output {
	o := blake3Output{cv: blake3IV, blockLen: 64, flags: blake3ParentFlag}
	copy(o.block[:8], l[:])
	copy(o.block[8:], r[:])
	return o
}

// blake3Chunk is the output of the chunk at index counter holding p (at most
// 1024 bytes), before its chaining value is taken.
func blake3Chunk(p []byte, counter uint64) blake3Output {
	cv := blake3IV
	flags := uint32(blake3ChunkStart)
	for len(p) > 64 {
		cv = blake3Compress(cv, blake3Words(p[:64]), counter, 64, flags)
		flags = 0
		p = p[64:]
	}
	return blake3Output{cv: cv, block: blake3Words(p), counter: counter, blockLen: uint32(len(p)), flags: flags | blake3ChunkEnd}
}

func blake3Sum(p []byte) []byte {
	var stack [][8]uint32
	chunk := uint64(0)
	for len(p) > 1024 {
		cv := blake3Chunk(p[:1024], chunk).chainingValue()
		p = p[1024:]
		chunk++
		for total := chunk; total&1 == 0; total >>= 1 {
			cv = blake3Parent(stack[len(stack)-1], cv).chainingValue()
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, cv)
	}
	out := blake3Chunk(p, chunk)
	for n := len(stack); n > 0; n-- {
		out = blake3Parent(stack[n-1], out.chainingValue())
	}
	w := blake3Compress(out.cv, out.block, 0, out.blockLen, out.flags|blake3Root)
	b := make([]byte, 0, 32)
	for _, x := range w {
		b = binary.LittleEndian.AppendUint32(b, x)
	}
	return b
}
