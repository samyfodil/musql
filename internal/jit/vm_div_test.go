//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"math/bits"
	"math/rand"
	"runtime"
	"testing"
)

// divisorsAndDividends are the divisors VDiv's constant path takes, and the
// dividends to check each against: the edges, the multiples either side of
// 0, and random ones.
func divisorsAndDividends(rng *rand.Rand) ([]int64, func(d int64) []int64) {
	ds := []int64{2, 3, 5, 6, 7, 9, 10, 11, 25, 27, 64, 100, 641, 1000, 4096, 1 << 30, 1162261467, math.MaxInt32 - 1, math.MaxInt32}
	for range 40 {
		ds = append(ds, 2+rng.Int63n(math.MaxInt32-1))
	}
	ns := func(d int64) []int64 {
		out := []int64{0, 1, -1, d - 1, d, d + 1, -d + 1, -d, -d - 1, 2*d - 1, -2*d + 1, math.MinInt64, math.MaxInt64, math.MinInt64 + 1,
			math.MaxInt64 - 1, math.MinInt64 / d * d, math.MaxInt64 / d * d}
		for range 60 {
			out = append(out, rng.Int63()-rng.Int63(), rng.Int63n(1<<20)-1<<19)
		}
		return out
	}
	return ds, ns
}

// TestSdivMagic: the magic number and shift divide as Go does, with the
// sequence the emitters write.
func TestSdivMagic(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	ds, ns := divisorsAndDividends(rng)
	for _, d := range ds {
		if d&(d-1) == 0 {
			continue
		}
		m, s := sdivMagic(d)
		for _, n := range ns(d) {
			hi, _ := bits.Mul64(uint64(m), uint64(n))
			q := int64(hi) // the signed high half: corrected for each negative factor
			if m < 0 {
				q -= n
			}
			if n < 0 {
				q -= m
			}
			if m < 0 {
				q += n
			}
			q = q>>s + int64(uint64(n)>>63)
			if q != n/d {
				t.Fatalf("%d / %d: magic %d, %d gives %d, want %d", n, d, m, s, q, n/d)
			}
		}
	}
}

// TestVMDivConst: VDiv and VRem by a divisor a VInt of the same run wrote
// answer as Go's / and % do, every dividend, with no exit.
func TestVMDivConst(t *testing.T) {
	if !Available {
		t.Skip("no JIT on this platform")
	}
	rng := rand.New(rand.NewSource(2))
	ds, ns := divisorsAndDividends(rng)
	vmDivConsts.Store(0)
	for _, d := range ds {
		prog := []VInsn{{Op: VInt, A: 0, Imm: d}, {Op: VDiv, A: 0, B: 1, C: 2}, {Op: VRem, A: 0, B: 1, C: 3}, {Op: VExit}}
		for _, n := range ns(d) {
			regs := []tvalue{{}, {Typ: tvInt, I: n}, {}, {}}
			var args ProgArgs
			runVM(t, prog, []bool{true, false, false, false}, regs, &args, 0)
			if args.PC != 3 || regs[2].Typ != tvInt || regs[2].I != n/d || regs[3].Typ != tvInt || regs[3].I != n%d {
				t.Fatalf("%d / %d: pc %d, quotient %+v, remainder %+v; want %d and %d", n, d, args.PC, regs[2], regs[3], n/d, n%d)
			}
		}
	}
	if n := vmDivConsts.Load(); runtime.GOARCH != "wasm" && n < int64(2*len(ds)) {
		t.Fatalf("%d divisions by a constant emitted without a divide, want %d", n, 2*len(ds))
	}
	// Not a constant the run wrote (r0 from before the entry): the IDIV, and
	// its exits on 0 and -1.
	for _, y := range []int64{7, 0, -1} {
		regs := []tvalue{{Typ: tvInt, I: y}, {Typ: tvInt, I: 100}, {}}
		var args ProgArgs
		runVM(t, []VInsn{{Op: VDiv, A: 0, B: 1, C: 2}, {Op: VExit}}, []bool{true, false}, regs, &args, 0)
		if y == 7 && (args.PC != 1 || regs[2].I != 14) || y != 7 && args.PC != 0 {
			t.Fatalf("100 / %d: pc %d, r2 %+v", y, args.PC, regs[2])
		}
	}
}
