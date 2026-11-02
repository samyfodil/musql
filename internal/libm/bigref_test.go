package libm

import (
	"math"
	"math/big"
	"sync"
)

// The correctly rounded value of a trigonometric function, computed with
// math/big far beyond any hard case's need: the referee where a port and
// glibc disagree.

const refPrec = 320

var (
	piOnce sync.Once
	bigPi  *big.Float // pi to 2400 bits, enough to reduce any float64
)

func newF(prec uint) *big.Float { return new(big.Float).SetPrec(prec) }

// atanInv is atan(1/n) for an integer n > 1, by its Taylor series.
func atanInv(n int64, prec uint) *big.Float {
	x := newF(prec).Quo(newF(prec).SetInt64(1), newF(prec).SetInt64(n))
	x2 := newF(prec).Mul(x, x)
	sum := newF(prec).Set(x)
	term := newF(prec).Set(x)
	eps := newF(prec).SetMantExp(newF(prec).SetInt64(1), -int(prec)-8)
	for k := int64(1); ; k++ {
		term.Mul(term, x2)
		t := newF(prec).Quo(term, newF(prec).SetInt64(2*k+1))
		if k%2 == 1 {
			sum.Sub(sum, t)
		} else {
			sum.Add(sum, t)
		}
		if t.Cmp(eps) < 0 {
			return sum
		}
	}
}

func refPi() *big.Float {
	piOnce.Do(func() {
		const prec = 2400
		// Machin: pi = 16 atan(1/5) - 4 atan(1/239).
		a := newF(prec).Mul(newF(prec).SetInt64(16), atanInv(5, prec))
		b := newF(prec).Mul(newF(prec).SetInt64(4), atanInv(239, prec))
		bigPi = a.Sub(a, b)
	})
	return bigPi
}

// reduce is x - k*pi/2 for the integer k nearest x/(pi/2), and k mod 4.
func reduce(x float64) (*big.Float, int) {
	const prec = 2400
	bx := newF(prec).SetFloat64(x)
	half := newF(prec).Quo(refPi(), newF(prec).SetInt64(2))
	q := newF(prec).Quo(bx, half)
	// Round q to the nearest integer.
	q.Add(q, newF(prec).SetFloat64(math.Copysign(0.5, x)))
	k, _ := q.Int(nil)
	r := newF(prec).Sub(bx, newF(prec).Mul(newF(prec).SetInt(k), half))
	m := new(big.Int).Mod(k, big.NewInt(4))
	return r.SetPrec(refPrec), int(m.Int64())
}

// sinCosSeries is sin(r) and cos(r) for |r| <= pi/4.
func sinCosSeries(r *big.Float) (s, c *big.Float) {
	prec := uint(refPrec)
	r2 := newF(prec).Mul(r, r)
	s, c = newF(prec).Set(r), newF(prec).SetInt64(1)
	ts, tc := newF(prec).Set(r), newF(prec).SetInt64(1)
	eps := newF(prec).SetMantExp(newF(prec).SetInt64(1), -int(prec)-8)
	for k := int64(1); ; k++ {
		ts.Mul(ts, r2)
		ts.Quo(ts, newF(prec).SetInt64((2*k)*(2*k+1)))
		tc.Mul(tc, r2)
		tc.Quo(tc, newF(prec).SetInt64((2*k-1)*(2*k)))
		if k%2 == 1 {
			s.Sub(s, ts)
			c.Sub(c, tc)
		} else {
			s.Add(s, ts)
			c.Add(c, tc)
		}
		if newF(prec).Abs(tc).Cmp(eps) < 0 && newF(prec).Abs(ts).Cmp(eps) < 0 {
			return s, c
		}
	}
}

func refSinCos(x float64) (s, c *big.Float) {
	r, k := reduce(x)
	s, c = sinCosSeries(r)
	switch k {
	case 1:
		s, c = c, newF(refPrec).Neg(s)
	case 2:
		s, c = newF(refPrec).Neg(s), newF(refPrec).Neg(c)
	case 3:
		s, c = newF(refPrec).Neg(c), s
	}
	return s, c
}

