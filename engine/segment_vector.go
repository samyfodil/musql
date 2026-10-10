package engine

import (
	"math"
	"slices"
	"sort"
	"sync"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// Exact nearest-neighbour search from the columnar segments:
//
//	SELECT ... FROM t ORDER BY vector_distance_cos(emb, ?) LIMIT k
//
// (or _l2, either argument order, ASC or DESC, with an OFFSET). The loop the
// compiler emits calls the distance function once per row and sorts; this
// computes every row's distance in a tiled kernel straight off the column's
// blob heap and keeps the closest k in a heap.
//
// It must give the loop's answer to the bit. libSQL's distances accumulate
// each row's sums in float32, in component order, so they cannot be split
// across SIMD lanes by component without changing the low bits. The kernel
// tiles ACROSS ROWS instead: a tile computes several rows at once, each with
// its own accumulators, each in component order. That is what lets a SIMD
// lane hold a row (the JIT's kernels) and what breaks the add-latency chain
// that bounds a one-row loop (this one).
//
// It serves float32 vectors, the type libSQL's F32_BLOB columns hold, on a
// column whose every cell is a BLOB of the query's length. Anything else -- a
// NULL, a TEXT cell, another length or type, a malformed query -- declines to
// the loop, which raises the function's error where there is one.

// segVectorPlan is what OpSegVectorTopK carries.
type segVectorPlan struct {
	l2      bool // vector_distance_l2, else _cos
	col     int  // the vector column
	param   int  // the query vector's parameter number
	desc    bool
	limit   int // rows the heap keeps: the LIMIT plus the OFFSET
	offset  int
	outCols []int
}

// segVectorPeephole rewrites prog when it is a distance-ordered top-k over a
// table. Reports whether it did.
func segVectorPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 14 || in[0].Op != OpInit || in[1].Op != OpSorterOpen || in[2].Op != OpOpenRead {
		return false
	}
	ki, ok := in[1].P4.(*sorterKeyInfo)
	if !ok || ki == nil || ki.nKey != 1 || ki.bound <= 0 {
		return false
	}
	if len(ki.nulls) > 0 && ki.nulls[0] != NullsDefault {
		return false
	}
	sorterNum := in[1].P1
	rewindAt := 3
	for rewindAt < len(in) && segIsScanPrologue(in[rewindAt].Op) {
		if in[rewindAt].Op == OpAutoIndexOrder {
			return false
		}
		rewindAt++
	}
	if rewindAt >= len(in) || in[rewindAt].Op != OpRewind {
		return false
	}
	cursor := in[rewindAt].P1
	if in[2].P1 != cursor {
		return false
	}

	// The fill loop: columns, the query parameter, and one distance call.
	type src struct {
		col, param int  // a column, or a parameter (the other is -1)
		dist       bool // the distance
	}
	regs := map[int]src{}
	plan := &segVectorPlan{param: -1}
	pc := rewindAt + 1
loop:
	for ; pc < len(in); pc++ {
		ins := in[pc]
		switch ins.Op {
		case OpColumn:
			if ins.P1 != cursor {
				return false
			}
			regs[ins.P3] = src{col: ins.P2, param: -1}
		case OpVariable:
			regs[ins.P2] = src{col: -1, param: ins.P1}
		case OpSCopy:
			s, ok := regs[ins.P1]
			if !ok {
				return false
			}
			regs[ins.P2] = s
		case OpFunction:
			name, _ := ins.P4.(string) // a registered UDF is a *ScalarFunction
			if (name != "vector_distance_cos" && name != "vector_distance_l2") || ins.P2 != 2 || plan.param >= 0 {
				return false
			}
			a, okA := regs[ins.P1]
			b, okB := regs[ins.P1+1]
			if !okA || !okB || a.dist || b.dist {
				return false
			}
			if a.col < 0 {
				a, b = b, a // the distance is symmetric in its arguments
			}
			if a.col < 0 || b.param < 0 {
				return false
			}
			plan.l2 = name == "vector_distance_l2"
			plan.col, plan.param = a.col, b.param
			regs[ins.P3] = src{col: -1, param: -1, dist: true}
		default:
			break loop
		}
	}
	if plan.param < 0 {
		return false
	}
	if pc < len(in) && in[pc].Op == OpSorterCheck {
		if in[pc].P1 != sorterNum {
			return false
		}
		pc++
	}
	if pc+2 >= len(in) || in[pc].Op != OpMakeRecord || in[pc+1].Op != OpSorterInsert || in[pc+2].Op != OpNext {
		return false
	}
	recBase, nRec := in[pc].P1, in[pc].P2
	if in[pc+1].P1 != sorterNum || in[pc+2].P1 != cursor || in[pc+2].P2 != rewindAt+1 || nRec < 2 {
		return false
	}
	if k, ok := regs[recBase]; !ok || !k.dist {
		return false
	}
	for i := 1; i < nRec; i++ {
		s, ok := regs[recBase+i]
		if !ok || s.col < 0 {
			return false
		}
		plan.outCols = append(plan.outCols, s.col)
	}
	offset, outBase, nOut, ok := segSorterDrain(in, pc+3, sorterNum, ki.bound)
	if !ok || nOut != nRec-1 {
		return false
	}
	plan.limit, plan.offset = ki.bound, offset
	plan.desc = len(ki.desc) > 0 && ki.desc[0]

	// The guard goes in before the loop; the emit loop goes after the program.
	out := make([]Instruction, 0, len(in)+5)
	out = append(out, in[0], in[1], in[2])
	emitAt := len(in) + 1
	out = append(out, Instruction{Op: OpSegVectorTopK, P1: outBase, P2: cursor, P3: emitAt, P4: plan})
	for _, ins := range in[3:] {
		switch ins.Op {
		case OpRewind, OpNext, OpIfNot, OpSorterSort, OpSorterNext, OpIf, OpGoto, OpSorterCheck:
			ins.P2++
		}
		out = append(out, ins)
	}
	out = append(out,
		Instruction{Op: OpSegEmitRow, P1: outBase, P2: emitAt + 3, P3: nOut},
		Instruction{Op: OpResultRow, P1: outBase, P2: nOut},
		Instruction{Op: OpGoto, P2: emitAt},
		Instruction{Op: OpHalt})
	prog.Insns = out
	return true
}

