// Ported from CORE-MATH, src/binary64/asin/asin.c and acos/acos.c, for
// glibc's asin and acos, which are IBM's. MIT licensed: see coremath.go.

package libm

import "math"

// Asin is asin, correctly rounded: glibc's except where glibc is not.
func Asin(x float64) float64 {
	ix := asuint64(x)
	ax := ix << 1
	var t, z, zl, jd, f0h, f0l, eps float64
	if ax > 0x7fc0000000000000 {
		// |x| > 0.5.
		k := ix >> 63
		f0h, f0l = asinOff[k][0], asinOff[k][1]
		if ax >= 0x7fe0000000000000 {
			// |x| >= 1.
			switch {
			case ax == 0x7fe0000000000000:
				return f0h + f0l
			case ax > 0xffe0000000000000:
				return x + x
			}
			return math.NaN()
		}
		// asin(x) = pi/2 - 2*asin(sqrt((1-x)/2)).
		t = 2 - 2*math.Abs(x)
		jd = math.RoundToEven(t * 0x1p5)
		z = math.Copysign(math.Sqrt(t), -x)
		zl = math.FMA(z, z, -t) * ((-0.5 / t) * z)
		t = 0.25*t - jd*0x1p-7
		eps = math.Abs(z*t) * 0x1.99p-52
	} else {
		if ax < 0x7cae26e892247dec {
			// |x| < 0x1.7137449123ef6p-26: asin(x) rounds to x.
			return math.FMA(0x1p-55, x, x)
		}
		t = x * x
		jd = math.RoundToEven(t * 0x1p7)
		t = math.FMA(x, x, -0x1p-7*jd)
		z = x
		eps = math.Abs(z*t) * 0x1.77p-52
	}
	fh, fl := asinPoly(t, z, zl, jd, f0h, f0l)
	lb, ub := fh+(fl-eps), fh+(fl+eps)
	if lb != ub {
		return asinRefine(x, lb)
	}
	return lb
}

// Acos is acos, correctly rounded: glibc's except where glibc is not.
func Acos(x float64) float64 {
	ix := asuint64(x)
	ax := ix << 1
	var t, z, zl, jd, f0h, f0l, eps float64
	if ax > 0x7fc0000000000000 {
		// |x| > 0.5.
		if ix>>63 != 0 {
			f0h, f0l = 0x1.921fb54442d18p+1, 0x1.1a62633145c07p-53
		}
		if ax >= 0x7fe0000000000000 {
			// |x| >= 1.
			switch {
			case ax == 0x7fe0000000000000:
				return f0h + f0l
			case ax > 0xffe0000000000000:
				return x + x
			}
			return math.NaN()
		}
		// acos(x) = 2*asin(sqrt((1-x)/2)) for x > 0.5 and
		// pi - 2*asin(sqrt((1+x)/2)) for x < -0.5.
		t = 2 - 2*math.Abs(x)
		jd = math.RoundToEven(t * 0x1p5)
		z = math.Copysign(math.Sqrt(t), x)
		zl = math.FMA(z, z, -t) * ((-0.5 / t) * z)
		t = 0.25*t - jd*0x1p-7
		eps = math.Abs(z*t)*0x1.8cp-52 + 0x1p-105
	} else {
		f0h, f0l = 0x1.921fb54442d18p+0, 0x1.1a62633145c07p-54
		if ax <= 0x7e00000000000000 {
			// |x| < 2^-15.
			var v float64
			if ax > 0x791967670d2e8fe8 {
				v = (x * x) * (-0x1.5555555555555p-3 * x)
			}
			h, w := fastTwoSum(f0h, -x)
			l := v + (w + f0l)
			const eps1 = 0x1.34p-79
			lb, ub := h+(l-eps1), h+(l+eps1)
			if lb != ub {
				return acosRefine(x, lb)
			}
			return lb
		}
		// acos(x) = pi/2 - asin(x), so both branches use the same tables.
		t = x * x
		jd = math.RoundToEven(t * 0x1p7)
		t = math.FMA(x, x, -0x1p-7*jd)
		z = -x
		eps = (z * t) * 0x1.81p-52
	}
	fh, fl := asinPoly(t, z, zl, jd, f0h, f0l)
	lb, ub := fh+(fl-eps), fh+(fl+eps)
	if lb != ub {
		return acosRefine(x, lb)
	}
	return lb
}

