package libm

import "math/bits"

// u128 is an unsigned 128-bit integer, for CORE-MATH's fixed-point paths.
type u128 struct{ hi, lo uint64 }

func (a u128) add(b u128) u128 {
	lo, c := bits.Add64(a.lo, b.lo, 0)
	hi, _ := bits.Add64(a.hi, b.hi, c)
	return u128{hi, lo}
}

func (a u128) sub(b u128) u128 {
	lo, c := bits.Sub64(a.lo, b.lo, 0)
	hi, _ := bits.Sub64(a.hi, b.hi, c)
	return u128{hi, lo}
}

func (a u128) shl(n uint) u128 {
	switch {
	case n >= 64:
		return u128{a.lo << (n - 64), 0}
	case n == 0:
		return a
	}
	return u128{a.hi<<n | a.lo>>(64-n), a.lo << n}
}

func (a u128) shr(n uint) u128 {
	switch {
	case n >= 64:
		return u128{0, a.hi >> (n - 64)}
	case n == 0:
		return a
	}
	return u128{a.hi >> n, a.lo>>n | a.hi<<(64-n)}
}

func (a u128) less(b u128) bool { return a.hi < b.hi || a.hi == b.hi && a.lo < b.lo }

// mul64 is the full product a*b.
func mul64(a, b uint64) u128 {
	hi, lo := bits.Mul64(a, b)
	return u128{hi, lo}
}

// mulLo is a*b modulo 2^128.
func mulLo(a, b u128) u128 {
	p := mul64(a.lo, b.lo)
	p.hi += a.hi*b.lo + a.lo*b.hi
	return p
}

// mhUU is CORE-MATH's fixed-point product: the high half of a*b, without
// the carry out of the low halves' product.
func mhUU(a, b u128) u128 {
	return mul64(a.hi, b.hi).add(u128{0, mul64(a.hi, b.lo).hi}.add(u128{0, mul64(a.lo, b.hi).hi}))
}

func (a u128) or(b u128) u128 { return u128{a.hi | b.hi, a.lo | b.lo} }
