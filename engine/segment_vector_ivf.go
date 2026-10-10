package engine

import (
	"math"
	"slices"
	"sync/atomic"
)

// vector_top_k's approximate mode (vector_index.go): each segment's vector
// column is partitioned into lists around k-means centroids, and a search
// reads only the lists whose centroids are nearest the query -- an inverted
// file (IVF). The rows it reads are ranked by their exact distances, through
// the same kernels as the exact search, so every row returned is a real row
// at its real distance; what the mode gives up is only that a true neighbour
// in a list it did not read is missed.
//
// The lists are built in memory from the segment's rows the first time an
// approximate search needs them, and kept with the segment, which never
// changes. Rows the log changed since are searched exactly, as in the exact
// mode.

// vecApprox is an approximate index's tuning, from libSQL's option names:
// max_neighbors is the rows per list, search_l how many rows a search reads
// (32 per unit), insert_l how many rows per list train the centroids.
type vecApprox struct {
	perList, searchL, insertL int
}

// vecIVFKey names one set of lists: a column, a metric and its tuning.
type vecIVFKey struct {
	col              int
	l2               bool
	perList, insertL int
}

// segVecIVF is one segment's lists: centroid c is cents[c], and lists[c] the
// rows nearest it.
type segVecIVF struct {
	cents [][]float32
	lists [][]int32
}

// vecIVF returns the segment's lists for key, building them with build the
// first time; nil when the segment has none (too few rows).
func (s *segment) vecIVF(key vecIVFKey, build func() *segVecIVF) *segVecIVF {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	if v, ok := s.vecIVFs[key]; ok {
		return v
	}
	v := build()
	if s.vecIVFs == nil {
		s.vecIVFs = map[vecIVFKey]*segVecIVF{}
	}
	s.vecIVFs[key] = v
	return v
}

// vecIVFIters is how many k-means rounds train the centroids.
const vecIVFIters = 10

// buildVecIVF partitions rows (float32 vectors of dims components at
// heap[offs[r]:]) into lists. It returns nil for fewer than two lists' worth
// of rows, where reading everything is the search anyway.
func buildVecIVF(heap []byte, offs []int, dims int, l2 bool, a *vecApprox) *segVecIVF {
	n := len(offs)
	nlist := int(math.Sqrt(float64(n)))
	if a.perList > 0 {
		nlist = n / a.perList
	}
	nlist = min(nlist, 1<<16)
	if nlist < 2 {
		return nil
	}
	// Train on an even sample, insert_l rows per list.
	ns := min(n, nlist*max(a.insertL, 1))
	sample := make([]int, ns)
	for i := range sample {
		sample[i] = offs[int(int64(i)*int64(n)/int64(ns))]
	}
	cents := make([][]float32, nlist)
	for c := range cents {
		rows, ok := f32Rows(heap, sample[c*ns/nlist:c*ns/nlist+1], dims)
		if !ok {
			return nil
		}
		cents[c] = rows[0]
	}
	for range vecIVFIters {
		assign, ok := vecNearest(heap, sample, dims, cents, l2)
		if !ok {
			return nil
		}
		next, ok := vecMeans(heap, sample, dims, assign, nlist, l2)
		if !ok {
			return nil
		}
		for c, m := range next {
			if m != nil { // an emptied list keeps its centroid
				cents[c] = m
			}
		}
	}
	assign, ok := vecNearest(heap, offs, dims, cents, l2)
	if !ok {
		return nil
	}
	lists := make([][]int32, nlist)
	for r, c := range assign {
		lists[c] = append(lists[c], int32(r))
	}
	return &segVecIVF{cents: cents, lists: lists}
}

// vecNearest is each row's nearest centroid, by the search's own distance.
// A row no centroid is nearer than NaN (a zero vector's cosine) goes to list 0.
func vecNearest(heap []byte, offs []int, dims int, cents [][]float32, l2 bool) ([]int32, bool) {
	n := len(offs)
	assign := make([]int32, n)
	qns := make([]float32, len(cents))
	for c, x := range cents {
		qns[c] = vecSelfDot(x)
	}
	step := segVectorChunkRows(dims)
	ok := segEachOn(vecWorkers, (n+step-1)/step, func(j int) bool {
		lo, hi := j*step, min((j+1)*step, n)
		best := make([]float32, hi-lo)
		for i := range best {
			best[i] = float32(math.Inf(1))
		}
		dist := make([]float32, hi-lo)
		for c, x := range cents {
			if !vecDistRows(l2, heap, offs[lo:hi], x, qns[c], dist) {
				return false
			}
			for i, d := range dist {
				if d < best[i] {
					best[i], assign[lo+i] = d, int32(c)
				}
			}
		}
		return true
	})
	return assign, ok
}

