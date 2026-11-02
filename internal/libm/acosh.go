// Ported from CORE-MATH, src/binary64/acosh/acosh.c, which glibc 2.43
// carries as sysdeps/ieee754/dbl-64/e_acosh.c. MIT licensed: see coremath.go.

package libm

import "math"

// Acosh is glibc's acosh, CORE-MATH's correctly rounded one.
func Acosh(x float64) float64 {
	ix := asuint64(x)
	if ix >= 0x7ff0000000000000 {
		// x < 0, or infinite, or NaN.
		if ix == 0x7ff0000000000000 || ix<<1 > 0x7ff<<53 {
			return x + x
		}
		return math.NaN()
	}
	if int64(ix) <= 0x3ff0000000000000 {
		// 0 <= x <= 1.
		if ix == 0x3ff0000000000000 {
			return 0
		}
		return math.NaN()
	}

	// x > 1.
	var g, eps float64
	off := 0x3fe
	t := ix
	switch {
	case ix < 0x3ff1e83e425aee63:
		// 1 < x < 0x1.1e83e425aee63p+0.
		z := x - 1
		iz, zt := -0.25/z, float64(2*z)
		sh := math.Sqrt(zt)
		sl := float64(math.FMA(sh, sh, -zt) * (sh * iz))
		cl := &acoshNearCL
		z2 := z * z
		z4 := z2 * z2
		ds := math.FMA(sh*z, cl[0]+float64(z*(((cl[1]+float64(z*cl[2]))+float64(z2*(cl[3]+float64(z*cl[4]))))+float64(z4*((cl[5]+float64(z*cl[6]))+float64(z2*(cl[7]+float64(z*cl[8]))))))), sl)
		eps = float64(ds*0x1.00p-50) - float64(0x1p-104*sh)
		lb, ub := sh+(ds-eps), sh+(ds+eps)
		if lb == ub {
			return lb
		}
		return acoshOne(z, sh, sl)
	case ix < 0x405bf00000000000:
		// x < 111.75.
		off = 0x3ff
		x2h := float64(x * x)
		wh := x2h - 1
		wl := math.FMA(x, x, -x2h)
		sh := math.Sqrt(wh)
		ish := 0.5 / wh
		sl := float64((wl - math.FMA(sh, sh, -wh)) * (sh * ish))
		th, tl := fastTwoSum(x, sh)
		tl += sl
		t = asuint64(th)
		g = tl / th
		eps = 0x1.81p-63
	case ix < 0x4087100000000000:
		// x < 738.
		z := 1 / (x * x)
		g = 0x1.5c4b6148816e2p-66 + float64(z*(-0x1.000000000005cp-2+float64(z*(-0x1.7fffffebf3e6cp-4+float64(z*-0x1.aab6691f2bae7p-5)))))
		eps = 0x1.c3p-63
	case ix < 0x40e0100000000000:
		// x < 32896.
		z := 1 / (x * x)
		g = -0x1.7f77c8429c6c6p-67 + float64(z*(-0x1.ffffffffff214p-3+float64(z*-0x1.8000268641bfep-4)))
		eps = 0x1.9ap-63
	case ix < 0x41ea000000000000:
		// x < 0x1.ap+31.
		z := 1 / (x * x)
		g = 0x1.7a0ed2effdd1p-67 + float64(z*-0x1.000000017d048p-2)
		eps = 0x1.99p-63
	default:
		eps = 0x1.b2p-63
	}
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
	f := float64(dx2 * ((-0x1p-1 + float64(dx*0x1.555555555553p-2)) + float64(dx2*((-0x1.fffffffffffap-3+float64(dx*0x1.99999e33a6366p-3))+float64(dx2*-0x1.555559ef9525fp-3)))))
	const l2h, l2l = 0x1.62e42fefa38p-1, 0x1.ef35793c7673p-45
	lh := (cmLogL1[i1][1] + cmLogL2[i2][1]) + float64(l2h*ed)
	t1 := (float64(l2l * ed)) + (cmLogL1[i1][0] + cmLogL2[i2][0])
	t2 := f + t1
	t3 := g + t2
	ll := dx + t3
	lb, ub := lh+(ll-eps), lh+(ll+eps)
	if lb == ub {
		return lb
	}
	return acoshRefine(x, float64(0x1.71547652b82fep+0*lb))
}

