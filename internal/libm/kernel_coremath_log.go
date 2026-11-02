package libm

import (
	"math"

	"github.com/samyfodil/musql/internal/fir"
)

// cmLogC0 and cmLogC1 are cmLogB's fields as the words a kernel loads.
var cmLogC0, cmLogC1 = func() (c0, c1 []uint64) {
	for _, b := range cmLogB {
		c0 = append(c0, uint64(b.c0))
		c1 = append(c1, uint64(int64(b.c1)))
	}
	return c0, c1
}()

// cmLogJ is the front of CORE-MATH's log that asinh, acosh and atanh share:
// for the mantissa bits t, the rows i1 and i2 of the tables R1/L1 and R2/L2.
func (k K) cmLogJ(t fir.Val) (i1, i2 fir.Val) {
	i := k.Shr(t, 52-5)
	d := k.AndK(t, ^uint64(0)>>17)
	j := k.Add(k.Add(t, k.Shl(k.LoadI(cmLogC0, i), 33)), k.Mul(k.LoadI(cmLogC1, i), k.Sar(d, 16)))
	j = k.Shr(j, 52-10)
	return k.Shr(j, 5), k.AndK(j, 0x1f)
}

// cmPair is row i of a table of pairs: its element 0 and 1.
func (k K) cmPair(tab []uint64, i fir.Val) (lo, hi fir.Val) {
	at := k.Shl(i, 1)
	return k.LoadF(tab, at), k.LoadF(tab, k.AddK(at, 1))
}

// cmLogF is the polynomial asinh and acosh evaluate at dx.
func (k K) cmLogF(dx fir.Val) fir.Val {
	dx2 := k.FMul(dx, dx)
	c := func(v float64) fir.Val { return k.K(v) }
	return k.FMul(dx2, k.FAdd(k.FAdd(c(-0x1p-1), k.FMul(dx, c(0x1.555555555553p-2))),
		k.FMul(dx2, k.FAdd(k.FAdd(c(-0x1.fffffffffffap-3), k.FMul(dx, c(0x1.99999e33a6366p-3))),
			k.FMul(dx2, c(-0x1.555559ef9525fp-3))))))
}

// cmRound is the end CORE-MATH's fast paths share: lh + ll when an error of
// eps either way rounds alike, and slow (an Exit) otherwise.
func (k K) cmRound(lh, ll, eps fir.Val) fir.Val {
	lb := k.FAdd(lh, k.FSub(ll, eps))
	ub := k.FAdd(lh, k.FAdd(ll, eps))
	res := k.result()
	k.If(fir.FEq, lb, ub, func() { k.Set(res, lb) }, k.Exit)
	return res
}

// copysign1 is 1.0 with x's sign.
func (k K) copysign1(x fir.Val) fir.Val { return k.Or(k.AndK(x, 1<<63), k.u(0x3ff0000000000000)) }

