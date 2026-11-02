// Ported from Arm Optimized Routines, math/log2.c, which glibc carries as
// sysdeps/ieee754/dbl-64/e_log2.c:
// Copyright (c) 2018-2025, Arm Limited.
// SPDX-License-Identifier: MIT OR Apache-2.0 WITH LLVM-exception

package libm

import "math"

const (
	log2TableBits = 6
	log2N         = 1 << log2TableBits
)

// Log2 is glibc's log2.
func Log2(x float64) float64 {
	ix := asuint64(x)
	top := top16(x)

	// Inputs in [1-0x1.5b51p-5, 1+0x1.6ab2p-5] take a polynomial of their own.
	const nearLo, nearHi = 0x3feea4af00000000, 0x3ff0b55900000000
	if ix-nearLo < nearHi-nearLo {
		if ix == 0x3ff0000000000000 {
			return 0
		}
		r := x - 1
		var hi, lo float64
		if fused {
			hi = float64(r * log2InvLn2hi)
			lo = math.FMA(log2InvLn2lo, r, math.FMA(log2InvLn2hi, r, -hi))
		} else {
			rhi := asdouble(asuint64(r) & 0xffffffff00000000)
			rlo := r - rhi
			hi = float64(rhi * log2InvLn2hi)
			lo = float64(rlo*log2InvLn2hi) + float64(r*log2InvLn2lo)
		}
		r2 := r * r
		r4 := r2 * r2
		p := madd(log2B[1], r, log2B[0])
		y := madd(p, r2, hi)
		lo += madd(p, r2, hi-y)
		q := madd(madd(log2B[5], r, log2B[4]), r2, madd(log2B[3], r, log2B[2]))
		q = madd(madd(madd(log2B[9], r, log2B[8]), r2, madd(log2B[7], r, log2B[6])), r4, q)
		return y + madd(q, r4, lo)
	}
	if top-0x0010 >= 0x7ff0-0x0010 {
		// x < 0x1p-1022, or infinite, or NaN.
		switch {
		case ix*2 == 0:
			return math.Inf(-1)
		case ix == 0x7ff0000000000000:
			return x
		case top&0x8000 != 0 || top&0x7ff0 == 0x7ff0:
			return math.NaN()
		}
		// Subnormal: normalize it.
		ix = asuint64(x * 0x1p52)
		ix -= 52 << 52
	}

	// x = 2^k z, with z in [logOff, 2*logOff) and exact. The range is split
	// into N subintervals, the ith holding z with c near its center.
	tmp := ix - logOff
	i := (tmp >> (52 - log2TableBits)) % log2N
	k := int64(tmp) >> 52
	iz := ix - tmp&(0xfff<<52)
	invc, logc := log2Tab[i].invc, log2Tab[i].logc
	z := asdouble(iz)
	kd := float64(k)

	// log2(x) = log2(z/c) + log2(c) + k, with r ~= z/c - 1.
	var r, t1, t2 float64
	if fused {
		r = math.FMA(z, invc, -1)
		t1 = float64(r * log2InvLn2hi)
		t2 = math.FMA(log2InvLn2lo, r, math.FMA(log2InvLn2hi, r, -t1))
	} else {
		r = float64((z - log2Tab2[i].chi - log2Tab2[i].clo) * invc)
		rhi := asdouble(asuint64(r) & 0xffffffff00000000)
		rlo := r - rhi
		t1 = float64(rhi * log2InvLn2hi)
		t2 = float64(rlo*log2InvLn2hi) + float64(r*log2InvLn2lo)
	}

	// hi + lo = r/ln2 + log2(c) + k.
	t3 := kd + logc
	hi := t3 + t1
	lo := t3 - hi + t1 + t2

	// log2(r+1) = r/ln2 + r^2*poly(r).
	r2 := r * r
	r4 := r2 * r2
	p := madd(madd(log2A[5], r, log2A[4]), r4, madd(madd(log2A[3], r, log2A[2]), r2, madd(log2A[1], r, log2A[0])))
	return madd(r2, p, lo) + hi
}
