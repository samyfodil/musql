package engine

// A correlated EXISTS runs its sub-program once per outer row, each run on a
// machine of its own: get one from the pool, open its cursor, run, scrub it,
// put it back. For "EXISTS (SELECT 1 FROM b WHERE b.id = t.bid ...)" that
// round trip was most of the statement, natively and far more on js/wasm.
//
// C SQLite codes the subquery INSIDE the enclosing program as a subroutine
// (sqlite3CodeSubselect, expr.c), so an outer row costs a Gosub, a seek and a
// Return. existsInlinePeephole does the same to a compiled program: the
// sub-program's instructions are relocated into the parent, after its last
// instruction, with registers and cursors renumbered past the parent's own,
// and the OpExists becomes a Gosub to them. It is a peephole over the finished
// program, like segPeephole, and it leaves anything it does not recognise
// untouched.
//
// What makes relocation safe is how little it accepts. Every instruction of
// the sub-program must be one of the opcodes listed in existsInlineOperands,
// whose every operand -- register, cursor, jump target or plain value -- is
// known exactly. Anything else (aggregates, sorters, nested subqueries, a
// row-filter plan, a live trigger body) keeps the per-row sub-program.
//
// The OpExists is replaced one-for-one by the Gosub and the subroutine is
// APPENDED, so no address in the parent moves: none of its jump targets,
// guards or plans need rewriting.

// existsInlinePeephole rewrites every correlated OpExists in prog it can.
func existsInlinePeephole(prog *Program) {
	if prog == nil {
		return
	}
	for i := range prog.Insns {
		op := prog.Insns[i]
		if op.Op != OpExists || op.P5&p5Correlated == 0 {
			continue
		}
		sub, ok := op.P4.(*Program)
		if !ok || !existsInlinable(sub) {
			continue
		}
		existsInline(prog, i, sub)
	}
}

// existsInlinable reports whether sub is a correlated EXISTS body this pass
// can relocate.
func existsInlinable(sub *Program) bool {
	if sub == nil || sub.Compound != nil || sub.LiveSource != nil ||
		sub.NSorters != 0 || sub.NDistinct != 0 || sub.NRecRegs != 0 || sub.NSubCache != 0 ||
		len(sub.Insns) == 0 || sub.Insns[0].Op != OpInit {
		return false
	}
	for _, in := range sub.Insns {
		if !existsInlineOperands(in) {
			return false
		}
		switch in.Op {
		case OpOuterColumn, OpOuterRowid:
			if in.P5 < 1 {
				return false
			}
		case OpRewind:
			if in.P4 != nil {
				return false // a row-filter plan: not read here, so not relocated
			}
		}
	}
	return true
}

// existsInlineOperands reports whether in is an opcode whose operands this
// pass knows: the whitelist relocate implements, operand by operand.
func existsInlineOperands(in Instruction) bool {
	switch in.Op {
	case OpInit, OpOpenRead, OpRewind, OpNext, OpClose, OpHalt, OpGoto,
		OpColumn, OpRowid, OpOuterColumn, OpOuterRowid,
		OpInteger, OpVariable, OpNull, OpString8, OpReal, OpSCopy, OpCopy,
		OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIf, OpIfNot, OpIsNull, OpNotNull,
		OpSeekRowidHint, OpSeekIndexHint, OpAutoIndexOrder, OpResultRow:
		return true
	}
	return false
}

// existsInline replaces prog.Insns[at], a correlated OpExists over sub, with a
// Gosub into a relocated copy of sub appended to prog.
//
// The subroutine sets the result to 0, runs the body, and leaves through one
// of two exits: a result row sets it to 1 and closes the body's cursors (the
// row is the answer; nothing more is read), and the body's Halt -- reached
// after its own Close, with no row -- leaves it 0. Either then applies NOT for
// NOT EXISTS and returns.
func existsInline(prog *Program, at int, sub *Program) {
	call := prog.Insns[at]
	dest, not := call.P1, call.P3 != 0
	regOff, curOff := prog.NReg, prog.NCursors
	retReg := prog.NReg + sub.NReg

	base := len(prog.Insns)
	body := base + 1               // sub address a lands at body+a
	found := body + len(sub.Insns) // the result-row exit
	nfound := 1 + sub.NCursors + 2 // Integer 1, the Closes, then Not? and Return
	if !not {
		nfound--
	}
	done := found + nfound // the no-row exit
	reg := func(r int) int { return r + regOff }
	cur := func(c int) int { return c + curOff }
	jump := func(a int) int { return body + a }

	out := append(prog.Insns, Instruction{Op: OpInteger, P1: 0, P2: dest})
	for _, in := range sub.Insns {
		r := in
		switch in.Op {
		case OpInit:
			r = Instruction{Op: OpGoto, P2: jump(in.P2)}
		case OpGoto:
			r.P2 = jump(in.P2)
		case OpHalt:
			r = Instruction{Op: OpGoto, P2: done}
		case OpResultRow:
			r = Instruction{Op: OpGoto, P2: found}
		case OpOpenRead, OpClose, OpAutoIndexOrder:
			r.P1 = cur(in.P1)
		case OpRewind, OpNext:
			r.P1, r.P2 = cur(in.P1), jump(in.P2)
		case OpColumn:
			r.P1, r.P3 = cur(in.P1), reg(in.P3)
		case OpRowid:
			r.P1, r.P2 = cur(in.P1), reg(in.P2)
		case OpOuterColumn:
			// One frame closer: level 1 is now this program's own cursor.
			if in.P5 == 1 {
				r = Instruction{Op: OpColumn, P1: in.P1, P2: in.P2, P3: reg(in.P3)}
			} else {
				r.P3, r.P5 = reg(in.P3), in.P5-1
			}
		case OpOuterRowid:
			if in.P5 == 1 {
				r = Instruction{Op: OpRowid, P1: in.P1, P2: reg(in.P2)}
			} else {
				r.P2, r.P5 = reg(in.P2), in.P5-1
			}
		case OpInteger, OpVariable, OpNull, OpString8, OpReal:
			r.P2 = reg(in.P2)
		case OpSCopy, OpCopy:
			r.P1, r.P2 = reg(in.P1), reg(in.P2)
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			r.P1, r.P3 = reg(in.P1), reg(in.P3)
			if in.P5&p5StoreP2 != 0 {
				r.P2 = reg(in.P2)
			} else {
				r.P2 = jump(in.P2)
			}
		case OpIf, OpIfNot, OpIsNull, OpNotNull:
			r.P1, r.P2 = reg(in.P1), jump(in.P2)
		case OpSeekRowidHint, OpSeekIndexHint:
			r.P1, r.P2 = cur(in.P1), reg(in.P2)
		}
		out = append(out, r)
	}
	// found: the answer is yes; close what the body left open.
	out = append(out, Instruction{Op: OpInteger, P1: 1, P2: dest})
	for c := 0; c < sub.NCursors; c++ {
		out = append(out, Instruction{Op: OpClose, P1: cur(c)})
	}
	if not {
		out = append(out, Instruction{Op: OpNot, P1: dest, P2: dest})
	}
	out = append(out, Instruction{Op: OpReturn, P1: retReg})
	// done: no row; the body closed its cursors before its Halt.
	if not {
		out = append(out, Instruction{Op: OpNot, P1: dest, P2: dest})
	}
	out = append(out, Instruction{Op: OpReturn, P1: retReg})

	out[at] = Instruction{Op: OpGosub, P1: retReg, P2: base}
	prog.Insns = out
	prog.NReg += sub.NReg + 1
	prog.NCursors += sub.NCursors
}
