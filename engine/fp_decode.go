// This file ports util.c's IEEE754-to-decimal conversions (sqlite3FpDecode).
// It is not correctly-rounded; the same arithmetic as C SQLite is needed
// to match its digit output exactly (util.c).
package engine

import (
	"math"
	"math/bits"
)

const (
	// sqliteU64Digits is the max decimal digits a u64 holds.
	sqliteU64Digits = 20
	// powersOf10First/Last are the range of powerOfTen's tables.
	powersOf10First = -348
	powersOf10Last  = 347
)

// sqliteMultiply128 is the full 128-bit product of a and b (util.c:473).
func sqliteMultiply128(a, b uint64) (hi, lo uint64) {
	return bits.Mul64(a, b)
}

// sqliteMultiply160 computes A=(a<<32)+aLo times b, returning high 64 bits
// and the next 32 bits of the 160-bit product (util.c:507).
func sqliteMultiply160(a uint64, aLo uint32, b uint64) (uint64, uint32) {
	rHi, rLo := bits.Mul64(a, b)
	tHi, tLo := bits.Mul64(uint64(aLo), b) // aLo*b < 2^96, so tHi < 2^32
	rLo, carry := bits.Add64(rLo, tHi<<32|tLo>>32, 0)
	return rHi + carry, uint32(rLo >> 32)
}

// powerOfTenBase, powerOfTenScale and powerOfTenScaleLo are powerOfTen's
// aBase[], aScale[] and aScaleLo[] (util.c:586-670), generated there by
// tool/mkfptab.c --round.
var powerOfTenBase = [27]uint64{
	0x8000000000000000, //  0: 1.0e+0 << 63
	0xa000000000000000, //  1: 1.0e+1 << 60
	0xc800000000000000, //  2: 1.0e+2 << 57
	0xfa00000000000000, //  3: 1.0e+3 << 54
	0x9c40000000000000, //  4: 1.0e+4 << 50
	0xc350000000000000, //  5: 1.0e+5 << 47
	0xf424000000000000, //  6: 1.0e+6 << 44
	0x9896800000000000, //  7: 1.0e+7 << 40
	0xbebc200000000000, //  8: 1.0e+8 << 37
	0xee6b280000000000, //  9: 1.0e+9 << 34
	0x9502f90000000000, // 10: 1.0e+10 << 30
	0xba43b74000000000, // 11: 1.0e+11 << 27
	0xe8d4a51000000000, // 12: 1.0e+12 << 24
	0x9184e72a00000000, // 13: 1.0e+13 << 20
	0xb5e620f480000000, // 14: 1.0e+14 << 17
	0xe35fa931a0000000, // 15: 1.0e+15 << 14
	0x8e1bc9bf04000000, // 16: 1.0e+16 << 10
	0xb1a2bc2ec5000000, // 17: 1.0e+17 << 7
	0xde0b6b3a76400000, // 18: 1.0e+18 << 4
	0x8ac7230489e80000, // 19: 1.0e+19 >> 0
	0xad78ebc5ac620000, // 20: 1.0e+20 >> 3
	0xd8d726b7177a8000, // 21: 1.0e+21 >> 6
	0x878678326eac9000, // 22: 1.0e+22 >> 10
	0xa968163f0a57b400, // 23: 1.0e+23 >> 13
	0xd3c21bcecceda100, // 24: 1.0e+24 >> 16
	0x84595161401484a0, // 25: 1.0e+25 >> 20
	0xa56fa5b99019a5c8, // 26: 1.0e+26 >> 23
}

