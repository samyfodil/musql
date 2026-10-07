//go:build !wasm

package jit

import "testing"

// TestEmitRowSelectsTheMatchingRows is POpEmitRow's own gate: a compiled
// predicate must hand back exactly the row indices a plain Go loop would, for
// every selectivity including the two that bound it (nothing matches, and
// everything does).
func TestEmitRowSelectsTheMatchingRows(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	const n = 1000
	col := make([]int64, n)
	for i := range col {
		col[i] = int64(i % 100)
	}
	for _, bound := range []int64{-1, 0, 49, 98, 99, 1000} {
		// r[0] = col0; r[1] = bound; r[2] = (r[0] > r[1]); skip if zero; emit.
		insns := []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpCmp, A: 2, B: 0, C: 1, Cond: CondG},
			{Op: POpSkipIfZero, A: 2},
			{Op: POpEmitRow},
		}
		code, err := EmitProgram(insns, 1)
		if err != nil {
			t.Fatalf("bound %d: %v", bound, err)
		}
		k, err := Map(code)
		if err != nil {
			t.Fatalf("bound %d: %v", bound, err)
		}
		regs := make([]int64, 8)
		regs[1] = bound
		sel := make([]int64, n)
		var out, ovf int64
		args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf, Sel: &sel[0]}
		args.Col[0] = &col[0]
		k.Call2(args)
		k.Close()

		var want []int
		for i := 0; i < n; i++ {
			if col[i] > bound {
				want = append(want, i)
			}
		}
		if int(out) != len(want) {
			t.Fatalf("bound %d: selected %d rows, want %d", bound, out, len(want))
		}
		for j := 0; j < int(out); j++ {
			// The emitted index counts up from -n; the caller adds n.
			if got := int(sel[j]) + n; got != want[j] {
				t.Fatalf("bound %d: selection[%d] = %d, want %d", bound, j, got, want[j])
			}
		}
	}
}