// Asinh is Asinh. Infinities, NaNs and the accurate paths are Go's.
func (k K) Asinh(x fir.Val) fir.Val {
	res := k.result()
	ax := k.FAbs(x)
	c := func(v float64) fir.Val { return k.K(v) }
	k.If(fir.ULt, ax, k.u(0x3fbb000000000000), func() {
		k.If(fir.ULt, ax, k.u(0x3e57137449123ef7), func() {
			k.If(fir.IEq, ax, k.u(0), func() {
				k.Set(res, x)
			}, func() {
				k.Set(res, k.FMA(c(-0x1p-60), x, x))
			})
		}, func() {
			x2h := k.FMul(x, x)
			x3h := k.FMul(x2h, x)
			sl := k.Var(fir.F)
			k.If(fir.ULt, ax, k.u(0x3e5a000000000000), func() {
				k.Set(sl, k.FMul(x3h, c(-0x1.5555555555555p-3)))
			}, func() {
				k.If(fir.ULt, ax, k.u(0x3f30000000000000), func() {
					k.Set(sl, k.FMul(x3h, k.FAdd(c(-0x1.5555555555555p-3), k.FMul(x2h, c(0x1.3333327c57c6p-4)))))
				}, func() {
					k.If(fir.ULt, ax, k.u(0x3f93000000000000), func() {
						p := k.FAdd(c(-0x1.6db6d9a665159p-5), k.FMul(x2h, c(0x1.f186866d775fp-6)))
						p = k.FAdd(c(0x1.333333332f2ffp-4), k.FMul(x2h, p))
						k.Set(sl, k.FMul(x3h, k.FAdd(c(-0x1.5555555555555p-3), k.FMul(x2h, p))))
					}, func() {
						c1 := k.FAdd(c(0x1.333333333331p-4), k.FMul(x2h, c(-0x1.6db6db6da466cp-5)))
						c3 := k.FAdd(c(0x1.f1c71c2ea7be4p-6), k.FMul(x2h, c(-0x1.6e8b651b09d72p-6)))
						c5 := k.FAdd(c(0x1.1c309fc0e69c2p-6), k.FMul(x2h, c(-0x1.bab7833c1ep-7)))
						x4 := k.FMul(x2h, x2h)
						p := k.FAdd(c1, k.FMul(x4, k.FAdd(c3, k.FMul(x4, c5))))
						k.Set(sl, k.FMul(x3h, k.FAdd(c(-0x1.5555555555555p-3), k.FMul(x2h, p))))
					})
				})
			})
			eps := k.FMul(c(0x1.79p-53), x3h)
			k.Set(res, k.cmRound(x, sl, eps))
		})
	}, func() {
		ah, al, off := k.Var(fir.F), k.Var(fir.F), k.Var(fir.I)
		k.Set(off, k.u(0x3ff))
		k.If(fir.ULt, ax, k.u(0x4190000000000000), func() {
			x2h := k.FMul(x, x)
			x2l := k.FMA(x, x, k.FNeg(x2h))
			th, tl := k.Var(fir.F), k.Var(fir.F)
			k.If(fir.ULt, ax, k.u(0x3ff0000000000000), func() {
				h, l := k.fastTwoSum(c(1), x2h)
				k.Set(th, h)
				k.Set(tl, l)
			}, func() {
				h, l := k.fastTwoSum(x2h, c(1))
				k.Set(th, h)
				k.Set(tl, l)
			})
			tl2 := k.FAdd(tl, x2l)
			h := k.FSqrt(th)
			rs := k.FDiv(c(0.5), th)
			l := k.FMul(k.FSub(tl2, k.FMA(h, h, k.FNeg(th))), k.FMul(rs, h))
			h2, t2 := k.fastTwoSum(h, ax)
			k.Set(ah, h2)
			k.Set(al, k.FAdd(l, t2))
		}, func() {
			k.If(fir.ULt, ax, k.u(0x4330000000000000), func() {
				k.Set(ah, k.FMul(c(2), ax))
				k.Set(al, k.FDiv(c(0.5), ax))
			}, func() {
				k.ExitIf(fir.UGe, ax, k.u(0x7ff0000000000000))
				k.Set(off, k.u(0x3fe))
				k.Set(ah, ax)
				k.Set(al, c(0))
			})
		})
		t := ah
		ed := k.IToF(k.Sub(k.Shr(t, 52), off))
		tm := k.AndK(t, ^uint64(0)>>12)
		i1, i2 := k.cmLogJ(tm)
		r := k.FMul(k.LoadF(words(cmLogR1[:]), i1), k.LoadF(words(cmLogR2[:]), i2))
		dx := k.FMA(r, k.Or(tm, k.u(0x3ff<<52)), c(-1))
		f := k.cmLogF(dx)
		l1lo, l1hi := k.cmPair(words(cmLogL1[:]), i1)
		l2lo, l2hi := k.cmPair(words(cmLogL2[:]), i2)
		const l2h, l2l = 0x1.62e42fefa38p-1, 0x1.ef35793c7673p-45
		lh := k.FAdd(k.FMul(c(l2h), ed), k.FAdd(l1hi, l2hi))
		ll := k.FAdd(k.FAdd(k.FAdd(k.FAdd(k.FMul(c(l2l), ed), l1lo), l2lo), k.FDiv(al, ah)), f)
		ll = k.FAdd(ll, dx)
		s := k.copysign1(x)
		k.Set(res, k.cmRound(k.FMul(lh, s), k.FMul(ll, s), c(1.63e-19)))
	})
	return res
}

