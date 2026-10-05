// This file compiles a compound SELECT ("SELECT ... (UNION [ALL] | INTERSECT |
// EXCEPT) SELECT ...") into a compoundProgram. Each arm compiles to its own
// Program through compileSubProgram -- the entry point any subquery body
// uses, so arms get scans, joins, aggregates and subqueries for free. The arms'
// rows are combined left to right by combineCompound, the one implementation
// of compound dedup and NULL-equal row equality. Only the compound-level ORDER
// BY/LIMIT/OFFSET lives here.
//
// stmt's own Columns/From/Where/... describe its first arm; each CompoundArm is
// an independent select-core. If any arm is outside compileSubProgram's scope,
// the whole compound declines (errVDBEUnsupported) -- no partial compilation.
//
// A compound used as a subquery body or nested in FROM arrives through
// compileSubProgramCompound, which threads the enclosing compile into every
// arm, so an arm may bind a correlated reference and the wrapper's Correlated
// is the OR over the arms. The top-level entry (tryVDBECompound) has no
// enclosing compile; when it is given an enclosing row it wraps it as a
// one-level compiler (rowOuterCompile) so an arm's unqualified correlated
// reference still resolves.
package engine

import (
	"sort"
)

// compoundProgram is a compiled compound SELECT: one standalone Program per
// arm (arms[0] is stmt's own first arm; arms[1:] are stmt.Compound, in
// order), the connective operator joining each arm to the accumulated result
// of the arms before it (ops[i] joins arms[i+1] onto arms[0..i] already
// combined -- len(ops) == len(arms)-1), and the compound-level ORDER BY/
// LIMIT/OFFSET, which apply to the COMBINED result only, never to an
// individual arm (SelectStmt.Compound's doc comment, sql_ast.go).
type compoundProgram struct {
	arms    []*Program
	ops     []CompoundOp
	orderBy []OrderTerm
	// orderByIdx[i] is the 0-based combined-result-column index orderBy[i]
	// sorts by, resolved once at compile time (compileCompound) against the
	// first arm's own columns/expressions so exec never has to re-resolve
	// (and so an expression term matching an arm expression, which needs
	// those expressions, is resolved where the pager/stmt are still in hand).
	orderByIdx []int
	// orderByColl[i] is the collating sequence orderBy[i] sorts by (see
	// resolveCompoundOrderCollation, sql_compound.go), resolved once here
	// for the same reason orderByIdx is: exec has no pager/stmt/arm-scope
	// left to re-derive it from.
	orderByColl []string
	// collations[i] is the collating sequence combineCompound's own
	// UNION/INTERSECT/EXCEPT dedup row-equality consults for output column
	// i (see compoundColumnCollations, sql_compound.go) -- resolved once
	// here (against the first arm's own output, still in hand) and reused
	// for every operator in the chain.
	collations []string
	limit      *int64
	offset     *int64
	// correlated is true when any ARM bound a column of a query enclosing the
	// compound (compileCompound threads its caller's compile scope into every
	// arm). It becomes the wrapper Program's Correlated flag, which is what
	// makes the caller re-run the whole compound per enclosing row instead of
	// caching one materialization -- see compileSubProgramCompound.
	correlated bool
}