// asinPoly is f0h+f0l + (z+zl)*(cc[j][0] + cc[j][1] + t*P(t)) with
// j = jd: asin(z+zl) added to an offset, for t = z^2 - j/128.
func asinPoly(t, z, zl, jd, f0h, f0l float64) (fh, fl float64) {
	c := &asinCC[int64(jd)]
	t2 := t * t
	d := t * ((c[2] + t*c[3]) + t2*((c[4]+t*c[5])+t2*(c[6]+t*c[7])))
	fh, fl = sinMulDD(z, zl, c[0], c[1]+d)
	return fastSum(f0h, f0l, fh, fl)
}

func asinPolyDD(xh, xl float64, c [][2]float64, l float64) (h, lo float64) {
	i := len(c) - 1
	ch, l := fastTwoSum(c[i][0], l)
	cl := c[i][1] + l
	for i--; i >= 0; i-- {
		ch, cl = sinMulDD(xh, xl, ch, cl)
		ch, cl = fastSum(c[i][0], c[i][1], ch, cl)
	}
	return ch, cl
}

// asinRotate is the low part of asin's and acos's accurate paths: with
// ch+cl = sqrt(1-x^2), the rotation by j*pi/64 that brings the argument
// near zero, as v+dv, and that rotation's sine and cosine.
func asinRotate(ax, ch, cl float64, j int64) (v, dv float64) {
	Ch, Cl, Sh, Sl := asinS[32-j][1], asinS[32-j][0], asinS[j][1], asinS[j][0]
	dsh, dsl := ax-Sh, -Sl
	dch, dcl := ch-Ch, cl-Cl
	const magic = 0x1.8p-4
	sc := math.FMA(Sh, dch, magic) - magic
	dsc := math.FMA(Sh, dch, -sc)
	cs := math.FMA(Ch, dsh, magic) - magic
	dcs := math.FMA(Ch, dsh, -cs)
	v = cs - sc
	dv = (Ch*dsl + Cl*dsh) - (Sh*dcl + Sl*dch) - (dsc - dcs)
	return fastTwoSum(v, dv)
}

// asinSqrt1m is sqrt(1-x^2) as ch+cl.
func asinSqrt1m(x float64) (ch, cl float64) {
	s2 := x * x
	dx2 := math.FMA(x, x, -s2)
	c2h, c2l := fastTwoSum(1.0, -s2)
	c2l -= dx2
	c2h, c2l = fastTwoSum(c2h, c2l)
	ch = math.Sqrt(c2h)
	return ch, (c2l - math.FMA(ch, ch, -c2h)) * (0.5 / ch)
}

// asinSeries is asin(v+dv) for |v| small, times sgn.
func asinSeries(v, dv, sgn float64) (fh, fl float64) {
	v2, dv2 := sinMulDD(v, dv, v, dv)
	v *= sgn
	dv *= sgn
	fl = v2 * (0x1.6e8ba2ec8cb69p-6 + v2*(0x1.1c4ea7a15c997p-6+v2*0x1.ca8355d39bb67p-7))
	fh, fl = asinPolyDD(v2, dv2, asinC[:], fl)
	return sinMulDD(v, dv, fh, fl)
}

