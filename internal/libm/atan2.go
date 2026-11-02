// Ported from CORE-MATH, src/binary64/atan2/atan2.c and tint.h, for glibc's
// atan2, which is IBM's. MIT licensed: see coremath.go.

package libm

import (
	"math"
	"math/bits"
)

const (
	piH      = 0x1.921fb54442d18p+1
	piL      = 0x1.1a62633145c07p-53
	piOver2H = 0x1.921fb54442d18p+0
	piOver2L = 0x1.1a62633145c07p-54
)

// Atan2 is atan2(y0, x0), correctly rounded: glibc's except where glibc is
// not.
func Atan2(y0, x0 float64) float64 {
	iy, ix := asuint64(y0), asuint64(x0)
	aiy := iy & (1<<63 - 1)
	if aiy == 0 || aiy >= 0x7ff<<52 {
		return atan2Special(y0, x0)
	}
	aix := ix & (1<<63 - 1)
	if aix == 0 || aix >= 0x7ff<<52 {
		return atan2Special(y0, x0)
	}
	ax, ay := math.Abs(x0), math.Abs(y0)
	x, y := math.Max(ax, ay), math.Min(ax, ay)
	sy, sx := iy>>63, ix>>63
	gt := b2u(aix < aiy)
	if (aix-aiy)^-gt >= 53<<52 {
		return atan2Accurate(y0, x0)
	}
	sgn := 1.0
	if gt^sx^sy != 0 {
		sgn = -1
	}
	kw := sx<<2 | sy<<1 | gt
	jt := int64(asuint64(y/x+(2+1/128.0))>>(52-7)) & 127
	fh := atan2F2[jt][1]*sgn + atan2O[kw][0]
	fl := atan2F2[jt][0]*sgn + atan2O[kw][1]
	if x < 0x1p-920 {
		x *= 0x1p920
		y *= 0x1p920
	}
	if x > 0x1p1022 && jt != 0 {
		x *= 0x1p-1
		y *= 0x1p-1
	}
	t0 := atan2T2[jt]
	zn := math.FMA(-t0, x, y)
	zd := math.FMA(t0, y, x)
	z := zn / zd
	z2 := z * z
	z *= sgn
	dz := (z * z2) * (-0x1.55555555554d2p-2 + z2*(0x1.999999860e1cap-3+z2*-0x1.248ad469844a1p-3))
	eps := math.Abs(z)*0x1.051p-51 + 0x1p-90
	rh, z := fastTwoSum(fh, z)
	rl := (fl + dz) + z
	lb, ub := rh+(rl-eps), rh+(rl+eps)
	if lb == ub {
		return ub
	}

	dh := y * t0
	dl := math.FMA(y, t0, -dh)
	dh, e := fastTwoSum(x, dh)
	rdh := 1 / dh
	dl += e
	nh := x * t0
	nl := math.FMA(x, t0, -nh)
	dt := y - nh
	if y1 := dt + nh; y1 == y {
		nh, nl = fastTwoSum(dt, -nl)
	} else {
		nh, nl = fastTwoSum(dt, (y-y1)-nl)
	}
	zh := nh * rdh
	z2 = zh * zh
	zl := rdh * (math.FMA(dh, -zh, nh) + (nl - (nh*rdh)*dl))
	b2 := &atan2B2
	zl += zh * z2 * ((b2[0] + z2*b2[1]) + (z2*z2)*(b2[2]+z2*b2[3]))
	zh *= sgn
	zl *= sgn
	eps = 0x1.4p-50 * (math.Abs(zh)*z2 + 0x1p-51)
	fh, fl = fastSum(fh, fl, zh, zl)
	lb, ub = fh+(fl-eps), fh+(fl+eps)
	if lb != ub {
		return atan2Accurate(y0, x0)
	}
	return ub
}