// compileCompound compiles stmt (non-empty Compound) into a compoundProgram.
// Arms are grouped left to right -- stmt's own arm with
// Compound/OrderBy/Limit/Offset stripped, then each CompoundArm -- as SQLite
// parses them ("the individual selects always group from left to right",
// select.c:2932-2933), each compiled via compileSubProgram. Every arm must
// yield the same column count, checked at compile time. The compound ORDER BY
// is resolved here too (resolveCompoundOrderIndex) so an unsupported term
// errors at compile time. Dedup and ORDER BY are collation-aware
// (compoundColumnCollations, resolveCompoundOrderCollation).
//
// outer is the enclosing compile, threaded into every arm so a correlated
// reference binds, as C resolves a compound arm outward like any subquery
// body (select7.test: "... NOT EXISTS (SELECT t2.pk ... WHERE t2.fk = p.pk
// EXCEPT SELECT t3.pk ... WHERE t3.fk = p.pk ...)"). Each arm is a frame
// directly below outer's (the wrapper has no machine of its own), so an
// arm's OpOuterColumn level counts the same hops the runtime walks. nil outer
// leaves every arm uncorrelated.
func compileCompound(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler) (*compoundProgram, error) {
	firstCore := *stmt
	firstCore.Compound = nil
	firstCore.OrderBy = nil
	firstCore.Limit = nil
	firstCore.Offset = nil
	// See SelectStmt.r35dCompoundOf: this copy is private to the compile, so an
	// aggregate the arm's planner hands OUTWARD has to be recorded against the
	// node the enclosing select list actually holds -- the compound itself.
	firstCore.r35dCompoundOf = stmt

	// An arm's own plan decides its rows' arrival order, and only the
	// sequential UNION ALL keeps that order the way SQLite's multiSelect does
	// (select.c:2989-3003); see r38cFlattenCompoundArms. Any other compound keeps
	// its arms unflattened, as it did before the flattener reached arms.
	if pager != nil && !r41SequentialCompound(stmt) {
		pager.r41ArmsHeld++
		defer func() { pager.r41ArmsHeld-- }()
	}

	firstProg, err := compileSubProgram(pager, &firstCore, outer)
	if err != nil {
		return nil, err
	}

	// compoundOrderByCollateNameQuirk is a column-name-only C quirk that fires
	// only when the chain has a non-UNION-ALL operator: select.c:5534's
	// convertCompoundSelectToSubquery trigger ("p->op!=TK_ALL &&
	// p->op!=TK_SELECT"). When it fires, every unaliased item shaped "x
	// COLLATE y" is renamed through subqueryColumnNames -- the
	// sqlite3ColumnsFromExprList rule the synthetic "SELECT * FROM
	// (compound)" applies (select.c:5563-5579, 5902, 2262-2286). A pure
	// UNION ALL chain keeps its names and needs nothing.
	if compoundOrderByCollateNameQuirk(stmt, firstProg.ColNames) && compoundHasNonUnionAllOp(stmt) {
		firstProg.ColNames = pager.subqueryColumnNames(&firstCore, firstProg.ColNames)
	}

	cp := &compoundProgram{
		correlated: firstProg.Correlated,
		arms:       make([]*Program, 0, len(stmt.Compound)+1),
		ops:        make([]CompoundOp, 0, len(stmt.Compound)),
		orderBy:    stmt.OrderBy,
		limit:      stmt.Limit,
		offset:     stmt.Offset,
	}
	cp.arms = append(cp.arms, firstProg)

	for i, arm := range stmt.Compound {
		armProg, aerr := compileSubProgram(pager, arm.Stmt, outer)
		if aerr != nil {
			return nil, aerr
		}
		if armProg.NResultCol != firstProg.NResultCol && i < stmt.ValuesArms {
			// A row of a multi-row VALUES: sqlite3SelectWrongNumTermsError's
			// SF_Values wording (select.c:3076-3078).
			return nil, semanticf("all VALUES must have the same number of terms")
		}
		if armProg.NResultCol != firstProg.NResultCol {
			// semanticf and C's exact wording, counts and all: this is a
			// prepare-time rejection C SQLite makes too (see
			// subquery_validate.go, which already words it this way), so
			// it is the STATEMENT's error and must not arrive wrapped in
			// "compound SELECT not compilable to bytecode". C names no column
			// counts, so the "(%d vs %d)" suffix goes too.
			return nil, semanticf(
				"SELECTs to the left and right of %s do not have the same number of result columns",
				arm.Op)
		}
		cp.arms = append(cp.arms, armProg)
		cp.ops = append(cp.ops, arm.Op)
		cp.correlated = cp.correlated || armProg.Correlated
	}

	// arms carries every arm's select-list expressions and names so ORDER BY
	// terms resolve to result positions here, at compile time (exec keeps
	// only cp.orderByIdx), and so compoundColumnCollations/
	// resolveCompoundOrderCollation can find each position's collation.
	//
	// resolveAllArmOutputsOuter is a second, throwaway resolution of every
	// arm's FROM, run only for those facts; for an arm naming a view it
	// would bump pager.viewRefCount (countViewReference, C's nTabRef++,
	// select.c:6036-6044) a second time per reference. The counter is
	// snapshotted and restored around it, as resolveGroupSource does for
	// its columns-only walk, so view3.test's "SELECT * FROM v32768"
	// (32768 references) stays under the 65535 cap while the doubled
	// UNION form still exceeds it.
	//
	// pager can be nil: a write-path subquery with no snapshot reaches here
	// that way. Any arm with a FROM would already have declined in
	// resolveJoinSources, so every arm is FROM-less and no view is involved;
	// the snapshot/restore is skipped rather than dereferencing nil (a
	// generated column "y AS ((SELECT 1 UNION SELECT 2))" crashed here
	// before).
	var savedViewRefCount map[string]int
	if pager != nil {
		savedViewRefCount = make(map[string]int, len(pager.viewRefCount))
		for k, v := range pager.viewRefCount {
			savedViewRefCount[k] = v
		}
	}
	arms := pager.resolveAllArmOutputsOuter(stmt, outerSchemaCtx(pager, outer))
	if pager != nil {
		pager.viewRefCount = savedViewRefCount
	}

	cp.collations = compoundColumnCollations(arms, firstProg.NResultCol)

	cp.orderByIdx = make([]int, len(stmt.OrderBy))
	cp.orderByColl = make([]string, len(stmt.OrderBy))
	for i, ot := range stmt.OrderBy {
		idx, oerr := resolveCompoundOrderIndex(ot.Expr, firstProg.ColNames, arms, i+1)
		if oerr != nil {
			return nil, declineOrSemantic(oerr)
		}
		cp.orderByIdx[i] = idx
		cp.orderByColl[i] = resolveCompoundOrderCollation(arms, idx, ot)
	}

	return cp, nil
}