// vecMeans is each list's new centroid: the mean of its rows, or for the
// cosine the mean of their directions. nil for a list with no rows.
func vecMeans(heap []byte, offs []int, dims int, assign []int32, nlist int, l2 bool) ([][]float32, bool) {
	sums := make([][]float64, nlist)
	rows, ok := f32Rows(heap, offs, dims)
	if !ok {
		return nil, false
	}
	for r, x := range rows {
		scale := 1.0
		if !l2 {
			var nn float64
			for _, v := range x {
				nn += float64(v) * float64(v)
			}
			if nn == 0 || math.IsInf(nn, 0) || math.IsNaN(nn) {
				continue
			}
			scale = 1 / math.Sqrt(nn)
		}
		c := assign[r]
		if sums[c] == nil {
			sums[c] = make([]float64, dims)
		}
		for i, v := range x {
			sums[c][i] += float64(v) * scale
		}
	}
	out := make([][]float32, nlist)
	counts := make([]int, nlist)
	for _, c := range assign {
		counts[c]++
	}
	for c, s := range sums {
		if s == nil {
			continue
		}
		m := make([]float32, dims)
		for i, v := range s {
			m[i] = float32(v / float64(counts[c]))
		}
		out[c] = m
	}
	return out, true
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

// vecApproxTopK offers top the rows in each segment's nearest lists, at their
// exact distances. It reports false, having offered nothing, when a segment
// has no lists.
func vecApproxTopK(plan *segVectorPlan, segs []*segment, cells [][]uint64, skips [][]int, query []float32, qn float32, top *vecTopK) bool {
	a := plan.approx
	total := 0
	for _, s := range segs {
		total += s.nRows
	}
	if total == 0 {
		return false
	}
	ivfs := make([]*segVecIVF, len(segs))
	for si, s := range segs {
		if cells[si] == nil {
			continue
		}
		key := vecIVFKey{col: plan.col, l2: plan.l2, perList: a.perList, insertL: a.insertL}
		ivfs[si] = s.vecIVF(key, func() *segVecIVF {
			offs := make([]int, len(cells[si]))
			for i, c := range cells[si] {
				offs[i] = int(uint32(c))
			}
			return buildVecIVF(s.heap, offs, len(query), plan.l2, a)
		})
		if ivfs[si] == nil {
			return false
		}
	}
	want := max(a.searchL, plan.limit) * 32
	tops := make([]vecTopK, len(segs))
	if !segEachOn(vecWorkers, len(segs), func(si int) bool {
		ivf := ivfs[si]
		t := vecTopK{p: plan, h: make([]vecHit, 0, plan.limit)}
		if ivf == nil {
			tops[si] = t
			return true
		}
		// This segment's share of the rows to read, nearest lists first.
		share := max(plan.limit, int(int64(want)*int64(segs[si].nRows)/int64(total)))
		cd := make([]float32, len(ivf.cents))
		if plan.l2 {
			l2Tile(ivf.cents, query, cd)
		} else {
			cosTile(ivf.cents, query, qn, cd)
		}
		order := make([]int, len(cd))
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(x, y int) int {
			a, b := cd[x], cd[y]
			switch {
			case a != a || b != b:
				return boolCmp(a != a, b != b)
			case a < b:
				return -1
			case a > b:
				return 1
			}
			return 0
		})
		var rows []int
		for _, c := range order {
			for _, r := range ivf.lists[c] {
				if _, dead := slices.BinarySearch(skips[si], int(r)); !dead {
					rows = append(rows, int(r))
				}
			}
			if len(rows) >= share {
				break
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

// vecApproxServed counts searches the lists answered, for tests.
var vecApproxServed atomic.Int64

// VecApproxServedForTest reports and resets how many searches the
// approximate mode's lists answered.
func VecApproxServedForTest() int64 { return vecApproxServed.Swap(0) }

// boolCmp orders false before true.
func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}