// atan2Special is atan2 where y0 or x0 is a zero, an infinity or a NaN.
func atan2Special(y0, x0 float64) float64 {
	iy, ix := asuint64(y0), asuint64(x0)
	aiy, aix := iy<<1, ix<<1
	const inf = 0x7ff << 53
	switch {
	case aiy > inf || aix > inf:
		return y0 + x0
	case aiy == inf && aix == inf:
		// atan2(+/-Inf, -Inf) = +/-3pi/4, atan2(+/-Inf, +Inf) = +/-pi/4.
		if ix>>63 != 0 {
			return math.Copysign(0x1.2d97c7f3321d2p+1, y0) + math.Copysign(0x1p-54, y0)
		}
		return math.Copysign(0x1.921fb54442d18p-1, y0) + math.Copysign(0x1p-55, y0)
	case aix == inf:
		if x0 < 0 {
			return math.Copysign(piH, y0) + math.Copysign(piL, y0)
		}
		return math.Copysign(0, y0)
	case aiy == inf:
		return math.Copysign(piOver2H, y0) + math.Copysign(piOver2L, y0)
	case aiy == 0 && aix == 0:
		switch {
		case ix == 0:
			return y0
		case iy == 0:
			return piH + piL
		}
		return -piH - piL
	case aiy == 0:
		switch {
		case x0 > 0:
			return y0 / x0
		case iy>>63 == 0:
			return piH + piL
		}
		return -piH - piL
	case y0 > 0:
		return piOver2H + piOver2L
	}
	return -piOver2H - piOver2L
}

// atan2Accurate is atan2(y, x) by a type [29,29] rational approximation of
// atan in 192-bit arithmetic: the path taken when the fast one cannot settle
// the rounding.
func atan2Accurate(y, x float64) float64 {
	t := y / x
	if t == 0 {
		switch {
		case x > 0:
			return t
		case y > 0:
			return piH + piL
		}
		return -piH - piL
	}
	// t exact and |t| <= 0x1.d12ed0af1a27fp-27, x > 0: atan(t) rounds to t.
	if math.FMA(t, x, -y) == 0 && x > 0 && math.Abs(t) <= 0x1.d12ed0af1a27fp-27 {
		if math.Abs(y) >= 0x1p-915 || math.FMA(t*0x1p105, x, -y*0x1p105) == 0 {
			return math.FMA(t, -0x1p-54, t)
		}
	}
	inv := math.Abs(y) > math.Abs(x)
	var z tint
	if inv {
		z = divTint(tintFromD(x), tintFromD(y))
	} else {
		z = divTint(tintFromD(y), tintFromD(x))
	}
	if !inv && x > 0 && z.ex <= -96 {
		// |y/x| < 2^-96: atan(y/x) rounds as y/x moved slightly toward zero.
		z.l -= 2
		z.m -= b2u(z.l < 2)
		z.h -= b2u(z.m < 1)
		return tintToD(z, 1)
	}
	// The rational approximation is for z > 0 only.
	sz := z.sgn
	z.sgn = 0
	p, q := atan2P[29], atan2Q[29]
	for i := 28; i >= 0; i-- {
		p = addTint(mulTint(p, z), atan2P[i])
		q = addTint(mulTint(q, z), atan2Q[i])
	}
	z = divTint(mulTint(p, z), q)
	// z approximates atan(y/x), or atan(x/y) if inv, to within 662 ulps.
	err := uint64(662)
	z.sgn = sz
	if inv {
		// pi/2 - atan(x/y) for x/y > 0, -pi/2 - atan(x/y) otherwise.
		if z.sgn == 0 {
			z.sgn = 1
			z = addTint(tintPi2, z)
		} else {
			z = addTint(tintPi2, z)
			z.sgn = 1
		}
		err = 524
	}
	if x < 0 {
		// Into the opposite quadrant.
		if z.sgn == 0 {
			z.sgn = 1
			z = addTint(tintPi, z)
			z.sgn = 1
		} else {
			z = addTint(tintPi, z)
		}
		err = 266
	}
	return tintToD(z, err)
}

// tint is (-1)^sgn * (h/2^64 + m/2^128 + l/2^192) * 2^ex, with h's top bit
// set unless it is zero: atan2's accurate path's 192-bit floating point.
type tint struct {
	h, m, l uint64
	ex      int64
	sgn     uint64
}

func (a tint) hm() u128 { return u128{a.h, a.m} }

var (
	tintZero = tint{ex: -1076}
	tintOne  = tint{h: 1 << 63, ex: 1}
	// tintPi and tintPi2 are pi and pi/2, to within 2^-196.96 and 2^-197.96.
	tintPi  = tint{h: 0xc90fdaa22168c234, m: 0xc4c6628b80dc1cd1, l: 0x29024e088a67cc74, ex: 2}
	tintPi2 = tint{h: 0xc90fdaa22168c234, m: 0xc4c6628b80dc1cd1, l: 0x29024e088a67cc74, ex: 1}
)

