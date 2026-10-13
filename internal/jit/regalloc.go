package jit

import (
	"slices"
	"sync/atomic"
)

// progAlloc is where each IR register of a program lives in its kernel: a
// machine register from the backend's pool, or its slot in the register file
// (ProgArgs.Regs), where every register lived before. Allocated registers are
// read from the file once on entry when the row loop carries them in, and
// written back once at the end when the caller reads them after (an
// accumulator's row counter) or a later call carries them on.
type progAlloc struct {
	slot  []int8 // per IR register: an index into the pool, or -1
	load  []int  // registers to read into their machine register on entry
	store []int  // registers to write back at the end
}

// at is r's pool index, or -1 for the register file.
func (pa *progAlloc) at(r int) int {
	if r < 0 || r >= len(pa.slot) {
		return -1
	}
	return int(pa.slot[r])
}

// The allocator's accounting (RegAllocStatsForTest): registers given a
// machine register, registers left in the file for want of one, and register
// copies the coalescing made into nothing.
var raAllocated, raSpilled, raCoalesced atomic.Int64

// RegAllocStatsForTest reads and clears the allocator's counts.
func RegAllocStatsForTest() (allocated, spilled, coalesced int64) {
	return raAllocated.Swap(0), raSpilled.Swap(0), raCoalesced.Swap(0)
}

// allocProgRegs is a linear scan over the program's liveness intervals.
//
// An IR register's interval is the hull of every instruction it is live
// after, read at, or written at. Jumps go only forward within a row, so the
// hull of those points is every place its value can be needed; a value the
// row loop carries round (live into the first instruction) or the caller
// reads after the last row is live everywhere. Two registers whose intervals
// do not overlap share a machine register. When more intervals overlap than
// the pool holds, the one ending furthest away stays in the file -- the
// classic choice: it would hold its machine register longest.
//
// Nothing is allocated when fusion is off (the unfused emission is the
// reference the fused one is tested against), when the program has a
// service (it reads any register, from the file, mid-row), or for a
// register a text step writes (those write the file themselves).
func allocProgRegs(insns []ProgInsn, pool int) progAlloc {
	var pa progAlloc
	if pool == 0 || !Fusing() || progHasService(insns) {
		return pa
	}
	nRegs := progRegs(insns)
	if nRegs == 0 {
		return pa
	}
	n := len(insns)
	liveOut, liveIn := progLivenessInOut(insns, nRegs)
	const none = -1
	start, end := make([]int, nRegs), make([]int, nRegs)
	for r := range start {
		start[r], end[r] = none, none
	}
	pinned := make([]bool, nRegs)
	touch := func(r, i int) {
		if start[r] == none || i < start[r] {
			start[r] = i
		}
		end[r] = max(end[r], i)
	}
	defined := make([]bool, nRegs)
	for i, in := range insns {
		st := stepOf(in)
		for _, r := range st.uses {
			touch(r, i)
		}
		for _, r := range st.defs {
			touch(r, i)
			defined[r] = true
		}
		if in.Op == POpTextLen || in.Op == POpTextMatch {
			pinned[in.A] = true
		}
		for r := 0; r < nRegs; r++ {
			if liveOut[i].has(r) {
				touch(r, i)
			}
		}
	}
	for r := 0; r < nRegs; r++ {
		if start[r] == none {
			continue
		}
		if liveIn[0].has(r) || liveIn[n].has(r) {
			// Carried round the loop, or read after it: live throughout.
			start[r], end[r] = 0, n
			if liveIn[0].has(r) {
				pa.load = append(pa.load, r)
			}
			if defined[r] {
				pa.store = append(pa.store, r)
			}
		}
	}
	order := make([]int, 0, nRegs)
	for r := 0; r < nRegs; r++ {
		if start[r] != none && !pinned[r] {
			order = append(order, r)
		}
	}
	slices.SortStableFunc(order, func(x, y int) int { return start[x] - start[y] })
	pa.slot = make([]int8, nRegs)
	for r := range pa.slot {
		pa.slot[r] = -1
	}
	free := make([]int, 0, pool)
	for p := pool - 1; p >= 0; p-- {
		free = append(free, p)
	}
	var active []int // allocated registers whose interval is still open
	spilled := 0
	for _, r := range order {
		// Expire every interval that ended before this one starts. An
		// interval ending AT this start still holds its register: the last
		// read of one and the write of the next can be the same instruction
		// (r = a + b), and the emitters read every operand first, but a
		// shared register there would still make two-operand forms alias.
		live := active[:0]
		for _, q := range active {
			if end[q] < start[r] {
				free = append(free, int(pa.slot[q]))
			} else {
				live = append(live, q)
			}
		}
		active = live
		// A copy whose source dies at it takes the source's register: the
		// copy is then no instruction at all.
		if i := start[r]; i < n && insns[i].Op == POpLoadReg && insns[i].A == r {
			q := insns[i].B
			if k := slices.Index(active, q); k >= 0 && end[q] == i {
				pa.slot[r] = pa.slot[q]
				active[k] = r
				raCoalesced.Add(1)
				continue
			}
		}
		if len(free) > 0 {
			pa.slot[r] = int8(free[len(free)-1])
			free = free[:len(free)-1]
			active = append(active, r)
			continue
		}
		// No register: whichever of r and the active ones ends furthest
		// stays in the file.
		far := -1
		for i, q := range active {
			if far < 0 || end[q] > end[active[far]] {
				far = i
			}
		}
		if far >= 0 && end[active[far]] > end[r] {
			q := active[far]
			pa.slot[r], pa.slot[q] = pa.slot[q], -1
			active[far] = r
		}
		spilled++
	}
	// The entry loads and exit stores are only for registers that got one.
	pa.load = slices.DeleteFunc(pa.load, func(r int) bool { return pa.slot[r] < 0 })
	pa.store = slices.DeleteFunc(pa.store, func(r int) bool { return pa.slot[r] < 0 })
	raAllocated.Add(int64(len(order) - spilled))
	raSpilled.Add(int64(spilled))
	return pa
}
