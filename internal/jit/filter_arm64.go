//go:build arm64

package jit

// EmitFilterCount emits a kernel counting rows satisfying one or two
// integer comparisons over int64 column blocks using CCMP chaining.
func EmitFilterCount(condA Cond, two bool, condC Cond) ([]byte, error) {
	a := NewArm()
	a.LdrImm(X1, X0, OffA)
	a.LdrImm(X3, X0, OffN)
	a.LdrImm(X4, X0, OffXA)
	if two {
		a.LdrImm(X2, X0, OffC)
		a.LdrImm(X5, X0, OffXC)
	}
	a.MovImm16(X6, 0)
	a.Cbz(X3, "done")

	a.Label("loop")
	a.LdrPost(X7, X1, 8)
	if two {
		// Both loads first: CCMP reads X8, and issuing them together lets the
		// two misses overlap.
		a.LdrPost(X8, X2, 8)
	}
	a.Cmp(X7, X4)
	if two {
		// If condA already failed, write flags on which condC reads false --
		// the short circuit, expressed as data rather than as a branch.
		a.Ccmp(X8, X5, nzcvFalseFor(condC), condA)
		a.Cinc(X6, X6, condC)
	} else {
		a.Cinc(X6, X6, condA)
	}
	// CINC leaves the flags alone, so the counter's SUBS below is still
	// reporting its own subtraction and nothing in between.
	a.SubsImm(X3, X3, 1)
	a.Bcond(CondNE, "loop")

	a.Label("done")
	a.LdrImm(X7, X0, OffOut)
	a.StrImm(X6, X7, 0)
	a.Ret()
	return a.Code()
}
