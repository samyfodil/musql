package engine

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestVec1KernelsMatchGo checks the JIT's vec1 kernels against the Go loops
// they stand in for, and those against the row-at-a-time definitions, to the
// bit, over awkward sizes and values.
func TestVec1KernelsMatchGo(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	val := func() float32 {
		switch r.IntN(20) {
		case 0:
			return 0
		case 1:
			return float32(r.NormFloat64() * 1e20)
		case 2:
			return float32(r.NormFloat64() * 1e-20)
		}
		return float32(r.NormFloat64())
	}
	for _, n := range []int{1, 3, 8, 13, 48, 384} {
		for _, m := range []int{1, 7, 8, 9, 31, 256, 316} {
			rows := make([]float32, m*n)
			for i := range rows {
				rows[i] = val()
			}
			q := make([]float32, n)
			for i := range q {
				q[i] = val()
			}
			t8 := vec1T8(rows, m, n)
			nBlk := vec1T8Blocks(t8, n)
			for _, op := range []jit.Vec1Op{jit.Vec1L2F, jit.Vec1DotF} {
				got := make([]float32, 8*nBlk)
				want := make([]float32, 8*nBlk)
				vec1T8F(op, t8, n, q, got)
				vec1T8FGo(op, t8, nBlk, n, q, want)
				for i := range want {
					if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
						t.Fatalf("op %d n=%d m=%d row %d: %v, want %v", op, n, m, i, got[i], want[i])
					}
				}
				if op == jit.Vec1DotF {
					for i := range m {
						if w := vec1Dot(rows[i*n:i*n+n], q); math.Float32bits(got[i]) != math.Float32bits(w) {
							t.Fatalf("dot n=%d m=%d row %d: %v, vec1Dot %v", n, m, i, got[i], w)
						}
					}
				}
			}
			got := make([]float64, 8*nBlk)
			want := make([]float64, 8*nBlk)
			vec1T8L2D(t8, n, q, got)
			vec1T8L2DGo(t8, nBlk, n, q, want)
			for i := range want {
				if math.Float64bits(got[i]) != math.Float64bits(want[i]) {
					t.Fatalf("L2D n=%d m=%d row %d: %v, want %v", n, m, i, got[i], want[i])
				}
			}
			for i := range m {
				if w := vec1L2(q, rows[i*n:i*n+n]); math.Float64bits(got[i]) != math.Float64bits(w) {
					t.Fatalf("L2D n=%d m=%d row %d: %v, vec1L2 %v", n, m, i, got[i], w)
				}
			}
		}
	}
	for _, n := range []int{3, 8, 384} {
		for _, nRow := range []int{1, 8, 21} {
			rows := make([]byte, 4*n*nRow)
			vecs := make([]float32, n*nRow)
			for i := range vecs {
				vecs[i] = val()
				vec1Native.PutUint32(rows[4*i:], math.Float32bits(vecs[i]))
			}
			q := make([]float32, n)
			for i := range q {
				q[i] = val()
			}
			for _, cos := range []bool{false, true} {
				got := make([]float64, nRow)
				vec1RowDists(rows, nRow, n, q, cos, got, make([]float32, n))
				for i := range nRow {
					w := vec1L2(q, vecs[i*n:i*n+n])
					if cos {
						w = vec1Cos(q, vecs[i*n:i*n+n])
					}
					if math.Float64bits(got[i]) != math.Float64bits(w) {
						t.Fatalf("rows n=%d nRow=%d cos=%v row %d: %v, want %v", n, nRow, cos, i, got[i], w)
					}
				}
			}
		}
	}
	for _, nBook := range []int{1, 5, 48} {
		for _, nNorm := range []int{0, 4} {
			for _, nBlk := range []int{1, 2, 17} {
				codes := make([]byte, nBlk*16*(nBook+nNorm))
				for i := range codes {
					codes[i] = byte(r.IntN(256))
				}
				lut := make([]float32, nBook*256)
				for i := range lut {
					lut[i] = val()
				}
				got := make([]float32, 16*nBlk)
				want := make([]float32, 16*nBlk)
				vec1Scan16(codes, nBlk, nBook, nNorm, lut, got)
				vec1Scan16Go(codes, nBlk, nBook, nNorm, lut, want)
				for i := range want {
					if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
						t.Fatalf("scan nBook=%d nNorm=%d nBlk=%d lane %d: %v, want %v", nBook, nNorm, nBlk, i, got[i], want[i])
					}
				}
			}
		}
	}
	// vec1PqBestMatch and the scalar vec1BestMatchN's choice, ties included.
	for _, n := range []int{1, 8, 384} {
		for _, m := range []int{1, 9, 256, 316} {
			for _, ties := range []bool{false, true} {
				rows := make([]float32, m*n)
				q := make([]float32, n)
				for i := range rows {
					rows[i] = val()
					if ties {
						rows[i] = float32(r.IntN(3))
					}
				}
				for i := range q {
					q[i] = val()
					if ties {
						q[i] = float32(r.IntN(3))
					}
				}
				for _, round := range []bool{false, true} {
					// Four at a time from estimates, against one at a time.
					vecs := make([]float32, 4*n)
					for i := range vecs {
						vecs[i] = q[i%n]
						if i >= n {
							vecs[i] = val()
							if ties {
								vecs[i] = float32(r.IntN(3))
							}
						}
					}
					var four [4]int
					ok := vec1Best4(vec1MakeT8N(rows, 1, m, n), rows, 1, m, n, vecs, n, round, &vec1Buf{}, four[:])
					for vv := range 4 {
						if !ok {
							break // no kernel here
						}
						if w, _, _ := vec1BestT8(vec1T8(rows, m, n), m, n, vecs[vv*n:vv*n+n], round, nil); four[vv] != w {
							t.Fatalf("best4 n=%d m=%d round=%v vec %d: row %d, want %d", n, m, round, vv, four[vv], w)
						}
					}
					got, gotD, _ := vec1BestT8(vec1T8(rows, m, n), m, n, q, round, nil)
					want, wantD := 0, math.Inf(1)
					for j := range m {
						d := vec1L2(q, rows[j*n:j*n+n])
						if round {
							d = float64(float32(d))
						}
						if d < wantD {
							want, wantD = j, d
						}
					}
					if got != want || gotD != wantD {
						t.Fatalf("best n=%d m=%d round=%v: row %d at %v, want row %d at %v", n, m, round, got, gotD, want, wantD)
					}
				}
			}
		}
	}
	// Rows a few ulps apart, where float32 estimates misorder them: the
	// four-at-a-time search must still give the exact answer.
	for _, n := range []int{8, 384} {
		m := 64
		base := make([]float32, n)
		for i := range base {
			base[i] = val()
		}
		rows := make([]float32, m*n)
		for j := range m {
			for i := range n {
				rows[j*n+i] = math.Float32frombits(math.Float32bits(base[i]) + uint32(r.IntN(4)))
			}
		}
		t8 := vec1T8(rows, m, n)
		for range 200 {
			vecs := make([]float32, 4*n)
			for i := range vecs {
				vecs[i] = base[i%n] + float32(r.NormFloat64()*1e-3)
			}
			for _, round := range []bool{false, true} {
				var four [4]int
				ok := vec1Best4(vec1MakeT8N(rows, 1, m, n), rows, 1, m, n, vecs, n, round, &vec1Buf{}, four[:])
				for vv := range 4 {
					if !ok {
						break
					}
					if w, _, _ := vec1BestT8(t8, m, n, vecs[vv*n:vv*n+n], round, nil); four[vv] != w {
						t.Fatalf("near best4 n=%d round=%v vec %d: row %d, want %d", n, round, vv, four[vv], w)
					}
				}
			}
		}
	}
	// A rebuild's PQ codes four at a time, against one at a time: codebooks
	// whose codes sit a few ulps apart, so estimates misorder them.
	for _, ne := range []int{1, 8, 16} {
		nb := 5
		mod := &vec1Model{hdr: vec1Header{nElem: uint32(nb * ne), nCodebook: uint32(nb)}, nCodeElem: ne}
		base := make([]float32, ne)
		for i := range base {
			base[i] = val()
		}
		mod.book = make([]float32, nb*vec1CodebookSize*ne)
		for i := range mod.book {
			mod.book[i] = math.Float32frombits(math.Float32bits(base[i%ne]) + uint32(r.IntN(4)))
			if r.IntN(10) == 0 {
				mod.book[i] = val()
			}
		}
		for range 100 {
			var a, tmp [4][]float32
			var got, want [4][]byte
			for i := range 4 {
				a[i] = make([]float32, nb*ne)
				for j := range a[i] {
					a[i][j] = base[j%ne] + float32(r.NormFloat64()*1e-3)
				}
				tmp[i] = make([]float32, nb*ne)
				got[i], want[i] = make([]byte, nb), make([]byte, nb)
				mod.encodePQN(a[i], want[i])
			}
			mod.quantizeN4(tmp, a, got, &vec1Buf{})
			for i := range 4 {
				if !slices.Equal(got[i], want[i]) {
					t.Fatalf("quantizeN4 ne=%d vec %d: %v, want %v", ne, i, got[i], want[i])
				}
			}
		}
	}
	// The training search: the kernel's lanes reduced to the first nearest
	// row, as a row-by-row search finds it.
	for _, n := range []int{1, 8, 384} {
		for _, m := range []int{1, 9, 256, 316} {
			for _, nVec := range []int{1, 3, 4, 9} {
				rows := make([]float32, m*n)
				for i := range rows {
					rows[i] = float32(r.IntN(3)) // ties, deliberately
				}
				vecs := make([]float32, nVec*n)
				for i := range vecs {
					vecs[i] = float32(r.IntN(3))
				}
				best := make([]int, nVec)
				dist := make([]float32, nVec)
				vec1Nearest(vec1MakeT8N(rows, 1, m, n), rows, m, n, vecs, best, dist)
				for i := range nVec {
					wb, wd := 0, float32(math.Inf(1))
					for j := range m {
						var d float32
						for k := range n {
							e := rows[j*n+k] - vecs[i*n+k]
							d += e * e
						}
						if d < wd {
							wb, wd = j, d
						}
					}
					if best[i] != wb || dist[i] != wd {
						t.Fatalf("nearest n=%d m=%d vec %d: row %d at %v, want row %d at %v", n, m, i, best[i], dist[i], wb, wd)
					}
				}
			}
		}
	}
	// The Walsh-Hadamard transform, against the scalar loop.
	for _, n := range []int{16, 32, 256, 1024} {
		x := make([]float32, n)
		for i := range x {
			x[i] = val()
		}
		want := slices.Clone(x)
		for stride := 1; stride < n; stride <<= 1 {
			for i := 0; i < n; i += stride << 1 {
				for j := i; j < i+stride; j++ {
					a, b := want[j], want[j+stride]
					want[j], want[j+stride] = a+b, a-b
				}
			}
		}
		vec1WHTPow2(x)
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(want[i]) {
				t.Fatalf("WHT n=%d [%d]: %v, want %v", n, i, x[i], want[i])
			}
		}
	}
	t.Logf("JIT kernels: %v", vec1Kernel(jit.Vec1L2D) != nil && vec1ScanKernel() != nil)
}
