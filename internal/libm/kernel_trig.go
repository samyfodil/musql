package libm

import "github.com/samyfodil/musql/internal/fir"

// atanCW is atanC as the words a kernel loads.
var atanCW = func() []uint64 {
	var w []uint64
	for _, c := range atanC {
		w = append(w, uint64(c[0]), uint64(c[1]), uint64(c[2]))
	}
	return w
}()

// row3 is element c of row i of a table of triples.
func (k K) row3(tab []uint64, i fir.Val, c uint64) fir.Val {
	return k.LoadF(tab, k.AddK(k.Mul(i, k.u(3)), c))
}

// Tan is Tan. An |x| above 2pi (its reduction is 128-bit integer
// arithmetic), a NaN and the accurate paths are Go's.
func (k K) Tan(x fir.Val) fir.Val {
	res := k.result()
	c := func(v float64) fir.Val { return k.K(v) }
	ax := k.AndK(x, 0x7fffffffffffffff)
	k.If(fir.ULe, ax, k.u(0x3e4d12ed0af1a27e), func() {
		k.If(fir.IEq, ax, k.u(0), func() {
			k.Set(res, x)
		}, func() {
			k.Set(res, k.FMA(x, c(0x1p-54), x))
		})
	}, func() {
		k.ExitIf(fir.UGt, ax, k.K(0x1.921fb54442d17p+2))
		const ch, cl = 0x1.45f306dc9c883p-3, -0x1.6b01ec5417056p-57
		h := k.FMul(c(ch), ax)
		l := k.FMA(c(cl), ax, k.FMA(c(ch), ax, k.FNeg(h)))
		fi := k.Floor(k.FMul(h, c(0x1p11)))
		h = k.FMA(fi, c(-0x1p-11), h)
		i := k.AndK(k.FToI(fi), 0x3ff)
		flip := k.Shr(i, 9)
		isTan := k.Xor(k.u(1), flip)
		neg := k.Xor(k.Shr(x, 63), flip)
		i = k.AndK(i, 0x1ff)
		hv, lv, iv, tv := k.Var(fir.F), k.Var(fir.F), k.Var(fir.I), k.Var(fir.I)
		k.Set(hv, h)
		k.Set(lv, l)
		k.Set(iv, i)
		k.Set(tv, isTan)
		k.If(fir.INe, k.AndK(i, 0x100), k.u(0), func() {
			k.Set(tv, k.Xor(isTan, k.u(1)))
			k.Set(iv, k.Sub(k.u(0x1ff), i))
			k.Set(hv, k.FSub(c(0x1p-11), h))
			k.Set(lv, k.FNeg(l))
		}, nil)
		// i == 0 && h < 2^-37: the fast path's error bound is 1, Go's.
		k.If(fir.IEq, iv, k.u(0), func() { k.ExitIf(fir.FLt, hv, c(0x1p-37)) }, nil)
		sc := words(tanSC[:])
		h, l = k.fastTwoSum(k.FSub(hv, k.row3(sc, iv, 0)), lv)
		uh := k.FMul(h, h)
		ul := k.FMA(k.FAdd(h, h), l, k.FMA(h, h, k.FNeg(uh)))
		sh, sl := k.tanEvalPSFast(h, l, uh, ul)
		chh, cll := k.tanEvalPCFast(uh, ul)
		sc1, sc2 := k.row3(sc, iv, 1), k.row3(sc, iv, 2)
		sh0, sl0 := k.sMul(sc2, sh, sl)
		ch0, cl0 := k.sMul(sc1, chh, cll)
		h1, l1 := k.fastTwoSum(ch0, sh0)
		l1 = k.FAdd(l1, k.FAdd(sl0, cl0))
		chh, cll = k.sMul(sc2, chh, cll)
		sh, sl = k.sMul(sc1, sh, sl)
		h2, l2 := k.fastTwoSum(chh, k.FNeg(sh))
		l2 = k.FAdd(l2, k.FSub(cll, sl))
		bh, bl, ah, al := k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F)
		k.If(fir.INe, tv, k.u(0), func() {
			k.Set(bh, h1)
			k.Set(bl, l1)
			k.Set(ah, h2)
			k.Set(al, l2)
		}, func() {
			k.Set(bh, h2)
			k.Set(bl, l2)
			k.Set(ah, h1)
			k.Set(al, l1)
		})
		y := k.FDiv(c(1), ah)
		qh := k.FMul(bh, y)
		eh := k.FMA(ah, k.FNeg(qh), bh)
		el := k.FMA(al, k.FNeg(qh), bl)
		ql := k.FMul(y, k.FAdd(eh, el))
		sign := k.Shl(neg, 63)
		qh, ql = k.Xor(qh, sign), k.Xor(ql, sign)
		k.Set(res, k.cmRound(qh, ql, k.FMul(qh, c(0x1.1ap-66))))
	})
	return res
}

