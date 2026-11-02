// Ported from glibc's sysdeps/ieee754/dbl-64/e_log10.c, s_expm1.c, s_log1p.c,
// e_sinh.c, e_cosh.c and s_tanh.c, which are fdlibm's, the expm1 and log1p
// polynomials as modified by Naohiko Shimizu/Tokai University, Japan
// 1997/08/25, for performance improvement on pipelined processors:
//
// Copyright (C) 1993 by Sun Microsystems, Inc. All rights reserved.
//
// Developed at SunPro, a Sun Microsystems, Inc. business.
// Permission to use, copy, modify, and distribute this
// software is freely granted, provided that this notice
// is preserved.

package libm

import "math"

const (
	ln2Hi  = 0x1.62e42fee00000p-1
	ln2Lo  = 0x1.a39ef35793c76p-33
	invLn2 = 0x1.71547652b82fep+0

	ivln10   = 0x1.bcb7b1526e50ep-2
	log102Hi = 0x1.34413509f6000p-2
	log102Lo = 0x1.9fef311f12b36p-42

	// expm1's coefficients, scaled.
	q1 = -0x1.11111111110f4p-5
	q2 = 0x1.a01a019fe5585p-10
	q3 = -0x1.4ce199eaadbb7p-14
	q4 = 0x1.0cfca86e65239p-18
	q5 = -0x1.afdb76e09c32dp-23

	lp1 = 0x1.5555555555593p-1
	lp2 = 0x1.999999997fa04p-2
	lp3 = 0x1.2492494229359p-2
	lp4 = 0x1.c71c51d8e78afp-3
	lp5 = 0x1.7466496cb03dep-3
	lp6 = 0x1.39a09d078c69fp-3
	lp7 = 0x1.2f112df3e5244p-3
)

func highWord(x float64) int32 { return int32(asuint64(x) >> 32) }

func withHighWord(x float64, hi uint32) float64 {
	return asdouble(uint64(hi)<<32 | asuint64(x)&0xffffffff)
}

// Log10 is glibc's log10: k*log10(2) + log10(x/2^k) with log10 of the
// remainder from Log.
func Log10(x float64) float64 {
	hx := int64(asuint64(x))
	var k int32
	if hx < 0x0010000000000000 {
		// x < 2^-1022.
		if hx&0x7fffffffffffffff == 0 {
			return math.Inf(-1)
		}
		if hx < 0 {
			return math.NaN()
		}
		k -= 54
		x = float64(x * 0x1p54)
		hx = int64(asuint64(x))
	}
	if uint64(hx) >= 0x7ff0000000000000 {
		return x + x
	}
	k += int32(hx>>52) - 1023
	i := int64(uint64(int64(k)) >> 63)
	hx = hx&0x000fffffffffffff | (0x3ff-i)<<52
	y := float64(int64(k) + i)
	z := float64(y*log102Lo) + float64(ivln10*Log(asdouble(uint64(hx))))
	return z + float64(y*log102Hi)
}

