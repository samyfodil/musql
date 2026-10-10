package engine

import (
	"math"
	"slices"
	"sync"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// Exact top-k that reads a quarter of the bytes.
//
// A large vector search is memory-bound: the exact kernel reads every row's
// float32 components, and one core can only read so fast. So each segment keeps,
// per vector column, an int8 copy of its rows (a sidecar), and a search first
// bounds every row's distance from the copy, then computes exactly only the
// rows that can still be among the closest k.
//
// The answer is unchanged, row for row, because the bounds are rigorous: for
// every row, [lo, hi] contains the float32 distance libSQL's loop computes --
// not merely the real-valued distance. They account for the quantization
// error of both row and query, the float32 rounding of every product and sum
// in that loop (Higham's gamma bound), and the float64 arithmetic of the
// bounds themselves, and they map those through the distance's own float32
// formula by its monotonicity. With T the k-th smallest hi, a row with lo > T
// has a distance strictly greater than the k-th answer, so it can neither be
// one of the k nor tie with one. Any row whose distance could be NULL or
// non-finite (a zero vector, a non-finite component, an overflow) gets no
// bound and is always computed.
//
// The sidecar is derived from the segment's own rows, in memory, the first time
// a search needs it. A segment never changes, so it can never disagree with
// them -- which is why it is not stored in the file, where a damaged copy
// would turn into wrong rows.

// segVecSide is one vector column's sidecar in one segment.
type segVecSide struct {
	dims  int
	norm  []float32 // each row's sum of squares, summed exactly as the distance sums it
	scale []float32 // component i is about codes[i] * scale
	errb  []float32 // max |component - code*scale|, rounded up; +Inf for no bound
	c1    []int32   // sum of |codes|
	codes []int8    // row-major
}

// vecSideMinRows is how many rows per kept one a search must have before the
// bounding pass is worth its own cost.
const vecSideMinRows = 64

// vecSide returns the sidecar of column col. It is built on a column's
// SECOND search in a segment, not its first: building costs a few exact
// scans, which a search that runs once should not pay.
func (s *segment) vecSide(col, dims int, build func() (*segVecSide, bool)) (*segVecSide, bool) {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	if side, ok := s.vecSides[col]; ok {
		return side, side != nil && side.dims == dims
	}
	if s.vecSearches == nil {
		s.vecSearches = map[int]int{}
	}
	if s.vecSearches[col]++; s.vecSearches[col] < 2 {
		return nil, false
	}
	side, ok := build()
	if !ok {
		side = nil
	}
	if s.vecSides == nil {
		s.vecSides = map[int]*segVecSide{}
	}
	s.vecSides[col] = side
	return side, ok
}

// buildVecSide quantizes each row (a float32 vector of dims components at
// heap[offs[r]:]) symmetrically onto -127..127, in parallel over runs of
// rows. Each row's sum of squares comes from the cosine kernel run against a
// zero query, which sums exactly as the distance does.
func buildVecSide(heap []byte, offs []int, dims int) (*segVecSide, bool) {
	n := len(offs)
	side := &segVecSide{
		dims: dims, norm: make([]float32, n), scale: make([]float32, n),
		errb: make([]float32, n), c1: make([]int32, n), codes: make([]int8, n*dims),
	}
	zero := make([]float32, dims)
	step := segVectorChunkRows(dims)
	k := jitVecKernel(false)
	return side, segEach((n+step-1)/step, func(j int) bool {
		lo, hi := j*step, min((j+1)*step, n)
		rows, ok := f32Rows(heap, offs[lo:hi], dims)
		if !ok {
			return false
		}
		sums, done := vecSumsJIT(k, false, heap, offs[lo:hi], zero)
		copy(side.norm[lo:], sums[done:2*done])
		for r := lo + done; r < hi; r++ {
			side.norm[r] = vecSelfDot(rows[r-lo])
		}
		q := quantizeRowsJIT(heap, offs[lo:hi], dims, side, lo)
		for r := lo + q; r < hi; r++ {
			side.scale[r], side.errb[r], side.c1[r] = quantizeRow(rows[r-lo], side.codes[r*dims:(r+1)*dims])
		}
		return true
	})
}

// quantizeRowsJIT quantizes the leading rows that sit back to back with the
// JIT's two kernels -- each row's max |component| as an integer max over the
// float bits, then the codes -- to the same rules as quantizeRow, and reports
// how many rows it did.
func quantizeRowsJIT(heap []byte, offs []int, dims int, side *segVecSide, first int) int {
	n := vecStridedRows(heap, offs, dims)
	kMax, kQuant := jitMaxAbs(), jitQuantize()
	if n == 0 || kMax == nil || kQuant == nil {
		return 0
	}
	bits := make([]uint32, n)
	args := jit.Args{
		A:   (*int64)(unsafe.Pointer(&heap[offs[0]])),
		XC:  int64(4 * dims),
		N:   int64(n),
		XA:  int64(dims),
		Out: (*int64)(unsafe.Pointer(&bits[0])),
	}
	kMax.Call(&args)
	inv := make([]float32, n)
	for i, b := range bits {
		r := first + i
		mx := math.Float32frombits(b)
		switch {
		case b >= 0x7f800000: // NaN or Inf: no bound, and the codes do not matter
			side.scale[r], side.errb[r] = 0, float32(math.Inf(1))
		case mx/127 < 0x1p-126: // zero, or a scale too small to trust: all code 0
			side.scale[r], side.errb[r] = 0, mx
		default:
			scale := mx / 127
			side.scale[r], side.errb[r] = scale, roundUp32(float64(scale)*(0.5+0x1p-14))
			inv[i] = 1 / scale
		}
	}
	args.V = (*int64)(unsafe.Pointer(&inv[0]))
	args.Out = (*int64)(unsafe.Pointer(&side.codes[first*dims]))
	args.Out2 = (*int64)(unsafe.Pointer(&side.c1[first]))
	kQuant.Call(&args)
	for i := range n {
		if inv[i] == 0 { // the special rows: code 0 (or, non-finite, unused)
			r := first + i
			clear(side.codes[r*dims : (r+1)*dims])
			side.c1[r] = 0
		}
	}
	return n
}

var (
	jitMaxAbs   = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitMaxAbsBits) })
	jitQuantize = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitQuantize) })
)