// asinPiTimes is jt*pi/64 + fh+fl as ph+pl+ps, for 0 <= jt <= 64; sum is
// how fh+fl is added in.
func asinPiTimes(jt, fh, fl float64, sum func(xh, xl, yh, yl float64) (float64, float64)) (ph, pl, ps float64) {
	ph = jt * 0x1.921fb54442dp-5
	pl = 0x1.8469898cc518p-53 * jt
	ps = -0x1.fc8f8cbb5bf6cp-102 * jt
	pl, ps = sum(fh, fl, pl, ps)
	ph, pl = fastTwoSum(ph, pl)
	pl, ps = fastTwoSum(pl, ps)
	ph, pl = fastTwoSum(ph, pl)
	pl, ps = fastTwoSum(pl, ps)
	return ph, pl, ps
}

// asinRefine is asin(x) where phi is the fast path's approximation of it.
func asinRefine(x, phi float64) float64 {
	ch, cl := asinSqrt1m(x)
	jf := math.RoundToEven(math.Abs(phi) * 0x1.45f306dc9c883p+4)
	v, dv := asinRotate(math.Abs(x), ch, cl, int64(jf))
	sgn := math.Copysign(1.0, x)
	fh, fl := asinSeries(v, dv, sgn)
	ph, pl, _ := asinPiTimes(jf*sgn, fh, fl, fastSum)
	th, tl := asuint64(ph), asuint64(pl)
	tn := th&(0x7ff<<52) - 53<<52
	tl &= ^uint64(0) >> 1
	gap := tn - tl
	res := ph + pl
	if gap <= 2 || gap>>52 > 47 {
		return asinDatabase(x, res)
	}
	return res
}

func asinDatabase(x, f float64) float64 {
	ax := math.Abs(x)
	a, b := 0, len(asinDB)
	for a+1 < b {
		m := (a + b) / 2
		if asinDB[m][0] <= ax {
			a = m
		} else {
			b = m
		}
	}
	switch {
	case asinDB[a][0] != ax:
		return f
	case x > 0:
		return asinDB[a][1] + asinDB[a][2]
	}
	return -asinDB[a][1] - asinDB[a][2]
}

// acosRefine is acos(x) where phi is the fast path's approximation of it.
func acosRefine(x, phi float64) float64 {
	ch, cl := asinSqrt1m(x)
	jf := int64(math.RoundToEven(math.Abs(phi-0x1.921fb54442d18p+0) * 0x1.45f306dc9c883p+4))
	v, dv := asinRotate(math.Abs(x), ch, cl, jf)
	sgn := math.Copysign(1.0, x)
	fh, fl := asinSeries(v, dv, -sgn)
	ph, pl, ps := asinPiTimes(32-float64(jf)*sgn, fh, fl, sum)
	t := asuint64(pl)
	e := int64((t>>52)&0x7ff) - 1023
	e = min(max(52-(107+e), 0), 52)
	m := uint64(1)<<52 - uint64(1)<<e
	if e == 0 {
		e = 64
	}
	if (t+uint64(1)<<(e-1))&m == 0 {
		switch x {
		case 0x1.ffffffffffdc0p-1:
			return 0x1.8000000000024p-22 + 0x1p-76
		case 0x1.53ea6c7255e88p-4:
			return 0x1.7cdacb6bbe707p+0 + 0x1p-54
		case 0x1.fd737be914578p-11:
			return 0x1.91e006d41d8d8p+0 + 0x1.8p-53
		case 0x1.fffffffffff70p-1:
			return 0x1.8000000000009p-23 + 0x1p-77
		case 0x1.390e6939cd1a6p-5:
			return 0x1.8856a5d3296a4p+0 - 0x1p-109
		}
		if (asuint64(ps)^t)>>63 != 0 {
			t--
		} else {
			t++
		}
		pl = asdouble(t)
	}
	return ph + pl
}

// sum is xh+yh as sh and the rest of (xh+xl)+(yh+yl) as l, for any xh, yh.
func sum(xh, xl, yh, yl float64) (sh, l float64) {
	sh, sl := twoSum(xh, yh)
	return sh, (xl + yl) + sl
}