// tanEvalPSFast is tanEvalPSFast.
func (k K) tanEvalPSFast(xh, xl, uh, ul fir.Val) (h, l fir.Val) {
	c := func(v float64) fir.Val { return k.K(v) }
	h = k.FMA(k.FMA(c(-0x1.331554ca19669p6), uh, c(0x1.466bc678d8d63p6)), uh, c(-0x1.4abbce625be53p5))
	h, l = k.sMul(h, uh, ul)
	h, t := k.fastTwoSum(c(0x1.921fb54442d18p+2), h)
	l = k.FAdd(l, k.FAdd(c(0x1.1a62645446203p-52), t))
	return k.dMul(h, l, xh, xl)
}

// tanEvalPCFast is tanEvalPCFast.
func (k K) tanEvalPCFast(uh, ul fir.Val) (h, l fir.Val) {
	c := func(v float64) fir.Val { return k.K(v) }
	h = k.FMA(k.FMA(c(-0x1.55a5c590f9e6ap6), uh, c(0x1.03c1f080ad892p6)), uh, c(-0x1.3bd3cc9be45dep4))
	h, l = k.sMul(h, uh, ul)
	h, t := k.fastTwoSum(c(1), h)
	return h, k.FAdd(l, k.FAdd(c(-0x1.923015cp-77), t))
}

// sMul is sMul.
func (k K) sMul(a, bh, bl fir.Val) (hi, lo fir.Val) {
	hi = k.FMul(a, bh)
	return hi, k.FMA(a, bl, k.FMA(a, bh, k.FNeg(hi)))
}

// dMul is dMul.
func (k K) dMul(ah, al, bh, bl fir.Val) (hi, lo fir.Val) {
	hi = k.FMul(ah, bh)
	return hi, k.FMA(ah, bl, k.FMA(al, bh, k.FMA(ah, bh, k.FNeg(hi))))
}

