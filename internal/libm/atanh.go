// Ported from CORE-MATH, src/binary64/atanh/atanh.c, which glibc 2.43
// carries as sysdeps/ieee754/dbl-64/e_atanh.c. MIT licensed: see coremath.go.

package libm

import "math"

// Atanh is glibc's atanh, CORE-MATH's correctly rounded one.
func Atanh(x float64) float64 {
	ax := math.Abs(x)
	aix := asuint64(ax)
	if aix >= 0x3ff0000000000000 {
		// |x| >= 1.
		switch {
		case aix == 0x3ff0000000000000:
			return math.Copysign(math.Inf(1), x)
		case aix > 0x7ff0000000000000:
			return x + x
		}
		return math.NaN()
	}

	if aix < 0x3fd0000000000000 {
		// |x| < 0x1p-2.
		if aix < 0x3e4d12ed0af1a27f {
			// |x| < 0x1.d12ed0af1a27fp-27: atanh(x) rounds to x.
			return math.FMA(x, 0x1p-55, x)
		}
		// x + (0x1.5555555555555p-2 + 0x1.5555555555555p-56) x^3 + x^5 c(x)
		// approximates atanh(x) with relative error <= 2^-64.377.
		c := &atanhSmallC
		x2 := x * x
		dx2 := math.FMA(x, x, -x2)
		x4, x3 := x2*x2, x2*x
		x8 := x4 * x4
		dx3 := math.FMA(x2, x, -x3) + dx2*x
		p := (c[0] + x2*c[1]) + x4*(c[2]+x2*c[3]) + x8*((c[4]+x2*c[5])+x4*(c[6]+x2*c[7])+x8*c[8])
		t := math.FMA(x2, p, 0x1.5555555555555p-56)
		ph, pl := fastTwoSum(0x1.5555555555555p-2, t)
		ph, pl = atanhMulDD(ph, pl, x3, dx3)
		ph, tl := fastTwoSum(x, ph)
		pl += tl
		eps := x * (x4*0x1.dp-53 + 0x1p-103)
		lb, ub := ph+(pl-eps), ph+(pl+eps)
		if lb == ub {
			return lb
		}
		return atanhZero(x)
	}

	ph, pl := fastTwoSum(1, ax)
	qh, ql := fastTwoSub(1, ax)
	iqh := 1 / qh
	th := ph * iqh
	tl := math.FMA(ph, iqh, -th) + (pl+ph*(math.FMA(-qh, iqh, 1)-ql*iqh))*iqh

	t := asuint64(th)
	ex := int(t >> 52)
	e := ex - 0x3ff
	t &= ^uint64(0) >> 12
	ed := float64(e)
	i := t >> (52 - 5)
	d := int64(t & (^uint64(0) >> 17))
	j := (t + uint64(cmLogB[i].c0)<<33 + uint64(int64(cmLogB[i].c1)*(d>>16))) >> (52 - 10)
	t |= 0x3ff << 52
	i1, i2 := j>>5, j&0x1f
	tf := asdouble(t)
	r := (0.5 * cmLogR1[i1]) * cmLogR2[i2]
	dx := math.FMA(r, tf, -0.5)
	dx2 := dx * dx
	rx := r * tf
	dxl := math.FMA(r, tf, -rx)
	c := &atanhC
	f := dx2 * ((c[0] + dx*c[1]) + dx2*(c[2]+dx*c[3]+dx2*c[4]))
	const l2h, l2l = 0x1.62e42fefa3ap-2, -0x1.0ca86c3898dp-50
	lh := (atanhL1[i1][1] + atanhL2[i2][1]) + l2h*ed
	lh, ll := fastTwoSum(lh, rx-0.5)
	ll += l2l*ed + (atanhL1[i1][0] + atanhL2[i2][0]) + dxl + 0.5*tl/th
	ll += f
	lh *= math.Copysign(1, x)
	ll *= math.Copysign(1, x)
	eps := 38e-24 + dx2*0x1p-49
	lb, ub := lh+(ll-eps), lh+(ll+eps)
	if lb == ub {
		return lb
	}
	th, tl = fastTwoSum(th, tl)
	return atanhRefine(x, th, tl, 0x1.71547652b82fep+1*math.Abs(lh+ll))
}

