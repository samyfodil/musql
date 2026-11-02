package libm

import (
	"math"
	"testing"
)

// TestCoreMathSlowPaths runs the accurate paths CORE-MATH takes only when its
// fast path cannot settle the rounding, on glibcVectors from their domains:
// they are correctly rounded on all of it, not only where they are reached.
func TestCoreMathSlowPaths(t *testing.T) {
	n := 0
	for _, v := range glibcVectors {
		x := math.Float64frombits(v.x)
		ax := math.Abs(x)
		var got float64
		switch {
		case v.fn == "asinh" && ax >= 0x1.7137449123ef7p-26 && ax < 0x1p-2:
			x2h := x * x
			got = asinhZero(x, x2h, math.FMA(x, x, -x2h))
		case v.fn == "atanh" && ax >= 0x1.d12ed0af1a27fp-27 && ax < 0x1p-2:
			got = atanhZero(x)
		case v.fn == "acosh" && x > 1 && x < 0x1.1e83e425aee63p+0:
			z := x - 1
			zt := 2 * z
			sh := math.Sqrt(zt)
			got = acoshOne(z, sh, math.FMA(sh, sh, -zt)*(sh*(-0.25/z)))
		default:
			continue
		}
		n++
		if math.Float64bits(got) != v.plain {
			t.Errorf("%s slow path(%x) = %x, glibc %x", v.fn, x, got, math.Float64frombits(v.plain))
		}
	}
	if n < 60 {
		t.Errorf("only %d vectors reach a slow path", n)
	}
}

// TestCoreMathDatabases checks the exception tables answer their entries,
// with the sign of x where the function is odd, and pass anything else on.
func TestCoreMathDatabases(t *testing.T) {
	for _, d := range []struct {
		name string
		odd  bool
		f    func(x, f float64) float64
		tab  [][3]float64
	}{
		{"asinh", true, asinhDatabase, asinhDB[:]},
		{"acosh", false, acoshDatabase, acoshDB[:]},
		{"atanh", true, atanhDatabase, atanhDB[:]},
	} {
		for _, e := range d.tab {
			if got := d.f(e[0], 0); got != e[1]+e[2] {
				t.Errorf("%sDatabase(%x) = %x, want %x", d.name, e[0], got, e[1]+e[2])
			}
			if d.odd {
				if got := d.f(-e[0], 0); got != -e[1]-e[2] {
					t.Errorf("%sDatabase(%x) = %x, want %x", d.name, -e[0], got, -e[1]-e[2])
				}
			}
		}
		if got := d.f(0x1.5p-3, 7); got != 7 {
			t.Errorf("%sDatabase passed %v through as %v", d.name, 7, got)
		}
	}
}