// jitMapOnce emits and maps a kernel, nil where the JIT is off or cannot.
func jitMapOnce(emit func() ([]byte, error)) *jit.Code {
	if !jitEnabled || !jit.HasVector() {
		return nil
	}
	code, err := emit()
	if err != nil {
		return nil
	}
	k, err := jit.Map(code)
	if err != nil {
		return nil
	}
	return k
}

// quantizeRow writes x's codes, c = round(x/s) with s = max|x|/127, and
// returns s, the error bound and the sum of |codes|. Computing x/s as x*(1/s)
// in float32 is off by at most 127*2^-22 in units of s, so every component is
// within s*(0.5 + 2^-14) of its code's value -- the bound, by construction,
// with no per-component bookkeeping. A row with a non-finite component gets
// no bound (+Inf); one too small to scale is all code 0, its bound its max.
func quantizeRow(x []float32, code []int8) (scale, errb float32, c1 int32) {
	var mx float32
	for _, v := range x {
		a := float32(math.Abs(float64(v)))
		if !(a <= math.MaxFloat32) { // NaN or Inf
			return 0, float32(math.Inf(1)), 0
		}
		mx = max(mx, a)
	}
	if mx == 0 {
		clear(code)
		return 0, 0, 0
	}
	scale = mx / 127
	if scale < 0x1p-126 {
		// A subnormal scale has lost relative precision, so x/scale could pass
		// 127 and the bound above would not hold: no codes, the max as bound.
		clear(code)
		return 0, mx, 0
	}
	inv := 1 / scale
	for i, v := range x {
		c := int32(math.RoundToEven(float64(v * inv)))
		c = min(max(c, -127), 127)
		code[i] = int8(c)
		c1 += max(c, -c)
	}
	return scale, roundUp32(float64(scale) * (0.5 + 0x1p-14)), c1
}

