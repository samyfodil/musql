// Ported from CORE-MATH, src/binary64/asinh/asinh.c, which glibc 2.43
// carries as sysdeps/ieee754/dbl-64/s_asinh.c. MIT licensed: see coremath.go.

package libm

import "math"

// Asinh is glibc's asinh, CORE-MATH's correctly rounded one.
func Asinh(x float64) float64 {
	ax := math.Abs(x)
	u := asuint64(ax)
	if u < 0x3fbb000000000000 {
		// |x| < 0x1.bp-4.
		if u < 0x3e57137449123ef7 {
			// |x| < 0x1.7137449123ef7p-26: asinh(x) rounds to x.
			if u == 0 {
				return x
			}
			return math.FMA(-0x1p-60, x, x)
		}
		x2h := x * x
		x2l := math.FMA(x, x, -x2h)
		x3h := x2h * x
		var sl float64
		switch {
		case u < 0x3e5a000000000000:
			// |x| < 0x1.ap-26.
			sl = x3h * -0x1.5555555555555p-3
		case u < 0x3f30000000000000:
			// |x| < 0x1p-12.
			sl = x3h * (-0x1.5555555555555p-3 + x2h*0x1.3333327c57c6p-4)
		case u < 0x3f93000000000000:
			// |x| < 0x1.3p-6.
			sl = x3h * (-0x1.5555555555555p-3 + x2h*(0x1.333333332f2ffp-4+x2h*(-0x1.6db6d9a665159p-5+x2h*0x1.f186866d775fp-6)))
		default:
			// 0x1.3p-6 <= |x| < 0x1.bp-4: a minimax polynomial with relative
			// error < 2^-63.091.
			c1 := 0x1.333333333331p-4 + x2h*-0x1.6db6db6da466cp-5
			c3 := 0x1.f1c71c2ea7be4p-6 + x2h*-0x1.6e8b651b09d72p-6
			c5 := 0x1.1c309fc0e69c2p-6 + x2h*-0x1.bab7833c1ep-7
			x4 := x2h * x2h
			sl = x3h * (-0x1.5555555555555p-3 + x2h*(c1+x4*(c3+x4*c5)))
		}
		eps := 0x1.79p-53 * x3h
		lb, ub := x+(sl-eps), x+(sl+eps)
		if lb == ub {
			return lb
		}
		return asinhZero(x, x2h, x2l)
	}

	// |x| >= 0x1.bp-4.
	var x2h, x2l, ah, al float64
	off := 0x3ff
	switch {
	case u < 0x4190000000000000:
		// |x| < 0x1p+26.
		var th, tl float64
		x2h = x * x
		x2l = math.FMA(x, x, -x2h)
		if u < 0x3ff0000000000000 {
			th, tl = fastTwoSum(1, x2h)
		} else {
			th, tl = fastTwoSum(x2h, 1)
		}
		tl += x2l
		ah = math.Sqrt(th)
		rs := 0.5 / th
		al = (tl - math.FMA(ah, ah, -th)) * (rs * ah)
		ah, tl = fastTwoSum(ah, ax)
		al += tl
	case u < 0x4330000000000000:
		// |x| < 0x1p+52.
		ah = 2 * ax
		al = 0.5 / ax
	default:
		// |x| >= 0x1p+52.
		if u >= 0x7ff0000000000000 {
			return x + x
		}
		off = 0x3fe
		ah = ax
		al = 0
	}

	t := asuint64(ah)
	ex := int(t >> 52)
	e := ex - off
	t &= ^uint64(0) >> 12
	ed := float64(e)
	i := t >> (52 - 5)
	d := int64(t & (^uint64(0) >> 17))
	j := (t + uint64(cmLogB[i].c0)<<33 + uint64(int64(cmLogB[i].c1)*(d>>16))) >> (52 - 10)
	t |= 0x3ff << 52
	i1, i2 := j>>5, j&0x1f
	r := cmLogR1[i1] * cmLogR2[i2]
	dx := math.FMA(r, asdouble(t), -1)
	dx2 := dx * dx
	f := dx2 * ((-0x1p-1 + dx*0x1.555555555553p-2) + dx2*((-0x1.fffffffffffap-3+dx*0x1.99999e33a6366p-3)+dx2*-0x1.555559ef9525fp-3))
	const l2h, l2l = 0x1.62e42fefa38p-1, 0x1.ef35793c7673p-45
	lh := l2h*ed + (cmLogL1[i1][1] + cmLogL2[i2][1])
	ll := l2l*ed + cmLogL1[i1][0] + cmLogL2[i2][0] + al/ah + f
	ll += dx
	lh *= math.Copysign(1, x)
	ll *= math.Copysign(1, x)
	const eps = 1.63e-19
	lb, ub := lh+(ll-eps), lh+(ll+eps)
	if lb == ub {
		return lb
	}
	if ax < 0x1p-2 {
		return asinhZero(x, x2h, x2l)
	}
	return asinhRefine(x, ah, al, 0x1.71547652b82fep+0*math.Abs(lb))
}