// fastTwoSub is s = x-y rounded and the error e = x-y-s, exact when |x| >= |y|.
func fastTwoSub(x, y float64) (s, e float64) {
	s = x - y
	z := x - s
	return s, z - y
}

// atanhMulDD is (xh+xl)*(ch+cl) as h+l, h not renormalized.
func atanhMulDD(xh, xl, ch, cl float64) (h, l float64) {
	ahlh, alhh, ahhh := ch*xl, cl*xh, ch*xh
	ahhl := math.FMA(ch, xh, -ahhh)
	return ahhh, ahhl + (alhh + ahlh)
}

func atanhMulDDD(xh, xl, ch float64) (h, l float64) {
	hh := xh * ch
	return hh, math.FMA(ch, xh, -hh) + xl*ch
}

func atanhZero(x float64) float64 {
	cl := &atanhZeroCL
	x2 := x * x
	x2l := math.FMA(x, x, -x2)
	y2 := x2 * (cl[0] + x2*(cl[1]+x2*(cl[2]+x2*(cl[3]+x2*cl[4]))))
	y1, y2 := acoshPolyDD(x2, x2l, atanhZeroCH[:], y2)
	y1, y2 = atanhMulDDD(y1, y2, x)
	y1, y2 = acoshMulDDAcc(x2, x2l, y1, y2)
	y0, y1 := fastTwoSum(x, y1)
	y1, y2 = fastTwoSum(y1, y2)
	return y0 + roundAway(y1, y2)
}

func atanhDatabase(x, f float64) float64 {
	ax, sgn := math.Abs(x), math.Copysign(1, x)
	a, b := 0, len(atanhDB)-1
	for a <= b {
		m := (a + b) / 2
		switch {
		case atanhDB[m][0] < ax:
			a = m + 1
		case atanhDB[m][0] == ax:
			return sgn*atanhDB[m][1] + sgn*atanhDB[m][2]
		default:
			b = m - 1
		}
	}
	return f
}

func atanhRefine(x, zh, zl, a float64) float64 {
	t := asuint64(zh)
	ex := int(t >> 52)
	e := ex - 0x3ff
	t &= ^uint64(0) >> 12
	t |= 0x3ff << 52
	ed := float64(e)
	v := asuint64(a - ed + 0x1.00008p+0)
	i := (v - 0x3ff<<52) >> (52 - 16)
	i1, i2, i3, i4 := i>>12, (i>>8)&0xf, (i>>4)&0xf, i&0xf
	const l20, l21, l22 = 0x1.62e42fefa3ap-2, -0x1.0ca86c3898dp-50, 0x1.f97b57a079ap-104
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
	xl += th * asdouble(asuint64(zl)-uint64(int64(e)<<52))
	xh, xl = asinhAddDD(xh, xl, sh, sl)
	sl = xh * (-0x1p-3 + xh*(0x1.9999999a0754fp-4+xh*-0x1.55555555c3157p-4))
	sh, sl = acoshPolyDD(xh, xl, cmLogCH[:], sl)
	sh, sl = acoshMulDDAcc(xh, xl, sh, sl)
	sh, sl = asinhAddDD(sh, sl, el1, el2)
	sh, sl = asinhAddDD(sh, sl, L[1], L[2])
	v0, v2 := fastTwoSum(L[0], sh)
	v1, v2 := fastTwoSum(v2, sl)
	t = asuint64(v1)
	if t&(^uint64(0)>>12) == 0 {
		// v1 is a power of 2, and v1 and v2 have the same sign.
		t++
		v1 = asdouble(t)
	}
	t0 := asuint64(v0)
	er := (t + 1) & (^uint64(0) >> 12)
	de := (t0>>52)&0x7ff - (t>>52)&0x7ff
	v0 *= math.Copysign(1, x)
	v1 *= math.Copysign(1, x)
	res := v0 + v1
	if de > 104 || er < 3 {
		return atanhDatabase(x, res)
	}
	return res
}