// roundUp32 is the smallest float32 not below x.
func roundUp32(x float64) float32 {
	f := float32(x)
	if float64(f) < x {
		f = math.Nextafter32(f, float32(math.Inf(1)))
	}
	return f
}

// roundDown32 is the largest float32 not above x.
func roundDown32(x float64) float32 {
	f := float32(x)
	if float64(f) > x {
		f = math.Nextafter32(f, float32(math.Inf(-1)))
	}
	return f
}

// vecQuery is the query, quantized like a row, and what the bounds need of it.
type vecQuery struct {
	l2            bool
	t             []int8
	t32           []int32 // t, widened once for the row loop
	t16           []int16 // and for the JIT kernel
	sigma         float64 // component i is about t[i] * sigma
	fErr          float64 // max |q_i - t_i*sigma|, rounded up
	q1            float64 // sum |q_i|, rounded up
	qn            float32 // the query's sum of squares, as the distance sums it
	n2lo, n2hi    float64 // the real sum of squares lies in [n2lo, n2hi]
	gamma, absErr float64
}

// gammaN is Higham's gamma_n for float32: |fl(sum) - sum| <= gamma_n * sum|terms|
// for n roundings, here with headroom for the products and the final step.
func gammaN(d int) float64 {
	const u = 0x1p-24
	n := float64(d + 4)
	return n * u / (1 - n*u)
}

// newVecQuery prepares q, or reports false when it admits no bounds (a
// non-finite component, or a zero or overflowing norm).
func newVecQuery(q []float32, l2 bool) (*vecQuery, bool) {
	vq := &vecQuery{l2: l2, t: make([]int8, len(q)), qn: vecSelfDot(q), gamma: gammaN(len(q))}
	// Products and sums that underflow lose up to half a subnormal ulp each,
	// an absolute error the relative gamma does not cover.
	vq.absErr = float64(4*len(q)+16) * 0x1p-149
	s, e, _ := quantizeRow(q, vq.t)
	if math.IsInf(float64(e), 0) || vq.qn == 0 || math.IsInf(float64(vq.qn), 0) {
		return nil, false
	}
	vq.sigma, vq.fErr = float64(s), float64(e)
	vq.t32, vq.t16 = make([]int32, len(q)), make([]int16, len(q))
	for i, c := range vq.t {
		vq.t32[i], vq.t16[i] = int32(c), int16(c)
	}
	for _, v := range q {
		vq.q1 += math.Abs(float64(v))
	}
	vq.q1 *= 1 + 0x1p-40
	vq.n2lo = (float64(vq.qn) - vq.absErr) / (1 + vq.gamma)
	vq.n2hi = (float64(vq.qn) + vq.absErr) / (1 - vq.gamma)
	return vq, true
}

// bounds is [lo, hi] around row r's distance, or ok false for no bound.
func (vq *vecQuery) bounds(side *segVecSide, r int) (lo, hi float32, ok bool) {
	d := side.dims
	return vq.boundsDot(side, r, int8Dot(side.codes[r*d:(r+1)*d], vq.t32))
}

