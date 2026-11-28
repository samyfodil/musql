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
		if !ok || !bodyInlinable(sub) {
			continue
		}
		existsInline(prog, i, sub)
	}
}

// bodyInlinable reports whether sub, a correlated EXISTS or scalar subquery,
// is a body these passes can relocate.
func bodyInlinable(sub *Program) bool {
	if sub == nil || sub.Compound != nil || sub.LiveSource != nil ||
		sub.NSorters != 0 || sub.NDistinct != 0 || sub.NRecRegs != 0 ||
		len(sub.Insns) == 0 || sub.Insns[0].Op != OpInit {
		return false
	}
	for _, in := range sub.Insns {
		if !inlineOperands(in) {
			return false
		}
		switch in.Op {
		case OpOuterColumn, OpOuterRowid:
			if in.P5 < 1 {
				return false
			}
		case OpOpenDerived:
			if ds, ok := in.P4.(*derivedSource); !ok || !derivedReadsSameRows(ds) && !seriesFedByRegisters(ds) {
				return false
			}
		}
	}
	return true
}

// derivedReadsSameRows reports whether a derived source an inlined body opens
// reads the same rows wherever it runs: a CTE, which resolveCTERows reads
// without the outer row, or an uncorrelated subquery or recursive CTE
// materialized once a statement, each with a body that is deterministic. The
// parent's slot then keeps the rows across the body's runs, as each run of
// the sub-program would have found them again.
func derivedReadsSameRows(ds *derivedSource) bool {
	if ds.rerun || ds.vtab != nil || ds.recSelf != nil || ds.catalogScope != scopeAny || ds.vtabWrite != nil ||
		ds.upfrom != nil || ds.viewUpfrom != nil {
		return false
	}
	switch {
	case ds.cte != nil && ds.prog == nil:
		return !selectCallsNondeterministicFunc(ds.cte.binding.core)
	case ds.prog != nil && ds.cte == nil:
		return !ds.prog.Correlated && (ds.stmtOnce || ds.cteOnce) && progDeterministic(ds.prog, map[*Program]bool{})
	}
	return false
}

// seriesFedByRegisters reports whether ds is a correlated generate_series
// whose every argument and pushed WHERE term the body computes into a
// register before opening it (vtab_correlated.go): its rows are then a
// function of those registers alone, which relocate with the body, and no
// value of its constraints is folded against the outer row the body's own
// machine would have had.
func seriesFedByRegisters(ds *derivedSource) bool {
	if !isSeriesSource(ds) || !ds.vtabStream || len(ds.vtabCorr) == 0 || ds.rightJoin || ds.vtabTrig != nil ||
		ds.rerun || ds.catalogScope != scopeAny || ds.vtabWrite != nil || ds.upfrom != nil || ds.viewUpfrom != nil {
		return false
	}
	args := make([]bool, len(ds.vtab.TableFuncArgs))
	conjs := make([]bool, len(ds.vtab.tvfWhere))
	for _, t := range ds.vtabCorr {
		switch {
		case t.arg > 0 && t.arg <= len(args):
			args[t.arg-1] = true
		case t.arg == 0 && t.conj >= 0 && t.conj < len(conjs):
			conjs[t.conj] = true
		}
	}
	for _, fed := range append(args, conjs...) {
		if !fed {
			return false
		}
	}
	return true
}

// inlinedSource is ds as an inlined body opens it. A source that streams
// only while the program opening it yields (streamIfYielding) stays
// materialized: the body's own machine never yields, and the parent's may,
// which would run the source again at every rewind.
func inlinedSource(ds *derivedSource) *derivedSource {
	if ds.stream != streamIfYielding {
		return ds
	}
	cp := *ds
	cp.stream = streamNever
	return &cp
}

// progDeterministic reports whether prog, and every program it runs, calls
// no function that may answer differently on another run.
func progDeterministic(prog *Program, seen map[*Program]bool) bool {
	if seen[prog] {
		return true
	}
	seen[prog] = true
	for _, in := range prog.Insns {
		switch p4 := in.P4.(type) {
		case *Program:
			if !progDeterministic(p4, seen) {
				return false
			}
		case *derivedSource:
			if p4.prog != nil && !progDeterministic(p4.prog, seen) ||
				p4.cte != nil && selectCallsNondeterministicFunc(p4.cte.binding.core) {
				return false
			}
		case *ScalarFunction:
			if in.Op == OpFunction {
				return false
			}
		case string:
			if in.Op == OpFunction && nonConstantFuncs[r33sFoldIdent(p4)] {
				return false
			}
		}
	}
	return true
}

// existsInline replaces prog.Insns[at], a correlated OpExists over sub, with a
// Gosub into a relocated copy of sub appended to prog.
//
// The subroutine sets the result to "no row", runs the body, and leaves through
// one of two exits: a result row sets it to "a row" and closes the body's
// cursors (the row is the answer; nothing more is read), and the body's Halt --
// reached after its own Close, with no row -- leaves it as it was. NOT EXISTS
// just swaps the two constants, so no Not runs on either exit.
func existsInline(prog *Program, at int, sub *Program) {
	call := prog.Insns[at]
	dest, not := call.P1, call.P3 != 0
	regOff, curOff, slotOff := prog.NReg, prog.NCursors, prog.NSubCache
	retReg := prog.NReg + sub.NReg

	base := len(prog.Insns)
	body := base + 1                     // sub address a lands at body+a
	found := body + len(sub.Insns)       // the result-row exit
	done := found + 1 + sub.NCursors + 1 // past Integer, the Closes and Return: the no-row exit
	none, some := 0, 1
	if not {
		none, some = 1, 0
	}
	x := inlineReloc{regOff: regOff, curOff: curOff, slotOff: slotOff, jump: func(a int) int { return body + a }}

	out := append(prog.Insns, Instruction{Op: OpInteger, P1: none, P2: dest})
	for _, in := range sub.Insns {
		var r Instruction
		switch in.Op {
		case OpInit:
			r = Instruction{Op: OpGoto, P2: x.jump(in.P2)}
		case OpHalt:
			r = Instruction{Op: OpGoto, P2: done}
		case OpResultRow:
			r = Instruction{Op: OpGoto, P2: found}
		case OpOuterColumn, OpOuterRowid:
			r = x.outerRead(in)
		default:
			r = x.relocate(in)
		}
		out = append(out, r)
	}
	// found: the answer is yes; close what the body left open.
	out = append(out, Instruction{Op: OpInteger, P1: some, P2: dest})
	for c := 0; c < sub.NCursors; c++ {
		out = append(out, Instruction{Op: OpClose, P1: x.cur(c)})
	}
	out = append(out, Instruction{Op: OpReturn, P1: retReg})
	// done: no row; the body closed its cursors before its Halt.
	out = append(out, Instruction{Op: OpReturn, P1: retReg})

	out[at] = Instruction{Op: OpGosub, P1: retReg, P2: base}
	prog.Insns = out
	prog.NReg += sub.NReg + 1
	prog.NCursors += sub.NCursors
	prog.NSubCache += sub.NSubCache
}
