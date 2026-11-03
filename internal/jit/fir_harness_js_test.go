//go:build js

package jit

import (
	"math"

	"github.com/samyfodil/musql/internal/fir"
)

// cpuFeat is what the kernel tests ask of the CPU: wasm rounds, and fma is
// computed from doubles.
var cpuFeat = struct{ fma, round bool }{fma: true, round: true}

// firBudgets: wasm keeps every value in a local, so there is one budget.
var firBudgets = []struct{}{{}}

// firLoop is a test harness around one kernel: for each i < N it runs the
// kernel on Col[0][i] (and Col[1][i]), storing the answer at Col[2][i], or
// a 1 at Col[3][i] when the kernel leaves the input to Go.
func firLoop(f *fir.Func, _ int) (*Code, error) {
	w := &Wasm{}
	ptr := make([]uint32, 4)
	for c := range ptr {
		ptr[c] = w.Local(wI32)
		w.LoadPtr(uint32(POffCol + 8*c))
		w.Set(ptr[c])
	}
	n := w.Local(wI64)
	w.Get(0)
	w.LoadI64(POffN)
	w.Set(n)
	args := []uint32{w.Local(wF64), w.Local(wF64)}[:f.NArgs]
	out := w.Local(wF64)
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(n)
	w.op(opI64Eqz)
	w.BrIf(1)
	for i, a := range args {
		w.Get(ptr[i])
		w.LoadF64(0)
		w.Set(a)
	}
	w.op(opBlock, opVoid) // next
	err := emitFirWasm(w, f, args, out, func() {
		w.Get(ptr[3])
		w.I64(1)
		w.StoreI64(0)
		w.Br(1)
	})
	if err != nil {
		return nil, err
	}
	w.Get(ptr[2])
	w.Get(out)
	w.StoreF64(0)
	w.op(opEnd)
	for _, p := range ptr {
		w.Get(p)
		w.I32(8)
		w.op(opI32Add)
		w.Set(p)
	}
	w.Get(n)
	w.I64(1)
	w.op(opI64Sub)
	w.Set(n)
	w.Br(0)
	w.op(opEnd, opEnd)
	return Map(w.Module())
}

// firFMod is whether this backend computes every fir.OpFMod itself.
const firFMod = false

// firFModExits is whether this backend's fmod leaves (x, y) to Go: unless
// |x| < |y|, a quotient |x/y| not below 2^53, which includes an infinity, a
// zero divisor and a NaN. Its fma may leave more (firExitsMore).
func firFModExits(x, y float64) bool {
	ax, ay := math.Abs(x), math.Abs(y)
	return !(ax < ay) && !(ax/ay < 1<<53)
}

// firFeaturesOptional is whether a CPU can lack the fma or the rounding
// instructions.
const firFeaturesOptional = false

// firExitsMore is whether this backend leaves to Go inputs fir.Eval answers:
// those where its fma could overflow or underflow.
const firExitsMore = true