// outerSchemaCtx rebuilds the enclosing compile chain's FROM scopes as a
// schema-only evalCtx chain, so a compound arm's correlated column resolves
// for the collation rule exactly where compileColumn resolves it for the
// read. Without it, "SELECT a,(SELECT a EXCEPT SELECT 'ABC') FROM c1" over
// c1(a TEXT COLLATE NOCASE) deduped BINARY, where C's multiSelectCollSeq uses
// the arm expression's own sqlite3ExprCollSeq.
//
// The walk stops at the first enclosing compiler that resolves names through
// something a schema-only chain cannot model -- a register-backed row scope
// (regScopes: an upsert's "excluded", a CHECK/RETURNING row), which
// compileColumn consults before its cursor scopes. Stopping only loses an
// opinion (BINARY); continuing could attach the wrong collation.
//
// A trigger body's NEW/OLD are modelled: they are ordinary tableScopes over
// the trigger table (trigPseudoRowScopes), and a "new.a" is a TK_COLUMN that
// carries its declared collation into multiSelectCollSeq (select.c:2574-2589).
// So over s3(a TEXT COLLATE NOCASE) holding 'abc', "(SELECT new.a INTERSECT
// SELECT 'ABC')" in a trigger body is 'abc'. They are appended and the walk
// then stops.
//
// The aggregate item compiler's register scopes are the exception that can be
// modelled: they are the aggregate query's own FROM items over the anchor row
// (compileAggItemProgram), with the same tableScopes and collations a cursor
// level contributes. Stopping there would drop every enclosing opinion for an
// arm inside an aggregate item's subquery.
func outerSchemaCtx(pager *ReadOnlyPager, outer *compiler) *evalCtx {
	var head, tail *evalCtx
	for oc := outer; oc != nil; oc = oc.outer {
		// hasOpaqueRegScope, not any register scope: the sorted GROUP BY
		// drain publishes only mirrors of its own cursor scopes, and stopping
		// there dropped the enclosing column's collation, so
		// "group_concat((SELECT a EXCEPT SELECT 'ABC'))" over a NOCASE a
		// deduped BINARY on the drain while the hash path deduped NOCASE.
		if oc.trig != nil || (oc.hasOpaqueRegScope() && oc.aggRegs == nil) {
			if ps := trigPseudoRowScopes(oc.trig); len(ps) != 0 {
				ctx := &evalCtx{tables: ps, pager: pager}
				if tail == nil {
					head = ctx
				} else {
					tail.outer = ctx
				}
			}
			break
		}
		tables := make([]tableScope, len(oc.scopes))
		for i, s := range oc.scopes {
			tables[i] = s.tableScope
		}
		for _, rs := range oc.regScopes {
			// A mirror re-publishes a scope already in oc.scopes; adding it
			// again would show every one of its columns twice.
			if rs.scope != nil && !rs.mirrorsCursorScope {
				tables = append(tables, *rs.scope)
			}
		}
		ctx := &evalCtx{tables: tables, pager: pager}
		if tail == nil {
			head = ctx
		} else {
			tail.outer = ctx
		}
		tail = ctx
		// A LIVE enclosing-row scope (compiler.rowOuter) belongs at the TAIL of
		// the chain, and only on the OUTERMOST compile -- exactly where
		// compiler.affCtx puts it ("case c.outer != nil ... case c.rowOuter !=
		// nil", vdbe_codegen.go), so the collation this walk resolves for an arm
		// expression is the same one the arm's own comparison codegen resolves.
		// Without it an UNQUALIFIED correlated reference in an arm -- the only
		// spelling that reaches a compile at all (rewriteSelectOuterRefs
		// substitutes the qualified one) -- deduped BINARY: over
		// c1(a TEXT COLLATE NOCASE) holding 'abc', "UPDATE c1 SET b='HIT' WHERE
		// EXISTS (SELECT a INTERSECT SELECT 'ABC')" marks the row in C SQLite.
		if oc.outer == nil && oc.rowOuter != nil {
			tail.outer = oc.rowOuter
		}
	}
	return head
}