// vecHit is one candidate: its distance (NaN for NULL) and where it is.
// (seg, row) is scan order, the sorter's tie-break; the log's rows come after
// every segment's, in rowid order, as the loop visits them.
type vecHit struct {
	d        float32
	seg, row int
	rid      int64 // a log row's rowid
}

// before is the sorter's order for one REAL key: NULL first ascending and last
// descending, then the value, then scan order.
func (p *segVectorPlan) before(a, b vecHit) bool {
	an, bn := a.d != a.d, b.d != b.d
	if an != bn {
		return an != p.desc
	}
	if !an && a.d != b.d {
		return (a.d < b.d) != p.desc
	}
	if a.seg != b.seg {
		return a.seg < b.seg
	}
	return a.row < b.row
}

// vecTopK is a bounded heap whose root is the worst kept hit.
type vecTopK struct {
	p *segVectorPlan
	h []vecHit
}

func (t *vecTopK) offer(c vecHit) {
	p, h := t.p, t.h
	if len(h) < p.limit {
		h = append(h, c)
		for i := len(h) - 1; i > 0; {
			up := (i - 1) / 2
			if !p.before(h[up], h[i]) {
				break
			}
			h[up], h[i] = h[i], h[up]
			i = up
		}
		t.h = h
		return
	}
	if !p.before(c, h[0]) {
		return
	}
	h[0] = c
	for i := 0; ; {
		l, r, big := 2*i+1, 2*i+2, i
		if l < len(h) && p.before(h[big], h[l]) {
			big = l
		}
		if r < len(h) && p.before(h[big], h[r]) {
			big = r
		}
		if big == i {
			break
		}
		h[i], h[big] = h[big], h[i]
		i = big
	}
}

// segVectorChunkRows is how many rows one job takes: about two million
// components, a multiple of 16. A JIT kernel cannot be preempted, so its work
// has to be bounded.
func segVectorChunkRows(dims int) int {
	return max(16, (1<<21)/dims&^15)
}