// boundsDot is bounds given the row's integer code dot product.
func (vq *vecQuery) boundsDot(side *segVecSide, r int, dot int32) (lo, hi float32, ok bool) {
	e, n1 := float64(side.errb[r]), side.norm[r]
	if math.IsInf(e, 0) || math.IsInf(float64(n1), 0) || n1 != n1 {
		return 0, 0, false
	}
	// The real dot product x.q, from the codes' exact integer dot product:
	// x.q = s*sigma*sum(c t) + sum(xhat_i f_i) + sum(e_i q_i), the last two
	// bounded by fErr*s*sum|c| and e*sum|q|.
	s := float64(side.scale[r])
	a := s * vq.sigma * float64(dot)
	errQ := vq.fErr*s*float64(side.c1[r]) + e*vq.q1
	slack := 0x1p-40*(math.Abs(a)+errQ) + 0x1p-1000
	rlo, rhi := a-errQ-slack, a+errQ+slack

	n1lo := (float64(n1) - vq.absErr) / (1 + vq.gamma)
	n1hi := (float64(n1) + vq.absErr) / (1 - vq.gamma)
	if vq.l2 {
		// ||x-q||^2 = ||x||^2 - 2 x.q + ||q||^2, then the loop's own rounding:
		// every term it sums is non-negative, so the error is relative.
		r2lo := n1lo - 2*rhi + vq.n2lo
		r2hi := n1hi - 2*rlo + vq.n2hi
		r2slack := 0x1p-40 * (n1hi + 2*math.Abs(a) + 2*errQ + vq.n2hi)
		r2lo, r2hi = max(r2lo-r2slack, 0), r2hi+r2slack
		flo := max(r2lo*(1-vq.gamma)-vq.absErr, 0)
		fhi := r2hi*(1+vq.gamma) + vq.absErr
		if fhi > math.MaxFloat32 {
			return widenLo(math.Sqrt(flo)), float32(math.Inf(1)), true
		}
		return widenLo(math.Sqrt(flo)), widenHi(math.Sqrt(fhi)), true
	}
	// Cosine: only the float32 dot product is unknown. The loop's error in it
	// is at most gamma * sum|x_i q_i| <= gamma * ||x|| ||q||.
	denom := math.Sqrt(float64(n1 * vq.qn))
	if denom == 0 || math.IsInf(denom, 0) {
		return 0, 0, false
	}
	b := vq.gamma*math.Sqrt(n1hi)*math.Sqrt(vq.n2hi) + vq.absErr
	dlo, dhi := rlo-b, rhi+b
	if math.Abs(dlo) > math.MaxFloat32 || math.Abs(dhi) > math.MaxFloat32 {
		return 0, 0, false
	}
	// cosine() is non-increasing in the dot product: every step of it is
	// monotone, the division being by a positive denominator.
	return widenLo(1 - dhi/denom), widenHi(1 - dlo/denom), true
}

// widenLo and widenHi turn a float64 evaluation of a distance formula at a
// bound into a float32 bound on what the formula gives in float32. The float32
// result differs from the real value by at most half a float32 ulp (2^-24
// relative) and the float64 steps by about 2^-52 -- ABSOLUTE for the cosine,
// whose 1 - dot/denom cancels near 0, where a relative margin would vanish.
// 2^-20 relative plus 2^-48 absolute covers both with room.
func widenLo(x float64) float32 { return float32(x - math.Abs(x)*0x1p-20 - 0x1p-48) }
func widenHi(x float64) float32 { return float32(x + math.Abs(x)*0x1p-20 + 0x1p-48) }

// vecJob is a run of one segment's rows that one worker takes.
type vecJob struct{ seg, lo, hi int }

