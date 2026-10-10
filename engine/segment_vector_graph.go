package engine

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
	"sync/atomic"
)

// vector_top_k's approximate mode (vector_index.go) is libSQL's: a DiskANN
// (Vamana) graph over the column, searched by a beam that walks from an entry
// point toward the query. Its options mean what they mean in libSQL
// (vectordiskann.c): max_neighbors is each node's degree, alpha the factor
// by which robust pruning keeps long edges, insert_l and search_l the beam
// widths of building and searching, and compress_neighbors the type the walk
// compares neighbours in.
//
// Two things differ, both in the answer's favour. The beam's candidates are
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
// out-edges (nil for a NULL row), walked from entry. The walk compares
// vectors in trav at toffs, the rows themselves unless compress_neighbors
// rounded them.
type segVecGraph struct {
	nbrs  [][]int32
	entry int32
	trav  []byte
	toffs []int
}

// vecGraph returns the segment's graph for key, building it with build the
// first time; nil when the segment has none.
func (s *segment) vecGraph(key vecGraphKey, build func() *segVecGraph) *segVecGraph {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	if g, ok := s.vecGraphs[key]; ok {
		return g
	}
	g := build()
	if s.vecGraphs == nil {
		s.vecGraphs = map[vecGraphKey]*segVecGraph{}
	}
	s.vecGraphs[key] = g
	return g
}

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
	dims  int
	seen  []uint32 // seen[r] == stamp: row r is a candidate or was one
	stamp uint32
	ids   []int32
	offs  []int
	dist  []float32
}

