package libm

import "github.com/samyfodil/musql/internal/fir"

// Log is Log. A zero, negative, subnormal, infinite or NaN x is Go's.
func (k K) Log(x fir.Val) fir.Val {
	res := k.result()
	const nearLo, nearHi = 0x3fee000000000000, 0x3ff1090000000000
	k.If(fir.ULt, k.Sub(x, k.u(nearLo)), k.u(nearHi-nearLo), func() {
		k.If(fir.IEq, x, k.u(0x3ff0000000000000), func() {
			k.Set(res, k.K(0))
		}, func() {
			r := k.FSub(x, k.K(1))
			r2 := k.FMul(r, r)
			r3 := k.FMul(r, r2)
			c := func(i int) fir.Val { return k.K(logB[i]) }
			p := k.MAdd(c(10), r3, k.MAdd(c(9), r2, k.MAdd(c(8), r, c(7))))
			p = k.MAdd(p, r3, k.MAdd(c(6), r2, k.MAdd(c(5), r, c(4))))
			p = k.MAdd(p, r3, k.MAdd(c(3), r2, k.MAdd(c(2), r, c(1))))
			w := k.FMul(r, k.K(0x1p27))
			rhi := k.FSub(k.FAdd(r, w), w)
			rlo := k.FSub(r, rhi)
			rr := k.FMul(rhi, rhi)
			hi := k.MAdd(rr, c(0), r)
			lo := k.MAdd(rr, c(0), k.FSub(r, hi))
			lo = k.MAdd(k.FMul(c(0), rlo), k.FAdd(rhi, r), lo)
			k.Set(res, k.FAdd(hi, k.MAdd(p, r3, lo)))
		})
	}, func() {
		k.ExitIf(fir.UGe, k.Sub(k.Shr(x, 48), k.u(0x0010)), k.u(0x7ff0-0x0010))
		tmp := k.Sub(x, k.u(logOff))
		i := k.Shl(k.AndK(k.Shr(tmp, 52-logTableBits), logN-1), 1)
		kd := k.IToF(k.Sar(tmp, 52))
		z := k.Sub(x, k.AndK(tmp, 0xfff<<52))
		invc, logc := k.LoadF(words(logTab[:]), i), k.LoadF(words(logTab[:]), k.AddK(i, 1))
		var r fir.Val
		if k.Fused() {
			r = k.FMA(z, invc, k.K(-1))
		} else {
			chi, clo := k.LoadF(words(logTab2[:]), i), k.LoadF(words(logTab2[:]), k.AddK(i, 1))
			r = k.FMul(k.FSub(k.FSub(z, chi), clo), invc)
		}
		w := k.MAdd(kd, k.K(logLn2hi), logc)
		hi := k.FAdd(w, r)
		lo := k.MAdd(kd, k.K(logLn2lo), k.FAdd(k.FSub(w, hi), r))
		r2 := k.FMul(r, r)
		a := func(i int) fir.Val { return k.K(logA[i]) }
		y := k.MAdd(k.FMul(r, r2), k.MAdd(k.MAdd(a(4), r, a(3)), r2, k.MAdd(a(2), r, a(1))), k.MAdd(a(0), r2, lo))
		k.Set(res, k.FAdd(y, hi))
	})
	return res
}

// Log2 is Log2. A zero, negative, subnormal, infinite or NaN x is Go's.
func (k K) Log2(x fir.Val) fir.Val {
	res := k.result()
	const nearLo, nearHi = 0x3feea4af00000000, 0x3ff0b55900000000
	b := func(i int) fir.Val { return k.K(log2B[i]) }
	k.If(fir.ULt, k.Sub(x, k.u(nearLo)), k.u(nearHi-nearLo), func() {
		k.If(fir.IEq, x, k.u(0x3ff0000000000000), func() {
			k.Set(res, k.K(0))
		}, func() {
			r := k.FSub(x, k.K(1))
			var hi, lo fir.Val
			if k.Fused() {
				hi = k.FMul(r, k.K(log2InvLn2hi))
				lo = k.FMA(k.K(log2InvLn2lo), r, k.FMA(k.K(log2InvLn2hi), r, k.FNeg(hi)))
			} else {
				rhi := k.AndK(r, 0xffffffff00000000)
				rlo := k.FSub(r, rhi)
				hi = k.FMul(rhi, k.K(log2InvLn2hi))
				lo = k.FAdd(k.FMul(rlo, k.K(log2InvLn2hi)), k.FMul(r, k.K(log2InvLn2lo)))
			}
			r2 := k.FMul(r, r)
			r4 := k.FMul(r2, r2)
			p := k.MAdd(b(1), r, b(0))
			y := k.MAdd(p, r2, hi)
			lo = k.FAdd(lo, k.MAdd(p, r2, k.FSub(hi, y)))
			q := k.MAdd(k.MAdd(b(5), r, b(4)), r2, k.MAdd(b(3), r, b(2)))
			q = k.MAdd(k.MAdd(k.MAdd(b(9), r, b(8)), r2, k.MAdd(b(7), r, b(6))), r4, q)
			k.Set(res, k.FAdd(y, k.MAdd(q, r4, lo)))
		})
	}, func() {
		k.ExitIf(fir.UGe, k.Sub(k.Shr(x, 48), k.u(0x0010)), k.u(0x7ff0-0x0010))
		tmp := k.Sub(x, k.u(logOff))
		i := k.Shl(k.AndK(k.Shr(tmp, 52-log2TableBits), log2N-1), 1)
		kd := k.IToF(k.Sar(tmp, 52))
		z := k.Sub(x, k.AndK(tmp, 0xfff<<52))
		invc, logc := k.LoadF(words(log2Tab[:]), i), k.LoadF(words(log2Tab[:]), k.AddK(i, 1))
		var r, t1, t2 fir.Val
		if k.Fused() {
			r = k.FMA(z, invc, k.K(-1))
			t1 = k.FMul(r, k.K(log2InvLn2hi))
			t2 = k.FMA(k.K(log2InvLn2lo), r, k.FMA(k.K(log2InvLn2hi), r, k.FNeg(t1)))
		} else {
			chi, clo := k.LoadF(words(log2Tab2[:]), i), k.LoadF(words(log2Tab2[:]), k.AddK(i, 1))
			r = k.FMul(k.FSub(k.FSub(z, chi), clo), invc)
			rhi := k.AndK(r, 0xffffffff00000000)
			rlo := k.FSub(r, rhi)
			t1 = k.FMul(rhi, k.K(log2InvLn2hi))
			t2 = k.FAdd(k.FMul(rlo, k.K(log2InvLn2hi)), k.FMul(r, k.K(log2InvLn2lo)))
		}
		t3 := k.FAdd(kd, logc)
		hi := k.FAdd(t3, t1)
		lo := k.FAdd(k.FAdd(k.FSub(t3, hi), t1), t2)
		r2 := k.FMul(r, r)
		r4 := k.FMul(r2, r2)
		a := func(i int) fir.Val { return k.K(log2A[i]) }
		p := k.MAdd(k.MAdd(a(5), r, a(4)), r4, k.MAdd(k.MAdd(a(3), r, a(2)), r2, k.MAdd(a(1), r, a(0))))
		k.Set(res, k.FAdd(k.MAdd(r2, p, lo), hi))
	})
	return res
}
