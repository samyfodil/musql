// This file compiles the ORDER BY half of a single-table scan
// (compileSelectScan, vdbe_scan.go, dispatches here whenever stmt.OrderBy is
// non-empty) into a two-phase VDBE program built on the sorter opcodes
// (vdbe_op.go/vdbe_sorter.go): SQLite's own canonical ORDER BY shape
// (src/vdbe.c) --
//
//	scan phase:  OpenRead; SorterOpen; Rewind->end; loop: WHERE check,
//	             compute output columns, compute ORDER BY key columns
//	             (reusing an already-computed output column for an
//	             ordinal/alias-resolved term -- see resolveOrderKeys),
//	             MakeRecord(key..,output..), SorterInsert; Next->loop;
//	             end: Close.
//	drain phase: SorterSort->end2; loop2: OFFSET/LIMIT checks, SorterData,
//	             RecordColumn per output column, ResultRow; SorterNext->
//	             loop2; end2: Halt.
//
// LIMIT/OFFSET are counted in the DRAIN phase ONLY -- never the scan -- since
// which rows survive LIMIT/OFFSET isn't knowable until every matching row has
// been seen, keyed, and sorted. That is a real semantic requirement here, not
// merely compileScanPlain's "evaluate then skip" optimization for a
// LIMIT-truncated unordered scan.
package engine


// orderKeySrc is one resolved ORDER BY term, decided ONCE, at compile time,
// rather than per row:
//
//   - outColIdx >= 0: an ordinal ("ORDER BY 2") or output-alias ("ORDER BY
//     b" where b names a select-list alias) reference -- the codegen loop
//     below reuses outCols[outColIdx]'s ALREADY-COMPUTED register rather
//     than re-evaluating the expression. That matters observably for a
//     non-deterministic or side-effecting output expression: it must be
//     evaluated exactly once per row, and the ORDER BY key must see that
//     SAME value.
//   - outColIdx == -1: a general expression (expr), which may reference the
//     table's own columns even when they aren't in the select list, and is
//     compiled fresh.
type orderKeySrc struct {
	outColIdx int
	expr      Expr
	desc      bool
	nulls     NullsOrder
}

// resolveOrderKeys statically resolves stmt.OrderBy against outCols: a bare
// (optionally signed) integer literal is a 1-based ordinal into outCols --
// out of range is a real ("ORDER BY term out of range") error, not merely
// "unsupported" (verified against C SQLite, tkt2822.test); a bare unqualified
// identifier matching an output column's name/alias resolves AS THAT OUTPUT
// COLUMN, taking precedence over resolving it as an ordinary expression
// against the table (so "SELECT a AS b FROM t ORDER BY b" sorts by a's
// values even if t also has an unrelated real column literally named "b");
// anything else is a general expression, checked with checkExprSupported
// up front (wrapped as errVDBEUnsupported, matching every other pre-flight
// check in vdbe_scan.go) -- actual compilation, which may still reference the
// table's own columns via compileColumn against the scan's scope, happens
// later, in compileScanSorted's codegen loop.
func resolveOrderKeys(stmt *SelectStmt, outCols []outputColumn) ([]orderKeySrc, error) {
	keys := make([]orderKeySrc, len(stmt.OrderBy))
	for i, ot := range stmt.OrderBy {
		// stripOrderCollate (query.go) lets an attached "COLLATE name" (or a
		// stack of them) pass through the ordinal/alias special-form checks
		// below unchanged -- verified directly against C SQLite that
		// "ORDER BY 1 COLLATE NOCASE"/"ORDER BY b COLLATE NOCASE" (b an
		// output alias) still resolve to that output column, with the
		// COLLATE governing its sort collation, exactly like sql_group.go's
		// GROUP BY-mode ORDER BY and sql_compound.go's compound ORDER BY
		// already do for the identical special forms. Without this, an
		// ordinal/alias term wrapped in COLLATE fell through to the general-
		// expression branch below instead, compiling as a CONSTANT (the
		// literal ordinal, or an unresolved bare name) rather than a
		// reference to the target output column -- silently sorting wrong
		// (effectively a no-op key) once compileExpr could compile a
		// CollateExpr at all. The collation itself is still recovered from
		// the ORIGINAL (unstripped) ot.Expr, by compileScanSorted's own
		// exprCollation call just after this function returns -- unaffected
		// by stripping here.
		stripped := stripOrderCollate(ot.Expr)
		if n, ok := orderByOrdinal(stripped); ok {
			if n < 1 || int(n) > len(outCols) {
				return nil, semanticf("%s ORDER BY term out of range - should be between 1 and %d",
					sqliteOrdinalWord(i+1), len(outCols))
			}
			keys[i] = orderKeySrc{outColIdx: int(n - 1), desc: ot.Desc, nulls: ot.Nulls}
			continue
		}
		if colRef, ok := stripped.(ColumnExpr); ok && colRef.Qualifier == "" {
			if idx := findOutputColByName(outCols, colRef.Name); idx >= 0 {
				keys[i] = orderKeySrc{outColIdx: idx, desc: ot.Desc, nulls: ot.Nulls}
				continue
			}
		}
		if err := checkExprSupported(ot.Expr); err != nil {
			return nil, declineOrSemantic(err)
		}
		keys[i] = orderKeySrc{outColIdx: -1, expr: ot.Expr, desc: ot.Desc, nulls: ot.Nulls}
	}
	return keys, nil
}