var asinCC = [...][8]float64{
	{1, 0, 0x1.5555555555555p-3, 0x1.33333333333e4p-4,
		0x1.6db6db6d31f82p-5, 0x1.f1c71f6889397p-6, 0x1.6e874b7045b46p-6, 0x1.1f753132271e2p-6},
	{0x1.0055a27e0d033p+0, -0x1.d9ba10494c062p-54, 0x1.57c00cb5d6c4dp-3, 0x1.37881f5649a75p-4,
		0x1.759af49d494ddp-5, 0x1.002e1864dda2ep-5, 0x1.7c2d5d468cdd9p-6, 0x1.292834c025357p-6},
	{0x1.00abe0c129e1ep+0, 0x1.7ceb0ee49d42ap-57, 0x1.5a3385d5c7ba5p-3, 0x1.3bf51056f6637p-4,
		0x1.7dba76b124b37p-5, 0x1.07be4b02e94c4p-5, 0x1.8a6bb92513f01p-6, 0x1.36afd4c615aecp-6},
	{0x1.0102bcffd6acdp+0, -0x1.c2294c65d2e86p-55, 0x1.5caff17351901p-3, 0x1.407abbc04feb3p-4,
		0x1.86179b8005949p-5, 0x1.0f97520bd4e72p-5, 0x1.9950c5c89f3dfp-6, 0x1.44f2344e7b664p-6},
	{0x1.015a397cf0f1cp+0, -0x1.eebd6ccfe3ee3p-55, 0x1.5f3581be7b08bp-3, 0x1.4519ddf1ae531p-4,
		0x1.8eb4b6ee35e92p-5, 0x1.17bc85414cd46p-5, 0x1.a8e5895e3fcf9p-6, 0x1.53fafdc629400p-6},
	{0x1.01b2588811eebp+0, 0x1.7193e5d0a915fp-59, 0x1.61c46a67205d2p-3, 0x1.49d33a6eeae0bp-4,
		0x1.979438563c014p-5, 0x1.20316ae977f05p-5, 0x1.b9339afb53aa4p-6, 0x1.63d6b02c42d0ap-6},
	{0x1.020b1c7df0575p+0, -0x1.dd547e329c1e5p-55, 0x1.645ce0ab901bbp-3, 0x1.4ea79c34fc7a6p-4,
		0x1.a0b8ac08940ecp-5, 0x1.28f9babd0629bp-5, 0x1.ca452cf90a55ep-6, 0x1.7492b016730efp-6},
	{0x1.026487c8c5d71p+0, -0x1.5fd9b68dc3b6ep-54, 0x1.66ff1b67d5d70p-3, 0x1.5397d613373ebp-4,
		0x1.aa24bce3aeb4ap-5, 0x1.3219610b150acp-5, 0x1.dc251825103f1p-6, 0x1.863d5a3932532p-6},
	{0x1.02be9ce0b87cdp+0, 0x1.e5d09da2e0f04p-56, 0x1.69ab5325bc359p-3, 0x1.58a4c3097aab3p-4,
		0x1.b3db3605f46f2p-5, 0x1.3b94821742cabp-5, 0x1.eedee7da72a15p-6, 0x1.98e6179a3e9a0p-6},
	{0x1.03195e4c483f1p+0, -0x1.5db10ad66eacbp-54, 0x1.6c61c22d908f0p-3, 0x1.5dcf46ab9f2cbp-4,
		0x1.bddf049bb1f4dp-5, 0x1.456f7db6ac768p-5, 0x1.013f738bd7bb3p-5, 0x1.ac9d739783d21p-6},
	{0x1.0374cea0c0c9fp+0, -0x1.917bff5241c76p-54, 0x1.6f22a497b2ec0p-3, 0x1.63184d8a79db5p-4,
		0x1.c83339caf946ep-5, 0x1.4faef331019d4p-5, 0x1.0b8917547d678p-5, 0x1.c17533f147e1cp-6},
	{0x1.03d0f082afcc8p+0, -0x1.018bbcddb49ebp-54, 0x1.71ee385efdf06p-3, 0x1.6880cda2d3885p-4,
		0x1.d2db0cbfae54dp-5, 0x1.5a57c56b50c5ep-5, 0x1.16535a40098b2p-5, 0x1.d780730b8ebb8p-6},
	{0x1.042dc6a65ffbfp+0, -0x1.c7ea28dce95d1p-55, 0x1.74c4bd7412f9ep-3, 0x1.6e09c6d2b72bcp-4,
		0x1.ddd9dcda253dep-5, 0x1.656f1f62b5001p-5, 0x1.21a5ae2ac77eep-5, 0x1.eed3bca067f0ep-6},
	{0x1.048b53d05907bp+0, 0x1.634fffed6e2a6p-54, 0x1.77a675d1978bep-3, 0x1.73b4435583415p-4,
		0x1.e9333402ebbf3p-5, 0x1.70fa78fd9f73fp-5, 0x1.2d8804d934fe1p-5, 0x1.03c29691a281cp-5},
	{0x1.04e99ad5e4bcdp+0, -0x1.e97a72fe827e0p-54, 0x1.7a93a5917200cp-3, 0x1.7981584731c05p-4,
		0x1.f4eac9268fae2p-5, 0x1.7cff9c3b19721p-5, 0x1.3a02d9c1e0145p-5, 0x1.10d64a0e56953p-5},
	{0x1.05489e9d99995p+0, 0x1.d177637ec6a2bp-55, 0x1.7d8c930314681p-3, 0x1.7f72262f532e4p-4,
		0x1.0082416e39013p-4, 0x1.8984aac80ddf4p-5, 0x1.471f3caf18eb8p-5, 0x1.1eb1cce6dd570p-5},
	{0x1.05a8621feb16bp+0, -0x1.e5b33b1407c5fp-56, 0x1.809186c2e57ddp-3, 0x1.8587d99442dc8p-4,
		0x1.06c23d1dfcb7fp-4, 0x1.969024036dd22p-5, 0x1.54e6dd4d2af33p-5, 0x1.2d62f439f2a31p-5},
	{0x1.0608e867bff30p+0, 0x1.cbef5d8580027p-55, 0x1.83a2cbd2d8ba2p-3, 0x1.8bc3ab9724c6ep-4,
		0x1.0d377ef1e0c39p-4, 0x1.a428eb7addf84p-5, 0x1.636417bc01ff2p-5, 0x1.3cf8acc7eb2a0p-5},
	{0x1.066a34930ec8dp+0, -0x1.480f445fedad1p-54, 0x1.86c0afb447a74p-3, 0x1.9226e29948d9cp-4,
		0x1.13e44a9b5a3a6p-4, 0x1.b2564fea8b3fep-5, 0x1.72a2023d92458p-5, 0x1.4d8313cec3485p-5},
	{0x1.06cc49d38146cp+0, -0x1.b55394f4fc07bp-55, 0x1.89eb82831feedp-3, 0x1.98b2d2eb9bb23p-4,
		0x1.1acb01e9ab414p-4, 0x1.c12012cbd00c6p-5, 0x1.82ac7c1d15c38p-5, 0x1.5f13925c6edcap-5},
	{0x1.072f2b6f1e601p+0, -0x1.2dcbb05419970p-54, 0x1.8d2397127aebbp-3, 0x1.9f68df88da51dp-4,
		0x1.21ee26a4f62a1p-4, 0x1.d08e707f7ae6fp-5, 0x1.93903dee3feb0p-5, 0x1.71bcfb5c57b59p-5},
	{0x1.0792dcc0fbd20p+0, -0x1.5bf23ee4f9d54p-56, 0x1.9069430ab508ap-3, 0x1.a64a7adb4cd85p-4,
		0x1.29505c8b48349p-4, 0x1.e0aa2921cfa60p-5, 0x1.a55aeb46f4322p-5, 0x1.8593acad3becep-5},
	{0x1.07f76139f761dp+0, 0x1.fa1046481bb82p-54, 0x1.93bcdf091cca6p-3, 0x1.ad59278edc42fp-4,
		0x1.30f46b7261652p-4, 0x1.f17c8a17c843ep-5, 0x1.b81b2619e15b5p-5, 0x1.9aadb395f5ae4p-5},
	{0x1.085cbc61783c1p+0, 0x1.0a6e9efa20176p-54, 0x1.971ec6c1531e4p-3, 0x1.b496797068912p-4,
		0x1.38dd419140184p-4, 0x1.0187bc3357bbbp-4, 0x1.cbe0a3dcafe26p-5, 0x1.b122f4fa499d0p-5},
	{0x1.08c2f1d638e4cp+0, 0x1.b47c159534a3dp-56, 0x1.9a8f592078624p-3, 0x1.bc04165b57ab2p-4,
		0x1.410df5f4bed1dp-4, 0x1.0ab6bdf478c71p-4, 0x1.e0bc44a945c64p-5, 0x1.c90d59bcd5701p-5},
	{0x1.092a054f1a2fcp+0, -0x1.2f657224e9830p-54, 0x1.9e0ef87243a2cp-3, 0x1.c3a3b7366a278p-4,
		0x1.4989cb22e2175p-4, 0x1.1450e5ba7ad39p-4, 0x1.f6c02c8f0ef93p-5, 0x1.e288ffc8d182cp-5},
	{0x1.0991fa9bffbf4p+0, -0x1.ca1140a1abbf4p-58, 0x1.a19e0a8823b80p-3, 0x1.cb772900f9c24p-4,
		0x1.525431f0cbb2ep-4, 0x1.1e5c2d06804e1p-4, 0x1.06ffefa7aa6b8p-4, 0x1.fdb4704dca347p-5},
	{0x1.09fad5a6b68f9p+0, 0x1.aa1f06e92964ep-56, 0x1.a53cf8e28c50ep-3, 0x1.d3804df1de350p-4,
		0x1.5b70cc8fa98dcp-4, 0x1.28def298c979bp-4, 0x1.13482f6347eebp-4, 0x1.0d586de48358cp-4},
	{0x1.0a649a73e61f2p+0, 0x1.74ac0d817e9c7p-55, 0x1.a8ec30dc93891p-3, 0x1.dbc11ea950625p-4,
		0x1.64e371d5616d3p-4, 0x1.33e0023936249p-4, 0x1.204426263066ap-4, 0x1.1cd12e4629723p-4},
	{0x1.0acf4d240ccc4p+0, 0x1.da890f3b40bd3p-54, 0x1.acac23da07797p-3, 0x1.e43bab7741a98p-4,
		0x1.6eb030c631819p-4, 0x1.3f669d2eb516ep-4, 0x1.2e0006ae505aep-4, 0x1.2d58204457c82p-4},
	{0x1.0b3af1f4880bbp+0, 0x1.f450fb78d32bap-56, 0x1.b07d4778263afp-3, 0x1.ecf21db7be0efp-4,
		0x1.78db5465013e4p-4, 0x1.4b7a8376f0996p-4, 0x1.3c88f9f2ef221p-4, 0x1.3f02ad9eb9753p-4},
	{0x1.0ba78d40a9260p+0, -0x1.57b07a441e242p-54, 0x1.b46015c126262p-3, 0x1.f5e6b94713f3dp-4,
		0x1.836967d0afecfp-4, 0x1.5823fdd1707b9p-4, 0x1.4bed355269dc2p-4, 0x1.51e83065121cfp-4},
	{0x1.0c152382d7366p+0, -0x1.ee6913347c2a6p-54, 0x1.b8550d62bfb6ep-3, 0x1.ff1bde0fa3cadp-4,
		0x1.8e5f3ab550989p-4, 0x1.656be8b38ebafp-4, 0x1.5c3c13008a099p-4, 0x1.662225a1b4f77p-4},
}

