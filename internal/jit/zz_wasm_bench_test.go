//go:build js

package jit

import "unsafe"

import "testing"

func benchKernel(b *testing.B, emit func(Cond, bool, Cond) ([]byte, error), two bool) {
	const n = 65536
	a, c, v := make([]int64, n), make([]int64, n), make([]int64, n)
	for i := range a {
		a[i], c[i], v[i] = int64(i*7919%1000003), int64(i%10), int64(i)
	}
	code, _ := emit(CondG, two, CondNE)
	k, err := Map(code)
	if err != nil {
		b.Fatal(err)
	}
	var out, out2 int64
	args := &Args{A: unsafe.Pointer(&a[0]), C: unsafe.Pointer(&c[0]), V: unsafe.Pointer(&v[0]), N: n, XA: 500000, XC: 3, Out: unsafe.Pointer(&out), Out2: unsafe.Pointer(&out2)}
	b.ResetTimer()
	for range b.N {
		k.Call(args)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/n, "ns/row")
}

func goLoop(a []int64, x int64) (n int) {
	for _, v := range a {
		if v > x {
			n++
		}
	}
	return
}

func BenchmarkWasmCountScalar(b *testing.B) { benchKernel(b, EmitFilterCount, false) }
func BenchmarkWasmCountSIMD(b *testing.B)   { benchKernel(b, EmitFilterCountSIMD, false) }
func BenchmarkWasmCount2SIMD(b *testing.B)  { benchKernel(b, EmitFilterCountSIMD, true) }
func BenchmarkWasmSum2SIMD(b *testing.B)    { benchKernel(b, EmitFilterSumSIMD, true) }
func BenchmarkGoLoopCount(b *testing.B) {
	a := make([]int64, 65536)
	for i := range a {
		a[i] = int64(i * 7919 % 1000003)
	}
	s := 0
	for range b.N {
		s += goLoop(a, 500000)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/65536, "ns/row")
}
