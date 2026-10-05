//go:build arm64

package jit

// EmitFilterCountSIMD emits a NEON kernel counting rows satisfying one or two
// integer comparisons, two int64 lanes per instruction. Uses comparison masks
// subtracted from an accumulator: CMGT/CMEQ leave lanes all-ones or zero, so
// subtracting adds exactly one per hit. NEON is 128-bit (two lanes) vs AVX2's
// 256-bit, but AArch64 has NOT outright, offsetting the lane loss.
func EmitFilterCountSIMD(condA Cond, two bool, condC Cond) ([]byte, error) {
	a := NewArm()

	// V1 = xa in both lanes, V2 = xc in both, V0 = accumulator.
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	if two {
		a.LdrImm(X2, X0, OffC)
		a.LdrImm(X5, X0, OffXC)
	}
	a.MovImm16(X6, 0)
	a.Dup(1, X4)
	if two {
		a.Dup(2, X5)
	}
	a.EorV(0, 0, 0)

	a.LsrImm(X7, X3, 1)    // full two-lane blocks
	a.AndImmLow(X3, X3, 1) // scalar tail: at most one row
	a.Cbz(X7, "tail")

	a.Label("block")
	a.LdrQPost(3, X1, 16)
	emitLaneCmp(a, 4, 3, 1, condA) // V4 = a OP xa
	if two {
		a.LdrQPost(5, X2, 16)
		emitLaneCmp(a, 6, 5, 2, condC) // V6 = c OP xc
		a.AndV(7, 4, 6)                // both
		a.SubV(0, 0, 7)                // += 1 per hit
	} else {
		a.SubV(0, 0, 4) // += 1 per hit
	}
	a.SubsImm(X7, X7, 1)
	a.Bcond(CondNE, "block")

	// Horizontal sum of the two lanes.
	a.UmovD(X8, 0, 0)
	a.AddReg(X6, X6, X8)
	a.UmovD(X8, 0, 1)
	a.AddReg(X6, X6, X8)

	a.Label("tail")
	a.Cbz(X3, "done")
	a.Label("tailloop")
	a.LdrPost(X8, X1, 8)
	if two {
		a.LdrPost(X9, X2, 8)
		a.Cmp(X8, X4)
		a.Ccmp(X9, X5, nzcvFalseFor(condC), condA)
		a.Cinc(X6, X6, condC)
	} else {
		a.Cmp(X8, X4)
		a.Cinc(X6, X6, condA)
	}
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "tailloop")

	a.Label("done")
	a.LdrImm(X8, X0, OffOut)
	a.StrImm(X6, X8, 0)
	a.Ret()
	return a.Code()
}

// emitLaneCmp leaves dst all-ones where "src OP bound" holds, composing other
// operators at emit time from CMGT and CMEQ. AArch64 uses NOT for negation,
// unlike x86's all-ones-then-ANDN.
func emitLaneCmp(a *Arm, dst, src, bound VReg, cond Cond) {
	switch cond {
	case CondG: // src > bound
		a.Cmgt(dst, src, bound)
	case CondLE: // NOT (src > bound)
		a.Cmgt(dst, src, bound)
		a.NotV(dst, dst)
	case CondL: // bound > src
		a.Cmgt(dst, bound, src)
	case CondGE: // NOT (bound > src)
		a.Cmgt(dst, bound, src)
		a.NotV(dst, dst)
	case CondE:
		a.Cmeq(dst, src, bound)
	default: // CondNE
		a.Cmeq(dst, src, bound)
		a.NotV(dst, dst)
	}
}
