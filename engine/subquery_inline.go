package engine

// A correlated scalar subquery, "(SELECT b.label FROM b WHERE b.id = t.bid)",
// ran its sub-program once per outer row on a machine of its own, as a
// correlated EXISTS did (exists_inline.go), and took the first of every row
// it produced. C codes it in the enclosing program (sqlite3CodeSubselect,
// expr.c) with a LIMIT 1: the destination starts NULL, and the first result
// row is copied into it and ends the subquery. subqueryInlinePeephole does the
// same: the OpSubquery becomes a Gosub to a relocated copy of the body.
//
// A subquery stays a sub-program when its first row is not provably C's
// (OrderUnproven: OpSubquery checks every row it got), when it answers every
// row packed (P3 < 0), and when its rows are kept per outer value
// (p5SubKeep) and some cursor it opens is read without a seek: a scan is
// worth running once per distinct value, which a subroutine does not do.

// subqueryInlinePeephole rewrites every correlated OpSubquery in prog it can.
func subqueryInlinePeephole(prog *Program) {
	if prog == nil {
		return
	}
	for i := range prog.Insns {
		op := prog.Insns[i]
		if op.Op != OpSubquery || op.P5&p5Correlated == 0 || op.P3 < 0 {
			continue
		}
		sub, ok := op.P4.(*Program)
		if !ok || sub.OrderUnproven || !bodyInlinable(sub) || op.P5&p5SubKeep != 0 && !seeksEveryCursor(sub) {
			continue
		}
		subqueryInline(prog, i, sub)
	}
}

// seeksEveryCursor reports whether every cursor sub opens is positioned by a
// seek rather than scanned.
func seeksEveryCursor(sub *Program) bool {
	seeked := map[int]bool{}
	for _, in := range sub.Insns {
		switch in.Op {
		case OpSeekRowidHint, OpSeekIndexHint, OpSeekRowidSetHint, OpSortedSeek:
			seeked[in.P1] = true
		}
	}
	for _, in := range sub.Insns {
		if (in.Op == OpOpenRead || in.Op == OpOpenDerived) && !seeked[in.P1] {
			return false
		}
	}
	return true
}

// subqueryInline replaces prog.Insns[at], a correlated OpSubquery over sub,
// with a Gosub into a relocated copy of sub appended to prog.
//
// The subroutine sets the destination to NULL and runs the body. Each of the
// body's result rows becomes a jump to a copy of that row into the
// destination, which closes the body's cursors and returns; the body's Halt,
// reached after its own Closes with no row, returns with the destination NULL.
func subqueryInline(prog *Program, at int, sub *Program) {
	call := prog.Insns[at]
	dest, width := call.P1, max(call.P3, 1)
	retReg := prog.NReg + sub.NReg

	base := len(prog.Insns)
	body := base + width // sub address a lands at body+a
	x := inlineReloc{regOff: prog.NReg, curOff: prog.NCursors, slotOff: prog.NSubCache, jump: func(a int) int { return body + a }}

	// Past the body: one copy per result row, then the exit that closes the
	// cursors, then the one the Halt takes.
	copyAt := map[int]int{}
	next := body + len(sub.Insns)
	for a, in := range sub.Insns {
		if in.Op == OpResultRow {
			copyAt[a] = next
			next += min(in.P2, width) + 1
		}
	}
	found := next
	done := found + sub.NCursors + 1

	out := prog.Insns
	for i := range width {
		out = append(out, Instruction{Op: OpNull, P2: dest + i})
	}
	for a, in := range sub.Insns {
		var r Instruction
		switch in.Op {
		case OpInit:
			r = Instruction{Op: OpGoto, P2: x.jump(in.P2)}
		case OpHalt:
			r = Instruction{Op: OpGoto, P2: done}
		case OpResultRow:
			r = Instruction{Op: OpGoto, P2: copyAt[a]}
		case OpOuterColumn, OpOuterRowid:
			r = x.outerRead(in)
		default:
			r = x.relocate(in)
		}
		out = append(out, r)
	}
	for _, in := range sub.Insns {
		if in.Op != OpResultRow {
			continue
		}
		for i := range min(in.P2, width) {
			out = append(out, Instruction{Op: OpSCopy, P1: x.reg(in.P1 + i), P2: dest + i})
		}
		out = append(out, Instruction{Op: OpGoto, P2: found})
	}
	for c := range sub.NCursors {
		out = append(out, Instruction{Op: OpClose, P1: x.cur(c)})
	}
	out = append(out, Instruction{Op: OpReturn, P1: retReg})
	out = append(out, Instruction{Op: OpReturn, P1: retReg})

	out[at] = Instruction{Op: OpGosub, P1: retReg, P2: base}
	prog.Insns = out
	prog.NReg += sub.NReg + 1
	prog.NCursors += sub.NCursors
	prog.NSubCache += sub.NSubCache
}