// Expm1 is glibc's expm1, which sinh, cosh and tanh build on.
func Expm1(x float64) float64 {
	hx := uint32(asuint64(x) >> 32)
	xsb := hx & 0x80000000
	hx &= 0x7fffffff

	// Huge or non-finite x.
	if hx >= 0x4043687a {
		// |x| >= 56*ln2.
		if hx >= 0x40862e42 {
			// |x| >= 709.78...
			if hx >= 0x7ff00000 {
				switch {
				case x != x:
					return x + x
				case xsb == 0:
					return x
				}
				return -1
			}
			if x > 0x1.62e42fefa39efp+9 {
				return math.Inf(1)
			}
		}
		if xsb != 0 {
			return -1
		}
	}

	// Argument reduction.
	var hi, lo, c float64
	var k int32
	switch {
	case hx > 0x3fd62e42:
		// |x| > 0.5 ln2.
		if hx < 0x3ff0a2b2 {
			// And |x| < 1.5 ln2.
			if xsb == 0 {
				hi, lo, k = x-ln2Hi, ln2Lo, 1
			} else {
				hi, lo, k = x+ln2Hi, -ln2Lo, -1
			}
		} else {
			h := 0.5
			if xsb != 0 {
				h = -0.5
			}
			k = int32(float64(invLn2*x) + h)
			t := float64(k)
			hi = madd(-t, ln2Hi, x)
			lo = float64(t * ln2Lo)
		}
		x = hi - lo
		c = hi - x - lo
	case hx < 0x3c900000:
		// |x| < 2^-54.
		return x
	}

	// x is now in the primary range.
	hfx := 0.5 * x
	hxs := float64(x * hfx)
	h2 := hxs * hxs
	r1 := madd(h2*h2, madd(hxs, q5, q4), madd(h2, madd(hxs, q3, q2), madd(hxs, q1, 1)))
	t := madd(-hfx, r1, 3)
	e := float64(hxs * ((r1 - t) / madd(-x, t, 6)))
	if k == 0 {
		return x - madd(x, e, -hxs)
	}
	e = madd(e-c, x, -c)
	e -= hxs
	switch {
	case k == -1:
		return madd(x-e, 0.5, -0.5)
	case k == 1:
		if x < -0.25 {
			return -2 * (e - (x + 0.5))
		}
		return madd(x-e, 2, 1)
	case k <= -2 || k > 56:
		// exp(x)-1 is close enough.
		y := 1 - (e - x)
		return addExponent(y, k) - 1
	case k < 20:
		t := asdouble(uint64(0x3ff00000-(0x200000>>k)) << 32)
		return addExponent(t-(e-x), k)
	}
	t = asdouble(uint64(0x3ff-k) << 52)
	y := x - (e + t)
	y++
	return addExponent(y, k)
}

// addExponent adds k to y's exponent, as glibc does on its high word.
func addExponent(y float64, k int32) float64 {
	return asdouble(asuint64(y) + uint64(int64(k))<<52)
}

// Log1p is glibc's log1p.
func Log1p(x float64) float64 {
	hx := highWord(x)
	ax := hx & 0x7fffffff
	k := int32(1)
	var f, c float64
	var hu int32
	if hx < 0x3fda827a {
		// x < 0.41422.
		if ax >= 0x3ff00000 {
			// x <= -1.
			if x == -1 {
				return math.Inf(-1)
			}
			return math.NaN()
		}
		if ax < 0x3e200000 {
			// |x| < 2^-29.
			if ax < 0x3c900000 {
				return x
			}
			xx := x * x
			return madd(-xx, 0.5, x)
		}
		if hx > 0 || hx <= -0x402d413d {
			// -0.2929 < x < 0.41422.
			k, f, hu = 0, x, 1
		}
	} else if hx >= 0x7ff00000 {
		return x + x
	}
	if k != 0 {
		var u float64
		if hx < 0x43400000 {
			u = 1 + x
			hu = highWord(u)
			k = hu>>20 - 1023
			// c is a correction term.
			if k > 0 {
				c = 1 - (u - x)
			} else {
				c = x - (u - 1)
			}
			c /= u
		} else {
			u = x
			hu = highWord(u)
			k = hu>>20 - 1023
			c = 0
		}
		hu &= 0x000fffff
		if hu < 0x6a09e {
			// Normalize u.
			u = withHighWord(u, uint32(hu|0x3ff00000))
		} else {
			// Normalize u/2.
			k++
			u = withHighWord(u, uint32(hu|0x3fe00000))
			hu = (0x00100000 - hu) >> 2
		}
		f = u - 1
	}
	hfsq := float64(0.5 * f * f)
	if hu == 0 {
		// |f| < 2^-20.
		kd := float64(k)
		if f == 0 {
			if k == 0 {
				return 0
			}
			return madd(kd, ln2Hi, madd(kd, ln2Lo, c))
		}
		r := float64(hfsq * madd(-f, 0x1.5555555555555p-1, 1))
		if k == 0 {
			return f - r
		}
		return madd(kd, ln2Hi, -(r - madd(kd, ln2Lo, c) - f))
	}
	s := f / (2 + f)
	z := s * s
	z2 := z * z
	z4 := z2 * z2
	r := madd(z, lp1, float64(z2*madd(z, lp3, lp2)))
	r = madd(z4, madd(z, lp5, lp4), r)
	r = madd(z2*z4, madd(z, lp7, lp6), r)
	sr := float64(s * (hfsq + r))
	if k == 0 {
		return f - (hfsq - sr)
	}
	kd := float64(k)
	return madd(kd, ln2Hi, -(hfsq - (madd(kd, ln2Lo, c) + sr) - f))
}

