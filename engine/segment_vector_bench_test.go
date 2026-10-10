package engine

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
)

// BenchmarkVecKernel times the distance kernel alone, on rows that fit in
// cache and rows that do not: their difference is how memory-bound it is.
func BenchmarkVecKernel(b *testing.B) {
	for _, rows := range []int{512, 100_000} {
		const dims = 384
		r := rand.New(rand.NewPCG(1, 1))
		heap := make([]byte, 0, rows*dims*4)
		offs := make([]int, rows)
		for i := range rows {
			offs[i] = len(heap)
			for range dims {
				heap = binary.LittleEndian.AppendUint32(heap, math.Float32bits(float32(r.NormFloat64())))
			}
		}
		q := make([]float32, dims)
		for i := range q {
			q[i] = float32(r.NormFloat64())
		}
		dist := make([]float32, rows)
		k := jitVecKernel(false)
		b.Run(map[int]string{512: "cache", 100_000: "memory"}[rows], func(b *testing.B) {
			for b.Loop() {
				vecDistJIT(k, false, heap, offs, q, 1, dist)
			}
			b.ReportMetric(float64(rows*dims)*float64(b.N)/b.Elapsed().Seconds()/1e9, "Gcomp/s")
		})
	}
}

// BenchmarkVecSideBuild times building one segment's int8 copy of a vector
// column, which a column's second search pays.
func BenchmarkVecSideBuild(b *testing.B) {
	const rows, dims = 100_000, 384
	r := rand.New(rand.NewPCG(1, 1))
	heap := make([]byte, 0, rows*dims*4)
	offs := make([]int, rows)
	for i := range rows {
		offs[i] = len(heap)
		for range dims {
			heap = binary.LittleEndian.AppendUint32(heap, math.Float32bits(float32(r.NormFloat64())))
		}
	}
	for b.Loop() {
		if _, ok := buildVecSide(heap, offs, dims); !ok {
			b.Fatal("declined")
		}
	}
}

// BenchmarkVectorText parses one 384-component vector written as text.
func BenchmarkVectorText(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 1))
	var sb []byte
	sb = append(sb, '[')
	for i := range 384 {
		if i > 0 {
			sb = append(sb, ',')
		}
		sb = strconv.AppendFloat(sb, r.NormFloat64(), 'g', 6, 64)
	}
	sb = append(sb, ']')
	b.SetBytes(int64(len(sb)))
	for b.Loop() {
		if _, err := parseVectorText(sb, vecF32); err != nil {
			b.Fatal(err)
		}
	}
}