var powerOfTenScale = [26]uint64{
	0x8049a4ac0c5811ae, //  0: 1.0e-351 << 1229
	0xcf42894a5dce35ea, //  1: 1.0e-324 << 1140
	0xa76c582338ed2621, //  2: 1.0e-297 << 1050
	0x873e4f75e2224e68, //  3: 1.0e-270 << 960
	0xda7f5bf590966848, //  4: 1.0e-243 << 871
	0xb080392cc4349dec, //  5: 1.0e-216 << 781
	0x8e938662882af53e, //  6: 1.0e-189 << 691
	0xe65829b3046b0afa, //  7: 1.0e-162 << 602
	0xba121a4650e4ddeb, //  8: 1.0e-135 << 512
	0x964e858c91ba2655, //  9: 1.0e-108 << 422
	0xf2d56790ab41c2a2, // 10: 1.0e-81 << 333
	0xc428d05aa4751e4c, // 11: 1.0e-54 << 243
	0x9e74d1b791e07e48, // 12: 1.0e-27 << 153
	0xcccccccccccccccc, // 13: 1.0e-1 << 67 (special case)
	0xcecb8f27f4200f3a, // 14: 1.0e+27 >> 26
	0xa70c3c40a64e6c51, // 15: 1.0e+54 >> 116
	0x86f0ac99b4e8dafd, // 16: 1.0e+81 >> 206
	0xda01ee641a708de9, // 17: 1.0e+108 >> 295
	0xb01ae745b101e9e4, // 18: 1.0e+135 >> 385
	0x8e41ade9fbebc27d, // 19: 1.0e+162 >> 475
	0xe5d3ef282a242e81, // 20: 1.0e+189 >> 564
	0xb9a74a0637ce2ee1, // 21: 1.0e+216 >> 654
	0x95f83d0a1fb69cd9, // 22: 1.0e+243 >> 744
	0xf24a01a73cf2dccf, // 23: 1.0e+270 >> 833
	0xc3b8358109e84f07, // 24: 1.0e+297 >> 923
	0x9e19db92b4e31ba9, // 25: 1.0e+324 >> 1013
}

var powerOfTenScaleLo = [26]uint32{
	0x205b896d, //  0: 1.0e-351 << 1229
	0x52064cad, //  1: 1.0e-324 << 1140
	0xaf2af2b8, //  2: 1.0e-297 << 1050
	0x5a7744a7, //  3: 1.0e-270 << 960
	0xaf39a475, //  4: 1.0e-243 << 871
	0xbd8d794e, //  5: 1.0e-216 << 781
	0x547eb47b, //  6: 1.0e-189 << 691
	0x0cb4a5a3, //  7: 1.0e-162 << 602
	0x92f34d62, //  8: 1.0e-135 << 512
	0x3a6a07f9, //  9: 1.0e-108 << 422
	0xfae27299, // 10: 1.0e-81 << 333
	0xaa97e14c, // 11: 1.0e-54 << 243
	0x775ea265, // 12: 1.0e-27 << 153
	0xcccccccc, // 13: 1.0e-1 << 67 (special case)
	0x00000000, // 14: 1.0e+27 >> 26
	0x999090b6, // 15: 1.0e+54 >> 116
	0x69a028bb, // 16: 1.0e+81 >> 206
	0xe80e6f48, // 17: 1.0e+108 >> 295
	0x5ec05dd0, // 18: 1.0e+135 >> 385
	0x14588f14, // 19: 1.0e+162 >> 475
	0x8f1668c9, // 20: 1.0e+189 >> 564
	0x6d953e2c, // 21: 1.0e+216 >> 654
	0x4abdaf10, // 22: 1.0e+243 >> 744
	0xbc633b39, // 23: 1.0e+270 >> 833
	0x0a862f81, // 24: 1.0e+297 >> 923
	0x6c07a2c2, // 25: 1.0e+324 >> 1013
}

// sqlitePowerOfTen returns the top 64 bits of 10^p plus the next 32 bits
// (util.c:585), for p in [powersOf10First, powersOf10Last].
func sqlitePowerOfTen(p int) (uint64, uint32) {
	var g, n int
	switch {
	case p < 0:
		if p == -1 {
			return powerOfTenScale[13], powerOfTenScaleLo[13]
		}
		g = p / 27
		n = p % 27
		if n != 0 {
			g--
			n += 27
		}
	case p < 27:
		return powerOfTenBase[p], 0
	default:
		g = p / 27
		n = p % 27
	}
	s := powerOfTenScale[g+13]
	if n == 0 {
		return s, powerOfTenScaleLo[g+13]
	}
	x, lo := sqliteMultiply160(s, powerOfTenScaleLo[g+13], powerOfTenBase[n])
	if x&(1<<63) == 0 {
		x = x<<1 | uint64((lo>>31)&1)
		lo = lo<<1 | 1
	}
	return x, lo
}