// Atan is Atan. A NaN and the accurate path are Go's.
func (k K) Atan(x fir.Val) fir.Val {
	res := k.result()
	c := func(v float64) fir.Val { return k.K(v) }
	at := k.AndK(x, ^uint64(0)>>1)
	k.If(fir.ULt, at, k.u(0x3f7b21c475e6362a), func() {
		k.If(fir.IEq, at, k.u(0), func() {
			k.Set(res, x)
		}, func() {
			k.If(fir.ULt, at, k.u(0x3e40000000000000), func() {
				k.Set(res, k.FMA(c(-0x1p-54), x, x))
			}, func() {
				x2 := k.FMul(x, x)
				x3, x4 := k.FMul(x, x2), k.FMul(x2, x2)
				cc := func(i int) fir.Val { return k.K(atanSmallCH[i]) }
				f := k.FMul(x3, k.FAdd(k.FAdd(cc(0), k.FMul(x2, cc(1))), k.FMul(x4, k.FAdd(cc(2), k.FMul(x2, cc(3))))))
				ub := k.FAdd(k.FAdd(f, k.FMul(f, c(0x1.6p-50))), x)
				lb := k.FAdd(k.FSub(f, k.FMul(f, c(0x1.6p-51))), x)
				k.If(fir.FEq, ub, lb, func() { k.Set(res, ub) }, func() { k.Set(res, k.atanRefine2(x, ub)) })
			})
		})
	}, func() {
		h, ah, al, done := k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.I)
		k.Set(done, k.u(0))
		k.If(fir.UGt, at, k.u(0x4062ded8e34a9035), func() {
			sign := k.AndK(x, 1<<63)
			k.Set(ah, k.Or(c(0x1.921fb54442d18p+0), sign))
			k.Set(al, k.Or(c(0x1.1a62633145c07p-54), sign))
			k.If(fir.UGe, at, k.u(0x434d02967c31cdb5), func() {
				k.ExitIf(fir.UGt, at, k.u(0x7ff<<52))
				k.Set(res, k.FAdd(ah, al))
				k.Set(done, k.u(1))
			}, nil)
			k.Set(h, k.FDiv(c(-1), x))
		}, func() {
			i := k.Sub(k.Shr(at, 51), k.u(2030))
			ut := k.Shr(k.AndK(x, ^uint64(0)>>13), 51-16)
			ut2 := k.Shr(k.Mul(ut, ut), 16)
			ci := k.Mul(i, k.u(3))
			c0 := k.LoadI(atanCW, ci)
			c1 := k.LoadI(atanCW, k.AddK(ci, 1))
			c2 := k.LoadI(atanCW, k.AddK(ci, 2))
			j := k.Shr(k.Sub(k.Add(k.Shl(c0, 16), k.Mul(ut, c1)), k.Mul(ut2, c2)), 16+9)
			sgn := k.copysign1(x)
			aa := words(atanA[:])
			ta := k.FMul(sgn, k.LoadF(aa, k.Shl(j, 1)))
			id := k.FMul(sgn, k.IToF(j))
			k.Set(al, k.FAdd(k.FMul(sgn, k.LoadF(aa, k.AddK(k.Shl(j, 1), 1))), k.FMul(c(0x1.8469898cc517p-55), id)))
			k.Set(h, k.FDiv(k.FSub(x, ta), k.FAdd(c(1), k.FMul(x, ta))))
			k.Set(ah, k.FMul(c(0x1.921fb54442dp-7), id))
		})
		k.If(fir.IEq, done, k.u(0), func() {
			h2 := k.FMul(h, h)
			h4 := k.FMul(h2, h2)
			f := k.FAdd(k.FAdd(c(1), k.FMul(h2, c(-0x1.555555555552bp-2))),
				k.FMul(h4, k.FAdd(c(0x1.9999999069c2p-3), k.FMul(h2, c(-0x1.248d2c8444ac6p-3)))))
			al2 := k.FMA(h, f, al)
			e := k.FMul(h, c(0x3.fp-52))
			ub := k.FAdd(k.FAdd(al2, e), ah)
			lb := k.FAdd(k.FSub(al2, e), ah)
			k.If(fir.FEq, ub, lb, func() { k.Set(res, ub) }, func() { k.Set(res, k.atanRefine2(x, ub)) })
		}, nil)
	})
	return res
}

// Asin is Asin. An |x| of 1 or more, a NaN and the accurate paths are Go's.
func (k K) Asin(x fir.Val) fir.Val {
	res := k.result()
	c := func(v float64) fir.Val { return k.K(v) }
	ax := k.Shl(x, 1)
	t, z, zl, jd, f0h, f0l, eps := k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F)
	done := k.Var(fir.I)
	k.Set(done, k.u(0))
	k.If(fir.UGt, ax, k.u(0x7fc0000000000000), func() {
		k.ExitIf(fir.UGe, ax, k.u(0x7fe0000000000000))
		off := k.Shl(k.Shr(x, 63), 1)
		k.Set(f0h, k.LoadF(words(asinOff[:]), off))
		k.Set(f0l, k.LoadF(words(asinOff[:]), k.AddK(off, 1)))
		k.asinFar(x, k.Xor(x, k.u(1<<63)), t, z, zl, jd)
		k.Set(eps, k.FMul(k.FAbs(k.FMul(z, t)), c(0x1.99p-52)))
	}, func() {
		k.If(fir.ULt, ax, k.u(0x7cae26e892247dec), func() {
			k.Set(res, k.FMA(c(0x1p-55), x, x))
			k.Set(done, k.u(1))
		}, func() {
			k.asinNear(x, t, jd)
			k.Set(z, x)
			k.Set(zl, c(0))
			k.Set(f0h, c(0))
			k.Set(f0l, c(0))
			k.Set(eps, k.FMul(k.FAbs(k.FMul(z, t)), c(0x1.77p-52)))
		})
	})
	k.If(fir.IEq, done, k.u(0), func() {
		fh, fl := k.asinPoly(t, z, zl, jd, f0h, f0l)
		k.Set(res, k.cmRound(fh, fl, eps))
	}, nil)
	return res
}

