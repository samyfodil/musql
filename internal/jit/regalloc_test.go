//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import "testing"

// TestRegAllocPressure runs a program with more values live at once than any
// backend has registers for -- thirty column-derived values all summed at the
// end, over one column so the pool is as large as it gets -- and requires
// the allocated kernel to answer as the unallocated one and the Go reference.
func TestRegAllocPressure(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	defer SetFusion(true)
	const n, live = 200, 30
	col := make([]int64, n)
	for i := range col {
		col[i] = int64(i*7%23) - 11
	}
	var p []ProgInsn
	for i := 0; i < live; i++ {
		p = append(p, ProgInsn{Op: POpLoadCol, A: i, B: 0},
			ProgInsn{Op: POpSetConst, A: live + i%2, B: i + 1},
			ProgInsn{Op: POpMul, A: i, B: i, C: live + i%2})
	}
	for i := 1; i < live; i++ {
		p = append(p, ProgInsn{Op: POpAdd, A: 0, B: 0, C: i})
	}
	p = append(p, ProgInsn{Op: POpCmpSkip, B: 0, C: live + 3, Cond: CondG}, ProgInsn{Op: POpAccSum, A: 0, B: live + 2})
	run := func(fuse bool) (int64, int64) {
		SetFusion(fuse)
		RegAllocStatsForTest()
		code, err := EmitProgram(p, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, spilled, _ := RegAllocStatsForTest(); fuse && spilled == 0 {
			t.Fatal("nothing spilled: the program does not exceed the register file")
		}
		kern, err := Map(code)
		if err != nil {
			t.Fatal(err)
		}
		defer kern.Close()
		regs := make([]int64, live+4)
		var out, ovf int64
		args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf}
		args.Col[0] = &col[0]
		kern.Call2(args)
		return out, regs[live+2]
	}
	regs := make([]int64, live+4)
	want := interpProgram(p, [][]int64{col}, n, regs)
	for _, fuse := range []bool{false, true} {
		if out, cnt := run(fuse); out != want.out || cnt != regs[live+2] {
			t.Fatalf("fused=%v: sum %d over %d rows, want %d over %d", fuse, out, cnt, want.out, regs[live+2])
		}
	}
}
