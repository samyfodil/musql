package engine

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// vector_top_k's approximate mode (vector_index.go) is libSQL's: a DiskANN
// (Vamana) graph over the column, searched by a beam that walks from an entry
// point toward the query. Its options mean what they mean in libSQL
// (vectordiskann.c): max_neighbors is each node's degree, alpha the factor
// by which robust pruning keeps long edges, insert_l and search_l the beam
// widths of building and searching, and compress_neighbors the type the walk
// compares neighbours in.
//
// Where it differs, it differs in the answer's favour. The walk compares
// int8 codes of the rows, which only steers it: the beam's candidates are
// ranked by their exact distances before the k nearest are returned -- the
// same kernels as the exact search -- so every row returned is a real row at
// its real distance, and only recall is approximate. And the graph is built
// from the segment's rows in parallel batches (ParlayANN's prefix doubling)
// the first time an approximate search needs it, rather than one INSERT at a
// time into shadow tables; it is kept with the segment, which never changes.
// Rows the log changed since are searched exactly, as in the exact mode.

// vecApprox is an approximate index's options, defaulted as libSQL defaults
// them. maxNeighbors 0 is libSQL's formula (vecGraphDegree); compress 0 is
// the column's own type.
type vecApprox struct {
	maxNeighbors     int
	alpha            float64
	insertL, searchL int
	compress         vecType
}

// vecGraphKey names one graph: a column, a metric and its options.
type vecGraphKey struct {
	col int
	l2  bool
	a   vecApprox
}

// segVecGraph is one segment's graph over its rows: nbrs[r] are row r's
// out-edges (nil for a NULL row), walked from entry. The walk compares rows
// as int8 codes, row r being about codes[r*dims:] times scale[r] -- after
// rounding through compress_neighbors' type when it is set -- with sq[r] its
// sum of squares. The beam it finds is ranked exactly afterwards, so the
// codes only steer the walk.
type segVecGraph struct {
	nbrs  [][]int32
	entry int32
	dims  int
	codes []int8
	scale []float32
	sq    []float32
	// ready is set once link has built the edges; until then a search
	// answers exactly. order is the insertion order link takes.
	ready atomic.Bool
	order []int32
}

// vecGraph returns the segment's graph for key once it is built, nil until
// then and for a segment that has none. The first call prepares it with prep
// -- the int8 codes, from the segment's rows, while they are mapped -- and
// links the edges in the background, from the codes alone, so no search waits
// for a build and a segment closed meanwhile is never read.
func (s *segment) vecGraph(key vecGraphKey, l2 bool, a vecApprox, prep func() *segVecGraph) *segVecGraph {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	if g, ok := s.vecGraphs[key]; ok {
		if g != nil && g.ready.Load() {
			return g
		}
		return nil
	}
	g := prep()
	if s.vecGraphs == nil {
		s.vecGraphs = map[vecGraphKey]*segVecGraph{}
	}
	s.vecGraphs[key] = g
	if g != nil {
		vecGraphBuilds.Add(1)
		go func() {
			defer vecGraphBuilds.Done()
			g.link(l2, a)
			g.ready.Store(true)
		}()
	}
	return nil
}

// vecGraphBuilds is the graphs still linking, for tests.
var vecGraphBuilds sync.WaitGroup

// VecGraphsBuiltForTest waits for every graph being built to be ready.
func VecGraphsBuiltForTest() { vecGraphBuilds.Wait() }

// vecGraphDegree is libSQL's default max_neighbors: 3(sqrt(dims)+1), capped
// so the edges take at most fifty times the node's own vector.
func vecGraphDegree(dims int, typ, edge vecType) int {
	node := typ.dataSize(dims) + 16
	edgeSz := edge.dataSize(dims) + 16
	return min(3*(int(math.Sqrt(float64(dims)))+1), 50*node/edgeSz+1)
}

// vecGraphMinRows is the fewest rows a graph is built for; below it the exact
// search is already as fast as a walk.
const vecGraphMinRows = 1024