// Acos is Acos. An |x| of 1 or more, a NaN and the accurate paths are Go's.
func (k K) Acos(x fir.Val) fir.Val {
	res := k.result()
	c := func(v float64) fir.Val { return k.K(v) }
	ax := k.Shl(x, 1)
	t, z, zl, jd, f0h, f0l, eps := k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F), k.Var(fir.F)
	done := k.Var(fir.I)
	k.Set(done, k.u(0))
	k.If(fir.UGt, ax, k.u(0x7fc0000000000000), func() {
		k.ExitIf(fir.UGe, ax, k.u(0x7fe0000000000000))
		k.Set(f0h, c(0))
		k.Set(f0l, c(0))
		k.If(fir.INe, k.Shr(x, 63), k.u(0), func() {
			k.Set(f0h, c(0x1.921fb54442d18p+1))
			k.Set(f0l, c(0x1.1a62633145c07p-53))
		}, nil)
		k.asinFar(x, x, t, z, zl, jd)
		k.Set(eps, k.FAdd(k.FMul(k.FAbs(k.FMul(z, t)), c(0x1.8cp-52)), c(0x1p-105)))
	}, func() {
		k.Set(f0h, c(0x1.921fb54442d18p+0))
		k.Set(f0l, c(0x1.1a62633145c07p-54))
		k.If(fir.ULe, ax, k.u(0x7e00000000000000), func() {
			v := k.Var(fir.F)
			k.Set(v, c(0))
			k.If(fir.UGt, ax, k.u(0x791967670d2e8fe8), func() {
				k.Set(v, k.FMul(k.FMul(x, x), k.FMul(c(-0x1.5555555555555p-3), x)))
			}, nil)
			h, w := k.fastTwoSum(f0h, k.FNeg(x))
			l := k.FAdd(v, k.FAdd(w, f0l))
			k.Set(res, k.cmRound(h, l, c(0x1.34p-79)))
			k.Set(done, k.u(1))
		}, func() {
			k.asinNear(x, t, jd)
			k.Set(z, k.FNeg(x))
			k.Set(zl, c(0))
			k.Set(eps, k.FMul(k.FMul(z, t), c(0x1.81p-52)))
		})
	})
	k.If(fir.IEq, done, k.u(0), func() {
		fh, fl := k.asinPoly(t, z, zl, jd, f0h, f0l)
		k.Set(res, k.cmRound(fh, fl, eps))
	}, nil)
	return res
}

// asinFar is asin's and acos's reduction for 0.5 < |x| < 1: z (with s's
// sign) + zl = sqrt(2 - 2|x|) and t, jd as theirs.
func (k K) asinFar(x, s, t, z, zl, jd fir.Val) {
	c := func(v float64) fir.Val { return k.K(v) }
	t0 := k.FSub(c(2), k.FMul(c(2), k.FAbs(x)))
	k.Set(jd, k.RoundEven(k.FMul(t0, c(0x1p5))))
	k.Set(z, k.Or(k.FSqrt(t0), k.AndK(s, 1<<63)))
	k.Set(zl, k.FMul(k.FMA(z, z, k.FNeg(t0)), k.FMul(k.FDiv(c(-0.5), t0), z)))
	k.Set(t, k.FSub(k.FMul(c(0.25), t0), k.FMul(jd, c(0x1p-7))))
}

// asinNear is asin's and acos's reduction for |x| <= 0.5.
func (k K) asinNear(x, t, jd fir.Val) {
	c := func(v float64) fir.Val { return k.K(v) }
	k.Set(jd, k.RoundEven(k.FMul(k.FMul(x, x), c(0x1p7))))
	k.Set(t, k.FMA(x, x, k.FMul(c(-0x1p-7), jd)))
}