// Acosh is Acosh. A NaN, an infinity, a negative x and the accurate paths
// are Go's.
func (k K) Acosh(x fir.Val) fir.Val {
	res := k.result()
	c := func(v float64) fir.Val { return k.K(v) }
	k.ExitIf(fir.UGe, x, k.u(0x7ff0000000000000))
	k.If(fir.ULe, x, k.u(0x3ff0000000000000), func() {
		k.If(fir.IEq, x, k.u(0x3ff0000000000000), func() {
			k.Set(res, c(0))
		}, func() {
			k.Set(res, k.K(math.NaN()))
		})
	}, func() {
		k.If(fir.ULt, x, k.u(0x3ff1e83e425aee63), func() {
			z := k.FSub(x, c(1))
			iz, zt := k.FDiv(c(-0.25), z), k.FMul(c(2), z)
			sh := k.FSqrt(zt)
			sl := k.FMul(k.FMA(sh, sh, k.FNeg(zt)), k.FMul(sh, iz))
			cl := func(i int) fir.Val { return k.K(acoshNearCL[i]) }
			z2 := k.FMul(z, z)
			z4 := k.FMul(z2, z2)
			a := k.FAdd(k.FAdd(cl(1), k.FMul(z, cl(2))), k.FMul(z2, k.FAdd(cl(3), k.FMul(z, cl(4)))))
			b := k.FAdd(k.FAdd(cl(5), k.FMul(z, cl(6))), k.FMul(z2, k.FAdd(cl(7), k.FMul(z, cl(8)))))
			ds := k.FMA(k.FMul(sh, z), k.FAdd(cl(0), k.FMul(z, k.FAdd(a, k.FMul(z4, b)))), sl)
			eps := k.FSub(k.FMul(ds, c(0x1.00p-50)), k.FMul(c(0x1p-104), sh))
			k.Set(res, k.cmRound(sh, ds, eps))
		}, func() {
			g, eps := k.Var(fir.F), k.Var(fir.F)
			t, off := k.Var(fir.I), k.Var(fir.I)
			k.Set(t, x)
			k.Set(off, k.u(0x3fe))
			k.Set(g, c(0))
			k.Set(eps, c(0x1.b2p-63))
			k.If(fir.ULt, x, k.u(0x405bf00000000000), func() {
				k.Set(off, k.u(0x3ff))
				x2h := k.FMul(x, x)
				wh := k.FSub(x2h, c(1))
				wl := k.FMA(x, x, k.FNeg(x2h))
				sh := k.FSqrt(wh)
				ish := k.FDiv(c(0.5), wh)
				sl := k.FMul(k.FSub(wl, k.FMA(sh, sh, k.FNeg(wh))), k.FMul(sh, ish))
				th, tl := k.fastTwoSum(x, sh)
				tl = k.FAdd(tl, sl)
				k.Set(t, th)
				k.Set(g, k.FDiv(tl, th))
				k.Set(eps, c(0x1.81p-63))
			}, func() {
				poly := func(cs ...float64) fir.Val {
					z := k.FDiv(c(1), k.FMul(x, x))
					p := c(cs[len(cs)-1])
					for i := len(cs) - 2; i >= 1; i-- {
						p = k.FAdd(c(cs[i]), k.FMul(z, p))
					}
					return k.FAdd(c(cs[0]), k.FMul(z, p))
				}
				k.If(fir.ULt, x, k.u(0x4087100000000000), func() {
					k.Set(g, poly(0x1.5c4b6148816e2p-66, -0x1.000000000005cp-2, -0x1.7fffffebf3e6cp-4, -0x1.aab6691f2bae7p-5))
					k.Set(eps, c(0x1.c3p-63))
				}, func() {
					k.If(fir.ULt, x, k.u(0x40e0100000000000), func() {
						k.Set(g, poly(-0x1.7f77c8429c6c6p-67, -0x1.ffffffffff214p-3, -0x1.8000268641bfep-4))
						k.Set(eps, c(0x1.9ap-63))
					}, func() {
						k.If(fir.ULt, x, k.u(0x41ea000000000000), func() {
							k.Set(g, poly(0x1.7a0ed2effdd1p-67, -0x1.000000017d048p-2))
							k.Set(eps, c(0x1.99p-63))
						}, nil)
					})
				})
			})
			ed := k.IToF(k.Sub(k.Shr(t, 52), off))
			tm := k.AndK(t, ^uint64(0)>>12)
			i1, i2 := k.cmLogJ(tm)
			r := k.FMul(k.LoadF(words(cmLogR1[:]), i1), k.LoadF(words(cmLogR2[:]), i2))
			dx := k.FMA(r, k.Or(tm, k.u(0x3ff<<52)), c(-1))
			f := k.cmLogF(dx)
			l1lo, l1hi := k.cmPair(words(cmLogL1[:]), i1)
			l2lo, l2hi := k.cmPair(words(cmLogL2[:]), i2)
			const l2h, l2l = 0x1.62e42fefa38p-1, 0x1.ef35793c7673p-45
			lh := k.FAdd(k.FAdd(l1hi, l2hi), k.FMul(c(l2h), ed))
			t1 := k.FAdd(k.FMul(c(l2l), ed), k.FAdd(l1lo, l2lo))
			ll := k.FAdd(dx, k.FAdd(g, k.FAdd(f, t1)))
			k.Set(res, k.cmRound(lh, ll, eps))
		})
	})
	return res
}