var asinOff = [...][2]float64{
	{0x1.921fb54442d18p+0, 0x1.1a62633145c07p-54}, {-0x1.921fb54442d18p+0, -0x1.1a62633145c07p-54},
}

var asinS = [33][2]float64{
	{0x0p+0, 0x0p+0}, {-0x1.912bd0d569a9p-61, 0x1.91f65f10dd814p-5},
	{-0x1.e2718d26ed688p-60, 0x1.917a6bc29b42cp-4}, {0x1.13000a89a11ep-58, 0x1.2c8106e8e613ap-3},
	{-0x1.26d19b9ff8d82p-57, 0x1.8f8b83c69a60bp-3}, {-0x1.42deef11da2c4p-57, 0x1.f19f97b215f1bp-3},
	{-0x1.5d28da2c4612dp-56, 0x1.294062ed59f06p-2}, {-0x1.efdc0d58cf62p-62, 0x1.58f9a75ab1fddp-2},
	{-0x1.72cedd3d5a61p-57, 0x1.87de2a6aea963p-2}, {0x1.5b362cb974183p-57, 0x1.b5d1009e15ccp-2},
	{0x1.e0d891d3c6841p-58, 0x1.e2b5d3806f63bp-2}, {-0x1.a5a014347406cp-55, 0x1.073879922ffeep-1},
	{0x1.b25dd267f66p-55, 0x1.1c73b39ae68c8p-1}, {-0x1.efcc626f74a6fp-57, 0x1.30ff7fce17035p-1},
	{0x1.8076a2cfdc6b3p-57, 0x1.44cf325091dd6p-1}, {-0x1.75720992bfbb2p-55, 0x1.57d69348cecap-1},
	{-0x1.bdd3413b26456p-55, 0x1.6a09e667f3bcdp-1}, {-0x1.0f537acdf0ad7p-56, 0x1.7b5df226aafafp-1},
	{-0x1.2c5e12ed1336dp-55, 0x1.8bc806b151741p-1}, {-0x1.30ee286712474p-55, 0x1.9b3e047f38741p-1},
	{0x1.9f630e8b6dac8p-60, 0x1.a9b66290ea1a3p-1}, {-0x1.bc69f324e6d61p-55, 0x1.b728345196e3ep-1},
	{-0x1.6e0b1757c8d07p-56, 0x1.c38b2f180bdb1p-1}, {-0x1.e7b6bb5ab58aep-58, 0x1.ced7af43cc773p-1},
	{0x1.457e610231ac2p-56, 0x1.d906bcf328d46p-1}, {-0x1.014c76c126527p-55, 0x1.e212104f686e5p-1},
	{0x1.760b1e2e3f81ep-55, 0x1.e9f4156c62ddap-1}, {0x1.52c7adc6b4989p-56, 0x1.f0a7efb9230d7p-1},
	{0x1.562172a361fd3p-56, 0x1.f6297cff75cbp-1}, {-0x1.7a0a8ca13571fp-55, 0x1.fa7557f08a517p-1},
	{-0x1.87df6378811c7p-55, 0x1.fd88da3d12526p-1}, {-0x1.c57bc2e24aa15p-57, 0x1.ff621e3796d7ep-1},
	{0x0p+0, 0x1p+0},
}