// Sinh is glibc's sinh.
func Sinh(x float64) float64 {
	jx := highWord(x)
	ix := jx & 0x7fffffff
	if ix >= 0x7ff00000 {
		return x + x
	}
	h := 0.5
	if jx < 0 {
		h = -h
	}
	if ix < 0x40360000 {
		// |x| < 22: sign(x)*0.5*(E+E/(E+1)).
		if ix < 0x3e300000 {
			// |x| < 2^-28.
			return x
		}
		t := Expm1(math.Abs(x))
		if ix < 0x3ff00000 {
			return h * madd(t, 2, -(t*t/(t+1)))
		}
		return h * (t + t/(t+1))
	}
	// |x| in [22, log(maxdouble)]: 0.5*exp(|x|).
	if ix < 0x40862e42 {
		return h * Exp(math.Abs(x))
	}
	// |x| in [log(maxdouble), overflow threshold].
	if lx := uint32(asuint64(x)); ix < 0x408633ce || ix == 0x408633ce && lx <= 0x8fb9f87d {
		w := Exp(float64(0.5 * math.Abs(x)))
		t := h * w
		return t * w
	}
	return x * 1e307
}

// Cosh is glibc's cosh. glibc builds it plainly even where it builds exp and
// expm1 with FMA, so only those two vary.
func Cosh(x float64) float64 {
	ix := highWord(x) & 0x7fffffff
	if ix < 0x40360000 {
		// |x| in [0, 0.5*ln2]: 1+expm1(|x|)^2/(2*exp(|x|)).
		if ix < 0x3fd62e43 {
			if ix < 0x3c800000 {
				return 1
			}
			t := Expm1(math.Abs(x))
			w := 1 + t
			return 1 + t*t/(w+w)
		}
		// |x| in [0.5*ln2, 22]: (exp(|x|)+1/exp(|x|))/2.
		t := Exp(math.Abs(x))
		return float64(0.5*t) + 0.5/t
	}
	// |x| in [22, log(maxdouble)]: exp(|x|)/2.
	if ix < 0x40862e42 {
		return 0.5 * Exp(math.Abs(x))
	}
	// |x| in [log(maxdouble), overflow threshold].
	if asuint64(x)&0x7fffffffffffffff <= 0x408633ce8fb9f87d {
		w := Exp(float64(0.5 * math.Abs(x)))
		t := 0.5 * w
		return t * w
	}
	if ix >= 0x7ff00000 {
		return x * x
	}
	return math.Inf(1)
}

// Tanh is glibc's tanh.
func Tanh(x float64) float64 {
	jx := highWord(x)
	ix := jx & 0x7fffffff
	if ix >= 0x7ff00000 {
		if jx >= 0 {
			return 1/x + 1
		}
		return 1/x - 1
	}
	z := 1.0
	if ix < 0x40360000 {
		// |x| < 22.
		switch {
		case ix|int32(asuint64(x)) == 0:
			return x
		case ix < 0x3c800000:
			// |x| < 2^-55.
			return x * (1 + x)
		case ix >= 0x3ff00000:
			t := Expm1(float64(2 * math.Abs(x)))
			z = 1 - 2/(t+2)
		default:
			t := Expm1(float64(-2 * math.Abs(x)))
			z = -t / (t + 2)
		}
	}
	if jx >= 0 {
		return z
	}
	return -z
}