// asinPoly is asinPoly.
func (k K) asinPoly(t, z, zl, jd, f0h, f0l fir.Val) (fh, fl fir.Val) {
	row := k.Shl(k.FToI(jd), 3)
	tab := words(asinCC[:])
	cc := func(i uint64) fir.Val { return k.LoadF(tab, k.AddK(row, i)) }
	t2 := k.FMul(t, t)
	d := k.FMul(t, k.FAdd(k.FAdd(cc(2), k.FMul(t, cc(3))),
		k.FMul(t2, k.FAdd(k.FAdd(cc(4), k.FMul(t, cc(5))), k.FMul(t2, k.FAdd(cc(6), k.FMul(t, cc(7))))))))
	fh, fl = k.sinMulDD(z, zl, cc(0), k.FAdd(cc(1), d))
	return k.fastSum(f0h, f0l, fh, fl)
}

// Atan2 is Atan2. A zero, infinite or NaN operand and the accurate path are
// Go's.
func (k K) Atan2(y0, x0 fir.Val) fir.Val {
	res := k.result()
	c := func(v float64) fir.Val { return k.K(v) }
	aiy := k.AndK(y0, 1<<63-1)
	aix := k.AndK(x0, 1<<63-1)
	k.ExitIf(fir.IEq, aiy, k.u(0))
	k.ExitIf(fir.UGe, aiy, k.u(0x7ff<<52))
	k.ExitIf(fir.IEq, aix, k.u(0))
	k.ExitIf(fir.UGe, aix, k.u(0x7ff<<52))
	x, y, gt := k.Var(fir.F), k.Var(fir.F), k.Var(fir.I)
	k.If(fir.ULt, aix, aiy, func() {
		k.Set(gt, k.u(1))
		k.Set(x, aiy)
		k.Set(y, aix)
	}, func() {
		k.Set(gt, k.u(0))
		k.Set(x, aix)
		k.Set(y, aiy)
	})
	k.ExitIf(fir.UGe, k.Xor(k.Sub(aix, aiy), k.Sub(k.u(0), gt)), k.u(53<<52))
	sy, sx := k.Shr(y0, 63), k.Shr(x0, 63)
	sgn := k.Or(k.Shl(k.Xor(k.Xor(gt, sx), sy), 63), c(1))
	kw := k.Or(k.Or(k.Shl(sx, 2), k.Shl(sy, 1)), gt)
	jt := k.AndK(k.Shr(k.FAdd(k.FDiv(y, x), c(2+1/128.0)), 52-7), 127)
	f2, o := words(atan2F2[:]), words(atan2O[:])
	fh := k.FAdd(k.FMul(k.LoadF(f2, k.AddK(k.Shl(jt, 1), 1)), sgn), k.LoadF(o, k.Shl(kw, 1)))
	fl := k.FAdd(k.FMul(k.LoadF(f2, k.Shl(jt, 1)), sgn), k.LoadF(o, k.AddK(k.Shl(kw, 1), 1)))
	k.If(fir.FLt, x, c(0x1p-920), func() {
		k.Set(x, k.FMul(x, c(0x1p920)))
		k.Set(y, k.FMul(y, c(0x1p920)))
	}, nil)
	k.If(fir.FGt, x, c(0x1p1022), func() {
		k.If(fir.INe, jt, k.u(0), func() {
			k.Set(x, k.FMul(x, c(0x1p-1)))
			k.Set(y, k.FMul(y, c(0x1p-1)))
		}, nil)
	}, nil)
	t0 := k.LoadF(words(atan2T2[:]), jt)
	z := k.FDiv(k.FMA(k.FNeg(t0), x, y), k.FMA(t0, y, x))
	z2 := k.FMul(z, z)
	z = k.FMul(z, sgn)
	dz := k.FMul(k.FMul(z, z2), k.FAdd(c(-0x1.55555555554d2p-2),
		k.FMul(z2, k.FAdd(c(0x1.999999860e1cap-3), k.FMul(z2, c(-0x1.248ad469844a1p-3))))))
	eps := k.FAdd(k.FMul(k.FAbs(z), c(0x1.051p-51)), c(0x1p-90))
	rh, zz := k.fastTwoSum(fh, z)
	rl := k.FAdd(k.FAdd(fl, dz), zz)
	lb := k.FAdd(rh, k.FSub(rl, eps))
	ub := k.FAdd(rh, k.FAdd(rl, eps))
	k.If(fir.FEq, lb, ub, func() {
		k.Set(res, ub)
	}, func() {
		dh := k.FMul(y, t0)
		dl := k.FMA(y, t0, k.FNeg(dh))
		dh, e := k.fastTwoSum(x, dh)
		rdh := k.FDiv(c(1), dh)
		dl = k.FAdd(dl, e)
		nh := k.FMul(x, t0)
		nl := k.FMA(x, t0, k.FNeg(nh))
		dt := k.FSub(y, nh)
		y1 := k.FAdd(dt, nh)
		nhv, nlv := k.Var(fir.F), k.Var(fir.F)
		k.If(fir.FEq, y1, y, func() {
			h, l := k.fastTwoSum(dt, k.FNeg(nl))
			k.Set(nhv, h)
			k.Set(nlv, l)
		}, func() {
			h, l := k.fastTwoSum(dt, k.FSub(k.FSub(y, y1), nl))
			k.Set(nhv, h)
			k.Set(nlv, l)
		})
		zh := k.FMul(nhv, rdh)
		z2 := k.FMul(zh, zh)
		zl := k.FMul(rdh, k.FAdd(k.FMA(dh, k.FNeg(zh), nhv), k.FSub(nlv, k.FMul(k.FMul(nhv, rdh), dl))))
		b := func(i int) fir.Val { return k.K(atan2B2[i]) }
		poly := k.FAdd(k.FAdd(b(0), k.FMul(z2, b(1))), k.FMul(k.FMul(z2, z2), k.FAdd(b(2), k.FMul(z2, b(3)))))
		zl = k.FAdd(zl, k.FMul(k.FMul(zh, z2), poly))
		zh = k.FMul(zh, sgn)
		zl = k.FMul(zl, sgn)
		eps := k.FMul(c(0x1.4p-50), k.FAdd(k.FMul(k.FAbs(zh), z2), c(0x1p-51)))
		h, l := k.fastSum(fh, fl, zh, zl)
		k.Set(res, k.cmRound(h, l, eps))
	})
	return res
}

