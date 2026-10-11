package engine

// The arithmetic vec1's searches and rebuilds spend their time in, laid out
// so a SIMD lane can hold a row: centroids, PQ codebooks and the OPQ rotation
// are kept "T8" -- in blocks of eight rows, component by component -- so one
// register holds the same component of eight rows, and each row's sum is
// accumulated in component order with one rounding per operation, as vec1's
// portable C does it. The answers are its answers, to the bit; only the rows
// done at once change. The JIT's kernels (internal/jit's vec1 emitters) take
// a call when the machine has one, these loops otherwise.

import (
	"math"
	mathbits "math/bits"
	"sync"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// vec1T8 lays m rows of n float32s out in blocks of eight: block b holds
// component d of rows 8b..8b+7 at [b*8*n + 8*d + lane]. The blocks are a
// multiple of four, the kernels' unit; rows past m are +Inf, so no search
// can choose one.
func vec1T8(rows []float32, m, n int) []float32 {
	nBlk := (m + 31) / 32 * 4
	out := make([]float32, nBlk*8*n)
	for i := range out {
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
			N:   int64(nBlk / 4),
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
// in vecs), the first of the m rows (row-major in rows, laid out in tn by
// vec1MakeT8N) nearest it by the float32 estimate of jit.EmitVec1Cand, and
// that distance; norms, when not nil, is vec1Norms(vecs). vec1's training is
// randomized and its builds differ in this arithmetic, so a trained model is
// not compared to the bit.
func vec1Nearest(tn *vec1T8N, rows []float32, m, n int, vecs, norms []float32, best []int, dist []float32) {
	nVec := len(vecs) / n
	if m == 1 && n%8 == 0 && nVec >= 8 {
		// One row (k-means++'s rounds): the vectors are the rows, back to
		// back, and the one row the query, eight vectors to a lane-set.
		if ks := jitVecKernelOf(jitVecKey{l2: true, strided: true}); ks != nil {
			done := nVec &^ 7
			sums := make([]float32, 2*done)
			ks.Call(&jit.Args{
				A:    unsafe.Pointer(unsafe.SliceData(vecs)),
				XC:   int64(4 * n),
				V:    unsafe.Pointer(unsafe.SliceData(rows)),
				N:    int64(done),
				XA:   int64(n),
				Out:  unsafe.Pointer(&sums[0]),
				Out2: unsafe.Pointer(&sums[done]),
			})
			for i := range done {
				best[i], dist[i] = 0, sums[i]
			}
			if done < nVec {
				vec1Nearest(tn, rows, m, n, vecs[done*n:], vec1Tail(norms, done), best[done:], dist[done:])
			}
			return
		}
	}
	if !vec1BestMany(tn, rows, m, n, vecs, norms, false, false, &vec1Buf{}, best, dist) {
		for i := range nVec {
			best[i], dist[i] = vec1NearestGo(rows, m, n, vecs[i*n:i*n+n])
		}
	}
}

// vec1Tail is norms[i:], or nil for none.
func vec1Tail(norms []float32, i int) []float32 {
	if norms == nil {
		return nil
	}
	return norms[i:]
}

// vec1Norms is each of the vectors' squared norm, in float32: what
// vec1Nearest adds to an estimate to give a distance, computed once for a
// k-means and not once a round.
func vec1Norms(vecs []float32, n int) []float32 {
	out := make([]float32, len(vecs)/n)
	for i := range out {
		var sq float32
		for _, x := range vecs[i*n : i*n+n] {
			sq += x * x
		}
		out[i] = sq
	}
	return out
}

// vec1NearestGo is vec1Nearest for one vector, in Go.
func vec1NearestGo(rows []float32, m, n int, q []float32) (int, float32) {
	b, d := 0, float32(math.Inf(1))
	for j := range m {
		var s float32
		for k, x := range rows[j*n : j*n+n] {
			e := x - q[k]
			s += e * e
		}
		if s < d {
			b, d = j, s
		}
	}
	return b, d
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
	v    []float32
	pack []float32
	cand []byte
	best []int
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
	// The centroids and codebooks as a rebuild's estimates read them.
	centN, booksN *vec1T8N
}

func (m *vec1Model) t8() *vec1T8Model {
	t := &m.t8m
	t.once.Do(func() {
		n := m.nElem()
		if m.nBucket() > 0 {
			t.cent = vec1T8(m.cent, m.nBucket(), n)
			t.centN = vec1MakeT8N(m.cent, 1, m.nBucket(), n)
		}
		if m.nCodebook() > 0 && !m.bitQuant() {
			ne := m.nCodeElem
			t.books = vec1T8(m.book, m.nCodebook()*vec1CodebookSize, ne)
			t.booksN = vec1MakeT8N(m.book, m.nCodebook(), vec1CodebookSize, ne)
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

// vec1T8N is sets of rows laid out for jit.EmitVec1Cand: each set b blocks
// of eight rows, a block's components (T8) followed by its rows' squared
// norms. Rows past a set's m have zero components and an infinite norm.
type vec1T8N struct {
	t    []float32
	b    int
	maxc []float64 // each set's largest row norm
}

func vec1MakeT8N(rows []float32, sets, m, n int) *vec1T8N {
	b := (m + 15) / 16 * 2 // the kernel takes blocks in pairs
	tn := &vec1T8N{t: make([]float32, sets*b*8*(n+1)), b: b, maxc: make([]float64, sets)}
	for s := range sets {
		blk := tn.t[s*b*8*(n+1):]
		for r := range 8 * b {
			base := r / 8 * 8 * (n + 1)
			if r >= m {
				blk[base+8*n+r%8] = float32(math.Inf(1))
				continue
			}
			row := rows[(s*m+r)*n : (s*m+r+1)*n]
			var sq float64
			for d, x := range row {
				blk[base+8*d+r%8] = x
				sq += float64(x) * float64(x)
			}
			blk[base+8*n+r%8] = float32(sq)
			// A row with a NaN is never anyone's nearest, so it does not
			// bound the others; an infinite one makes the bound infinite,
			// and the set is then searched exactly.
			if c := math.Sqrt(sq); c > tn.maxc[s] {
				tn.maxc[s] = c
			}
		}
	}
	return tn
}

// vec1Best4 is vec1BestT8 for four vectors at once over sets of rows: set s
// is rows[s*m*comp:] (m rows of comp, row-major) laid out in tn, and vector
// i's part for it the comp floats at vecs[i*stride+s*comp:]. out[4*s+i] is
// the answer, vec1BestT8's to the bit: jit.EmitVec1Cand estimates every
// distance in float32 and names the rows whose estimate is within the
// estimates' error bound of the smallest, and only those have their exact
// distance computed. Where the bound cannot be relied on (a non-finite
// estimate or norm) the vector is searched exactly. false when there is no
// kernel.
func vec1Best4(tn *vec1T8N, rows []float32, sets, m, comp int, vecs []float32, stride int, round bool, sc *vec1Buf, out []int) bool {
	k := vec1CandKernel()
	b := tn.b
	if k == nil || sets == 0 || b == 0 || len(tn.t) < sets*b*8*(comp+1) || len(vecs) < 3*stride+sets*comp {
		return false
	}
	// The estimate of row c's distance from v, less ||v||^2, is ||c||^2 -
	// 2 c.v: the norm rounded to float32 and the dot product summed in
	// float32 (comp products, each operation rounded), so it is within
	// (gamma + 2^-22) * G of the true value, G = (||v|| + max ||c||)^2. The
	// exact key (the float64 sum, maybe rounded to float32) is within
	// 2^-22 * d of the true distance d <= G. So a row can be nearest only if
	// its estimate is within (2*gamma + 2^-20) * G of the smallest, lo, and
	// the kernel's float32 lo + A rounds by at most 2^-24 * (G + A) more:
	// A = (2*gamma + 2^-19) * G (+ the underflow terms) covers it all, per
	// vector, over the sets' largest G -- provided nothing overflows float32,
	// which G < 1e37 ensures (every norm, product and sum is at most G).
	gamma := float64(comp+2) * 0x1p-24 / (1 - float64(comp+2)*0x1p-24)
	under := float64(8*comp+8) * 0x1p-149
	var maxc float64
	for _, c := range tn.maxc[:sets] {
		maxc = max(maxc, c)
	}
	if need := 64 + 32*b; cap(sc.f) < need {
		sc.f = make([]float32, need)
	}
	scratch := sc.f[:64+32*b]
	scratch[0], scratch[1] = math.Float32frombits(uint32(b)), 0 // B, an int64
	var slack [4]float64
	for i := range 4 {
		var vmax float64
		for s := range sets {
			var sq float64
			for _, x := range vecs[i*stride+s*comp : i*stride+s*comp+comp] {
				sq += float64(x) * float64(x)
			}
			// A part with a NaN has no finite estimate: its lo is +Inf
			// and it is searched exactly.
			if v := math.Sqrt(sq); v > vmax {
				vmax = v
			}
		}
		g := (vmax + maxc) * (vmax + maxc)
		slack[i] = (2*gamma+0x1p-19)*g + under
		if !(g < 1e37) {
			slack[i] = math.Inf(1) // searched exactly
		}
		a := math.Nextafter32(float32(slack[i]), float32(math.Inf(1)))
		for l := range 8 {
			scratch[8+l] = 1 // F
			scratch[16+l] = -2
			scratch[32+8*i+l] = a
		}
	}
	rec := 16 + 4*b
	if cap(sc.cand) < sets*rec {
		sc.cand = make([]byte, sets*rec)
	}
	res := sc.cand[:sets*rec]
	k.Call(&jit.Args{
		A:   unsafe.Pointer(unsafe.SliceData(tn.t)),
		N:   int64(sets),
		XA:  int64(comp),
		V:   unsafe.Pointer(unsafe.SliceData(vecs)),
		XC:  int64(4 * stride),
		C:   unsafe.Pointer(unsafe.SliceData(scratch)),
		Out: unsafe.Pointer(unsafe.SliceData(res)),
	})
	for s := range sets {
		r := res[s*rec : (s+1)*rec]
		set := rows[s*m*comp : (s+1)*m*comp]
		for i := range 4 {
			q := vecs[i*stride+s*comp : i*stride+s*comp+comp]
			lo := float64(math.Float32frombits(vec1Native.Uint32(r[4*i:])))
			if bound := lo + slack[i]; !(bound < 1e37 && bound > -1e37) {
				out[4*s+i] = vec1BestRows(set, m, comp, q, round)
				continue
			}
			best, bd := 0, math.Inf(1)
			for j, bits := range r[16+i*b : 16+(i+1)*b] {
				for ; bits != 0; bits &= bits - 1 {
					row := 8*j + mathbits.TrailingZeros8(bits)
					if row >= m {
						break
					}
					d := vec1L2(q, set[row*comp:row*comp+comp])
					if round {
						d = float64(float32(d))
					}
					if d < bd {
						best, bd = row, d
					}
				}
			}
			out[4*s+i] = best
		}
	}
	return true
}

// vec1CandGroups is how many groups of four vectors one call of
// jit.EmitVec1CandMany takes: enough that a pair of blocks is read once for
// many vectors, few enough that a call stays short (a kernel cannot be
// preempted) and its estimates stay in cache.
const vec1CandGroups = 16

// vec1BestMany is vec1Best4 over one set of rows (m of them, row-major in
// rows, laid out in tn) for any number of vectors (row-major in vecs, n
// each): out[i] is vector i's answer, vec1BestT8's to the bit, or with exact
// false the first row with the smallest estimate (training's search, and
// dist[i] that estimate as a squared distance). The estimates come from
// jit.EmitVec1CandMany, vec1CandGroups groups of four at a time. false when
// there is no kernel.
func vec1BestMany(tn *vec1T8N, rows []float32, m, n int, vecs, norms []float32, exact, round bool, sc *vec1Buf, out []int, dist []float32) bool {
	k := vec1CandManyKernel()
	b := tn.b
	nv := len(vecs) / n
	if k == nil || b == 0 || len(tn.t) < b*8*(n+1) || nv == 0 {
		return false
	}
	// The bound is vec1Best4's; see there.
	gamma := float64(n+2) * 0x1p-24 / (1 - float64(n+2)*0x1p-24)
	under := float64(8*n+8) * 0x1p-149
	maxc := tn.maxc[0]
	g := min(vec1CandGroups, (nv+3)/4)
	if need := 32 + 64*g + 32*b*g; cap(sc.f) < need {
		sc.f = make([]float32, need)
	}
	scratch := sc.f[:32+64*g+32*b*g]
	scratch[0], scratch[1] = math.Float32frombits(uint32(b)), 0 // B, an int64
	for l := range 8 {
		scratch[8+l], scratch[16+l] = 1, -2 // F, and -2
	}
	rec := 16 + 4*b
	if cap(sc.cand) < g*rec {
		sc.cand = make([]byte, g*rec)
	}
	res := sc.cand[:g*rec]
	slack := make([]float64, 4*g)
	if cap(sc.v) < 4*g*n {
		sc.v = make([]float32, 4*g*n)
	}
	for i := 0; i < nv; i += 4 * g {
		cnt := min(4*g, nv-i)
		ng := (cnt + 3) / 4
		v := vecs[i*n:]
		if cnt%4 != 0 {
			// The last group filled out with copies of its last vector.
			v = sc.v[:4*ng*n]
			copy(v, vecs[i*n:(i+cnt)*n])
			for j := cnt; j < 4*ng; j++ {
				copy(v[j*n:j*n+n], vecs[(i+cnt-1)*n:(i+cnt)*n])
			}
		}
		for j := range 4 * ng {
			slack[j] = 0
			if exact {
				var sq float64
				for _, x := range v[j*n : j*n+n] {
					sq += float64(x) * float64(x)
				}
				gg := (math.Sqrt(sq) + maxc) * (math.Sqrt(sq) + maxc)
				slack[j] = (2*gamma+0x1p-19)*gg + under
				if !(gg < 1e37) {
					slack[j] = math.Inf(1)
				}
			}
			a := math.Nextafter32(float32(slack[j]), float32(math.Inf(1)))
			if !exact {
				a = 0
			}
			for l := range 8 {
				scratch[32+32*ng+8*j+l] = a
			}
		}
		k.Call(&jit.Args{
			A:   unsafe.Pointer(unsafe.SliceData(tn.t)),
			N:   int64(ng),
			XA:  int64(n),
			V:   unsafe.Pointer(unsafe.SliceData(v)),
			XC:  int64(4 * n),
			C:   unsafe.Pointer(unsafe.SliceData(scratch)),
			Out: unsafe.Pointer(unsafe.SliceData(res)),
		})
		for j := range cnt {
			r := res[j/4*rec : (j/4+1)*rec]
			vv := j % 4
			q := v[j*n : j*n+n]
			lo := float64(math.Float32frombits(vec1Native.Uint32(r[4*vv:])))
			if bound := lo + slack[j]; !(bound < 1e37 && bound > -1e37) {
				if exact {
					out[i+j] = vec1BestRows(rows, m, n, q, round)
				} else {
					out[i+j], dist[i+j] = vec1NearestGo(rows, m, n, q)
				}
				continue
			}
			best, bd := -1, math.Inf(1)
			for jj, bits := range r[16+vv*b : 16+(vv+1)*b] {
				for ; bits != 0; bits &= bits - 1 {
					row := 8*jj + mathbits.TrailingZeros8(bits)
					if row >= m {
						break
					}
					if !exact {
						best = row
						break
					}
					d := vec1L2(q, rows[row*n:row*n+n])
					if round {
						d = float64(float32(d))
					}
					if d < bd {
						best, bd = row, d
					}
				}
				if !exact && best >= 0 {
					break
				}
			}
			if !exact {
				if best < 0 {
					out[i+j], dist[i+j] = vec1NearestGo(rows, m, n, q)
					continue
				}
				var sq float32
				if norms != nil {
					sq = norms[i+j]
				} else {
					for _, x := range q {
						sq += x * x
					}
				}
				dist[i+j] = max(0, float32(lo)+sq)
			}
			out[i+j] = max(best, 0)
		}
	}
	return true
}

var vec1CandManyCode = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1CandMany) })

func vec1CandManyKernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1CandManyCode()
}

// vec1BestRows is vec1PqBestMatch over row-major rows, or with round the
// scalar vec1BestMatchN's choice.
func vec1BestRows(rows []float32, m, n int, q []float32, round bool) int {
	best, bd := 0, math.Inf(1)
	for r := range m {
		d := vec1L2(q, rows[r*n:r*n+n])
		if round {
			d = float64(float32(d))
		}
		if d < bd {
			best, bd = r, d
		}
	}
	return best
}

var vec1CandCode = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1Cand) })

func vec1CandKernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1CandCode()
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

var vec1WHTCode = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitVec1WHT) })

// vec1WHTKernel is the compiled Walsh-Hadamard transform, nil without one.
func vec1WHTKernel() *jit.Code {
	if !jitEnabled {
		return nil
	}
	return vec1WHTCode()
}
