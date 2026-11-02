package jit

import (
	"fmt"
	"slices"

	"github.com/samyfodil/musql/internal/fir"
)

// Register allocation for a fir kernel, shared by the backends.

// firHome is a value's home: a register index of its class, or a slot.
type firHome struct {
	reg   int // index into the allocatable registers of its class, or -1
	slot  int // spill slot, or -1
	isK   bool
	bits  uint64 // a constant's
	class fir.Class
}

// firUses is the values step st reads.
func firUses(st *fir.Step) []fir.Val {
	var out []fir.Val
	for _, v := range []fir.Val{st.A, st.B, st.C} {
		if v != 0 {
			out = append(out, v)
		}
	}
	return out
}

// firAllocate gives every value of f a home, from nGPR integer registers,
// nFP floating-point ones and spillN slots: a linear scan over the kernel's
// steps in emission order, which is a valid order for intervals because
// control flow only goes forward. A variable's interval is the hull of its
// sets and uses, so it keeps one home in every branch.
func firAllocate(f *fir.Func, nGPR, nFP, spillN int) ([]firHome, error) {
	nv := f.NVals()
	home := make([]firHome, nv)
	start, end := make([]int, nv), make([]int, nv)
	for i := range start {
		start[i], end[i] = -1, -1
		home[i] = firHome{reg: -1, slot: -1, class: f.Class[i]}
	}
	touch := func(v fir.Val, pos int) {
		if start[v] < 0 || pos < start[v] {
			start[v] = pos
		}
		end[v] = max(end[v], pos)
	}
	pos := 0
	var walk func([]fir.Step)
	walk = func(steps []fir.Step) {
		for i := range steps {
			st := &steps[i]
			pos++
			if st.Op == fir.OpConst {
				home[st.Dst].isK, home[st.Dst].bits = true, st.Imm
				continue
			}
			for _, v := range firUses(st) {
				touch(v, pos)
			}
			if st.Dst != 0 {
				touch(st.Dst, pos)
			}
			if st.Op == fir.OpIf {
				walk(st.Then)
				pos++
				walk(st.Else)
				pos++
			}
		}
	}
	walk(f.Body)
	// The step that defines each value, for the coalescing below.
	defAt := map[fir.Val]*fir.Step{}
	var defs func([]fir.Step)
	defs = func(steps []fir.Step) {
		for i := range steps {
			st := &steps[i]
			if st.Dst != 0 && !f.Var[st.Dst] {
				defAt[st.Dst] = st
			}
			defs(st.Then)
			defs(st.Else)
		}
	}
	defs(f.Body)

	order := make([]fir.Val, 0, nv)
	for v := 1; v < nv; v++ {
		if start[v] >= 0 && !home[v].isK {
			order = append(order, fir.Val(v))
		}
	}
	slices.SortStableFunc(order, func(x, y fir.Val) int { return start[x] - start[y] })
	type pool struct {
		free   []int
		active []fir.Val
	}
	pools := [2]*pool{{}, {}}
	for i := nFP - 1; i >= 0; i-- {
		pools[fir.F].free = append(pools[fir.F].free, i)
	}
	for i := nGPR - 1; i >= 0; i-- {
		pools[fir.I].free = append(pools[fir.I].free, i)
	}
	// A spill slot holds one value at a time: a value takes a slot none of
	// whose values' intervals meet its own. A value evicted from a register
	// lives in its slot for its whole interval, from before the eviction.
	var occupied [][]fir.Val
	spill := func(v fir.Val) error {
		for k, vs := range occupied {
			free := true
			for _, q := range vs {
				if start[q] <= end[v] && start[v] <= end[q] {
					free = false
					break
				}
			}
			if free {
				home[v].slot = k
				occupied[k] = append(occupied[k], v)
				return nil
			}
		}
		if len(occupied) >= spillN {
			return fmt.Errorf("jit: kernel %s needs more than %d spill slots", f.Name, spillN)
		}
		home[v].slot = len(occupied)
		occupied = append(occupied, []fir.Val{v})
		return nil
	}
	for _, v := range order {
		p := pools[f.Class[v]]
		live := p.active[:0]
		for _, q := range p.active {
			if end[q] < start[v] {
				p.free = append(p.free, home[q].reg)
			} else {
				live = append(live, q)
			}
		}
		p.active = live
		// A two-operand form writes its first operand's register: take it
		// when that operand dies here.
		if st := defAt[v]; st != nil && st.A != 0 && st.Op != fir.OpLoadF && st.Op != fir.OpLoadI {
			if q := st.A; !home[q].isK && f.Class[q] == f.Class[v] && end[q] == start[v] {
				if k := slices.Index(p.active, q); k >= 0 && home[q].reg >= 0 {
					home[v].reg = home[q].reg
					p.active[k] = v
					continue
				}
			}
		}
		if len(p.free) > 0 {
			home[v].reg = p.free[len(p.free)-1]
			p.free = p.free[:len(p.free)-1]
			p.active = append(p.active, v)
			continue
		}
		far := -1
		for i, q := range p.active {
			if far < 0 || end[q] > end[p.active[far]] {
				far = i
			}
		}
		if far >= 0 && end[p.active[far]] > end[v] {
			q := p.active[far]
			home[v].reg, home[q].reg = home[q].reg, -1
			p.active[far] = v
			if err := spill(q); err != nil {
				return nil, err
			}
			continue
		}
		if err := spill(v); err != nil {
			return nil, err
		}
	}
	return home, nil
}

// firEnds reports whether steps never fall through: they end in an Exit or a
// Return, or an If both of whose arms do.
func firEnds(steps []fir.Step) bool {
	if len(steps) == 0 {
		return false
	}
	switch st := steps[len(steps)-1]; st.Op {
	case fir.OpExit, fir.OpReturn:
		return true
	case fir.OpIf:
		return st.Else != nil && firEnds(st.Then) && firEnds(st.Else)
	}
	return false
}

// firHasOp reports whether op occurs in steps.
func firHasOp(steps []fir.Step, op fir.Op) bool {
	for _, st := range steps {
		if st.Op == op || firHasOp(st.Then, op) || firHasOp(st.Else, op) {
			return true
		}
	}
	return false
}
