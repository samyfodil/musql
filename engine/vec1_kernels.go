package engine

// The arithmetic vec1's searches and rebuilds spend their time in, laid out
// so a SIMD lane can hold a row: centroids, PQ codebooks and the OPQ rotation
// are kept "T8" -- in blocks of eight rows, component by component -- so one
// register holds the same component of eight rows, and each row's sum is
// accumulated in component order with one rounding per operation, as vec1's
// portable C does it. The answers are its answers, to the bit; only the rows
// done at once change. The JIT's kernels (internal/jit, vec1_jit.go) take a
// call when the machine has one, these loops otherwise.

import (
	"math"
	mathbits "math/bits"
	"sync"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// vec1T8 lays m rows of n float32s out in blocks of eight: block b holds
// component d of rows 8b..8b+7 at [b*8*n + 8*d + lane]. The blocks are an
// even number, the kernels' unit; rows past m are +Inf, so no search can
// choose one.
func vec1T8(rows []float32, m, n int) []float32 {
	nBlk := (m + 15) / 16 * 2
	out := make([]float32, nBlk*8*n)
	for i := m * n; i < len(out); i++ {
		out[i] = float32(math.Inf(1))
	}
	for r := range m {
		base := r / 8 * 8 * n
		for d := range n {
			out[base+8*d+r%8] = rows[r*n+d]
		}
	}
	return out
}

// vec1T8Blocks is the block count of a T8 layout of n-dimensional rows.
func vec1T8Blocks(t []float32, n int) int { return len(t) / (8 * n) }

// vec1T8F is every row's float32 sum against q: (q-x)^2 for jit.Vec1L2F
// (vec1ModelTDist), x*q for jit.Vec1DotF (vec1DotProduct), in component
// order. out holds a float32 per row of the layout.
func vec1T8F(op jit.Vec1Op, t []float32, n int, q []float32, out []float32) {
	nBlk := vec1T8Blocks(t, n)
	if k := vec1Kernel(op); k != nil && nBlk > 0 && len(q) >= n && len(out) >= 8*nBlk {
		args := jit.Args{
			A:   unsafe.Pointer(unsafe.SliceData(t)),
			XC:  int64(32 * n),
			V:   unsafe.Pointer(unsafe.SliceData(q)),
			N:   int64(nBlk / 2),
			XA:  int64(n),
			Out: unsafe.Pointer(unsafe.SliceData(out)),
		}
		k.Call(&args)
		return
	}
	vec1T8FGo(op, t, nBlk, n, q, out)
}

func vec1T8FGo(op jit.Vec1Op, t []float32, nBlk, n int, q []float32, out []float32) {
	for b := range nBlk {
		var acc [8]float32
		blk := t[b*8*n : (b+1)*8*n]
		for d := range n {
			v := q[d]
			c := blk[8*d : 8*d+8]
			for l := range 8 {
				if op == jit.Vec1DotF {
					acc[l] += float32(c[l] * v)
				} else {
					e := v - c[l]
					acc[l] += float32(e * e)
				}
			}
		}
		copy(out[8*b:], acc[:])
	}
}

// vec1T8L2D is every row's squared distance to q as vec1L2Dist sums it: each
// difference in float32, squared and added in float64.
func vec1T8L2D(t []float32, n int, q []float32, out []float64) {
	nBlk := vec1T8Blocks(t, n)
	if k := vec1Kernel(jit.Vec1L2D); k != nil && nBlk > 0 && len(q) >= n && len(out) >= 8*nBlk {
		args := jit.Args{
			A:   unsafe.Pointer(unsafe.SliceData(t)),
			XC:  int64(32 * n),
			V:   unsafe.Pointer(unsafe.SliceData(q)),
			N:   int64(nBlk / 2),
			XA:  int64(n),
			Out: unsafe.Pointer(unsafe.SliceData(out)),
		}
		k.Call(&args)
		return
	}
	vec1T8L2DGo(t, nBlk, n, q, out)
}

func vec1T8L2DGo(t []float32, nBlk, n int, q []float32, out []float64) {
	for b := range nBlk {
		var acc [8]float64
		blk := t[b*8*n : (b+1)*8*n]
		for d := range n {
			v := q[d]
			c := blk[8*d : 8*d+8]
			for l := range 8 {
				e := float64(v - c[l])
				acc[l] += float64(e * e)
			}
		}
		copy(out[8*b:], acc[:])
	}
}

// vec1Scan16 sums each block of 16 codes through the table (vec1ScanBlocked's
// accumulation): for block k and book b, out[16k+j] += lut[256b +
// codes[k*szBlk + (b+nNorm)*16 + j]], books in order, in float32.
func vec1Scan16(codes []byte, nBlk, nBook, nNorm int, lut []float32, out []float32) {
	szBlk := 16 * (nBook + nNorm)
	if k := vec1ScanKernel(); k != nil && nBlk > 0 && nBook > 0 &&
		len(codes) >= nBlk*szBlk && len(lut) >= nBook*256 && len(out) >= 16*nBlk {
		args := jit.Args{
			A:   unsafe.Pointer(&codes[nNorm*16]),
			XC:  int64(szBlk),
			N:   int64(nBlk),
			XA:  int64(nBook),
			V:   unsafe.Pointer(unsafe.SliceData(lut)),
			Out: unsafe.Pointer(unsafe.SliceData(out)),
		}
		k.Call(&args)
		return
	}
	vec1Scan16Go(codes, nBlk, nBook, nNorm, lut, out)
}

func vec1Scan16Go(codes []byte, nBlk, nBook, nNorm int, lut []float32, out []float32) {
	szBlk := 16 * (nBook + nNorm)
	for k := range nBlk {
		var acc [16]float32
		blk := codes[k*szBlk:]
		for b := range nBook {
			tab := lut[b*256 : b*256+256]
			c := blk[(b+nNorm)*16 : (b+nNorm)*16+16]
			for j := range 16 {
				acc[j] += tab[c[j]]
			}
		}
		copy(out[16*k:], acc[:])
	}
}

// vec1RowDists is each of nRow vectors' vec1L2Dist (or vec1CosDist) from q:
// a flat index's list, rows of n native-order float32s back to back. Whole
// tiles of eight rows take a kernel when n is a multiple of 8, the rest are
// decoded into vec and summed in Go.
func vec1RowDists(rows []byte, nRow, n int, q []float32, cos bool, out []float64, vec []float32) {
	done := 0
	if n%8 == 0 && nRow >= 8 && len(rows) >= 4*n*nRow && len(q) >= n {
		tiles := nRow &^ 7
		if cos {
			if k := jitVecKernelOf(jitVecKey{strided: true}); k != nil {
				sums := make([]float32, 2*tiles)
				k.Call(&jit.Args{
					A:    unsafe.Pointer(&rows[0]),
					XC:   int64(4 * n),
					V:    unsafe.Pointer(unsafe.SliceData(q)),
					N:    int64(tiles),
					XA:   int64(n),
					Out:  unsafe.Pointer(&sums[0]),
					Out2: unsafe.Pointer(&sums[tiles]),
				})
				var as float32
				for _, x := range q[:n] {
					as += float32(x * x)
				}
				for i := range tiles {
					out[i] = vec1CosFinish(sums[i], as, sums[tiles+i])
				}
				done = tiles
			}
		} else if k := vec1RowsKernel(); k != nil {
			k.Call(&jit.Args{
				A:   unsafe.Pointer(&rows[0]),
				XC:  int64(4 * n),
				V:   unsafe.Pointer(unsafe.SliceData(q)),
				N:   int64(tiles),
				XA:  int64(n),
				Out: unsafe.Pointer(unsafe.SliceData(out)),
			})
			done = tiles
		}
	}
	for i := done; i < nRow; i++ {
		vec1ReadFloats(vec, rows[4*n*i:4*n*(i+1)])
		if cos {
			out[i] = vec1Cos(q[:n], vec)
		} else {
			out[i] = vec1L2(q[:n], vec)
		}
	}
}

var vec1RowsCode = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1RowsL2D) })