// exec runs every arm's sub-Program to completion -- uncorrelated, so no
// outer evalCtx is threaded through (exactly like the subquery opcodes'
// runSubOnce; see this file's package doc comment) -- and folds their rows
// together left to right via combineCompound (sql_compound.go), the SAME
// helper. The compound-level ORDER BY (resolved against the first arm's
// columns) and LIMIT/OFFSET are then applied to the combined result once.
func (cp *compoundProgram) exec(pager *ReadOnlyPager, params []Value) (cols []string, rows [][]Value, err error) {
	return cp.execFrom(pager, params, nil, nil, nil)
}

// execOuter is exec threaded with the LIVE enclosing row's evalCtx, for a
// compound reached as a correlated subquery's BODY (tryVDBECompound). Each arm gets
// it as its own m.outer -- the channel the aggregate opcodes resolve a
// correlated reference through (aggResult), which is
// what an arm like "SELECT count(*)+a" needs at RUN time even though
// compileColumn already baked the plain "a" in at compile time.
func (cp *compoundProgram) execOuter(pager *ReadOnlyPager, outer *evalCtx, params []Value) (cols []string, rows [][]Value, err error) {
	return cp.execFrom(pager, params, nil, outer, nil)
}

// execFrom is exec with an optional live parent frame: for a correlated body
// (cp.correlated), each arm runs via execWithParent so its OpOuterColumn reads
// the current outer row. parent nil is the uncorrelated path. outer is the
// live enclosing row (execOuter), set only on the parent-less path.
//
// from is the machine whose firing trigger row each parent-less arm must see:
// a live sub-program runs on a fresh machine carrying only pager and params,
// so without it an arm in a trigger body found an empty NEW/OLD. nil for
// ordinary callers.
func (cp *compoundProgram) execFrom(pager *ReadOnlyPager, params []Value, parent *vdbe, outer *evalCtx, from *vdbe) (cols []string, rows [][]Value, err error) {
	runArm := func(arm *Program) ([][]Value, error) {
		if parent != nil {
			return arm.execWithParentOn(parent, pager)
		}
		return arm.execOuterTrig(pager, outer, params, from)
	}
	cols = cp.arms[0].ColNames

	rows, err = runArm(cp.arms[0])
	if err != nil {
		return nil, nil, err
	}

	for i, op := range cp.ops {
		armRows, aerr := runArm(cp.arms[i+1])
		if aerr != nil {
			return nil, nil, aerr
		}
		// cp.collations[i] is column i's collating sequence (compile-time
		// resolved from the first arm's own output -- compoundColumnCollations,
		// sql_compound.go), consulted by combineCompound's own dedup
		// row-equality.
		rows, err = combineCompound(op, rows, armRows, cp.collations, pager.encoding())
		if err != nil {
			return nil, nil, err
		}
	}

	if len(cp.orderBy) > 0 {
		// keyIdx/keyColl were resolved once at compile time (cp.orderByIdx/
		// cp.orderByColl); exec never re-resolves -- an expression term needs
		// the arm's select-list expressions, which only compileCompound had.
		keyIdx := cp.orderByIdx
		keyColl := cp.orderByColl
		sort.SliceStable(rows, func(i, j int) bool {
			for k, ot := range cp.orderBy {
				less, equal := orderTermLess(ot, rows[i][keyIdx[k]], rows[j][keyIdx[k]], keyColl[k], pager.encoding())
				if equal {
					continue
				}
				return less
			}
			return false
		})
	}

	start, end := limitOffsetRange(len(rows), cp.limit, cp.offset)
	rows = rows[start:end]

	return cols, rows, nil
}