// refAtan is atan(x): the argument halved eight times by
// atan(x) = 2*atan(x/(1+sqrt(1+x^2))), then the Taylor series. For a tiny x,
// atan(x) is x less x^3/3, and x need not be a double, so it can sit on a
// rounding midpoint; the precision grows with -log2(x) to see which side.
func refAtan(x *big.Float) *big.Float {
	prec := uint(refPrec + 64)
	if e := x.MantExp(nil); e < 0 {
		prec += uint(-2 * e)
	}
	one := newF(prec).SetInt64(1)
	x = newF(prec).Set(x)
	neg := x.Sign() < 0
	x.Abs(x)
	inv := x.Cmp(one) > 0
	if inv {
		x.Quo(one, x)
	}
	const halvings = 8
	for range halvings {
		s := newF(prec).Mul(x, x)
		s.Add(s, one)
		s.Sqrt(s)
		s.Add(s, one)
		x.Quo(x, s)
	}
	x2 := newF(prec).Mul(x, x)
	sum, term := newF(prec).Set(x), newF(prec).Set(x)
	eps := newF(prec).SetMantExp(one, -int(prec)-8)
	for n := int64(1); ; n++ {
		term.Mul(term, x2)
		term.Neg(term)
		t := newF(prec).Quo(term, newF(prec).SetInt64(2*n+1))
		sum.Add(sum, t)
		if newF(prec).Abs(t).Cmp(eps) < 0 {
			break
		}
	}
	sum.SetMantExp(sum, halvings)
	if inv {
		half := newF(prec).SetMantExp(refPi(), -1)
		sum.Sub(half, sum)
	}
	if neg {
		sum.Neg(sum)
	}
	return sum
}

func refAsin(x float64) *big.Float {
	prec := uint(refPrec + 64)
	bx := newF(prec).SetFloat64(x)
	if math.Abs(x) == 1 {
		h := newF(prec).SetMantExp(refPi(), -1)
		if x < 0 {
			h.Neg(h)
		}
		return h
	}
	d := newF(prec).Mul(bx, bx)
	d.Sub(newF(prec).SetInt64(1), d)
	d.Sqrt(d)
	return refAtan(d.Quo(bx, d))
}

func round53(f *big.Float) float64 {
	v, _ := f.Float64()
	return v
}

// bigRef is the correctly rounded value of fn(x, y), and ok == false for a
// function it has no reference for or an input it does not cover.
func bigRef(fn string, x, y float64) (float64, bool) {
	if math.IsInf(x, 0) || math.IsNaN(x) || x == 0 {
		return 0, false
	}
	switch fn {
	case "sin":
		s, _ := refSinCos(x)
		return round53(s), true
	case "cos":
		_, c := refSinCos(x)
		return round53(c), true
	case "tan":
		s, c := refSinCos(x)
		return round53(newF(refPrec).Quo(s, c)), true
	case "asin":
		if math.Abs(x) > 1 {
			return 0, false
		}
		return round53(refAsin(x)), true
	case "acos":
		if math.Abs(x) > 1 {
			return 0, false
		}
		half := newF(refPrec+64).SetMantExp(refPi(), -1)
		return round53(half.Sub(half, refAsin(x))), true
	case "atan":
		return round53(refAtan(newF(refPrec + 64).SetFloat64(x))), true
	case "atan2":
		if math.IsInf(y, 0) || math.IsNaN(y) || y == 0 {
			return 0, false
		}
		// x/y to 4400 bits is within 2^-4400 of x/y whatever the exponents.
		prec := uint(4400)
		a := refAtan(newF(prec).Quo(newF(prec).SetFloat64(x), newF(prec).SetFloat64(y)))
		// atan2(x, y) with x the first argument, as the records hold them.
		if y < 0 {
			pi := newF(prec).Set(refPi())
			if x > 0 {
				a.Add(a, pi)
			} else {
				a.Sub(a, pi)
			}
		}
		return round53(a), true
	}
	return 0, false
}