// vecWalk is one worker's scratch for walking the graph.
type vecWalk struct {
	g     *segVecGraph
	l2    bool
	seen  []uint32 // seen[r] == stamp: row r is a candidate or was one
	stamp uint32
	ids   []int32
	offs  []int64
	dots  []int32
	dist  []float32
}

func newVecWalk(g *segVecGraph, n int, l2 bool) *vecWalk {
	return &vecWalk{g: g, l2: l2, seen: make([]uint32, n)}
}

// vecPoint is a vector as the walk compares it from: its int8 codes, its
// scale and its sum of squares.
type vecPoint struct {
	codes     []int8
	scale, sq float32
}

// point is row id as a vecPoint.
func (w *vecWalk) point(id int32) vecPoint {
	g := w.g
	return vecPoint{g.codes[int(id)*g.dims : int(id+1)*g.dims], g.scale[id], g.sq[id]}
}

// vecCand is a beam entry: a row, its distance in the walk's metric, and
// whether its edges were followed.
type vecCand struct {
	id   int32
	d    float32
	done bool
}

// candLess orders the beam: nearer first, a NaN last, then by row.
func candLess(a, b vecCand) bool {
	an, bn := a.d != a.d, b.d != b.d
	if an != bn {
		return bn
	}
	if !an && a.d != b.d {
		return a.d < b.d
	}
	return a.id < b.id
}

// dists is the walk's distance from p to each of ids: from the int8 dot
// products, the cosine distance or the Euclidean one.
func (w *vecWalk) dists(p vecPoint, ids []int32) []float32 {
	g := w.g
	if cap(w.dots) < len(ids) {
		w.dots = make([]int32, len(ids))
		w.dist = make([]float32, len(ids))
	}
	dots, dist := w.dots[:len(ids)], w.dist[:len(ids)]
	if k := jitI8DotGatherKernel(); k != nil && g.dims%16 == 0 && len(ids) > 0 {
		w.offs = w.offs[:0]
		for _, id := range ids {
			w.offs = append(w.offs, int64(id)*int64(g.dims))
		}
		k.Call(&jit.Args{
			A:   unsafe.Pointer(unsafe.SliceData(g.codes)),
			V:   unsafe.Pointer(unsafe.SliceData(w.offs)),
			C:   unsafe.Pointer(unsafe.SliceData(p.codes)),
			N:   int64(len(ids)),
			XA:  int64(g.dims),
			Out: unsafe.Pointer(unsafe.SliceData(dots)),
		})
	} else {
		for i, id := range ids {
			dots[i] = int8Dot8(g.codes[int(id)*g.dims:int(id+1)*g.dims], p.codes)
		}
	}
	for i, id := range ids {
		dot := p.scale * g.scale[id] * float32(dots[i])
		if w.l2 {
			dist[i] = float32(math.Sqrt(math.Max(0, float64(p.sq+g.sq[id]-2*dot))))
		} else if den := p.sq * g.sq[id]; den > 0 {
			dist[i] = 1 - dot/float32(math.Sqrt(float64(den)))
		} else {
			dist[i] = 1
		}
	}
	return dist
}

// search is diskAnnSearchInternal: a beam of width l from the entry,
// following the nearest unfollowed candidate's edges until every one in the
// beam has been followed. It returns the beam and every row it followed.
func (w *vecWalk) search(q vecPoint, l int) (beam []vecCand, visited []int32) {
	w.stamp++
	if w.stamp == 0 { // wrapped: forget every old stamp
		clear(w.seen)
		w.stamp = 1
	}
	g := w.g
	d := w.dists(q, []int32{g.entry})
	w.seen[g.entry] = w.stamp
	beam = append(beam, vecCand{id: g.entry, d: d[0]})
	for {
		i := slices.IndexFunc(beam, func(c vecCand) bool { return !c.done })
		if i < 0 {
			return beam, visited
		}
		beam[i].done = true
		visited = append(visited, beam[i].id)
		w.ids = w.ids[:0]
		for _, nb := range g.nbrs[beam[i].id] {
			if w.seen[nb] != w.stamp {
				w.seen[nb] = w.stamp
				w.ids = append(w.ids, nb)
			}
		}
		if len(w.ids) == 0 {
			continue
		}
		ds := w.dists(q, w.ids)
		for j, id := range w.ids {
			c := vecCand{id: id, d: ds[j]}
			if len(beam) == l && !candLess(c, beam[l-1]) {
				continue
			}
			at, _ := slices.BinarySearchFunc(beam, c, func(a, b vecCand) int {
				if candLess(a, b) {
					return -1
				}
				return 1
			})
			beam = slices.Insert(beam, at, c)
			if len(beam) > l {
				beam = beam[:l]
			}
		}
	}
}