var atanhZeroCH = [...][2]float64{
	{0x1.5555555555555p-2, 0x1.5555555555555p-56},  // degree 3
	{0x1.999999999999ap-3, -0x1.999999999611cp-57}, // degree 5
	{0x1.2492492492492p-3, 0x1.2492490f76b25p-57},  // degree 7
	{0x1.c71c71c71c71cp-4, 0x1.c71cd5c38a112p-58},  // degree 9
	{0x1.745d1745d1746p-4, -0x1.7556c4165f4cap-59}, // degree 11
	{0x1.3b13b13b13b14p-4, -0x1.b893c3b36052ep-59}, // degree 13
	{0x1.1111111111105p-4, 0x1.4e1afd723ed1fp-59},  // degree 15
	{0x1.e1e1e1e1e2678p-5, -0x1.f86ea96fb1435p-59}, // degree 17
	{0x1.af286bc9f90ccp-5, 0x1.1e51a6e54fde9p-60},  // degree 19
	{0x1.8618618c779b6p-5, -0x1.ab913de95c3bfp-61}, // degree 21
	{0x1.642c84aa383ebp-5, 0x1.632e747641b12p-59},  // degree 23
	{0x1.47ae2d205013cp-5, -0x1.0c9617e7bcff2p-60}, // degree 25
	{0x1.2f664d60473f9p-5, 0x1.3adb3e2b7f35ep-61}}

var atanhZeroCL = [...]float64{
	0x1.1a9a91fd692afp-5, // degree 29
	0x1.06dfbb35e7f44p-5, // degree 31
	0x1.037bed4d7588fp-5, // degree 33
	0x1.5aca6d6d720d6p-6, // degree 35
	0x1.99ea5700d53a5p-5}

var atanhSmallC = [...]float64{0x1.999999999999ap-3, 0x1.2492492492244p-3, 0x1.c71c71c79715fp-4, 0x1.745d16f777723p-4,
	0x1.3b13ca4174634p-4, 0x1.110c9724989bdp-4, 0x1.e2d17608a5b2ep-5, 0x1.a0b56308cba0bp-5, 0x1.fb6341208ad2ep-5}

var atanhC = [...]float64{-0x1p+0, 0x1.555555555553p+0, -0x1.fffffffffffap+0, 0x1.99999e33a6366p+1, -0x1.555559ef9525fp+2}

var atanhL1 = [...][2]float64{
	{0x0p+0, 0x0p+0}, {-0x1.532c1269e2038p-27, 0x1.62e5p-7},
	{0x1.ce42d81b54e84p-27, 0x1.62e3cp-6}, {-0x1.25826f815ec3dp-26, 0x1.0a2acp-5},
	{0x1.0db1b1e7cee11p-26, 0x1.62e4ap-5}, {-0x1.1f3a8c6c95003p-26, 0x1.bb9dcp-5},
	{-0x1.774cd4fb8c30dp-26, 0x1.0a2b2p-4}, {0x1.452e56c030a0ap-29, 0x1.3687fp-4},
	{0x1.6b63c4966a79ap-28, 0x1.62e41p-4}, {-0x1.b20a21ccb525ep-28, 0x1.8f40ap-4},
	{0x1.4006cfb3d8f85p-26, 0x1.bb9d1p-4}, {-0x1.cdb026b310c41p-26, 0x1.e7f9bp-4},
	{-0x1.69124fdc0f16dp-26, 0x1.0a2b08p-3}, {-0x1.084656cdc2727p-26, 0x1.205958p-3},
	{-0x1.376fa8b0357fdp-26, 0x1.3687cp-3}, {0x1.e56ae55a47b4ap-28, 0x1.4cb5e8p-3},
	{0x1.070ff8834eeb4p-26, 0x1.62e44p-3}, {0x1.623516109f4fep-26, 0x1.79129p-3},
	{-0x1.ec656b95fbdacp-29, 0x1.8f40bp-3}, {0x1.f0ca2e729f51p-28, 0x1.a56ed8p-3},
	{-0x1.7d260a858354ap-26, 0x1.bb9d68p-3}, {0x1.e7279075503d3p-27, 0x1.d1cb9p-3},
	{0x1.39e1a0a503873p-27, 0x1.e7f9dp-3}, {0x1.cd86d7b87c3d6p-26, 0x1.fe27d8p-3},
	{0x1.060ab88de341ep-26, 0x1.0a2b24p-2}, {0x1.20a860d3f939p-28, 0x1.154244p-2},
	{-0x1.dacee95fc2f1p-27, 0x1.205974p-2}, {0x1.45de3a86e0acap-26, 0x1.2b707p-2},
	{0x1.c164cbfb991afp-27, 0x1.3687bp-2}, {0x1.d3f66b24225efp-26, 0x1.419ec4p-2},
	{0x1.fc023efa144bap-26, 0x1.4cb5f8p-2}, {0x1.086a8af6f26cp-28, 0x1.57cd28p-2},
	{-0x1.05c610ca86c39p-30, 0x1.62e43p-2}}

