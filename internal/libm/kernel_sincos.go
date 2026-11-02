package libm

import "github.com/samyfodil/musql/internal/fir"

// Sin is Sin. An |x| of 2^31 or more, a NaN, and the inputs whose rounding
// the fast path cannot settle (the accurate paths) are Go's.
func (k K) Sin(x fir.Val) fir.Val {
	res := k.result()
	e := k.AndK(k.Shr(x, 52), 0x7ff)
	k.If(fir.ULt, e, k.u(0x3ff-26), func() {
		k.If(fir.IEq, k.Shl(x, 1), k.u(0), func() {
			k.Set(res, x)
		}, func() {
			k.Set(res, k.FMA(x, k.K(-0x1p-54), x))
		})
	}, func() {
		k.ExitIf(fir.UGe, e, k.u(0x3ff+31))
		k.Set(res, k.sinModerate(x))
	})
	return res
}

// sinModerate is sinModerate, for 2^-26 <= |x| < 2^31.
func (k K) sinModerate(x fir.Val) fir.Val {
	ax := k.FAbs(x)
	kk := k.RoundEven(k.FMul(k.K(invPi), ax))
	rh := k.FMA(kk, k.K(sinPih), ax)
	rl := k.FMul(kk, k.K(sinPil))
	r := k.FAdd(rh, rl)
	r2 := k.FMul(r, r)
	j := k.FToI(kk)
	sbit := k.Xor(k.Shr(x, 63), k.AndK(k.Shr(j, 14), 1))
	u1, u2 := k.sinTabs(j)
	s1h, s1l := k.sinMulDD(u1(0), u1(1), u2(2), u2(3))
	s2h, s2l := k.sinMulDD(u2(0), u2(1), u1(2), u1(3))
	sh, sl := k.fastSum(s1h, s1l, s2h, s2l)
	ch := k.FSub(k.FMul(u1(2), u2(2)), k.FMul(u1(0), u2(0)))
	pr := k.FMul(r, k.FSub(k.K(1), k.FMul(k.K(0x1.55555553068fp-3), r2)))
	qr := k.FMul(r2, k.FAdd(k.K(-0.5), k.FMul(k.K(0x1.55555553bfd3p-5), r2)))
	fh, fl := sh, k.FAdd(k.FAdd(sl, k.FMul(sh, qr)), k.FMul(ch, pr))
	return k.sinRound(fh, fl, sbit, 0x1.dep-64, func() {
		// The accurate path: sinSmallAccurate or sinLargeAccurate.
		k.Exit()
	})
}

// Cos is Cos. An |x| of 2^31 or more, a NaN, and the accurate paths are Go's.
func (k K) Cos(x fir.Val) fir.Val {
	res := k.result()
	z := k.FAbs(x)
	e := k.Shr(z, 52)
	k.If(fir.ULt, e, k.u(0x3ff-27), func() {
		k.Set(res, k.FMA(x, k.FNeg(x), k.K(1)))
	}, func() {
		k.ExitIf(fir.UGe, e, k.u(0x3ff+31))
		k.Set(res, k.cosModerate(z))
	})
	return res
}

// cosModerate is cosModerate, for 2^-27 <= x < 2^31.
func (k K) cosModerate(x fir.Val) fir.Val {
	kk := k.RoundEven(k.FMul(k.K(invPi), x))
	rh := k.FMA(kk, k.K(sinPih), x)
	rl := k.FMul(kk, k.K(sinPil))
	r := k.FAdd(rh, rl)
	r2 := k.FMul(r, r)
	j := k.FToI(kk)
	sbit := k.AndK(k.Shr(j, 14), 1)
	u1, u2 := k.sinTabs(j)
	c1h, c1l := k.sinMulDD(u1(2), u1(3), u2(2), u2(3))
	c2h, c2l := k.sinMulDD(u2(0), u2(1), u1(0), u1(1))
	ch, cl := k.fastSum(c1h, c1l, k.FNeg(c2h), k.FNeg(c2l))
	sh := k.FAdd(k.FMul(u1(0), u2(2)), k.FMul(u1(2), u2(0)))
	pr := k.FMul(r, k.FSub(k.K(1), k.FMul(k.K(0x1.55555553068fp-3), r2)))
	qr := k.FMul(r2, k.FAdd(k.K(-0.5), k.FMul(k.K(0x1.55555553bfd3p-5), r2)))
	fh, fl := ch, k.FSub(k.FAdd(cl, k.FMul(ch, qr)), k.FMul(sh, pr))
	return k.sinRound(fh, fl, sbit, 0x1.dep-64, k.Exit)
}

// sinTabs is the rows of sinU1 and sinU2 that j's bits pick.
func (k K) sinTabs(j fir.Val) (u1, u2 func(int) fir.Val) {
	t1, t2 := words(sinU1[:]), words(sinU2[:])
	i1 := k.Shl(k.AndK(k.Shr(j, 7), 0x7f), 2)
	i2 := k.Shl(k.AndK(j, 0x7f), 2)
	return func(c int) fir.Val { return k.LoadF(t1, k.AddK(i1, uint64(c))) },
		func(c int) fir.Val { return k.LoadF(t2, k.AddK(i2, uint64(c))) }
}

// sinRound is the end the sin and cos fast paths share: fh + fl, negated when
// sbit is 1, when an error of eps either way rounds alike; otherwise slow.
func (k K) sinRound(fh, fl, sbit fir.Val, eps float64, slow func()) fir.Val {
	sign := k.Shl(sbit, 63)
	fh, fl = k.Xor(fh, sign), k.Xor(fl, sign)
	lb := k.FAdd(fh, k.FSub(fl, k.K(eps)))
	ub := k.FAdd(fh, k.FAdd(fl, k.K(eps)))
	res := k.result()
	k.If(fir.FEq, ub, lb, func() { k.Set(res, lb) }, slow)
	return res
}

// sinMulDD is sinMulDD.
func (k K) sinMulDD(xh, xl, ch, cl fir.Val) (h, l fir.Val) {
	ahhh := k.FMul(xh, ch)
	return ahhh, k.FAdd(k.FAdd(k.FMul(xh, cl), k.FMul(xl, ch)), k.FMA(xh, ch, k.FNeg(ahhh)))
}

// fastSum is fastSum.
func (k K) fastSum(xh, xl, yh, yl fir.Val) (h, l fir.Val) {
	s, e := k.fastTwoSum(xh, yh)
	return s, k.FAdd(k.FAdd(xl, yl), e)
}

// fastTwoSum is fastTwoSum.
func (k K) fastTwoSum(x, y fir.Val) (s, e fir.Val) {
	s = k.FAdd(x, y)
	return s, k.FSub(y, k.FSub(s, x))
}