// mulTint is a*b to within 10 ulps.
func mulTint(a, b tint) tint {
	r := tint{ex: a.ex + b.ex, sgn: a.sgn ^ b.sgn}
	rh := mul64(a.h, b.h)
	rm1, rm2 := mul64(a.h, b.m), mul64(a.m, b.h)
	rl1, rl2, rl3 := mul64(a.h, b.l), mul64(a.m, b.m), mul64(a.l, b.h)
	r.h, r.m = rh.hi, rh.lo
	r.l = rm1.lo
	h := rm1.hi
	r.m += h
	r.h += b2u(r.m < h)
	l := rm2.lo
	h = rm2.hi
	r.l += l
	cm := b2u(r.l < l)
	r.m += h
	r.h += b2u(r.m < h)
	s := u128{0, rl1.hi}.add(u128{0, rl2.hi}).add(u128{0, rl3.hi})
	l = s.lo
	cm += s.hi
	r.l += l
	cm += b2u(r.l < l)
	r.m += cm
	r.h += b2u(r.m < cm)
	if r.h>>63 == 0 {
		r.h = r.h<<1 | r.m>>63
		r.m = r.m<<1 | r.l>>63
		r.l <<= 1
		r.ex--
	}
	return r
}

// cmpTintAbs is -1, 0 or +1 as |a| is less than, equal to or greater than |b|.
func cmpTintAbs(a, b tint) int {
	switch {
	case a.h == 0 && b.h == 0:
		return 0
	case a.h == 0:
		return -1
	case b.h == 0:
		return 1
	case a.ex != b.ex:
		if a.ex < b.ex {
			return -1
		}
		return 1
	case a.hm().less(b.hm()) || a.hm() == b.hm() && a.l < b.l:
		return -1
	case a.hm() == b.hm() && a.l == b.l:
		return 0
	}
	return 1
}

// tintShr is b's significand shifted right by k bits.
func tintShr(b tint, k uint64) (u128, uint64) {
	bh := b.hm()
	switch {
	case k == 0:
		return bh, b.l
	case k < 64:
		return bh.shr(uint(k)), bh.lo<<(64-k) | b.l>>k
	case k == 64:
		return bh.shr(64), bh.lo
	case k < 128:
		return bh.shr(uint(k)), bh.shr(uint(k - 64)).lo
	case k < 192:
		return u128{}, bh.shr(uint(k - 64)).lo
	}
	return u128{}, 0
}

// tintShl is b's significand shifted left by k bits.
func tintShl(b tint, k uint64) (u128, uint64) {
	bh := b.hm()
	switch {
	case k == 0:
		return bh, b.l
	case k < 64:
		return bh.shl(uint(k)).or(u128{0, b.l >> (64 - k)}), b.l << k
	case k == 64:
		return bh.shl(64).or(u128{0, b.l}), 0
	case k < 128:
		return bh.shl(uint(k)).or(u128{0, b.l}.shl(uint(k - 64))), 0
	case k < 192:
		return u128{0, b.l}.shl(uint(k - 64)), 0
	}
	return u128{}, 0
}

// tintClz is the number of leading zeros of the 192-bit h:l.
func tintClz(h u128, l uint64) uint64 {
	switch {
	case h.hi != 0:
		return uint64(bits.LeadingZeros64(h.hi))
	case h.lo != 0:
		return 64 + uint64(bits.LeadingZeros64(h.lo))
	}
	return 128 + uint64(bits.LeadingZeros64(l))
}

func tintFromParts(h u128, l uint64, ex int64) tint {
	return tint{h: h.hi, m: h.lo, l: l, ex: ex}
}

// addTint is a+b to within 2 ulps.
func addTint(a, b tint) tint {
	switch cmpTintAbs(a, b) {
	case 0:
		if a.sgn^b.sgn != 0 {
			return tintZero
		}
		a.ex++
		return a
	case -1:
		a, b = b, a
	}
	// |a| > |b|, so a.ex >= b.ex.
	sh := uint64(a.ex - b.ex)
	th, tl := tintShr(b, sh)
	var r tint
	if a.sgn^b.sgn != 0 {
		tl = a.l - tl
		th = a.hm().sub(th).sub(u128{0, b2u(tl > a.l)})
		ex := tintClz(th, tl)
		if ex <= 1 || sh == 0 {
			rh, rl := tintShl(tintFromParts(th, tl, 0), ex)
			r = tintFromParts(rh, rl, a.ex-int64(ex))
		} else {
			// ex >= 2 only when sh = 1: shift both left, so no bit of b is
			// lost and the subtraction is exact.
			bh, bl := tintShl(b, ex-sh)
			rh, rl := tintShl(a, ex)
			tl = rl - bl
			th = rh.sub(bh).sub(u128{0, b2u(tl > rl)})
			ex1 := tintClz(th, tl)
			rh, rl = tintShl(tintFromParts(th, tl, 0), ex1)
			r = tintFromParts(rh, rl, a.ex-int64(ex+ex1))
		}
	} else {
		ah := a.hm()
		rl := a.l + tl
		cl := b2u(rl < a.l)
		rh := ah.add(th)
		ch := b2u(rh.less(ah))
		rh = rh.add(u128{0, cl})
		ch += b2u(rh.less(u128{0, cl}))
		ex := a.ex
		if ch != 0 {
			ex++
			rl = rh.lo<<63 | rl>>1
			rh = u128{ch<<63 | rh.hi>>1, rh.lo>>1 | rh.hi<<63}
		}
		r = tintFromParts(rh, rl, ex)
	}
	r.sgn = a.sgn
	return r
}

