//go:build amd64 && (unix || windows)

package jit

import "github.com/samyfodil/musql/internal/fir"

// firBudgets are the register sets the kernel tests give a kernel: what a
// test loop can spare, a few, and so few that most values spill.
var firBudgets = []struct {
	gprs []Reg
	xmms []XReg
}{
	{[]Reg{RAX, RDX, RCX, R8, R9, R10, R11}, []XReg{X0, X1, X2, X3, X4, X5, X6, X7, X8, X9, X10, X11, X12, X13, X14}},
	{[]Reg{RAX, RDX, R10}, []XReg{X0, X1, X2, X3, X4}},
	{[]Reg{RAX, RDX}, []XReg{X0, X1, X2, X3}},
}

// firLoop is a test harness around one kernel: for each i < N it runs the
// kernel on Col[0][i] (and Col[1][i]), storing the answer at Col[2][i], or
// a 1 at Col[3][i] when the kernel leaves the input to Go. budget picks the
// registers the kernel gets (firBudgets).
func firLoop(f *fir.Func, budget int) (*Code, error) {
	a := NewAsm()
	pool := newFirPool()
	a.MovRegMem(RBX, RDI, POffCol)
	a.MovRegMem(R12, RDI, POffCol+8)
	a.MovRegMem(R13, RDI, POffCol+16)
	a.MovRegMem(RSI, RDI, POffCol+24)
	a.MovRegMem(R15, RDI, POffN)
	a.TestRegReg(R15, R15)
	a.Jcc(CondE, "done")
	a.Label("loop")
	env := &firEnv{
		args: []firLoc{firMem(RBX, 0), firMem(R12, 0)}[:f.NArgs], out: firMem(R13, 0), exit: "kexit",
		gprs: firBudgets[budget].gprs, xmms: firBudgets[budget].xmms,
		spillBase: RDI, spillDisp: POffScratch, spillN: ProgScratch, prefix: "k", pool: pool,
	}
	if err := emitFirAmd64(a, f, env, cpuFeat); err != nil {
		return nil, err
	}
	a.Jmp("next")
	a.Label("kexit")
	a.MovMemDImm(RSI, 0, 1)
	a.Label("next")
	for _, r := range []Reg{RBX, R12, R13, RSI} {
		a.AddRegImm8(r, 8)
	}
	a.DecReg(R15)
	a.Jcc(CondNE, "loop")
	a.Label("done")
	a.Ret()
	pool.emit(a)
	code, err := a.Code()
	if err != nil {
		return nil, err
	}
	return Map(code)
}

// firFMod is whether this backend compiles fir.OpFMod.
const firFMod = true