// atanRefine2 is atanRefine2. The rare results its last test sends to the
// exceptions list, and the ones it nudges, are Go's.
func (k K) atanRefine2(x, a fir.Val) fir.Val {
	c := func(v float64) fir.Val { return k.K(v) }
	phi := k.FAdd(k.FMul(k.FAbs(a), c(0x1.45f306dc9c883p6)), c(256.5))
	i := k.AndK(k.Shr(phi, 52-8), 0xff)
	h, hl := k.Var(fir.F), k.Var(fir.F)
	aa := words(atanA[:])
	sign := k.AndK(x, 1<<63)
	k.If(fir.IEq, i, k.u(128), func() {
		hh := k.FDiv(c(-1), x)
		k.Set(h, hh)
		k.Set(hl, k.FMul(k.FMA(hh, x, c(1)), hh))
	}, func() {
		ta := k.Or(k.LoadF(aa, k.Shl(i, 1)), sign)
		zta := k.FMul(x, ta)
		ztal := k.FMA(x, ta, k.FNeg(zta))
		zmta := k.FSub(x, ta)
		v := k.FAdd(c(1), zta)
		d := k.FSub(c(1), v)
		ev := k.FAdd(k.FSub(k.FAdd(d, zta), k.FSub(k.FAdd(d, v), c(1))), ztal)
		r := k.FDiv(c(1), v)
		rl := k.FMul(k.FSub(k.FMA(r, k.FNeg(v), c(1)), k.FMul(ev, r)), r)
		hh := k.FMul(r, zmta)
		k.Set(h, hh)
		k.Set(hl, k.FAdd(k.FMA(r, zmta, k.FNeg(hh)), k.FMul(rl, zmta)))
	})
	h2, h2l := k.acoshMulDDAcc(h, hl, h, hl)
	h4 := k.FMul(h2, h2)
	h3, h3l := k.acoshMulDDAcc(h, hl, h2, h2l)
	cl := func(j int) fir.Val { return k.K(atanRefCL[j]) }
	fl := k.FMul(h2, k.FAdd(k.FAdd(cl(0), k.FMul(h2, cl(1))), k.FMul(h4, k.FAdd(cl(2), k.FMul(h2, cl(3))))))
	f, fl := k.asinhPolyDD(h2, h2l, atanRefCH[:], fl)
	f, fl = k.acoshMulDDAcc(h3, h3l, f, fl)
	ah, al, at := k.Var(fir.F), k.Var(fir.F), k.Var(fir.F)
	k.If(fir.IEq, i, k.u(0), func() {
		k.Set(ah, h)
		k.Set(al, f)
		k.Set(at, fl)
	}, func() {
		df := k.Var(fir.F)
		k.Set(df, c(0))
		k.If(fir.ULt, i, k.u(128), func() {
			k.Set(df, k.FMul(k.copysign1(x), k.LoadF(aa, k.AddK(k.Shl(i, 1), 1))))
		}, nil)
		id := k.Or(k.IToF(i), sign)
		l, t := k.asinhAddDD(k.FMul(c(0x1.8469898cc518p-55), id), k.FMul(c(-0x1.fc8f8cbb5bf8p-104), id), df, c(0))
		l, t = k.asinhAddDD(l, t, h, hl)
		l, t = k.asinhAddDD(l, t, f, fl)
		k.Set(ah, k.FMul(c(0x1.921fb54442dp-7), id))
		k.Set(al, l)
		k.Set(at, t)
	})
	v0, v2 := k.fastTwoSum(ah, al)
	v1, _ := k.fastTwoSum(v2, at)
	k.ExitIf(fir.ULe, k.AndK(k.AddK(v1, 1), ^uint64(0)>>12), k.u(2))
	k.ExitIf(fir.UGt, k.Sub(k.AndK(k.Shr(v0, 52), 0x7ff), k.AndK(k.Shr(v1, 52), 0x7ff)), k.u(103))
	return k.FAdd(v1, v0)
}