// sqlitePwr10to2 and sqlitePwr2to10 are pwr10to2/pwr2to10 (util.c:726-727):
// floor(log2(10^p)) and floor(log10(2^p)) by fixed-point ratio. Go's >> on a
// signed int is arithmetic, the rounding-toward-minus-infinity the C relies
// on for negative p.
func sqlitePwr10to2(p int) int { return (p * 108853) >> 15 }
func sqlitePwr2to10(p int) int { return (p * 78913) >> 18 }

// sqliteFp2Convert10 is sqlite3Fp2Convert10 (util.c:756): given r == m*2^e
// with m's top bit set, it returns d and p with r ~= d*10^p, d holding at
// least n (1..18) significant digits. At n==18 the one extra bit is rounded
// half-up; below that the product is truncated.
func sqliteFp2Convert10(m uint64, e, n int) (uint64, int) {
	p := n - 1 - sqlitePwr2to10(e+63)
	pw, _ := sqlitePowerOfTen(p)
	h, _ := sqliteMultiply128(m, pw)
	if n == 18 {
		h >>= uint(-(e + sqlitePwr10to2(p) + 2))
		return (h + ((h << 1) & 2)) >> 1, -p
	}
	return h >> uint(-(e + sqlitePwr10to2(p) + 1)), -p
}

// sqliteFp10Convert2 is sqlite3Fp10Convert2 (util.c:780): the double nearest
// d*10^p, d != 0, by Russ Cox's fpfmt method against powerOfTen's 96-bit
// table -- the conversion sqlite3AtoF (json_funcs.go's sqliteAtoF) and
// FpDecode's round-trip test both use.
func sqliteFp10Convert2(d uint64, p int) float64 {
	if p < powersOf10First {
		return 0
	}
	if p > powersOf10Last {
		return math.Inf(1)
	}
	b := 64 - bits.LeadingZeros64(d)
	lp := sqlitePwr10to2(p)
	e := 53 - b - lp
	if e > 1074 {
		if e >= 1130 {
			return 0
		}
		e = 1074
	}
	// s is 8 for a normal result and at most 63 for a subnormal one (e
	// clamped to 1074 from below 1130), so every shift below is in range.
	s := uint(-(e - (64 - b) + lp + 3))
	pwr10h, pwr10l := sqlitePowerOfTen(p)
	if pwr10l != 0 {
		pwr10h++
		pwr10l = ^pwr10l
	}
	x := d << uint(64-b)
	hi, lo := sqliteMultiply128(x, pwr10h)
	mid1 := uint32(lo >> 32)
	sticky := uint64(1)
	if hi&(uint64(1)<<s-1) == 0 {
		h2, _ := sqliteMultiply128(x, uint64(pwr10l)<<32)
		mid2 := uint32(h2 >> 32)
		if mid1-mid2 <= 1 { // u32 subtraction, wrapping as in C
			sticky = 0
		}
		if mid1 < mid2 {
			hi--
		}
	}
	u := hi>>s | sticky
	if u >= 1<<55-2 { // adj
		u = u>>1 | u&1
		e--
	}
	m := (u + 1 + (u>>2)&1) >> 2
	if e <= -972 {
		return math.Inf(1)
	}
	if m&(1<<52) != 0 {
		m = m&^(1<<52) | uint64(1075-e)<<52
	}
	return math.Float64frombits(m)
}

// fpDecode is struct FpDecode (sqliteInt.h:4848): the decimal digits
// sqliteFpDecode found, with z holding its n significant digits (not
// NUL-terminated in C; exactly n long here) and iDP the position of the
// decimal point relative to z's first digit.
type fpDecode struct {
	n         int
	iDP       int
	z         []byte
	sign      byte // '+' or '-'
	isSpecial int  // 1: Infinity, 2: NaN
}