func acoshAddDD(xh, xl, ch, cl float64) (s, l float64) {
	s, l = twoSum(xh, ch)
	return s, l + (xl + cl)
}

func acoshMulDDAcc(xh, xl, ch, cl float64) (h, l float64) {
	ahlh, alhh, ahhh := float64(ch*xl), float64(cl*xh), float64(ch*xh)
	ahhl := math.FMA(ch, xh, -ahhh)
	ahhl += alhh + ahlh
	return fastTwoSum(ahhh, ahhl)
}

func acoshPolyDD(xh, xl float64, c [][2]float64, l float64) (h, lo float64) {
	i := len(c) - 1
	ch, cl := fastTwoSum(c[i][0], l)
	cl += c[i][1]
	for i--; i >= 0; i-- {
		ch, cl = acoshMulDDAcc(xh, xl, ch, cl)
		th, tl := fastTwoSum(c[i][0], ch)
		ch = th
		cl += tl + c[i][1]
	}
	return ch, cl
}

func acoshOne(x, sh, sl float64) float64 {
	cl := &acoshOneCL
	y2 := float64(x * (cl[0] + float64(x*(cl[1]+float64(x*(cl[2]+float64(x*(cl[3]+float64(x*(cl[4]+float64(x*cl[5])))))))))))
	y1, y2 := acoshPolyDD(x, 0, acoshOneCH[:], y2)
	y1, y2 = cmMulDDD(y1, y2, x)
	y0, y1 := fastTwoSum(1, y1)
	y1 += y2
	y0, y1 = acoshMulDDAcc(y0, y1, sh, sl)
	return y0 + y1
}

func acoshDatabase(x, f float64) float64 {
	a, b := 0, len(acoshDB)-1
	for a <= b {
		m := (a + b) / 2
		switch {
		case acoshDB[m][0] < x:
			a = m + 1
		case acoshDB[m][0] == x:
			return acoshDB[m][1] + acoshDB[m][2]
		default:
			b = m - 1
		}
	}
	return f
}

// acoshRefine is acosh(x) where a approximates acosh(x)/log(2).
func acoshRefine(x, a float64) float64 {
	ix := asuint64(x)
	var zh, zl float64
	huge := 0
	switch {
	case ix < 0x4190000000000000:
		// x < 2^26.
		x2h := float64(x * x)
		x2l := math.FMA(x, x, -x2h)
		wh, wl := fastTwoSum(x2h-1, x2l)
		sh := math.Sqrt(wh)
		sl := (wl - math.FMA(sh, sh, -wh)) / (2.0 * sh)
		zh, zl = fastTwoSum(x, sh)
		zl += sl
		zh, zl = fastTwoSum(zh, zl)
	case ix < 0x4330000000000000:
		// x < 2^52.
		zh = float64(2 * x)
		zl = -0.5 / x
	default:
		// x >= 2^52: zh+zl is (x+sqrt(x^2-1))/2, so 2*x cannot overflow.
		zh = x
		huge = 1
	}
	t := asuint64(zh)
	ex := int(t >> 52)
	e := ex - 0x3ff + huge
	t &= ^uint64(0) >> 12
	t |= 0x3ff << 52
	ed := float64(e)
	v := asuint64(a - ed + 0x1.00008p+0)
	i := (v - 0x3ff<<52) >> (52 - 16)
	i1, i2, i3, i4 := (i>>12)&0x1f, (i>>8)&0xf, (i>>4)&0xf, i&0xf
	const l20, l21, l22 = 0x1.62e42fefa38p-2, 0x1.ef35793c768p-46, -0x1.9ff0342542fc3p-91
	el2, el1, el0 := float64(l22*ed), float64(l21*ed), float64(l20*ed)
	var L [3]float64
	for k := range L {
		L[k] = cmLogLL[0][i1][k] + cmLogLL[1][i2][k] + (cmLogLL[2][i3][k] + cmLogLL[3][i4][k])
	}
	L[0] += el0
	t12, t34 := cmLogT1[i1]*cmLogT2[i2], cmLogT3[i3]*cmLogT4[i4]
	th := float64(t12 * t34)
	tl := math.FMA(t12, t34, -th)
	tf := asdouble(t)
	dh := float64(th * tf)
	dl := math.FMA(th, tf, -dh)
	sh := float64(tl * tf)
	sl := math.FMA(tl, tf, -sh)
	xh, xl := fastTwoSum(dh-1, dl)
	if zl != 0 {
		xl += float64(th * asdouble(asuint64(zl)-uint64(int64(e)<<52)))
	}
	xh, xl = acoshAddDD(xh, xl, sh, sl)
	sl = float64(xh * (-0x1p-3 + float64(xh*(0x1.9999999a0754fp-4+float64(xh*-0x1.55555555c3157p-4)))))
	sh, sl = acoshPolyDD(xh, xl, cmLogCH[:], sl)
	sh, sl = acoshMulDDAcc(xh, xl, sh, sl)
	sh, sl = acoshAddDD(sh, sl, el1, el2)
	sh, sl = acoshAddDD(sh, sl, L[1], L[2])
	v0, v2 := fastTwoSum(L[0], sh)
	v1, v2 := fastTwoSum(v2, sl)
	v0 = float64(v0 * 2)
	v1 = float64(v1 * 2)
	v2 = float64(v2 * 2)
	v1 = roundAway(v1, v2)
	t, t0 := asuint64(v1), asuint64(v0)
	er := (t + 7) & (^uint64(0) >> 12)
	de := (t0>>52)&0x7ff - (t>>52)&0x7ff
	res := v0 + v1
	if de > 102 || er < 15 {
		return acoshDatabase(x, res)
	}
	return res
}

