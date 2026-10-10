package engine

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"
)

// TestVecDistKernelsExact checks the tiled kernels (the JIT's where there is
// one, and the Go tile) against the scalar distance functions bit for bit,
// over rows at uneven offsets and values chosen to expose any reassociation
// or fused multiply-add: subnormals, huge and tiny magnitudes, zeros, infinities.
func TestVecDistKernelsExact(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	special := []float32{0, float32(math.Copysign(0, -1)), 1e-41, -3e-39, 1e30, -2.5e35, float32(math.Inf(1)), 1e-20, 3.4e38}
	for _, packed := range []bool{false, true} { // back to back: the strided kernel
		for _, dims := range []int{1, 3, 8, 17, 384} {
			for _, nRows := range []int{1, 15, 16, 33, 100} {
				heap := make([]byte, 0, 1<<16)
				var offs []int
				rowVecs := make([]vector, nRows)
				for range nRows {
					if !packed && r.IntN(4) == 0 {
						heap = append(heap, 0, 0, 0, 0) // a gap between rows
					}
					off := len(heap)
					for range dims {
						x := float32(r.NormFloat64() * math.Pow(10, float64(r.IntN(12)-6)))
						if r.IntN(10) == 0 {
							x = special[r.IntN(len(special))]
						}
						heap = binary.LittleEndian.AppendUint32(heap, math.Float32bits(x))
					}
					offs = append(offs, off)
				}
				for i, off := range offs {
					rowVecs[i] = vector{typ: vecF32, dims: dims, data: heap[off : off+4*dims]}
				}
				q := make([]float32, dims)
				for i := range q {
					q[i] = float32(r.NormFloat64())
				}
				qv := vector{typ: vecF32, dims: dims}
				for _, x := range q {
					qv.data = binary.LittleEndian.AppendUint32(qv.data, math.Float32bits(x))
				}
				qn := vecSelfDot(q)
				for _, l2 := range []bool{false, true} {
					got := make([]float32, nRows)
					done := 0
					if k := jitVecKernel(l2); k != nil {
						done = vecDistJIT(k, l2, heap, offs, q, qn, got)
					}
					if packed && dims%8 == 0 && nRows >= 8 && jitVecKernel(l2) != nil && done == 0 {
						t.Fatalf("dims=%d rows=%d: the strided kernel did not run", dims, nRows)
					}
					rows, ok := f32Rows(heap, offs[done:], dims)
					if !ok {
						t.Fatal("f32Rows declined")
					}
					if l2 {
						l2Tile(rows, q, got[done:])
					} else {
						cosTile(rows, q, qn, got[done:])
					}
					for i := range nRows {
						var want float32
						if l2 {
							want = distanceL2(rowVecs[i], qv)
						} else {
							want = distanceCos(rowVecs[i], qv)
						}
						if math.Float32bits(got[i]) != math.Float32bits(want) && !(got[i] != got[i] && want != want) {
							t.Fatalf("dims=%d rows=%d l2=%v row %d (jit rows %d): got %v (%08x), want %v (%08x)",
								dims, nRows, l2, i, done, got[i], math.Float32bits(got[i]), want, math.Float32bits(want))
						}
					}
				}
			}
		}
	}
}