func asinhAddDD(xh, xl, ch, cl float64) (s, l float64) {
	s = xh + ch
	d := s - xh
	return s, ((ch - d) + (xh + (d - s))) + (xl + cl)
}

func asinhMulDDAcc(xh, xl, ch, cl float64) (h, l float64) {
	ahlh, alhh, ahhh := ch*xl, cl*xh, ch*xh
	ahhl := math.FMA(ch, xh, -ahhh)
	ahhl += alhh + ahlh
	h = ahhh + ahhl
	return h, (ahhh - h) + ahhl
}

func cmMulDDD(xh, xl, ch float64) (h, l float64) {
	ahlh, ahhh := ch*xl, ch*xh
	ahhl := math.FMA(ch, xh, -ahhh)
	ahhl += ahlh
	h = ahhh + ahhl
	return h, (ahhh - h) + ahhl
}

// asinhPolyDD evaluates the double-double polynomial c at xh+xl, with l the
// low part to add to its last coefficient; it returns the value as h+l.
func asinhPolyDD(xh, xl float64, c [][2]float64, l float64) (h, lo float64) {
	i := len(c) - 1
	ch := c[i][0] + l
	cl := ((c[i][0] - ch) + l) + c[i][1]
	for i--; i >= 0; i-- {
		ch, cl = asinhMulDDAcc(xh, xl, ch, cl)
		th := ch + c[i][0]
		tl := (c[i][0] - th) + ch
		ch = th
		cl += tl + c[i][1]
	}
	return ch, cl
}

// roundAway nudges the low part y1 of a sum one ulp toward y2 when y1 is a
// power of two, so y0+y1 rounds as the whole sum would.
func roundAway(y1, y2 float64) float64 {
	t := asuint64(y1)
	if t&(^uint64(0)>>12) == 0 {
		if (asuint64(y2)^t)>>63 != 0 {
			t--
		} else {
			t++
		}
		return asdouble(t)
	}
	return y1
}

func asinhZero(x, x2h, x2l float64) float64 {
	cl := &asinhZeroCL
	y2 := x2h * (cl[0] + x2h*(cl[1]+x2h*(cl[2]+x2h*(cl[3]+x2h*cl[4]))))
	y1, y2 := asinhPolyDD(x2h, x2l, asinhZeroCH[:], y2)
	y1, y2 = asinhMulDDAcc(y1, y2, x2h, x2l)
	y1, y2 = cmMulDDD(y1, y2, x)
	y0, y1 := fastTwoSum(x, y1)
	y1, y2 = fastTwoSum(y1, y2)
	return y0 + roundAway(y1, y2)
}

func asinhDatabase(x, f float64) float64 {
	ax := math.Abs(x)
	a, b := 0, len(asinhDB)-1
	for a <= b {
		m := (a + b) / 2
		switch {
		case asinhDB[m][0] < ax:
			a = m + 1
		case asinhDB[m][0] == ax:
			sgn := math.Copysign(1, x)
			return sgn*asinhDB[m][1] + sgn*asinhDB[m][2]
		default:
			b = m - 1
		}
	}
	return f
}