// segVectorTopK answers the statement for the query vector q, or reports
// false to decline.
func (p *ReadOnlyPager) segVectorTopK(rootPage uint32, plan *segVectorPlan, ipkCol int, q Value) ([][]Value, bool) {
	if p == nil || p.segs == nil || plan == nil || plan.limit <= 0 || q.Typ != Blob {
		return nil, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok || !p.segs.deltaIsSmallFor(rootPage, segs) {
		return nil, false
	}
	qv, err := parseVector(q, 0)
	if err != nil || qv.typ != vecF32 || qv.dims == 0 || plan.col == ipkCol {
		return nil, false
	}
	query := make([]float32, qv.dims)
	for i := range query {
		query[i] = f32At(qv.data, i)
	}
	width := 4 * qv.dims
	skips, live, mergeable := p.segs.segDeltaSkips(rootPage, segs)
	if !mergeable {
		return nil, false
	}

	// Every cell must be a BLOB of the query's length: then each one parses
	// as a float32 vector of the query's dimensions, as the function would.
	type work struct{ seg, lo, hi int }
	var jobs []work
	cells := make([][]uint64, len(segs))
	for si, s := range segs {
		c, _, ok := s.BytesColumn(plan.col)
		if !ok || s.cols[plan.col].phys != PhysBlob {
			return nil, false
		}
		for _, cell := range c {
			if int(cell>>32) != width {
				return nil, false
			}
		}
		cells[si] = c
		for lo, step := 0, segVectorChunkRows(qv.dims); lo < s.nRows; lo += step {
			jobs = append(jobs, work{si, lo, min(lo+step, s.nRows)})
		}
	}

	qn := vecSelfDot(query) // the query's norm, summed as the function sums it
	tops := make([]vecTopK, len(jobs))
	if !segEach(len(jobs), func(j int) bool {
		w := jobs[j]
		s := segs[w.seg]
		dist := make([]float32, w.hi-w.lo)
		offs := make([]int, w.hi-w.lo)
		for i := range offs {
			offs[i] = int(uint32(cells[w.seg][w.lo+i]))
		}
		// The JIT kernel takes the rows sixteen at a time, straight off the
		// heap; the Go tile takes the rest, and any rows it would have to copy.
		done := 0
		if k := jitVecKernel(plan.l2); k != nil {
			done = vecDistJIT(k, plan.l2, s.heap, offs, query, qn, dist)
		}
		if done < len(offs) {
			rows, ok := f32Rows(s.heap, offs[done:], len(query))
			if !ok {
				return false
			}
			if plan.l2 {
				l2Tile(rows, query, dist[done:])
			} else {
				cosTile(rows, query, qn, dist[done:])
			}
		}
		t := vecTopK{p: plan, h: make([]vecHit, 0, plan.limit)}
		skip := skips[w.seg]
		k, _ := slices.BinarySearch(skip, w.lo)
		for i, d := range dist {
			r := w.lo + i
			if k < len(skip) && skip[k] == r {
				k++
				continue
			}
			t.offer(vecHit{d: d, seg: w.seg, row: r})
		}
		tops[j] = t
		return true
	}) {
		return nil, false
	}
	all := vecTopK{p: plan, h: make([]vecHit, 0, plan.limit)}
	for _, t := range tops {
		for _, c := range t.h {
			all.offer(c)
		}
	}

	// The log's live rows go through the function itself.
	if len(live) > 0 {
		rids := make([]int64, 0, len(live))
		for rid := range live {
			rids = append(rids, rid)
		}
		slices.Sort(rids)
		for i, rid := range rids {
			vals := live[rid]
			if plan.col >= len(vals) || vals[plan.col].Typ != Blob || len(vals[plan.col].S) != width {
				return nil, false
			}
			v, err := parseVector(vals[plan.col], 0)
			if err != nil {
				return nil, false
			}
			var d float32
			if plan.l2 {
				d = distanceL2(v, qv)
			} else {
				d = distanceCos(v, qv)
			}
			all.offer(vecHit{d: d, seg: len(segs), row: i, rid: rid})
		}
	}

	sort.Slice(all.h, func(i, j int) bool { return plan.before(all.h[i], all.h[j]) })
	rows := make([][]Value, len(all.h))
	readers := make([][]segColReader, len(segs))
	for i, c := range all.h {
		row := make([]Value, len(plan.outCols))
		if c.seg == len(segs) {
			vals := live[c.rid]
			for j, col := range plan.outCols {
				switch {
				case col == ipkCol && ipkCol >= 0:
					row[j] = Value{Typ: Int, I: c.rid}
				case col < len(vals):
					row[j] = vals[col]
				default:
					return nil, false
				}
			}
		} else {
			s := segs[c.seg]
			if readers[c.seg] == nil {
				readers[c.seg] = segColReadersLenient(s, plan.outCols, ipkCol)
			}
			for j := range plan.outCols {
				v, ok := readers[c.seg][j].value(s, c.row)
				if !ok {
					return nil, false
				}
				row[j] = v
			}
		}
		rows[i] = row
	}
	return rows, true
}

// jitVecKey names a distance kernel: the distance, and whether it is the
// strided one (rows back to back) or the gathering one (rows anywhere).
type jitVecKey struct{ l2, strided bool }

var jitVecCache sync.Map // jitVecKey -> *jit.Code, nil when there is no emitter

// jitVecKernel returns the compiled distance kernel, emitting it on first
// use; nil where the JIT is off or has no vector emitter.
func jitVecKernel(l2 bool) *jit.Code { return jitVecKernelOf(jitVecKey{l2: l2}) }

func jitVecKernelOf(key jitVecKey) *jit.Code {
	if !jitEnabled || !jit.HasVector() {
		return nil
	}
	if c, ok := jitVecCache.Load(key); ok {
		return c.(*jit.Code)
	}
	emit := jit.EmitVecDist
	if key.strided {
		emit = jit.EmitVecDistStrided
	}
	var k *jit.Code
	if code, err := emit(key.l2); err == nil {
		if m, err := jit.Map(code); err == nil {
			k = m
		}
	}
	actual, loaded := jitVecCache.LoadOrStore(key, k)
	if loaded && k != nil {
		k.Close()
	}
	return actual.(*jit.Code)
}

// vecDistJIT runs a kernel over the leading rows, writing their distances to
// dist, and reports how many rows it did. Rows stored back to back (how a
// segment stores a column of equal-length vectors) take the strided kernel
// eight at a time when the dimensions are a multiple of 8; otherwise rows
// that sit 4-aligned take the gathering kernel k sixteen at a time.
func vecDistJIT(k *jit.Code, l2 bool, heap []byte, offs []int, q []float32, qn float32, dist []float32) int {
	if len(offs) == 0 || len(heap) == 0 {
		return 0
	}
	if n := vecStridedRows(heap, offs, len(q)); n > 0 {
		if ks := jitVecKernelOf(jitVecKey{l2: l2, strided: true}); ks != nil {
			sums := make([]float32, 2*n)
			args := jit.Args{
				A:    (*int64)(unsafe.Pointer(&heap[offs[0]])),
				XC:   int64(4 * len(q)),
				V:    (*int64)(unsafe.Pointer(unsafe.SliceData(q))),
				N:    int64(n),
				XA:   int64(len(q)),
				Out:  (*int64)(unsafe.Pointer(&sums[0])),
				Out2: (*int64)(unsafe.Pointer(&sums[n])),
			}
			ks.Call(&args)
			vecFinish(l2, sums, n, qn, dist)
			return n
		}
	}
	n := len(offs) &^ 15
	if n == 0 {
		return 0
	}
	base := uintptr(unsafe.Pointer(unsafe.SliceData(heap)))
	idx := make([]int32, n)
	for i, off := range offs[:n] {
		if (base+uintptr(off))%4 != 0 || off > math.MaxInt32-4*len(q) {
			n = i &^ 15
			break
		}
		idx[i] = int32(off)
	}
	if n == 0 {
		return 0
	}
	sums := make([]float32, 2*n)
	args := jit.Args{
		A:    (*int64)(unsafe.Pointer(unsafe.SliceData(heap))),
		C:    (*int64)(unsafe.Pointer(unsafe.SliceData(idx))),
		V:    (*int64)(unsafe.Pointer(unsafe.SliceData(q))),
		N:    int64(n),
		XA:   int64(len(q)),
		Out:  (*int64)(unsafe.Pointer(&sums[0])),
		Out2: (*int64)(unsafe.Pointer(&sums[n])),
	}
	k.Call(&args)
	vecFinish(l2, sums, n, qn, dist)
	return n
}

// vecFinish turns a kernel's per-row sums (sums[:n], and for the cosine the
// rows' norms in sums[n:]) into distances.
func vecFinish(l2 bool, sums []float32, n int, qn float32, dist []float32) {
	for r := range n {
		if l2 {
			dist[r] = float32(math.Sqrt(float64(sums[r])))
		} else {
			dist[r] = cosine(sums[r], sums[n+r], qn)
		}
	}
}

// vecStridedRows is how many leading rows, a multiple of 8, the strided kernel
// can take: dimensions a multiple of 8, and rows exactly one vector apart.
func vecStridedRows(heap []byte, offs []int, dims int) int {
	if dims%8 != 0 {
		return 0
	}
	n := len(offs) &^ 7
	stride := 4 * dims
	for i := 1; i < n; i++ {
		if offs[i] != offs[0]+i*stride {
			n = i &^ 7
			break
		}
	}
	if n == 0 || offs[0]+n*stride > len(heap) {
		return 0
	}
	return n
}

// vecSelfDot is a vector's sum of squares, in component order, in float32.
func vecSelfDot(q []float32) float32 {
	var n float32
	for _, y := range q {
		n += float32(y * y)
	}
	return n
}

// f32Rows views each row (a float32 vector of dims components at
// heap[offs[r]:]) as a []float32. The segment is little-endian (BytesColumn
// checks), so a 4-aligned row is viewed in place; the rest are copied into
// one aligned buffer, a memmove, since the heap packs cells without padding.
func f32Rows(heap []byte, offs []int, dims int) ([][]float32, bool) {
	rows := make([][]float32, len(offs))
	var spill []float32
	for r, off := range offs {
		if off < 0 || off+4*dims > len(heap) {
			return nil, false
		}
		src := heap[off : off+4*dims]
		if p := unsafe.Pointer(unsafe.SliceData(src)); uintptr(p)%4 == 0 {
			rows[r] = unsafe.Slice((*float32)(p), dims)
			continue
		}
		if spill == nil {
			spill = make([]float32, dims*(len(offs)-r))
		}
		row := spill[:dims:dims]
		spill = spill[dims:]
		copy(unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(row))), 4*dims), src)
		rows[r] = row
	}
	return rows, true
}

