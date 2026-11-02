package libm

import "github.com/samyfodil/musql/internal/fir"

// expm1Shift is 0x200000>>k for each k, which Expm1 takes for 2 <= k < 20:
// a load, since a kernel shifts only by constants.
var expm1Shift = func() []uint64 {
	t := make([]uint64, 64)
	for k := range t {
		t[k] = 0x200000 >> k
	}
	return t
}()

// Expm1 is Expm1. An |x| of 709.78 or more, and a NaN, are Go's.
func (k K) Expm1(x fir.Val) fir.Val {
	res := k.result()
	hx := k.AndK(k.Shr(x, 32), 0x7fffffff)
	neg := k.Shr(x, 63)
	done := k.Var(fir.I)
	k.Set(done, k.u(0))
	k.If(fir.UGe, hx, k.u(0x4043687a), func() {
		k.ExitIf(fir.UGe, hx, k.u(0x40862e42))
		k.If(fir.INe, neg, k.u(0), func() {
			k.Set(res, k.K(-1))
			k.Set(done, k.u(1))
		}, nil)
	}, nil)
	k.If(fir.IEq, done, k.u(0), func() {
		xr, c := k.Var(fir.F), k.Var(fir.F)
		kk := k.Var(fir.I)
		k.Set(xr, x)
		k.Set(c, k.K(0))
		k.Set(kk, k.u(0))
		k.If(fir.UGt, hx, k.u(0x3fd62e42), func() {
			hi, lo := k.Var(fir.F), k.Var(fir.F)
			k.If(fir.ULt, hx, k.u(0x3ff0a2b2), func() {
				k.If(fir.IEq, neg, k.u(0), func() {
					k.Set(hi, k.FSub(x, k.K(ln2Hi)))
					k.Set(lo, k.K(ln2Lo))
					k.Set(kk, k.u(1))
				}, func() {
					k.Set(hi, k.FAdd(x, k.K(ln2Hi)))
					k.Set(lo, k.K(-ln2Lo))
					k.Set(kk, k.u(^uint64(0)))
				})
			}, func() {
				h := k.Or(k.AndK(x, 1<<63), k.u(0x3fe0000000000000))
				k.Set(kk, k.FToI(k.FAdd(k.FMul(k.K(invLn2), x), h)))
				t := k.IToF(kk)
				k.Set(hi, k.MAdd(k.FNeg(t), k.K(ln2Hi), x))
				k.Set(lo, k.FMul(t, k.K(ln2Lo)))
			})
			k.Set(xr, k.FSub(hi, lo))
			k.Set(c, k.FSub(k.FSub(hi, xr), lo))
		}, func() {
			k.If(fir.ULt, hx, k.u(0x3c900000), func() {
				k.Set(res, x)
				k.Set(done, k.u(1))
			}, nil)
		})
		k.If(fir.IEq, done, k.u(0), func() {
			k.Set(res, k.expm1Primary(xr, c, kk))
		}, nil)
	}, nil)
	return res
}

// expm1Primary is Expm1 from "x is now in the primary range" on.
func (k K) expm1Primary(x, c, kk fir.Val) fir.Val {
	res := k.result()
	hfx := k.FMul(k.K(0.5), x)
	hxs := k.FMul(x, hfx)
	h2 := k.FMul(hxs, hxs)
	r1 := k.MAdd(k.FMul(h2, h2), k.MAdd(hxs, k.K(q5), k.K(q4)),
		k.MAdd(h2, k.MAdd(hxs, k.K(q3), k.K(q2)), k.MAdd(hxs, k.K(q1), k.K(1))))
	t := k.MAdd(k.FNeg(hfx), r1, k.K(3))
	e := k.FMul(hxs, k.FDiv(k.FSub(r1, t), k.MAdd(k.FNeg(x), t, k.K(6))))
	k.If(fir.IEq, kk, k.u(0), func() {
		k.Set(res, k.FSub(x, k.MAdd(x, e, k.FNeg(hxs))))
	}, func() {
		e := k.FSub(k.MAdd(k.FSub(e, c), x, k.FNeg(c)), hxs)
		k.If(fir.IEq, kk, k.u(^uint64(0)), func() {
			k.Set(res, k.MAdd(k.FSub(x, e), k.K(0.5), k.K(-0.5)))
		}, func() {
			k.If(fir.IEq, kk, k.u(1), func() {
				k.If(fir.FLt, x, k.K(-0.25), func() {
					k.Set(res, k.FMul(k.K(-2), k.FSub(e, k.FAdd(x, k.K(0.5)))))
				}, func() {
					k.Set(res, k.MAdd(k.FSub(x, e), k.K(2), k.K(1)))
				})
			}, func() {
				// addExponent(y, k) is y's bits plus k<<52.
				addExp := func(y fir.Val) fir.Val { return k.Add(y, k.Shl(kk, 52)) }
				k.If(fir.ILe, kk, k.u(^uint64(1)), func() {
					y := k.FSub(k.K(1), k.FSub(e, x))
					k.Set(res, k.FSub(addExp(y), k.K(1)))
				}, func() {
					k.If(fir.IGt, kk, k.u(56), func() {
						y := k.FSub(k.K(1), k.FSub(e, x))
						k.Set(res, k.FSub(addExp(y), k.K(1)))
					}, func() {
						k.If(fir.ILt, kk, k.u(20), func() {
							t := k.Shl(k.Sub(k.u(0x3ff00000), k.LoadI(expm1Shift, k.AndK(kk, 63))), 32)
							k.Set(res, addExp(k.FSub(t, k.FSub(e, x))))
						}, func() {
							t := k.Shl(k.Sub(k.u(0x3ff), kk), 52)
							y := k.FAdd(k.FSub(x, k.FAdd(e, t)), k.K(1))
							k.Set(res, addExp(y))
						})
					})
				})
			})
		})
	})
	return res
}

