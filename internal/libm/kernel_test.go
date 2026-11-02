package libm

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/samyfodil/musql/internal/fir"
)

// kernelCase is a function and its kernel, with where to draw inputs from:
// typical ones, which the kernel should almost always answer itself, and
// any bits at all.
type kernelCase struct {
	name   string
	fn     func(float64) float64
	kernel func(K, fir.Val) fir.Val
	draw   func(*rand.Rand) float64
}

func uniform(lo, hi float64) func(*rand.Rand) float64 {
	return func(r *rand.Rand) float64 { return lo + (hi-lo)*r.Float64() }
}

// logUniform draws a positive double whose exponent is uniform in [lo, hi).
func logUniform(lo, hi int) func(*rand.Rand) float64 {
	return func(r *rand.Rand) float64 {
		return math.Ldexp(1+r.Float64(), lo+r.IntN(hi-lo))
	}
}

// anyBits draws from every double, special values included.
func anyBits(r *rand.Rand) float64 { return math.Float64frombits(r.Uint64()) }

func mixed(draws ...func(*rand.Rand) float64) func(*rand.Rand) float64 {
	return func(r *rand.Rand) float64 { return draws[r.IntN(len(draws))](r) }
}

// signed gives draw's values either sign.
func signed(draw func(*rand.Rand) float64) func(*rand.Rand) float64 {
	return func(r *rand.Rand) float64 { return math.Copysign(draw(r), float64(r.IntN(2)*2-1)) }
}

var kernelCases = []kernelCase{
	{"exp", Exp, K.Exp, mixed(uniform(-760, 720), uniform(-1, 1), uniform(-1e-15, 1e-15))},
	{"log", Log, K.Log, mixed(logUniform(-1022, 1024), uniform(0.9, 1.1))},
	{"log2", Log2, K.Log2, mixed(logUniform(-1022, 1024), uniform(0.9, 1.1))},
	{"log10", Log10, K.Log10, mixed(logUniform(-1022, 1024), uniform(0.9, 1.1))},
	{"sin", Sin, K.Sin, mixed(uniform(-10, 10), uniform(-1e6, 1e6), signed(logUniform(-7, 31)))},
	{"cos", Cos, K.Cos, mixed(uniform(-10, 10), uniform(-1e6, 1e6), signed(logUniform(-40, 31)))},
}

// kernelCase2 is kernelCase for a function of two arguments.
type kernelCase2 struct {
	name   string
	fn     func(float64, float64) float64
	kernel func(K, fir.Val, fir.Val) fir.Val
	draw   func(*rand.Rand) (float64, float64)
}

var kernelCases2 = []kernelCase2{
	{"pow", Pow, K.Pow, func(r *rand.Rand) (float64, float64) {
		switch r.IntN(3) {
		case 0:
			return logUniform(-20, 20)(r), uniform(-50, 50)(r)
		case 1:
			return uniform(0, 1000)(r), uniform(-3, 3)(r)
		}
		return logUniform(-1022, 1024)(r), uniform(-1, 1)(r)
	}},
}

func buildKernel2(t testing.TB, c kernelCase2) *fir.Func {
	t.Helper()
	k := NewKernel(c.name, 2)
	k.Return(c.kernel(k, k.Arg(0), k.Arg(1)))
	f, err := k.Func()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func buildKernel(t testing.TB, c kernelCase) *fir.Func {
	t.Helper()
	k := NewKernel(c.name, 1)
	k.Return(c.kernel(k, k.Arg(0)))
	f, err := k.Func()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestKernels holds each kernel to its function: the same bits wherever it
// answers, glibc's recorded answers included, in both builds.
func TestKernels(t *testing.T) {
	n := 200000
	if testing.Short() {
		n = 20000
	}
	for _, on := range []bool{false, true} {
		restore := SetFusedForTest(on)
		for _, c := range kernelCases {
			f := buildKernel(t, c)
			check := func(x float64) bool {
				got, exited := fir.Eval(f, x)
				if exited {
					return true
				}
				if want := c.fn(x); math.Float64bits(got) != math.Float64bits(want) {
					t.Errorf("fused=%v %s(%x): kernel %x, Go %x", on, c.name, math.Float64bits(x),
						math.Float64bits(got), math.Float64bits(want))
					return false
				}
				return false
			}
			for _, v := range glibcVectors {
				if v.fn == c.name {
					check(math.Float64frombits(v.x))
				}
			}
			r := rand.New(rand.NewPCG(1, uint64(len(c.name))))
			exits := 0
			for range n {
				if check(c.draw(r)) {
					exits++
				}
				check(anyBits(r))
			}
			// The kernel is the fast path: Go answers only what is rare.
			if exits > n/50 {
				t.Errorf("fused=%v %s: %d of %d inputs left to Go", on, c.name, exits, n)
			}
		}
		for _, c := range kernelCases2 {
			f := buildKernel2(t, c)
			check := func(x, y float64) bool {
				got, exited := fir.Eval(f, x, y)
				if exited {
					return true
				}
				if want := c.fn(x, y); math.Float64bits(got) != math.Float64bits(want) {
					t.Errorf("fused=%v %s(%x, %x): kernel %x, Go %x", on, c.name, math.Float64bits(x), math.Float64bits(y),
						math.Float64bits(got), math.Float64bits(want))
				}
				return false
			}
			for _, v := range glibcVectors {
				if v.fn == c.name {
					check(math.Float64frombits(v.x), math.Float64frombits(v.y))
				}
			}
			r := rand.New(rand.NewPCG(2, uint64(len(c.name))))
			exits := 0
			for range n {
				if check(c.draw(r)) {
					exits++
				}
				check(anyBits(r), anyBits(r))
			}
			if exits > n/50 {
				t.Errorf("fused=%v %s: %d of %d inputs left to Go", on, c.name, exits, n)
			}
		}
		restore()
	}
}