// tintFromD is the tint of a finite non-zero x.
func tintFromD(x float64) tint {
	u := asuint64(x)
	a := tint{sgn: u >> 63}
	ax := u & 0x7fffffffffffffff
	if e := int64(ax >> 52); e != 0 {
		a.ex = e - 0x3fe
		a.h = 1<<63 | ax<<11
	} else {
		lz := bits.LeadingZeros64(ax)
		a.ex = -0x3f2 - int64(lz)
		a.h = ax << lz
	}
	return a
}

// tintToD is a rounded to a double, where err bounds the error on a.l in
// ulps, or is zero for a value that is a double.
func tintToD(a tint, err uint64) float64 {
	if a.ex >= 1025 {
		if a.sgn != 0 {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	if a.ex <= -1074 {
		s := 0x1p-1074
		if a.sgn != 0 {
			s = -s
		}
		if a.ex < -1074 || a.h == 1<<63 && a.m == 0 && a.l == 0 {
			return s * 0.5
		}
		return s * 0.75
	}
	hh, mm, ll := a.h, a.m, a.l
	ex := a.ex
	low := hh & 0x7ff
	// CORE-MATH stops here when it cannot round, which no known input does.
	if ex <= -1022 {
		// Subnormal.
		sh := uint(-1021 - ex)
		ll = mm<<(64-sh) | ll>>sh | b2u(ll > 0)
		mm = hh<<(64-sh) | mm>>sh
		hh >>= sh
		low = hh & 0x7ff
		ex += int64(sh)
	}
	h := float64(hh >> 11)
	var l float64
	switch {
	case err == 0:
	case low < 0x400:
		l = 0.25
	case low > 0x400:
		l = 0.75
	case mm == 0 && ll == 0:
		l = 0.5
	default:
		l = 0.75
	}
	s := 1.0
	if a.sgn != 0 {
		s = -1
	}
	h = math.FMA(l, s, s*h) * 0x1p-52
	return h * asdouble(asuint64(1)+uint64(ex-1)<<52)
}

// invTint is 1/a for a non-zero a, to within 2^87.1 ulps of 1/a in [1, 2].
func invTint(A tint) tint {
	a := tintToD(A, 0)
	subnormal := math.Abs(a) < 0x1p-1022
	if subnormal {
		a *= 0x1p53
	}
	r := tintFromD(1.0 / a)
	if subnormal {
		r.ex += 53
	}
	// Newton: r + r*(1-a*r).
	q := mulTint(A, r)
	q.sgn = 1 - q.sgn
	q = addTint(tintOne, q)
	q = mulTint(r, q)
	return addTint(r, q)
}

// divTint is b/a for a non-zero a, with relative error < 2^-185.53
// (Karp-Markstein).
func divTint(b, a tint) tint {
	y := invTint(a)
	r := mulTint(y, b)
	z := mulTint(a, r)
	z.sgn = 1 - z.sgn
	z = addTint(b, z)
	z = mulTint(y, z)
	return addTint(r, z)
}

var atan2T2 = [...]float64{
	0x0p+0, 0x1p-6, 0x1p-5, 0x1.8p-5, 0x1p-4, 0x1.4p-4, 0x1.8p-4, 0x1.cp-4,
	0x1p-3, 0x1.2p-3, 0x1.4p-3, 0x1.6p-3, 0x1.8p-3, 0x1.ap-3, 0x1.cp-3, 0x1.ep-3,
	0x1p-2, 0x1.1p-2, 0x1.2p-2, 0x1.3p-2, 0x1.4p-2, 0x1.5p-2, 0x1.6p-2, 0x1.7p-2,
	0x1.8p-2, 0x1.9p-2, 0x1.ap-2, 0x1.bp-2, 0x1.cp-2, 0x1.dp-2, 0x1.ep-2, 0x1.fp-2,
	0x1p-1, 0x1.08p-1, 0x1.1p-1, 0x1.18p-1, 0x1.2p-1, 0x1.28p-1, 0x1.3p-1, 0x1.38p-1,
	0x1.4p-1, 0x1.48p-1, 0x1.5p-1, 0x1.58p-1, 0x1.6p-1, 0x1.68p-1, 0x1.7p-1, 0x1.78p-1,
	0x1.8p-1, 0x1.88p-1, 0x1.9p-1, 0x1.98p-1, 0x1.ap-1, 0x1.a8p-1, 0x1.bp-1, 0x1.b8p-1,
	0x1.cp-1, 0x1.c8p-1, 0x1.dp-1, 0x1.d8p-1, 0x1.ep-1, 0x1.e8p-1, 0x1.fp-1, 0x1.f8p-1, 0x1p+0}

var atan2F2 = [...][2]float64{
	{0x0p+0, 0x0p+0}, {-0x1.95220c39d4dffp-53, 0x1.fff555bbb73p-7},
	{0x1.2542779d776dep-53, 0x1.ffd55bba976p-6}, {-0x1.6061bbe3de53cp-53, 0x1.7fb818430da4p-5},
	{-0x1.639269b0da47ep-53, 0x1.ff55bb72cfep-5}, {-0x1.4a7663af440f7p-55, 0x1.3f59f0e7c55ap-4},
	{0x1.d1824d59f9e13p-53, 0x1.7ee182602f1p-4}, {0x1.bef71e5340b31p-55, 0x1.be39ebe6f07cp-4},
	{0x1.b8cb225e627dp-53, 0x1.fd5ba9aac2f6p-4}, {0x1.b92de9bac94c2p-53, 0x1.1e1fafb04372p-3},
	{-0x1.d3cb89e62dafdp-54, 0x1.3d6eee8c6627p-3}, {-0x1.882a55960087ap-53, 0x1.5c9811e3ec27p-3},
	{0x1.1347b0b4f881dp-54, 0x1.7b97b4bce5bp-3}, {0x1.873d8079ed0d2p-53, 0x1.9a6a8e96c862p-3},
	{0x1.022f621a5c1cbp-54, 0x1.b90d7529260ap-3}, {0x1.9c648d1534598p-53, 0x1.d77d5df20573p-3},
	{-0x1.4ea9238610a08p-54, 0x1.f5b75f92c80ep-3}, {0x1.2c5c8e721970dp-53, 0x1.09dc597d8636p-2},
	{0x1.30ca4748b1bf9p-57, 0x1.18bf5a30bf178p-2}, {-0x1.20ef9ba6dbf9p-53, 0x1.278372057ef48p-2},
	{0x1.e69c5abb498d2p-53, 0x1.362773707ebc8p-2}, {0x1.a8a86f0ea9311p-54, 0x1.44aa436c2af08p-2},
	{0x1.db5336feef7fp-54, 0x1.530ad9951cd48p-2}, {0x1.9636a3aa3b84p-54, 0x1.614840309cfep-2},
	{0x1.1ce2a8c848b74p-55, 0x1.6f61941e4defp-2}, {-0x1.4b1bbd1ea6db3p-55, 0x1.7d5604b63b3f8p-2},
	{-0x1.4925e8b916e0bp-53, 0x1.8b24d394a1b28p-2}, {0x1.9e6c988fd0a77p-56, 0x1.98cd5454d6b18p-2},
	{-0x1.a49bd836a17p-53, 0x1.a64eec3cc24p-2}, {-0x1.ca3cf09c6b5f8p-53, 0x1.b3a911da65c7p-2},
	{-0x1.cc1ce70934c34p-56, 0x1.c0db4c94ec9fp-2}, {0x1.2e982ddf3872ap-55, 0x1.cde53432c135p-2},
	{-0x1.2ea406ee84d0fp-55, 0x1.dac670561bb5p-2}, {-0x1.de35847c81979p-53, 0x1.e77eb7f175a38p-2},
	{-0x1.a3992dc382a23p-57, 0x1.f40dd0b541418p-2}, {-0x1.b32c949c9d593p-55, 0x1.0039c73c1a40cp-1},
	{-0x1.d5b495f6349e6p-56, 0x1.0657e94db30dp-1}, {-0x1.f34582f6255fep-53, 0x1.0c6145b5b43dcp-1},
	{0x1.ed42511e3f11dp-54, 0x1.1255d9bfbd2a8p-1}, {-0x1.1cef189ff9e7fp-54, 0x1.1835a88be7c14p-1},
	{-0x1.928df287a668fp-58, 0x1.1e00babdefeb4p-1}, {-0x1.e3bde360c7ddbp-53, 0x1.23b71e2cc9e6cp-1},
	{0x1.bd86313ce4fdep-54, 0x1.2958e59308e3p-1}, {-0x1.8e8a85803cc1dp-53, 0x1.2ee628406cbccp-1},
	{-0x1.77ef7641c777fp-54, 0x1.345f01cce37bcp-1}, {0x1.b73ef3389d02fp-53, 0x1.39c391cd41718p-1},
	{0x1.ecf8b492644fp-56, 0x1.3f13fb89e96f4p-1}, {0x1.c1125fd3810c7p-53, 0x1.445065b795b54p-1},
	{0x1.2483350fe548bp-53, 0x1.4978fa3269eep-1}, {0x1.4a33dbeb3796cp-55, 0x1.4e8de5bb6ec04p-1},
	{-0x1.46edd2af69483p-53, 0x1.538f57b89062p-1}, {-0x1.2bcb93b18b52ap-53, 0x1.587d81f732fbcp-1},
	{0x1.0028e4bc5e7cap-57, 0x1.5d58987169b18p-1}, {0x1.ed487acaf1174p-53, 0x1.6220d115d7b8cp-1},
	{-0x1.2dd4dfd7d1777p-53, 0x1.66d663923e088p-1}, {0x1.2bfe3cf3b9d79p-54, 0x1.6b798920b3d98p-1},
	{-0x1.8c34d25aadef6p-56, 0x1.700a7c5784634p-1}, {-0x1.f426acf4d3bdbp-54, 0x1.748978fba8e1p-1},
	{-0x1.afe57dd9ff23p-53, 0x1.78f6bbd5d316p-1}, {-0x1.54fbef0e862abp-54, 0x1.7d528289fa094p-1},
	{0x1.90227758b11bap-54, 0x1.819d0b7158a4cp-1}, {0x1.16b66e7fc8b8cp-53, 0x1.85d69576cc2c4p-1},
	{-0x1.55b9a5e177a1bp-55, 0x1.89ff5ff57f1f8p-1}, {0x1.c27cfaa9f7a14p-53, 0x1.8e17aa99cc05cp-1},
	{0x1.1a62633145c07p-55, 0x1.921fb54442d18p-1}}

var atan2O = [8][2]float64{
	{0, 0}, {0x1.921fb54442d18p+0, 0x1.1a62633145c07p-54},
	{0, 0}, {-0x1.921fb54442d18p+0, -0x1.1a62633145c07p-54},
	{0x1.921fb54442d18p+1, 0x1.1a62633145c07p-53}, {0x1.921fb54442d18p+0, 0x1.1a62633145c07p-54},
	{-0x1.921fb54442d18p+1, -0x1.1a62633145c07p-53}, {-0x1.921fb54442d18p+0, -0x1.1a62633145c07p-54}}

var atan2B2 = [...]float64{-0x1.5555555555555p-2, 0x1.999999999755ep-3, -0x1.24924883596f8p-3, 0x1.c6f7d73531bc2p-4}

var atan2P = [30]tint{
	{h: 0x82703f8b53112eca, m: 0x71ce6279d467aa45, l: 0x19c9b28466b6fc0, ex: -10, sgn: 0},
	{h: 0xb3cf74b427d53e03, m: 0xee590b839e751890, l: 0xf2570b12bcb2381a, ex: -8, sgn: 0},
	{h: 0xb08498b17b88c39a, m: 0x11e37821d784d8fa, l: 0x2795b14b84b9d78c, ex: -6, sgn: 0},
	{h: 0xfd4baa7bb52b0d83, m: 0xb25425beb0a04dc3, l: 0x92008d9fb34b68ff, ex: -5, sgn: 0},
	{h: 0x96541948b85fb386, m: 0xe0b815ace425a859, l: 0xbeca17c69bb4f7a0, ex: -3, sgn: 0},
	{h: 0x96aaa5d4baffc590, m: 0xf23db783b6e4ce55, l: 0x2c54e054d3b69f7e, ex: -2, sgn: 0},
	{h: 0x83cd41b5fa862b11, m: 0xad8f9d6e3eb9d833, l: 0x4ef470d801fa03ca, ex: -1, sgn: 0},
	{h: 0xcbbc7752197cc106, m: 0xbc66cfb3edefa6e3, l: 0x568b2f9e4feb8910, ex: -1, sgn: 0},
	{h: 0x8cf8f7c585767042, m: 0xcd92a5b9ac13d8c8, l: 0x43bd4a023dbe04d, ex: 0, sgn: 0},
	{h: 0xafdc36d6b36139bf, m: 0x11df63474e3859d4, l: 0x42b30b2c089a2b6c, ex: 0, sgn: 0},
	{h: 0xc6f8fb1463822519, m: 0x2f9f04cf55c10377, l: 0x1c44675c6d483606, ex: 0, sgn: 0},
	{h: 0xcce79f10f754e315, m: 0x91ce47edcbc8a4ee, l: 0xb2a6f721ac656b42, ex: 0, sgn: 0},
	{h: 0xc090384dbd77132e, m: 0x1dfb4d9a1d595de7, l: 0xe506ca899a394f70, ex: 0, sgn: 0},
	{h: 0xa556ac9eee25e867, m: 0xd84dafa25141d203, l: 0xf1c7c2a5156b3ec3, ex: 0, sgn: 0},
	{h: 0x81bd2677ddfcbc38, m: 0x1df33f27dbb60939, l: 0xcb4c2c73286a6c29, ex: 0, sgn: 0},
	{h: 0xb9ed12169d72e8d9, m: 0xc81d95dcef13c227, l: 0x974b95796b93c332, ex: -1, sgn: 0},
	{h: 0xf2df6da875b666fa, m: 0xc29dd3a69c274c65, l: 0x83b9260976c8f0c5, ex: -2, sgn: 0},
	{h: 0x902d34d4db910ed8, m: 0x6f3ecddfd6af0a9f, l: 0x8e950d7e70a8e9b2, ex: -2, sgn: 0},
	{h: 0x9aefa41961ce1207, m: 0xacd0317666753751, l: 0x3eaf42a53fcc6534, ex: -3, sgn: 0},
	{h: 0x95db9977c5e6e263, m: 0x85367a63fab8a720, l: 0xfc2b63bd115998d8, ex: -4, sgn: 0},
	{h: 0x817cb6d8236d5f4d, m: 0x70c7815f28909d9b, l: 0xc54e97155d12158, ex: -5, sgn: 0},
	{h: 0xc5eec4228c352098, m: 0x789e56be1dee9b67, l: 0xba25bda81292ccc1, ex: -7, sgn: 0},
	{h: 0x840c38ccfcb5dc64, m: 0xb55a44095bf7f312, l: 0x6668b8792d4ecd08, ex: -8, sgn: 0},
	{h: 0x97074af7b286b19e, m: 0xc20aac21f3130f1d, l: 0x369e9fdbdd48528a, ex: -10, sgn: 0},
	{h: 0x9054011c32d4b7a5, m: 0xc68d3c5d525f4af4, l: 0xeeb3c98615dee188, ex: -12, sgn: 0},
	{h: 0xdddb4a2eaab2bb15, m: 0x753761a8f0e9aa8d, l: 0x3f20620cd3e075bb, ex: -15, sgn: 0},
	{h: 0x80ef9a1b3e59abe5, m: 0x18e6afe0651fdf49, l: 0xd1c0329e6a702d9d, ex: -17, sgn: 0},
	{h: 0xca16dd49cd30854c, m: 0xbad8ca379401027c, l: 0x1fc7f47985486f16, ex: -21, sgn: 0},
	{h: 0xa0ba01b8742ba801, m: 0xe6dc14a9516d7f04, l: 0x1f5303d878905d2f, ex: -25, sgn: 0},
	{h: 0x9c0440896ce6515c, m: 0xcf3470fa7f10ac3, l: 0xa9dc80682c111a9d, ex: -59, sgn: 1},
}

var atan2Q = [30]tint{
	{h: 0x82703f8b53112eca, m: 0x71ce6279d467aa45, l: 0x19c9b28466b6fc0, ex: -10, sgn: 0},
	{h: 0xb3cf74b427d53e03, m: 0xee590b839e751890, l: 0xf2570b12bcb238cf, ex: -8, sgn: 0},
	{h: 0xb33c44af0d43c9e8, m: 0xf4426f84614701db, l: 0x979e49dc5b858442, ex: -6, sgn: 0},
	{h: 0x8264d256f0bf4d61, m: 0xee21431cc0f29764, l: 0xce0cc1b593ac9952, ex: -4, sgn: 0},
	{h: 0x9d97c4947687b2ee, m: 0xef4087c49dde5229, l: 0x742bc6e02bfb6df8, ex: -3, sgn: 0},
	{h: 0xa0f888dc7aab949e, m: 0xf623f19ac413db23, l: 0x854744a164f70ecd, ex: -2, sgn: 0},
	{h: 0x8fd9c7172ab0828e, m: 0x1a9f72fadafc3dfb, l: 0xaf96a180ea297178, ex: -1, sgn: 0},
	{h: 0xe38168b78a51ee15, m: 0x604a47815c8091eb, l: 0x9faaa42312e8eb30, ex: -1, sgn: 0},
	{h: 0xa164ccdbb65ee133, m: 0xe623433c5e086402, l: 0xcb228afd335a245f, ex: 0, sgn: 0},
	{h: 0xced0c0cbe44fac2f, m: 0xd7c2adfc7c5dda9e, l: 0xa22b4a311cf5a5b1, ex: 0, sgn: 0},
	{h: 0xf0e6660438c90371, m: 0x3dca1a0f58f0d943, l: 0x8a92b9de68c4488c, ex: 0, sgn: 0},
	{h: 0x8000000000000000, m: 0x0, l: 0x0, ex: 1, sgn: 0},
	{h: 0xf8ea68f9ad849103, m: 0xb257e4bfc606967f, l: 0x818fa2816928c4aa, ex: 0, sgn: 0},
	{h: 0xddc64eeee3506234, m: 0x47a33ea40a2b22e4, l: 0xa6a2c129ace2470a, ex: 0, sgn: 0},
	{h: 0xb52ddfba2d37c8e2, m: 0x24551a9b19fa064a, l: 0x3d78c68b54bc3850, ex: 0, sgn: 0},
	{h: 0x87ac113d26b683a1, m: 0x4e97756c38785b0b, l: 0xf75f0a4506931ba8, ex: 0, sgn: 0},
	{h: 0xba065070b900f3d5, m: 0xe49d8f9f238f1eeb, l: 0x6805d48d67f1dcd9, ex: -1, sgn: 0},
	{h: 0xe902d2a4bfc22441, m: 0x945cfe6c83a5b828, l: 0x519ebb457f8ccca0, ex: -2, sgn: 0},
	{h: 0x84e405c915f7769b, m: 0xfaaf34c3c51dd39, l: 0xa97fa0ae99382c93, ex: -2, sgn: 0},
	{h: 0x896da46c7e6cebb3, m: 0xe0afc0b6db3dbbf5, l: 0x61ccff6e78efff4, ex: -3, sgn: 0},
	{h: 0x801a0c1a0dfefc92, m: 0xf8e0a97902e08d47, l: 0x81898e0b7d8ef0bc, ex: -4, sgn: 0},
	{h: 0xd59d088930df07db, m: 0xa8cb00021849fbaf, l: 0xf80cc91aa0bddf89, ex: -6, sgn: 0},
	{h: 0x9db56a6984b77231, m: 0xcf7c2074e0480959, l: 0x3316ecaa6aa4604e, ex: -7, sgn: 0},
	{h: 0xcb7450d9d62d2e68, m: 0x9787b4265977d004, l: 0xba615cee00a841d7, ex: -9, sgn: 0},
	{h: 0xe12a52e02b64b23e, m: 0x97c9139a8ced716c, l: 0x70a176ad2b5f16d2, ex: -11, sgn: 0},
	{h: 0xd057b6d0900ed30d, m: 0x5e04bb17eb879a5e, l: 0x23ed135b60bea070, ex: -13, sgn: 0},
	{h: 0x9b1f1ca22fef7ae5, m: 0x19a687f0a4073eee, l: 0x5a9ccbc8c14578b, ex: -15, sgn: 0},
	{h: 0xaeba710f053aac3b, m: 0x30ea5d6fb116f775, l: 0x8a9bcfef5698f8f1, ex: -18, sgn: 0},
	{h: 0x84b9ada076cd007d, m: 0x13e55006073f76b1, l: 0x4cf4259968228f3e, ex: -21, sgn: 0},
	{h: 0xcca4b9046786937d, m: 0x233c1e691c127c1f, l: 0xf493e0b7a3de459a, ex: -26, sgn: 0},
}
