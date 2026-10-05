package jit

import "testing"

// TestEmitVMLargeProgram compiles a program whose code is far past the +-1 MB a
// conditional branch reaches on arm64 (Doom's is ~12 MB). Before islands, one
// out-of-range branch failed the whole compile and the program silently ran on
// the VDBE.
func TestEmitVMLargeProgram(t *testing.T) {
	const n = 60000
	prog := make([]VInsn, n)
	for pc := range prog {
		prog[pc] = VInsn{Op: VAdd, A: 0, B: 1, C: 2}
	}
	prog[n-1] = VInsn{Op: VCmpJump, A: 0, B: 1, T: n / 2, Cond: CondL} // a far backward jump
	prog[10] = VInsn{Op: VCmpJump, A: 0, B: 1, T: n - 2, Cond: CondL}  // a far forward one
	lay := ValueLayout{Size: 32, OffTyp: 0, OffI: 8, OffF: 16, OffS: 24, TypNull: 5, TypInt: 1}
	b, err := EmitVM(prog, 3, lay)
	if !Available {
		return
	}
	if err != nil {
		t.Fatalf("EmitVM: %v", err)
	}
	if len(b) < 2<<20 {
		t.Fatalf("code is %d bytes; the test needs more than 2 MB to mean anything", len(b))
	}
}