// vec1RowsKernel is the compiled flat-list L2 kernel, nil without one.
func vec1RowsKernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1RowsCode()
}

// vec1Nearest is training's nearest-row search: for each vector (rows of n
// in vecs), the first of the m rows of T8 layout t nearest it by the float32
// sum of squared differences, and that distance. vec1's training is
// randomized and its builds differ in this arithmetic (see
// jit.EmitVec1Best4), so a trained model is not compared to the bit.
func vec1Nearest(t []float32, m, n int, vecs []float32, best []int, dist []float32) {
	nVec := len(vecs) / n
	nBlk := vec1T8Blocks(t, n)
	if m == 1 && n%8 == 0 && nVec >= 8 {
		// One row (k-means++'s rounds): the vectors are the rows, back to
		// back, and the one row the query, eight vectors to a lane-set.
		if ks := jitVecKernelOf(jitVecKey{l2: true, strided: true}); ks != nil {
			c := make([]float32, n)
			for d := range n {
				c[d] = t[8*d]
			}
			done := nVec &^ 7
			sums := make([]float32, 2*done)
			ks.Call(&jit.Args{
				A:    unsafe.Pointer(unsafe.SliceData(vecs)),
				XC:   int64(4 * n),
				V:    unsafe.Pointer(unsafe.SliceData(c)),
				N:    int64(done),
				XA:   int64(n),
				Out:  unsafe.Pointer(&sums[0]),
				Out2: unsafe.Pointer(&sums[done]),
			})
			for i := range done {
				best[i], dist[i] = 0, sums[i]
			}
			if done < nVec {
				vec1Nearest(t, m, n, vecs[done*n:], best[done:], dist[done:])
			}
			return
		}
	}
	// The kernel visits only the blocks holding the m rows.
	nBlk4 := (m + 7) / 8
	k := vec1Best4Kernel()
	if k == nil || nBlk == 0 {
		buf := make([]float32, 8*nBlk)
		for i := range nVec {
			vec1T8FGo(jit.Vec1L2F, t, nBlk, n, vecs[i*n:i*n+n], buf)
			b, d := 0, float32(math.Inf(1))
			for j, x := range buf[:m] {
				if x < d {
					b, d = j, x
				}
			}
			best[i], dist[i] = b, d
		}
		return
	}
	// Groups of four vectors per call, as many as keep a call near a
	// million row components: a kernel cannot be preempted while it runs.
	per := max(1, (1<<20)/(8*nBlk4*n))
	out := make([]uint32, 64*min(per, (nVec+3)/4))
	var tail []float32
	for i := 0; i < nVec; {
		g := min(per, (nVec-i)/4)
		v := vecs[i*n:]
		if g == 0 {
			// The last one to three vectors, the last repeated to fill a group.
			tail = make([]float32, 4*n)
			for vv := range 4 {
				copy(tail[vv*n:vv*n+n], vecs[min(i+vv, nVec-1)*n:])
			}
			g, v = 1, tail
		}
		k.Call(&jit.Args{
			A:   unsafe.Pointer(unsafe.SliceData(t)),
			N:   int64(nBlk4),
			XA:  int64(n),
			V:   unsafe.Pointer(unsafe.SliceData(v)),
			XC:  int64(g),
			C:   unsafe.Pointer(&vec1Best4Table),
			Out: unsafe.Pointer(unsafe.SliceData(out)),
		})
		for gi := range g {
			o := out[64*gi:]
			for vv := range min(4, nVec-i-4*gi) {
				b, d := 0, float32(math.Inf(1))
				for l := range 8 {
					x, r := math.Float32frombits(o[8*vv+l]), int(o[32+8*vv+l])
					if x < d || (x == d && r < b) {
						b, d = r, x
					}
				}
				best[i+4*gi+vv], dist[i+4*gi+vv] = b, d
			}
		}
		i += 4 * g
	}
}

