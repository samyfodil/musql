// Ported from Arm Optimized Routines, math/pow.c and math/pow_common.h, which
// glibc carries as sysdeps/ieee754/dbl-64/e_pow.c:
// Copyright (c) 2018-2026, Arm Limited.
// SPDX-License-Identifier: MIT OR Apache-2.0 WITH LLVM-exception

package libm

import "math"

const (
	powLogTableBits = 7
	powLogN         = 1 << powLogTableBits
	powOff          = 0x3fe6955500000000
)

// Pow is glibc's pow.
func Pow(x, y float64) float64 {
	var sign uint64
	ix, iy := asuint64(x), asuint64(y)
	topx, topy := top12(x), top12(y)
	// x < 0x1p-126 or infinite or NaN, or |y| < 0x1p-65 or |y| >= 0x1p63 or
	// NaN.
	if topx-0x001 >= 0x7ff-0x001 || topy&0x7ff-0x3be >= 0x43e-0x3be {
		if zeroInfNaN(iy) {
			switch {
			case 2*iy == 0:
				return 1
			case ix == asuint64(1):
				return 1
			case 2*ix > 2*asuint64(math.Inf(1)) || 2*iy > 2*asuint64(math.Inf(1)):
				return x + y
			case 2*ix == 2*asuint64(1):
				return 1
			case (2*ix < 2*asuint64(1)) == (iy>>63 == 0):
				// |x| < 1 and y == inf, or |x| > 1 and y == -inf.
				return 0
			}
			return y * y
		}
		if zeroInfNaN(ix) {
			x2 := x * x
			if ix>>63 != 0 && checkInt(iy) == 1 {
				x2 = -x2
				sign = 1
			}
			if 2*ix == 0 && iy>>63 != 0 {
				return oflow(sign)
			}
			if iy>>63 != 0 {
				return 1 / x2
			}
			return x2
		}
		// x and y are finite and non-zero.
		if ix>>63 != 0 {
			yint := checkInt(iy)
			if yint == 0 {
				return math.NaN()
			}
			if yint == 1 {
				sign = signBias
			}
			ix &= 0x7fffffffffffffff
			topx &= 0x7ff
		}
		if topy&0x7ff-0x3be >= 0x43e-0x3be {
			// sign == 0 here: y is not odd.
			if ix == asuint64(1) {
				return 1
			}
			if topy&0x7ff < 0x3be {
				// |y| < 2^-65, x^y ~= 1 + y*log(x).
				if ix > asuint64(1) {
					return 1 + y
				}
				return 1 - y
			}
			if (ix > asuint64(1)) == (topy < 0x800) {
				return oflow(0)
			}
			return uflow(0)
		}
		if topx == 0 {
			// Subnormal x: normalize it so its exponent is negative.
			ix = asuint64(x*0x1p52) & 0x7fffffffffffffff
			ix -= 52 << 52
		}
	}

	hi, lo := powLog(ix)
	var ehi, elo float64
	if fused {
		ehi = float64(y * hi)
		elo = math.FMA(y, lo, math.FMA(y, hi, -ehi))
	} else {
		yhi := asdouble(iy & 0xfffffffff8000000)
		ylo := y - yhi
		lhi := asdouble(asuint64(hi) & 0xfffffffff8000000)
		llo := hi - lhi + lo
		ehi = float64(yhi * lhi)
		elo = float64(ylo*lhi) + float64(y*llo)
	}
	return expInline(ehi, elo, sign)
}

// powLog is log(x) as hi+lo, rounded to hi with lo about 15 bits more. ix is
// x's bits, normalized in the subnormal range with the sign bit as part of
// the exponent.
func powLog(ix uint64) (hi, lo float64) {
	// x = 2^k z, with z in [powOff, 2*powOff) and exact. The range is split
	// into N subintervals, the ith holding z with c near its center.
	tmp := ix - powOff
	i := (tmp >> (52 - powLogTableBits)) % powLogN
	k := int64(tmp) >> 52
	iz := ix - tmp&(0xfff<<52)
	z := asdouble(iz)
	kd := float64(k)

	// log(x) = k*Ln2 + log(c) + log1p(z/c-1). 1/c is j/N or j/N/2 for an
	// integer j in [N,2N), and |z/c - 1| < 1/N, so r = z/c - 1 is exact.
	invc, logc, logctail := powTab[i].invc, powTab[i].logc, powTab[i].logctail
	var r, rhi, rlo float64
	if fused {
		r = math.FMA(z, invc, -1)
	} else {
		// Split z so rhi, rlo and rhi*rhi are exact and |rlo| <= |r|.
		zhi := asdouble((iz + 1<<31) & 0xffffffff00000000)
		zlo := z - zhi
		rhi = float64(zhi*invc) - 1
		rlo = float64(zlo * invc)
		r = rhi + rlo
	}

	// k*Ln2 + log(c) + r.
	t1 := madd(kd, powLn2hi, logc)
	t2 := t1 + r
	lo1 := madd(kd, powLn2lo, logctail)
	lo2 := t1 - t2 + r

	// k*Ln2 + log(c) + r + A[0]*r*r, with A[0] = -0.5.
	ar := float64(powA[0] * r)
	ar2 := float64(r * ar)
	ar3 := r * ar2
	var lo3, lo4 float64
	if fused {
		hi = t2 + ar2
		lo3 = math.FMA(r, ar, -ar2)
		lo4 = t2 - hi + ar2
	} else {
		arhi := float64(powA[0] * rhi)
		arhi2 := float64(rhi * arhi)
		hi = t2 + arhi2
		lo3 = float64(rlo * (ar + arhi))
		lo4 = t2 - hi + arhi2
	}
	// p = log1p(r) - r - A[0]*r*r.
	p := madd(ar2, madd(ar2, madd(r, powA[6], powA[5]), madd(r, powA[4], powA[3])), madd(r, powA[2], powA[1]))
	lo = madd(ar3, p, lo1+lo2+lo3+lo4)
	y := hi + lo
	return y, hi - y + lo
}

// checkInt is 0 if iy is not the bits of an integer, 1 for an odd one and 2
// for an even one; iy is a non-zero finite value.
func checkInt(iy uint64) int {
	e := iy >> 52 & 0x7ff
	switch {
	case e < 0x3ff:
		return 0
	case e > 0x3ff+52:
		return 2
	case iy&(1<<(0x3ff+52-e)-1) != 0:
		return 0
	case iy&(1<<(0x3ff+52-e)) != 0:
		return 1
	}
	return 2
}

// zeroInfNaN is whether i is the bits of a zero, an infinity or a NaN.
func zeroInfNaN(i uint64) bool {
	return 2*i-1 >= 2*asuint64(math.Inf(1))-1
}
