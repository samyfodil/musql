//go:build arm64 && (unix || windows)

package jit

import (
	"math"

	"github.com/samyfodil/musql/internal/fir"
)

// firBudgets are the register sets the kernel tests give a kernel: what a
// test loop can spare, a few, and so few that most values spill.
var firBudgets = []struct {
	gprs []Reg64
	fprs []VReg
}{
	{[]Reg64{X9, X10, X11, X12, X13, X14, X15, X16, X17, X19, X20, X21, X22, X23}, progKernelFPRs},
	{[]Reg64{X9, X10, X11}, []VReg{0, 1, 2, 3, 4}},
	{[]Reg64{X9, X10}, []VReg{0, 1, 2, 3}},
}

// firLoop is a test harness around one kernel: for each i < N it runs the
// kernel on Col[0][i] (and Col[1][i]), storing the answer at Col[2][i], or
// a 1 at Col[3][i] when the kernel leaves the input to Go. budget picks the
// registers the kernel gets (firBudgets).
func firLoop(f *fir.Func, budget int) (*Code, error) {
	a := NewArm()
	pool := newFirPool()
	a.LdrImm(X5, X0, POffCol)
	a.LdrImm(X6, X0, POffCol+8)
	a.LdrImm(X7, X0, POffCol+16)
	a.LdrImm(X8, X0, POffCol+24)
	a.LdrImm(X2, X0, POffN)
	a.Cbz(X2, "done")
	a.Label("loop")
	env := &firEnv{
		args: []firLoc{firMem(X5, 0), firMem(X6, 0)}[:f.NArgs], out: firMem(X7, 0), exit: "kexit",
		gprs: firBudgets[budget].gprs, fprs: firBudgets[budget].fprs,
		spillBase: X0, spillOff: POffScratch, spillN: ProgScratch, prefix: "k", pool: pool,
	}
	if err := emitFirArm64(a, f, env, cpuFeat); err != nil {
		return nil, err
	}
	a.B("next")
	a.Label("kexit")
	a.MovImm16(X3, 1)
	a.StrImm(X3, X8, 0)
	a.Label("next")
	for _, r := range []Reg64{X5, X6, X7, X8} {
		a.AddImm(r, r, 8)
	}
	a.SubsImm(X2, X2, 1)
	a.Cbnz(X2, "loop")
	a.Label("done")
	a.Ret()
	pool.emit(a)
	code, err := a.Code()
	if err != nil {
		return nil, err
	}
	return Map(code)
}

// firFMod is whether this backend computes every fir.OpFMod itself.
const firFMod = false

// firFModExits is whether this backend's fmod leaves (x, y) to Go: unless
// |x| < |y|, a quotient |x/y| not below 2^53, which includes an infinity, a
// zero divisor and a NaN.
func firFModExits(x, y float64) bool {
	ax, ay := math.Abs(x), math.Abs(y)
	return !(ax < ay) && !(ax/ay < 1<<53)
}
