//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"math/rand"
	"testing"
)

// TestEmittedRemainderMatchesSQLite: POpRem is C's truncating remainder with
// vdbe.c's divisor rules -- -1 gives 0 (MinInt64 % -1 included, which traps in
// a naive idiv), and a zero divisor is SQL NULL, which the program cannot hold
// and so flags for the caller to decline.
func TestEmittedRemainderMatchesSQLite(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	ref := func(a, b int64) int64 {
		if b == -1 {
			b = 1
		}
		return a % b
	}
	edges := []int64{math.MinInt64, math.MaxInt64, -1, 1, 0, -7, 7, 1 << 40, -(1 << 40)}
	divs := []int64{-1, 1, 2, 3, 7, -7, 1000003, math.MaxInt64, math.MinInt64}
	rng := rand.New(rand.NewSource(11))
	const n = 4096
	a, b := make([]int64, n), make([]int64, n)
	for i := range a {
		if i%5 == 0 {
			a[i] = edges[rng.Intn(len(edges))]
		} else {
			a[i] = rng.Int63n(2_000_001) - 1_000_000
		}
		b[i] = divs[rng.Intn(len(divs))]
	}
	// Emit each row's remainder through a min and a max so every value is
	// checked, not only their sum: count rows whose remainder equals the
	// reference, which must be all of them.
	run := func(bcol []int64) (int64, int64) {
		insns := []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpLoadCol, A: 1, B: 1},
			{Op: POpRem, A: 2, B: 0, C: 1},
			{Op: POpLoadCol, A: 3, B: 2},
			{Op: POpCmp, A: 4, B: 2, C: 3, Cond: CondE},
			{Op: POpSkipIfZero, A: 4},
			{Op: POpAccCount},
		}
		want := make([]int64, n)
		for i := range want {
			if bcol[i] != 0 {
				want[i] = ref(a[i], bcol[i])
			}
		}
		code, err := EmitProgram(insns, 3)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		kern, err := Map(code)
		if err != nil {
			t.Fatalf("map: %v", err)
		}
		defer kern.Close()
		regs := make([]int64, 8)
		var out, ovf int64
		args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf}
		args.Col[0], args.Col[1], args.Col[2] = &a[0], &bcol[0], &want[0]
		kern.Call2(args)
		return out, ovf
	}
	if got, ovf := run(b); got != n || ovf != 0 {
		t.Fatalf("remainders matching the reference: %d of %d (overflow flag %d)", got, n, ovf)
	}
	withZero := append([]int64(nil), b...)
	withZero[n/2] = 0
	if _, ovf := run(withZero); ovf == 0 {
		t.Fatal("a zero divisor was not flagged")
	}
}

// TestEmittedAbsMatchesSQLite: POpAbs is |x|, and abs(MinInt64) -- SQLite's
// "integer overflow" -- is flagged for the caller to decline.
func TestEmittedAbsMatchesSQLite(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	const n = 2048
	rng := rand.New(rand.NewSource(13))
	a, want := make([]int64, n), make([]int64, n)
	edges := []int64{math.MaxInt64, math.MinInt64 + 1, -1, 0, 1}
	for i := range a {
		if i%7 == 0 {
			a[i] = edges[i%len(edges)]
		} else {
			a[i] = rng.Int63n(2_000_001) - 1_000_000
		}
		want[i] = a[i]
		if want[i] < 0 {
			want[i] = -want[i]
		}
	}
	run := func(col []int64) (int64, int64) {
		insns := []ProgInsn{
			{Op: POpLoadCol, A: 0, B: 0},
			{Op: POpAbs, A: 1, B: 0},
			{Op: POpLoadCol, A: 2, B: 1},
			{Op: POpCmp, A: 3, B: 1, C: 2, Cond: CondE},
			{Op: POpSkipIfZero, A: 3},
			{Op: POpAccCount},
		}
		code, err := EmitProgram(insns, 2)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		kern, err := Map(code)
		if err != nil {
			t.Fatalf("map: %v", err)
		}
		defer kern.Close()
		regs := make([]int64, 8)
		var out, ovf int64
		args := &ProgArgs{N: n, Regs: &regs[0], Out: &out, Overflow: &ovf}
		args.Col[0], args.Col[1] = &col[0], &want[0]
		kern.Call2(args)
		return out, ovf
	}
	if got, ovf := run(a); got != n || ovf != 0 {
		t.Fatalf("abs matching |x|: %d of %d (overflow flag %d)", got, n, ovf)
	}
	withMin := append([]int64(nil), a...)
	withMin[n/3] = math.MinInt64
	if _, ovf := run(withMin); ovf == 0 {
		t.Fatal("abs(MinInt64) was not flagged")
	}
}
