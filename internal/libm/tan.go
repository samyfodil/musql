// Ported from CORE-MATH, src/binary64/tan/tan.c, for glibc's tan, which is
// IBM's. MIT licensed: see coremath.go.

package libm

import (
	"math"
	"math/bits"
)

// Tan is tan, correctly rounded: glibc's except where glibc is not.
func Tan(x float64) float64 {
	t := asuint64(x)
	if int(t>>52)&0x7ff == 0x7ff {
		return math.NaN()
	}
	if t&0x7fffffffffffffff <= 0x3e4d12ed0af1a27e {
		// |x| <= 0x1.d12ed0af1a27ep-27: tan(x) rounds to x + x^3/3.
		if x == 0 {
			return x
		}
		return math.FMA(x, 0x1p-54, x)
	}
	h, l, err := tanFast(x)
	left, right := h+(l-err), h+(l+err)
	if left == right {
		return left
	}
	return tanAccurate(x)
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// dint is (-1)^sgn * (hi/2^64 + lo/2^128) * 2^ex: tan's accurate path's
// 128-bit floating point.
type dint struct {
	hi, lo uint64
	ex     int64
	sgn    uint64
}

func (a dint) r() u128 { return u128{a.hi, a.lo} }

var (
	dintZero = dint{ex: -1076}
	dintOne  = dint{hi: 1 << 63, ex: 1}
	// dintMagic is 2^-11.
	dintMagic = dint{hi: 1 << 63, ex: -10}
)

// cmpDintAbs is -1, 0 or +1 as |a| is less than, equal to or greater than |b|.
func cmpDintAbs(a, b dint) int {
	switch {
	case a.hi == 0 && b.hi == 0:
		return 0
	case a.hi == 0:
		return -1
	case b.hi == 0:
		return 1
	case a.ex != b.ex:
		if a.ex < b.ex {
			return -1
		}
		return 1
	case a.r().less(b.r()):
		return -1
	case b.r().less(a.r()):
		return 1
	}
	return 0
}

// addDint is a+b to within 2 ulps, exact where Sterbenz's lemma applies.
func addDint(a, b dint) dint {
	if a.hi|a.lo == 0 {
		return b
	}
	switch cmpDintAbs(a, b) {
	case 0:
		if a.sgn^b.sgn != 0 {
			return dintZero
		}
		a.ex++
		return a
	case -1:
		a, b = b, a
	}

	// |a| > |b|, so a.ex >= b.ex.
	A, B := a.r(), b.r()
	k := uint64(a.ex - b.ex)
	if k > 0 {
		if k < 128 {
			B = B.shr(uint(k))
		} else {
			B = u128{}
		}
	}
	var C u128
	ex := a.ex
	if a.sgn^b.sgn != 0 {
		C = A.sub(B)
		var lz uint64
		if C.hi != 0 {
			lz = uint64(bits.LeadingZeros64(C.hi))
		} else {
			lz = 64 + uint64(bits.LeadingZeros64(C.lo))
		}
		if lz > 0 {
			if k == 1 {
				// Sterbenz.
				C = A.shl(uint(lz)).sub(b.r().shl(uint(lz - 1)))
			} else {
				C = A.shl(uint(lz)).sub(B.shl(uint(lz)))
			}
			ex -= int64(lz)
			lz = uint64(bits.LeadingZeros64(C.hi))
		}
		C = C.shl(uint(lz))
		ex -= int64(lz)
	} else {
		C = A.add(B)
		if C.less(A) {
			C = u128{1<<63 | C.hi>>1, C.lo>>1 | C.hi<<63}
			ex++
		}
	}
	return dint{C.hi, C.lo, ex, a.sgn}
}

// mulDint is a*b to within 6 ulps.
func mulDint(a, b dint) dint {
	r := mul64(a.hi, b.hi).add(u128{0, mul64(a.hi, b.lo).hi}).add(u128{0, mul64(a.lo, b.hi).hi})
	ex := r.hi >> 63
	r = r.shl(uint(1 - ex))
	return dint{r.hi, r.lo, a.ex + b.ex + int64(ex) - 1, a.sgn ^ b.sgn}
}

// mulDint21 is a*b taking b's low part as zero, to within 2 ulps.
func mulDint21(a, b dint) dint {
	r := mul64(a.hi, b.hi).add(u128{0, mul64(a.lo, b.hi).hi})
	ex := r.hi >> 63
	r = r.shl(uint(1 - ex))
	return dint{r.hi, r.lo, a.ex + b.ex + int64(ex) - 1, a.sgn ^ b.sgn}
}

// invDint is 1/a for a non-zero a, with relative error < 2^-124.999.
func invDint(a dint) dint {
	h := a.hi
	t := tanTinv[(h>>55)&0xff]
	// Three Newton iterations on a 64-bit inverse of h.
	one := u128{1 << 63, 0}
	e := one.sub(mul64(h, t))
	e = mulLo(u128{0, t}, e.shr(55))
	t += e.shr(72).lo
	e = one.sub(mul64(h, t))
	e = mulLo(u128{0, t}, e.shr(47))
	t += e.shr(80).lo
	e = one.sub(mul64(h, t))
	e = mulLo(u128{0, t}, e.shr(31))
	t += e.shr(96).lo
	// One more in dint: r + r*(1-a*r).
	r := dint{hi: t, ex: 1 - a.ex, sgn: 1}
	q := mulDint21(a, r)
	r.sgn = 0
	q = addDint(dintOne, q)
	q = mulDint(r, q)
	return addDint(r, q)
}

// divDint is b/a for a non-zero a, with relative error < 2^-123.67.
func divDint(b, a dint) dint {
	return mulDint(invDint(a), b)
}

// dintFromD is the dint of a non-zero b.
func dintFromD(b float64) dint {
	u := asuint64(b)
	e := int64(u>>52) & 0x7ff
	m := u & (^uint64(0) >> 12)
	if e != 0 {
		m += 1 << 52
	}
	e -= 0x3fe
	t := uint(bits.LeadingZeros64(m))
	a := dint{hi: m << t, ex: e, sgn: b2u(b < 0)}
	if t > 11 {
		a.ex -= int64(t - 12)
	}
	return a
}

// subnormalizeDint rounds a to nearest in the subnormal range.
func subnormalizeDint(a *dint) {
	if a.ex > -1023 {
		return
	}
	ex := uint(-(1011 + a.ex))
	hi := a.hi >> ex
	md := (a.hi >> (ex - 1)) & 1
	if a.hi&(^uint64(0)>>ex) != 0 || a.lo != 0 {
		hi += md
	} else {
		hi += hi & md
	}
	a.hi = hi << ex
	a.lo = 0
	if a.hi == 0 {
		a.ex++
		a.hi = 1 << 63
	}
}

// dintToD is a rounded to a double.
func dintToD(a dint) float64 {
	subnormalizeDint(&a)
	rd := 0.0
	if (a.hi>>10)&1 != 0 {
		rd += 0x1p-53
	}
	if a.hi&0x3ff != 0 || a.lo != 0 {
		rd += 0x1p-54
	}
	if a.sgn != 0 {
		rd = -rd
	}
	r := asdouble(a.hi>>11|0x3ff<<52|a.sgn<<63) + rd
	var e float64
	switch {
	case a.ex > 1025:
		r, e = math.MaxFloat64, math.MaxFloat64
	case a.ex == 1025:
		r, e = r*0x1p+1, 0x1p+1023
	case a.ex > -1022:
		e = asdouble(uint64((a.ex+1022)&0x7ff) << 52)
	case a.ex == -1074:
		r, e = r*0x1p-1, 0x1p-1074
	case a.ex < -1074:
		r, e = 0x1p-1074, 0x1p-1074
	default:
		e = asdouble(1 << (a.ex + 1073))
	}
	return float64(r * e)
}

// sMul is a*(bh+bl) to within ulp(lo).
func sMul(a, bh, bl float64) (hi, lo float64) {
	hi = float64(a * bh)
	lo = math.FMA(a, bh, -hi)
	return hi, math.FMA(a, bl, lo)
}

// dMul is (ah+al)*(bh+bl) - al*bl.
func dMul(ah, al, bh, bl float64) (hi, lo float64) {
	hi = float64(ah * bh)
	s := math.FMA(ah, bh, -hi)
	t := math.FMA(al, bh, s)
	return hi, math.FMA(ah, bl, t)
}

// tanEvalPSFast is sin2pi(xh+xl) for -2^-24 <= xh+xl < 2^-11+2^-24, with
// relative error < 2^-71.61, where uh+ul approximates (xh+xl)^2.
func tanEvalPSFast(xh, xl, uh, ul float64) (h, l float64) {
	h = -0x1.331554ca19669p6
	h = math.FMA(h, uh, 0x1.466bc678d8d63p6)
	h = math.FMA(h, uh, -0x1.4abbce625be53p5)
	h, l = sMul(h, uh, ul)
	h, t := fastTwoSum(0x1.921fb54442d18p+2, h)
	l += 0x1.1a62645446203p-52 + t
	return dMul(h, l, xh, xl)
}

// tanEvalPCFast is cos2pi(xh+xl) on tanEvalPSFast's terms, with relative
// error < 2^-69.96.
func tanEvalPCFast(uh, ul float64) (h, l float64) {
	h = -0x1.55a5c590f9e6ap6
	h = math.FMA(h, uh, 0x1.03c1f080ad892p6)
	h = math.FMA(h, uh, -0x1.3bd3cc9be45dep4)
	h, l = sMul(h, uh, ul)
	h, t := fastTwoSum(0x1p+0, h)
	l += -0x1.923015cp-77 + t
	return h, l
}

// tanEvalPS is sin2pi(X) for 0 <= X < 2^-11, where X2 approximates X^2.
func tanEvalPS(X, X2 dint) dint {
	Y := mulDint21(X2, tanPS[5])
	for k := 4; k >= 0; k-- {
		Y = addDint(Y, tanPS[k])
		if k > 0 {
			Y = mulDint(Y, X2)
		}
	}
	return mulDint(Y, X)
}

// tanEvalPC is cos2pi(X) for 0 <= X < 2^-11, where X2 approximates X^2.
func tanEvalPC(X2 dint) dint {
	Y := mulDint21(X2, tanPC[5])
	for k := 4; k >= 0; k-- {
		Y = addDint(Y, tanPC[k])
		if k > 0 {
			Y = mulDint(Y, X2)
		}
	}
	return Y
}

// tanNormalize shifts X so X.hi's top bit is set, unless X is zero.
func tanNormalize(X *dint) {
	switch {
	case X.hi != 0:
		if cnt := uint(bits.LeadingZeros64(X.hi)); cnt != 0 {
			X.hi = X.hi<<cnt | X.lo>>(64-cnt)
			X.lo <<= cnt
			X.ex -= int64(cnt)
		}
	case X.lo != 0:
		cnt := uint(bits.LeadingZeros64(X.lo))
		X.hi = X.lo << cnt
		X.lo = 0
		X.ex -= int64(64 + cnt)
	}
}

// tanReduce replaces a normalized X by X/(2*pi) mod 1, with relative error
// < 2^-126.67, normalized.
func tanReduce(X *dint) {
	e := X.ex
	if e <= 1 {
		// |X| < 2: multiply by T[0]/2^64 + T[1]/2^128.
		u := mul64(X.hi, tanT[1])
		tiny := u.lo
		X.lo = u.hi
		u = mul64(X.hi, tanT[0])
		X.lo += u.lo
		X.hi = u.hi + b2u(X.lo < u.lo)
		e = X.ex
		tanNormalize(X)
		if e -= X.ex; e != 0 {
			X.lo |= tiny >> (64 - e)
		}
		return
	}

	// 2 <= e <= 1024: only T[i] with -127 <= e-64i <= 127 contribute.
	i := 0
	if e >= 127 {
		i = int(e-127+64-1) / 64
	}
	var c [5]uint64
	u := mul64(X.hi, tanT[i+3])
	c[0], c[1] = u.lo, u.hi
	u = mul64(X.hi, tanT[i+2])
	c[1] += u.lo
	c[2] = u.hi + b2u(c[1] < u.lo)
	u = mul64(X.hi, tanT[i+1])
	c[2] += u.lo
	c[3] = u.hi + b2u(c[2] < u.lo)
	u = mul64(X.hi, tanT[i])
	c[3] += u.lo
	c[4] = u.hi + b2u(c[3] < u.lo)

	// Shift c left by f bits.
	f := uint(int(e) - 64*i)
	var tiny uint64
	switch {
	case f < 64:
		X.hi = c[4]<<f | c[3]>>(64-f)
		X.lo = c[3]<<f | c[2]>>(64-f)
		tiny = c[2]<<f | c[1]>>(64-f)
	case f == 64:
		X.hi, X.lo, tiny = c[3], c[2], c[1]
	default:
		// 65 <= f <= 127: one more term.
		g := f - 64
		uh := mul64(X.hi, tanT[i+4]).hi
		c[0] += uh
		cy := c[0] < uh
		c[1] += b2u(cy)
		c[2] += b2u(cy && c[1] == 0)
		c[3] += b2u(cy && c[1] == 0 && c[2] == 0)
		c[4] += b2u(cy && c[1] == 0 && c[2] == 0 && c[3] == 0)
		X.hi = c[3]<<g | c[2]>>(64-g)
		X.lo = c[2]<<g | c[1]>>(64-g)
		tiny = c[1]<<g | c[0]>>(64-g)
	}
	X.ex = 0
	tanNormalize(X)
	if X.ex < 0 {
		X.lo |= tiny >> (64 + X.ex)
	}
}

// tanReduce2 splits 0 <= X < 1 exactly into i/2^11 + X with 0 <= X < 2^-11.
func tanReduce2(X *dint) int {
	if X.ex <= -11 {
		return 0
	}
	sh := uint(64 - 11 - X.ex)
	i := int(X.hi >> sh)
	X.hi &= 1<<sh - 1
	tanNormalize(X)
	return i
}

// tanSetDD is c1/2^64 + c0/2^128 as a double-double, rounded toward zero.
func tanSetDD(c1, c0 uint64) (h, l float64) {
	switch {
	case c1 != 0:
		e := uint64(bits.LeadingZeros64(c1))
		if e != 0 {
			c1 = c1<<e | c0>>(64-e)
			c0 <<= e
		}
		f := 0x3fe - e
		h = asdouble(f<<52 | (c1<<1)>>12)
		if c0 = c1<<53 | c0>>11; c0 != 0 {
			g := uint64(bits.LeadingZeros64(c0))
			c0 <<= g
			l = asdouble((f-53-g)<<52 | (c0<<1)>>12)
		}
	case c0 != 0:
		e := uint64(bits.LeadingZeros64(c0))
		f := 0x3fe - 64 - e
		c0 <<= e + 1
		h = asdouble(f<<52 | c0>>12)
		if c0 <<= 52; c0 != 0 {
			g := uint64(bits.LeadingZeros64(c0))
			c0 <<= g + 1
			l = asdouble((f-64-g)<<52 | c0>>12)
		}
	}
	return h, l
}

// tanReduceFast is i and h+l with i/2^11 + h + l approximating
// frac(x/(2*pi)) to within 2^-104.815, |h| < 2^-11 and |l| < 2^-52.36, for
// 0x1.7137449123ef6p-26 < x < +Inf.
func tanReduceFast(x float64) (int, float64, float64) {
	var h, l float64
	if x <= 0x1.921fb54442d17p+2 {
		// x < 2*pi: |CH+CL - 1/(2pi)| < 2^-110.523.
		const ch, cl = 0x1.45f306dc9c883p-3, -0x1.6b01ec5417056p-57
		h = float64(ch * x)
		l = math.FMA(ch, x, -h)
		l = math.FMA(cl, x, l)
	} else {
		t := asuint64(x)
		e := int(t>>52) & 0x7ff
		m := 1<<52 | t&0xfffffffffffff
		var c [3]uint64
		if e <= 1074 {
			// 2^2 <= x < 2^52.
			v := mul64(m, tanT[2])
			u := mul64(m, tanT[1])
			c[0] = u.lo + v.hi
			c[1] = u.hi + b2u(c[0] < u.lo)
			u = mul64(m, tanT[0])
			c[1] += u.lo
			c[2] = u.hi + b2u(c[1] < u.lo)
			e = 1075 - e
		} else {
			// 2^52 <= x < 2^1024.
			i := (e - 1138 + 63) / 64
			v := mul64(m, tanT[i+3])
			u := mul64(m, tanT[i+2])
			c[0] = u.lo + v.hi
			c[1] = u.hi + b2u(c[0] < u.lo)
			u = mul64(m, tanT[i+1])
			c[1] += u.lo
			c[2] = u.hi + b2u(c[1] < u.lo)
			u = mul64(m, tanT[i])
			c[2] += u.lo
			e = 1139 + i<<6 - e
		}
		// e is the number of bits of c[2] to ignore.
		if e == 64 {
			c[0], c[1] = c[1], c[2]
		} else {
			c[0] = c[1]<<(64-e) | c[0]>>e
			c[1] = c[2]<<(64-e) | c[1]>>e
		}
		h, l = tanSetDD(c[1], c[0])
	}
	i := math.Floor(h * 0x1p11)
	h = math.FMA(i, -0x1p-11, h)
	return int(i), h, l
}

// tanFastDiv is (bh+bl)/(ah+al) with relative error < 2^-96.99, for
// |bl|, |al| < 2^-49.47 (Karp-Markstein).
func tanFastDiv(bh, bl, ah, al float64) (h, l float64) {
	y := 1.0 / ah
	h = float64(bh * y)
	eh := math.FMA(ah, -h, bh)
	el := math.FMA(al, -h, bl)
	return h, float64(y * (eh + el))
}

// tanFast is tan(x) as h+l and err with |h + l - tan(x)| < err, for a finite
// |x| > 0x1.d12ed0af1a27ep-27.
func tanFast(x float64) (h, l, err float64) {
	neg, isTan := 0, 1
	absx := x
	if x < 0 {
		neg, absx = 1, -x
	}
	i, h, l := tanReduceFast(absx)
	// pi <= x <= 2pi: tan(pi+x) = tan(x).
	i &= 0x3ff
	// pi/2 <= x <= pi: tan(pi/2+x) = -cot(x).
	isTan ^= i >> 9
	neg ^= i >> 9
	i &= 0x1ff
	if i&0x100 != 0 {
		// pi/4 <= x <= pi/2: tan(pi/2-x) = cot(x); 0x1p-11 - h is exact.
		isTan ^= 1
		i = 0x1ff - i
		h = 0x1p-11 - h
		l = -l
	}
	if i == 0 && h < 0x1p-37 {
		return h, l, 1
	}
	// SC[i] is at i/2^11 + SC[i][0]; h - SC[i][0] is exact.
	h -= tanSC[i][0]
	h, l = fastTwoSum(h, l)
	uh := float64(h * h)
	ul := math.FMA(h, h, -uh)
	ul = math.FMA(h+h, l, ul)
	sh, sl := tanEvalPSFast(h, l, uh, ul)
	ch, cl := tanEvalPCFast(uh, ul)
	sh0, sl0 := sMul(tanSC[i][2], sh, sl)
	ch0, cl0 := sMul(tanSC[i][1], ch, cl)
	h1, l1 := fastTwoSum(ch0, sh0)
	l1 += sl0 + cl0
	ch, cl = sMul(tanSC[i][2], ch, cl)
	sh, sl = sMul(tanSC[i][1], sh, sl)
	h2, l2 := fastTwoSum(ch, -sh)
	l2 += cl - sl
	if isTan != 0 {
		h, l = tanFastDiv(h1, l1, h2, l2)
	} else {
		h, l = tanFastDiv(h2, l2, h1, l1)
	}
	if neg != 0 {
		h, l = -h, -l
	}
	return h, l, float64(h * 0x1.1ap-66)
}

// tanAccurate is tan(x) for a finite |x| > 0x1.d12ed0af1a27ep-27, the path
// taken when the fast one cannot settle the rounding.
func tanAccurate(x float64) float64 {
	absx := x
	if !(x > 0) {
		absx = -x
	}
	X := dintFromD(absx)
	tanReduce(&X)
	isTan, neg := 1, b2u(x < 0)
	i := tanReduce2(&X) & 0x3ff
	if i&0x200 != 0 {
		// pi/2 <= x < pi: tan(x) = -1/tan(x-pi/2).
		isTan = 0
		neg ^= 1
		i &= 0x1ff
	}
	if i&0x100 != 0 {
		// pi/4 <= x < pi/2: tan(x) = 1/tan(pi/2-x).
		isTan ^= 1
		X.sgn = 1
		X = addDint(dintMagic, X)
		i = 0x1ff - i
	}
	X2 := mulDint(X, X)
	U := tanEvalPC(X2)
	V := tanEvalPS(X, X2)
	sin := addDint(mulDint(tanS[i], U), mulDint(tanC[i], V))
	U = mulDint(tanC[i], U)
	V = mulDint(tanS[i], V)
	V.sgn = 1 - V.sgn
	cos := addDint(U, V)
	if isTan != 0 {
		U = divDint(sin, cos)
	} else {
		U = divDint(cos, sin)
	}
	// The relative error is less than 86 ulps.
	const err = 86
	lo0 := U.lo - err
	hi0 := U.hi - b2u(lo0 > U.lo)
	lo1 := U.lo + err
	hi1 := U.hi + b2u(lo1 < U.lo)
	if hi0>>10 != hi1>>10 {
		for _, e := range [...][3]float64{
			{0x1.dffffffffff1fp-22, 0x1.e000000000151p-22, 0x1.fffffffffffffp-76},
			{0x1.dfffffffffc7cp-21, 0x1.e000000000546p-21, -0x1.658bcedb6e1d4p-147},
		} {
			if math.Abs(x) == e[0] {
				if x > 0 {
					return e[1] + e[2]
				}
				return -e[1] - e[2]
			}
		}
	}
	if neg != 0 {
		U.sgn = 1 - U.sgn
	}
	return dintToD(U)
}

var tanTinv = [256]uint64{0xff00ff00ff00ff02, 0xfe03f80fe03f80ff, 0xfd08e5500fd08e56, 0xfc0fc0fc0fc0fc11, 0xfb18856506ddaba7, 0xfa232cf252138ac1, 0xf92fb2211855a867, 0xf83e0f83e0f83e11, 0xf74e3fc22c700f76, 0xf6603d980f6603db, 0xf57403d5d00f5742, 0xf4898d5f85bb3952, 0xf3a0d52cba872338, 0xf2b9d6480f2b9d66, 0xf1d48bcee0d399fc, 0xf0f0f0f0f0f0f0f2, 0xf00f00f00f00f010, 0xef2eb71fc4345239, 0xee500ee500ee5010, 0xed7303b5cc0ed731, 0xec979118f3fc4da3, 0xebbdb2a5c1619c8d, 0xeae56403ab959010, 0xea0ea0ea0ea0ea10, 0xe939651fe2d8d35d, 0xe865ac7b7603a198, 0xe79372e225fe30da, 0xe6c2b4481cd8568a, 0xe5f36cb00e5f36cc, 0xe525982af70c880f, 0xe45932d7dc52100f, 0xe38e38e38e38e38f, 0xe2c4a6886a4c2e11, 0xe1fc780e1fc780e3, 0xe135a9c97500e137, 0xe070381c0e070383, 0xdfac1f74346c5760, 0xdee95c4ca037ba58, 0xde27eb2c41f3d9d2, 0xdd67c8a60dd67c8b, 0xdca8f158c7f91ab9, 0xdbeb61eed19c5959, 0xdb2f171df770291a, 0xda740da740da740f, 0xd9ba4256c0366e92, 0xd901b2036406c80f, 0xd84a598ec9151f44, 0xd79435e50d79435f, 0xd6df43fca482f00e, 0xd62b80d62b80d62c, 0xd578e97c3f5fe552, 0xd4c77b03531dec0e, 0xd4173289870ac52e, 0xd3680d3680d3680e, 0xd2ba083b445250ac, 0xd20d20d20d20d20e, 0xd161543e28e50275, 0xd0b69fcbd2580d0c, 0xd00d00d00d00d00e, 0xcf6474a8819ec8ea, 0xcebcf8bb5b4169cc, 0xce168a7725080ce2, 0xcd712752a886d243, 0xccccccccccccccce, 0xcc29786c7607f99f, 0xcb8727c065c393e1, 0xcae5d85f1bbd6c96, 0xca4587e6b74f032a, 0xc9a633fcd967300d, 0xc907da4e871146ad, 0xc86a78900c86a78a, 0xc7ce0c7ce0c7ce0d, 0xc73293d789b9f839, 0xc6980c6980c6980d, 0xc5fe740317f9d00d, 0xc565c87b5f9d4d1c, 0xc4ce07b00c4ce07c, 0xc4372f855d824ca6, 0xc3a13de60495c774, 0xc30c30c30c30c30d, 0xc2780613c0309e02, 0xc1e4bbd595f6e948, 0xc152500c152500c2, 0xc0c0c0c0c0c0c0c1, 0xc0300c0300c0300d, 0xbfa02fe80bfa02ff, 0xbf112a8ad278e8de, 0xbe82fa0be82fa0bf, 0xbdf59c91700bdf5a, 0xbd69104707661aa3, 0xbcdd535db1cc5b7c, 0xbc52640bc52640bd, 0xbbc8408cd63069a1, 0xbb3ee721a54d880c, 0xbab656100bab6562, 0xba2e8ba2e8ba2e8c, 0xb9a7862a0ff46588, 0xb92143fa36f5e02f, 0xb89bc36ce3e0453b, 0xb81702e05c0b8171, 0xb79300b79300b794, 0xb70fbb5a19be3659, 0xb68d31340e4307d9, 0xb60b60b60b60b60c, 0xb58a485518d1e7e4, 0xb509e68a9b948220, 0xb48a39d44685fe97, 0xb40b40b40b40b40c, 0xb38cf9b00b38cf9b, 0xb30f63528917c80c, 0xb2927c29da5519d0, 0xb21642c8590b2165, 0xb19ab5c45606f00c, 0xb11fd3b80b11fd3c, 0xb0a59b418d749d54, 0xb02c0b02c0b02c0b, 0xafb321a1496fdf0f, 0xaf3addc680af3ade, 0xaec33e1f671529a5, 0xae4c415c9882b931, 0xadd5e6323fd48a87, 0xad602b580ad602b6, 0xaceb0f891e6551bc, 0xac7691840ac76919, 0xac02b00ac02b00ac, 0xab8f69e28359cd12, 0xab1cbdd3e2970f60, 0xaaaaaaaaaaaaaaab, 0xaa392f35dc17f00b, 0xa9c84a47a07f5638, 0xa957fab5402a55ff, 0xa8e83f5717c0a8e9, 0xa87917088e262b70, 0xa80a80a80a80a80b, 0xa79c7b16ea64d422, 0xa72f05397829cbc2, 0xa6c21df6e1625c80, 0xa655c4392d7b73a8, 0xa5e9f6ed347f0721, 0xa57eb50295fad40b, 0xa513fd6bb00a5140, 0xa4a9cf1d96833751, 0xa44029100a440291, 0xa3d70a3d70a3d70b, 0xa36e71a2cb033129, 0xa3065e3fae7cd0e0, 0xa29ecf163bb6500a, 0xa237c32b16cfd772, 0xa1d139855f7268ee, 0xa16b312ea8fc377d, 0xa105a932f2ca891f, 0xa0a0a0a0a0a0a0a1, 0xa03c1688732b3032, 0x9fd809fd809fd80a, 0x9f747a152d7836d0, 0x9f1165e7254813e2, 0x9eaecc8d53ae2ddf, 0x9e4cad23dd5f3a20, 0x9deb06c9194aa416, 0x9d89d89d89d89d8a, 0x9d2921c3d6411308, 0x9cc8e160c3fb19b9, 0x9c69169b30446dfa, 0x9c09c09c09c09c0a, 0x9baade8e4a2f6e10, 0x9b4c6f9ef03a3caa, 0x9aee72fcf957c10f, 0x9a90e7d95bc609a9, 0x9a33cd67009a33ce, 0x99d722dabde58f06, 0x997ae76b50efd00a, 0x991f1a515885fb37, 0x98c3bac74f5db00a, 0x9868c809868c8099, 0x980e4156201301c8, 0x97b425ed097b425f, 0x975a750ff68a58af, 0x97012e025c04b80a, 0x96a850096a850097, 0x964fda6c0964fda7, 0x95f7cc72d1b887e9, 0x95a02568095a0257, 0x9548e4979e0829fd, 0x94f2094f2094f209, 0x949b92ddc02526e5, 0x9445809445809446, 0x93efd1c50e726b7c, 0x939a85c40939a85c, 0x93459be6b009345a, 0x92f113840497889c, 0x929cebf48bbd90e5, 0x9249249249249249, 0x91f5bcb8bb02d9cd, 0x91a2b3c4d5e6f809, 0x9150091500915009, 0x90fdbc090fdbc091, 0x90abcc0242af3009, 0x905a38633e06c43b, 0x9009009009009009, 0x8fb823ee08fb823f, 0x8f67a1e3fdc26179, 0x8f1779d9fdc3a219, 0x8ec7ab397255e41d, 0x8e78356d1408e783, 0x8e2917e0e702c6ce, 0x8dda520237694809, 0x8d8be33f95d71590, 0x8d3dcb08d3dcb08d, 0x8cf008cf008cf009, 0x8ca29c046514e023, 0x8c55841c815ed5ca, 0x8c08c08c08c08c09, 0x8bbc50c8deb420c0, 0x8b70344a139bc75b, 0x8b246a87e19008b2, 0x8ad8f2fba9386823, 0x8a8dcd1feeae465c, 0x8a42f8705669db46, 0x89f87469a23920e0, 0x89ae4089ae4089ae, 0x89645c4f6e055dec, 0x891ac73ae9819b50, 0x88d180cd3a4133d7, 0x8888888888888889, 0x883fddf00883fddf, 0x87f78087f78087f8, 0x87af6fd5992d0d40, 0x8767ab5f34e47ef1, 0x872032ac13008720, 0x86d905447a34acc6, 0x869222b1acf1ce96, 0x864b8a7de6d1d608, 0x86053c345a0b8473, 0x85bf37612cee3c9b, 0x85797b917765ab89, 0x8534085340853408, 0x84eedd357c1b0085, 0x84a9f9c8084a9f9d, 0x84655d9bab2f1008, 0x8421084210842108, 0x83dcf94dc7570ce1, 0x839930523fbe3368, 0x8355ace3c897db10, 0x83126e978d4fdf3b, 0x82cf750393ac3319, 0x828cbfbeb9a020a3, 0x824a4e60b3262bc5, 0x8208208208208208, 0x81c635bc123fdf8e, 0x81848da8faf0d277, 0x814327e3b94f462f, 0x8102040810204081, 0x80c121b28bd1ba98, 0x8080808080808081, 0x8040201008040201, 0x8000000000000000}

var tanT = [20]uint64{
	0x28be60db9391054a,
	0x7f09d5f47d4d3770,
	0x36d8a5664f10e410,
	0x7f9458eaf7aef158,
	0x6dc91b8e909374b8,
	0x1924bba82746487,
	0x3f877ac72c4a69cf,
	0xba208d7d4baed121,
	0x3a671c09ad17df90,
	0x4e64758e60d4ce7d,
	0x272117e2ef7e4a0e,
	0xc7fe25fff7816603,
	0xfbcbc462d6829b47,
	0xdb4d9fb3c9f2c26d,
	0xd3d18fd9a797fa8b,
	0x5d49eeb1faf97c5e,
	0xcf41ce7de294a4ba,
	0x9afed7ec47e35742,
	0x1580cc11bf1edaea,
	0xfc33ef0826bd0d87,
}

var tanS = [256]dint{
	{hi: 0x0, lo: 0x0, ex: 128, sgn: 0},
	{hi: 0xc90fc5f66525d257, lo: 0x480f7956b6470765, ex: -8, sgn: 0},
	{hi: 0xc90f87f3380388d5, lo: 0xcb3ff35bd4d81baa, ex: -7, sgn: 0},
	{hi: 0x96cb587284b81770, lo: 0xb767005691b9d9d1, ex: -6, sgn: 0},
	{hi: 0xc90e8fe6f63c2330, lo: 0xf1d7d06db39ea9fc, ex: -6, sgn: 0},
	{hi: 0xfb514b55ccbe541a, lo: 0xd784e031f9af76d6, ex: -6, sgn: 0},
	{hi: 0x96c9b5df1877e9b5, lo: 0xf91ee371d6467dca, ex: -5, sgn: 0},
	{hi: 0xafea690fd5912ef3, lo: 0xf56e3c87ae3c56df, ex: -5, sgn: 0},
	{hi: 0xc90aafbd1b33efc9, lo: 0xc539edcbfda0cf2c, ex: -5, sgn: 0},
	{hi: 0xe22a7a6729d8e453, lo: 0x850021e392744a4f, ex: -5, sgn: 0},
	{hi: 0xfb49b98e8e7807f6, lo: 0xb21ccebc9caac3, ex: -5, sgn: 0},
	{hi: 0x8a342eda160bf5ae, lo: 0xde5b1068d174be9c, ex: -4, sgn: 0},
	{hi: 0x96c32baca2ae68b4, lo: 0x37b2dd49d5fca3c0, ex: -4, sgn: 0},
	{hi: 0xa351cb7fc30bc889, lo: 0xb56007d16d4ad5a3, ex: -4, sgn: 0},
	{hi: 0xafe00694866a1b44, lo: 0xcd34d2751c2e1da7, ex: -4, sgn: 0},
	{hi: 0xbc6dd52c3a342eb5, lo: 0xf10bfca3d6464012, ex: -4, sgn: 0},
	{hi: 0xc8fb2f886ec09f37, lo: 0x6a17954b2b7c5171, ex: -4, sgn: 0},
	{hi: 0xd5880deafc18b534, lo: 0x73d1472472f4a390, ex: -4, sgn: 0},
	{hi: 0xe214689606bf1676, lo: 0x438b4a73aecd2541, ex: -4, sgn: 0},
	{hi: 0xeea037cc04764844, lo: 0xc4e92d01a2f42935, ex: -4, sgn: 0},
	{hi: 0xfb2b73cfc106ff68, lo: 0xf0a0e36a000c7350, ex: -4, sgn: 0},
	{hi: 0x83db0a7231831d8f, lo: 0x60e782313f6161af, ex: -3, sgn: 0},
	{hi: 0x8a2009a6b84d9402, lo: 0x77724a2b2a669bc4, ex: -3, sgn: 0},
	{hi: 0x9064b3a76a22640c, lo: 0x56e0a8b0d177b55d, ex: -3, sgn: 0},
	{hi: 0x96a9049670cfae65, lo: 0xf77574094d3c35c4, ex: -3, sgn: 0},
	{hi: 0x9cecf8962d14c822, lo: 0x50ffe4f5caa7f1fa, ex: -3, sgn: 0},
	{hi: 0xa3308bc93904ad69, lo: 0xdec1b7f2768bdafa, ex: -3, sgn: 0},
	{hi: 0xa973ba526a6850d9, lo: 0x76f8c63986598c79, ex: -3, sgn: 0},
	{hi: 0xafb68054d520c60b, lo: 0xfdd2fc0936594c2d, ex: -3, sgn: 0},
	{hi: 0xb5f8d9f3cd8945d6, lo: 0x924bef13600f9852, ex: -3, sgn: 0},
	{hi: 0xbc3ac352ead90abe, lo: 0xeb13e106732687f1, ex: -3, sgn: 0},
	{hi: 0xc27c389609850433, lo: 0xb228a03916371f6f, ex: -3, sgn: 0},
	{hi: 0xc8bd35e14da15f0e, lo: 0xc7396c894bbf7389, ex: -3, sgn: 0},
	{hi: 0xcefdb7592542e1e9, lo: 0x6b47b8c44e5b037e, ex: -3, sgn: 0},
	{hi: 0xd53db9224ae01bca, lo: 0x7337412cf70716cb, ex: -3, sgn: 0},
	{hi: 0xdb7d3761c7b263b6, lo: 0xbb286d23e11c8337, ex: -3, sgn: 0},
	{hi: 0xe1bc2e3cf616a7ac, lo: 0x31883b30137c6e62, ex: -3, sgn: 0},
	{hi: 0xe7fa99d983ee098f, lo: 0xeeb8f9c33340a2f2, ex: -3, sgn: 0},
	{hi: 0xee38765d74fe4897, lo: 0xed16b994af6c18ae, ex: -3, sgn: 0},
	{hi: 0xf475bfef2551f5b9, lo: 0x14e1a5488eaeab96, ex: -3, sgn: 0},
	{hi: 0xfab272b54b9871a2, lo: 0x704729ae56d78a37, ex: -3, sgn: 0},
	{hi: 0x8077456b7dc2d967, lo: 0x3eac8308f1113e5e, ex: -2, sgn: 0},
	{hi: 0x8395023dd418e919, lo: 0xdb1f70118c9c2198, ex: -2, sgn: 0},
	{hi: 0x86b26de5933c2e8e, lo: 0xc5a9decdfaad4db5, ex: -2, sgn: 0},
	{hi: 0x89cf8676d7abb55b, lo: 0x97965c9860c34e44, ex: -2, sgn: 0},
	{hi: 0x8cec4a05f12739e8, lo: 0xdcdca90cc73b116a, ex: -2, sgn: 0},
	{hi: 0x9008b6a763de75b7, lo: 0xa6e3df5975cca9da, ex: -2, sgn: 0},
	{hi: 0x9324ca6fe9a04b4e, lo: 0x899c4de737feec22, ex: -2, sgn: 0},
	{hi: 0x964083747309d113, lo: 0xa89a11e07c1fe, ex: -2, sgn: 0},
	{hi: 0x995bdfca28b53a54, lo: 0x49c4863de522b217, ex: -2, sgn: 0},
	{hi: 0x9c76dd866c689dcc, lo: 0xe7bc08111d0bfca4, ex: -2, sgn: 0},
	{hi: 0x9f917abeda4498df, lo: 0xf3ff913a4aadb85e, ex: -2, sgn: 0},
	{hi: 0xa2abb58949f2ced7, lo: 0xa5dbee6084ee1260, ex: -2, sgn: 0},
	{hi: 0xa5c58bfbcfd4436a, lo: 0x69fcb11e19f58619, ex: -2, sgn: 0},
	{hi: 0xa8defc2cbe2f8fcc, lo: 0xcd12a1f6ab6b095, ex: -2, sgn: 0},
	{hi: 0xabf80432a65ef190, lo: 0x8c95c4c91179176b, ex: -2, sgn: 0},
	{hi: 0xaf10a22459fe32a6, lo: 0x3feef3bb58b1f10d, ex: -2, sgn: 0},
	{hi: 0xb228d418ec1869ad, lo: 0x16031a34d4fc855d, ex: -2, sgn: 0},
	{hi: 0xb5409827b25591f0, lo: 0xcd73fb5d8d45d302, ex: -2, sgn: 0},
	{hi: 0xb857ec684627fa4c, lo: 0x187e26d290714d70, ex: -2, sgn: 0},
	{hi: 0xbb6ecef285f98a3a, lo: 0xbddd8a0365d6b1d3, ex: -2, sgn: 0},
	{hi: 0xbe853dde9658dc60, lo: 0xdfe1b074e22fc666, ex: -2, sgn: 0},
	{hi: 0xc19b3744e3262dcd, lo: 0xad5a41de48f6b26f, ex: -2, sgn: 0},
	{hi: 0xc4b0b93e20c0213f, lo: 0xdab4e426409b23a0, ex: -2, sgn: 0},
	{hi: 0xc7c5c1e34d3055b2, lo: 0x5cc8c00e4fccd850, ex: -2, sgn: 0},
	{hi: 0xcada4f4db157cf77, lo: 0xfa6171200ab2efc3, ex: -2, sgn: 0},
	{hi: 0xcdee5f96e21b332c, lo: 0x65a3132adfb7dfd5, ex: -2, sgn: 0},
	{hi: 0xd101f0d8c18ed1c1, lo: 0xaadb580a1eba209f, ex: -2, sgn: 0},
	{hi: 0xd415012d802284f0, lo: 0xdf4005ef6a64aa02, ex: -2, sgn: 0},
	{hi: 0xd7278eaf9dcd5b55, lo: 0x1779df36d1cc8912, ex: -2, sgn: 0},
	{hi: 0xda399779eb391377, lo: 0xcbabaeb97af8e8aa, ex: -2, sgn: 0},
	{hi: 0xdd4b19a78aed6515, lo: 0xece7f445cecf1e28, ex: -2, sgn: 0},
	{hi: 0xe05c1353f27b17e5, lo: 0xebc61ade6ca83cd, ex: -2, sgn: 0},
	{hi: 0xe36c829aeba6e720, lo: 0x26a0eecdb4f16266, ex: -2, sgn: 0},
	{hi: 0xe67c659895943123, lo: 0x82b0aecadf808123, ex: -2, sgn: 0},
	{hi: 0xe98bba6965ef725f, lo: 0xb91caf23416e7e80, ex: -2, sgn: 0},
	{hi: 0xec9a7f2a2a188aeb, lo: 0x7244ee20f591983b, ex: -2, sgn: 0},
	{hi: 0xefa8b1f8084ccdfc, lo: 0x1050cdf22f34182f, ex: -2, sgn: 0},
	{hi: 0xf2b650f080d0da8d, lo: 0x587f3fa044e2d27d, ex: -2, sgn: 0},
	{hi: 0xf5c35a316f1a3c80, lo: 0x643720de93ba81bd, ex: -2, sgn: 0},
	{hi: 0xf8cfcbd90af8d57a, lo: 0x4221dc4ba772598d, ex: -2, sgn: 0},
	{hi: 0xfbdba405e9c00cca, lo: 0xd24d3023da491920, ex: -2, sgn: 0},
	{hi: 0xfee6e0d6ff6fc5a4, lo: 0x8b74fe2508ab8fc2, ex: -2, sgn: 0},
	{hi: 0x80f8c035cfee8d76, lo: 0xfd958d68e8b49e6b, ex: -1, sgn: 0},
	{hi: 0x827dc071bfed6ffa, lo: 0xfb4c92369f0cf008, ex: -1, sgn: 0},
	{hi: 0x8402702f5b30f2a9, lo: 0xcb07b25a7b0372a7, ex: -1, sgn: 0},
	{hi: 0x8586ce7ededc809d, lo: 0x9d3dc689006896f4, ex: -1, sgn: 0},
	{hi: 0x870ada70ba4e6d49, lo: 0x9d52755ece3f70, ex: -1, sgn: 0},
	{hi: 0x888e93158fb3bb04, lo: 0x984156f553344306, ex: -1, sgn: 0},
	{hi: 0x8a11f77e349bc245, lo: 0xa66d1d936c38c329, ex: -1, sgn: 0},
	{hi: 0x8b9506bbb28bb922, lo: 0x575f33366be0afef, ex: -1, sgn: 0},
	{hi: 0x8d17bfdf47921ac8, lo: 0xcb590d74f64e77c9, ex: -1, sgn: 0},
	{hi: 0x8e9a21fa66d9ee8d, lo: 0xf2be3ecae62789d4, ex: -1, sgn: 0},
	{hi: 0x901c2c1eb93dee39, lo: 0x632b9cff5cfee724, ex: -1, sgn: 0},
	{hi: 0x919ddd5e1ddb8b33, lo: 0x609c464b3dd676ec, ex: -1, sgn: 0},
	{hi: 0x931f34caaaa5d23a, lo: 0x6a1ff8bfe6396e28, ex: -1, sgn: 0},
	{hi: 0x94a03176acf82d45, lo: 0xae4ba773da6bf754, ex: -1, sgn: 0},
	{hi: 0x9620d274aa290339, lo: 0xe06a955a5b8e301d, ex: -1, sgn: 0},
	{hi: 0x97a116d7601c3515, lo: 0xfc8b7184b21f2d50, ex: -1, sgn: 0},
	{hi: 0x9920fdb1c5d5783d, lo: 0x9dd1eedf18a2e4df, ex: -1, sgn: 0},
	{hi: 0x9aa086170c0a8d86, lo: 0x9ffa0d23f3c26c62, ex: -1, sgn: 0},
	{hi: 0x9c1faf1a9db554af, lo: 0xdab6b478577e7be5, ex: -1, sgn: 0},
	{hi: 0x9d9e77d020a5bbe6, lo: 0xdb895384528d0d60, ex: -1, sgn: 0},
	{hi: 0x9f1cdf4b76138b02, lo: 0x98dbd3555ebcdefe, ex: -1, sgn: 0},
	{hi: 0xa09ae4a0bb300a19, lo: 0x2f895f44a303cc0b, ex: -1, sgn: 0},
	{hi: 0xa21886e449b78316, lo: 0xd29d23a624acd00c, ex: -1, sgn: 0},
	{hi: 0xa395c52ab8829dfc, lo: 0x2be036401ba87cc2, ex: -1, sgn: 0},
	{hi: 0xa5129e88dc17976a, lo: 0x82d9495ead5be348, ex: -1, sgn: 0},
	{hi: 0xa68f1213c73b5124, lo: 0x17218792857f4c5a, ex: -1, sgn: 0},
	{hi: 0xa80b1ee0cb823c27, lo: 0x3269f4702b88324a, ex: -1, sgn: 0},
	{hi: 0xa986c40579e11c0a, lo: 0x8e3bdf8085321556, ex: -1, sgn: 0},
	{hi: 0xab020097a33da341, lo: 0xc1654b64a0081b46, ex: -1, sgn: 0},
	{hi: 0xac7cd3ad58fee7f0, lo: 0x811f953984eff83e, ex: -1, sgn: 0},
	{hi: 0xadf73c5ced9db0f3, lo: 0x9a5318ac6fe94e4d, ex: -1, sgn: 0},
	{hi: 0xaf7139bcf5349ac6, lo: 0x9fe5f4ea48965e2c, ex: -1, sgn: 0},
	{hi: 0xb0eacae4461013ed, lo: 0x63c66682bae74898, ex: -1, sgn: 0},
	{hi: 0xb263eee9f93e3088, lo: 0x695a5332090bb09b, ex: -1, sgn: 0},
	{hi: 0xb3dca4e56b1e54bb, lo: 0x992d96e5021e3c37, ex: -1, sgn: 0},
	{hi: 0xb554ebee3bf0b58e, lo: 0x971f4da709ad4378, ex: -1, sgn: 0},
	{hi: 0xb6ccc31c5065afee, lo: 0x35ebacd79f209137, ex: -1, sgn: 0},
	{hi: 0xb8442987d22cf576, lo: 0x9cc3ef36746de3b8, ex: -1, sgn: 0},
	{hi: 0xb9bb1e4930848ead, lo: 0xcdb0531c4e58484b, ex: -1, sgn: 0},
	{hi: 0xbb31a07920c7b256, lo: 0x55b92083658bb897, ex: -1, sgn: 0},
	{hi: 0xbca7af309efd7182, lo: 0xa4b0d21fc5036a5, ex: -1, sgn: 0},
	{hi: 0xbe1d4988ee67380c, lo: 0xd1f90f79f46c7e01, ex: -1, sgn: 0},
	{hi: 0xbf926e9b9a0f2127, lo: 0x91a1b5eb79658c67, ex: -1, sgn: 0},
	{hi: 0xc1071d8275561f9b, lo: 0x721853f8e528a934, ex: -1, sgn: 0},
	{hi: 0xc27b55579c81f96d, lo: 0xcdc2bd470675104d, ex: -1, sgn: 0},
	{hi: 0xc3ef1535754b168d, lo: 0x3122c2a59efddc37, ex: -1, sgn: 0},
	{hi: 0xc5625c36af6a222f, lo: 0xf4ff2895ab6ebe89, ex: -1, sgn: 0},
	{hi: 0xc6d5297645257e8d, lo: 0x14d24739de27e2e9, ex: -1, sgn: 0},
	{hi: 0xc8477c0f7bde8a98, lo: 0x4ce0246ad4fa74, ex: -1, sgn: 0},
	{hi: 0xc9b9531de49eb968, lo: 0x4319e5ad5b0dcb84, ex: -1, sgn: 0},
	{hi: 0xcb2aadbd5ca47af5, lo: 0xfaa3dfe675a65ee2, ex: -1, sgn: 0},
	{hi: 0xcc9b8b0a0deff5d4, lo: 0x2e663b3c7555a6c3, ex: -1, sgn: 0},
	{hi: 0xce0bea206fcf9192, lo: 0x3c540a9eec47af38, ex: -1, sgn: 0},
	{hi: 0xcf7bca1d476c516d, lo: 0xa81290bdbaad62e4, ex: -1, sgn: 0},
	{hi: 0xd0eb2a1da855fefd, lo: 0xb9302788604e88f1, ex: -1, sgn: 0},
	{hi: 0xd25a093ef50f2482, lo: 0x721fc87ba1d42456, ex: -1, sgn: 0},
	{hi: 0xd3c8669edf98d680, lo: 0x87967926fdcecec4, ex: -1, sgn: 0},
	{hi: 0xd536415b69fe4c54, lo: 0x1df22346611c6b4b, ex: -1, sgn: 0},
	{hi: 0xd6a39892e6e04764, lo: 0x3090d44db12c418c, ex: -1, sgn: 0},
	{hi: 0xd8106b63fa0048a0, lo: 0xa573f2aa90434ba5, ex: -1, sgn: 0},
	{hi: 0xd97cb8ed98cb93f5, lo: 0x2e349483e3fb2a6a, ex: -1, sgn: 0},
	{hi: 0xdae8804f0ae6015b, lo: 0x362cb974182e3030, ex: -1, sgn: 0},
	{hi: 0xdc53c0a7eab49b35, lo: 0x3ccca3982328ed8b, ex: -1, sgn: 0},
	{hi: 0xddbe791825e8099e, lo: 0x1a5bd9269d408d7e, ex: -1, sgn: 0},
	{hi: 0xdf28a8bffe06ca56, lo: 0xcce2634be2bf54df, ex: -1, sgn: 0},
	{hi: 0xe0924ec008f734fd, lo: 0x8aa895d5bf3e84ea, ex: -1, sgn: 0},
	{hi: 0xe1fb6a3931894b38, lo: 0xf7a1f9bd9ba13b6b, ex: -1, sgn: 0},
	{hi: 0xe363fa4cb8005482, lo: 0x7b32c72e31824e51, ex: -1, sgn: 0},
	{hi: 0xe4cbfe1c329c453a, lo: 0xd40e9e6b989f89e5, ex: -1, sgn: 0},
	{hi: 0xe63374c98e22f0b4, lo: 0x2872ce1bfc7ad1cd, ex: -1, sgn: 0},
	{hi: 0xe79a5d770e6905dc, lo: 0xf1b65cc5fd780262, ex: -1, sgn: 0},
	{hi: 0xe900b7474edad637, lo: 0x431626c10485bdda, ex: -1, sgn: 0},
	{hi: 0xea66815d4304e6c8, lo: 0xcc39cfcc29960b1, ex: -1, sgn: 0},
	{hi: 0xebcbbadc371c4aaa, lo: 0x1d90f780ae951140, ex: -1, sgn: 0},
	{hi: 0xed3062e7d086c6f0, lo: 0xc71debc372b6f9d4, ex: -1, sgn: 0},
	{hi: 0xee9478a40e62bf86, lo: 0x2a24164daec85ccb, ex: -1, sgn: 0},
	{hi: 0xeff7fb354a0eecb1, lo: 0x527233b40d3432bb, ex: -1, sgn: 0},
	{hi: 0xf15ae9c037b1d8f0, lo: 0x6c48e9e3420b0f1e, ex: -1, sgn: 0},
	{hi: 0xf2bd4369e6c126d3, lo: 0x7f232aee178c6323, ex: -1, sgn: 0},
	{hi: 0xf41f0757c2889e84, lo: 0x3c7f10db458c337c, ex: -1, sgn: 0},
	{hi: 0xf58034af92b102a7, lo: 0x93fa6107c4327527, ex: -1, sgn: 0},
	{hi: 0xf6e0ca977bc6ac45, lo: 0xe1079824233fef46, ex: -1, sgn: 0},
	{hi: 0xf840c835ffbfed66, lo: 0xa9a56012067c570c, ex: -1, sgn: 0},
	{hi: 0xf9a02cb1fe833a0d, lo: 0x8da894471de1a18, ex: -1, sgn: 0},
	{hi: 0xfafef732b66d1742, lo: 0x343fbf4a7d42af3, ex: -1, sgn: 0},
	{hi: 0xfc5d26dfc4d5cfda, lo: 0x27c07c911290b8d1, ex: -1, sgn: 0},
	{hi: 0xfdbabae12696eea4, lo: 0x2377c3799c052fa, ex: -1, sgn: 0},
	{hi: 0xff17b25f38907dad, lo: 0xa9c6ba50490539f, ex: -1, sgn: 0},
	{hi: 0x803a06415c170525, lo: 0x6f53873e2f1477ff, ex: 0, sgn: 0},
	{hi: 0x80e7e43a61f5b6cb, lo: 0x5ca183dc973abc22, ex: 0, sgn: 0},
	{hi: 0x819572af6decac84, lo: 0x9fba97fdf0c4d24c, ex: 0, sgn: 0},
	{hi: 0x8242b1357110d372, lo: 0x6fb2123fedfa6e22, ex: 0, sgn: 0},
	{hi: 0x82ef9f618dc5b70e, lo: 0x91a965931f1a200a, ex: 0, sgn: 0},
	{hi: 0x839c3cc917ff6cb4, lo: 0xbfd79717f2880abf, ex: 0, sgn: 0},
	{hi: 0x8448890195846099, lo: 0x246efcff30cb064a, ex: 0, sgn: 0},
	{hi: 0x84f483a0be2f0403, lo: 0x51917cac857fd5f5, ex: 0, sgn: 0},
	{hi: 0x85a02c3c7c2f5ca5, lo: 0x327888fe4b62687b, ex: 0, sgn: 0},
	{hi: 0x864b826aec4c74e5, lo: 0x85043222c9bdd18d, ex: 0, sgn: 0},
	{hi: 0x86f685c25e25acf5, lo: 0x7e0b9b07548471a2, ex: 0, sgn: 0},
	{hi: 0x87a135d95473ec89, lo: 0x4e091160e2430712, ex: 0, sgn: 0},
	{hi: 0x884b9246854ab50b, lo: 0x4f14c8afe4560291, ex: 0, sgn: 0},
	{hi: 0x88f59aa0da591421, lo: 0xb892ca8361d8c84c, ex: 0, sgn: 0},
	{hi: 0x899f4e7f712a765e, lo: 0xc88302a31afce54a, ex: 0, sgn: 0},
	{hi: 0x8a48ad799b6759f3, lo: 0x660558a02136130a, ex: 0, sgn: 0},
	{hi: 0x8af1b726df15e13c, lo: 0x545f7d79ead8fa19, ex: 0, sgn: 0},
	{hi: 0x8b9a6b1ef6da4502, lo: 0x21a6675f51580bc4, ex: 0, sgn: 0},
	{hi: 0x8c42c8f9d2372644, lo: 0x101a5adbcb9ffb43, ex: 0, sgn: 0},
	{hi: 0x8cead04f95cdbf66, lo: 0x4d49cbaf15aecd80, ex: 0, sgn: 0},
	{hi: 0x8d9280b89b9df49b, lo: 0xde2d43c6b67a7cbe, ex: 0, sgn: 0},
	{hi: 0x8e39d9cd73464364, lo: 0xbba4cfecbff54867, ex: 0, sgn: 0},
	{hi: 0x8ee0db26e24390f8, lo: 0xaf0e2345f3bd24b4, ex: 0, sgn: 0},
	{hi: 0x8f87845de430d777, lo: 0x9311a82459aa0f72, ex: 0, sgn: 0},
	{hi: 0x902dd50bab06b1b7, lo: 0xb144016c7a30b39a, ex: 0, sgn: 0},
	{hi: 0x90d3ccc99f5ac58b, lo: 0x9d1072e09b72292, ex: 0, sgn: 0},
	{hi: 0x91796b31609f0c54, lo: 0x6714fe6925b78cc4, ex: 0, sgn: 0},
	{hi: 0x921eafdcc560f9c5, lo: 0x33d0a284a8c954ad, ex: 0, sgn: 0},
	{hi: 0x92c39a65db88809d, lo: 0x1f8481e704e4a767, ex: 0, sgn: 0},
	{hi: 0x93682a66e896f544, lo: 0xb17821911e71c16e, ex: 0, sgn: 0},
	{hi: 0x940c5f7a69e5ce1c, lo: 0x1489a97671a42, ex: 0, sgn: 0},
	{hi: 0x94b0393b14e54156, lo: 0xd6c7af02d5c16fd9, ex: 0, sgn: 0},
	{hi: 0x9553b743d75ac03f, lo: 0xac0106650f4ef023, ex: 0, sgn: 0},
	{hi: 0x95f6d92fd79f4fba, lo: 0xd9f8e1a446e973b9, ex: 0, sgn: 0},
	{hi: 0x96999e9a74ddbde3, lo: 0xa7a7556c3b33abc1, ex: 0, sgn: 0},
	{hi: 0x973c071f4750b49c, lo: 0xc0a03934f0cce19b, ex: 0, sgn: 0},
	{hi: 0x97de125a2080a8ed, lo: 0xd243aa0843a2c144, ex: 0, sgn: 0},
	{hi: 0x987fbfe70b81a708, lo: 0x19cec845ac87a5c6, ex: 0, sgn: 0},
	{hi: 0x99210f624d30facb, lo: 0xc4b992a37fb9b9bd, ex: 0, sgn: 0},
	{hi: 0x99c200686472b4a8, lo: 0x1ab42d43235757b6, ex: 0, sgn: 0},
	{hi: 0x9a6292960a6f0ab0, lo: 0x7e92c655656e6b85, ex: 0, sgn: 0},
	{hi: 0x9b02c58832cf95c0, lo: 0x698b94f50326a043, ex: 0, sgn: 0},
	{hi: 0x9ba298dc0bfc6a88, lo: 0x9a5614e8ffbeac6f, ex: 0, sgn: 0},
	{hi: 0x9c420c2eff590e5f, lo: 0xc7fd954194e6d8aa, ex: 0, sgn: 0},
	{hi: 0x9ce11f1eb18147b1, lo: 0x3e93627de8fd5779, ex: 0, sgn: 0},
	{hi: 0x9d7fd1490285c9e3, lo: 0xe25e39549638ae68, ex: 0, sgn: 0},
	{hi: 0x9e1e224c0e28bc94, lo: 0x2cad377d5c9c35d8, ex: 0, sgn: 0},
	{hi: 0x9ebc11c62c1a1dfb, lo: 0xcc141e10c6460c8b, ex: 0, sgn: 0},
	{hi: 0x9f599f55f0340061, lo: 0xa88d5f46834bbf8d, ex: 0, sgn: 0},
	{hi: 0x9ff6ca9a2ab6a26d, lo: 0x22cc118a0c118aa0, ex: 0, sgn: 0},
	{hi: 0xa0939331e8846237, lo: 0x7cec6df5bea167cf, ex: 0, sgn: 0},
	{hi: 0xa12ff8bc735d8af6, lo: 0x71acea2819360c35, ex: 0, sgn: 0},
	{hi: 0xa1cbfad9521bfd1b, lo: 0x166c36e7bb3c402f, ex: 0, sgn: 0},
	{hi: 0xa267992848eeb0c0, lo: 0x3b5167ee359a234e, ex: 0, sgn: 0},
	{hi: 0xa302d34959951243, lo: 0x9443372e20d4377c, ex: 0, sgn: 0},
	{hi: 0xa39da8dcc39a38e5, lo: 0xca9a8a720d4c69c, ex: 0, sgn: 0},
	{hi: 0xa4381983048ff747, lo: 0xbf623cf5301a2dde, ex: 0, sgn: 0},
	{hi: 0xa4d224dcd849c5b0, lo: 0x23d251cc8d7975cc, ex: 0, sgn: 0},
	{hi: 0xa56bca8b391785db, lo: 0x189d39ffe11aaa2b, ex: 0, sgn: 0},
	{hi: 0xa6050a2f60002049, lo: 0x8c33ebf3aa8501fb, ex: 0, sgn: 0},
	{hi: 0xa69de36ac4fbfadc, lo: 0x9b3ad6e4022183d9, ex: 0, sgn: 0},
	{hi: 0xa73655df1f2f489e, lo: 0x149f6e75993468a3, ex: 0, sgn: 0},
	{hi: 0xa7ce612e65243291, lo: 0x6b2a39f856a69781, ex: 0, sgn: 0},
	{hi: 0xa86604facd04d969, lo: 0x3463a2c2e6e9cc55, ex: 0, sgn: 0},
	{hi: 0xa8fd40e6ccd52ffd, lo: 0x6cc14c4f53e2e82d, ex: 0, sgn: 0},
	{hi: 0xa99414951aacae5e, lo: 0xd147625fda929af8, ex: 0, sgn: 0},
	{hi: 0xaa2a7fa8acefdd63, lo: 0xb714ee81b53b4b9d, ex: 0, sgn: 0},
	{hi: 0xaac081c4ba89ba8a, lo: 0xe1b3dfc4dbda9bfd, ex: 0, sgn: 0},
	{hi: 0xab561a8cbb24f410, lo: 0xf17cee69b0d2ecde, ex: 0, sgn: 0},
	{hi: 0xabeb49a46764fd15, lo: 0x1becda8089c1a94c, ex: 0, sgn: 0},
	{hi: 0xac800eafb91ef9a9, lo: 0xf86ba0dde982fb59, ex: 0, sgn: 0},
	{hi: 0xad146952eb9282af, lo: 0x44bf16268608db96, ex: 0, sgn: 0},
	{hi: 0xada859327ba24151, lo: 0x9d30d4cfeb04f1fb, ex: 0, sgn: 0},
	{hi: 0xae3bddf3280c620d, lo: 0x3d53817865422565, ex: 0, sgn: 0},
	{hi: 0xaecef739f1a2df10, lo: 0xf74d099042e8f326, ex: 0, sgn: 0},
	{hi: 0xaf61a4ac1b83a1de, lo: 0xa89a9b8f726b95bf, ex: 0, sgn: 0},
	{hi: 0xaff3e5ef2b507c06, lo: 0x8c679e67fc462d51, ex: 0, sgn: 0},
	{hi: 0xb085baa8e966f6da, lo: 0xe4cad00d5c94bcd2, ex: 0, sgn: 0},
	{hi: 0xb117227f6117f9f9, lo: 0x8d8be132d576e614, ex: 0, sgn: 0},
	{hi: 0xb1a81d18e0df4889, lo: 0x24784f32c3e3e5bd, ex: 0, sgn: 0},
	{hi: 0xb238aa1bfa9ad507, lo: 0x8cc7d4bd05ffd5ae, ex: 0, sgn: 0},
	{hi: 0xb2c8c92f83c1eb87, lo: 0xac9f7ebbc469ef59, ex: 0, sgn: 0},
	{hi: 0xb35879fa959c323c, lo: 0x5d6635109164f740, ex: 0, sgn: 0},
	{hi: 0xb3e7bc248d78802e, lo: 0xa156468ef6c18c60, ex: 0, sgn: 0},
	{hi: 0xb4768f550ce389fd, lo: 0x4a85350f69018c55, ex: 0, sgn: 0},
}

var tanC = [256]dint{
	{hi: 0x8000000000000000, lo: 0x0, ex: 1, sgn: 0},
	{hi: 0xffffb10b10e80e95, lo: 0x3031437d7eccb9df, ex: 0, sgn: 0},
	{hi: 0xfffec42c7454926b, lo: 0x38e310779edfec68, ex: 0, sgn: 0},
	{hi: 0xfffd3964bc6275ba, lo: 0x69fff9ae0dedb047, ex: 0, sgn: 0},
	{hi: 0xfffb10b4dc96dabb, lo: 0xb47903f7a19f8ee2, ex: 0, sgn: 0},
	{hi: 0xfff84a1e29de8571, lo: 0x8cc193c5d508e13f, ex: 0, sgn: 0},
	{hi: 0xfff4e5a25a8d095b, lo: 0x43366df666fd54ff, ex: 0, sgn: 0},
	{hi: 0xfff0e343865bbb13, lo: 0x5428ed0647c9e5d1, ex: 0, sgn: 0},
	{hi: 0xffec4304266865d9, lo: 0x5657552366961732, ex: 0, sgn: 0},
	{hi: 0xffe704e71533c508, lo: 0x53aa9423bb0adc21, ex: 0, sgn: 0},
	{hi: 0xffe128ef8e9fc17a, lo: 0x7d209f32d42d864e, ex: 0, sgn: 0},
	{hi: 0xffdaaf212fed72db, lo: 0x4fd8f038449ec436, ex: 0, sgn: 0},
	{hi: 0xffd3977ff7bae4e9, lo: 0x664649b4d541b9c5, ex: 0, sgn: 0},
	{hi: 0xffcbe2104600a0a9, lo: 0x5595ca3f421ae09c, ex: 0, sgn: 0},
	{hi: 0xffc38ed6dc0ef98b, lo: 0x1c676208aa3be545, ex: 0, sgn: 0},
	{hi: 0xffba9dd8dc8b1e83, lo: 0xccfed60a91097c48, ex: 0, sgn: 0},
	{hi: 0xffb10f1bcb6bef1d, lo: 0x421e8edaaf59453e, ex: 0, sgn: 0},
	{hi: 0xffa6e2a58df6947d, lo: 0xd2c665c2da3e7844, ex: 0, sgn: 0},
	{hi: 0xff9c187c6abade6a, lo: 0x1e1862cca089938b, ex: 0, sgn: 0},
	{hi: 0xff90b0a7098f6443, lo: 0x2dabd3195a05710f, ex: 0, sgn: 0},
	{hi: 0xff84ab2c738d6a03, lo: 0x519c314973ccae6b, ex: 0, sgn: 0},
	{hi: 0xff780814130c893c, lo: 0x3ea4f30adda3016f, ex: 0, sgn: 0},
	{hi: 0xff6ac765b39e1e19, lo: 0x1b9d5851979f28fb, ex: 0, sgn: 0},
	{hi: 0xff5ce92982087867, lo: 0x50a7bb6a6ee3b0f1, ex: 0, sgn: 0},
	{hi: 0xff4e6d680c41d0a9, lo: 0xf668633f1ab858a, ex: 0, sgn: 0},
	{hi: 0xff3f542a416b0134, lo: 0xb085c1828f69296a, ex: 0, sgn: 0},
	{hi: 0xff2f9d7971ca0364, lo: 0x27e31939e2eec09c, ex: 0, sgn: 0},
	{hi: 0xff1f495f4ec430d7, lo: 0xf5971326a3540ea9, ex: 0, sgn: 0},
	{hi: 0xff0e57e5ead848d1, lo: 0x1f1901544271c3f8, ex: 0, sgn: 0},
	{hi: 0xfefcc917b99839a5, lo: 0xe0abd3a9b64df725, ex: 0, sgn: 0},
	{hi: 0xfeea9cff8fa2ae54, lo: 0xec34413e87ef2740, ex: 0, sgn: 0},
	{hi: 0xfed7d3a8a29c603b, lo: 0x2f88b949a72ff96c, ex: 0, sgn: 0},
	{hi: 0xfec46d1e89292cf0, lo: 0x41390efdc726e9ef, ex: 0, sgn: 0},
	{hi: 0xfeb0696d3ae4f04d, lo: 0xb7b6cc53c3abc817, ex: 0, sgn: 0},
	{hi: 0xfe9bc8a1105c22a5, lo: 0xd3af6ee4f2101c20, ex: 0, sgn: 0},
	{hi: 0xfe868ac6c3043b2e, lo: 0xb4f70c910505e10, ex: 0, sgn: 0},
	{hi: 0xfe70afeb6d33d6a2, lo: 0x2907cf2b3f6feac2, ex: 0, sgn: 0},
	{hi: 0xfe5a381c8a1aa224, lo: 0xd54faa364b7da8f6, ex: 0, sgn: 0},
	{hi: 0xfe432367f5b90a62, lo: 0x87b8875373a818a4, ex: 0, sgn: 0},
	{hi: 0xfe2b71dbecd7aefc, lo: 0x8598c2c429caf7, ex: 0, sgn: 0},
	{hi: 0xfe1323870cfe9a3d, lo: 0x90cd1d959db674ef, ex: 0, sgn: 0},
	{hi: 0xfdfa3878546c3d28, lo: 0x9bfe5c51e91cbdcd, ex: 0, sgn: 0},
	{hi: 0xfde0b0bf220c2fd4, lo: 0xe276d247626a23fd, ex: 0, sgn: 0},
	{hi: 0xfdc68c6b356db62f, lo: 0x499ddb331d19539d, ex: 0, sgn: 0},
	{hi: 0xfdabcb8caeba091b, lo: 0xfac7397cc07a6470, ex: 0, sgn: 0},
	{hi: 0xfd906e340eaa6401, lo: 0xd6e270740a186977, ex: 0, sgn: 0},
	{hi: 0xfd747472367dd6c5, lo: 0x61beb8cd2696fc78, ex: 0, sgn: 0},
	{hi: 0xfd57de5867eedc39, lo: 0x6c696582f346fd91, ex: 0, sgn: 0},
	{hi: 0xfd3aabf84528b50b, lo: 0xeae6bd951c1dabbe, ex: 0, sgn: 0},
	{hi: 0xfd1cdd63d0bc8735, lo: 0x863b87258f11ad7e, ex: 0, sgn: 0},
	{hi: 0xfcfe72ad6d9641f2, lo: 0xa06fab9f9d106709, ex: 0, sgn: 0},
	{hi: 0xfcdf6be7def1464c, lo: 0xa4e064308f4999f4, ex: 0, sgn: 0},
	{hi: 0xfcbfc926484cd43a, lo: 0xa3e22b4d38917e73, ex: 0, sgn: 0},
	{hi: 0xfc9f8a7c2d603c60, lo: 0x5d582cac7cb4391c, ex: 0, sgn: 0},
	{hi: 0xfc7eaffd720ed673, lo: 0x2880268f2e62955, ex: 0, sgn: 0},
	{hi: 0xfc5d39be5a5bbc4b, lo: 0x1c0d254b6c8da4bd, ex: 0, sgn: 0},
	{hi: 0xfc3b27d38a5d49ab, lo: 0x256778ffcb5c1769, ex: 0, sgn: 0},
	{hi: 0xfc187a52063060c2, lo: 0x9433b49289417ea2, ex: 0, sgn: 0},
	{hi: 0xfbf5314f31eb7375, lo: 0x25aafd7fdba12c5f, ex: 0, sgn: 0},
	{hi: 0xfbd14ce0d191516e, lo: 0x7190c94899dff1b8, ex: 0, sgn: 0},
	{hi: 0xfbaccd1d0903bb09, lo: 0xe63ae8632b84473c, ex: 0, sgn: 0},
	{hi: 0xfb87b21a5bf5b917, lo: 0x75df66f0ec3dd459, ex: 0, sgn: 0},
	{hi: 0xfb61fbefadddb985, lo: 0x61ce9d5ef5a81487, ex: 0, sgn: 0},
	{hi: 0xfb3baab441e770f7, lo: 0xb4b54683879c9c17, ex: 0, sgn: 0},
	{hi: 0xfb14be7fbae58156, lo: 0x2172a361fd2a722f, ex: 0, sgn: 0},
	{hi: 0xfaed376a1b42e559, lo: 0x2079880c450348ac, ex: 0, sgn: 0},
	{hi: 0xfac5158bc4f4211f, lo: 0x4a188aa367f90ab1, ex: 0, sgn: 0},
	{hi: 0xfa9c58fd796837d4, lo: 0x10655ecd5cc771d8, ex: 0, sgn: 0},
	{hi: 0xfa7301d859796671, lo: 0x1fe196a53fb5b237, ex: 0, sgn: 0},
	{hi: 0xfa491035e55da3a3, lo: 0xd24377c77a591e24, ex: 0, sgn: 0},
	{hi: 0xfa1e842ffc96e4e0, lo: 0x431c393c7f62da65, ex: 0, sgn: 0},
	{hi: 0xf9f35de0dde328ab, lo: 0xba5dbf4510eddc8f, ex: 0, sgn: 0},
	{hi: 0xf9c79d63272c4628, lo: 0x4504ae08d19b2980, ex: 0, sgn: 0},
	{hi: 0xf99b42d1d57781eb, lo: 0x78685d850f80ecdc, ex: 0, sgn: 0},
	{hi: 0xf96e4e4844d4e82a, lo: 0x80e8c17bf80e8f02, ex: 0, sgn: 0},
	{hi: 0xf940bfe2304e6c45, lo: 0xc0e2a1352ed7f292, ex: 0, sgn: 0},
	{hi: 0xf91297bbb1d6cdbe, lo: 0x68fc6e4d6a920bd2, ex: 0, sgn: 0},
	{hi: 0xf8e3d5f1423842a0, lo: 0x9701914c7f8fbcd7, ex: 0, sgn: 0},
	{hi: 0xf8b47a9fb902e76c, lo: 0xac9f07f54ff5bc14, ex: 0, sgn: 0},
	{hi: 0xf88485e44c7af48a, lo: 0xb36a9dfaadafc1e1, ex: 0, sgn: 0},
	{hi: 0xf853f7dc9186b952, lo: 0xc7adc6b4988891bb, ex: 0, sgn: 0},
	{hi: 0xf822d0a67b9c5cb5, lo: 0xa776175bd284fe05, ex: 0, sgn: 0},
	{hi: 0xf7f110605caf6390, lo: 0xa76f7efc19aed41c, ex: 0, sgn: 0},
	{hi: 0xf7beb728e51dfcb8, lo: 0x730785813f78aa1e, ex: 0, sgn: 0},
	{hi: 0xf78bc51f239e12c6, lo: 0x214cffcee9dd33ca, ex: 0, sgn: 0},
	{hi: 0xf7583a62852a23b2, lo: 0x4becad887680c197, ex: 0, sgn: 0},
	{hi: 0xf7241712d4edde49, lo: 0xf99107e50d631330, ex: 0, sgn: 0},
	{hi: 0xf6ef5b503c328589, lo: 0x50ca117eb18beed7, ex: 0, sgn: 0},
	{hi: 0xf6ba073b424b19e8, lo: 0x2c791f59cc1ffc23, ex: 0, sgn: 0},
	{hi: 0xf6841af4cc8048a4, lo: 0xce8c455197cdf8a7, ex: 0, sgn: 0},
	{hi: 0xf64d969e1dfc2119, lo: 0x119d358de0493956, ex: 0, sgn: 0},
	{hi: 0xf6167a58d7b59026, lo: 0x9dc7e5954c5a8f24, ex: 0, sgn: 0},
	{hi: 0xf5dec646f85ba1c6, lo: 0xc8c615e72768d6b5, ex: 0, sgn: 0},
	{hi: 0xf5a67a8adc4088ca, lo: 0xed0dd4bf62edd13f, ex: 0, sgn: 0},
	{hi: 0xf56d97473d446cda, lo: 0x275a2bbb2bab6c8a, ex: 0, sgn: 0},
	{hi: 0xf5341c9f32bffeb9, lo: 0x8da64484aaa0febc, ex: 0, sgn: 0},
	{hi: 0xf4fa0ab6316ed2ec, lo: 0x163c5c7f03b718c5, ex: 0, sgn: 0},
	{hi: 0xf4bf61b00b5982b7, lo: 0x890ac4aafa6a37bf, ex: 0, sgn: 0},
	{hi: 0xf48421b0efbf939b, lo: 0xf8f9d3b87d11fd52, ex: 0, sgn: 0},
	{hi: 0xf4484add6b01254b, lo: 0x667e06866c07c369, ex: 0, sgn: 0},
	{hi: 0xf40bdd5a6688662f, lo: 0x5019794a1f5896e5, ex: 0, sgn: 0},
	{hi: 0xf3ced94d28b2ce8a, lo: 0x18ef535a7ffa7a3d, ex: 0, sgn: 0},
	{hi: 0xf3913edb54ba2242, lo: 0x50f29b4b49f31c37, ex: 0, sgn: 0},
	{hi: 0xf3530e2aea9d3966, lo: 0xd981acdcf6bc3e4, ex: 0, sgn: 0},
	{hi: 0xf314476247088f74, lo: 0xa5486bdc455d56a2, ex: 0, sgn: 0},
	{hi: 0xf2d4eaa8233e997d, lo: 0x431be53f92ece9e6, ex: 0, sgn: 0},
	{hi: 0xf294f82394ffe320, lo: 0xebadcdbf915e8f6c, ex: 0, sgn: 0},
	{hi: 0xf2546ffc0e72f286, lo: 0xaf0eed81e8c51e55, ex: 0, sgn: 0},
	{hi: 0xf21352595e0bf350, lo: 0xe7112e89103cc0c7, ex: 0, sgn: 0},
	{hi: 0xf1d19f63ae7428a2, lo: 0x844e6a35ddc2b713, ex: 0, sgn: 0},
	{hi: 0xf18f574386712643, lo: 0x8f6bac72988088b0, ex: 0, sgn: 0},
	{hi: 0xf14c7a21c8cbd0f4, lo: 0x2730081c758fb42b, ex: 0, sgn: 0},
	{hi: 0xf1090827b43725fd, lo: 0x67127db35b287316, ex: 0, sgn: 0},
	{hi: 0xf0c5017ee336ca0f, lo: 0xc4e557b119ef3185, ex: 0, sgn: 0},
	{hi: 0xf08066514c055f7e, lo: 0x973ea9903ed5125f, ex: 0, sgn: 0},
	{hi: 0xf03b36c9407aa3e8, lo: 0x992d39ec5c561d28, ex: 0, sgn: 0},
	{hi: 0xeff573116df1555d, lo: 0x62aef7b55319d1d4, ex: 0, sgn: 0},
	{hi: 0xefaf1b54dd2cdf0f, lo: 0xf03a18a5e16ab641, ex: 0, sgn: 0},
	{hi: 0xef682fbef23ecda6, lo: 0x767c0e8ad33bc085, ex: 0, sgn: 0},
	{hi: 0xef20b07b6c6c0b37, lo: 0xe2398bf0eeb28cde, ex: 0, sgn: 0},
	{hi: 0xeed89db66611e307, lo: 0x86f8c20fb664b01b, ex: 0, sgn: 0},
	{hi: 0xee8ff79c548acd0f, lo: 0xa1d2c3d018a9279f, ex: 0, sgn: 0},
	{hi: 0xee46be5a0813016b, lo: 0x7872773830d368be, ex: 0, sgn: 0},
	{hi: 0xedfcf21cabacd3b1, lo: 0xfee6a1eebfa13b4a, ex: 0, sgn: 0},
	{hi: 0xedb29311c504d652, lo: 0x11815196b9fbf5df, ex: 0, sgn: 0},
	{hi: 0xed67a1673455c601, lo: 0x7289102076a125e5, ex: 0, sgn: 0},
	{hi: 0xed1c1d4b344c3d4f, lo: 0xddffe98c4f8aa031, ex: 0, sgn: 0},
	{hi: 0xecd006ec59ea306f, lo: 0xa8392eb238578ab0, ex: 0, sgn: 0},
	{hi: 0xec835e79946a3145, lo: 0x7e610231ac1d6181, ex: 0, sgn: 0},
	{hi: 0xec3624222d227bd1, lo: 0x278047ae3dd0889, ex: 0, sgn: 0},
	{hi: 0xebe85815c767cb00, lo: 0x1e99ccb9adc62ca6, ex: 0, sgn: 0},
	{hi: 0xeb99fa84606ff5ff, lo: 0xdae311e656e0661, ex: 0, sgn: 0},
	{hi: 0xeb4b0b9e4f345617, lo: 0x39e39c6c2ab3655d, ex: 0, sgn: 0},
	{hi: 0xeafb8b944453f52f, lo: 0x3383bbb5156bf1d7, ex: 0, sgn: 0},
	{hi: 0xeaab7a9749f584fe, lo: 0x24db98ad3a0647a1, ex: 0, sgn: 0},
	{hi: 0xea5ad8d8c3a91f05, lo: 0x4a0ca5ea449b1c83, ex: 0, sgn: 0},
	{hi: 0xea09a68a6e49cd62, lo: 0x15ad45b4a1b5e823, ex: 0, sgn: 0},
	{hi: 0xe9b7e3de5fdedc8b, lo: 0xcd24d4bd1056c826, ex: 0, sgn: 0},
	{hi: 0xe9659107077cf60f, lo: 0x89a92b199adfbafa, ex: 0, sgn: 0},
	{hi: 0xe912ae372d27045d, lo: 0xacb1c26a06e5ae02, ex: 0, sgn: 0},
	{hi: 0xe8bf3ba1f1aedfbb, lo: 0xf8972affb3d98e1f, ex: 0, sgn: 0},
	{hi: 0xe86b397ace95c46f, lo: 0x9fec1e78c4376186, ex: 0, sgn: 0},
	{hi: 0xe816a7f595ec9232, lo: 0xbfe8378abfb87b6f, ex: 0, sgn: 0},
	{hi: 0xe7c187467233d508, lo: 0xdbfb0fe56c6f80fe, ex: 0, sgn: 0},
	{hi: 0xe76bd7a1e63b9786, lo: 0x125129529d48a92f, ex: 0, sgn: 0},
	{hi: 0xe715993ccd02fe9c, lo: 0xe2ba81b9ce96e02e, ex: 0, sgn: 0},
	{hi: 0xe6becc4c5997af06, lo: 0x82fcedb4c6434d76, ex: 0, sgn: 0},
	{hi: 0xe667710616f4fc59, lo: 0xdd2a3e32c3859960, ex: 0, sgn: 0},
	{hi: 0xe60f879fe7e2e1e5, lo: 0x7613b68f6ab03130, ex: 0, sgn: 0},
	{hi: 0xe5b7105006d4c560, lo: 0x9b695cd67c93bd79, ex: 0, sgn: 0},
	{hi: 0xe55e0b4d05c80388, lo: 0x5a7c210a3a15e7ea, ex: 0, sgn: 0},
	{hi: 0xe50478cdce2246bc, lo: 0xe1f5a58c80292554, ex: 0, sgn: 0},
	{hi: 0xe4aa5909a08fa7b4, lo: 0x122785ae67f5515d, ex: 0, sgn: 0},
	{hi: 0xe44fac3814e09856, lo: 0x20d63b5b9e3cd6ac, ex: 0, sgn: 0},
	{hi: 0xe3f4729119e798d9, lo: 0x56992551ae074e99, ex: 0, sgn: 0},
	{hi: 0xe398ac4cf556b732, lo: 0xd1197dc12c63176, ex: 0, sgn: 0},
	{hi: 0xe33c59a4439cd8ec, lo: 0x36563e2ffad8351a, ex: 0, sgn: 0},
	{hi: 0xe2df7acff7c2cf83, lo: 0xd6fe4dd22e60a4a2, ex: 0, sgn: 0},
	{hi: 0xe28210095b483751, lo: 0xfd39138aa2d508ed, ex: 0, sgn: 0},
	{hi: 0xe224198a0e002123, lo: 0xe0521df01a1be6f5, ex: 0, sgn: 0},
	{hi: 0xe1c5978c05ed8691, lo: 0xf4e8a8372f8c5810, ex: 0, sgn: 0},
	{hi: 0xe1668a498f1f892c, lo: 0xe2f9d4600f4d0325, ex: 0, sgn: 0},
	{hi: 0xe106f1fd4b8d7c96, lo: 0x6ba8a9d9ba877899, ex: 0, sgn: 0},
	{hi: 0xe0a6cee232f2bb9c, lo: 0x6d6c98fe79817946, ex: 0, sgn: 0},
	{hi: 0xe046213392aa486c, lo: 0x55ff6038a5197367, ex: 0, sgn: 0},
	{hi: 0xdfe4e92d0d8a37f5, lo: 0x720588ff6547d884, ex: 0, sgn: 0},
	{hi: 0xdf83270a9bbee890, lo: 0xab01350f013d78dd, ex: 0, sgn: 0},
	{hi: 0xdf20db088aa60404, lo: 0x64a58b2f103485dd, ex: 0, sgn: 0},
	{hi: 0xdebe05637ca94cfb, lo: 0x4b19aa71fec3ae6d, ex: 0, sgn: 0},
	{hi: 0xde5aa65869193805, lo: 0x4248f15548f69ca, ex: 0, sgn: 0},
	{hi: 0xddf6be249c075037, lo: 0xd597b10a01676659, ex: 0, sgn: 0},
	{hi: 0xdd924d05b620678a, lo: 0x739c45b982193b5e, ex: 0, sgn: 0},
	{hi: 0xdd2d5339ac8692fd, lo: 0x49c6e0ea76cbcaac, ex: 0, sgn: 0},
	{hi: 0xdcc7d0fec8aaf2aa, lo: 0xb2069fd0b482b4e8, ex: 0, sgn: 0},
	{hi: 0xdc61c693a82745d5, lo: 0xaca8017e375b64e5, ex: 0, sgn: 0},
	{hi: 0xdbfb34373c974b0e, lo: 0xccb7fd40d543f4a1, ex: 0, sgn: 0},
	{hi: 0xdb941a28cb71ec87, lo: 0x2c19b63253da43fc, ex: 0, sgn: 0},
	{hi: 0xdb2c78a7ede238a9, lo: 0x5a98479cbef2ecbc, ex: 0, sgn: 0},
	{hi: 0xdac44ff490a02710, lo: 0x5b267c1bcff0ab62, ex: 0, sgn: 0},
	{hi: 0xda5ba04ef3c929f4, lo: 0xe257bde73d83dc1a, ex: 0, sgn: 0},
	{hi: 0xd9f269f7aab88c29, lo: 0x28e81dcb6dab91ac, ex: 0, sgn: 0},
	{hi: 0xd988ad2f9bdf9bbb, lo: 0xc4e4dc69fc2fff6f, ex: 0, sgn: 0},
	{hi: 0xd91e6a38009da15a, lo: 0x1bb35ad6d2e74b67, ex: 0, sgn: 0},
	{hi: 0xd8b3a1526517a48b, lo: 0x1ed1a8ff78f1b632, ex: 0, sgn: 0},
	{hi: 0xd84852c0a80ffcdb, lo: 0x24b9fe00663574a4, ex: 0, sgn: 0},
	{hi: 0xd7dc7ec4fabdb011, lo: 0xced12d2899b803db, ex: 0, sgn: 0},
	{hi: 0xd77025a1e0a39d8b, lo: 0xcb78e80e67ba1b8, ex: 0, sgn: 0},
	{hi: 0xd703479a2f6776cc, lo: 0x6cb3bfd65b38562b, ex: 0, sgn: 0},
	{hi: 0xd695e4f10ea88570, lo: 0x83f082b570611d7, ex: 0, sgn: 0},
	{hi: 0xd627fde9f7d63e7e, lo: 0x7afbefc05e9f7d99, ex: 0, sgn: 0},
	{hi: 0xd5b992c8b606a351, lo: 0x7190b755535d4f18, ex: 0, sgn: 0},
	{hi: 0xd54aa3d165cc7018, lo: 0x7d00ae97abaa4096, ex: 0, sgn: 0},
	{hi: 0xd4db3148750d1819, lo: 0xf630e8b6dac83e69, ex: 0, sgn: 0},
	{hi: 0xd46b3b72a2d68fc9, lo: 0xdc4663a3168698d2, ex: 0, sgn: 0},
	{hi: 0xd3fac294ff34e4d0, lo: 0xb77d4f6bd0ee8591, ex: 0, sgn: 0},
	{hi: 0xd389c6f4eb07a41c, lo: 0xa8faac741a6394dc, ex: 0, sgn: 0},
	{hi: 0xd31848d817d70e16, lo: 0xeeeaddb72f00e0dd, ex: 0, sgn: 0},
	{hi: 0xd2a6488487a91918, lo: 0x4300fd1c1ce507e5, ex: 0, sgn: 0},
	{hi: 0xd233c6408cd64236, lo: 0x981ba7e42537275f, ex: 0, sgn: 0},
	{hi: 0xd1c0c252c9de2c86, lo: 0xda7485a5aeffeb4c, ex: 0, sgn: 0},
	{hi: 0xd14d3d02313c0eed, lo: 0x744fea20e8abef92, ex: 0, sgn: 0},
	{hi: 0xd0d93696053af098, lo: 0x77a18eb13d2ecde5, ex: 0, sgn: 0},
	{hi: 0xd064af55d7c9b43e, lo: 0x6b8a685f6cb61c21, ex: 0, sgn: 0},
	{hi: 0xcfefa7898a4ef23c, lo: 0xdaf200dd81212d10, ex: 0, sgn: 0},
	{hi: 0xcf7a1f794d7ca1b1, lo: 0xdfcb60445c1bf973, ex: 0, sgn: 0},
	{hi: 0xcf04176da12390ac, lo: 0x4d27090f10c454e, ex: 0, sgn: 0},
	{hi: 0xce8d8faf5406ab8b, lo: 0xf5babff66def7892, ex: 0, sgn: 0},
	{hi: 0xce16888783ae13b3, lo: 0x93e391861a034684, ex: 0, sgn: 0},
	{hi: 0xcd9f023f9c3a059e, lo: 0x23af31db7179a4aa, ex: 0, sgn: 0},
	{hi: 0xcd26fd2158358e7d, lo: 0x649474e36b8db9d3, ex: 0, sgn: 0},
	{hi: 0xccae7976c0691177, lo: 0x83e907fbd7aaf0b0, ex: 0, sgn: 0},
	{hi: 0xcc35778a2bac9ca1, lo: 0xf839ce18e08bfb50, ex: 0, sgn: 0},
	{hi: 0xcbbbf7a63eba0dd5, lo: 0x70cbb7f3343451be, ex: 0, sgn: 0},
	{hi: 0xcb41fa15ebff0777, lo: 0x2293661be51140ab, ex: 0, sgn: 0},
	{hi: 0xcac77f24736eb553, lo: 0xd9944be1631846d8, ex: 0, sgn: 0},
	{hi: 0xca4c871d625361a9, lo: 0x5328edeb3e6784de, ex: 0, sgn: 0},
	{hi: 0xc9d1124c931fda7a, lo: 0x8335241be1693225, ex: 0, sgn: 0},
	{hi: 0xc95520fe2d40a74b, lo: 0x83b0e96e1249c2b0, ex: 0, sgn: 0},
	{hi: 0xc8d8b37ea4ed0f62, lo: 0xb562c00b34ee771, ex: 0, sgn: 0},
	{hi: 0xc85bca1abaf7f0a7, lo: 0x65862939b83382e0, ex: 0, sgn: 0},
	{hi: 0xc7de651f7ca06749, lo: 0x2b31bc86877fd2c, ex: 0, sgn: 0},
	{hi: 0xc76084da43624634, lo: 0xd5c149509e9059f1, ex: 0, sgn: 0},
	{hi: 0xc6e22998b4c6608e, lo: 0xcfe6c1b1a6b4e2a4, ex: 0, sgn: 0},
	{hi: 0xc66353a8c232a43c, lo: 0xe993503baf5afb41, ex: 0, sgn: 0},
	{hi: 0xc5e40358a8ba05a7, lo: 0x43da25d99267326b, ex: 0, sgn: 0},
	{hi: 0xc56438f6f0ec3cca, lo: 0xab4906075507e74, ex: 0, sgn: 0},
	{hi: 0xc4e3f4d26ea553b6, lo: 0xdd40950cf1ed92fa, ex: 0, sgn: 0},
	{hi: 0xc463373a40dd06a3, lo: 0x9dd768f30ca8e85c, ex: 0, sgn: 0},
	{hi: 0xc3e2007dd175f5a4, lo: 0xa87e78136665cdb2, ex: 0, sgn: 0},
	{hi: 0xc36050ecd50ca830, lo: 0x8ac9e1386e4cbabb, ex: 0, sgn: 0},
	{hi: 0xc2de28d74ac6628b, lo: 0x74c8f010d986a9e0, ex: 0, sgn: 0},
	{hi: 0xc25b888d7c1fcd38, lo: 0xb7041e9bc8c18b0d, ex: 0, sgn: 0},
	{hi: 0xc1d8705ffcbb6e90, lo: 0xbdf0715cb8b20bd7, ex: 0, sgn: 0},
	{hi: 0xc154e09faa2ff69a, lo: 0x17858573216e0a22, ex: 0, sgn: 0},
	{hi: 0xc0d0d99dabd65d44, lo: 0x2bda5328933c854a, ex: 0, sgn: 0},
	{hi: 0xc04c5bab7297d322, lo: 0x6dd06968e0ed1957, ex: 0, sgn: 0},
	{hi: 0xbfc7671ab8bb84c6, lo: 0xe4e62d86dd136e78, ex: 0, sgn: 0},
	{hi: 0xbf41fc3d81b430db, lo: 0xd46655d6b012455, ex: 0, sgn: 0},
	{hi: 0xbebc1b6619ed9116, lo: 0x2715ef03f8543355, ex: 0, sgn: 0},
	{hi: 0xbe35c4e716999630, lo: 0x29d7f7b67d43b177, ex: 0, sgn: 0},
	{hi: 0xbdaef913557d76f0, lo: 0xac85320f528d6d5d, ex: 0, sgn: 0},
	{hi: 0xbd27b83dfcbe9279, lo: 0x2ea36923d5d8e213, ex: 0, sgn: 0},
	{hi: 0xbca002ba7aaf25ea, lo: 0x4a48496734be336d, ex: 0, sgn: 0},
	{hi: 0xbc17d8dc859ad583, lo: 0x727c405ffc73af56, ex: 0, sgn: 0},
	{hi: 0xbb8f3af81b93095c, lo: 0xfce8d84068e825b6, ex: 0, sgn: 0},
	{hi: 0xbb062961823b1ddc, lo: 0x5120e35e1c1a250c, ex: 0, sgn: 0},
	{hi: 0xba7ca46d46946802, lo: 0x33201477347447d8, ex: 0, sgn: 0},
	{hi: 0xb9f2ac703cca0db3, lo: 0x39db32d014440024, ex: 0, sgn: 0},
	{hi: 0xb96841bf7ffcb21a, lo: 0x9de1e3b22b8bf4db, ex: 0, sgn: 0},
	{hi: 0xb8dd64b0720df647, lo: 0xa726f4f0828585c9, ex: 0, sgn: 0},
	{hi: 0xb8521598bb6bce26, lo: 0x1c041d1ea5fb3fdb, ex: 0, sgn: 0},
	{hi: 0xb7c654ce4adba9f2, lo: 0x2e7a35723f3ed035, ex: 0, sgn: 0},
	{hi: 0xb73a22a755457448, lo: 0x7f86f63bb23f496a, ex: 0, sgn: 0},
	{hi: 0xb6ad7f7a557e64f2, lo: 0xeb2d28ef943dc88c, ex: 0, sgn: 0},
	{hi: 0xb6206b9e0c13a892, lo: 0xea7c015f12b987f7, ex: 0, sgn: 0},
	{hi: 0xb592e7697f14dd4a, lo: 0x737dd2824b608d13, ex: 0, sgn: 0},
}

var tanPS = [...]dint{
	{hi: 0xc90fdaa22168c234, lo: 0xc4c6628b80dc1cd1, ex: 3, sgn: 0},
	{hi: 0xa55de7312df295f5, lo: 0x5dc72f712aa57db4, ex: 6, sgn: 1},
	{hi: 0xa335e33bad570e92, lo: 0x3f33be0021aa54d2, ex: 7, sgn: 0},
	{hi: 0x9969667315ec2d9d, lo: 0xe59d6ab8509a2025, ex: 7, sgn: 1},
	{hi: 0xa83c1a43bf1c6485, lo: 0x7d5f8f76fa7d74ed, ex: 6, sgn: 0},
	{hi: 0xf16ab2898eae62f9, lo: 0xa7f0339113b8b3c5, ex: 4, sgn: 1},
}

var tanPC = [...]dint{
	{hi: 0x8000000000000000, lo: 0x0, ex: 1, sgn: 0},
	{hi: 0x9de9e64df22ef2d2, lo: 0x56e26cd9808c1949, ex: 5, sgn: 1},
	{hi: 0x81e0f840dad61d9a, lo: 0x9980f00630cb655e, ex: 7, sgn: 0},
	{hi: 0xaae9e3f1e5ffcfe2, lo: 0xa508509534006249, ex: 7, sgn: 1},
	{hi: 0xf0fa83448dd1e094, lo: 0xe0603ce7044eeba, ex: 6, sgn: 0},
	{hi: 0xd368f6f4207cfe49, lo: 0xec63157807ebffa, ex: 5, sgn: 1},
}

var tanSC = [256][3]float64{
	{0x0p+0, 0x0p+0, 0x1p+0},
	{-0x1.c0f6cp-35, 0x1.921f892b900fep-9, 0x1.ffff621623fap-1},
	{-0x1.9c7935ep-35, 0x1.921f0ea27ce01p-8, 0x1.fffd8858eca2ep-1},
	{-0x1.d14d1acp-34, 0x1.2d96af779b0bbp-7, 0x1.fffa72c986392p-1},
	{-0x1.dba8f6a8p-33, 0x1.921d1ce2d0a1cp-7, 0x1.fff62169dddaap-1},
	{0x1.a6b7cdfp-32, 0x1.f6a29bdb7377p-7, 0x1.fff0943c02419p-1},
	{0x1.b49618dp-33, 0x1.2d936d1506f3dp-6, 0x1.ffe9cb44829cp-1},
	{-0x1.398d6fcp-35, 0x1.5fd4d1e21de6dp-6, 0x1.ffe1c687174b1p-1},
	{-0x1.e9e9a8c8p-31, 0x1.9215597791e0ap-6, 0x1.ffd886097afcfp-1},
	{-0x1.34e844cp-32, 0x1.c454f2e9480c7p-6, 0x1.ffce09ce95933p-1},
	{-0x1.989a8a4p-32, 0x1.f693709b94f92p-6, 0x1.ffc251dfbac0cp-1},
	{0x1.04a9b99p-30, 0x1.146860e69a571p-5, 0x1.ffb55e40a5c43p-1},
	{-0x1.56947cp-36, 0x1.2d865748774adp-5, 0x1.ffa72efff95d1p-1},
	{-0x1.c348768p-35, 0x1.46a396d34121ap-5, 0x1.ff97c420a8451p-1},
	{0x1.9e80552p-32, 0x1.5fc00e6e4c65cp-5, 0x1.ff871dacd8761p-1},
	{0x1.3f11d74p-34, 0x1.78dbaa97099ebp-5, 0x1.ff753bb18af95p-1},
	{0x1.c039af4p-33, 0x1.91f65fc0abc0ap-5, 0x1.ff621e370ca7ap-1},
	{0x1.53e1f8p-35, 0x1.ab101bf74ac2ep-5, 0x1.ff4dc54b00181p-1},
	{0x1.114a649p-29, 0x1.c428d7de920e9p-5, 0x1.ff3830f2e9043p-1},
	{0x1.adf0ef4p-31, 0x1.dd40723a3cdfbp-5, 0x1.ff21614b9d9adp-1},
	{-0x1.d21f5918p-30, 0x1.f656e1e9e59cdp-5, 0x1.ff09565e83d77p-1},
	{-0x1.4f54d708p-30, 0x1.07b612d6be078p-4, 0x1.fef0102c634e3p-1},
	{-0x1.1efec9ap-30, 0x1.1440118ba7bdp-4, 0x1.fed58ecf342dap-1},
	{0x1.cc17ba88p-29, 0x1.20c96cf0a7eedp-4, 0x1.feb9d24646fa6p-1},
	{0x1.121dbe4p-33, 0x1.2d5209628edfp-4, 0x1.fe9cdacf99cffp-1},
	{-0x1.9ecf61p-34, 0x1.39d9f103bf7f7p-4, 0x1.fe7ea854e6b08p-1},
	{-0x1.04ede8ep-31, 0x1.466116c629e5cp-4, 0x1.fe5f3af4ee201p-1},
	{-0x1.1821cecp-31, 0x1.52e773c9920c7p-4, 0x1.fe3e92c0e4108p-1},
	{0x1.cdec726p-31, 0x1.5f6d02131f0b2p-4, 0x1.fe1cafc7f1a24p-1},
	{-0x1.edece4dp-31, 0x1.6bf1b2653648cp-4, 0x1.fdf99233c230cp-1},
	{-0x1.2aa4d1cp-31, 0x1.787585bc45f0fp-4, 0x1.fdd53a01d11d9p-1},
	{0x1.d461592p-32, 0x1.84f871e32cf68p-4, 0x1.fdafa74f16482p-1},
	{0x1.f0cbd728p-29, 0x1.917a71d3d2956p-4, 0x1.fd88da29f302ep-1},
	{-0x1.583247p-30, 0x1.9dfb6c9865b06p-4, 0x1.fd60d2e14a6b1p-1},
	{-0x1.2e81bf4p-30, 0x1.aa7b706bfdbbap-4, 0x1.fd3791484ff5p-1},
	{-0x1.13941418p-28, 0x1.b6fa680a05c27p-4, 0x1.fd0d15a4b8471p-1},
	{0x1.71098ffp-30, 0x1.c3785eba12b42p-4, 0x1.fce15fceddccfp-1},
	{-0x1.c3519e8p-32, 0x1.cff53302f059p-4, 0x1.fcb4703b969e1p-1},
	{0x1.2f522a5p-27, 0x1.dc70fb84af16ep-4, 0x1.fc8646987fc1dp-1},
	{-0x1.ae9bed8p-33, 0x1.e8eb7f8a589e2p-4, 0x1.fc56e3b91ca3ap-1},
	{0x1.f8868b2p-30, 0x1.f564e87d2330fp-4, 0x1.fc264701f9a09p-1},
	{-0x1.b07985f8p-29, 0x1.00ee8835051f4p-3, 0x1.fbf47105f7439p-1},
	{0x1.cbdaa94p-30, 0x1.072a05e1d4d8ep-3, 0x1.fbc16172a9e36p-1},
	{0x1.37c5b908p-28, 0x1.0d64df9619f0dp-3, 0x1.fb8d18b635327p-1},
	{-0x1.068b5fc8p-28, 0x1.139f09bc617f5p-3, 0x1.fb5797351da85p-1},
	{-0x1.8ea66818p-29, 0x1.19d8919fa4ec8p-3, 0x1.fb20dc7da8affp-1},
	{0x1.6278ceb8p-28, 0x1.2011719d50b87p-3, 0x1.fae8e8bd4427fp-1},
	{-0x1.096df84p-29, 0x1.264993433763ap-3, 0x1.faafbcbfca356p-1},
	{0x1.9b2534fp-29, 0x1.2c810967bbf7p-3, 0x1.fa7557d8d987ep-1},
	{0x1.215b4ep-34, 0x1.32b7bfa25c91bp-3, 0x1.fa39bac71954bp-1},
	{-0x1.94db891p-30, 0x1.38edb9d29b39dp-3, 0x1.f9fce56700a6dp-1},
	{0x1.7727f7b8p-29, 0x1.3f22f7c3cce3ap-3, 0x1.f9bed7b8c8d8cp-1},
	{-0x1.0cb33038p-29, 0x1.45576971dd53p-3, 0x1.f97f925d53c83p-1},
	{-0x1.9071106p-31, 0x1.4b8b175c71e22p-3, 0x1.f93f14feb8022p-1},
	{0x1.62741e78p-29, 0x1.51bdfa7ea30d5p-3, 0x1.f8fd5fe3efac8p-1},
	{0x1.f8e16d0cp-28, 0x1.57f00e80e6e12p-3, 0x1.f8ba733a1ceb1p-1},
	{-0x1.76acbcap-31, 0x1.5e2143b7bc1c2p-3, 0x1.f8764fad5e9bfp-1},
	{-0x1.0a0f73ap-30, 0x1.6451a76411746p-3, 0x1.f830f4ad232d8p-1},
	{0x1.ca11d1bcp-28, 0x1.6a8135d7bd143p-3, 0x1.f7ea625eb5af7p-1},
	{-0x1.02f23628p-29, 0x1.70afd74071191p-3, 0x1.f7a299d3f182ap-1},
	{0x1.b34dcb8p-29, 0x1.76dda08544b5cp-3, 0x1.f7599a1ac7ecdp-1},
	{0x1.161ff4p-32, 0x1.7d0a7bf2d4abap-3, 0x1.f70f64322da74p-1},
	{-0x1.c49b8b4p-31, 0x1.83366ddb3de23p-3, 0x1.f6c3f7e7c2707p-1},
	{0x1.21da851p-29, 0x1.8961743b1429p-3, 0x1.f6775552a6ba2p-1},
	{0x1.ac63edap-30, 0x1.8f8b851098588p-3, 0x1.f6297cef0cdd6p-1},
	{0x1.27ef489cp-27, 0x1.95b4a5b9f2cebp-3, 0x1.f5da6e7820551p-1},
	{0x1.ae8937p-30, 0x1.9bdcc07900146p-3, 0x1.f58a2b0689c82p-1},
	{0x1.eb48c7ep-29, 0x1.a203e4a4f950ep-3, 0x1.f538b1d392049p-1},
	{-0x1.bfd282fp-29, 0x1.a829ffaad0d79p-3, 0x1.f4e603d51f1aap-1},
	{0x1.7ccf638p-29, 0x1.ae4f1fa80e1b5p-3, 0x1.f492204c5ef9ep-1},
	{-0x1.2435c578p-28, 0x1.b4732b72ebc86p-3, 0x1.f43d0890e1e72p-1},
	{0x1.0293fecp-30, 0x1.ba9634155f866p-3, 0x1.f3e6bbb6c2ea4p-1},
	{-0x1.7bb1f92p-29, 0x1.c0b82461f65ep-3, 0x1.f38f3ae6f9afcp-1},
	{0x1.27aaebcp-29, 0x1.c6d906faacf65p-3, 0x1.f3368589e17a2p-1},
	{-0x1.2e2bcd5p-27, 0x1.ccf8c3f74a6c9p-3, 0x1.f2dc9cfb5fa74p-1},
	{-0x1.6f070acp-30, 0x1.d31773ba218a8p-3, 0x1.f2817fd4d045bp-1},
	{0x1.469adfcp-29, 0x1.d935004779e57p-3, 0x1.f2252f59c122dp-1},
	{0x1.4f51c18p-32, 0x1.df5164301377ap-3, 0x1.f1c7abdeaa3efp-1},
	{0x1.78e44dap-29, 0x1.e56ca4202807cp-3, 0x1.f168f51c5d5d5p-1},
	{0x1.49bb5f8p-32, 0x1.eb86b4a1b7e9bp-3, 0x1.f1090bc4b68p-1},
	{-0x1.67ba541p-28, 0x1.f19f9369d5e93p-3, 0x1.f0a7effdc937fp-1},
	{0x1.c0cab95p-29, 0x1.f7b74ab7219d2p-3, 0x1.f045a1219e594p-1},
	{-0x1.2b77e32p-30, 0x1.fdcdc0ca3288dp-3, 0x1.efe220cf5c751p-1},
	{-0x1.e0d8cbp-33, 0x1.01f18054c8362p-2, 0x1.ef7d6e54c347dp-1},
	{-0x1.ecd5b9cp-29, 0x1.04fb7f6d35d68p-2, 0x1.ef178a6f9a987p-1},
	{0x1.eb24de5p-29, 0x1.0804e1d369ff2p-2, 0x1.eeb074934fdfp-1},
	{0x1.4a897c4p-30, 0x1.0b0d9d7b0d042p-2, 0x1.ee482e14bcdep-1},
	{0x1.336c376p-30, 0x1.0e15b555e7becp-2, 0x1.eddeb6908ca8cp-1},
	{-0x1.3952d9p-31, 0x1.111d25efd48b8p-2, 0x1.ed740e7eb8dd6p-1},
	{0x1.fc2a5d4p-31, 0x1.1423ef5c7e1bdp-2, 0x1.ed0835dc24e89p-1},
	{0x1.a88ed37p-29, 0x1.172a0eb8361dap-2, 0x1.ec9b2d0ec8288p-1},
	{-0x1.8ca4cb94p-27, 0x1.1a2f7b10b6d7p-2, 0x1.ec2cf55d6117cp-1},
	{0x1.0144524p-27, 0x1.1d3446fd0cd3fp-2, 0x1.ebbd8c1d62f96p-1},
	{-0x1.abf810cp-28, 0x1.203855b85f89ap-2, 0x1.eb4cf57454132p-1},
	{0x1.5d4c5d58p-28, 0x1.233bbcca40561p-2, 0x1.eadb2e40746cap-1},
	{-0x1.a1b0c58p-29, 0x1.263e685b1d714p-2, 0x1.ea68396d87754p-1},
	{-0x1.77c8dacp-29, 0x1.294061d2eb611p-2, 0x1.e9f41597393c8p-1},
	{0x1.915540ep-30, 0x1.2c41a580014cfp-2, 0x1.e97ec348fb87fp-1},
	{-0x1.abb6d9bp-28, 0x1.2f422b2d0990cp-2, 0x1.e90843c55b996p-1},
	{-0x1.b8ee5d58p-28, 0x1.3241f8cea2836p-2, 0x1.e890962268c49p-1},
	{-0x1.1cd29828p-28, 0x1.35410a8396266p-2, 0x1.e817baf85c094p-1},
	{-0x1.e216afp-32, 0x1.383f5e08283e2p-2, 0x1.e79db2a188b0ap-1},
	{-0x1.24afc3p-31, 0x1.3b3cef6993c0bp-2, 0x1.e7227dbf82004p-1},
	{-0x1.aa1657cp-31, 0x1.3e39be4767224p-2, 0x1.e6a61c62d5274p-1},
	{-0x1.c5b65fap-30, 0x1.4135c898485bbp-2, 0x1.e6288ee07fea5p-1},
	{0x1.23e8978p-32, 0x1.44310de3c284bp-2, 0x1.e5a9d54bbd26cp-1},
	{-0x1.2b1d77ap-29, 0x1.472b8976d498dp-2, 0x1.e529f06cb187dp-1},
	{-0x1.daaa348p-31, 0x1.4a253cb97efd1p-2, 0x1.e4a8e007231a2p-1},
	{-0x1.322f5708p-28, 0x1.4d1e2260c3422p-2, 0x1.e426a500f6e33p-1},
	{0x1.64758e8p-29, 0x1.50163eca0b337p-2, 0x1.e3a33e996b722p-1},
	{0x1.12486278p-28, 0x1.530d89a17e007p-2, 0x1.e31eae3fb917bp-1},
	{-0x1.6c3416ccp-27, 0x1.5603fcf8cd8a3p-2, 0x1.e298f502a579bp-1},
	{0x1.ab481ffp-29, 0x1.58f9a896aa209p-2, 0x1.e2121016e14fcp-1},
	{-0x1.6eb838bp-29, 0x1.5bee77aaf890bp-2, 0x1.e18a032eb4df5p-1},
	{-0x1.d159b8p-32, 0x1.5ee2734efeef5p-2, 0x1.e100ccaa6bd78p-1},
	{-0x1.a42e4ap-34, 0x1.61d595bedeabcp-2, 0x1.e0766d944915ep-1},
	{-0x1.43d0dcp-30, 0x1.64c7dd5cc0cd1p-2, 0x1.dfeae63903034p-1},
	{-0x1.8c7bdb7p-27, 0x1.67b9453ca2122p-2, 0x1.df5e378482eaep-1},
	{0x1.1c0ead6p-30, 0x1.6aa9d844c980ap-2, 0x1.ded05f6a23a52p-1},
	{0x1.7d526p-31, 0x1.6d99867e90d92p-2, 0x1.de4160e97b2e2p-1},
	{0x1.924e0368p-28, 0x1.7088555d3c816p-2, 0x1.ddb13afb14e37p-1},
	{-0x1.74b7c3ep-30, 0x1.73763c09fba09p-2, 0x1.dd1fef5335416p-1},
	{-0x1.7943adp-30, 0x1.766340685c982p-2, 0x1.dc8d7ccf2567ap-1},
	{0x1.79dd614p-29, 0x1.794f5f7522b88p-2, 0x1.dbf9e402aa5c3p-1},
	{0x1.7b64f32p-30, 0x1.7c3a939c32d81p-2, 0x1.db652607e0db1p-1},
	{-0x1.2bea5ce8p-28, 0x1.7f24db825141cp-2, 0x1.dacf43268b5bp-1},
	{0x1.733c024p-30, 0x1.820e3b8bf15ap-2, 0x1.da383a7aed887p-1},
	{-0x1.eac0fc94p-27, 0x1.84f6a51d077b3p-2, 0x1.d9a00efd84537p-1},
	{0x1.aca37338p-27, 0x1.87de2f4704f98p-2, 0x1.d906bbf17f4dap-1},
	{-0x1.910c4fp-30, 0x1.8ac4b7dc0d986p-2, 0x1.d86c4862b5d6ep-1},
	{-0x1.33bb86p-31, 0x1.8daa52b4dc041p-2, 0x1.d7d0b0374a559p-1},
	{-0x1.69e1507p-27, 0x1.908ef408ad22p-2, 0x1.d733f5e71c3bcp-1},
	{0x1.cffacf08p-27, 0x1.9372ab7784d36p-2, 0x1.d696161d786c9p-1},
	{-0x1.8629d9fp-26, 0x1.965552b0849abp-2, 0x1.d5f7190eeae23p-1},
	{0x1.415p-30, 0x1.99371687c64f3p-2, 0x1.d556f5155d9ddp-1},
	{-0x1.bd37aad8p-27, 0x1.9c17cf40715cbp-2, 0x1.d4b5b2caf8386p-1},
	{0x1.d02cde7p-26, 0x1.9ef79ea4d995dp-2, 0x1.d4134ac5eb246p-1},
	{-0x1.10547acp-30, 0x1.a1d653d9adf5ep-2, 0x1.d36fc7d291602p-1},
	{-0x1.01a1a228p-27, 0x1.a4b40f9c0120bp-2, 0x1.d2cb22b45236bp-1},
	{0x1.3ce2bacp-29, 0x1.a790ce2056b9ap-2, 0x1.d2255c3ae11a5p-1},
	{-0x1.ccb4a6p-32, 0x1.aa6c828db4ea8p-2, 0x1.d17e774d4e3e2p-1},
	{0x1.5db4bp-29, 0x1.ad47321f29847p-2, 0x1.d0d672bc0b122p-1},
	{0x1.32f6a6ep-29, 0x1.b020d7a285e23p-2, 0x1.d02d4fb84d334p-1},
	{0x1.cf8e39bcp-26, 0x1.b2f97c27f7494p-2, 0x1.cf830c2248c5ep-1},
	{0x1.8927bbp-30, 0x1.b5d10129a750ap-2, 0x1.ced7af22cb105p-1},
	{-0x1.3dec3c1p-28, 0x1.b8a77f8d0bbc5p-2, 0x1.ce2b32e50d6cdp-1},
	{-0x1.26ba536p-28, 0x1.bb7cf08f0290dp-2, 0x1.cd7d98fcf3b1ep-1},
	{0x1.23c568ep-29, 0x1.be51524e3aa53p-2, 0x1.cccee1da3d56ep-1},
	{-0x1.f3b3afp-29, 0x1.c1249c1f5f2f6p-2, 0x1.cc1f0f95e1e24p-1},
	{-0x1.1286a47p-28, 0x1.c3f6d2ef7054bp-2, 0x1.cb6e20ff37e81p-1},
	{0x1.641214ep-29, 0x1.c6c7f594003d9p-2, 0x1.cabc165bf1b6p-1},
	{0x1.0cda7c9p-27, 0x1.c997ff2bffccbp-2, 0x1.ca08f0dee434cp-1},
	{-0x1.5557ac9p-28, 0x1.cc66e7b42e8f1p-2, 0x1.c954b28bca62ep-1},
	{0x1.555eb62p-28, 0x1.cf34bccc567a1p-2, 0x1.c89f57f6e20f3p-1},
	{-0x1.4e0e361p-28, 0x1.d2016cbb5e39ap-2, 0x1.c7e8e59999e1fp-1},
	{0x1.446da1ep-29, 0x1.d4cd039d0ed05p-2, 0x1.c731585f970ebp-1},
	{0x1.103d328p-29, 0x1.d797767638decp-2, 0x1.c678b3174afe1p-1},
	{0x1.5814d6p-28, 0x1.da60c7ae9dc22p-2, 0x1.c5bef522be6fbp-1},
	{-0x1.5e2321ep-29, 0x1.dd28f054cbb3fp-2, 0x1.c5042052c8c42p-1},
	{-0x1.a259ffep-29, 0x1.dfeff54854631p-2, 0x1.c44833611bc7dp-1},
	{-0x1.4f28d8p-31, 0x1.e2b5d34665b35p-2, 0x1.c38b2f278ea7ep-1},
	{-0x1.de571p-36, 0x1.e57a86d137f2p-2, 0x1.c2cd1493d05c2p-1},
	{0x1.e0d8d14p-29, 0x1.e83e0ffb7bfb4p-2, 0x1.c20de3a08ea07p-1},
	{-0x1.12a858ep-28, 0x1.eb0067e48baf4p-2, 0x1.c14d9e2bd511ep-1},
	{0x1.9a17403p-27, 0x1.edc19997a4431p-2, 0x1.c08c413089b2ep-1},
	{0x1.68c8636p-29, 0x1.f0819163d1bcp-2, 0x1.bfc9d21568f32p-1},
	{0x1.4cc5eb8p-29, 0x1.f3405a482e11dp-2, 0x1.bf064dd580fc9p-1},
	{-0x1.fce7cd8p-27, 0x1.f5fde8f3f11d4p-2, 0x1.be41b798f6b97p-1},
	{-0x1.af8169p-29, 0x1.f8ba4c98a9816p-2, 0x1.bd7c0b1a7f14bp-1},
	{0x1.6e39e2p-33, 0x1.fb7575d1ea75p-2, 0x1.bcb54cac5dde5p-1},
	{0x1.30f9256p-28, 0x1.fe2f665dcd168p-2, 0x1.bbed7bd1e17bp-1},
	{0x1.626de2p-31, 0x1.00740ca0d5fbbp-1, 0x1.bb2499f9fe7a3p-1},
	{0x1.5cc703p-30, 0x1.01cfc8afeea0ep-1, 0x1.ba5aa650dd495p-1},
	{-0x1.6191e6p-32, 0x1.032ae54fe4057p-1, 0x1.b98fa2065a5e6p-1},
	{-0x1.6b1485p-31, 0x1.0485624c328c8p-1, 0x1.b8c38d39737bcp-1},
	{-0x1.11fbc3ap-29, 0x1.05df3e66a716dp-1, 0x1.b7f668a580fdp-1},
	{-0x1.0eca7fp-27, 0x1.07387825589ecp-1, 0x1.b728352c44517p-1},
	{-0x1.8073bc9ep-25, 0x1.089109ef1284dp-1, 0x1.b658f630112edp-1},
	{-0x1.9dcf0adp-27, 0x1.09e9051603e29p-1, 0x1.b588a13ab750fp-1},
	{-0x1.06ea9fp-29, 0x1.0b405820e78e7p-1, 0x1.b4b740d3cc07bp-1},
	{-0x1.36a8d0cp-30, 0x1.0c9704a1ea4e5p-1, 0x1.b3e4d40f5524dp-1},
	{0x1.63d1f3p-30, 0x1.0ded0bc01a533p-1, 0x1.b3115a3a628afp-1},
	{0x1.f3181f14p-26, 0x1.0f4270e4787bfp-1, 0x1.b23cd1314c779p-1},
	{-0x1.f269b78p-29, 0x1.109723e75c5cfp-1, 0x1.b167430cfebdbp-1},
	{0x1.1d84dc08p-27, 0x1.11eb36bc9db52p-1, 0x1.b090a4915ee88p-1},
	{-0x1.08e60068p-27, 0x1.133e9ba0061d8p-1, 0x1.afb8fe69a6527p-1},
	{0x1.cda72abp-27, 0x1.14915d557a7c9p-1, 0x1.aee049bc0aeep-1},
	{-0x1.f32f95p-30, 0x1.15e36dfb6bb55p-1, 0x1.ae068f6991699p-1},
	{0x1.138092dp-28, 0x1.1734d6f34d7fp-1, 0x1.ad2bc96c1e1f5p-1},
	{0x1.6b382dd4p-26, 0x1.188595ae376a5p-1, 0x1.ac4ff962bdb6dp-1},
	{-0x1.f12fafap-28, 0x1.19d59f592a587p-1, 0x1.ab7326685eb57p-1},
	{-0x1.2909e5ap-28, 0x1.1b2500aed7ac6p-1, 0x1.aa954823cf815p-1},
	{-0x1.d66a8978p-25, 0x1.1c73aa0150cf9p-1, 0x1.a9b668fb0503fp-1},
	{0x1.311ea86p-27, 0x1.1dc1b7db74db1p-1, 0x1.a8d675d9c6cc8p-1},
	{-0x1.41c02b8p-31, 0x1.1f0f08a1a06a4p-1, 0x1.a7f5853bb4309p-1},
	{-0x1.ca1f4edp-26, 0x1.205ba57211271p-1, 0x1.a71391146958fp-1},
	{-0x1.910ce77p-28, 0x1.21a7988f8326bp-1, 0x1.a63092626202fp-1},
	{0x1.2bfadbeep-25, 0x1.22f2dc71afab6p-1, 0x1.a54c8cd9fd0d9p-1},
	{-0x1.5f1c02a8p-27, 0x1.243d5df4afb93p-1, 0x1.a4678dbbe5e73p-1},
	{-0x1.db12b9p-30, 0x1.2587347f493a4p-1, 0x1.a38184db0df23p-1},
	{-0x1.7b29ep-30, 0x1.26d05490f2f61p-1, 0x1.a29a7a2f40b49p-1},
	{-0x1.b3ddca4p-29, 0x1.2818be6930629p-1, 0x1.a1b26d8f070d7p-1},
	{0x1.e112744p-29, 0x1.2960730ff2bcdp-1, 0x1.a0c95e3df5e0ep-1},
	{-0x1.5269766p-28, 0x1.2aa76dafcbbf4p-1, 0x1.9fdf4fae1df6fp-1},
	{-0x1.09777e1p-28, 0x1.2bedb1b6b4e15p-1, 0x1.9ef43f6cbe162p-1},
	{0x1.ae2051fp-28, 0x1.2d333e4617f25p-1, 0x1.9e082e148680ep-1},
	{-0x1.36f6ced8p-27, 0x1.2e780cb47180ep-1, 0x1.9d1b207f383c3p-1},
	{-0x1.23fdc6bp-28, 0x1.2fbc23fba2f44p-1, 0x1.9c2d1197130a7p-1},
	{0x1.bc540ep-33, 0x1.30ff7fd6d967dp-1, 0x1.9b3e0478b961bp-1},
	{-0x1.cfb4ed7p-28, 0x1.32421da0bf0e9p-1, 0x1.9a4dfb1c89326p-1},
	{0x1.55802aecp-26, 0x1.3384042a92b1dp-1, 0x1.995cf06920d11p-1},
	{0x1.60719e4p-28, 0x1.34c52608e3a92p-1, 0x1.986aee6d6837ep-1},
	{-0x1.cbf2e48p-30, 0x1.36058ac8863b6p-1, 0x1.9777ef832c986p-1},
	{0x1.9061c32p-27, 0x1.374533ab707dp-1, 0x1.9683f2ad7e2ecp-1},
	{-0x1.da84dfep-27, 0x1.3884160f9488fp-1, 0x1.958f000fdd50ap-1},
	{0x1.92e8a74p-29, 0x1.39c23eba6b22ap-1, 0x1.94990dd9cee51p-1},
	{-0x1.bff5d9ap-29, 0x1.3affa20756bddp-1, 0x1.93a225056084ap-1},
	{0x1.4c462p-36, 0x1.3c3c4498e98ebp-1, 0x1.92aa41fbb951cp-1},
	{-0x1.e4613e9p-28, 0x1.3d782261dff62p-1, 0x1.91b167e92d706p-1},
	{0x1.0eb2964p-30, 0x1.3eb33ed579bbep-1, 0x1.90b794146043cp-1},
	{-0x1.60abec2p-29, 0x1.3fed94c834d8ap-1, 0x1.8fbcca9583479p-1},
	{0x1.6954977p-27, 0x1.4127281ddac03p-1, 0x1.8ec1085083553p-1},
	{0x1.a16fec2p-29, 0x1.425ff1f841235p-1, 0x1.8dc452ca328d3p-1},
	{-0x1.27bcdd3p-27, 0x1.4397f44aa44f2p-1, 0x1.8cc6a8771e165p-1},
	{-0x1.60dded4p-28, 0x1.44cf317a563dbp-1, 0x1.8bc8076122736p-1},
	{-0x1.9a8f405cp-26, 0x1.4605a2b02d705p-1, 0x1.8ac875232f3efp-1},
	{0x1.32777dcp-27, 0x1.473b532bc5a67p-1, 0x1.89c7e8713120cp-1},
	{-0x1.1418a7bp-26, 0x1.4870306ca20e2p-1, 0x1.88c670a0ea774p-1},
	{-0x1.fed182ep-28, 0x1.49a44886b534p-1, 0x1.87c401fdf05e5p-1},
	{0x1.86144d8p-27, 0x1.4ad796ea1410cp-1, 0x1.86c0a04dbacc5p-1},
	{0x1.1bc2e6p-33, 0x1.4c0a14640d2afp-1, 0x1.85bc51aa114c2p-1},
	{-0x1.f53d2fep-28, 0x1.4d3bc5aaa8cd5p-1, 0x1.84b7121b30a13p-1},
	{-0x1.2e100ap-30, 0x1.4e6cab91556bep-1, 0x1.83b0e0e6b6cccp-1},
	{-0x1.fa58c62p-29, 0x1.4f9cc1c69fddep-1, 0x1.82a9c1c1ab463p-1},
	{0x1.bb491ep-33, 0x1.50cc09fdcbd92p-1, 0x1.81a1b3342f858p-1},
	{0x1.a11541p-28, 0x1.51fa82c3aa029p-1, 0x1.8098b67ea8509p-1},
	{0x1.ab0a5d3p-27, 0x1.53282b20b96b6p-1, 0x1.7f8ecc791953p-1},
	{-0x1.cba0438p-28, 0x1.5454fe43a7d7cp-1, 0x1.7e83f96af78ap-1},
	{-0x1.0dd83a4p-29, 0x1.5581033a81573p-1, 0x1.7d783712e20ecp-1},
	{-0x1.e9a8299p-28, 0x1.56ac33fbb8253p-1, 0x1.7c6b8acf90fa6p-1},
	{0x1.225c4aap-29, 0x1.57d6939d4b513p-1, 0x1.7b5df1da18065p-1},
	{-0x1.82e66ep-27, 0x1.59001b9e64d79p-1, 0x1.7a4f72157cfdfp-1},
	{0x1.51a6a354p-26, 0x1.5a28d5b36d597p-1, 0x1.794002a7c9023p-1},
	{0x1.13917f4p-26, 0x1.5b50b4e10bec1p-1, 0x1.782faf6dc7ba2p-1},
	{0x1.49310ccp-30, 0x1.5c77bc15ab4efp-1, 0x1.771e75c43942ep-1},
	{0x1.24d493cp-30, 0x1.5d9dee9de49dbp-1, 0x1.760c529bc17bp-1},
	{-0x1.04638f7p-26, 0x1.5ec347044e0f4p-1, 0x1.74f94b0af972p-1},
	{-0x1.3f41b28p-29, 0x1.5fe7cb834600cp-1, 0x1.73e55936a516p-1},
	{-0x1.a5f6f5cp-30, 0x1.610b7515d1562p-1, 0x1.72d083b8214ebp-1},
	{0x1.19fb2ep-28, 0x1.622e459eafbc1p-1, 0x1.71bac8c7b0592p-1},
	{-0x1.56d2c2bp-28, 0x1.6350396fe4e62p-1, 0x1.70a42bec51665p-1},
	{-0x1.3c156c2p-28, 0x1.64715385bed93p-1, 0x1.6f8caa4969708p-1},
	{-0x1.f23e576p-29, 0x1.659191d2fd57fp-1, 0x1.6e7445d74f711p-1},
	{0x1.1e4be38p-30, 0x1.66b0f41d484c4p-1, 0x1.6d5afecd4938dp-1},
	{-0x1.397cc8d8p-27, 0x1.67cf76eac73dfp-1, 0x1.6c40d89625f63p-1},
	{-0x1.202f686p-28, 0x1.68ed1e0990551p-1, 0x1.6b25cf728c35p-1},
}
