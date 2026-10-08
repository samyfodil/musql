//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math/rand"
	"testing"
	"unsafe"
)

// runWithServices is the engine's driver loop in miniature: call the
// program, and while it stops for a service, perform it on the row it stopped
// at and call again.
func runWithServices(t *testing.T, kern *Code, args *ProgArgs, n int, svc func(k, row int)) {
	t.Helper()
	for calls := 0; ; calls++ {
		kern.Call2(args)
		if args.PC == 0 {
			return
		}
		if calls > 10*n+10 {
			t.Fatal("the program kept stopping: resume does not advance")
		}
		svc(int(args.PC-1), n+int(args.Row))
	}
}

// TestProgramServiceExitsResume: a program stops at each POpService, the
// caller performs the service on that row, and the program resumes after it
// on the same row with its loop index, accumulator and overflow flag intact.
func TestProgramServiceExitsResume(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	if POffRow != int(unsafe.Offsetof(ProgArgs{}.Row)) {
		t.Fatalf("POffRow %d, field at %d", POffRow, unsafe.Offsetof(ProgArgs{}.Row))
	}
	const n = 3000
	rng := rand.New(rand.NewSource(23))
	a := make([]int64, n)
	for i := range a {
		a[i] = rng.Int63n(1000)
	}
	// r0 = a; service 0: r1 = (a*7 + row) % 101 (Go); keep rows where r1 > 40;
	// service 1: r2 = row % 5; sum r2 over the kept rows.
	insns := []ProgInsn{
		{Op: POpLoadCol, A: 0, B: 0},
		{Op: POpService, A: 0},
		{Op: POpSetConst, A: 3, B: 40},
		{Op: POpCmp, A: 4, B: 1, C: 3, Cond: CondG},
		{Op: POpSkipIfZero, A: 4},
		{Op: POpService, A: 1},
		{Op: POpAccSum, A: 2, B: 5},
	}
	code, err := EmitProgram(insns, 1)
	if err != nil {
		t.Fatal(err)
	}
	kern, err := Map(code)
	if err != nil {
		t.Fatal(err)
	}
	defer kern.Close()
	regs := make([]int64, 8)
	var out, ovf int64
	args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf}
	args.Col[0] = &a[0]
	calls := [2]int{}
	runWithServices(t, kern, args, n, func(k, row int) {
		calls[k]++
		switch k {
		case 0:
			if regs[0] != a[row] {
				t.Fatalf("service 0 at row %d sees r0=%d, column holds %d", row, regs[0], a[row])
			}
			regs[1] = (a[row]*7 + int64(row)) % 101
		case 1:
			regs[2] = int64(row % 5)
		}
	})
	var want, kept int64
	for i := range a {
		if (a[i]*7+int64(i))%101 > 40 {
			want += int64(i % 5)
			kept++
		}
	}
	if out != want || ovf != 0 || regs[5] != kept {
		t.Fatalf("sum %d rows %d (overflow %d), want %d over %d", out, regs[5], ovf, want, kept)
	}
	if calls[0] != n || int64(calls[1]) != kept {
		t.Fatalf("services called %v times, want [%d %d]", calls, n, kept)
	}
	if args.PC != 0 {
		t.Fatalf("finished with PC %d", args.PC)
	}

	// And as a selection, where the accumulator is the count of rows emitted.
	sel := make([]int64, n)
	insns = []ProgInsn{
		{Op: POpLoadCol, A: 0, B: 0},
		{Op: POpService, A: 0},
		{Op: POpSkipIfZero, A: 1},
		{Op: POpEmitRow},
	}
	code, err = EmitProgram(insns, 1)
	if err != nil {
		t.Fatal(err)
	}
	kern2, err := Map(code)
	if err != nil {
		t.Fatal(err)
	}
	defer kern2.Close()
	out = 0
	args = &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf, Sel: &sel[0]}
	args.Col[0] = &a[0]
	runWithServices(t, kern2, args, n, func(k, row int) {
		regs[1] = 0
		if a[row]%3 == 0 {
			regs[1] = 1
		}
	})
	var got []int64
	for _, i := range sel[:out] {
		got = append(got, i+n)
	}
	var wantSel []int64
	for i := range a {
		if a[i]%3 == 0 {
			wantSel = append(wantSel, int64(i))
		}
	}
	if len(got) != len(wantSel) {
		t.Fatalf("selected %d rows, want %d", len(got), len(wantSel))
	}
	for i := range got {
		if got[i] != wantSel[i] {
			t.Fatalf("selection[%d] = %d, want %d", i, got[i], wantSel[i])
		}
	}
}