var atanhL2 = [...][2]float64{
	{0x0p+0, 0x0p+0}, {-0x1.37e152a129e4ep-28, 0x1.632p-12},
	{-0x1.3f6c916b8be9cp-26, 0x1.63p-11}, {0x1.20505936739d5p-26, 0x1.0a24p-10},
	{-0x1.23e2e8cb541bap-26, 0x1.62dcp-10}, {-0x1.acb7983ac4f5ep-32, 0x1.bbap-10},
	{0x1.6f7c7689c63aep-28, 0x1.0a2ap-9}, {0x1.f5ca695b4c58bp-30, 0x1.368cp-9},
	{-0x1.c6c18bd953226p-27, 0x1.62e6p-9}, {0x1.7a516c34846bdp-26, 0x1.8f46p-9},
	{-0x1.f3b83dd8b853p-27, 0x1.bbap-9}, {-0x1.c3459046e4e57p-31, 0x1.e8p-9},
	{0x1.b5c7e34cb79f6p-38, 0x1.0a2cp-8}, {-0x1.2487e9af9a692p-27, 0x1.205cp-8},
	{0x1.f21bbc4ad79cep-26, 0x1.3687p-8}, {-0x1.550ffc857b731p-29, 0x1.4cb7p-8},
	{0x1.87458ec1b7b34p-27, 0x1.62e2p-8}, {0x1.103d4fe83ee81p-26, 0x1.7911p-8},
	{0x1.810483d3b398cp-27, 0x1.8f44p-8}, {-0x1.2085cb340608ep-27, 0x1.a573p-8},
	{0x1.12698a119c42fp-26, 0x1.bb9dp-8}, {-0x1.edb8c172b4c33p-26, 0x1.d1ccp-8},
	{-0x1.8b55b87a5e238p-26, 0x1.e7fep-8}, {0x1.be5e17763f78ap-26, 0x1.fe2bp-8},
	{-0x1.c2d496790073ep-30, 0x1.0a2a8p-7}, {0x1.6542f523abeecp-26, 0x1.1541p-7},
	{-0x1.b7fdbe5b193f8p-26, 0x1.205ap-7}, {0x1.fa4d42fe30c7cp-26, 0x1.2b7p-7},
	{0x1.0d46ad04adc86p-26, 0x1.36888p-7}, {-0x1.1c22d02d17c4cp-26, 0x1.419fp-7},
	{0x1.a7d1e330dcccep-30, 0x1.4cb7p-7}, {0x1.187025e656ba3p-31, 0x1.57cdp-7},
	{-0x1.532c1269e2038p-27, 0x1.62e5p-7}}

var atanhDB = [...][3]float64{
	{0x1.2dbb7b1c91363p-2, 0x1.36f33d51c264dp-2, 0x1p-56},
	{0x1.c493dc899e4a5p-2, 0x1.e611aa58ab608p-2, -0x1p-56},
}