// vec1Best4Table is EmitVec1Best4's constants: the lanes' row numbers, the
// step between blocks, and +Inf.
var vec1Best4Table = func() (t [24]uint32) {
	for i := range 8 {
		t[i] = uint32(i)
		t[8+i] = 8
		t[16+i] = math.Float32bits(float32(math.Inf(1)))
	}
	return
}()

// vec1BestT8x4 is vec1BestT8 for four vectors at once (vecs, 4n floats),
// answered from estimates: jit.EmitVec1T8x4's float32 sums find each
// vector's nearest row approximately, and only the rows whose estimate
// could hide the exact answer -- those within the estimates' error bound of
// the smallest -- have their exact distance computed from rows (row-major).
// The answers are vec1BestT8's, to the bit; where the bound cannot be relied
// on (a non-finite or enormous estimate) a vector is searched exactly.
func vec1BestT8x4(t, rows []float32, m, n int, vecs []float32, round bool, best *[4]int, est []float32) []float32 {
	nBlk := vec1T8Blocks(t, n)
	k := vec1T8x4Kernel()
	if k == nil || nBlk == 0 || len(vecs) < 4*n {
		for vv := range 4 {
			best[vv], _, _ = vec1BestT8(t, m, n, vecs[vv*n:vv*n+n], round, nil)
		}
		return est
	}
	if cap(est) < 32*nBlk {
		est = make([]float32, 32*nBlk)
	}
	est = est[:32*nBlk]
	k.Call(&jit.Args{
		A:   unsafe.Pointer(unsafe.SliceData(t)),
		N:   int64(nBlk),
		XA:  int64(n),
		V:   unsafe.Pointer(unsafe.SliceData(vecs)),
		XC:  int64(4 * n),
		Out: unsafe.Pointer(unsafe.SliceData(est)),
	})
	// An estimate is a sum of n nonnegative terms, each operation rounded to
	// float32: within gamma*d (+ A for underflow) of the true sum d. The
	// exact key (float64, maybe rounded to float32) is within eps*d of it.
	gamma := float64(n+2) * 0x1p-24 / (1 - float64(n+2)*0x1p-24)
	delta := gamma + 0x1p-23
	under := float64(2*n+2) * 0x1p-149
	for vv := range 4 {
		q := vecs[vv*n : vv*n+n]
		lo := float32(math.Inf(1))
		ok := true
		for r := range m {
			x := est[r/8*32+vv*8+r%8]
			if x != x {
				ok = false
				break
			}
			lo = min(lo, x)
		}
		bound := (float64(lo)+under)*(1+delta)*(1+delta)/((1-delta)*(1-delta)) + under
		if !ok || !(bound < 1e37) {
			best[vv], _, _ = vec1BestT8(t, m, n, q, round, nil)
			continue
		}
		b, bd := 0, math.Inf(1)
		for r := range m {
			if float64(est[r/8*32+vv*8+r%8]) > bound {
				continue
			}
			d := vec1L2(q, rows[r*n:r*n+n])
			if round {
				d = float64(float32(d))
			}
			if d < bd {
				b, bd = r, d
			}
		}
		best[vv] = b
	}
	return est
}