func asinhRefine(x, zh, zl, a float64) float64 {
	t := asuint64(zh)
	ex := int(t >> 52)
	e := ex - 0x3ff
	if zl == 0 {
		e++
	}
	t &= ^uint64(0) >> 12
	t |= 0x3ff << 52
	ed := float64(e)
	v := asuint64(a - ed + 0x1.00008p+0)
	i := (v - 0x3ff<<52) >> (52 - 16)
	i1, i2, i3, i4 := (i>>12)&0x1f, (i>>8)&0xf, (i>>4)&0xf, i&0xf
	const l20, l21, l22 = 0x1.62e42fefa38p-2, 0x1.ef35793c768p-46, -0x1.9ff0342542fc3p-91
	el2, el1, el0 := l22*ed, l21*ed, l20*ed
	var L [3]float64
	for k := range L {
		L[k] = cmLogLL[0][i1][k] + cmLogLL[1][i2][k] + (cmLogLL[2][i3][k] + cmLogLL[3][i4][k])
	}
	L[0] += el0
	t12, t34 := cmLogT1[i1]*cmLogT2[i2], cmLogT3[i3]*cmLogT4[i4]
	th := t12 * t34
	tl := math.FMA(t12, t34, -th)
	tf := asdouble(t)
	dh := th * tf
	dl := math.FMA(th, tf, -dh)
	sh := tl * tf
	sl := math.FMA(tl, tf, -sh)
	xh, xl := fastTwoSum(dh-1, dl)
	if zl != 0 {
		xl += th * asdouble(asuint64(zl)-uint64(int64(e)<<52))
	}
	xh, xl = asinhAddDD(xh, xl, sh, sl)
	sl = xh * (-0x1p-3 + xh*(0x1.9999999a0754fp-4+xh*-0x1.55555555c3157p-4))
	sh, sl = asinhPolyDD(xh, xl, cmLogCH[:], sl)
	sh, sl = asinhMulDDAcc(xh, xl, sh, sl)
	sh, sl = asinhAddDD(sh, sl, el1, el2)
	sh, sl = asinhAddDD(sh, sl, L[1], L[2])
	v0, v2 := fastTwoSum(L[0], sh)
	v1, v2 := fastTwoSum(v2, sl)
	v0, v1 = fastTwoSum(v0, v1)
	v1, v2 = fastTwoSum(v1, v2)
	s2 := math.Copysign(2, x)
	v0 *= s2
	v1 *= s2
	v2 *= s2
	v1 = roundAway(v1, v2)
	t, t0 := asuint64(v1), asuint64(v0)
	er := (t + 41) & (^uint64(0) >> 12)
	de := (t0>>52)&0x7ff - (t>>52)&0x7ff
	res := v0 + v1
	if de > 99 || er < 80 {
		return asinhDatabase(x, res)
	}
	return res
}

var asinhZeroCH = [...][2]float64{
	{-0x1.5555555555555p-3, -0x1.5555555555555p-57}, {0x1.3333333333333p-4, 0x1.99999999949dfp-59},
	{-0x1.6db6db6db6db7p-5, 0x1.2492496091b0cp-60}, {0x1.f1c71c71c71c7p-6, 0x1.c71a35cfa0671p-62},
	{-0x1.6e8ba2e8ba2e9p-6, 0x1.17f937248cf81p-60}, {0x1.1c4ec4ec4ec4fp-6, -0x1.74e3c1dfd4c3dp-60},
	{-0x1.c999999999977p-7, -0x1.38e7a467ecc55p-61}, {0x1.7a87878786c7ep-7, 0x1.a83c7bace55ebp-61},
	{-0x1.3fde50d764083p-7, -0x1.d024df7fa0542p-61}, {0x1.12ef3ceae4d12p-7, -0x1.ba9c13deb261fp-61},
	{-0x1.df3bd104aa267p-8, -0x1.546da9bc5b32ap-62}, {0x1.a685fc5de7a04p-8, 0x1.40d284a1d67f9p-62}}

var asinhZeroCL = [...]float64{
	-0x1.7828d553ec8p-8, 0x1.51712f7bee368p-8, -0x1.2e6d98527bcc6p-8, 0x1.0095da47b392cp-8, -0x1.3b92d6368192cp-9}

