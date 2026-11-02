package libm

import "github.com/samyfodil/musql/internal/fir"

// Exp is Exp. An infinite or NaN x is Go's.
func (k K) Exp(x fir.Val) fir.Val { return k.expInline(x, k.K(0), 0) }

// expInline is expInline. An |x| below 2^-54 or of 1024 or more is Go's.
func (k K) expInline(x, xtail fir.Val, sign uint64) fir.Val {
	abstop := k.AndK(k.Shr(x, 52), 0x7ff)
	scaled := k.Var(fir.I) // expInline's abstop == 0: the scale may overflow or underflow
	k.Set(scaled, k.u(0))
	d := k.Sub(abstop, k.u(0x3c9))
	k.If(fir.UGe, d, k.u(0x408-0x3c9), func() {
		k.ExitIf(fir.UGe, d, k.u(0x80000000))
		k.ExitIf(fir.UGe, abstop, k.u(0x409))
		k.Set(scaled, k.u(1))
	}, nil)

	kd := k.MAdd(k.K(expInvLn2N), x, k.K(expShift))
	ki := kd
	kd = k.FSub(kd, k.K(expShift))
	r := k.FAdd(k.MAdd(k.K(expNegLn2loN), kd, k.MAdd(k.K(expNegLn2hiN), kd, x)), xtail)
	idx := k.Shl(k.AndK(ki, expN-1), 1)
	top := ki
	if sign != 0 {
		top = k.AddK(ki, sign)
	}
	top = k.Shl(top, 52-expTableBits)
	tail := k.LoadF(expTab[:], idx)
	sbits := k.Add(k.LoadI(expTab[:], k.AddK(idx, 1)), top)
	r2 := k.FMul(r, r)
	tmp := k.MAdd(k.FMul(r2, r2), k.MAdd(k.K(expC5), r, k.K(expC4)),
		k.MAdd(k.MAdd(k.K(expC3), r, k.K(expC2)), r2, k.FAdd(tail, r)))
	res := k.result()
	k.If(fir.INe, scaled, k.u(0), func() {
		k.Set(res, k.expSpecial(tmp, sbits, ki))
	}, func() {
		k.Set(res, k.MAdd(sbits, tmp, sbits))
	})
	return res
}

// expSpecial is expSpecial.
func (k K) expSpecial(tmp, sbits, ki fir.Val) fir.Val {
	res := k.result()
	k.If(fir.IEq, k.AndK(ki, 0x80000000), k.u(0), func() {
		scale := k.Sub(sbits, k.u(1009<<52))
		k.Set(res, k.FMul(k.MAdd(scale, tmp, scale), k.K(0x1p1009)))
	}, func() {
		scale := k.Add(sbits, k.u(1022<<52))
		st := k.FMul(scale, tmp)
		y := k.Var(fir.F)
		k.Set(y, k.FAdd(scale, st))
		k.If(fir.FLt, k.FAbs(y), k.K(1), func() {
			one := k.Var(fir.F)
			k.Set(one, k.K(1))
			k.If(fir.FLt, y, k.K(0), func() { k.Set(one, k.K(-1)) }, nil)
			lo := k.FAdd(k.FSub(scale, y), st)
			hi := k.FAdd(one, y)
			lo = k.FAdd(k.FAdd(k.FSub(one, hi), y), lo)
			k.Set(y, k.FSub(k.FAdd(hi, lo), one))
			k.If(fir.FEq, y, k.K(0), func() {
				k.Set(y, k.AndK(scale, 0x8000000000000000))
			}, nil)
		}, nil)
		k.Set(res, k.FMul(k.K(0x1p-1022), y))
	})
	return res
}