// Atanh is Atanh. An |x| of 1 or more, a NaN and the accurate paths are Go's.
func (k K) Atanh(x fir.Val) fir.Val {
	res := k.result()
	c := func(v float64) fir.Val { return k.K(v) }
	ax := k.FAbs(x)
	k.ExitIf(fir.UGe, ax, k.u(0x3ff0000000000000))
	k.If(fir.ULt, ax, k.u(0x3fd0000000000000), func() {
		k.If(fir.ULt, ax, k.u(0x3e4d12ed0af1a27f), func() {
			k.Set(res, k.FMA(x, c(0x1p-55), x))
		}, func() {
			cc := func(i int) fir.Val { return k.K(atanhSmallC[i]) }
			x2 := k.FMul(x, x)
			dx2 := k.FMA(x, x, k.FNeg(x2))
			x4, x3 := k.FMul(x2, x2), k.FMul(x2, x)
			x8 := k.FMul(x4, x4)
			dx3 := k.FAdd(k.FMA(x2, x, k.FNeg(x3)), k.FMul(dx2, x))
			p := k.FAdd(k.FAdd(cc(0), k.FMul(x2, cc(1))), k.FMul(x4, k.FAdd(cc(2), k.FMul(x2, cc(3)))))
			q := k.FAdd(k.FAdd(k.FAdd(cc(4), k.FMul(x2, cc(5))), k.FMul(x4, k.FAdd(cc(6), k.FMul(x2, cc(7))))), k.FMul(x8, cc(8)))
			p = k.FAdd(p, k.FMul(x8, q))
			t := k.FMA(x2, p, c(0x1.5555555555555p-56))
			ph, pl := k.fastTwoSum(c(0x1.5555555555555p-2), t)
			ph, pl = k.atanhMulDD(ph, pl, x3, dx3)
			ph, tl := k.fastTwoSum(x, ph)
			pl = k.FAdd(pl, tl)
			eps := k.FMul(x, k.FAdd(k.FMul(x4, c(0x1.dp-53)), c(0x1p-103)))
			k.Set(res, k.cmRound(ph, pl, eps))
		})
	}, func() {
		ph, pl := k.fastTwoSum(c(1), ax)
		qh, ql := k.fastTwoSub(c(1), ax)
		iqh := k.FDiv(c(1), qh)
		th := k.FMul(ph, iqh)
		inner := k.FAdd(pl, k.FMul(ph, k.FSub(k.FMA(k.FNeg(qh), iqh, c(1)), k.FMul(ql, iqh))))
		tl := k.FAdd(k.FMA(ph, iqh, k.FNeg(th)), k.FMul(inner, iqh))
		ed := k.IToF(k.Sub(k.Shr(th, 52), k.u(0x3ff)))
		tm := k.AndK(th, ^uint64(0)>>12)
		i1, i2 := k.cmLogJ(tm)
		tf := k.Or(tm, k.u(0x3ff<<52))
		r := k.FMul(k.FMul(c(0.5), k.LoadF(words(cmLogR1[:]), i1)), k.LoadF(words(cmLogR2[:]), i2))
		dx := k.FMA(r, tf, c(-0.5))
		dx2 := k.FMul(dx, dx)
		rx := k.FMul(r, tf)
		dxl := k.FMA(r, tf, k.FNeg(rx))
		cc := func(i int) fir.Val { return k.K(atanhC[i]) }
		f := k.FMul(dx2, k.FAdd(k.FAdd(cc(0), k.FMul(dx, cc(1))),
			k.FMul(dx2, k.FAdd(k.FAdd(cc(2), k.FMul(dx, cc(3))), k.FMul(dx2, cc(4))))))
		l1lo, l1hi := k.cmPair(words(atanhL1[:]), i1)
		l2lo, l2hi := k.cmPair(words(atanhL2[:]), i2)
		const l2h, l2l = 0x1.62e42fefa3ap-2, -0x1.0ca86c3898dp-50
		lh := k.FAdd(k.FAdd(l1hi, l2hi), k.FMul(c(l2h), ed))
		lh, ll := k.fastTwoSum(lh, k.FSub(rx, c(0.5)))
		add := k.FAdd(k.FAdd(k.FAdd(k.FMul(c(l2l), ed), k.FAdd(l1lo, l2lo)), dxl), k.FDiv(k.FMul(c(0.5), tl), th))
		ll = k.FAdd(k.FAdd(ll, add), f)
		s := k.copysign1(x)
		eps := k.FAdd(c(38e-24), k.FMul(dx2, c(0x1p-49)))
		k.Set(res, k.cmRound(k.FMul(lh, s), k.FMul(ll, s), eps))
	})
	return res
}

// fastTwoSub is fastTwoSub.
func (k K) fastTwoSub(x, y fir.Val) (s, e fir.Val) {
	s = k.FSub(x, y)
	return s, k.FSub(k.FSub(x, s), y)
}

// atanhMulDD is atanhMulDD.
func (k K) atanhMulDD(xh, xl, ch, cl fir.Val) (h, l fir.Val) {
	ahlh, alhh, ahhh := k.FMul(ch, xl), k.FMul(cl, xh), k.FMul(ch, xh)
	ahhl := k.FMA(ch, xh, k.FNeg(ahhh))
	return ahhh, k.FAdd(ahhl, k.FAdd(alhh, ahlh))
}