// sqliteFpDecode is sqlite3FpDecode (util.c:1385). iRound<=0 rounds to
// -iRound digits right of the decimal point; iRound>0 rounds to
// min(iRound, mxRound) significant digits; mxRound (16, or 20 under printf's
// '!' flag) caps both.
func sqliteFpDecode(r float64, iRound, mxRound int) fpDecode {
	var p fpDecode
	// A NaN fails both tests and so decodes with a '+' sign, as in C.
	switch {
	case r < 0:
		p.sign = '-'
		r = -r
	case r == 0:
		p.sign = '+'
		p.n = 1
		p.iDP = 1
		p.z = []byte{'0'}
		return p
	default:
		p.sign = '+'
	}
	v := math.Float64bits(r)
	e := int(v>>52) & 0x7ff
	if e == 0x7ff {
		p.isSpecial = 1
		if v != 0x7ff0000000000000 {
			p.isSpecial = 2
		}
		return p
	}
	v &= 0x000fffffffffffff
	if e == 0 {
		nn := bits.LeadingZeros64(v)
		v <<= uint(nn)
		e = -1074 - nn
	} else {
		v = v<<11 | 1<<63
		e -= 1086
	}
	nDigits := 18
	if iRound > 0 && iRound < 18 {
		nDigits = iRound + 1
	}
	v, exp := sqliteFp2Convert10(v, e, nDigits)

	// Extract the digits right-aligned in zBuf, as util.c:1435-1454 does two
	// at a time; i is the index of the first one. d holds 18 or 19 digits at
	// nDigits==18 (r < 2*10^k for the k pwr2to10 picks), so i >= 1 always
	// leaves the one slot the iRound==0 carry below may prepend.
	var zBuf [sqliteU64Digits + 1]byte
	i := sqliteU64Digits
	for v > 0 {
		i--
		zBuf[i] = byte(v%10) + '0'
		v /= 10
	}
	n := sqliteU64Digits - i
	if n == 0 {
		// util.c:1434 asserts v>0; decode a zero rather than index out of
		// range should that ever fail.
		p.n = 1
		p.iDP = 1
		p.z = []byte{'0'}
		return p
	}
	p.iDP = n + exp
	if iRound <= 0 {
		iRound = p.iDP - iRound
		if iRound == 0 && zBuf[i] >= '5' {
			iRound = 1
			i--
			zBuf[i] = '0'
			n++
			p.iDP++
		}
	}
	z := zBuf[i:]
	if iRound > 0 && (iRound < n || n > mxRound) {
		if iRound > mxRound {
			iRound = mxRound
		}
		if iRound == 17 {
			// Precision 17 only arises under '!' (mxRound 20), and then n >= 18,
			// so z[13..15] are real digits. Try a shorter rendering that
			// round-trips: 49.47 rather than 49.469999999999999
			// (util.c:1472-1503).
			if z[15] == '9' && z[14] == '9' {
				jj := 14
				for jj > 0 && z[jj-1] == '9' {
					jj--
				}
				var v2 uint64
				if jj == 0 {
					v2 = 1
				} else {
					v2 = uint64(z[0] - '0')
					for kk := 1; kk < jj; kk++ {
						v2 = v2*10 + uint64(z[kk]-'0')
					}
					v2++
				}
				if r == sqliteFp10Convert2(v2, exp+n-jj) {
					iRound = jj + 1
				}
			} else if p.iDP >= n || (z[15] == '0' && z[14] == '0' && z[13] == '0') {
				// z[0] is never '0' here, so the scan stops by jj==1; the
				// jj>0 guard only keeps an impossible case from panicking.
				jj := 13
				for jj > 0 && z[jj-1] == '0' {
					jj--
				}
				if jj > 0 {
					v2 := uint64(z[0] - '0')
					for kk := 1; kk < jj; kk++ {
						v2 = v2*10 + uint64(z[kk]-'0')
					}
					if r == sqliteFp10Convert2(v2, exp+n-jj) {
						iRound = jj + 1
					}
				}
			}
		}
		n = iRound
		if z[iRound] >= '5' {
			j := iRound - 1
			for {
				z[j]++
				if z[j] <= '9' {
					break
				}
				z[j] = '0'
				if j == 0 {
					i--
					z = zBuf[i:]
					z[0] = '1'
					n++
					p.iDP++
					break
				}
				j--
			}
		}
	}
	for n > 1 && z[n-1] == '0' {
		n--
	}
	p.n = n
	p.z = z[:n]
	return p
}
