// A LIMIT or OFFSET whose value is not an integer is a RUN-TIME failure in real
// SQLite, never a prepare-time one -- and a subquery that is never stepped
// therefore never fails at all.
//
// computeLimitRegisters (select.c:2517) codes the clause into a register and
// emits OP_MustBeInt after it,
//
//	select.c:2548   sqlite3ExprCode(pParse, pLimit->pLeft, iLimit);
//	select.c:2549   sqlite3VdbeAddOp1(v, OP_MustBeInt, iLimit);
//	select.c:2556   sqlite3ExprCode(pParse, pLimit->pRight, iOffset);
//	select.c:2557   sqlite3VdbeAddOp1(v, OP_MustBeInt, iOffset);
//
// and OP_MustBeInt with a zero jump target raises SQLITE_MISMATCH
// ("datatype mismatch") when the machine reaches it (vdbe.c's OP_MustBeInt).
// The difference between "LIMIT 56.1" being an error or harmless depends on
// whether that instruction is ever executed.
//
// resolveSubProgramLimitOffset (vdbe_codegen.go) folds a subquery's clause at
// COMPILE time, which is right for the VALUE and wrong for the TIMING, and it
// hands a non-integer back to the codegen to CODE into a counter register
// instead -- C's own treatment, and the one that puts the failure back where C
// has it. A COMPOUND body is the shape that cannot take that offer: it has no
// instruction stream of its own at all (its arms ARE the frames -- see
// Program.Compound), so there is no register for a counter to live in.
//
// This file is that shape's answer, and it is the same answer expressed as a
// whole program rather than as two instructions inside a larger one: the body
// compiles normally for its RESULT SHAPE (column count and names, which the
// enclosing compile reads off it), and its instruction stream is then replaced
// by exactly what C emits ahead of a compound's own scan -- load the clause's
// value, OP_MustBeInt it, halt. Nothing else can run, because MustBeInt is the
// first thing that does.
package engine

// compileCompoundLimitMismatch compiles stmt -- a COMPOUND subquery body whose
// own LIMIT or OFFSET evaluates to a value OP_MustBeInt refuses -- into the
// program described in this file's comment, and reports whether it handled the
// statement at all.
//
// It answers (nil, false, nil) -- "not mine, carry on" -- for everything else,
// so the ordinary route below it is reached unchanged by every statement whose
// clause folds to an integer, has a bound parameter in it, or fails to evaluate
// for some other reason (that last one is a genuine compile-time decline, and
// it stays one).
//
// A constant-zero LIMIT jumps to the statement's end, skipping the OFFSET
// code, so "LIMIT 0 OFFSET <non-integer>" returns ZERO ROWS in C rather than
// a mismatch. This file does not close that gap.
func compileCompoundLimitMismatch(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler) (*Program, bool, error) {
	if pager == nil || len(stmt.Compound) == 0 {
		return nil, false, nil
	}
	if stmt.LimitParam == nil && stmt.OffsetParam == nil {
		return nil, false, nil
	}
	// A bound parameter's value is not knowable at this compile (see
	// resolveSubProgramLimitOffset), so it keeps its own decline.
	if exprHasParam(stmt.LimitParam) || exprHasParam(stmt.OffsetParam) {
		return nil, false, nil
	}
	// A LITERAL "LIMIT 0" never reaches limitOffsetMismatchValue's own
	// zero check, because parseLimitTerm's fast path puts it in stmt.Limit
	// rather than stmt.LimitParam. Same rule, same reason.
	if stmt.LimitParam == nil && stmt.Limit != nil && *stmt.Limit == 0 {
		return nil, false, nil
	}
	bad, isOffset, ok := limitOffsetMismatchValue(pager, stmt)
	if !ok {
		return nil, false, nil
	}
	// The body is compiled with the clause REMOVED, and only for the shape of
	// its result: NResultCol and ColNames are what the enclosing compile reads
	// off a sub-Program, and neither depends on a LIMIT. Its instructions are
	// discarded, so nothing this compile emits can run.
	origLimit, origLimitParam := stmt.Limit, stmt.LimitParam
	origOffset, origOffsetParam := stmt.Offset, stmt.OffsetParam
	stmt.Limit, stmt.LimitParam, stmt.Offset, stmt.OffsetParam = nil, nil, nil, nil
	prog, err := compileSubProgramCompound(pager, stmt, outer)
	stmt.Limit, stmt.LimitParam = origLimit, origLimitParam
	stmt.Offset, stmt.OffsetParam = origOffset, origOffsetParam
	if err != nil {
		// A decline of the body itself, which this file has no opinion about.
		// Returned as HANDLED so the caller reports it rather than folding the
		// clause again and reporting the mismatch instead.
		return nil, true, err
	}
	return limitOffsetMismatchProgram(prog, bad, isOffset), true, nil
}

