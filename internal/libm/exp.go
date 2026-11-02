// Ported from Arm Optimized Routines, math/exp.c, which glibc carries as
// sysdeps/ieee754/dbl-64/e_exp.c:
// Copyright (c) 2018-2025, Arm Limited.
// SPDX-License-Identifier: MIT OR Apache-2.0 WITH LLVM-exception

package libm

import "math"

const (
	expTableBits = 7
	expN         = 1 << expTableBits
	signBias     = 0x800 << expTableBits
)

// Exp is glibc's exp.
func Exp(x float64) float64 {
	switch {
	case math.IsInf(x, -1):
		return 0
	case math.IsInf(x, 1) || x != x:
		return 1 + x
	}
	return expInline(x, 0, 0)
}

// expInline is sign*exp(x+xtail) for a finite x, |xtail| < 2^-8/N and
// |xtail| <= |x|, where sign is 0 or signBias: exp's body, shared with pow.
func expInline(x, xtail float64, sign uint64) float64 {
	abstop := top12(x) & 0x7ff
	// 0x3c9, 0x408 and 0x409 are the top bits of 0x1p-54, 512 and 1024.
	if abstop-0x3c9 >= 0x408-0x3c9 {
		if abstop-0x3c9 >= 0x80000000 {
			one := 1 + x
			if sign != 0 {
				return -one
			}
			return one
		}
		if abstop >= 0x409 {
			if asuint64(x)>>63 != 0 {
				return uflow(sign)
			}
			return oflow(sign)
		}
		abstop = 0
	}

	// exp(x) = 2^(k/N) * exp(r), with exp(r) in [2^(-1/2N),2^(1/2N)], where
	// x = ln2/N*k + r, with int k and r in [-ln2/2N, ln2/2N].
	kd := madd(expInvLn2N, x, expShift)
	ki := asuint64(kd)
	kd -= expShift
	r := madd(expNegLn2loN, kd, madd(expNegLn2hiN, kd, x)) + xtail
	// 2^(k/N) ~= scale * (1 + tail).
	idx := 2 * (ki % expN)
	top := (ki + sign) << (52 - expTableBits)
	tail := asdouble(expTab[idx])
	sbits := expTab[idx+1] + top
	// exp(x) = 2^(k/N) * exp(r) ~= scale + scale * (tail + exp(r) - 1).
	r2 := r * r
	tmp := madd(r2*r2, madd(expC5, r, expC4), madd(madd(expC3, r, expC2), r2, tail+r))
	if abstop == 0 {
		return expSpecial(tmp, sbits, ki)
	}
	scale := asdouble(sbits)
	return madd(scale, tmp, scale)
}

// expSpecial is scale*(1+tmp) where that may overflow or underflow. sbits is
// scale's bits, with an exponent that may have overflowed into the sign, and
// int32(ki) is the k of the argument reduction: positive may overflow,
// negative may underflow.
func expSpecial(tmp float64, sbits, ki uint64) float64 {
	if ki&0x80000000 == 0 {
		// k > 0: the exponent of scale may have overflowed by <= 460.
		sbits -= 1009 << 52
		scale := asdouble(sbits)
		return madd(scale, tmp, scale) * 0x1p1009
	}
	// k < 0: y is rounded to its precision before it is scaled into the
	// subnormal range, to avoid a double rounding.
	sbits += 1022 << 52
	scale := asdouble(sbits)
	st := float64(scale * tmp)
	y := scale + st
	if math.Abs(y) < 1 {
		one := 1.0
		if y < 0 {
			one = -1
		}
		lo := scale - y + st
		hi := one + y
		lo = one - hi + y + lo
		y = hi + lo - one
		if y == 0 {
			y = asdouble(sbits & 0x8000000000000000)
		}
	}
	return 0x1p-1022 * y
}