// prune is DiskANN's robust prune of p's candidate edges, the rule libSQL's
// diskAnnPruneEdges applies: nearest first, each kept edge c removing every
// remaining candidate e with d(p,e) > alpha*d(c,e), at most r kept.
func (w *vecWalk) prune(p int32, cands []int32, alpha float64, r int) []int32 {
	slices.Sort(cands)
	cands = slices.Compact(cands)
	if i := slices.Index(cands, p); i >= 0 {
		cands = slices.Delete(cands, i, i+1)
	}
	dp := w.dists(w.point(p), cands)
	type ce struct {
		id int32
		d  float32
	}
	cs := make([]ce, len(cands))
	for i, id := range cands {
		cs[i] = ce{id, dp[i]}
	}
	slices.SortFunc(cs, func(a, b ce) int {
		if candLess(vecCand{id: a.id, d: a.d}, vecCand{id: b.id, d: b.d}) {
			return -1
		}
		return 1
	})
	out := make([]int32, 0, r)
	alive := make([]bool, len(cs))
	for i := range alive {
		alive[i] = true
	}
	rest := make([]int32, 0, len(cs))
	for i, c := range cs {
		if !alive[i] {
			continue
		}
		out = append(out, c.id)
		if len(out) == r {
			break
		}
		rest = rest[:0]
		for j := i + 1; j < len(cs); j++ {
			if alive[j] {
				rest = append(rest, cs[j].id)
			}
		}
		if len(rest) == 0 {
			break
		}
		dc := w.dists(w.point(c.id), rest)
		k := 0
		for j := i + 1; j < len(cs); j++ {
			if !alive[j] {
				continue
			}
			if float64(cs[j].d) > alpha*float64(dc[k]) {
				alive[j] = false
			}
			k++
		}
	}
	return out
}

// prepVecGraph prepares the graph over a segment's rows (float32 vectors at
// heap[offs[r]:]), leaving out the rows in nulls: each row's codes, the
// insertion order and the entry. nil when there are too few rows for a walk
// to beat reading them all.
func prepVecGraph(heap []byte, offs []int, nulls []int, dims int, a vecApprox) *segVecGraph {
	n := len(offs)
	if n-len(nulls) < vecGraphMinRows {
		return nil
	}
	g := &segVecGraph{
		nbrs: make([][]int32, n), dims: dims,
		codes: make([]int8, n*dims), scale: make([]float32, n), sq: make([]float32, n),
	}
	rows, ok := f32Rows(heap, offs, dims)
	if !ok {
		return nil
	}
	step := segVectorChunkRows(dims)
	if !segEachOn(vecWorkers, (n+step-1)/step, func(j int) bool {
		for r := j * step; r < min((j+1)*step, n); r++ {
			x := rows[r]
			if a.compress != 0 && a.compress != vecF32 {
				x = vecRound(x, a.compress)
			}
			g.scale[r], _, _ = quantizeRow(x, g.codes[r*dims:(r+1)*dims])
			var sq float64
			for _, v := range x {
				sq += float64(v) * float64(v)
			}
			g.sq[r] = float32(sq)
		}
		return true
	}) {
		return nil
	}

	// The rows in a fixed shuffled order; the entry, which every walk starts
	// from, is the row nearest their mean.
	order := make([]int32, 0, n-len(nulls))
	for r, k := 0, 0; r < n; r++ {
		if k < len(nulls) && nulls[k] == r {
			k++
			continue
		}
		order = append(order, int32(r))
	}
	rand.New(rand.NewPCG(uint64(n), uint64(dims))).Shuffle(len(order), func(i, j int) {
		order[i], order[j] = order[j], order[i]
	})
	mean := make([]float64, dims)
	for _, r := range order {
		for i, v := range rows[r] {
			mean[i] += float64(v)
		}
	}
	m := make([]float32, dims)
	for i := range m {
		m[i] = float32(mean[i] / float64(len(order)))
	}
	md := make([]float32, len(order))
	orderOffs := make([]int, len(order))
	for i, r := range order {
		orderOffs[i] = offs[r]
	}
	if !vecDistRows(true, heap, orderOffs, m, 0, md) {
		return nil
	}
	best := 0
	for i, d := range md {
		if d < md[best] {
			best = i
		}
	}
	g.entry = order[best]
	order[0], order[best] = order[best], order[0]
	g.order = order
	return g
}