func newVecWalk(g *segVecGraph, n, dims int, l2 bool) *vecWalk {
	return &vecWalk{g: g, l2: l2, dims: dims, seen: make([]uint32, n)}
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

// dists is the walk's distance from q to each of ids.
func (w *vecWalk) dists(q []float32, qn float32, ids []int32) ([]float32, bool) {
	w.offs = w.offs[:0]
	for _, id := range ids {
		w.offs = append(w.offs, w.g.toffs[id])
	}
	if cap(w.dist) < len(ids) {
		w.dist = make([]float32, len(ids))
	}
	d := w.dist[:len(ids)]
	return d, vecDistRows(w.l2, w.g.trav, w.offs, q, qn, d)
}

// search is diskAnnSearchInternal: a beam of width l from the entry,
// following the nearest unfollowed candidate's edges until every one in the
// beam has been followed. It returns the beam and every row it followed.
func (w *vecWalk) search(q []float32, qn float32, l int) (beam []vecCand, visited []int32, ok bool) {
	w.stamp++
	if w.stamp == 0 { // wrapped: forget every old stamp
		clear(w.seen)
		w.stamp = 1
	}
	g := w.g
	d, ok := w.dists(q, qn, []int32{g.entry})
	if !ok {
		return nil, nil, false
	}
	w.seen[g.entry] = w.stamp
	beam = append(beam, vecCand{id: g.entry, d: d[0]})
	for {
		i := slices.IndexFunc(beam, func(c vecCand) bool { return !c.done })
		if i < 0 {
			return beam, visited, true
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
		ds, ok := w.dists(q, qn, w.ids)
		if !ok {
			return nil, nil, false
		}
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

// row is the walk's vector for row id.
func (w *vecWalk) row(id int32) ([]float32, bool) {
	rows, ok := f32Rows(w.g.trav, []int{w.g.toffs[id]}, w.dims)
	if !ok {
		return nil, false
	}
	return rows[0], true
}

// prune is DiskANN's robust prune of p's candidate edges, the rule libSQL's
// diskAnnPruneEdges applies: nearest first, each kept edge c removing every
// remaining candidate e with d(p,e) > alpha*d(c,e), at most r kept.
func (w *vecWalk) prune(p int32, cands []int32, alpha float64, r int) ([]int32, bool) {
	slices.Sort(cands)
	cands = slices.Compact(cands)
	if i := slices.Index(cands, p); i >= 0 {
		cands = slices.Delete(cands, i, i+1)
	}
	pv, ok := w.row(p)
	if !ok {
		return nil, false
	}
	dp, ok := w.dists(pv, vecSelfDot(pv), cands)
	if !ok {
		return nil, false
	}
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
		cv, ok := w.row(c.id)
		if !ok {
			return nil, false
		}
		dc, ok := w.dists(cv, vecSelfDot(cv), rest)
		if !ok {
			return nil, false
		}
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
	return out, true
}

// buildVecGraph builds the graph over a segment's rows (float32 vectors at
// heap[offs[r]:]), leaving out the rows in nulls. nil when there are too few
// rows for a walk to beat reading them all.
func buildVecGraph(heap []byte, offs []int, nulls []int, dims int, l2 bool, a vecApprox) *segVecGraph {
	n := len(offs)
	if n-len(nulls) < vecGraphMinRows {
		return nil
	}
	g := &segVecGraph{nbrs: make([][]int32, n), trav: heap, toffs: offs}
	if a.compress != 0 && a.compress != vecF32 {
		// The walk compares rows rounded through the compressed type, as
		// libSQL's edges store them.
		g.trav = make([]byte, 0, 4*dims*n)
		g.toffs = make([]int, n)
		rows, ok := f32Rows(heap, offs, dims)
		if !ok {
			return nil
		}
		for r, x := range rows {
			g.toffs[r] = len(g.trav)
			data := make([]byte, 0, 4*dims)
			for _, f := range x {
				data = binary.LittleEndian.AppendUint32(data, math.Float32bits(f))
			}
			for _, f := range (vector{typ: vecF32, dims: dims, data: data}).convert(a.compress).floats() {
				g.trav = binary.LittleEndian.AppendUint32(g.trav, math.Float32bits(f))
			}
		}
	}
	degree := a.maxNeighbors
	if degree <= 0 {
		degree = vecGraphDegree(dims, vecF32, max(a.compress, vecF32))
	}

	// The rows in a fixed shuffled order; the entry is the row nearest their
	// mean, which every walk starts from.
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
	travOffs := make([]int, len(order))
	for i, r := range order {
		travOffs[i] = g.toffs[r]
	}
	rows, ok := f32Rows(g.trav, travOffs, dims)
	if !ok {
		return nil
	}
	for _, x := range rows {
		for i, v := range x {
			mean[i] += float64(v)
		}
	}
	m := make([]float32, dims)
	for i := range m {
		m[i] = float32(mean[i] / float64(len(order)))
	}
	md := make([]float32, len(order))
	if !vecDistRows(true, g.trav, travOffs, m, 0, md) {
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

	// Prefix doubling: each batch walks the graph built so far in parallel,
	// then the batch's edges, and their reverse edges, go in.
	maxBatch := max(1, len(order)/50)
	walks := make(chan *vecWalk, vecWorkers)
	for range vecWorkers {
		walks <- newVecWalk(g, n, dims, l2)
	}
	for lo := 1; lo < len(order); {
		hi := min(len(order), lo+min(lo, maxBatch))
		batch := order[lo:hi]
		out := make([][]int32, len(batch))
		if !segEachOn(vecWorkers, len(batch), func(i int) bool {
			w := <-walks
			defer func() { walks <- w }()
			p := batch[i]
			pv, ok := w.row(p)
			if !ok {
				return false
			}
			beam, visited, ok := w.search(pv, vecSelfDot(pv), a.insertL)
			if !ok {
				return false
			}
			cands := slices.Clone(visited)
			for _, c := range beam {
				cands = append(cands, c.id)
			}
			out[i], ok = w.prune(p, cands, a.alpha, degree)
			return ok
		}) {
			return nil
		}
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
		if !segEachOn(vecWorkers, len(targets), func(i int) bool {
			j := targets[i]
			merged := append(slices.Clone(g.nbrs[j]), rev[j]...)
			if len(merged) <= degree {
				slices.Sort(merged)
				next[i] = slices.Compact(merged)
				return true
			}
			w := <-walks
			defer func() { walks <- w }()
			var ok bool
			next[i], ok = w.prune(j, merged, a.alpha, degree)
			return ok
		}) {
			return nil
		}
		for i, j := range targets {
			g.nbrs[j] = next[i]
		}
		lo = hi
	}
	return g
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
		graphs[si] = s.vecGraph(key, func() *segVecGraph {
			offs := make([]int, len(cells[si]))
			for i, c := range cells[si] {
				offs[i] = int(uint32(c))
			}
			return buildVecGraph(s.heap, offs, nulls[si], len(query), plan.l2, *a)
		})
		if graphs[si] == nil {
			return false
		}
	}
	tops := make([]vecTopK, len(segs))
	if !segEachOn(vecWorkers, len(segs), func(si int) bool {
		g := graphs[si]
		t := vecTopK{p: plan, h: make([]vecHit, 0, plan.limit)}
		if g == nil {
			tops[si] = t
			return true
		}
		w := newVecWalk(g, len(cells[si]), len(query), plan.l2)
		// Deleted rows stay in the graph as stepping stones; the beam widens by
		// as many so they do not crowd out the k.
		l := max(a.searchL, plan.limit) + min(len(skips[si]), len(cells[si]))
		beam, _, ok := w.search(query, qn, l)
		if !ok {
			return false
		}
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

// vecApproxServed counts searches a graph answered, for tests.
var vecApproxServed atomic.Int64

// VecApproxServedForTest reports and resets how many searches the
// approximate mode's graphs answered.
func VecApproxServedForTest() int64 { return vecApproxServed.Swap(0) }