var acoshOneCH = [...][2]float64{
	{-0x1.5555555555555p-4, -0x1.5555555554af1p-58}, {0x1.3333333333333p-6, 0x1.9999998933f0ep-61},
	{-0x1.6db6db6db6db7p-8, 0x1.24929b16ec6b7p-63}, {0x1.f1c71c71c71c7p-10, 0x1.c56d45e265e2cp-66},
	{-0x1.6e8ba2e8ba2e9p-11, 0x1.6d50ce7188d3dp-65}, {0x1.1c4ec4ec4ec43p-12, 0x1.c6791d1cf399ap-66},
	{-0x1.c99999999914fp-14, 0x1.ee0d9408a2e2ap-68}, {0x1.7a878787648e2p-15, -0x1.1cea281e08012p-69},
	{-0x1.3fde50d0cb4b9p-16, 0x1.0335101403d9dp-72}, {0x1.12ef3bf8a0a74p-17, 0x1.f9c6b51787043p-80}}

var acoshOneCL = [...]float64{
	-0x1.df3b9d1296ea9p-19, 0x1.a681d7d2298ebp-20, -0x1.77ead7b1ca449p-21, 0x1.4edd2ddb3721fp-22,
	-0x1.1bf173531ee23p-23, 0x1.613229230e255p-25}

var acoshNearCL = [...]float64{
	-0x1.5555555555555p-4, 0x1.3333333332f95p-6, -0x1.6db6db6d5534cp-8, 0x1.f1c71c1e04356p-10,
	-0x1.6e8b8e3e40d58p-11, 0x1.1c4ba825ac4fep-12, -0x1.c9045534e6d9ep-14, 0x1.71fedae26a76bp-15,
	-0x1.f1f4f8cc65342p-17}

var acoshDB = [...][3]float64{
	{0x1.5bff041b260fep+0, 0x1.a6031cd5f93bap-1, 0x1p-55},
	{0x1.9efdca62b700ap+0, 0x1.104b648f113a1p+0, 0x1p-54},
	{0x1.a5bf3acfde4b2p+0, 0x1.1585720f35cd9p+0, -0x1p-54},
	{0x1.45ea160ddc71fp+7, 0x1.725811dcf6782p+2, 0x1p-52},
	{0x1.2a686e4b567cep+10, 0x1.f1c928e7f1e65p+2, 0x1p-52},
	{0x1.cb62eec26bd78p+15, 0x1.759a2ad4c4d56p+3, 0x1p-51},
	{0x1.3bf8009648dcp+16, 0x1.7fce95ea5c653p+3, -0x1p-53},
}