// link builds the edges, from the codes alone. Prefix doubling: each batch
// walks the graph built so far in parallel, then the batch's edges, and their
// reverse edges, go in.
func (g *segVecGraph) link(l2 bool, a vecApprox) {
	n, order := len(g.nbrs), g.order
	degree := a.maxNeighbors
	if degree <= 0 {
		degree = vecGraphDegree(g.dims, vecF32, max(a.compress, vecF32))
	}
	maxBatch := max(1, len(order)/50)
	walks := make(chan *vecWalk, vecWorkers)
	for range vecWorkers {
		walks <- newVecWalk(g, n, l2)
	}
	for lo := 1; lo < len(order); {
		hi := min(len(order), lo+min(lo, maxBatch))
		batch := order[lo:hi]
		out := make([][]int32, len(batch))
		segEachOn(vecWorkers, len(batch), func(i int) bool {
			w := <-walks
			defer func() { walks <- w }()
			p := batch[i]
			beam, visited := w.search(w.point(p), a.insertL)
			cands := slices.Clone(visited)
			for _, c := range beam {
				cands = append(cands, c.id)
			}
			out[i] = w.prune(p, cands, a.alpha, degree)
			return true
		})
		rev := map[int32][]int32{}
		for i, p := range batch {
			g.nbrs[p] = out[i]
			for _, j := range out[i] {
				rev[j] = append(rev[j], p)
			}
		}
		targets := make([]int32, 0, len(rev))
		for j := range rev {
			targets = append(targets, j)
		}
		next := make([][]int32, len(targets))
		segEachOn(vecWorkers, len(targets), func(i int) bool {
			j := targets[i]
			merged := append(slices.Clone(g.nbrs[j]), rev[j]...)
			if len(merged) <= degree {
				slices.Sort(merged)
				next[i] = slices.Compact(merged)
				return true
			}
			w := <-walks
			defer func() { walks <- w }()
			next[i] = w.prune(j, merged, a.alpha, degree)
			return true
		})
		for i, j := range targets {
			g.nbrs[j] = next[i]
		}
		lo = hi
	}
	g.order = nil
}

// vecRound is x rounded through type t and back, as compress_neighbors
// stores a neighbour.
func vecRound(x []float32, t vecType) []float32 {
	data := make([]byte, 0, 4*len(x))
	for _, f := range x {
		data = binary.LittleEndian.AppendUint32(data, math.Float32bits(f))
	}
	return (vector{typ: vecF32, dims: len(x), data: data}).convert(t).floats()
}

