package libm

import (
	"unsafe"

	"github.com/samyfodil/musql/internal/fir"
)

// K writes this package's functions as JIT kernels (internal/fir): each
// method is the Go function of the same name, step for step, inline in the
// kernel being built. What a kernel answers is what the function answers, bit
// for bit, and the inputs a kernel leaves to Go (fir's Exit) get the
// function's own answer there; TestKernels holds the two to that.
//
// A Go early return becomes a variable set in each branch, since a kernel
// built from these methods composes them (pow is a log and an exp) and Return
// would end the whole kernel.
type K struct{ *fir.B }

// NewKernel starts a kernel built for this CPU's choice of glibc build.
func NewKernel(name string, nargs int) K { return K{fir.New(name, nargs, fused)} }

// Fused is whether glibc's FMA builds are the ones this package answers as,
// which the JIT must agree with.
func Fused() bool { return fused }

// SetFusedForTest makes this package answer as glibc's FMA build (or its
// plain one), returning the undo.
func SetFusedForTest(on bool) (restore func()) {
	old := fused
	fused = on
	return func() { fused = old }
}

// words is a table of pairs of doubles, as the uint64 words a kernel loads.
func words[T any](tab []T) []uint64 {
	n := len(tab) * int(unsafe.Sizeof(tab[0])) / 8
	return unsafe.Slice((*uint64)(unsafe.Pointer(&tab[0])), n)
}

// result is a variable for a method's answer.
func (k K) result() fir.Val { return k.Var(fir.F) }

func (k K) u(c uint64) fir.Val { return k.Const(c) }

// Kernel1 is a one-argument function with a kernel: the function, and the
// method that writes it.
type Kernel1 struct {
	Go func(float64) float64
	K  func(K, fir.Val) fir.Val
}

// Kernels1 is every one-argument function with a kernel, by C's name.
var Kernels1 = map[string]Kernel1{
	"exp":   {Exp, K.Exp},
	"log":   {Log, K.Log},
	"log2":  {Log2, K.Log2},
	"log10": {Log10, K.Log10},
	"sin":   {Sin, K.Sin},
	"cos":   {Cos, K.Cos},
	"expm1": {Expm1, K.Expm1},
	"sinh":  {Sinh, K.Sinh},
	"cosh":  {Cosh, K.Cosh},
	"tanh":  {Tanh, K.Tanh},
	"asinh": {Asinh, K.Asinh},
	"acosh": {Acosh, K.Acosh},
	"atanh": {Atanh, K.Atanh},
	"tan":   {Tan, K.Tan},
	"atan":  {Atan, K.Atan},
	"asin":  {Asin, K.Asin},
	"acos":  {Acos, K.Acos},
}

// Kernel2 is a two-argument function with a kernel.
type Kernel2 struct {
	Go func(float64, float64) float64
	K  func(K, fir.Val, fir.Val) fir.Val
}

// Kernels2 is every two-argument function with a kernel.
var Kernels2 = map[string]Kernel2{
	"pow":   {Pow, K.Pow},
	"atan2": {Atan2, K.Atan2},
}

// Build1 is the kernel answering name(x), built for this CPU's glibc build.
func Build1(name string) (*fir.Func, error) {
	k := NewKernel(name, 1)
	k.Return(Kernels1[name].K(k, k.Arg(0)))
	return k.Func()
}

// Build2 is the kernel answering name(x, y).
func Build2(name string) (*fir.Func, error) {
	k := NewKernel(name, 2)
	k.Return(Kernels2[name].K(k, k.Arg(0), k.Arg(1)))
	return k.Func()
}