// encodePQN4 is encodePQN for four vectors: jit.EmitVec1PQCand's estimates
// name the rows that could be each sub-vector's nearest (vec1BestT8x4's
// bound), and only those have their exact distance computed. false when
// there is no kernel, the caller then encoding one vector at a time.
func (m *vec1Model) encodePQN4(a [4][]float32, code [4][]byte, sc *vec1Buf) bool {
	k := vec1PQCandKernel()
	ne, nb := m.nCodeElem, m.nCodebook()
	if k == nil || nb == 0 {
		return false
	}
	t := m.t8()
	w := nb * ne
	if cap(sc.f) < 4*w+16+1024 {
		sc.f = make([]float32, 4*w+16+1024)
	}
	vecs := sc.f[:4*w]
	for i := range 4 {
		copy(vecs[i*w:i*w+w], a[i][:w])
	}
	gamma := float64(ne+2) * 0x1p-24 / (1 - float64(ne+2)*0x1p-24)
	delta := gamma + 0x1p-23
	under := float64(2*ne+2) * 0x1p-149
	// The kernel's bound is (lo + 2A) * F in float32: twice the underflow
	// term covers the one added after the factor, and F is widened for the
	// float32 rounding of the two operations.
	f := (1 + delta) * (1 + delta) / ((1 - delta) * (1 - delta)) * (1 + 0x1p-20)
	scratch := sc.f[4*w : 4*w+16+1024]
	for i := range 8 {
		scratch[i] = math.Nextafter32(float32(2*under), float32(math.Inf(1)))
		scratch[8+i] = math.Nextafter32(float32(f), float32(math.Inf(1)))
	}
	if cap(sc.cand) < 144*nb {
		sc.cand = make([]byte, 144*nb)
	}
	out := sc.cand[:144*nb]
	k.Call(&jit.Args{
		A:   unsafe.Pointer(unsafe.SliceData(t.books)),
		N:   int64(nb),
		XA:  int64(ne),
		V:   unsafe.Pointer(unsafe.SliceData(vecs)),
		XC:  int64(4 * w),
		C:   unsafe.Pointer(unsafe.SliceData(scratch)),
		Out: unsafe.Pointer(unsafe.SliceData(out)),
	})
	for b := range nb {
		rec := out[144*b : 144*b+144]
		book := m.book[b*ne*vec1CodebookSize : (b+1)*ne*vec1CodebookSize]
		for i := range 4 {
			sub := vecs[i*w+b*ne : i*w+b*ne+ne]
			lo := math.Float32frombits(vec1Native.Uint32(rec[128+4*i:]))
			if bound := (float64(lo) + 2*under) * f; !(bound < 1e37) {
				best, _, _ := vec1BestT8(t.book[b], vec1CodebookSize, ne, sub, true, nil)
				code[i][b] = byte(best)
				continue
			}
			best, bd := 0, math.Inf(1)
			for j, bits := range rec[32*i : 32*i+32] {
				for ; bits != 0; bits &= bits - 1 {
					r := 8*j + mathbits.TrailingZeros8(bits)
					d := float64(float32(vec1L2(sub, book[r*ne:r*ne+ne])))
					if d < bd {
						best, bd = r, d
					}
				}
			}
			code[i][b] = byte(best)
		}
	}
	return true
}