// cosTile writes the cosine distance between q and each row to dist[r], four
// rows at a time. Every row's sums are accumulated in component order, so
// each answer is cosF32's to the bit.
func cosTile(rows [][]float32, q []float32, qn float32, dist []float32) {
	r := 0
	for ; r+4 <= len(rows); r += 4 {
		a, b, c, d := rows[r][:len(q)], rows[r+1][:len(q)], rows[r+2][:len(q)], rows[r+3][:len(q)]
		var da, db, dc, dd, na, nb, nc, nd float32
		for i, y := range q {
			xa, xb, xc, xd := a[i], b[i], c[i], d[i]
			da += float32(xa * y)
			db += float32(xb * y)
			dc += float32(xc * y)
			dd += float32(xd * y)
			na += float32(xa * xa)
			nb += float32(xb * xb)
			nc += float32(xc * xc)
			nd += float32(xd * xd)
		}
		dist[r], dist[r+1] = cosine(da, na, qn), cosine(db, nb, qn)
		dist[r+2], dist[r+3] = cosine(dc, nc, qn), cosine(dd, nd, qn)
	}
	for ; r < len(rows); r++ {
		a := rows[r][:len(q)]
		var dot, nx float32
		for i, y := range q {
			dot += float32(a[i] * y)
			nx += float32(a[i] * a[i])
		}
		dist[r] = cosine(dot, nx, qn)
	}
}

// l2Tile is cosTile for the Euclidean distance.
func l2Tile(rows [][]float32, q []float32, dist []float32) {
	r := 0
	for ; r+4 <= len(rows); r += 4 {
		a, b, c, d := rows[r][:len(q)], rows[r+1][:len(q)], rows[r+2][:len(q)], rows[r+3][:len(q)]
		var sa, sb, sc, sd float32
		for i, y := range q {
			ea, eb, ec, ed := a[i]-y, b[i]-y, c[i]-y, d[i]-y
			sa += float32(ea * ea)
			sb += float32(eb * eb)
			sc += float32(ec * ec)
			sd += float32(ed * ed)
		}
		dist[r], dist[r+1] = float32(math.Sqrt(float64(sa))), float32(math.Sqrt(float64(sb)))
		dist[r+2], dist[r+3] = float32(math.Sqrt(float64(sc))), float32(math.Sqrt(float64(sd)))
	}
	for ; r < len(rows); r++ {
		a := rows[r][:len(q)]
		var sum float32
		for i, y := range q {
			e := a[i] - y
			sum += float32(e * e)
		}
		dist[r] = float32(math.Sqrt(float64(sum)))
	}
}