var asinhDB = [...][3]float64{
	{0x1.00f9476450863p-2, 0x1.fcb35067f343cp-3, 0x1p-57},
	{0x1.1f0a79315b287p-2, 0x1.1b68aae88febap-2, 0x1p-56},
	{0x1.2b9618ff7acb7p-2, 0x1.27781d9aa4e25p-2, -0x1p-56},
	{0x1.389ef683f3aa7p-2, 0x1.33f52db6df1afp-2, 0x1p-56},
	{0x1.3b07e0c779ddap-2, 0x1.364303e1ad8f6p-2, 0x1p-56},
	{0x1.48441df33b6d3p-2, 0x1.42e385800f0a4p-2, 0x1p-56},
	{0x1.687bd068c1c1ep-2, 0x1.616cc75d49226p-2, -0x1p-56},
	{0x1.8740c4453a056p-2, 0x1.7e4f2ad132a1dp-2, 0x1p-56},
	{0x1.891acda11167ep-2, 0x1.8009d924a3ffdp-2, 0x1p-56},
	{0x1.bafc3479fc9ccp-2, 0x1.ae3773250e7d2p-2, 0x1p-56},
	{0x1.c59869f17b483p-2, 0x1.b7efa91915c95p-2, 0x1p-56},
	{0x1.c8be879787986p-2, 0x1.bad0485e0fe0ap-2, -0x1p-56},
	{0x1.e73b46abb01e1p-2, 0x1.d68039861ab53p-2, 0x1p-56},
	{0x1.ed6236da268bp-2, 0x1.dc0cb8f638126p-2, 0x1p-56},
	{0x1.f399ebafc1951p-2, 0x1.e1a4f519fab77p-2, -0x1p-56},
	{0x1.f70975ab0d471p-2, 0x1.e4bae8bcd6ea6p-2, 0x1p-56},
	{0x1.fbdd4a37760b7p-2, 0x1.e90f16eb88c09p-2, 0x1p-56},
	{0x1.fee72efb4bfddp-2, 0x1.ebc791a88bed8p-2, 0x1p-56},
	{0x1.02339d6bdb741p-1, 0x1.f0b2264e34555p-2, 0x1p-56},
	{0x1.09e7c831b1a23p-1, 0x1.fe694c3c89138p-2, 0x1p-56},
	{0x1.16d32c862fc3bp-1, 0x1.0a9c9334066dbp-1, -0x1p-55},
	{0x1.857954132083dp-1, 0x1.67425fe575c88p-1, -0x1p-55},
	{0x1.8a5c3b60f7e11p-1, 0x1.6b23ad4415a17p-1, -0x1p-55},
	{0x1.9740eb419dd04p-1, 0x1.754ab7535d47dp-1, 0x1p-55},
	{0x1.a16d9cc06011ap-1, 0x1.7d3755d851062p-1, -0x1p-55},
	{0x1.bb635be2213d1p-1, 0x1.91167cae3cfa9p-1, 0x1p-55},
	{0x1.d4b21ebf542fp-1, 0x1.a3fc7e4dd47d1p-1, -0x1p-55},
	{0x1.7b8516ffd2406p+0, 0x1.2f5d3b178914ap+0, 0x1p-54},
	{0x1.9295b9116e2e2p+0, 0x1.3bffa8863976p+0, 0x1p-54},
	{0x1.fedc65e32714p+0, 0x1.710f91e844f9bp+0, 0x1p-54},
	{0x1.57e377b3f0b4bp+1, 0x1.b6e2c73f41415p+0, 0x1p-54},
	{0x1.6056b06a21918p+3, 0x1.8c0a26d055288p+1, 0x1p-53},
	{0x1.843e1b5e5979cp+4, 0x1.f0f978201eb84p+1, 0x1p-53},
	{0x1.fee8f69c4cd25p+10, 0x1.0a19aebb51e9p+3, -0x1p-51},
	{0x1.0fbc6c02b1c9p+24, 0x1.16369cd53bb69p+4, 0x1p-50},
}