var vec1PQCandCode = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1PQCand) })

func vec1PQCandKernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1PQCandCode()
}

var vec1T8x4Code = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1T8x4) })

func vec1T8x4Kernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1T8x4Code()
}

// vec1BestDTable is EmitVec1BestD's constants: the lanes' rows, the step
// between blocks, and +Inf, as float64.
var vec1BestDTable = func() (t [16]float64) {
	for i := range 8 {
		t[i] = float64(i)
	}
	for i := range 4 {
		t[8+i] = 8
		t[12+i] = math.Inf(1)
	}
	return
}()

var vec1BestDCode [2]struct {
	once sync.Once
	code *jit.Code
}

// vec1BestDKernel is the compiled vec1PqBestMatch search, nil without one.
func vec1BestDKernel(round bool) *jit.Code {
	if !jitEnabled {
		return nil
	}
	i := 0
	if round {
		i = 1
	}
	c := &vec1BestDCode[i]
	c.once.Do(func() { c.code = jitMapOnce(func() ([]byte, error) { return jit.EmitVec1BestD(round) }) })
	return c.code
}

var vec1Best4Code = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1Best4) })

func vec1Best4Kernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1Best4Code()
}

var vec1Kernels [3]sync.Once
var vec1KernelCode [3]*jit.Code

// vec1Kernel is the compiled T8 kernel for op, nil without one.
func vec1Kernel(op jit.Vec1Op) *jit.Code {
	if !jitEnabled {
		return nil
	}
	vec1Kernels[op].Do(func() {
		vec1KernelCode[op] = jitMapOnce(func() ([]byte, error) { return jit.EmitVec1T8(op) })
	})
	return vec1KernelCode[op]
}