// vecGraphTopK offers top each segment's beam for the query, at exact
// distances. It reports false, having offered nothing, when a segment has no
// graph.
func vecGraphTopK(plan *segVectorPlan, segs []*segment, cells [][]uint64, nulls, skips [][]int, query []float32, qn float32, top *vecTopK) bool {
	a := plan.approx
	graphs := make([]*segVecGraph, len(segs))
	for si, s := range segs {
		if cells[si] == nil {
			continue
		}
		key := vecGraphKey{col: plan.col, l2: plan.l2, a: *a}
		graphs[si] = s.vecGraph(key, plan.l2, *a, func() *segVecGraph {
			offs := make([]int, len(cells[si]))
			for i, c := range cells[si] {
				offs[i] = int(uint32(c))
			}
			return prepVecGraph(s.heap, offs, nulls[si], len(query), *a)
		})
		if graphs[si] == nil {
			return false
		}
	}
	// The query as the walk compares it.
	x := query
	if a.compress != 0 && a.compress != vecF32 {
		x = vecRound(x, a.compress)
	}
	codes := make([]int8, len(x))
	scale, _, _ := quantizeRow(x, codes)
	var sq float64
	for _, v := range x {
		sq += float64(v) * float64(v)
	}
	qp := vecPoint{codes, scale, float32(sq)}

	tops := make([]vecTopK, len(segs))
	if !segEachOn(vecWorkers, len(segs), func(si int) bool {
		g := graphs[si]
		t := vecTopK{p: plan, h: make([]vecHit, 0, plan.limit)}
		if g == nil {
			tops[si] = t
			return true
		}
		w := newVecWalk(g, len(cells[si]), plan.l2)
		// Deleted rows stay in the graph as stepping stones; the beam widens by
		// as many so they do not crowd out the k.
		l := max(a.searchL, plan.limit) + min(len(skips[si]), len(cells[si]))
		beam, _ := w.search(qp, l)
		rows := make([]int, 0, len(beam))
		for _, c := range beam {
			if _, dead := slices.BinarySearch(skips[si], int(c.id)); !dead {
				rows = append(rows, int(c.id))
			}
		}
		offs := make([]int, len(rows))
		for i, r := range rows {
			offs[i] = int(uint32(cells[si][r]))
		}
		dist := make([]float32, len(rows))
		if !vecDistRows(plan.l2, segs[si].heap, offs, query, qn, dist) {
			return false
		}
		for i, r := range rows {
			t.offer(vecHit{d: dist[i], seg: si, row: r})
		}
		tops[si] = t
		return true
	}) {
		return false
	}
	for _, t := range tops {
		for _, c := range t.h {
			top.offer(c)
		}
	}
	vecApproxServed.Add(1)
	return true
}

// vecDistRows is the distance from q to each row, the JIT kernel taking the
// rows sixteen at a time and the Go tile the rest.
func vecDistRows(l2 bool, heap []byte, offs []int, q []float32, qn float32, dist []float32) bool {
	done := vecDistJIT(jitVecKernel(l2), l2, heap, offs, q, qn, dist)
	if done == len(offs) {
		return true
	}
	rows, ok := f32Rows(heap, offs[done:], len(q))
	if !ok {
		return false
	}
	if l2 {
		l2Tile(rows, q, dist[done:])
	} else {
		cosTile(rows, q, qn, dist[done:])
	}
	return true
}

// int8Dot8 is the exact int32 dot product of two rows of int8 codes.
func int8Dot8(a, b []int8) int32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 int32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += int32(a[i]) * int32(b[i])
		s1 += int32(a[i+1]) * int32(b[i+1])
		s2 += int32(a[i+2]) * int32(b[i+2])
		s3 += int32(a[i+3]) * int32(b[i+3])
	}
	for ; i < len(a); i++ {
		s0 += int32(a[i]) * int32(b[i])
	}
	return s0 + s1 + s2 + s3
}

var jitI8DotGather = sync.OnceValue(func() *jit.Code { return jitMapOnce(jit.EmitI8DotGather) })

// jitI8DotGatherKernel is the compiled gathering int8 dot-product kernel, nil
// without one.
func jitI8DotGatherKernel() *jit.Code { return jitI8DotGather() }

// vecApproxServed counts searches a graph answered, for tests.
var vecApproxServed atomic.Int64

// VecApproxServedForTest reports and resets how many searches the
// approximate mode's graphs answered.
func VecApproxServedForTest() int64 { return vecApproxServed.Swap(0) }