// compileScanSorted compiles the ORDER BY shape described in this file's
// package-level doc comment. c already has every source's cursor/scope
// installed (compileSelectScan); srcs/outCols are the shared setup it already
// computed; orderKeys is resolveOrderKeys' output. The scan phase's nested-
// loop skeleton (OpenRead/Rewind/Next/Close for every source, ON/LEFT/
// NullRow handling) is emitJoinLoops' (vdbe_join_codegen.go) concern -- this
// function supplies its innermost body (WHERE, output columns, ORDER BY key
// columns, MakeRecord, SorterInsert), now simply nested N levels deep instead
// of one; the drain phase below is unaffected by joins (it only ever touches
// the sorter, never a table cursor).
//
// DISTINCT dedups on the select-list OUTPUT columns only (never the ORDER BY
// key -- an ORDER BY term may reference an expression outside the select
// list, e.g. "SELECT DISTINCT a FROM t ORDER BY b"), and -- critically --
// which row's key SURVIVES for a group of output-duplicates is the FIRST one
// scanned (the first row's output and keys are kept together, then the kept
// keys sorted). This function reproduces that by evaluating the output AND the
// order-by key columns for every WHERE-matching row UNCONDITIONALLY (so any
// evaluation error surfaces identically regardless of eventual duplicate
// status), then gating only the MakeRecord[key..,output..]+SorterInsert step
// on distinctness: a duplicate's already-computed (and now discarded) key
// never reaches the sorter, so the first occurrence's key is the one that
// gets sorted.
func compileScanSorted(c *compiler, stmt *SelectStmt, srcs []joinSource, outCols []outputColumn, orderKeys []orderKeySrc) (*Program, error) {
	// Same bump/restore as compileScanPlain's identical guard, for the same
	// reason: every compileExpr call in this function (WHERE, the select
	// list, the ORDER BY keys) happens during the SCAN phase, which is inside
	// this query's own WHERE loop body -- the drain phase past the sort only
	// replays already-computed sorter records (this file's own doc comment:
	// "unaffected by joins... only ever touches the sorter"), so it compiles
	// no further expressions and needs no separate treatment. See
	// compiler.nQueryLoop/nQueryLoopKnown/planNRow.
	savedNQueryLoop, savedNQueryLoopKnown := c.nQueryLoop, c.nQueryLoopKnown
	if c.planNRowOK {
		c.nQueryLoop += c.planNRow
	} else {
		c.nQueryLoopKnown = false
	}
	defer func() { c.nQueryLoop, c.nQueryLoopKnown = savedNQueryLoop, savedNQueryLoopKnown }()

	// The select list and ORDER BY keys are evaluated during the scan, with
	// cursors positioned at the current row (the output phase past the sort only
	// reads materialized records), so a correlated subquery beneath this body
	// may read these cursors as its live outer source -- see compiler.rowLive.
	c.rowLive = true
	sorterNum := c.allocSorter()

	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)

	// Predicate pushdown: see compileScanPlain (vdbe_scan.go). buckets are
	// pruned at their join level; this body tests only the deferred residue.
	plan := buildJoinPlan(srcs, stmt.Where)
	// Index-driven inner-side seeks (see annotateJoinSeeks, vdbe_join_seek.go).
	// Safe here for the same reason as the plain scan -- and the final ORDER BY
	// sort re-imposes output order regardless -- so it only prunes the inner scan.
	annotateJoinSeeks(c, srcs, plan)
	deferredWhere := andConjuncts(plan.deferred)

	desc := make([]bool, len(orderKeys))
	nulls := make([]NullsOrder, len(orderKeys))
	coll := make([]string, len(orderKeys))
	for i, ok := range orderKeys {
		desc[i] = ok.desc
		nulls[i] = ok.nulls
		// Mirrors orderTermEffectiveCollation (query.go): an explicit COLLATE
		// on the ORIGINAL ORDER BY term always wins (though in practice one
		// never reaches here -- an explicit CollateExpr anywhere in a term's
		// own AST fails to compile at all, see compCollation's doc comment);
		// otherwise a bare column's own DECLARED collation, resolved through
		// the SAME ordinal/output-alias shortcut resolveOrderKeys itself
		// already applied to ok (outColIdx>=0 reuses that output column's own
		// expression, so the collation comes from the alias TARGET), else
		// BINARY.
		var target Expr
		if ok.outColIdx >= 0 {
			target = outCols[ok.outColIdx].expr
		} else {
			target = ok.expr
		}
		if n, exOk := exprCollation(stmt.OrderBy[i].Expr); exOk {
			coll[i] = n
		} else if n, declOk := topExprCollation(c.affCtx(), target); declOk {
			coll[i] = n
		} else {
			coll[i] = "BINARY"
		}
	}
	// Bounded top-N: when LIMIT is a literal, the drain below consumes only the
	// first OFFSET+LIMIT sorted rows, so the sorter need keep only that many (a
	// max-heap in insert) instead of every scanned row -- a large win for
	// ORDER BY ... LIMIT n over a big table. Left 0 (unbounded, full sort) when
	// there is no literal LIMIT, or the bound would overflow / isn't smaller than
	// a plausible row count. DISTINCT is fine: duplicates are dropped before
	// OpSorterInsert (below), so the sorter only ever sees rows that count toward
	// the bound. A runtime-parameter LIMIT (stmt.LimitParam, already resolved to a
	// literal by the caller when possible) that remains non-literal stays
	// unbounded.
	bound := 0
	if stmt.Limit != nil && *stmt.Limit > 0 && stmt.OffsetParam == nil {
		// stmt.OffsetParam != nil is the case that must stay UNBOUNDED even
		// though the LIMIT is a literal: the drain consumes OFFSET+LIMIT rows and
		// the OFFSET is not known until the program runs, so sizing the sorter
		// from the LIMIT alone throws away the rows the offset was going to skip
		// past. Measured against 3.53.3 with a trigger body
		// "INSERT INTO dst SELECT b FROM u ORDER BY b LIMIT 2 OFFSET new.c" over
		// u = 1,2,3,4 and new.c = 1: the oracle writes 2,3 and a sorter bounded
		// at 2 kept only 1,2 and wrote 2.
		off := int64(0)
		if stmt.Offset != nil && *stmt.Offset > 0 {
			off = *stmt.Offset
		}
		if sum := off + *stmt.Limit; sum > 0 && sum <= int64(^uint(0)>>1) {
			bound = int(sum)
		}
	}
	c.emit(Instruction{Op: OpSorterOpen, P1: sorterNum, P4: &sorterKeyInfo{nKey: len(orderKeys), desc: desc, nulls: nulls, coll: coll, bound: bound}})

	// Key registers (keyBase..keyBase+len(orderKeys)-1), then the output/
	// payload registers (resultBase..resultBase+len(outCols)-1) reserved
	// IMMEDIATELY afterward -- nothing else is allocated in between -- so
	// the two blocks are contiguous and OpMakeRecord can pack them as one
	// range starting at keyBase: SQLite's own canonical ORDER BY record
	// layout (sort key columns first, then the row's output columns).
	keyBase := c.allocN(len(orderKeys))
	resultBase := c.allocN(len(outCols))
	recReg := c.allocRec()

	var distinctNum, distinctRecReg int
	if stmt.Distinct {
		distinctNum = c.allocDistinct()
		distinctRecReg = c.allocRec()
		c.emit(Instruction{Op: OpDistinctOpen, P1: distinctNum, P4: distinctCollations(c, outCols)})
	}

	body := func() error {
		// WHERE: a false/NULL result skips this combination entirely,
		// exactly like compileScanPlain -- jumping straight past the rest of
		// this body (patched in once that address is known), before either
		// the output columns or the ORDER BY keys are computed.
		whereJump := -1
		if deferredWhere != nil {
			// See compileScanPlain's identical guard (vdbe_scan.go) and
			// compiler.inWhereConjunct's doc comment: deferredWhere is real
			// top-level WHERE AND-conjuncts, so tag-20220128a's WHERE-position
			// rule applies here too.
			c.inWhereConjunct = true
			wReg, werr := c.compileExpr(deferredWhere)
			if werr != nil {
				return werr
			}
			whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
		}

		// Output columns FIRST -- every ordinal/alias-resolved key below
		// reuses the value just computed here (see orderKeySrc's doc
		// comment) instead of re-evaluating the expression.
		for i, oc := range outCols {
			reg, cerr := c.compileExpr(oc.expr)
			if cerr != nil {
				return cerr
			}
			c.emit(Instruction{Op: OpSCopy, P1: reg, P2: resultBase + i})
		}
		for i, ok := range orderKeys {
			if ok.outColIdx >= 0 {
				c.emit(Instruction{Op: OpSCopy, P1: resultBase + ok.outColIdx, P2: keyBase + i})
				continue
			}
			reg, cerr := c.compileExpr(ok.expr)
			if cerr != nil {
				return cerr
			}
			c.emit(Instruction{Op: OpSCopy, P1: reg, P2: keyBase + i})
		}

		// DISTINCT: test the just-computed output columns against every row
		// already inserted; a duplicate skips straight to bodyEnd (see this
		// function's doc comment) -- its key, computed above, is simply
		// discarded, never reaching the sorter.
		distinctJump := -1
		if stmt.Distinct {
			c.emit(Instruction{Op: OpMakeRecord, P1: resultBase, P2: len(outCols), P3: distinctRecReg})
			distinctJump = c.emit(Instruction{Op: OpDistinct, P1: distinctNum, P3: distinctRecReg})
		}

		// Bounded top-N only: a row whose key cannot make the cut is rejected
		// HERE, before its record is built, exactly as pushOntoSorter tests the
		// LIMIT counter and the largest held key before reaching
		// makeSorterRecord (select.c:832-859). The record is the expensive half
		// -- OpMakeRecord deep-copies every key and payload value into the
		// record arena -- and for "SELECT id, v FROM t ORDER BY v DESC LIMIT 20"
		// over 100k rows it was ~99% of the query's allocated bytes, spent
		// building 100,000 records to keep 20. Measured engine-direct
		// (BenchmarkOrderByLimit20): 16.39 MB/op and 1,346 allocs/op became
		// 0.46 MB/op and 172. Placed AFTER the DISTINCT probe, never before: OpDistinct
		// REMEMBERS the row it just tested, so skipping it for a losing row
		// would let a later duplicate -- with a different ORDER BY key, since a
		// key may reference a non-output column -- reach the sorter in its place.
		sorterCheckJump := -1
		if bound > 0 {
			sorterCheckJump = c.emit(Instruction{Op: OpSorterCheck, P1: sorterNum, P3: keyBase})
		}
		c.emit(Instruction{Op: OpMakeRecord, P1: keyBase, P2: len(orderKeys) + len(outCols), P3: recReg})
		c.emit(Instruction{Op: OpSorterInsert, P1: sorterNum, P2: recReg})

		bodyEnd := c.here()
		if whereJump >= 0 {
			c.patch(whereJump, bodyEnd)
		}
		if distinctJump >= 0 {
			c.patch(distinctJump, bodyEnd)
		}
		if sorterCheckJump >= 0 {
			c.patch(sorterCheckJump, bodyEnd)
		}
		return nil
	}

	if err := emitJoinLoops(c, srcs, plan, body); err != nil {
		return nil, err
	}

	// Drain phase. LIMIT/OFFSET counters are established here -- AFTER the
	// scan that fills the sorter -- and counted against the SORTED output,
	// never the scan above (see this file's package-level doc comment).
	var offsetReg, limitReg, oneReg int
	haveOffset, haveLimit := false, false
	// An UNRESOLVED clause (LimitParam/OffsetParam) is CODED into the same
	// counter register a literal would have gone into -- computeLimitRegisters'
	// only treatment of a non-integer-literal clause (select.c:2547-2558), and
	// what this engine already does on the unsorted path (compileScanPlain).
	// LIMIT is coded before OFFSET, matching C's order, so two expressions
	// evaluate in the order 3.53.3 evaluates them, and OpLimitCounter applies
	// OP_MustBeInt to each.
	//
	// "LIMIT ?" is the reason this arm is here at all: execSelect deliberately
	// does NOT fold a clause holding a bound parameter, because the plan cache is
	// keyed on SQL TEXT and a folded value would make the second execution of a
	// prepared statement reuse the first one's limit (see query.go's own comment
	// for the measured wrong answer). A paginating application's statement is
	// ORDER BY ... LIMIT ? OFFSET ?, so it lands here rather than on the plain
	// scan.
	if stmt.LimitParam != nil {
		r, lerr := c.emitLimitOffsetReg(stmt.LimitParam, false)
		if lerr != nil {
			return nil, lerr
		}
		haveLimit, limitReg = true, r
	}
	if stmt.OffsetParam != nil {
		r, oerr := c.emitLimitOffsetReg(stmt.OffsetParam, true)
		if oerr != nil {
			return nil, oerr
		}
		haveOffset, offsetReg = true, r
	}
	if !haveOffset && stmt.Offset != nil {
		off := *stmt.Offset
		if off > 0 {
			haveOffset = true
			offsetReg = c.alloc()
			c.emit(literalIntInstr(off, offsetReg))
		}
	}
	if !haveLimit && stmt.Limit != nil && *stmt.Limit >= 0 {
		haveLimit = true
		limitReg = c.alloc()
		c.emit(literalIntInstr(*stmt.Limit, limitReg))
	}
	if haveOffset || haveLimit {
		oneReg = c.alloc()
		c.emit(literalIntInstr(1, oneReg))
	}

	drainRecReg := c.allocRec()
	outBase := c.allocN(len(outCols))

	sortJump := c.emit(Instruction{Op: OpSorterSort, P1: sorterNum})
	drainLoop := c.here()

	// OFFSET: skip this (sorted) row while offsetReg is still positive --
	// decrement and jump straight to OpSorterNext, the same shape as
	// compileScanPlain's OFFSET skip, just over the drained (sorted) order
	// instead of raw scan order.
	offGotoNext := -1
	if haveOffset {
		offHas := c.emit(Instruction{Op: OpIf, P1: offsetReg}) // offsetReg != 0 -> skip block
		offPast := c.emit(Instruction{Op: OpGoto})             // offsetReg == 0 -> past the skip, to LIMIT/output
		c.patch(offHas, c.here())
		c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: offsetReg, P3: offsetReg})
		offGotoNext = c.emit(Instruction{Op: OpGoto}) // -> OpSorterNext, patched below
		c.patch(offPast, c.here())
	}

	// LIMIT: once limitReg has reached 0, every remaining (sorted) row is
	// beyond the requested count -- stop draining immediately by jumping to
	// `drainEnd`, patched in once known.
	limExhausted := -1
	if haveLimit {
		limHas := c.emit(Instruction{Op: OpIf, P1: limitReg}) // limitReg != 0 -> continue to output
		limExhausted = c.emit(Instruction{Op: OpGoto})        // limitReg == 0 -> drainEnd, patched below
		c.patch(limHas, c.here())
	}

	c.emit(Instruction{Op: OpSorterData, P1: sorterNum, P2: drainRecReg})
	for i := range outCols {
		c.emit(Instruction{Op: OpRecordColumn, P1: drainRecReg, P2: i, P3: outBase + i})
	}
	c.emit(Instruction{Op: OpResultRow, P1: outBase, P2: len(outCols)})
	if haveLimit {
		c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: limitReg, P3: limitReg})
	}

	sorterNextAddr := c.emit(Instruction{Op: OpSorterNext, P1: sorterNum, P2: drainLoop})
	if offGotoNext >= 0 {
		c.patch(offGotoNext, sorterNextAddr)
	}

	drainEnd := c.here()
	c.patch(sortJump, drainEnd)
	if limExhausted >= 0 {
		c.patch(limExhausted, drainEnd)
	}
	c.emit(Instruction{Op: OpHalt})

	cols := make([]string, len(outCols))
	for i, oc := range outCols {
		cols[i] = oc.name
	}
	return &Program{
		Insns:      c.insns,
		NReg:       c.nReg,
		NCursors:   c.nCursor,
		NRecRegs:   c.nRec,
		NSorters:   c.nSorter,
		NSubCache:  c.nSub,
		NDistinct:  c.nDistinct,
		NResultCol: len(outCols),
		ColNames:   cols,
		Correlated: c.correlated,
	}, nil
}