// halfSign is 0.5 with x's sign.
func (k K) halfSign(x fir.Val) fir.Val { return k.Or(k.AndK(x, 1<<63), k.u(0x3fe0000000000000)) }

// Sinh is Sinh. An |x| of log(maxdouble) or more, and a NaN, are Go's.
func (k K) Sinh(x fir.Val) fir.Val {
	res := k.result()
	ix := k.AndK(k.Shr(x, 32), 0x7fffffff)
	k.ExitIf(fir.UGe, ix, k.u(0x40862e42))
	h := k.halfSign(x)
	ax := k.FAbs(x)
	k.If(fir.ULt, ix, k.u(0x40360000), func() {
		k.If(fir.ULt, ix, k.u(0x3e300000), func() {
			k.Set(res, x)
		}, func() {
			t := k.Expm1(ax)
			k.If(fir.ULt, ix, k.u(0x3ff00000), func() {
				k.Set(res, k.FMul(h, k.MAdd(t, k.K(2), k.FNeg(k.FDiv(k.FMul(t, t), k.FAdd(t, k.K(1)))))))
			}, func() {
				k.Set(res, k.FMul(h, k.FAdd(t, k.FDiv(t, k.FAdd(t, k.K(1))))))
			})
		})
	}, func() {
		k.Set(res, k.FMul(h, k.Exp(ax)))
	})
	return res
}

// Cosh is Cosh. An |x| of log(maxdouble) or more, and a NaN, are Go's.
func (k K) Cosh(x fir.Val) fir.Val {
	res := k.result()
	ix := k.AndK(k.Shr(x, 32), 0x7fffffff)
	k.ExitIf(fir.UGe, ix, k.u(0x40862e42))
	ax := k.FAbs(x)
	k.If(fir.ULt, ix, k.u(0x40360000), func() {
		k.If(fir.ULt, ix, k.u(0x3fd62e43), func() {
			k.If(fir.ULt, ix, k.u(0x3c800000), func() {
				k.Set(res, k.K(1))
			}, func() {
				t := k.Expm1(ax)
				w := k.FAdd(k.K(1), t)
				k.Set(res, k.FAdd(k.K(1), k.FDiv(k.FMul(t, t), k.FAdd(w, w))))
			})
		}, func() {
			t := k.Exp(ax)
			k.Set(res, k.FAdd(k.FMul(k.K(0.5), t), k.FDiv(k.K(0.5), t)))
		})
	}, func() {
		k.Set(res, k.FMul(k.K(0.5), k.Exp(ax)))
	})
	return res
}

// Tanh is Tanh. An infinite or NaN x is Go's.
func (k K) Tanh(x fir.Val) fir.Val {
	res := k.result()
	ix := k.AndK(k.Shr(x, 32), 0x7fffffff)
	k.ExitIf(fir.UGe, ix, k.u(0x7ff00000))
	z := k.Var(fir.F)
	k.Set(z, k.K(1))
	done := k.Var(fir.I)
	k.Set(done, k.u(0))
	k.If(fir.ULt, ix, k.u(0x40360000), func() {
		k.If(fir.IEq, k.AndK(x, 0x7fffffffffffffff), k.u(0), func() {
			k.Set(res, x)
			k.Set(done, k.u(1))
		}, func() {
			k.If(fir.ULt, ix, k.u(0x3c800000), func() {
				k.Set(res, k.FMul(x, k.FAdd(k.K(1), x)))
				k.Set(done, k.u(1))
			}, func() {
				ax := k.FAbs(x)
				k.If(fir.UGe, ix, k.u(0x3ff00000), func() {
					t := k.Expm1(k.FMul(k.K(2), ax))
					k.Set(z, k.FSub(k.K(1), k.FDiv(k.K(2), k.FAdd(t, k.K(2)))))
				}, func() {
					t := k.Expm1(k.FMul(k.K(-2), ax))
					k.Set(z, k.FDiv(k.FNeg(t), k.FAdd(t, k.K(2))))
				})
			})
		})
	}, nil)
	k.If(fir.IEq, done, k.u(0), func() {
		k.Set(res, k.Xor(z, k.AndK(x, 1<<63)))
	}, nil)
	return res
}
