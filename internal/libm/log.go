// Ported from Arm Optimized Routines, math/log.c, which glibc carries as
// sysdeps/ieee754/dbl-64/e_log.c:
// Copyright (c) 2018-2025, Arm Limited.
// SPDX-License-Identifier: MIT OR Apache-2.0 WITH LLVM-exception

package libm

import "math"

const (
	logTableBits = 7
	logN         = 1 << logTableBits
	logOff       = 0x3fe6000000000000
)

// Log is glibc's log.
func Log(x float64) float64 {
	ix := asuint64(x)
	top := top16(x)

	// Inputs in [1-0x1p-4, 1+0x1.09p-4] take a polynomial of their own.
	const nearLo, nearHi = 0x3fee000000000000, 0x3ff1090000000000
	if ix-nearLo < nearHi-nearLo {
		if ix == 0x3ff0000000000000 {
			return 0
		}
		r := x - 1
		r2 := r * r
		r3 := r * r2
		p := madd(logB[10], r3, madd(logB[9], r2, madd(logB[8], r, logB[7])))
		p = madd(p, r3, madd(logB[6], r2, madd(logB[5], r, logB[4])))
		p = madd(p, r3, madd(logB[3], r2, madd(logB[2], r, logB[1])))
		w := float64(r * 0x1p27)
		rhi := r + w - w
		rlo := r - rhi
		rr := rhi * rhi
		hi := madd(rr, logB[0], r)
		lo := madd(rr, logB[0], r-hi)
		lo = madd(float64(logB[0]*rlo), rhi+r, lo)
		return hi + madd(p, r3, lo)
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
	i := (tmp >> (52 - logTableBits)) % logN
	k := int64(tmp) >> 52
	iz := ix - tmp&(0xfff<<52)
	invc, logc := logTab[i].invc, logTab[i].logc
	z := asdouble(iz)

	// log(x) = log1p(z/c-1) + log(c) + k*Ln2, with r ~= z/c - 1.
	var r float64
	if fused {
		r = math.FMA(z, invc, -1)
	} else {
		r = float64((z - logTab2[i].chi - logTab2[i].clo) * invc)
	}
	kd := float64(k)

	// hi + lo = r + log(c) + k*Ln2.
	w := madd(kd, logLn2hi, logc)
	hi := w + r
	lo := madd(kd, logLn2lo, w-hi+r)

	// log(x) = lo + (log1p(r) - r) + hi.
	r2 := r * r
	y := madd(r*r2, madd(madd(logA[4], r, logA[3]), r2, madd(logA[2], r, logA[1])), madd(logA[0], r2, lo))
	return y + hi
}
