//go:build amd64

package jit

// EmitFilterCount emits a branch-free kernel counting rows satisfying one or
// two integer comparisons over int64 column blocks. Uses SETcc for comparisons
// and an indexed loop for efficiency. Registers: RDI (*Args), RSI/RDX/RCX/RAX/RBX/R8/R10/R9.
func EmitFilterCount(condA Cond, two bool, condC Cond) ([]byte, error) {
	a := NewAsm()
	a.MovRegMem(RSI, RDI, OffA)
	a.MovRegMem(RCX, RDI, OffN)
	a.MovRegMem(RAX, RDI, OffXA)
	if two {
		a.MovRegMem(RDX, RDI, OffC)
		a.MovRegMem(RBX, RDI, OffXC)
	}
	a.XorRegReg(R8, R8)
	// R10 zeroed once; R9 holds zero for CMOV.
	a.XorRegReg(R10, R10)
	a.XorRegReg(R9, R9)
	a.TestRegReg(RCX, RCX)
	// n == 0 would otherwise run INC/JNZ around the whole index space.
	a.Jcc(CondE, "done")

	a.LeaIdx(RSI, RSI, RCX)
	if two {
		a.LeaIdx(RDX, RDX, RCX)
	}
	a.NegReg(RCX)

	a.Label("loop")
	a.CmpMemIdxReg(RSI, RCX, RAX)
	a.Setcc(condA, R10)
	if two {
		a.CmpMemIdxReg(RDX, RCX, RBX)
		// Zero r10 when the second predicate fails, leave it when it holds.
		a.Cmovcc(condC.Negate(), R10, R9)
	}
	a.AddRegReg(R8, R10)
	a.IncReg(RCX)
	a.Jcc(CondNE, "loop")

	a.Label("done")
	a.MovRegMem(R9, RDI, OffOut)
	a.MovMemReg(R9, 0, R8)
	a.Ret()
	return a.Code()
}