// vecCandidates returns, per job, the rows that can still be among the plan's
// k: every row whose bound reaches the k-th best bound on the far side, and
// every row without a bound. It reports false when the bounds prune too
// little to be worth it.
func vecCandidates(plan *segVectorPlan, vq *vecQuery, sides []*segVecSide, skips [][]int, jobs []vecJob) ([][]int, bool) {
	// far is a bound in the plan's direction: hi ascending, -lo descending,
	// so that smaller is better either way. The k-th smallest far over
	// bounded live rows bounds the k-th answer: k rows are at least that
	// good. (NULLs sort first ascending and last descending, which only ever
	// moves the k-th answer further in.)
	type rowBound struct {
		near, far float32 // lo and hi, or -hi and -lo descending
		ok        bool
	}
	bounds := make([][]rowBound, len(jobs))
	bests := make([][]float32, len(jobs)) // each job's k smallest fars
	if !segEach(len(jobs), func(j int) bool {
		w := jobs[j]
		side, skip := sides[w.seg], skips[w.seg]
		k, _ := slices.BinarySearch(skip, w.lo)
		bs := make([]rowBound, w.hi-w.lo)
		best := make(vecMaxHeap, 0, plan.limit)
		dots := vq.dots(side, w.lo, w.hi)
		for r := w.lo; r < w.hi; r++ {
			if k < len(skip) && skip[k] == r {
				k++
				continue
			}
			lo, hi, ok := vq.boundsDot(side, r, dots[r-w.lo])
			if !ok {
				bs[r-w.lo] = rowBound{ok: false, near: float32(math.Inf(-1))}
				continue
			}
			b := rowBound{near: lo, far: hi, ok: true}
			if plan.desc {
				b = rowBound{near: -hi, far: -lo, ok: true}
			}
			bs[r-w.lo] = b
			best.keep(b.far, plan.limit)
		}
		bounds[j], bests[j] = bs, best
		return true
	}) {
		return nil, false
	}
	all := make(vecMaxHeap, 0, plan.limit)
	for _, b := range bests {
		for _, f := range b {
			all.keep(f, plan.limit)
		}
	}
	if len(all) < plan.limit {
		return nil, false
	}
	t := all[0] // the k-th smallest far

	cands := make([][]int, len(jobs))
	total, rows := 0, 0
	for j, bs := range bounds {
		w := jobs[j]
		skip := skips[w.seg]
		k, _ := slices.BinarySearch(skip, w.lo)
		for i, b := range bs {
			r := w.lo + i
			if k < len(skip) && skip[k] == r {
				k++
				continue
			}
			rows++
			if !b.ok || b.near <= t {
				cands[j] = append(cands[j], r)
			}
		}
		total += len(cands[j])
	}
	if total*4 > rows { // pruned less than three quarters: the plain scan is as good
		return nil, false
	}
	return cands, true
}

// vecMaxHeap keeps the smallest values offered to it, its root the largest kept.
type vecMaxHeap []float32

func (h *vecMaxHeap) keep(v float32, k int) {
	a := *h
	if len(a) < k {
		a = append(a, v)
		for i := len(a) - 1; i > 0 && a[(i-1)/2] < a[i]; i = (i - 1) / 2 {
			a[(i-1)/2], a[i] = a[i], a[(i-1)/2]
		}
		*h = a
		return
	}
	if !(v < a[0]) {
		return
	}
	a[0] = v
	for i := 0; ; {
		l, r, big := 2*i+1, 2*i+2, i
		if l < len(a) && a[l] > a[big] {
			big = l
		}
		if r < len(a) && a[r] > a[big] {
			big = r
		}
		if big == i {
			return
		}
		a[i], a[big] = a[big], a[i]
		i = big
	}
}

// int8Dot is sum(a[i]*b[i]). Every product is at most 127*127 and there are at
// most 65536 of them, so int32 sums cannot overflow.
func int8Dot(a []int8, b []int32) int32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 int32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += int32(a[i]) * b[i]
		s1 += int32(a[i+1]) * b[i+1]
		s2 += int32(a[i+2]) * b[i+2]
		s3 += int32(a[i+3]) * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += int32(a[i]) * b[i]
	}
	return s0 + s1 + s2 + s3
}

// dots is each row's integer code dot product with the query, rows lo..hi:
// the JIT kernel's where there is one, else Go's.
func (vq *vecQuery) dots(side *segVecSide, lo, hi int) []int32 {
	d := side.dims
	out := make([]int32, hi-lo)
	if k := jitI8DotKernel(); k != nil && d%16 == 0 && hi > lo {
		args := jit.Args{
			A:   (*int64)(unsafe.Pointer(&side.codes[lo*d])),
			C:   (*int64)(unsafe.Pointer(unsafe.SliceData(vq.t16))),
			N:   int64(hi - lo),
			XA:  int64(d),
			Out: (*int64)(unsafe.Pointer(&out[0])),
		}
		k.Call(&args)
		return out
	}
	for r := lo; r < hi; r++ {
		out[r-lo] = int8Dot(side.codes[r*d:(r+1)*d], vq.t32)
	}
	return out
}

var jitI8Dot = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitI8Dot) })

// jitI8DotKernel is the compiled int8 dot-product kernel, nil without one.
func jitI8DotKernel() *jit.Code { return jitI8Dot() }