// acoshMulDDAcc is acoshMulDDAcc.
func (k K) acoshMulDDAcc(xh, xl, ch, cl fir.Val) (h, l fir.Val) {
	ahlh, alhh, ahhh := k.FMul(ch, xl), k.FMul(cl, xh), k.FMul(ch, xh)
	ahhl := k.FAdd(k.FMA(ch, xh, k.FNeg(ahhh)), k.FAdd(alhh, ahlh))
	return k.fastTwoSum(ahhh, ahhl)
}

// asinhMulDDAcc is asinhMulDDAcc.
func (k K) asinhMulDDAcc(xh, xl, ch, cl fir.Val) (h, l fir.Val) {
	ahlh, alhh, ahhh := k.FMul(ch, xl), k.FMul(cl, xh), k.FMul(ch, xh)
	ahhl := k.FAdd(k.FMA(ch, xh, k.FNeg(ahhh)), k.FAdd(alhh, ahlh))
	h = k.FAdd(ahhh, ahhl)
	return h, k.FAdd(k.FSub(ahhh, h), ahhl)
}

// asinhPolyDD is asinhPolyDD, unrolled over its table.
func (k K) asinhPolyDD(xh, xl fir.Val, c [][2]float64, l fir.Val) (h, lo fir.Val) {
	i := len(c) - 1
	ch := k.FAdd(k.K(c[i][0]), l)
	cl := k.FAdd(k.FAdd(k.FSub(k.K(c[i][0]), ch), l), k.K(c[i][1]))
	for i--; i >= 0; i-- {
		ch, cl = k.asinhMulDDAcc(xh, xl, ch, cl)
		th := k.FAdd(ch, k.K(c[i][0]))
		tl := k.FAdd(k.FSub(k.K(c[i][0]), th), ch)
		ch = th
		cl = k.FAdd(cl, k.FAdd(tl, k.K(c[i][1])))
	}
	return ch, cl
}

// asinhAddDD is asinhAddDD.
func (k K) asinhAddDD(xh, xl, ch, cl fir.Val) (s, l fir.Val) {
	s = k.FAdd(xh, ch)
	d := k.FSub(s, xh)
	return s, k.FAdd(k.FAdd(k.FSub(ch, d), k.FAdd(xh, k.FSub(d, s))), k.FAdd(xl, cl))
}