var vec1ScanCode = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1Scan16) })

// vec1ScanKernel is the compiled PQ scan kernel, nil without one.
func vec1ScanKernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1ScanCode()
}

// vec1Buf is a search's scratch, pooled: a rebuild quantizes every vector.
type vec1Buf struct {
	d    []float64
	f    []float32
	est  []float32
	cand []byte
}

var vec1Bufs = sync.Pool{New: func() any { return new(vec1Buf) }}

// vec1T8Model is a model's centroids, codebooks and rotation in T8 layout,
// built once on first use: a model is never changed after it is decoded.
type vec1T8Model struct {
	once  sync.Once
	cent  []float32
	books []float32   // every codebook, back to back
	book  [][]float32 // each codebook's part of books
	rot   []float32
}

func (m *vec1Model) t8() *vec1T8Model {
	t := &m.t8m
	t.once.Do(func() {
		n := m.nElem()
		if m.nBucket() > 0 {
			t.cent = vec1T8(m.cent, m.nBucket(), n)
		}
		if m.nCodebook() > 0 && !m.bitQuant() {
			ne := m.nCodeElem
			t.books = vec1T8(m.book, m.nCodebook()*vec1CodebookSize, ne)
			t.book = make([][]float32, m.nCodebook())
			for b := range t.book {
				t.book[b] = t.books[b*ne*vec1CodebookSize : (b+1)*ne*vec1CodebookSize]
			}
		}
		if m.rot != nil {
			t.rot = vec1T8(m.rot, n, n)
		}
	})
	return t
}

// vec1BestT8 is vec1PqBestMatch over the first m rows of a T8 layout: the
// first row nearest a by the float64 distance, or with round by that
// distance rounded to float32 (the scalar vec1BestMatchN). buf is scratch.
func vec1BestT8(t []float32, m, n int, a []float32, round bool, buf []float64) (int, float64, []float64) {
	if nBlk := vec1T8Blocks(t, n); nBlk > 0 && len(a) >= n {
		if k := vec1BestDKernel(round); k != nil {
			var lanes, rows [8]float64
			k.Call(&jit.Args{
				A:    unsafe.Pointer(unsafe.SliceData(t)),
				XC:   int64(32 * n),
				V:    unsafe.Pointer(unsafe.SliceData(a)),
				N:    int64(nBlk / 2),
				XA:   int64(n),
				C:    unsafe.Pointer(&vec1BestDTable),
				Out:  unsafe.Pointer(&lanes),
				Out2: unsafe.Pointer(&rows),
			})
			// Each lane holds its first nearest row; the first of those
			// nearest is the search's answer. Rows past m are +Inf and
			// never win a lane.
			best, bestD := 0, math.Inf(1)
			for l, d := range lanes {
				if r := int(rows[l]); d < bestD || (d == bestD && r < best) {
					best, bestD = r, d
				}
			}
			return best, bestD, buf
		}
	}
	if need := 8 * vec1T8Blocks(t, n); cap(buf) < need {
		buf = make([]float64, need)
	} else {
		buf = buf[:need]
	}
	vec1T8L2D(t, n, a, buf)
	best, bestD := 0, math.Inf(1)
	for i, d := range buf[:m] {
		if round {
			d = float64(float32(d))
		}
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best, bestD, buf
}

// vec1BestT8F is vec1EncodeVectorPQ's search over a T8 codebook: the first
// code nearest a by the float32 sum (vec1ModelTDist), or -1 when none is
// below +Inf (vec1 then leaves the code byte as it was).
func vec1BestT8F(t []float32, m, n int, a []float32, buf []float32) (int, []float32) {
	if need := 8 * vec1T8Blocks(t, n); cap(buf) < need {
		buf = make([]float32, need)
	} else {
		buf = buf[:need]
	}
	vec1T8F(jit.Vec1L2F, t, n, a, buf)
	best, bestD := -1, float32(math.Inf(1))
	for i, d := range buf[:m] {
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best, buf
}
