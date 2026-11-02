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

// TestCoreMathAccuratePaths runs each trigonometric function's accurate path
// directly on glibcVectors from its domain, where it must give the same
// correctly rounded answer as the whole function.
func TestCoreMathAccuratePaths(t *testing.T) {
	n := 0
	for _, v := range glibcVectors {
		x, y, want := math.Float64frombits(v.x), math.Float64frombits(v.y), v.plain
		if math.IsInf(x, 0) || x != x || x == 0 {
			continue
		}
		ax := math.Abs(x)
		var got float64
		switch {
		case v.fn == "sin" && ax >= 0x1p-16:
			got = sinLargeAccurate(x)
		case v.fn == "sin" && ax >= 0x1p-26:
			got = sinSmallAccurate(x)
		case v.fn == "cos" && ax >= 0x1p-27:
			got = cosLargeAccurate(ax)
		case v.fn == "tan" && ax > 0x1.d12ed0af1a27ep-27:
			got = tanAccurate(x)
		case v.fn == "asin" && ax >= 0x1.7137449123ef6p-26 && ax < 1:
			got = asinRefine(x, math.Float64frombits(want))
		case v.fn == "acos" && ax < 1:
			got = acosRefine(x, math.Float64frombits(want))
		case v.fn == "atan" && ax >= 0x1p-27:
			got = atanRefine2(x, math.Float64frombits(want))
		case v.fn == "atan2" && !math.IsInf(y, 0) && y == y && y != 0:
			got = atan2Accurate(x, y)
		default:
			continue
		}
		n++
		if math.Float64bits(got) != want {
			t.Errorf("%s accurate path(%x, %x) = %x, want %x", v.fn, x, y, got, math.Float64frombits(want))
		}
	}
	if n < 400 {
		t.Errorf("only %d vectors reach an accurate path", n)
	}
}

// TestCoreMathExceptions checks the inputs CORE-MATH answers from a list,
// where even its accurate path cannot settle the rounding.
func TestCoreMathExceptions(t *testing.T) {
	for _, c := range []struct {
		fn   func(float64) float64
		x    float64
		want float64
	}{
		{Cos, 0x1.8000000000009p-23, 0x1.fffffffffff7p-1 + 0x1.b56666666666cp-143},
		{Cos, -0x1.8000000000024p-22, 0x1.ffffffffffdcp-1 + 0x1.b56666666667ep-137},
		{Cos, 0x1.800000000009p-21, 0x1.ffffffffff7p-1 + 0x1.b5666666666c4p-131},
		{Cos, 0x1.20000000000f3p-20, 0x1.fffffffffebcp-1 + 0x1.37642666666fdp-127},
		{Tan, 0x1.dffffffffff1fp-22, 0x1.e000000000151p-22 + 0x1.fffffffffffffp-76},
		{Tan, -0x1.dfffffffffc7cp-21, -0x1.e000000000546p-21 - -0x1.658bcedb6e1d4p-147},
		{Acos, 0x1.ffffffffffdc0p-1, 0x1.8000000000024p-22 + 0x1p-76},
		{Acos, 0x1.53ea6c7255e88p-4, 0x1.7cdacb6bbe707p+0 + 0x1p-54},
		{Acos, 0x1.fd737be914578p-11, 0x1.91e006d41d8d8p+0 + 0x1.8p-53},
		{Acos, 0x1.fffffffffff70p-1, 0x1.8000000000009p-23 + 0x1p-77},
		{Acos, 0x1.390e6939cd1a6p-5, 0x1.8856a5d3296a4p+0 - 0x1p-109},
	} {
		if got := c.fn(c.x); got != c.want {
			t.Errorf("f(%x) = %x, want %x", c.x, got, c.want)
		}
	}
	for _, e := range asinDB {
		if got := Asin(e[0]); got != e[1]+e[2] {
			t.Errorf("Asin(%x) = %x, want %x", e[0], got, e[1]+e[2])
		}
		if got := asinDatabase(-e[0], 0); got != -e[1]-e[2] {
			t.Errorf("asinDatabase(%x) = %x, want %x", -e[0], got, -e[1]-e[2])
		}
	}
	if got := asinDatabase(0.3, 7); got != 7 {
		t.Errorf("asinDatabase passed 7 through as %v", got)
	}
	for _, e := range atanDB {
		if got := Atan(-e[0]); got != -e[1]-e[2] {
			t.Errorf("Atan(%x) = %x, want %x", -e[0], got, -e[1]-e[2])
		}
	}
}

// TestAtan2Special checks atan2's zeros, infinities and NaNs, as C99 Annex F
// has them.
func TestAtan2Special(t *testing.T) {
	inf, nan, negZero := math.Inf(1), math.NaN(), math.Copysign(0, -1)
	pi, pi2, pi4 := math.Pi, math.Pi/2, math.Pi/4
	for _, c := range []struct{ y, x, want float64 }{
		{0, 0, 0}, {negZero, 0, negZero}, {0, negZero, pi}, {negZero, negZero, -pi},
		{0, 1, 0}, {negZero, 1, negZero}, {0, -1, pi}, {negZero, -1, -pi},
		{1, 0, pi2}, {-1, negZero, -pi2},
		{inf, inf, pi4}, {-inf, inf, -pi4}, {inf, -inf, 3 * pi4}, {-inf, -inf, -3 * pi4},
		{1, inf, 0}, {-1, inf, negZero}, {1, -inf, pi}, {-1, -inf, -pi},
		{inf, 1, pi2}, {-inf, -1, -pi2},
		{nan, 1, nan}, {1, nan, nan},
	} {
		got := Atan2(c.y, c.x)
		if math.Float64bits(got) != math.Float64bits(c.want) && !(got != got && c.want != c.want) {
			t.Errorf("Atan2(%v, %v) = %v, want %v", c.y, c.x, got, c.want)
		}
	}
}