var asinC = [...][2]float64{
	{0x1p+0, -0x1.fc2c76456515bp-108}, {0x1.5555555555555p-3, 0x1.5555555623513p-57},
	{0x1.3333333333333p-4, 0x1.9997e3427441bp-59}, {0x1.6db6db6db6db7p-5, -0x1.cb95ff08658e6p-62},
	{0x1.f1c71c71c6d5bp-6, 0x1.b125bccdcc89ep-60}}

var asinDB = [...][3]float64{
	{0x1.7137449123ef6p-26, 0x1.7137449123ef7p-26, -0x1.fffffffffffffp-80},
	{0x1.d12ed0af1a27ep-26, 0x1.d12ed0af1a27fp-26, -0x1.63676aa7969aap-134},
	{0x1.51c4b960778f5p-23, 0x1.51c4b9607790dp-23, 0x1.fffffffffffffp-77},
	{0x1.3cfc2a006a414p-22, 0x1.3cfc2a006a465p-22, -0x1.1e68bc303367cp-131},
	{0x1.cbaa95dadb559p-22, 0x1.cbaa95dadb65p-22, 0x1.742593ef4c3d3p-128},
	{0x1.acd69f89ad8f1p-20, 0x1.acd69f89ae57ap-20, 0x1.b0ddfe3e29b3cp-126},
	{0x1.2bffffffc233bp-16, 0x1.2c00000006dddp-16, -0x1.1c26f2054c23p-122},
	{0x1.ff0f3022b2e9dp-16, 0x1.ff0f3024065e7p-16, -0x1.ffffffffffffep-70},
	{0x1.3217783d70d1dp-14, 0x1.32177841ffbefp-14, -0x1.fffffffffffffp-68},
	{0x1.c373ff4aad79bp-14, 0x1.c373ff594d65bp-14, -0x1.fffffffffffffp-68},
	{0x1.b3f28593cad2fp-9, 0x1.b3f2ba40dbc66p-9, -0x1.27ca20c684481p-116},
	{0x1.e17b3f6bb5e6ep-7, 0x1.e17faefac7797p-7, -0x1.15a1c95184d1bp-112},
	{0x1.41d60a76a82edp-6, 0x1.41db571d96126p-6, -0x1.df34bcdacf353p-114},
	{0x1.921c0a0486537p-6, 0x1.9226605233224p-6, -0x1.38e672f1696d5p-108},
	{0x1.9c360a8dd681ap-6, 0x1.9c412d62c144p-6, 0x1.1edd261167bafp-110},
	{0x1.d6315f7ee7e01p-6, 0x1.d641e6d5e769ap-6, -0x1.af7dec023f5b9p-109},
	{0x1.ea6fdc56fc61ap-6, 0x1.ea829e3e988e5p-6, -0x1.f358e90326f9ap-109},
	{0x1.2749dc4d19c6dp-5, 0x1.275a3d78c01ecp-5, 0x1.49ab71c17bd11p-110},
	{0x1.69768dc89bbp-5, 0x1.69949b3d51fb1p-5, -0x1.14b91318650e8p-113},
	{0x1.a4816b2066707p-5, 0x1.a4b0bfb0454d5p-5, 0x1.fffffffffffffp-59},
	{0x1.d77b117f230d6p-5, 0x1.d7bdcd778049fp-5, 0x1.aafc31a59e093p-114},
	{0x1.fc7a07b2549aap-5, 0x1.fccdc252cad1fp-5, -0x1.dc02fdac5159ap-109},
	{0x1.2df0542154f1bp-4, 0x1.2e36813a9874p-4, -0x1.63abaa885a4dap-108},
	{0x1.51cf5db1b1956p-4, 0x1.5231b416ba885p-4, -0x1.e1e3f45d0bec3p-108},
	{0x1.d0ef799001ba9p-3, 0x1.d5064e6fe82c5p-3, -0x1.84b9ecb11e241p-109},
	{0x1.4a8e1a96e38e3p-2, 0x1.50954b7bbf87bp-2, -0x1.b9f88cfb1ef42p-108},
	{0x1.ceee68154d1c9p-2, 0x1.e05b0e0a809bcp-2, 0x1.117d6aecd2594p-107},
	{0x1.da4e0e6c717a5p-2, 0x1.ed25c5eb8c916p-2, -0x1.89e120e5744a5p-109},
	{0x1.e9950730c4696p-2, 0x1.fe767739d0f6dp-2, 0x1.16376667a73f3p-120},
}
