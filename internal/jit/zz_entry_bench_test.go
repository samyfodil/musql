//go:build js

package jit

import "testing"

// BenchmarkWasmEntry is the cost of one call into generated code: what the
// engine's vmJITMinWork weighs an entry from the VDBE against.
func BenchmarkWasmEntry(b *testing.B) {
	prog := []VInsn{{Op: VExit}}
	lay := ValueLayout{Size: 32, OffTyp: 0, OffI: 8, OffF: 16, OffS: 24, TypNull: 5, TypInt: 1}
	code, err := EmitVM(prog, 1, lay)
	if err != nil {
		b.Fatal(err)
	}
	k, err := Map(code)
	if err != nil {
		b.Fatal(err)
	}
	regs := make([]int64, 4)
	args := ProgArgs{Regs: &regs[0]}
	for range b.N {
		args.PC = 0
		k.Call2(&args)
	}
}