// limitOffsetMismatchValue evaluates stmt's LIMIT and then its OFFSET -- C's
// order, select.c:2537 before select.c:2555 -- and returns the first value
// INTEGER affinity will not fold to an integer, which is precisely the value
// OP_MustBeInt would refuse.
//
// ok is false when there is no such value, which covers three different
// answers, all of them "leave this statement alone": every clause folds to an
// integer; a clause could not be evaluated here at all (a real decline, which
// the ordinary route reports with its own message); or the LIMIT is the integer
// zero, whose OFFSET C never evaluates -- see compileCompoundLimitMismatch's
// own doc comment.
func limitOffsetMismatchValue(pager *ReadOnlyPager, stmt *SelectStmt) (bad Value, isOffset bool, ok bool) {
	for _, term := range []struct {
		e        Expr
		isOffset bool
	}{{stmt.LimitParam, false}, {stmt.OffsetParam, true}} {
		if term.e == nil {
			continue
		}
		v, err := foldLimitOffsetValue(pager, term.e, nil)
		if err != nil {
			return Value{}, false, false
		}
		folded := applyAffinityToValue(v, affInteger)
		if folded.Typ != Int {
			return v, term.isOffset, true
		}
		if !term.isOffset && folded.I == 0 {
			return Value{}, false, false
		}
	}
	return Value{}, false, false
}

// foldLimitOffsetValue is foldLimitOffsetExpr (vdbe_codegen.go) stopping one
// step earlier: it returns the clause's VALUE instead of the integer count,
// because the caller needs the value itself to code back into the program it
// builds. See foldLimitOffsetExpr's doc comment for why the evaluation is a
// COMPILE and a run rather than a walk, and for the zeroed NameContext
// (resolve.c:1900-1908) re-imposed first.
func foldLimitOffsetValue(pager *ReadOnlyPager, e Expr, args []Value) (Value, error) {
	if err := requireZeroedNameContext(e); err != nil {
		return Value{}, err
	}
	prog, err := compileSelectNoFromPager(pager, &SelectStmt{Columns: []SelectColumn{{Expr: e}}}, nil)
	if err != nil {
		return Value{}, err
	}
	rows, err := prog.exec(pager, args)
	if err != nil {
		return Value{}, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return Value{}, errVDBEUnsupported
	}
	return rows[0][0], nil
}

// limitOffsetMismatchProgram returns prog with its whole body replaced by the
// three instructions C emits ahead of the scan for a clause it cannot read off
// the grammar: load the value, MustBeInt it, and (never reached) run on.
//
// Program.Compound is cleared along with the instructions, because a
// Compound-carrying Program is dispatched to its arms and never executes an
// instruction at all (Program.exec checks that field FIRST -- see its doc
// comment, vdbe_op.go). Everything the enclosing compile reads -- NResultCol,
// ColNames, Correlated -- is kept exactly as the body's own compile produced
// it.
//
// OpLimitCounter is this engine's OP_MustBeInt for this clause: it applies the
// same INTEGER affinity and raises the same "datatype mismatch" (see its own
// opcode comment and limitOffsetValueToCount, value_convert.go), so the message
// and the moment are identical to the ones this same body produces today when
// it IS stepped.
func limitOffsetMismatchProgram(prog *Program, bad Value, isOffset bool) *Program {
	out := *prog
	out.Compound = nil
	reg := prog.NReg
	out.NReg = reg + 1
	p2 := 0
	if isOffset {
		p2 = 1
	}
	out.Insns = []Instruction{
		{Op: OpInit, P2: 1},
		literalValueInstr(bad, reg),
		{Op: OpLimitCounter, P1: reg, P2: p2},
	}
	return &out
}

// literalValueInstr is compiler.compileLiteral's instruction choice
// (vdbe_codegen.go) without a compiler to allocate the register: the same
// storage-class-to-opcode mapping, for a caller that assembles a program
// directly rather than emitting into one.
func literalValueInstr(v Value, reg int) Instruction {
	switch v.Typ {
	case Int:
		return literalIntInstr(v.I, reg)
	case Float:
		return Instruction{Op: OpReal, P2: reg, P4: v.F}
	case Text:
		return Instruction{Op: OpString8, P2: reg, P4: string(v.S)}
	case Blob:
		return Instruction{Op: OpBlob, P2: reg, P4: append([]byte(nil), v.S...)}
	default:
		return Instruction{Op: OpNull, P2: reg}
	}
}
