package engine

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
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

// TestVecBoundsContainExact checks the int8 bounds against the distance they
// stand in for, for every row: lo <= the exact float32 distance <= hi, over
// rows built to stress them -- huge and subnormal magnitudes, near-duplicates
// of the query (where L2's cancellation is worst), zeros, opposite vectors,
// mixed scales within a row.
func TestVecBoundsContainExact(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 13))
	for _, dims := range []int{1, 2, 7, 64, 384} {
		q := make([]float32, dims)
		for i := range q {
			q[i] = float32(r.NormFloat64())
		}
		var rows [][]float32
		for k := range 400 {
			x := make([]float32, dims)
			for i := range x {
				switch k % 8 {
				case 0: // near the query
					x[i] = q[i] * (1 + float32(r.NormFloat64())*1e-6)
				case 1: // the query itself
					x[i] = q[i]
				case 2: // opposite
					x[i] = -q[i]
				case 3: // huge
					x[i] = float32(r.NormFloat64() * 1e18)
				case 4: // tiny: rows whose scale straddles float32's smallest normal
					x[i] = float32(r.NormFloat64() * math.Pow(10, -34-float64(k%7)))
				case 5: // mixed scales
					x[i] = float32(r.NormFloat64() * math.Pow(10, float64(r.IntN(20)-10)))
				default:
					x[i] = float32(r.NormFloat64())
				}
			}
			rows = append(rows, x)
		}
		var heap []byte
		offs := make([]int, len(rows))
		for i, x := range rows {
			offs[i] = len(heap)
			for _, v := range x {
				heap = binary.LittleEndian.AppendUint32(heap, math.Float32bits(v))
			}
		}
		side, ok := buildVecSide(heap, offs, dims)
		if !ok {
			t.Fatal("buildVecSide declined")
		}
		qbytes := vector{typ: vecF32, dims: dims}
		for _, v := range q {
			qbytes.data = binary.LittleEndian.AppendUint32(qbytes.data, math.Float32bits(v))
		}
		for _, l2 := range []bool{false, true} {
			vq, ok := newVecQuery(q, l2)
			if !ok {
				t.Fatal("query admits no bounds")
			}
			bounded := 0
			for i, x := range rows {
				lo, hi, ok := vq.bounds(side, i)
				if !ok {
					continue
				}
				bounded++
				xv := vector{typ: vecF32, dims: dims}
				for _, v := range x {
					xv.data = binary.LittleEndian.AppendUint32(xv.data, math.Float32bits(v))
				}
				var d float32
				if l2 {
					d = distanceL2(xv, qbytes)
				} else {
					d = distanceCos(xv, qbytes)
				}
				if !(lo <= d && d <= hi) {
					t.Fatalf("dims=%d l2=%v row %d (kind %d): %v not in [%v, %v]", dims, l2, i, i%8, d, lo, hi)
				}
			}
			if bounded < len(rows)/2 {
				t.Fatalf("dims=%d l2=%v: only %d of %d rows bounded", dims, l2, bounded, len(rows))
			}
		}
	}
}

// TestI8DotKernel checks the JIT's int8 dot products against Go's, at the
// extremes of the codes' range.
func TestI8DotKernel(t *testing.T) {
	if jitI8DotKernel() == nil {
		t.Skip("no int8 dot-product kernel here")
	}
	r := rand.New(rand.NewPCG(5, 5))
	for _, d := range []int{16, 384, 4096} {
		const n = 37
		side := &segVecSide{dims: d, codes: make([]int8, n*d)}
		for i := range side.codes {
			side.codes[i] = int8(r.IntN(255) - 127)
			if r.IntN(5) == 0 {
				side.codes[i] = []int8{-127, 127}[r.IntN(2)]
			}
		}
		vq := &vecQuery{t32: make([]int32, d), t16: make([]int16, d)}
		for i := range d {
			c := int8(r.IntN(255) - 127)
			vq.t32[i], vq.t16[i] = int32(c), int16(c)
		}
		got := vq.dots(side, 3, n)
		for row := 3; row < n; row++ {
			if want := int8Dot(side.codes[row*d:(row+1)*d], vq.t32); got[row-3] != want {
				t.Fatalf("d=%d row %d: %d, want %d", d, row, got[row-3], want)
			}
		}
	}
}

// TestVecSideJITMatchesGo builds a sidecar both ways -- the JIT's max-abs and
// quantize kernels, and quantizeRow -- and requires every array to agree,
// over rows that take each special case: non-finite, zero, subnormal scale.
func TestVecSideJITMatchesGo(t *testing.T) {
	if jitMaxAbs() == nil || jitQuantize() == nil {
		t.Skip("no quantize kernels here")
	}
	r := rand.New(rand.NewPCG(17, 19))
	for _, dims := range []int{8, 64, 384} {
		const n = 203
		var heap []byte
		offs := make([]int, n)
		rows := make([][]float32, n)
		for i := range n {
			x := make([]float32, dims)
			for j := range x {
				switch i % 9 {
				case 0:
					x[j] = 0
				case 1:
					x[j] = float32(r.NormFloat64() * 1e-38)
				case 2:
					x[j] = float32(r.NormFloat64() * 1e-44)
				case 3:
					x[j] = float32(r.NormFloat64() * 3e38)
				default:
					x[j] = float32(r.NormFloat64() * math.Pow(10, float64(r.IntN(10)-5)))
				}
			}
			if i%9 == 4 {
				x[r.IntN(dims)] = float32(math.NaN())
			}
			if i%9 == 5 {
				x[r.IntN(dims)] = float32(math.Inf(-1))
			}
			offs[i] = len(heap)
			for _, v := range x {
				heap = binary.LittleEndian.AppendUint32(heap, math.Float32bits(v))
			}
			rows[i] = x
		}
		got, ok := buildVecSide(heap, offs, dims)
		if !ok {
			t.Fatal("buildVecSide declined")
		}
		for i, x := range rows {
			code := make([]int8, dims)
			s, e, c1 := quantizeRow(x, code)
			if math.IsInf(float64(e), 0) && math.IsInf(float64(got.errb[i]), 0) {
				continue // no bound: the rest does not matter
			}
			if got.scale[i] != s || got.errb[i] != e || got.c1[i] != c1 || !slices.Equal(got.codes[i*dims:(i+1)*dims], code) {
				t.Fatalf("dims=%d row %d: jit (%v %v %d) go (%v %v %d)", dims, i, got.scale[i], got.errb[i], got.c1[i], s, e, c1)
			}
			if nn := vecSelfDot(x); math.Float32bits(got.norm[i]) != math.Float32bits(nn) && !(nn != nn && got.norm[i] != got.norm[i]) {
				t.Fatalf("dims=%d row %d: norm %v, want %v", dims, i, got.norm[i], nn)
			}
		}
	}
}
