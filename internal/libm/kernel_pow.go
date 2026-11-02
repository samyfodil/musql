package libm

import "github.com/samyfodil/musql/internal/fir"

// Log10 is Log10. A zero, negative, subnormal, infinite or NaN x is Go's.
func (k K) Log10(x fir.Val) fir.Val {
	k.ExitIf(fir.ILt, x, k.u(0x0010000000000000))
	k.ExitIf(fir.UGe, x, k.u(0x7ff0000000000000))
	e := k.Sub(k.Shr(x, 52), k.u(1023))
	i := k.Shr(e, 63)
	hx := k.Or(k.AndK(x, 0x000fffffffffffff), k.Shl(k.Sub(k.u(0x3ff), i), 52))
	y := k.IToF(k.Add(e, i))
	z := k.FAdd(k.FMul(y, k.K(log102Lo)), k.FMul(k.K(ivln10), k.Log(hx)))
	return k.FAdd(z, k.FMul(y, k.K(log102Hi)))
}

// Pow is Pow. A zero, negative, subnormal, infinite or NaN x, and a y below
// 2^-65 or of 2^63 or more in magnitude, are Go's, as are the x and y whose
// x^y is tiny or huge (expInline's).
func (k K) Pow(x, y fir.Val) fir.Val {
	k.ExitIf(fir.UGe, k.Sub(k.Shr(x, 52), k.u(0x001)), k.u(0x7ff-0x001))
	k.ExitIf(fir.UGe, k.Sub(k.AndK(k.Shr(y, 52), 0x7ff), k.u(0x3be)), k.u(0x43e-0x3be))
	hi, lo := k.powLog(x)
	var ehi, elo fir.Val
	if k.Fused() {
		ehi = k.FMul(y, hi)
		elo = k.FMA(y, lo, k.FMA(y, hi, k.FNeg(ehi)))
	} else {
		yhi := k.AndK(y, 0xfffffffff8000000)
		ylo := k.FSub(y, yhi)
		lhi := k.AndK(hi, 0xfffffffff8000000)
		llo := k.FAdd(k.FSub(hi, lhi), lo)
		ehi = k.FMul(yhi, lhi)
		elo = k.FAdd(k.FMul(ylo, lhi), k.FMul(y, llo))
	}
	return k.expInline(ehi, elo, 0)
}

// powLog is powLog.
func (k K) powLog(ix fir.Val) (hi, lo fir.Val) {
	tab := words(powTab[:])
	tmp := k.Sub(ix, k.u(powOff))
	i := k.Mul(k.AndK(k.Shr(tmp, 52-powLogTableBits), powLogN-1), k.u(3))
	kd := k.IToF(k.Sar(tmp, 52))
	iz := k.Sub(ix, k.AndK(tmp, 0xfff<<52))
	invc, logc, logctail := k.LoadF(tab, i), k.LoadF(tab, k.AddK(i, 1)), k.LoadF(tab, k.AddK(i, 2))
	var r, rhi, rlo fir.Val
	if k.Fused() {
		r = k.FMA(iz, invc, k.K(-1))
	} else {
		zhi := k.AndK(k.AddK(iz, 1<<31), 0xffffffff00000000)
		zlo := k.FSub(iz, zhi)
		rhi = k.FSub(k.FMul(zhi, invc), k.K(1))
		rlo = k.FMul(zlo, invc)
		r = k.FAdd(rhi, rlo)
	}
	t1 := k.MAdd(kd, k.K(powLn2hi), logc)
	t2 := k.FAdd(t1, r)
	lo1 := k.MAdd(kd, k.K(powLn2lo), logctail)
	lo2 := k.FAdd(k.FSub(t1, t2), r)
	ar := k.FMul(k.K(powA[0]), r)
	ar2 := k.FMul(r, ar)
	ar3 := k.FMul(r, ar2)
	var lo3, lo4 fir.Val
	if k.Fused() {
		hi = k.FAdd(t2, ar2)
		lo3 = k.FMA(r, ar, k.FNeg(ar2))
		lo4 = k.FAdd(k.FSub(t2, hi), ar2)
	} else {
		arhi := k.FMul(k.K(powA[0]), rhi)
		arhi2 := k.FMul(rhi, arhi)
		hi = k.FAdd(t2, arhi2)
		lo3 = k.FMul(rlo, k.FAdd(ar, arhi))
		lo4 = k.FAdd(k.FSub(t2, hi), arhi2)
	}
	a := func(i int) fir.Val { return k.K(powA[i]) }
	p := k.MAdd(ar2, k.MAdd(ar2, k.MAdd(r, a(6), a(5)), k.MAdd(r, a(4), a(3))), k.MAdd(r, a(2), a(1)))
	lo = k.MAdd(ar3, p, k.FAdd(k.FAdd(k.FAdd(lo1, lo2), lo3), lo4))
	y := k.FAdd(hi, lo)
	return y, k.FAdd(k.FSub(hi, y), lo)
}
