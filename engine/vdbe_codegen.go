// Expression compilation into a VDBE Program (vdbe_op.go), and the FROM-less
// SELECT compiler. Anything the compiler cannot lower is a "vdbe: unsupported"
// error; there is no fallback.
//
// Registers are allocated monotonically and never reused, which keeps the
// in-place-mutating opcodes (OpCast, comparison affinity coercion) trivially
// safe at the cost of a larger register file.
package engine

import (
	"errors"
	"fmt"
	"math"
)

// compiler accumulates a Program under construction. scopes is empty for a
// FROM-less program; a cursor-driven compile (compileSelectScan) has one
// compileScope per open cursor in FROM order, against which compileColumn
// resolves column references with resolveColumnEx's qualifier, ambiguity and
// rowid-shadowing rules.
// onceSlots is one upsert clause's shared once-subquery slots: the first
// tuple's base and how many it allocated (compiler.upsertOnceSub).
type onceSlots struct{ base, count int }

type compiler struct {
	// pureFuncP5 is stamped onto every OpFunction this compiler emits: real
	// SQLite's OP_PureFunc, naming the schema expression being compiled when
	// there is one (pureCtxCheck / pureCtxGenerated / pureCtxIndex). Zero for
	// an ordinary statement, which is every compiler but the ones
	// compileSelfRowExpr builds and the CHECK emission in
	// emitCheckConstraintsAction.
	pureFuncP5 uint16

	insns     []Instruction
	nReg      int
	nCursor   int
	nRec      int // record registers allocated so far (rr[]; see the sorter opcodes, vdbe_op.go)
	nSorter   int // sorter cursors allocated so far
	nSub      int // run-once subquery cache slots allocated so far (subCache[]; see the subquery opcodes, vdbe_op.go)
	nDistinct int // distinct-set trackers allocated so far (see the DISTINCT opcodes, vdbe_op.go)
	scopes    []compileScope

	// returningOnceSubBase/Set hold the sub-cache slot base the first tuple of an
	// unrolled "INSERT ... VALUES ... RETURNING" allocated, so later tuples reuse the
	// same slots: this engine's OP_Once for a RETURNING subquery (C never unrolls).
	// Set is separate because 0 is a valid base. Indexed by once group: an upsert's
	// two arms are two codings in C, each with its own OP_Once.
	returningOnceSubBase  [returningOnceGroups]int
	returningOnceSubCount [returningOnceGroups]int
	returningOnceSubSet   [returningOnceGroups]bool

	// upsertOnceSub is the same for an ON CONFLICT DO UPDATE SET list: C codes the
	// block once (upsert.c:325), so its OP_Once fires once per statement, while
	// emitUpsertTail runs per unrolled tuple. A slot count, not a high-water mark,
	// since a later tuple's VALUES may allocate slots first. Keyed by ON CONFLICT
	// clause. ponytail: C codes a clause once per constraint it covers
	// (insert.c:2363, 2589), so a target-less DO UPDATE hitting two constraints gets
	// two OP_Onces there and shares one here.
	upsertOnceSub map[int]onceSlots

	// beforeInsertRowid holds the rowid a BEFORE INSERT trigger sees: C allocates
	// an auto rowid after the BEFORE program, so NEW.rowid is -1 unless the rowid
	// was supplied explicitly (triggerD.test 2.x). Set by emitInsertRowBody; 0 means
	// use the ordinary rowid register.
	beforeInsertRowid int

	// insertBeforeFire, when non-nil, emits the BEFORE program where sqlite3Insert
	// fires it (insert.c:1494): after NEW.* is built and coerced, before rowid
	// allocation (insert.c:1539) and constraint checks (insert.c:1569). Set per row
	// by the INSERT compilers and consumed by emitInsertRowBody; nil emits nothing.
	insertBeforeFire func() error

	// trig, when non-nil, resolves NEW.*/OLD.* column references inside a
	// trigger body sub-program to OpParam reads of the firing row (see
	// compileTriggerBodyStmt). It is consulted before regScopes and the cursor
	// scopes.
	trig *trigCompileCtx

	// excl, when non-nil, is the upsert "excluded" pseudo-row in force for the
	// code being compiled -- an ON CONFLICT DO UPDATE's SET list. It is read
	// through the WHOLE outer chain (enclosingExcl), not off this compiler
	// alone, so a SUBQUERY written in a SET right-hand side can still name
	// excluded.<col>; the DO UPDATE compile itself never reaches it, because
	// its own register scope (regScopes) answers first. See
	// compiler.emitExcludedParam.
	excl *exclCompileCtx

	// retRow, when non-nil, is the RETURNING clause's AFFECTED ROW -- the same
	// idea as excl, for the block emitReturning compiles. It is C's NC_UBaseReg
	// (resolve.c:528-536, trigger.c:1074-1075), and like excl it is needed only
	// by a SUBQUERY written in the RETURNING list, whose own program cannot read
	// the register block the list itself resolves against. Unlike excl it is
	// carried on the SUB-COMPILER directly as well, because a RETURNING subquery
	// is lowered LIVE with no enclosing compiler at all (trigOnlyOuter,
	// vdbe_live_read.go). See compiler.emitReturningParam.
	retRow *retRowCompileCtx

	// retForcePerRow says that every subquery in the RETURNING block about to
	// be emitted names the thing being modified, so emitReturning must treat
	// them as C's PER-ROW lifetime whatever the insn walk concluded. Set only
	// by emitViewReturning (vdbe_view_write.go): a subquery over a VIEW opens
	// the view's BASE tables and never the view itself, so the walk -- which
	// matches an OpOpenRead against the target's NAME -- cannot tell C's two
	// lifetimes apart there. See returningSubqueryLifetimes.
	retForcePerRow bool

	// regScopes is a stack of REGISTER-backed row scopes: a column reference is
	// resolved against these (a register per column, plus a rowid register)
	// BEFORE the cursor scopes. This is how SQLite compiles a CHECK constraint,
	// a RETURNING list, an upsert's DO UPDATE SET (the conflicting row plus the
	// "excluded" pseudo-table), and a trigger's NEW.*/OLD.* -- all read a row
	// that lives in registers, not a cursor. Innermost (last) scope wins.
	regScopes []regScope

	// drain is the aggregate sorted-drain row block currently pushed, if any.
	// compileColumn needs it for the one shape a regScope cannot express: see
	// aggDrainRow.push.
	drain *aggDrainRow

	// stripRowSubtype makes every column read in this compile drop its JSON subtype
	// (OpClearSubtype after the read). Set only for a hash-grouped aggregate's
	// per-row argument registers (emitAggArgRegs), where C reads the value out of a
	// sorter record that has already lost the subtype (select.c:8601,
	// vdbeaux.c:4123). Not set for WHERE or the GROUP BY key, which C codes before
	// the sorter. A sub-program has its own flag; only a correlated reference back
	// into this frame is stripped (stripsRowSubtypeAt).
	stripRowSubtype bool

	// regScopeStrict makes resolveRowReg reject a name offered by more than one
	// register scope ("ambiguous column name", resolve.c:784) instead of taking
	// the innermost. Set for a window projection over a join and an aggregate
	// item's anchor row. Opt-in because the upsert DO UPDATE compile relies on
	// innermost-wins (a target named "excluded", upsert3.test).
	regScopeStrict bool

	// ignoreDbQualifier drops the database part of "schema.table.column" without
	// validating it, as C does for CHECK bodies and partial-index WHERE only
	// (resolve.c:313, NC_IsCheck|NC_PartIdx); not for index key expressions or
	// generated columns. The table part is still resolved.
	ignoreDbQualifier bool

	// winRegs[i] holds window call i's computed result in a window query's outer
	// projection: C's pWin->regResult (expr.c:5358). nil elsewhere, so a
	// windowResultExpr is an error in any other compile.
	winRegs []int

	// bufOut, when non-nil, makes a column reference this compile cannot
	// resolve out of its own register block a BUFFERED batch column instead of
	// a decline -- the one compile that has such a block, a window query's
	// outer projection (compileWindowRowProgram, vdbe_window_codegen.go). See
	// windowBufCols, which is also where the affinity/collation scope chain
	// affCtx appends for those references lives.
	bufOut *windowBufCols

	// aggRegs is the same idea for an aggregate query's RESULT phase: the
	// registers holding one group's finalized accumulators, its GROUP BY key
	// tuple and its anchor row, which is what the three rewritten-tree
	// placeholders resolve to. C does exactly this and nothing else --
	// sqlite3ExprCodeTarget returns AggInfoFuncReg / AggInfoColumnReg for
	// TK_AGG_FUNCTION and TK_AGG_COLUMN (expr.c:5342, expr.c:4996) without
	// emitting an instruction. Set only by compileAggItemProgram
	// (vdbe_agg_item_codegen.go); nil everywhere else, for winRegs' reason.
	aggRegs *aggResultRegs

	// aggSkipped is the aggregate SCAN compiler this ITEM compiler was hung
	// past: compileAggItemProgram sets outer to enclosing.OUTER, exactly as
	// aggItemVM points the item's frame at m.parent, so the two chains stay
	// one-to-one. The scan compiler is off the chain but still owns the body
	// sub-Program's Correlated flag, so markAggCorrelated reaches it through
	// here (aggResultReg, vdbe_agg_item_codegen.go).
	aggSkipped *compiler

	// liveDB, when non-nil, makes every subquery this compile emits live: lowered
	// now (proving it lowers) and again at run time against the current database.
	// Set for a trigger's WHEN guard and body statements; elsewhere the
	// pre-statement snapshot is the semantics (writeSubqueryPager). See
	// vdbe_live_read.go.
	liveDB *DB

	// liveRow, when non-nil, lowers every subquery this compile emits as a
	// live one correlated to the UPDATE row -- see liveRowCtx
	// (vdbe_live_read.go). Set only around one SET expression.
	liveRow *liveRowCtx

	// writeTbl/writeAlias are the table an UPDATE or DELETE compile scans on
	// cursor 0, set by newWriteScanCompilerAs, so a TRIGGER BODY statement's
	// correlated subquery can be lowered live against that same row (see
	// liveSubSelect).
	writeTbl   *tableMeta
	writeAlias string

	// inWhere is set by whereSpan around an UPDATE's or DELETE's own WHERE,
	// and bodySetLive around a trigger-body UPDATE's SET list when its row
	// loop visits rows in C's order -- see liveSubSelect.
	inWhere     bool
	bodySetLive bool

	// onePassPlan asks the ported planner for an UPDATE's own scan
	// (WHERE_ONEPASS_DESIRED) rather than a SELECT's. See DB.updateOnePassOrder.
	onePassPlan bool
	// planWinner, when set, is where the ported planner reports what its
	// single-table plan's winning loop is -- see wherePlanWinner.
	planWinner *wherePlanWinner

	// outer is the enclosing query's compiler when this compiler is building a
	// subquery's body, or nil for a top-level compile. compileColumn walks this
	// chain outward (mirroring evalCtx.outer's own chain, column_scope.go) to
	// resolve a CORRELATED column reference -- one that names a table/cursor of
	// an enclosing query -- emitting an OpOuterColumn/OpOuterRowid against that
	// ancestor's still-live cursor. It is threaded in by compileSubProgram.
	outer *compiler

	// rowOuter, when non-nil, is a live enclosing-row evalCtx (a trigger body or
	// write clause row), from tryVDBENoFrom or rowOuterCompile. It is compileColumn's
	// last resort after this compile's scopes and the outer compiler chain, and
	// resolves through resolveColumnEx like a row read. The value is baked in as a
	// literal, which is sound because such compiles are fresh per execution and
	// never plan-cached.
	rowOuter *evalCtx

	// rowLive reports whether this compiler's cursors hold the correlated row when a
	// subquery compiled beneath it runs, i.e. whether a correlated subquery may read
	// them. True for a row-mode scan body; false for an aggregate's result phase or
	// a FROM-less compile, where compileColumn declines instead.
	rowLive bool

	// inWhereConjunct is true only while compiling a direct top-level AND conjunct
	// of this query's WHERE, mirroring the "pWC->op==TK_AND" guard on the
	// vector-equality rewrite (whereexpr.c:1467). compileExpr reads and clears it;
	// only compileAndOr re-arms it for its operands. Used by compileRowSubCompare
	// to pick the per-field comparison or the general vector comparison.
	inWhereConjunct bool

	// correlated records that this compiler's program (or a subquery nested
	// within it) bound at least one column of an ENCLOSING scope -- set by
	// compileColumn when it resolves a reference to an outer compiler, for every
	// compiler on the chain from the referrer up to (not including) the owning
	// outer one. It becomes the sub-Program's Program.Correlated, which the
	// enclosing compiler reads to choose the run-once (uncorrelated) vs
	// re-run-per-evaluation (correlated) subquery opcode form.
	correlated bool

	// pager is the open database this compile resolves table references
	// against. It is set for a cursor-driven compile (compileSelectScan) and
	// for any subquery compiled beneath one (compileSubProgram threads it in),
	// and nil for a top-level FROM-less compile (compileSelectNoFrom). A
	// compiled subquery (SubqueryExpr/ExistsExpr/InExpr.Sub) requires a
	// non-nil pager -- so subqueries are supported only within a cursor-driven
	// scope, and a top-level FROM-less SELECT containing one declines.
	pager *ReadOnlyPager

	// ownSchema, when non-empty, is the NAME this compile's write session
	// answers to (DB.localSchema through localSchemaOr). It carries no handle
	// and no liveness -- deliberately, since the only question it answers is
	// compileColumn's "does this three-part reference's database qualifier
	// name the database I am writing", and that needs a name, not a pager. A
	// plain write (no subquery anywhere) never opens the writeSubqueryPager,
	// so c.pager is legitimately nil there and this is the only thing left to
	// decide the qualifier with.
	ownSchema string

	// fts3MatchGood is the set of scopes (index into c.scopes) whose fts3/4 MATCH C
	// can hand to the module as a constraint (fts3MatchBindings). checkFts3Match
	// refuses any other, as C does. Set only by compileSelectScan. The value is the
	// scope's single MatchExpr, or nil for an OR; read by compileFts3Aux.
	fts3MatchGood map[int]*MatchExpr

	// fts3ContentUnneeded marks bound scopes whose %_content existence check may be
	// skipped (fts3StmtContentUnneeded). Absent means false. The read side's copy
	// is FromItem.fts3ContentUnneeded, set by the same predicate.
	fts3ContentUnneeded map[int]bool

	// fts5MatchGood is fts3MatchGood's fts5 twin: the set of table scopes (by
	// index into c.scopes) whose fts5 MATCH C SQLite's planner can hand the
	// module as a virtual-table constraint, computed once by fts5MatchBindings
	// (fts5_match_placement.go). compileFts5Match compiles a MATCH whose scope
	// is not a key of this map to a run-time refusal instead. Set only by
	// compileSelectScan; nil everywhere else.
	fts5MatchGood *fts5MatchPlacement

	// fts3Conjuncts is stmt.Where's top-level AND-conjuncts and fts3Elsewhere
	// every OTHER expression of the statement that can constrain a row. They
	// exist for one question: which LANGUAGE an fts4 "languageid=" MATCH
	// searches, which C SQLite answers from the equality constraint its
	// planner hands the module (fts3_langid.go's fts3ResolveMatchLangid). Set
	// by compileSelectScan only.
	fts3Conjuncts []Expr
	fts3Elsewhere []Expr

	// hoist is this scan-compile attempt's collection point for aggregates an
	// INNER compile discovers belong to THIS query (C SQLite's outward
	// aggregate-association walk -- see vdbe_agg_hoist.go). Set only by
	// compileSelectScanRow, which owns the box and recompiles the statement as
	// an aggregate query when a failed attempt left it non-empty. nil for every
	// other compile, which is what keeps the write path and the FROM-less
	// compilers out of it.
	hoist *hoistBox

	// hoisted is the spec list a RETRY compile was started with: one aggregate
	// call per entry, each written inside a subquery of this query's select list
	// but owned by this query. Non-empty forces the aggregate dispatch (see
	// compileSelectScanRow) and is consumed by planHoistedAggs.
	hoisted []hoistSpec

	// fts5Aux is the compile's fts5 auxiliary-function context (parsed MATCH
	// phrases plus bm25 statistics), built once by buildFts5AuxState and read by
	// compileColumn's "rank" case and compileFunc. Set only for row-mode scans; nil
	// elsewhere, including subqueries.
	fts5Aux *fts5AuxState

	// flatOuter is the SELECT whose FROM clause compileScanAttempt is resolving, for
	// flattenedSubtypeKeep (subquery_subtype.go); nil outside that call.
	flatOuter *SelectStmt

	// cteOuterCapName / cteOuterCapPlus1 publish THIS statement's own exact
	// bound on how many rows it can read from the recursive CTE it names
	// (recursiveCTEOuterCap, cte.go) down to resolveCTESource, which copies
	// them onto the one cteRef that consumes it. Plus-one encoded, so 0 means
	// "no bound"; see cteRef.outerRowCapPlus1 for why the bound rides the
	// REFERENCE and not the shared *cteBinding.
	cteOuterCapName  string
	cteOuterCapPlus1 int64

	// nQueryLoop is this point's pParse->nQueryLoop (sqliteInt.h:3887): the LogEst
	// of how often the enclosing loop nest runs the code being compiled. 0 at top
	// level (prepare.c:788); compileScanPlain/Sorted add their plan's row estimate
	// while compiling the body, as sqlite3WhereBegin does (where.c:7198), and
	// restore it after. Inherited through outer (inheritedNQueryLoop). It seeds
	// wherePathSolver (where.c:5921).
	//
	// Write statements: INSERT values at 0; UPDATE/DELETE WHERE inside the loop;
	// trigger programs start from the firing statement's value (trigger.c:1288),
	// which is 0 everywhere this engine fires them. ponytail: a DELETE's
	// ONEPASS_SINGLE row would seed a body plan at 1, not 0; both sit under the
	// "nRow < 3" automatic-index gate, so no plan differs. Carry the firing plan's
	// nRowOut into the trigger compile if that stops holding.
	//
	// A compiler built with no outer leaves this unset with nQueryLoopKnown false.
	nQueryLoop logEst

	// nQueryLoopKnown reports whether nQueryLoop provably matches C's value here.
	// True at top level and inherited through outer, except that aggregate,
	// GROUP BY and window compiles clear it: sqlite3WhereEnd restores nQueryLoop
	// before an aggregate's result phase but after a plain query's select list, and
	// this compiler does not track which side a nested compile sits on. The WHERE
	// planner consults it before seeding a nested solve.
	nQueryLoopKnown bool

	// planNRow / planNRowOK are the winning plan's row estimate from the WHERE
	// planner (markWherePlanEligibility), consumed by compileScanPlain/Sorted to
	// bump nQueryLoop. planNRowOK is false when the planner did not fully decide the
	// plan: unknown, not zero.
	planNRow   logEst
	planNRowOK bool

	// groupSpanRecursion is true only for the throwaway compiler
	// resolveGroupSource builds to resolve a parenthesized join group's span with
	// its outward connector neutralized. checkOneRebuiltGroup's front-move decline
	// skips there, since the outer resolveJoinSources call re-checks with full
	// context (join2.test's "t1 NATURAL LEFT OUTER JOIN (t2 NATURAL JOIN t3)").
	groupSpanRecursion bool
}

// inheritedNQueryLoop is the nQueryLoop/nQueryLoopKnown value a compiler built
// beneath outer should start with -- the SAME chain c.outer itself is threaded
// through (compileSubProgram and friends), so every construction site that
// sets outer: outer should also set nQueryLoop/nQueryLoopKnown from this. nil
// (a true top-level compile) answers (0, true): prepare.c:788's
// "assert( 0==sParse.nQueryLoop )".
func inheritedNQueryLoop(outer *compiler) (logEst, bool) {
	if outer == nil {
		return 0, true
	}
	return outer.nQueryLoop, outer.nQueryLoopKnown
}

// alloc returns a fresh, never-before-used register number.
func (c *compiler) alloc() int {
	r := c.nReg
	c.nReg++
	return r
}

// allocN reserves a consecutive block of n registers and returns its base.
func (c *compiler) allocN(n int) int {
	base := c.nReg
	c.nReg += n
	return base
}

// allocCursor returns a fresh, never-before-used cursor number.
func (c *compiler) allocCursor() int {
	n := c.nCursor
	c.nCursor++
	return n
}

// allocRec returns a fresh, never-before-used record register number (rr[]:
// the sorter opcodes' separate register space for a packed key+payload
// record -- see vdbe_op.go's sorter-opcode doc comment).
func (c *compiler) allocRec() int {
	n := c.nRec
	c.nRec++
	return n
}

// allocSorter returns a fresh, never-before-used sorter cursor number.
func (c *compiler) allocSorter() int {
	n := c.nSorter
	c.nSorter++
	return n
}

// allocSub returns a fresh, never-before-used run-once subquery cache slot
// number (subCache[]; see the subquery opcodes, vdbe_op.go).
func (c *compiler) allocSub() int {
	n := c.nSub
	c.nSub++
	return n
}

// allocDistinct returns a fresh, never-before-used distinct-set tracker number
// (distinctSets[]; see the DISTINCT opcodes, vdbe_op.go).
func (c *compiler) allocDistinct() int {
	n := c.nDistinct
	c.nDistinct++
	return n
}

// emit appends one instruction and returns its address (for later patching of
// a jump target).
func (c *compiler) emit(in Instruction) int {
	c.insns = append(c.insns, in)
	return len(c.insns) - 1
}

// here is the address the next emitted instruction will occupy -- a jump
// target for code that hasn't been emitted yet.
func (c *compiler) here() int { return len(c.insns) }

// patch sets instruction addr's jump target (P2) to target.
func (c *compiler) patch(addr, target int) { c.insns[addr].P2 = target }

// errVDBEUnsupported is returned (wrapped) whenever the compiler meets a
// construct outside this increment's scope. The integration layer treats any
// error from compilation as "decline cleanly (no fallback exists)".
var errVDBEUnsupported = fmt.Errorf("vdbe: unsupported")

// compileSelectNoFrom compiles a FROM-less, non-aggregate SELECT into a
// Program. It returns errVDBEUnsupported (wrapped) for anything it can't yet
// handle. Column names are computed by expandSelectList with no scopes,
// so a successful Program is directly
// comparable to execNoFrom's output.
func compileSelectNoFrom(stmt *SelectStmt) (*Program, error) {
	return compileSelectNoFromPager(nil, stmt, nil)
}

// compileSelectNoFromPager is compileSelectNoFrom with a pager and an outer
// compiler chain, so a FROM-less subquery can resolve tables in nested
// subqueries and correlated references. The top-level entry passes nil for
// both, so a subquery in a top-level FROM-less SELECT declines.
func compileSelectNoFromPager(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler) (*Program, error) {
	return compileSelectNoFromTrig(pager, stmt, outer, nil, nil, nil)
}

// compileSelectNoFromRow is compileSelectNoFromPager with a live enclosing-row
// evalCtx to resolve against as a last resort -- see compiler.rowOuter.
func compileSelectNoFromRow(pager *ReadOnlyPager, stmt *SelectStmt, rowOuter *evalCtx) (*Program, error) {
	return compileSelectNoFromTrig(pager, stmt, nil, nil, rowOuter, nil)
}

// live, when non-nil, makes every subquery this FROM-less compile emits a LIVE
// one (compiler.liveDB) -- the trigger-body case, where a read must bind at the
// firing instant rather than to the image the firing statement was compiled
// over. It travels with a pager, exactly as compileTriggerGuard pairs the two
// for a WHEN clause. nil everywhere else, which compiles byte-identically to
// before.
func compileSelectNoFromTrig(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler, trig *trigCompileCtx, rowOuter *evalCtx, live *DB) (*Program, error) {
	if len(stmt.From) != 0 || len(stmt.Compound) != 0 {
		return nil, fmt.Errorf("%w: not a plain FROM-less SELECT", errVDBEUnsupported)
	}
	// A FROM-less GROUP BY is validated and then dropped, as in C (one row is one
	// group; see validateNoFromGroupBy). Needed here too because a subquery body
	// (a CTE core, scalar/EXISTS/IN) reaches this compiler with the clause attached
	// (with3.test).
	if len(stmt.GroupBy) != 0 {
		trimmed, gerr := validateNoFromGroupByCompile(pager, stmt, outer, trig, rowOuter)
		if gerr != nil {
			return nil, gerr
		}
		stmt = trimmed
	}
	// "SELECT 1 HAVING 1" -- a HAVING on a query with neither GROUP BY nor an
	// aggregate result set is C SQLite's own "HAVING clause on a
	// non-aggregate query" rejection, not a gap. See
	// requireAggregateForHaving (sql_agg.go); the aggregate FROM-less shape
	// never reaches here (the containsAggregate loop just below rejects it,
	// routing to compileNoFromAggregate instead).
	if err := requireAggregateForHaving(stmt); err != nil {
		return nil, err
	}
	// A window function without a FROM is a window over the ONE synthetic row
	// this shape yields -- compileScanWindow with zero join sources. See
	// compileNoFromWindow (vdbe_window.go) for the oracle evidence. Dispatched
	// before the loop below because a windowed call is not an aggregate call
	// (isAggregateCall declines any FuncExpr with an OVER clause), so without
	// this it fell all the way through to compileExpr and declined as an
	// unknown scalar "function sum()".
	if selectHasWindow(stmt) || orderByHasWindow(stmt) {
		return compileNoFromWindow(pager, stmt, outer, trig, rowOuter)
	}
	for _, sc := range stmt.Columns {
		if sc.Star {
			return nil, fmt.Errorf("%w: SELECT *", errVDBEUnsupported)
		}
		if containsAggregate(sc.Expr) {
			// An aggregate select list is compileNoFromAggregate's shape, not
			// this one's. The TOP-level spelling is dispatched there by
			// execNoFrom (tryVDBENoFromAggregate, vdbe_run.go) before this
			// function is ever called, and a SUBQUERY BODY by compileSubProgram
			// below -- so what still reaches this decline is only the shapes
			// neither route covers (a trigger body, a register-backed row
			// scope), which that compiler has no scope machinery for.
			return nil, fmt.Errorf("%w: aggregate", errVDBEUnsupported)
		}
	}
	// The same result-set-alias rule compileSelectScan applies (sql_alias.go):
	// "SELECT 1 AS one, 2 AS two WHERE one<two". A FROM-less SELECT has no
	// scopes at all, so every unqualified WHERE name is a candidate -- which is
	// still correct for a CORRELATED FROM-less subquery, because this level's
	// own alias outranks an outer scope's column (see sql_alias.go's evidence).
	// GROUP BY/HAVING can't reach here (rejected above / unparseable without
	// GROUP BY), so WHERE is the only clause involved.
	if trig == nil {
		stmt = substituteResultAliases(stmt, nil, nil, pager)
	}
	outCols, err := expandSelectList(stmt.Columns, nil, pager.colNameMode())
	if err != nil {
		return nil, err
	}
	// At most one row, so ORDER BY reorders nothing, but an ordinal is still
	// range-checked at prepare time ("1st ORDER BY term out of range - should be
	// between 1 and 1"), as resolveOrderKeys does for a scan.
	for i, ot := range stmt.OrderBy {
		if n, ok := orderByOrdinal(stripOrderCollate(ot.Expr)); ok && (n < 1 || int(n) > len(outCols)) {
			return nil, semanticf("%s ORDER BY term out of range - should be between 1 and %d",
				sqliteOrdinalWord(i+1), len(outCols))
		}
	}

	c := &compiler{pager: pager, outer: outer, trig: trig, rowOuter: rowOuter, liveDB: live}
	// A FROM-less body has no WHERE loop of its own (no sqlite3WhereBegin/End
	// pair), so it never touches nQueryLoop -- it only passes through whatever
	// its own outer's ambient value was, exactly like C SQLite's Parse
	// object is untouched by a FROM-less sqlite3Select. trig != nil (a
	// trigger body's own FROM-less SELECT) is NOT specially distrusted here:
	// outer's inherited trust already carries whatever the caller could prove,
	// and vdbe_trigger.go's own root compiler (outer == nil there) already
	// starts distrusted, so this just propagates that correctly either way.
	c.nQueryLoop, c.nQueryLoopKnown = inheritedNQueryLoop(outer)
	// OP_Init at address 0, jumping to the program body at address 1 -- a
	// structural echo of every C SQLite program (the body has no one-time
	// setup to skip yet, so this is a no-op jump kept for EXPLAIN fidelity).
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)

	// Result registers occupy a consecutive block so OpResultRow can name them
	// as one range (P1..P1+P2-1), matching SQLite's codegen.
	resultBase := c.allocN(len(outCols))

	// An unresolved LIMIT/OFFSET (e.g. a trigger body's "LIMIT new.a") is coded
	// into a register; -1 means none. Coded before the WHERE, as
	// computeLimitRegisters is (select.c:2517): a non-integer LIMIT is "datatype
	// mismatch" even when the WHERE excludes the row.
	limitReg, offsetReg := -1, -1
	if stmt.LimitParam != nil {
		r, lerr := c.emitLimitOffsetReg(stmt.LimitParam, false)
		if lerr != nil {
			return nil, lerr
		}
		limitReg = r
	}
	if stmt.OffsetParam != nil {
		r, oerr := c.emitLimitOffsetReg(stmt.OffsetParam, true)
		if oerr != nil {
			return nil, oerr
		}
		offsetReg = r
	}

	// WHERE: evaluate once; if it is false OR NULL, jump past the result-row
	// emission (producing zero rows), exactly like execNoFrom's early return.
	// The result-list expressions below are therefore not evaluated when WHERE
	// excludes the row.
	var whereJump = -1
	if stmt.Where != nil {
		// A FROM-less WHERE still gets the vector-equality rewrite: where.c splits the
		// WHERE (6941) before the nTabList==0 case (6945) and runs
		// sqlite3WhereExprAnalyze regardless (6990).
		c.inWhereConjunct = true
		wReg, err := c.compileExpr(stmt.Where)
		if err != nil {
			return nil, err
		}
		// OpIfNot with jumpIfNull=1: take the jump when WHERE is false or NULL.
		whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
	}

	for i, oc := range outCols {
		reg, err := c.compileExpr(oc.expr)
		if err != nil {
			return nil, err
		}
		c.emit(Instruction{Op: OpSCopy, P1: reg, P2: resultBase + i})
	}

	// LIMIT 0 or OFFSET>=1 makes a FROM-less query yield zero rows -- but only
	// AFTER the row has been evaluated (execNoFrom computes the row, then drops
	// it), so any evaluation error still surfaces. We therefore always emit the
	// result-list evaluation above and merely skip OpResultRow here.
	skipRow := (stmt.Offset != nil && *stmt.Offset >= 1) || (stmt.Limit != nil && *stmt.Limit == 0)
	// The same rule as a RUN-TIME test, over the counters coded above.
	//
	// LIMIT: skip when the counter is 0 (no rows -- select.c:2531), keep the row
	// when it is negative (unlimited -- select.c:2528), which is exactly what
	// truthiness gives. OFFSET: skip when it is non-zero; OpLimitCounter already
	// clamped a negative to 0, since C's OP_IfPos skip never fires on one.
	var skipJumps []int
	if limitReg >= 0 {
		skipJumps = append(skipJumps, c.emit(Instruction{Op: OpIfNot, P1: limitReg}))
	}
	if offsetReg >= 0 {
		skipJumps = append(skipJumps, c.emit(Instruction{Op: OpIf, P1: offsetReg}))
	}
	if !skipRow {
		c.emit(Instruction{Op: OpResultRow, P1: resultBase, P2: len(outCols)})
	}
	for _, j := range skipJumps {
		c.patch(j, c.here())
	}
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}
	c.emit(Instruction{Op: OpHalt})

	cols := make([]string, len(outCols))
	for i, oc := range outCols {
		cols[i] = oc.name
	}
	return &Program{
		Insns:      c.insns,
		NReg:       c.nReg,
		NSubCache:  c.nSub,
		NResultCol: len(outCols),
		ColNames:   cols,
		Correlated: c.correlated,
	}, nil
}

// validateNoFromGroupByCompile is validateNoFromGroupBy's compile-time
// counterpart for a FROM-less subquery body, with the same rules: each term
// must resolve, an aggregate (even via an alias) is rejected, an out-of-range
// ordinal is rejected, GROUP BY with HAVING declines. The probe compiles via
// compileSelectNoFromTrig, so it needs no parameter values; a term carrying an
// unresolved bound parameter declines.
func validateNoFromGroupByCompile(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler, trig *trigCompileCtx, rowOuter *evalCtx) (*SelectStmt, error) {
	if stmt.Having != nil {
		return nil, fmt.Errorf("%w: GROUP BY without FROM combined with HAVING is not supported", errVDBEUnsupported)
	}
	var probe []SelectColumn
	for i, g := range stmt.GroupBy {
		resolved, viaSelectList := g, false
		if n, ok := orderByOrdinal(g); ok {
			if n < 1 || n > int64(len(stmt.Columns)) {
				return nil, semanticf("engine: %s GROUP BY term out of range - should be between 1 and %d", ordinalWord(i+1), len(stmt.Columns))
			}
			resolved, viaSelectList = stmt.Columns[n-1].Expr, true
		} else if c, ok := g.(ColumnExpr); ok && c.Qualifier == "" && c.Schema == "" {
			for _, sc := range stmt.Columns {
				if sc.Alias != "" && equalFoldName(sc.Alias, c.Name) {
					resolved, viaSelectList = sc.Expr, true
					break
				}
			}
		}
		if containsAggregate(resolved) {
			return nil, semanticf("engine: aggregate functions are not allowed in the GROUP BY clause")
		}
		// A GROUP BY term that is or contains a window function is "misuse of %s
		// function" (resolve.c:1263), FROM-less or not.
		if exprHasWindow(resolved) {
			return nil, semanticf("engine: misuse of window function in GROUP BY")
		}
		if !viaSelectList {
			if exprHasParam(g) {
				return nil, fmt.Errorf("%w: FROM-less subquery GROUP BY term with an unresolved parameter", errVDBEUnsupported)
			}
			probe = append(probe, SelectColumn{Expr: g})
		}
	}
	if len(probe) > 0 {
		check := *stmt
		check.Columns = probe
		check.GroupBy = nil
		check.Having = nil
		check.OrderBy = nil
		check.Limit, check.Offset = nil, nil
		check.LimitParam, check.OffsetParam = nil, nil
		prog, err := compileSelectNoFromTrig(pager, &check, outer, trig, rowOuter, nil)
		if err != nil {
			return nil, err
		}
		// A discarded GROUP BY term is never executed, so compiling it only proves it
		// is well-formed. A correlated probe would need per-row evaluation to prove it
		// never errors, so it declines.
		if prog.Correlated {
			return nil, fmt.Errorf("%w: FROM-less subquery GROUP BY term correlates to an outer row and cannot be probed at compile time", errVDBEUnsupported)
		}
	}
	out := *stmt
	out.GroupBy = nil
	return &out, nil
}

// enclosingTriggerCtx returns the trigger context of the nearest enclosing
// compiler that has one, so a sub-SELECT in a trigger body resolves NEW/OLD.
// C resolves them off pParse->pTriggerTab (resolve.c:525), a parse-level field
// visible at every depth. NEW/OLD become OpParam reads of the trigger row
// (resolveTriggerParam), never folded literals.
func enclosingTriggerCtx(outer *compiler) *trigCompileCtx {
	for c := outer; c != nil; c = c.outer {
		if c.trig != nil {
			return c.trig
		}
	}
	return nil
}

// compileSubProgram compiles a subquery into its own Program against pager,
// chaining column resolution to outer so a correlated reference binds to an
// enclosing query's cursor. Compounds go to compileSubProgramCompound,
// FROM-less bodies to compileSelectNoFromPager, everything else to
// compileSelectScan. Program.Correlated tells the caller whether to run it once
// or per evaluation. An unservable body or correlation is errVDBEUnsupported.
func compileSubProgram(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler) (*Program, error) {
	// A subquery body may carry its own WITH clause; push its CTEs for the length
	// of its compile, since a CTE FROM item binds at compile time
	// (resolveCTESource).
	if len(stmt.CTEs) > 0 && pager != nil {
		pop := pager.pushCTEScope(stmt.CTEs)
		defer pop()
	}
	// A subquery body is a SELECT like any other to sqlite3Select: its own FROM
	// clause is flattened (flattenSubquery, select.c:4290) and pushed down into
	// (pushDownWhereTerms, select.c:5134) exactly as the top-level statement's
	// is in execSelect. See flatten_projection.go and pushdown_where.go.
	if pager != nil && pager.r41ArmsHeld == 0 && len(stmt.Compound) == 0 && len(stmt.From) != 0 {
		stmt = pager.r41FlattenProjection(stmt)
		if len(stmt.Compound) == 0 {
			stmt = pager.r41PushDown(stmt)
		}
	}
	// An unresolved LIMIT/OFFSET in a sub-select would otherwise be ignored
	// (execSelect resolves only the top level). resolveSubProgramLimitOffset folds
	// it for this compile only. A clause naming the firing row, a bound parameter,
	// or one folding to a non-integer is left for the codegen to code into a
	// register (emitLimitOffsetReg), as C does. Since only some bodies can code
	// one, the program is checked for the matching OpLimitCounter afterwards; a
	// body that ignored the clause declines rather than returning every row.
	// A compound body whose LIMIT/OFFSET is a non-integer is answered here first
	// (limit_offset_halt.go): it has no instruction stream to code into.
	if prog, handled, err := compileCompoundLimitMismatch(pager, stmt, outer); handled {
		return prog, err
	}
	codedLimits := 0
	var deferredLimit error
	if stmt.LimitParam != nil || stmt.OffsetParam != nil {
		if enclosingTriggerCtx(outer) != nil && limitOffsetNamesFiringRow(stmt) {
			if stmt.LimitParam != nil {
				codedLimits++
			}
			if stmt.OffsetParam != nil {
				codedLimits++
			}
		} else {
			restore, d, err := resolveSubProgramLimitOffset(pager, stmt)
			if err != nil {
				return nil, err
			}
			defer restore()
			// A clause the fold DEFERRED (see resolveSubProgramLimitOffset)
			// is still unresolved, and is coded into a counter register by
			// exactly the two bodies that can, exactly like the firing-row
			// case above -- so it is counted the same way and checked by the
			// same post-condition.
			deferredLimit = d
			if stmt.LimitParam != nil {
				codedLimits++
			}
			if stmt.OffsetParam != nil {
				codedLimits++
			}
		}
	}
	if len(stmt.Compound) != 0 {
		if codedLimits > 0 {
			// A compound has no instructions of its own -- its arms ARE the
			// frames (Program.Compound) -- so there is nowhere for this
			// statement's own LIMIT/OFFSET counter to be coded, and the arms
			// would run unbounded. Refused BEFORE the compile rather than after
			// it, because this arm returns early and never reaches the
			// post-condition below. Measured, with the check missing: a trigger
			// body's "INSERT INTO dst SELECT b FROM u UNION SELECT 9 LIMIT
			// new.a" wrote THREE rows where 3.53.3 writes two.
			return nil, fmt.Errorf("%w: LIMIT/OFFSET naming the firing row on a compound SELECT", errVDBEUnsupported)
		}
		return compileSubProgramCompound(pager, stmt, outer)
	}
	var (
		prog *Program
		err  error
	)
	switch {
	case len(stmt.From) == 0 && noFromBodyIsAggregate(stmt):
		// A FROM-less body with an aggregate goes to compileNoFromAggregate. outer is
		// threaded so a correlated reference beside the aggregate binds
		// (bindAggOuterRefs). An aggregate whose argument names an enclosing column
		// still declines (checkAggregateAssociation): C re-associates it outward.
		prog, err = compileNoFromAggregate(pager, stmt, outer)
	case len(stmt.From) == 0:
		prog, err = compileSelectNoFromTrig(pager, stmt, outer, enclosingTriggerCtx(outer), nil, nil)
	case pager == nil:
		return nil, fmt.Errorf("%w: subquery over a table needs an open database", errVDBEUnsupported)
	default:
		prog, err = compileSelectScan(pager, stmt, outer)
	}
	if err != nil {
		return nil, err
	}
	if codedLimits > 0 && countLimitCounters(prog) != codedLimits {
		// The body this statement dispatched to does not code a LIMIT/OFFSET
		// register (a compound, a FROM-less aggregate, a sorted or grouped or
		// windowed scan, ...), so it built a program that silently IGNORES the
		// clause. Refused here rather than returned. See the codedLimits block
		// above for why this is checked instead of predicted.
		if deferredLimit != nil {
			// A DEFERRED clause had a perfectly good compile-time failure
			// already -- the one the fold produced. Report that one verbatim,
			// so a shape this dispatch cannot code fails exactly as it did
			// before the deferral existed.
			return nil, deferredLimit
		}
		return nil, fmt.Errorf("%w: LIMIT/OFFSET naming the firing row in this query shape", errVDBEUnsupported)
	}
	// The aggregate opcodes resolve a CORRELATED reference through
	// evalCtx.outer rather than through an OpOuterColumn, so they need the
	// enclosing frames described to them -- see Program.OuterFrames and
	// buildOuterEvalCtx (vdbe.go). Recording them here, at the single place a
	// sub-Program is built with an enclosing compile in hand, keeps the
	// description and the compile that produced it in lock-step.
	prog.OuterFrames = outerFramesOf(outer)
	// Same idea, for the CTE scope this compile ran under -- see Program.
	// CTEScopeSnapshot's doc comment. Captured here, still inside this
	// function's own pushCTEScope bracket (the defer above has not fired
	// yet), so it includes this statement's OWN WITH clause too, not just
	// whatever was already on pager.cteScopes when compileSubProgram was
	// entered.
	prog.CTEScopeSnapshot = pager.snapshotCTEScopes()
	return prog, nil
}

// resolveSubProgramLimitOffset folds stmt's LimitParam/OffsetParam into a
// concrete Limit/Offset for the length of one compile and returns the undo. A
// nested subquery body never passes through execSelect's top-level fold.
//
// The fold is undone because the write path compiles ahead of running: a
// trigger body compiled while compiling the firing statement would otherwise
// keep a stale bound in its cached AST (a LIMIT (SELECT count(*) FROM ctr) in a
// trigger returned too few rows). Kept within the Program it is safe: a read
// compiles and runs together, a write's sub-Program carries WritePager and is
// never cached, and a trigger body reading a frozen snapshot is declined.
//
// C never folds beyond sqlite3ExprIsInteger (expr.c:2899): computeLimitRegisters
// codes the clause into a register and checks it with OP_MustBeInt
// (select.c:2547-2562). Coding into a register is the end state here too; until
// every consumer of the *int64 Limit/Offset takes a register, a per-compile fold
// is the approximation.
//
// A nil pager declines (the clause may hold a subquery). The expression is
// evaluated by compiling it (foldLimitOffsetExpr), never by walking the AST;
// outer is nil because LIMIT/OFFSET cannot name anything (resolve.c:1900). stmt
// is mutated in place rather than copied because aggregate hoisting matches
// subquery bodies by pointer identity (vdbe_agg_hoist.go); the undo keeps that
// from lasting.
//
// deferred is non-nil when a clause was not folded because its value is not an
// integer, which C reports only when the statement runs. The clause is left
// for the codegen to code, and the caller reports this error if the body
// dispatched to cannot code one.
func resolveSubProgramLimitOffset(pager *ReadOnlyPager, stmt *SelectStmt) (restore func(), deferred error, err error) {
	origLimit, origLimitParam := stmt.Limit, stmt.LimitParam
	origOffset, origOffsetParam := stmt.Offset, stmt.OffsetParam
	restore = func() {
		stmt.Limit, stmt.LimitParam = origLimit, origLimitParam
		stmt.Offset, stmt.OffsetParam = origOffset, origOffsetParam
	}
	// Every failure exit undoes whatever was already folded, so a declined
	// compile leaves the AST exactly as it was parsed.
	fail := func(e error) (func(), error, error) {
		restore()
		return nil, nil, e
	}
	if pager == nil {
		return fail(fmt.Errorf("%w: subquery with an unresolved LIMIT/OFFSET expression", errVDBEUnsupported))
	}
	// A bound parameter is left for the codegen to code into the counter register,
	// as C does (select.c:2547); its value is unknown at compile time. The caller
	// checks the program for the matching OpLimitCounter, so a body that ignored
	// it declines.
	if exprHasParam(stmt.LimitParam) || exprHasParam(stmt.OffsetParam) {
		return restore, nil, nil
	}
	// fold reports false when the clause must be left for the codegen. That is
	// exactly errLimitNotInteger: C raises it from OP_MustBeInt when the SELECT runs
	// (select.c:2547), so a subquery never stepped never raises it (aggnested.test
	// 5.5). Other failures still fail the compile. A compound body is excluded: it
	// has no instructions to code into.
	fold := func(e Expr) (int64, bool, error) {
		n, ferr := foldLimitOffsetExpr(pager, e, nil)
		switch {
		case ferr == nil:
			return n, true, nil
		case errors.Is(ferr, errLimitNotInteger) && len(stmt.Compound) == 0:
			deferred = ferr
			return 0, false, nil
		default:
			return 0, false, ferr
		}
	}
	if stmt.LimitParam != nil {
		n, folded, ferr := fold(stmt.LimitParam)
		if ferr != nil {
			return fail(ferr)
		}
		if folded {
			stmt.Limit = &n
			stmt.LimitParam = nil
		}
	}
	if stmt.OffsetParam != nil {
		n, folded, ferr := fold(stmt.OffsetParam)
		if ferr != nil {
			return fail(ferr)
		}
		if folded {
			stmt.Offset = &n
			stmt.OffsetParam = nil
		}
	}
	return restore, deferred, nil
}

// foldLimitOffsetExpr evaluates one LIMIT/OFFSET expression to the integer the
// *int64 Limit/Offset needs by compiling it as a one-column FROM-less SELECT and
// running it once (subqueries compile as sub-Programs). args are the bound
// parameters: nil from resolveSubProgramLimitOffset (which declines
// parameters), real ones from resolveNoFromLimitOffset.
//
// The synthetic SELECT allows aggregates and window functions (resolve.c:1966)
// where the clause does not (resolve.c:1903), so requireZeroedNameContext
// re-imposes that first: "LIMIT row_number() OVER ()" is "misuse of window
// function row_number()".
func foldLimitOffsetExpr(pager *ReadOnlyPager, e Expr, args []Value) (int64, error) {
	if err := requireZeroedNameContext(e); err != nil {
		return 0, err
	}
	prog, err := compileSelectNoFromPager(pager, &SelectStmt{Columns: []SelectColumn{{Expr: e}}}, nil)
	if err != nil {
		return 0, err
	}
	rows, err := prog.exec(pager, args)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("%w: LIMIT/OFFSET expression did not yield one value", errVDBEUnsupported)
	}
	return limitOffsetValueToCount(rows[0][0])
}

// emitLimitOffsetReg codes one LIMIT/OFFSET expression into a register instead
// of folding it, as C's computeLimitRegisters always does:
//
//	select.c:2547   sqlite3ExprCode(pParse, pLimit->pLeft, iLimit);
//	select.c:2548   sqlite3VdbeAddOp1(v, OP_MustBeInt, iLimit);
//	select.c:2556   sqlite3ExprCode(pParse, pLimit->pRight, iOffset);
//	select.c:2557   sqlite3VdbeAddOp1(v, OP_MustBeInt, iOffset);
//
// Needed beside the fold for a trigger body's "LIMIT new.a", which differs per
// firing row; it reads the trigger row via OpParam like any NEW/OLD reference.
//
// It compiles under an empty NameContext (resolve.c:1900-1908), hiding every
// scope except c.trig: NEW/OLD resolve through pParse->pTriggerTab
// (resolve.c:525), which the memset cannot clear, while "excluded" needs
// NC_UUpsert (resolve.c:547), which it does (see limitOffsetPseudoRowRefs).
// requireZeroedNameContext re-imposes the no-aggregate, no-window half.
func (c *compiler) emitLimitOffsetReg(e Expr, isOffset bool) (int, error) {
	if err := requireZeroedNameContext(e); err != nil {
		return 0, err
	}
	savedScopes, savedRegScopes, savedOuter, savedRowOuter := c.scopes, c.regScopes, c.outer, c.rowOuter
	savedDrain, savedBuf, savedAggRegs, savedWinRegs, savedExcl := c.drain, c.bufOut, c.aggRegs, c.winRegs, c.excl
	c.scopes, c.regScopes, c.outer, c.rowOuter = nil, nil, nil, nil
	c.drain, c.bufOut, c.aggRegs, c.winRegs, c.excl = nil, nil, nil, nil, nil
	reg, err := c.compileExpr(e)
	c.scopes, c.regScopes, c.outer, c.rowOuter = savedScopes, savedRegScopes, savedOuter, savedRowOuter
	c.drain, c.bufOut, c.aggRegs, c.winRegs, c.excl = savedDrain, savedBuf, savedAggRegs, savedWinRegs, savedExcl
	if err != nil {
		return 0, err
	}
	p2 := 0
	if isOffset {
		p2 = 1
	}
	c.emit(Instruction{Op: OpLimitCounter, P1: reg, P2: p2})
	return reg, nil
}

// limitOffsetNamesFiringRow reports whether stmt's own LIMIT/OFFSET expression
// names a trigger pseudo-row -- the one value a compile-time fold can never
// know, since it differs per firing row. See emitLimitOffsetReg.
func limitOffsetNamesFiringRow(stmt *SelectStmt) bool {
	if !exprNamesFiringRow(stmt.LimitParam) && !exprNamesFiringRow(stmt.OffsetParam) {
		return false
	}
	// Withheld when this statement's FROM names an item "new" or "old", as
	// exclCompileCtx.shadowed does for "excluded": compileColumn tries the
	// trigger pseudo-row before the FROM items, where resolve.c consults it
	// only when the FROM walk found nothing ("cnt==0", :521). So a body
	// statement with a table or alias of that name already reads the firing
	// row where C reads the table -- a known divergence:
	//
	//	INSERT INTO dst SELECT new.b FROM new LIMIT 1        C 70, this 20
	//
	// Coding "LIMIT new.a" there would add a second wrong answer, so it stays
	// on the fold, which declines a firing-row reference.
	return !fromNamesTriggerPseudoRow(stmt.From)
}

// fromNamesTriggerPseudoRow reports whether any FROM item at this statement's
// own level is named -- by table name or by alias -- "new" or "old", the two
// names resolve.c's pseudo-row block answers (:537, :540). See
// limitOffsetNamesFiringRow, its only caller.
func fromNamesTriggerPseudoRow(from []FromItem) bool {
	for _, f := range from {
		for _, n := range [2]string{f.Alias, f.Table} {
			if n == "" {
				continue
			}
			switch r33sFoldIdent(n) {
			case "new", "old":
				return true
			}
			// An ALIAS, when written, REPLACES the table name for every
			// qualified reference, so a named alias ends the check for this
			// item rather than falling through to its table name.
			break
		}
	}
	return false
}

// exprNamesFiringRow reports whether e holds a NEW./OLD. reference at its own
// level. A subquery body is not entered: "LIMIT (SELECT new.a)" stays on the
// fold, which declines it.
//
// A three-part "main.new.a" is not one: resolve.c gates the pseudo-row block on
// "if( cnt==0 && zDb==0 )" (:522), so C rejects it ("no such column:
// main.new.a"), while compileColumn's c.trig arm ignores x.Schema and would
// resolve it. emitReturningParam and emitExcludedParam apply the same gate.
func exprNamesFiringRow(e Expr) bool {
	if col, ok := e.(ColumnExpr); ok && col.Qualifier != "" && col.Schema == "" {
		switch r33sFoldIdent(col.Qualifier) {
		case "new", "old":
			return true
		}
	}
	found := false
	walkExprOperands(e, func(sub Expr) {
		if !found && sub != nil && exprNamesFiringRow(sub) {
			found = true
		}
	})
	return found
}

// countLimitCounters reports how many LIMIT/OFFSET expressions THIS program
// coded into registers. It is the post-condition compileSubProgram checks after
// handing an unresolved clause to the codegen: a body that silently IGNORED the
// clause emits none, and an ignored LIMIT is a wrong answer (every row where the
// oracle answers some), so the count not matching is a decline. Only this
// program's own instructions are counted -- a nested sub-program's own clause
// lives in a P4 payload and is that program's business.
func countLimitCounters(prog *Program) int {
	if prog == nil {
		return 0
	}
	n := 0
	for i := range prog.Insns {
		if prog.Insns[i].Op == OpLimitCounter {
			n++
		}
	}
	return n
}

// noFromBodyIsAggregate reports whether a FROM-less subquery body belongs to
// compileNoFromAggregate rather than compileSelectNoFromPager: at least one
// non-"*" select-list item contains an aggregate call, and the statement has no
// window function anywhere. The window exclusion mirrors compileSelectNoFromTrig's
// own dispatch order -- a windowed call is never an aggregate call
// (isAggregateCall declines any FuncExpr with an OVER clause), and a FROM-less
// window query is compileNoFromWindow's (vdbe_window.go), so a body mixing the
// two must keep taking the window route.
func noFromBodyIsAggregate(stmt *SelectStmt) bool {
	if selectHasWindow(stmt) || orderByHasWindow(stmt) {
		return false
	}
	for _, sc := range stmt.Columns {
		if !sc.Star && containsAggregate(sc.Expr) {
			return true
		}
	}
	return false
}

// outerFramesOf snapshots the enclosing compile chain outward from oc (index 0
// = oc itself, the immediate parent of the sub-Program being compiled): each
// level's table scopes and the cursor numbers holding their rows. A scope
// whose columns are REBASED into a shared group cursor (compileScope.colBase
// != 0) is omitted rather than described wrong -- tableScope.colIndex is local
// there, so reading it against the cursor's flattened row would fetch the
// wrong column; leaving it out makes a reference to it simply not resolve,
// which surfaces as an ordinary "no such column" instead.
func outerFramesOf(oc *compiler) []outerFrameInfo {
	var out []outerFrameInfo
	for ; oc != nil; oc = oc.outer {
		var (
			scopes  []tableScope
			cursors []int
		)
		for _, s := range oc.scopes {
			if s.colBase != 0 {
				continue
			}
			scopes = append(scopes, s.tableScope)
			cursors = append(cursors, s.cursor)
		}
		out = append(out, outerFrameInfo{scopes: scopes, cursors: cursors})
	}
	return out
}

// compileSubProgramCompound compiles a compound SELECT used as a subquery body
// or nested derived table into a Program whose Compound field carries the
// compiled compoundProgram (compileCompound). outer is threaded to every arm,
// so an arm can bind a correlated reference, as in C. Correlated is the OR over
// the arms, so the caller re-runs the whole compound per enclosing row
// (execWithParent).
func compileSubProgramCompound(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler) (*Program, error) {
	cp, err := compileCompound(pager, stmt, outer)
	if err != nil {
		return nil, err
	}
	return &Program{
		Compound:   cp,
		NResultCol: cp.arms[0].NResultCol,
		ColNames:   cp.arms[0].ColNames,
		Correlated: cp.correlated,
	}, nil
}

// selectNeedsNoRowSource reports whether stmt reads no table of its own: no
// FROM here or in any compound arm, and no WITH. Such a body compiles to a
// cursor-free program and needs no snapshot.
//
// It lets a compile with no pager still lower a scalar subquery, which C
// reaches from ATTACH's path argument (codeAttach, attach.c:403, "
// sqlite3ExprCode(pParse, pFilename, regArgs);"): "ATTACH (SELECT 'q.db') AS
// y" attaches q.db. A nested subquery that needs a table declines in the
// pager-less sub-compiler (this test, or resolveJoinSources' nil-pager guard).
func selectNeedsNoRowSource(stmt *SelectStmt) bool {
	if stmt == nil || len(stmt.From) > 0 || len(stmt.CTEs) > 0 {
		return false
	}
	for _, arm := range stmt.Compound {
		if !selectNeedsNoRowSource(arm.Stmt) {
			return false
		}
	}
	return true
}

// compileScalarSubquery compiles a scalar SubqueryExpr: its SelectStmt into a
// standalone sub-Program (which must yield exactly one result column, else we
// decline with SQLite's own "must return exactly 1 column" error), then an
// OpSubquery reading its first row's first column (or
// NULL for no rows) into a fresh register. An UNCORRELATED subquery runs once
// and is cached (a fresh cache slot); a CORRELATED one (prog.Correlated: it
// bound an outer column via c's scope chain) re-runs per evaluation against the
// enclosing frame, flagged p5Correlated and given no cache slot.
func (c *compiler) compileScalarSubquery(x SubqueryExpr) (int, error) {
	if c.pager == nil && c.liveDB == nil && !selectNeedsNoRowSource(x.Stmt) {
		return 0, fmt.Errorf("%w: scalar subquery outside a cursor-driven scope", errVDBEUnsupported)
	}
	prog, err := c.liveSubSelect(x.Stmt)
	if err != nil {
		return 0, err
	}
	if c.pager == nil && c.liveDB == nil && prog.NCursors != 0 {
		// Belt-and-braces on top of the structural test above: a program that
		// opened a cursor would read it off this VM's nil pager. The counter
		// is the compiler's own, so it cannot drift from what the opcodes
		// actually do -- the same guard, for the same reason, that
		// compileSelfRowExpr applies to its own cursor-free programs.
		return 0, fmt.Errorf("%w: scalar subquery outside a cursor-driven scope", errVDBEUnsupported)
	}
	if prog.NResultCol != 1 {
		// A PREPARE-time rejection C SQLite makes too, in its own words:
		// sqlite3SubselectError's "sub-select returns %d columns - expected %d"
		// (expr.c:3489-3501), reached here with the expected width always 1
		// (a row-value comparison is compileRowSubCompare's case, and a
		// mismatched one is "row value misused" -- rowValueMisusedErr above).
		return 0, semanticf("engine: sub-select returns %d columns - expected 1", prog.NResultCol)
	}
	d := c.alloc()
	if prog.Correlated {
		c.emit(Instruction{Op: OpSubquery, P1: d, P4: prog, P5: p5Correlated})
	} else {
		c.emit(Instruction{Op: OpSubquery, P1: d, P2: c.allocSub(), P4: prog})
	}
	return d, nil
}

// compileExistsSubquery compiles "[NOT] EXISTS (SELECT ...)": its SelectStmt
// into a standalone sub-Program (any column count is fine -- EXISTS ignores
// the rows' values), then an OpExists producing 1/0 (NOT inverting) into a
// fresh register. Uncorrelated: run once and cached; correlated
// (prog.Correlated): re-run per evaluation (p5Correlated, no cache slot).
func (c *compiler) compileExistsSubquery(x ExistsExpr) (int, error) {
	// A FROM-less body needs no pager, exactly as compileScalarSubquery's
	// does: "ATTACH EXISTS(SELECT 'x') AS zz" attaches in 3.53.3.
	if c.pager == nil && c.liveDB == nil && !selectNeedsNoRowSource(x.Stmt) {
		return 0, fmt.Errorf("%w: EXISTS subquery outside a cursor-driven scope", errVDBEUnsupported)
	}
	prog, err := c.liveSubSelect(x.Stmt)
	if err != nil {
		return 0, err
	}
	if c.pager == nil && c.liveDB == nil && prog.NCursors != 0 {
		// compileScalarSubquery's belt-and-braces, for the same reason.
		return 0, fmt.Errorf("%w: EXISTS subquery outside a cursor-driven scope", errVDBEUnsupported)
	}
	d := c.alloc()
	not := 0
	if x.Not {
		not = 1
	}
	if prog.Correlated {
		c.emit(Instruction{Op: OpExists, P1: d, P3: not, P4: prog, P5: p5Correlated})
	} else {
		c.emit(Instruction{Op: OpExists, P1: d, P2: c.allocSub(), P3: not, P4: prog})
	}
	return d, nil
}

// compileExpr emits code computing e and returns the register that will hold
// its value at run time.
func (c *compiler) compileExpr(e Expr) (int, error) {
	// "Consume on read": capture whatever compiler.inWhereConjunct stood at as
	// this compile begins, then clear it so every nested sub-compile below
	// (function args, a CASE arm, NOT's operand, a subquery's own compiler,
	// ...) defaults to false unless explicitly re-armed -- see that field's
	// doc comment. Only compileAndOr's AND branch re-arms it, and only for
	// its own two operands, mirroring sqlite3WhereSplit's recursive descent
	// through a chain of top-level ANDs.
	top := c.inWhereConjunct
	c.inWhereConjunct = false
	switch x := e.(type) {
	case whereFixedCol:
		// The planner's view of a column propagateConstants fixed
		// (where_plan_constprop.go), reaching code through a planner-built
		// expression: a WHERE_MULTI_OR disjunct's sub-scan filter. It is the
		// column it stands for; on every row the WHERE keeps, that column
		// equals the constant C would code (expr.c:5025).
		c.inWhereConjunct = top
		return c.compileExpr(x.col)
	case LiteralExpr:
		if x.deferredErr != "" {
			// A trigger-body literal whose magnitude check parsePrimary held
			// open (LiteralExpr.deferredErr's doc comment, sql_ast.go) is
			// reached here -- the choke point every expression compile
			// funnels through -- for real: raise it now, mirroring codeInteger
			// (expr.c:4330) being called only from sqlite3ExprCodeTarget.
			return 0, errors.New(x.deferredErr)
		}
		return c.compileLiteral(x.Val), nil

	case ParamExpr:
		r := c.alloc()
		c.emit(Instruction{Op: OpVariable, P1: x.Index, P2: r})
		return r, nil

	case UnaryExpr:
		return c.compileUnary(x)

	case BinaryExpr:
		return c.compileBinary(x, top)

	case IsNullExpr:
		return c.compileIsNull(x)

	case BetweenExpr:
		return c.compileBetween(x)

	case LikeExpr:
		return c.compileLike(x)

	case GlobExpr:
		return c.compileGlob(x)

	case MatchExpr:
		return c.compileMatchExpr(x)

	case InExpr:
		return c.compileIn(x)

	case CaseExpr:
		return c.compileCase(x)

	case CastExpr:
		r, err := c.compileExpr(x.X)
		if err != nil {
			return 0, err
		}
		// OpCast rewrites its register in place (vdbe.c:2162), so it must never target a
		// register something else still reads. C codes the operand into a fresh target
		// first (sqlite3ExprCode, expr.c:5173, 5899-5923). Copy here, since compileExpr
		// can return a register-backed row scope's own column register (CHECK,
		// generated columns, index expressions, RETURNING, "excluded", NEW/OLD):
		//
		//	CREATE TABLE t(a TEXT, b AS (CAST(a AS INTEGER) || '-' || a))
		//	INSERT INTO t(a) VALUES('5x')   -- b was '5-5', C says '5-5x'
		dst := c.alloc()
		c.emit(Instruction{Op: OpSCopy, P1: r, P2: dst})
		c.emit(Instruction{Op: OpCast, P1: dst, P4: x.Type})
		return dst, nil

	case FuncExpr:
		return c.compileFunc(x)

	case ColumnExpr:
		return c.compileColumn(x)

	case SubqueryExpr:
		return c.compileScalarSubquery(x)

	case ExistsExpr:
		return c.compileExistsSubquery(x)

	case CollateExpr:
		// COLLATE never changes the value, only which collation a consumer picks, and
		// consumers read it from the AST (resolveCompareCollation, topExprCollation),
		// so compile the operand.
		return c.compileExpr(x.X)

	case windowResultExpr:
		// A window function's value, already computed for this row. C does
		// exactly this and nothing else: sqlite3ExprCodeTarget's TK_FUNCTION
		// case returns pWin->regResult straight away for an EP_WinFunc node
		// (expr.c:5358-5360). See compiler.winRegs.
		if x.idx < 0 || x.idx >= len(c.winRegs) {
			return 0, fmt.Errorf("%w: window function outside a window projection", errVDBEUnsupported)
		}
		return c.winRegs[x.idx], nil

	case groupAggExpr:
		// One group's FINALIZED accumulator value. C codes this and nothing
		// else: sqlite3ExprCodeTarget's TK_AGG_FUNCTION case is "return
		// AggInfoFuncReg(pInfo, pExpr->iAgg);" (expr.c:5333/:5342) -- the
		// register finalizeAggFunctions' OP_AggFinal wrote (select.c:6786).
		return c.aggResultRegOrBuffer(aggRegAgg, x.accIdx, x, x.owner)

	case groupKeyExpr:
		// This group's GROUP BY key column, read back out of the sorter record
		// the compiled scan body built -- OP_Column off the sorting cursor in
		// C (select.c:8657), a register seeded from OpAggResult's P3 record
		// here. See compiler.aggRegs.
		return c.aggResultRegOrBuffer(aggRegKey, x.idx, x, x.owner)

	case groupBareColExpr:
		// A bare column of the group's ANCHOR ROW -- SQLite's
		// bare-columns-in-an-aggregate-query extension, which C accumulates
		// into AggInfoColumnReg under the regHit gate (updateAccumulator,
		// select.c:6955-6961) and reads back with TK_AGG_COLUMN's own "return
		// AggInfoColumnReg(pAggInfo, pExpr->iAgg);" (expr.c:4977/:4996).
		if x.isRowid {
			return c.aggResultRegOrBuffer(aggRegRowid, x.rowidTableIdx, x, x.owner)
		}
		return c.aggResultRegOrBuffer(aggRegCol, x.idx, x, x.owner)

	case RaiseExpr:
		// RAISE(IGNORE) needs no message; RAISE(ABORT/FAIL/ROLLBACK, msg)
		// evaluates the message into a register. OpRaise raises at run time.
		dst := c.allocN(1)
		if x.Ignore {
			c.emit(Instruction{Op: OpRaise, P1: int(conflictIgnore), P2: dst})
			return dst, nil
		}
		msgReg, err := c.compileExpr(x.Msg)
		if err != nil {
			return 0, err
		}
		c.emit(Instruction{Op: OpRaise, P1: int(x.Action), P2: msgReg})
		return dst, nil

	default:
		return 0, fmt.Errorf("%w: expression %T", errVDBEUnsupported, e)
	}
}

// regScope is one register-backed row scope (compiler.regScopes): the
// registers holding a row's columns (via scope.colIndex) and its rowid, for an
// expression reading a row held in registers rather than a cursor.
type regScope struct {
	scope    *tableScope
	regs     []int
	rowidReg int

	// mirrorsCursorScope marks a scope that re-publishes a FROM item c.scopes
	// already has, only to read it from registers (the sorted GROUP BY drain,
	// newAggDrainRow). affCtx must not append it: a second copy makes an
	// unqualified name "ambiguous" there, losing the column's collation and
	// affinity in comparisons.
	mirrorsCursorScope bool
}

// compileScope is one open cursor's column-resolution scope in a cursor-driven
// compile: the cursor plus the same tableScope resolveColumnEx uses, so column
// resolution (including rowid pseudo-columns and their shadowing) is the same
// lookup. One per FROM item, in FROM order.
type compileScope struct {
	tableScope
	cursor int

	// colBase is added to tableScope.colIndex's local index to get the OpColumn
	// index. Non-zero only for a member scope of a materialized parenthesized join
	// group, whose members share one cursor over the concatenated row. Kept out of
	// tableScope because its colIndex is also used to index cols directly for
	// naming.
	colBase int

	// resolvedFallback is a join-group member's coalesceFallback already resolved to
	// {cursor, colIdx} owners (resolveGroupSource). The raw fallback indexes the
	// group's internal scope list, which is wrong once members are flattened into
	// an enclosing list, so the raw lists are emptied. An enclosing RIGHT/FULL hop
	// appends its own owner, read after this chain (coalesceOwnersCombined). nil
	// otherwise.
	resolvedFallback map[string][]coalesceOwner
}

// scopesAffCtx builds the (schema-only, no row values) evalCtx exprAffinity
// needs to look up a ColumnExpr's declared affinity at compile time, across
// every open scope -- nil-safe (an empty/nil scopes works for a FROM-less
// compile, exactly the empty &evalCtx{} compAffinity/inAffinity built
// directly before compileScope existed).
func scopesAffCtx(scopes []compileScope) *evalCtx {
	if len(scopes) == 0 {
		return &evalCtx{}
	}
	tables := make([]tableScope, len(scopes))
	for i, s := range scopes {
		tables[i] = s.tableScope
	}
	return &evalCtx{tables: tables}
}

// trigPseudoRowScopes is the NEW/OLD half of affCtx, shared with outerSchemaCtx
// so a compound arm's dedup collation and its comparisons agree. Only the sides
// the event supplies (INSERT has no OLD, DELETE no NEW).
func trigPseudoRowScopes(trig *trigCompileCtx) []tableScope {
	if trig == nil {
		return nil
	}
	cols := trigPseudoRowCols(trig.cols)
	ci := buildColIndex(cols)
	var out []tableScope
	if trig.hasNew {
		out = append(out, tableScope{name: "new", cols: cols, colIndex: ci, noRowid: trig.noRowid})
	}
	if trig.hasOld {
		out = append(out, tableScope{name: "old", cols: cols, colIndex: ci, noRowid: trig.noRowid})
	}
	return out
}

// affCtx builds the schema-only evalCtx the affinity helpers resolve columns
// against, with an outer chain mirroring the compiler's, so a correlated
// operand gets its declared affinity as at run time.
func (c *compiler) affCtx() *evalCtx {
	return c.fillAffCtx(scopesAffCtx(c.scopes))
}

// enclosingAffCtx is affCtx without this compile's own FROM scopes: what a
// name the statement's FROM clause does not supply resolves against -- an
// enclosing query's row, a register-backed row (NEW/OLD, "excluded", the
// conflicting row), a live enclosing row. The planner binds such names as
// constants of the column's affinity and collation (wherePlanBoundOuter).
func (c *compiler) enclosingAffCtx() *evalCtx {
	return c.fillAffCtx(&evalCtx{})
}

func (c *compiler) fillAffCtx(ctx *evalCtx) *evalCtx {
	// The pager is what lets a SCALAR SUBQUERY operand report the affinity of
	// the column it selects (scalarSubqueryColumn resolves the subquery's own
	// FROM against it). It is read-only here -- nothing in the affinity path
	// executes anything -- and is nil on the write path, where the subquery
	// operand simply falls back to no affinity as before.
	ctx.pager = c.pager
	// Register-backed row scopes (CHECK, RETURNING, "excluded", NEW/OLD) carry
	// declared affinity and collation for comparisons inside them (check.test
	// 12.70). "excluded" publishes neither, as in C (resolve.c:581; see
	// excludedPseudoRowCols).
	for _, rs := range c.regScopes {
		if rs.scope != nil && !rs.mirrorsCursorScope {
			ctx.tables = append(ctx.tables, *rs.scope)
		}
	}
	// Inside a trigger body, expose NEW/OLD as resolvable scopes so a
	// comparison's collation (columnDeclaredCollation) picks up a NEW.col /
	// OLD.col column's DECLARED collation -- e.g. "WHEN new.a = 'a'" on a
	// COLLATE NOCASE column must compare case-insensitively (collate6.test).
	ctx.tables = append(ctx.tables, trigPseudoRowScopes(c.trig)...)
	// The compiler chain first, then the live enclosing-row scope (rowOuter) at the
	// level that holds it. compileColumn bakes such a reference in as a literal,
	// which carries no affinity or collation, so without this a comparison against
	// it silently changed meaning (e.g. a NOCASE column in an UPDATE's WHERE
	// subquery). It only resolves names nothing nearer resolved. Use c.rowOuter,
	// not outerRowCtx(): appending to the tail of the live row's own chain would
	// mutate the caller's evalCtx and create a cycle.
	switch {
	case c.outer != nil:
		ctx.outer = c.outer.affCtx()
	case c.rowOuter != nil:
		ctx.outer = c.rowOuter
	case c.bufOut != nil:
		// A window projection has no compiler chain, so a buffered batch column would
		// contribute no affinity or collation; use the scan body's chain, where it was
		// compiled. Metadata only: compiler.outer stays nil so no OpOuterColumn targets
		// finished cursors.
		ctx.outer = c.bufOut.aff
	}
	return ctx
}

// compAffinity computes the comparison affinity carried in P5 for "l <op> r" via
// comparisonAffinity (affinity.go), resolving operands against affCtx so a
// correlated column contributes its declared affinity.
func (c *compiler) compAffinity(l, r Expr) affinity {
	return comparisonAffinity(c.affCtx(), l, r)
}

// inAffinity computes the affinity applied to each IN-list element: X's own
// affinity, and only when it is TEXT or numeric -- the asymmetric IN rule
// (a comparison with a NONE-affinity right side), where the list element, not
// X, is the side coerced. X is resolved against the whole scope chain (affCtx),
// so a correlated X contributes its declared affinity.
func (c *compiler) inAffinity(x Expr) affinity {
	a := exprAffinity(c.affCtx(), x)
	if isNumericAffinity(a) {
		return affNumeric
	}
	if a == affText {
		return affText
	}
	return affNone
}

// trigCompileCtx carries the NEW/OLD schema for a trigger body: the columns,
// and whether NEW and OLD exist for the event. cols is a column list because a
// view's INSTEAD OF trigger has no table (viewColumnInfos); C treats both alike
// (trigger.c:1284). noRowid: a view has no visible rowid (build.c:3026), so
// NEW.rowid / OLD.rowid is "no such column" (resolve.c:564).
type trigCompileCtx struct {
	cols    []columnInfo
	noRowid bool
	hasNew  bool
	hasOld  bool
	// isTemp: a TEMP trigger's unqualified body targets search TEMP, MAIN, then
	// attachments on every firing (attach.c:495, 565; build.c:374), while a
	// non-TEMP trigger's are pinned to its own schema. Read by
	// compileRoutedTriggerBody.
	isTemp bool
	// memo is the statement compile's shared TriggerPrg list (SQLite's
	// pTop->pTriggerPrg). A body statement that fires triggers of its own must
	// compile them against THIS memo, not a fresh one, or a cyclic trigger
	// graph never terminates -- see triggerPrgMemo (vdbe_trigger.go).
	memo *triggerPrgMemo
	// orconf / orconfSet are codeTriggerProgram's pParse->eOrconf: a non-default
	// policy from the firing statement replaces each body statement's own clause
	// (trigger.c:1137). Zero leaves the body as written. Today only the REPLACE
	// victim delete sets it, to OE_Replace (insert.c:2339, 2613).
	orconf    conflictAction
	orconfSet bool
}

// resolveTriggerParam resolves a NEW.col/OLD.col reference to an OpParam read.
// It returns ok=false for a non-NEW/OLD reference (so the caller falls through
// to the ordinary scopes) and an error for a reference to the wrong side for
// this event (e.g. OLD in an INSERT trigger).
func (c *compiler) resolveTriggerParam(x ColumnExpr) (int, bool, error) {
	if x.Schema != "" && !c.ignoreDbQualifier {
		// A three-part reference never reaches the pseudo-row block (resolve.c:522,
		// "cnt==0 && zDb==0"), so "main.new.a" is "no such column". Inside a CHECK or
		// partial index the qualifier was already dropped (ignoreDbQualifier).
		return 0, false, nil
	}
	which := -1
	switch r33sFoldIdent(x.Qualifier) {
	case "new":
		if !c.trig.hasNew {
			return 0, false, fmt.Errorf("%w: no NEW row for this trigger event", errVDBEUnsupported)
		}
		which = 1
	case "old":
		if !c.trig.hasOld {
			return 0, false, fmt.Errorf("%w: no OLD row for this trigger event", errVDBEUnsupported)
		}
		which = 0
	default:
		return 0, false, nil
	}
	dst := c.allocN(1)
	// An ORDINARY COLUMN named rowid/oid/_rowid_ SHADOWS the rowid
	// pseudo-column, as in every other scope: the named-column lookup comes
	// first (triggerD.test: NEW.rowid/oid/_rowid_ on t1(rowid, oid, _rowid_,
	// x) are the columns).
	col := -1
	if idx, ok := buildColIndex(c.trig.cols)[r33sFoldIdent(x.Name)]; ok {
		col = idx
	} else if !isRowidAliasName(x.Name) || c.trig.noRowid {
		// noRowid: a VIEW's INSTEAD OF trigger. Its OLD/NEW row is the view's
		// output, which has no rowid, so the pseudo-column names fall through
		// to the same "no such column" viewOldNewScopes' noRowid reports
		// (view_trigger.go). Declining rather than
		// reading a rowid register that, for a view, holds nothing.
		// resolve.c:785-796 -- see the qualified arm in compileColumn for the
		// full rule; semanticf because C rejects this at prepare time.
		return 0, false, semanticf("engine: no such column: %s.%s", x.Qualifier, x.Name)
	}
	c.emit(Instruction{Op: OpParam, P1: which, P2: col, P3: dst})
	return dst, true, nil
}

// exclCompileCtx describes an upsert's "excluded" row to compiles nested in its
// DO UPDATE SET list. The SET expressions themselves read it from registers
// (resolveRowReg, as C's resolve.c:572); a subquery has its own register file,
// so it reads the row via OpParam from OpExcludedRow's snapshot.
//
// Only qualified names reach it (resolve.c:547). The target table's own FROM
// item is searched first (resolve.c:521), so a target or alias named "excluded"
// wins (upsert3.test); a sub-compile has no scope stack for that, so it records
// the collision and declines.
type exclCompileCtx struct {
	cols     []columnInfo
	noRowid  bool
	shadowed bool
}

// trigPseudoRowCols is cols with declared affinity dropped and collation kept:
// what NEW.<col>/OLD.<col> contributes to a comparison. C's TK_TRIGGER
// reference has no affinity (sqlite3ExprAffinity has no TK_TRIGGER arm,
// expr.c:90) but keeps collation (sqlite3ExprCollSeq does, expr.c:255). The IPK
// is folded to rowid (resolve.c:562) and keeps INTEGER affinity (resolve.c:602).
// Returns a copy.
func trigPseudoRowCols(cols []columnInfo) []columnInfo {
	out := make([]columnInfo, len(cols))
	copy(out, cols)
	for i := range out {
		if out[i].IsRowidAlias {
			continue
		}
		out[i].Aff, out[i].NoAffinity = affNone, true
	}
	return out
}

// excludedPseudoRowCols is cols with declared affinity and collation both
// dropped, including the NoAffinity / NoCollation bits: C rewrites
// excluded.<col> to a bare TK_REGISTER with no column identity
// (resolve.c:582), so sqlite3ExprCollSeq yields BINARY (expr.c:254, 302) and
// sqlite3ExprAffinity "not a column" (expr.c:45). Clearing only the values is
// not enough: an empty Collation is still a declared BINARY that stops
// collation fall-through (expr.c:436), and affNone still counts as a column
// for sqlite3CompareAffinity (expr.c:356). The same scope serves the SET list
// and SET subqueries, so both spellings agree. Returns a copy.
func excludedPseudoRowCols(cols []columnInfo) []columnInfo {
	out := make([]columnInfo, len(cols))
	copy(out, cols)
	for i := range out {
		out[i].Aff, out[i].Collation = affNone, ""
		out[i].NoAffinity, out[i].NoCollation = true, true
	}
	return out
}

// retRowCompileCtx describes a RETURNING clause's affected row to compiles nested
// in its output expressions, resolve.c's third pseudo-row arm (resolve.c:528):
// an unqualified name or one qualified by the target table's own name (never
// the alias). The reference becomes a register read (resolve.c:587), which
// OpPseudoRow snapshots.
type retRowCompileCtx struct {
	cols      []columnInfo
	tableName string
	noRowid   bool
}

// emitReturningParam resolves a reference to the RETURNING clause's affected
// row against ONE NameContext level's context (rc, that level's own
// compiler.retRow) and emits an OpParam read of it (P1 == 3) INTO THIS
// compile's program. It reports ok=false for every other reference.
//
// Reached only from a SUBQUERY inside the RETURNING list: emitReturning pushes
// the row as a regScope, so the list's own expressions are answered by
// resolveRowReg long before this. See retRowCompileCtx for the C.
func (c *compiler) emitReturningParam(rc *retRowCompileCtx, x ColumnExpr) (int, bool, error) {
	if rc == nil {
		return 0, false, nil
	}
	if x.Schema != "" {
		// A THREE-part reference never reaches the pseudo-row block at all:
		// resolve.c:521 gates the whole thing on "cnt==0 && zDb==0", so
		// "main.t.a" inside a RETURNING subquery is an ordinary schema-
		// qualified name that has to find a real FROM item or fail. Serving
		// the affected row here would answer a name 3.53.3 rejects.
		return 0, false, nil
	}
	if x.Qualifier != "" && !equalFoldName(x.Qualifier, rc.tableName) {
		return 0, false, nil
	}
	col := -1
	if idx, ok := buildColIndex(rc.cols)[r33sFoldIdent(x.Name)]; ok {
		col = idx
	} else if !isRowidAliasName(r33sFoldIdent(x.Name)) || rc.noRowid {
		// Unqualified and not a column of the target: not this row's name at
		// all, so it falls through to whatever else is in scope (and, finding
		// nothing, to the ordinary "no such column"). A QUALIFIED miss is the
		// same fall-through: C reaches the pseudo-row block only after the
		// FROM-item walk failed, and failing here too is what it does next.
		return 0, false, nil
	}
	dst := c.allocN(1)
	c.emit(Instruction{Op: OpParam, P1: 3, P2: col, P3: dst})
	return dst, true, nil
}

// enclosingRetRowAnswers reports whether a RETURNING clause's affected row at
// some level of this compile's outer chain answers x -- emitReturningParam's
// test, asked without emitting anything.
func (c *compiler) enclosingRetRowAnswers(x ColumnExpr) bool {
	for cc := c; cc != nil; cc = cc.outer {
		rc := cc.retRow
		if rc == nil || x.Schema != "" || (x.Qualifier != "" && !equalFoldName(x.Qualifier, rc.tableName)) {
			continue
		}
		if _, ok := buildColIndex(rc.cols)[r33sFoldIdent(x.Name)]; ok || (isRowidAliasName(r33sFoldIdent(x.Name)) && !rc.noRowid) {
			return true
		}
	}
	return false
}

// enclosingExcl reports the upsert "excluded" row this compile can reach at
// SOME level of its outer chain -- the "is this name answerable at all" form of
// the per-level lookup compileColumn does. Its one caller is the aggregate
// binder's pseudo-row carve-out (bindAggOuterRefsReason, vdbe_agg_codegen.go),
// which asks that question and not which level answers.
func (c *compiler) enclosingExcl() *exclCompileCtx {
	for cc := c; cc != nil; cc = cc.outer {
		if cc.excl != nil {
			return cc.excl
		}
	}
	return nil
}

// emitExcludedParam resolves "excluded.<col>" against one level's upsert context
// and emits an OpParam read of the proposed row (P1 == 2). ok=false for any
// other reference. No frame crossing: execOuterTrig copied the row down. Column
// mapping mirrors resolveRowReg (real column, then rowid pseudo-column).
func (c *compiler) emitExcludedParam(ec *exclCompileCtx, x ColumnExpr) (int, bool, error) {
	// x.Schema != "" is the same "cnt==0 && zDb==0" gate emitReturningParam
	// states (resolve.c:521): "main.excluded.v" is a schema-qualified name, and
	// 3.53.3 answers it "no such column" rather than reaching the upsert arm.
	if ec == nil || ec.shadowed || x.Schema != "" || x.Qualifier == "" || !equalFoldName(x.Qualifier, "excluded") {
		return 0, false, nil
	}
	col := -1
	if idx, ok := buildColIndex(ec.cols)[r33sFoldIdent(x.Name)]; ok {
		col = idx
	} else if !isRowidAliasName(r33sFoldIdent(x.Name)) || ec.noRowid {
		return 0, false, semanticf("engine: no such column: %s.%s", x.Qualifier, x.Name)
	}
	dst := c.allocN(1)
	c.emit(Instruction{Op: OpParam, P1: 2, P2: col, P3: dst})
	return dst, true, nil
}

func (c *compiler) pushRegScope(rs regScope) { c.regScopes = append(c.regScopes, rs) }
func (c *compiler) popRegScope()             { c.regScopes = c.regScopes[:len(c.regScopes)-1] }

// hasOpaqueRegScope reports whether this compile publishes a register row a
// schema-only walk of its cursor scopes would not see ("excluded",
// CHECK/RETURNING, an aggregate anchor row). Mirror scopes do not count. Used by
// outerSchemaCtx.
func (c *compiler) hasOpaqueRegScope() bool {
	for _, rs := range c.regScopes {
		if !rs.mirrorsCursorScope {
			return true
		}
	}
	return false
}

// resolveRowReg resolves x against the register-backed row scopes. The error
// result is "ambiguous column name" (resolve.c:784) when a name matches more
// than one scope at one level; only regScopeStrict produces it. It is an error,
// not "not found", because not found would send compileColumn outward where an
// enclosing column might answer what C rejects. Returned unwrapped, like
// resolveInScopes.
func (c *compiler) resolveRowReg(x ColumnExpr) (int, bool, error) {
	reg, hits := 0, 0
	for i := len(c.regScopes) - 1; i >= 0; i-- {
		rs := c.regScopes[i]
		// "excluded" is reachable only by its bare two-part qualifier, never
		// unqualified or schema-qualified (resolve.c:522), the same gate
		// emitExcludedParam and resolveTriggerParam apply.
		if rs.scope.unqualifiedHidden && (x.Qualifier == "" || x.Schema != "") {
			continue
		}
		if x.Qualifier != "" && !equalFoldName(x.Qualifier, rs.scope.name) {
			continue
		}
		hit := -1
		if idx, ok := rs.scope.colIndex[r33sFoldIdent(x.Name)]; ok {
			// A USING/NATURAL duplicate is not a second match for an unqualified name
			// (resolve.c:438-449); tableScope.coalesced records it, as resolveColumn and
			// resolveInScopes already honour.
			if _, dup := rs.scope.coalesced[r33sFoldIdent(x.Name)]; dup && x.Qualifier == "" {
				continue
			}
			// A name held only through a RIGHT/FULL coalesce fallback cannot come from a
			// register: C picks at run time (resolve.c:450, extendFJMatch at :456) because
			// the representative may be NULL-extended. Decline this reference only.
			if _, fb := rs.scope.coalesceFallback[r33sFoldIdent(x.Name)]; fb {
				return 0, false, fmt.Errorf("%w: %s names a RIGHT/FULL JOIN coalesced column",
					errVDBEUnsupported, x.Name)
			}
			hit = rs.regs[idx]
		} else if !rs.scope.noRowid && isRowidAliasName(r33sFoldIdent(x.Name)) {
			// A real column of that name SHADOWS the pseudo-column, which the
			// colIndex arm above already took -- the same order resolveColumnEx
			// uses.
			hit = rs.rowidReg
		}
		if hit < 0 {
			continue
		}
		if hits == 0 {
			reg = hit
		}
		hits++
		// Innermost wins, so the first hit found walking DOWN the stack is the
		// answer; only regScopeStrict needs to know whether a second exists.
		if !c.regScopeStrict {
			return reg, true, nil
		}
	}
	// Reached only when regScopeStrict is set (the loop returns on the first
	// hit otherwise), so exactly one match is the answer and a second is the
	// "ambiguous column name" C SQLite raises (resolve.c:784).
	if hits > 1 {
		return 0, false, fmt.Errorf("engine: ambiguous column name: %s", x.Name)
	}
	if hits != 1 {
		return 0, false, nil
	}
	return reg, true, nil
}

// outerRowCtx returns the nearest live enclosing-row evalCtx on this compile's
// chain (compiler.rowOuter), or nil. Walking the chain -- rather than reading
// c.rowOuter alone -- is what lets a subquery compiled BENEATH a FROM-less
// trigger-body SELECT still see the firing row: only the outermost compile is
// handed the evalCtx, and compileSubProgram threads itself in as that
// subquery's `outer`. tkt3554.test needs exactly that: "SELECT RAISE(IGNORE)
// WHERE EXISTS (SELECT obj FROM test WHERE obj = new.obj ...)".
func (c *compiler) outerRowCtx() *evalCtx {
	for cc := c; cc != nil; cc = cc.outer {
		if cc.rowOuter != nil {
			return cc.rowOuter
		}
	}
	return nil
}

// resolveRowCtxValue reads x out of a live enclosing-row evalCtx, reporting
// ok=false (no error) when the name does not resolve there, so the caller can
// decline. A resolution that fails otherwise (a pseudo-column with no row) is
// a real rejection, returned as errVDBESemantic so it surfaces as the
// statement's error.
//
// resolveColumnEx already locates the value (owning scope, then a row index or
// rowid slot); this just reads it. A FallbackLiteral applies only on failure,
// which the caller handles.
//
// The fts5 "rank" intercept comes first, as in columnRowValue: where an aux
// context is live, "rank" is the row's bm25() score even if a column has that
// name.
func resolveRowCtxValue(row *evalCtx, x ColumnExpr) (Value, bool, error) {
	foundCtx, idx, _, rowidTableIdx, err := resolveColumnEx(row, x)
	if err != nil {
		return Value{}, false, nil
	}
	if row.fts5Aux != nil && x.Schema == "" && equalFoldName(x.Name, "rank") && row.fts5Aux.matchesRankQualifier(x.Qualifier) {
		return row.fts5Aux.bm25(row, nil), true, nil
	}
	noRow := func() (Value, bool, error) {
		return Value{}, false, semanticf("engine: no such column: %s (no row context available)", x.Name)
	}
	if rowidTableIdx >= 0 {
		if rowidTableIdx >= len(foundCtx.rowids) {
			return noRow()
		}
		return foundCtx.rowids[rowidTableIdx], true, nil
	}
	if idx >= len(foundCtx.vals) {
		// A correlated reference into a collapsed GROUP BY group resolves to
		// the group's (uniform) key value -- see groupKeyVals.
		if v, ok := foundCtx.groupKeyVals[idx]; ok {
			return v, true, nil
		}
		return noRow()
	}
	return foundCtx.vals[idx], true, nil
}

// compileColumn resolves x against c.scopes, then each enclosing compiler's
// scopes outward, and emits the read: OpColumn/OpRowid for this query's
// cursors, OpOuterColumn/OpOuterRowid (P5 = levels out) for a correlated
// reference.
//
// Within a level, a qualified name matches a scope by name and must then exist
// there (a miss is an error, not a fall-through); an unqualified name must
// match at most one scope's real column, else at most one rowid
// pseudo-column, or it is ambiguous. Only a level matching nothing falls
// outward. A correlated match marks every compiler up to the owner correlated
// and requires the owner's cursors to be live (rowLive), else it declines.
func (c *compiler) compileColumn(x ColumnExpr) (int, error) {
	// The fts5 "rank" hidden column resolves to the row's bm25() score (or
	// NULL, with no MATCH -- see compileFts5Rank) rather than a real column,
	// exactly mirroring columnRowValue's identical ColumnExpr
	// shortcut (row_scope.go) and validateColumnRefs' matching
	// carve-out (query.go). Checked first because "rank" is not in scope.cols
	// at all -- there is no real column for it to shadow.
	if c.fts5Aux != nil && x.Schema == "" && equalFoldName(x.Name, "rank") && c.fts5Aux.matchesRankQualifier(x.Qualifier) {
		return c.compileFts5Rank()
	}
	// A three-part "schema.table.column" reference: validate the database
	// qualifier as validateColumnRefs does (qualifierResolves -- this
	// pager's schema or one of its attachedReaders), then resolve the
	// plain "Qualifier.Name" form; resolveInScopes ignores x.Schema. This
	// re-validates because a foreign view's stored SELECT compiles here too
	// and the top-level validateSelectSchemaQualifiers pass never walks it.
	// An unresolvable qualifier is a real rejection (errVDBESemantic). The
	// exception is c.ignoreDbQualifier, C's (NC_PartIdx|NC_IsCheck)
	// carve-out at resolve.c:316.
	if x.Schema != "" && !c.ignoreDbQualifier {
		switch {
		case c.pager != nil:
			ok, serr := c.pager.qualifierResolves(x.Schema)
			if serr != nil {
				return 0, semanticf("%v", serr)
			}
			if !ok {
				return 0, semanticf("engine: no such column: %s.%s.%s", x.Schema, x.Qualifier, x.Name)
			}
		case c.ownSchema != "" && ownSchemaQualifier(c.ownSchema, x.Schema):
			// No pager but a live write session: the qualifier is still decidable,
			// since it needs only the schema names. lookupName's qualifier loop
			// just maps zDb to a pSchema by name (resolve.c:319-330, with :330's
			// "main" fallback); a table missing from it is then "no such column".
			//
			// Only the session's own schema (under localSchema's name) and temp
			// are accepted here, which is what the pager arm would conclude. An
			// attached or unknown name keeps the decline: the read path accepts an
			// attached name, but a write session cannot route there
			// (checkWriteSchemaQualifier), and erroring would fail an answerable
			// statement.
		default:
			return 0, fmt.Errorf("%w: schema-qualified column reference outside a cursor-driven scope", errVDBEUnsupported)
		}
	}
	if c.trig != nil {
		if reg, ok, terr := c.resolveTriggerParam(x); terr != nil {
			return 0, terr
		} else if ok {
			return reg, nil
		}
	}
	// Before resolveRowReg, because resolveRowReg resolves by NAME with
	// innermost-wins and the drain resolves the way the cursor path does. On a
	// USING/NATURAL join the two disagree about which arm the name means, and
	// the drain is the one that is right. See compileDrainColumn.
	if c.drain != nil {
		if reg, ok := c.drain.compileDrainColumn(c, x); ok {
			return reg, nil
		}
	}
	if len(c.regScopes) != 0 {
		reg, ok, amb := c.resolveRowReg(x)
		if amb != nil {
			return 0, amb
		}
		if ok {
			return reg, nil
		}
	}
	// A window query's outer projection BUFFERS a reference to an ENCLOSING
	// query as a batch column rather than declining it -- C's own answer for
	// every column reference in such a projection (selectWindowRewriteExprCb,
	// window.c:788-817). Asked AFTER the register block, so this query's own
	// row still wins, and it refuses any name that block could have offered
	// (windowBufCols.slot). nil in every other compile.
	if reg, ok := c.bufOut.slot(c, x); ok {
		return reg, nil
	}
	cursor, colIdx, isRowid, found, fb, hard := resolveInScopes(c.scopes, x, c.pager)
	if hard != nil {
		return 0, hard
	}
	if found {
		if fb.has {
			return c.emitColumnReadCoalesce(cursor, colIdx, fb.owners), nil
		}
		return c.emitColumnRead(cursor, colIdx, isRowid, 0), nil
	}
	// An upsert's "excluded" pseudo-row, tried at THIS level once this level's
	// own FROM items have failed -- resolve.c's own placement, since the block
	// at :521 is gated on "cnt==0" and sits inside the do/while that walks the
	// NameContext chain outward. So a FROM item genuinely named "excluded"
	// still wins over the pseudo-table, at whichever level owns it. Only the
	// compile that PUSHED the DO UPDATE's register scopes carries the context
	// today, and there resolveRowReg has already answered above, so this is
	// reached only from a SUBQUERY written in a SET right-hand side.
	if reg, ok, eerr := c.emitExcludedParam(c.excl, x); eerr != nil {
		return 0, eerr
	} else if ok {
		return reg, nil
	}
	// A RETURNING clause's affected row, the third arm of the same resolve.c
	// block (:528) and tried in the same place, for the same reason.
	if reg, ok, rerr := c.emitReturningParam(c.retRow, x); rerr != nil {
		return 0, rerr
	} else if ok {
		return reg, nil
	}

	level := 1
	for oc := c.outer; oc != nil; oc = oc.outer {
		// An enclosing compile's register-backed row, tried before its cursor
		// scopes (as for this compile's own). The read crosses a frame, so it is
		// OpOuterAggReg.
		//
		// For a window query's outer projection this is C's lift:
		// selectWindowRewriteExprCb moves a sub-select's references to outer FROM
		// cursors into the ephemeral row (window.c:756-819), held in registers.
		//
		// Gated on rowLive (the projection's block copies a live row) or drain !=
		// nil (the sorted GROUP BY drain, where the block is the only live row; C
		// reads the sorter record, expr.c:7454, 4996; see
		// aggDrainRow.lowerable). Otherwise the cursor arm runs, which declines
		// when not rowLive.
		if oc.drain != nil {
			// The drain's own resolver, NOT resolveRowReg. The two disagree
			// about which arm of a USING/NATURAL join a bare name means --
			// resolveRowReg is innermost-wins, resolveInScopes applies the
			// join's rule -- and that disagreement was a measured wrong answer
			// on this very block (see aggDrainRow.slotFor). A name the drain
			// cannot serve falls through to the cursor arm below, which
			// declines it because this compile is not row-live.
			if reg, ok := oc.drain.outerSlotReg(oc, x); ok {
				for p := c; p != oc; p = p.outer {
					p.correlated = true
				}
				d := c.alloc()
				c.emit(Instruction{Op: OpOuterAggReg, P1: reg, P3: d, P5: uint16(level)})
				return d, nil
			}
			// A RIGHT/FULL JOIN's USING/NATURAL column: aggDrainRow.
			// compileCoalesce's chain, with each arm's row-block register
			// fetched across the frame first. The fetches are unconditional
			// rather than behind the OpNotNull jumps -- a register copy cannot
			// raise, so hoisting them changes nothing but the instruction
			// count, and it keeps the jump chain identical to the one the
			// drain's own compile emits.
			if regs, ok := oc.drain.outerCoalesceRegs(oc, x); ok {
				for p := c; p != oc; p = p.outer {
					p.correlated = true
				}
				local := make([]int, len(regs))
				for i, r := range regs {
					local[i] = c.alloc()
					c.emit(Instruction{Op: OpOuterAggReg, P1: r, P3: local[i], P5: uint16(level)})
				}
				d := c.alloc()
				c.emit(Instruction{Op: OpSCopy, P1: local[0], P2: d})
				skips := make([]int, 0, len(local))
				for _, l := range local[1:] {
					skips = append(skips, c.emit(Instruction{Op: OpNotNull, P1: d}))
					c.emit(Instruction{Op: OpSCopy, P1: l, P2: d})
				}
				end := c.here()
				for _, j := range skips {
					c.patch(j, end)
				}
				return d, nil
			}
		} else if oc.rowLive && len(oc.regScopes) != 0 {
			reg, ok, amb := oc.resolveRowReg(x)
			if amb != nil {
				return 0, amb
			}
			if ok {
				// Mark correlated up to (not including) the owning compile,
				// exactly as the cursor arm below does. It is not an
				// optimisation: without it the enclosing sub-Program is run
				// through runSubOnce, whose exec has NO parent frame, and the
				// OpOuterAggReg would walk off a nil one.
				for p := c; p != oc; p = p.outer {
					p.correlated = true
				}
				d := c.alloc()
				c.emit(Instruction{Op: OpOuterAggReg, P1: reg, P3: d, P5: uint16(level)})
				return d, nil
			}
		}
		// The same enclosing compile's buffering seam, asked in the order it
		// asks itself (regScopes, then bufOut): a window query's projection
		// buffers a reference it cannot serve from its register block as a batch
		// column (windowBufCols). Reached from a subquery inside that projection,
		// whose own compiler has no bufOut.
		//
		// C reads such a reference off the enclosing cursor
		// (selectWindowRewriteExprCb prunes the sub-select, window.c:756-771),
		// since it codes the sub-select inside the open query. Buffering gives
		// the same value: the enclosing row does not move during the scan (why
		// window.c:1080 may mark the sub-select correlated). The register lands
		// in the owning compile's block and is read across the same frames, with
		// the same OpOuterAggReg and correlated marking.
		if reg, ok := oc.bufOut.slot(oc, x); ok {
			for p := c; p != oc; p = p.outer {
				p.correlated = true
			}
			d := c.alloc()
			c.emit(Instruction{Op: OpOuterAggReg, P1: reg, P3: d, P5: uint16(level)})
			return d, nil
		}
		cursor, colIdx, isRowid, found, fb, hard = resolveInScopes(oc.scopes, x, oc.pager)
		if hard != nil {
			return 0, hard
		}
		if found {
			if !oc.rowLive {
				// Never emit OpOuterColumn against a frame whose cursor is not on the
				// correlated row. Row-mode scan bodies (and the aggregate, window and write
				// spans that declare rowLive) restore the flag afterwards, so an aggregate's
				// result phase or a write's RETURNING is refused here.
				return 0, fmt.Errorf("%w: correlated reference %q to a non-row-live outer scope", errVDBEUnsupported, x.Name)
			}
			if fb.has {
				// A RIGHT/FULL JOIN's coalesced column referenced from an
				// outer/correlated scope: OpOuterColumn has no coalesce-aware
				// counterpart (emitColumnReadCoalesce's OpNotNull/OpColumn
				// chain only ever targets THIS frame's own cursors) -- a
				// genuinely rare combination not worth that codegen
				// complexity; decline cleanly (no fallback exists) instead of
				// risking reading the wrong (uncoalesced) side.
				return 0, fmt.Errorf("%w: RIGHT/FULL JOIN coalesced column %q referenced from an outer/correlated scope", errVDBEUnsupported, x.Name)
			}
			for p := c; p != oc; p = p.outer {
				p.correlated = true
			}
			return c.emitColumnRead(cursor, colIdx, isRowid, level), nil
		}
		// This enclosing level's own upsert "excluded" row -- see the same
		// lookup at this compile's own level above. The read needs no frame
		// crossing and no correlated marking: OpParam takes the row off the
		// running MACHINE (vdbe.trigExcluded, seeded by OpExcludedRow and
		// copied into every sub-machine by execOuterTrig), not off a register
		// of the enclosing frame.
		if reg, ok, eerr := c.emitExcludedParam(oc.excl, x); eerr != nil {
			return 0, eerr
		} else if ok {
			return reg, nil
		}
		if reg, ok, rerr := c.emitReturningParam(oc.retRow, x); rerr != nil {
			return 0, rerr
		} else if ok {
			return reg, nil
		}
		level++
	}

	// Last resort: a live enclosing-row evalCtx (rowOuter), e.g. a firing
	// trigger's NEW/OLD row. Its values are baked in as literals (never cached).
	// Tried before the qualifier/no-such-column errors so "new.a" reaches it, and
	// before FallbackLiteral so a real column named "true" shadows the keyword,
	// as columnRowValue orders them (row_scope.go).
	if row := c.outerRowCtx(); row != nil {
		if v, ok, rerr := resolveRowCtxValue(row, x); rerr != nil {
			return 0, rerr
		} else if ok {
			// Resolving against the live enclosing row is still outer context, so mark
			// every compiler passed through as correlated, as C sets isCorrelated whenever
			// a sub-select touched the outer NameContext (resolve.c:1953).
			// resolveCorrelatedCTESource trusts prog.Correlated (with3.test).
			for p := c; p != nil && p.rowOuter == nil; p = p.outer {
				p.correlated = true
			}
			return c.compileLiteral(v), nil
		}
	}
	if x.Qualifier != "" {
		// resolve.c:785-796 builds ONE message for every unresolved reference, from the
		// parts the user WROTE: "no such column: %s.%s.%s" with a schema, "%s.%s"
		// with a table, "%s" bare. lookupName never separates "the table is not in
		// scope" from "the table has no such column" -- both are cnt==0 -- so an
		// unknown QUALIFIER is still "no such column: nosuch.a", not "no such
		// table". semanticf because C rejects it at prepare time.
		if x.Schema != "" {
			return 0, semanticf("engine: no such column: %s.%s.%s", x.Schema, x.Qualifier, x.Name)
		}
		return 0, semanticf("engine: no such column: %s.%s", x.Qualifier, x.Name)
	}
	// An unquoted TRUE/FALSE that resolves to no column anywhere falls back to 1/0,
	// as the row read does (columnRowValue). Only a plain "no such column" miss
	// triggers it.
	if x.FallbackLiteral != nil {
		return c.compileLiteral(*x.FallbackLiteral), nil
	}
	// A bare name that binds nowhere is C's prepare-time "no such column"
	// (resolve.c:785). errVDBESemantic, so a write statement fails before running
	// and leaves changes() untouched, as C publishes nothing for a VM that never
	// reached RUN state (vdbeaux.c:3335, 3481). Qualified forms keep declining:
	// the write compiler declines some shapes before building the scope set a
	// qualifier is resolved against.
	return 0, semanticf("engine: no such column: %s", x.Name)
}

// emitColumnRead emits the read of an already-resolved column/rowid: at level 0
// (this query's own cursors) an ordinary OpColumn/OpRowid; at an outer level (a
// correlated reference into an ancestor frame) an OpOuterColumn/OpOuterRowid
// carrying that level in P5. It returns the fresh register holding the value.
func (c *compiler) emitColumnRead(cursor, colIdx int, isRowid bool, level int) int {
	r := c.alloc()
	switch {
	case level == 0 && isRowid:
		c.emit(Instruction{Op: OpRowid, P1: cursor, P2: r})
	case level == 0:
		c.emit(Instruction{Op: OpColumn, P1: cursor, P2: colIdx, P3: r})
	case isRowid:
		c.emit(Instruction{Op: OpOuterRowid, P1: cursor, P2: r, P5: uint16(level)})
	default:
		c.emit(Instruction{Op: OpOuterColumn, P1: cursor, P2: colIdx, P3: r, P5: uint16(level)})
	}
	// A rowid is an integer and has nowhere to put a subtype; only a COLUMN
	// needs the record round-trip's loss reproduced. See compiler.stripRowSubtype.
	if !isRowid && c.stripsRowSubtypeAt(level) {
		c.emit(Instruction{Op: OpClearSubtype, P1: r})
	}
	return r
}

// stripsRowSubtypeAt reports whether a column read LEVEL frames out from this
// compile lands in a row whose subtype the reading block must drop -- i.e.
// whether the compile that OWNS that row is inside its stripRowSubtype block.
// Level 0 is this compile; level N is the Nth enclosing one, matching
// OpOuterColumn's P5 exactly (compileColumn counts the same hops up c.outer).
func (c *compiler) stripsRowSubtypeAt(level int) bool {
	cc := c
	for ; level > 0 && cc != nil; level-- {
		cc = cc.outer
	}
	return cc != nil && cc.stripRowSubtype
}

// emitColumnReadCoalesce emits a RIGHT/FULL JOIN coalesced column read:
// COALESCE(representative, owners[0], owners[1], ...), owners in FROM order
// (installCoalesceFallback accumulates every hop on one representative; a
// chain through a parenthesized join group is already flattened by
// coalesceOwnersCombined). The choice depends on NULLs at run time, so it is
// conditional bytecode with a shared exit at the first non-NULL value, like
// coalesce().
func (c *compiler) emitColumnReadCoalesce(cursor, colIdx int, owners []coalesceOwner) int {
	r := c.alloc()
	c.emit(Instruction{Op: OpColumn, P1: cursor, P2: colIdx, P3: r})
	skips := make([]int, 0, len(owners))
	for _, o := range owners {
		skips = append(skips, c.emit(Instruction{Op: OpNotNull, P1: r}))
		c.emit(Instruction{Op: OpColumn, P1: o.cursor, P2: o.colIdx, P3: r})
	}
	end := c.here()
	for _, j := range skips {
		c.patch(j, end)
	}
	// Every arm of the chain above is one of THIS frame's own columns, so the
	// grouped strip applies to whichever one won -- emitted after the merge
	// point so it runs exactly once (see compiler.stripRowSubtype).
	if c.stripsRowSubtypeAt(0) {
		c.emit(Instruction{Op: OpClearSubtype, P1: r})
	}
	return r
}

// qualifiedScopeAmbiguous reports whether a scope other than scopes[at] with the
// same name also exposes lname, which makes a qualified reference ambiguous in
// C (sameNamedScopeAlsoExposes is the same rule for "*"). filterByDB restricts
// it to the same database when the reference was schema-qualified: "main.t4.a"
// and "aux1.t4.a" are distinct (select.c:6304, resolve.c:125; selectD.test 2.4).
func qualifiedScopeAmbiguous(scopes []compileScope, at int, lname string, wantDBIdx int, filterByDB bool) bool {
	for j := range scopes {
		if j == at || !equalFoldName(scopes[j].name, scopes[at].name) {
			continue
		}
		if filterByDB && scopes[j].dbIdx != wantDBIdx {
			continue
		}
		if _, ok := scopes[j].colIndex[lname]; !ok {
			continue
		}
		if scopes[j].coalesced != nil {
			if _, hidden := scopes[j].coalesced[lname]; hidden {
				continue
			}
		}
		return true
	}
	return false
}

// coalesceOwner is one fallback owner in a coalesceFallbackRef's chain: the
// cursor/column to read for one RHS argument of the COALESCE(representative,
// owners[0].col, owners[1].col, ...) emitColumnReadCoalesce builds.
type coalesceOwner struct {
	cursor, colIdx int
}

// coalesceFallbackRef is resolveInScopes' report of a column's RIGHT/FULL JOIN
// coalesce chain: empty normally, one owner per RIGHT/FULL hop coalescing the
// same USING/NATURAL column onto this representative, in FROM order.
type coalesceFallbackRef struct {
	owners []coalesceOwner
	has    bool
}

// coalesceOwnersFor builds the fallback owners for lname from
// tableScope.coalesceFallback[lname] (ascending FROM order), stopping at limit
// (x.UsingReprOwnItem for a desugared USING condition, so a hop never coalesces
// against itself; math.MaxInt otherwise). A group connector can have both kinds
// of fallback; see coalesceOwnersCombined.
func coalesceOwnersFor(scopes []compileScope, fallback []int, lname string, limit int) []coalesceOwner {
	var owners []coalesceOwner
	for _, ownerIdx := range fallback {
		if ownerIdx >= limit {
			break
		}
		if ownerIdx < 0 || ownerIdx >= len(scopes) {
			continue
		}
		if ownerColIdx, ok := scopes[ownerIdx].colIndex[lname]; ok {
			owners = append(owners, coalesceOwner{cursor: scopes[ownerIdx].cursor, colIdx: ownerColIdx + scopes[ownerIdx].colBase})
			// Transitive hop: the owner may be a join group whose own connector needs
			// coalescing ("t1 FULL JOIN (t2 FULL JOIN t3 USING(a)) USING(a)"), so splice its
			// resolved chain in after it, as resolveCoalesceChain does for a row read.
			owners = append(owners, scopes[ownerIdx].resolvedFallback[lname]...)
		}
	}
	return owners
}

// coalesceOwnersCombined is the full COALESCE list for lname on s: its resolved
// group-internal chain (resolvedFallback), then this level's raw fallback
// (coalesceOwnersFor). Concatenated, never one or the other. In C a
// parenthesized group is a nested FROM whose exposed column is already a
// coalesce() over its members, and the enclosing RIGHT/FULL join's coalesce
// takes that as one argument; COALESCE is associative, so flatten.
func coalesceOwnersCombined(scopes []compileScope, s compileScope, lname string, limit int) []coalesceOwner {
	inner := s.resolvedFallback[lname]
	outer := coalesceOwnersFor(scopes, s.coalesceFallback[lname], lname, limit)
	if len(outer) == 0 {
		return inner
	}
	if len(inner) == 0 {
		return outer
	}
	return append(append(make([]coalesceOwner, 0, len(inner)+len(outer)), inner...), outer...)
}

// resolveInScopes resolves x against one scope level, as resolveColumn does per
// level. found=true binds it here; found=false, hard=nil means try the next
// level; hard!=nil stops the search ("ambiguous column name" unwrapped, or a
// qualified table missing the column, wrapped errVDBEUnsupported). fb reports a
// RIGHT/FULL coalesce fallback.
//
// pager resolves x.Schema to a database index (dbIdxForSchema), so "main.t4.a"
// and "aux1.t4.a" bind to the right one of two scopes named t4. nil is safe;
// with no schema it is never consulted.
func resolveInScopes(scopes []compileScope, x ColumnExpr, pager *ReadOnlyPager) (cursor, colIdx int, isRowid, found bool, fb coalesceFallbackRef, hard error) {
	lname := r33sFoldIdent(x.Name)
	wantDBIdx, filterByDB := dbIdxForSchema(pager, x.Schema)

	// A PINNED reference (ColumnExpr.UsingPinned, sql_ast.go): one side of a
	// desugared USING/NATURAL condition whose FROM item -- an unaliased derived
	// table -- has no scope name to match on, so it binds by FROM-item index
	// instead. Bound only in THIS scope level (a desugared join condition never
	// reaches an outer query's FROM), and otherwise identical to the qualified
	// branch below, coalesce fallback included.
	if x.UsingPinned {
		if x.UsingPinnedItem < 0 || x.UsingPinnedItem >= len(scopes) {
			return 0, 0, false, false, fb, fmt.Errorf("%w: USING/NATURAL join condition pinned to FROM item %d, out of range", errVDBEUnsupported, x.UsingPinnedItem)
		}
		s := scopes[x.UsingPinnedItem]
		idx, ok := s.colIndex[lname]
		if !ok {
			return 0, 0, false, false, fb, fmt.Errorf("%w: no such column: %s", errVDBEUnsupported, x.Name)
		}
		if x.UsingRepr {
			if owners := coalesceOwnersCombined(scopes, s, lname, x.UsingReprOwnItem); len(owners) > 0 {
				fb = coalesceFallbackRef{owners: owners, has: true}
			}
		}
		return s.cursor, idx + s.colBase, false, true, fb, nil
	}

	if x.Qualifier != "" {
		for i, s := range scopes {
			if !equalFoldName(s.name, x.Qualifier) {
				continue
			}
			// x.Schema named a specific database: a scope of the SAME bare
			// name but a DIFFERENT database is not a candidate for THIS
			// reference at all -- skip it entirely (never treat it as "the
			// qualifier matched but the column is missing"), so a LATER scope
			// in the loop that does carry x.Schema's database still gets a
			// chance. See resolveInScopes' own doc comment and
			// dbIdxForSchema (cross_db.go).
			if filterByDB && s.dbIdx != wantDBIdx {
				continue
			}
			if idx, ok := s.colIndex[lname]; ok {
				// An UNALIASED SELF-JOIN can leave this column exposed by a
				// SECOND item of the same name ("t1 JOIN t1 USING(x)" leaves
				// both copies of y), which C SQLite calls ambiguous. Without
				// this the first match won and showed one copy's value twice.
				// A UsingRepr reference is exempt: it is join.go's own
				// synthesized condition against a chosen representative, not a
				// reference the user wrote.
				if !x.UsingRepr && !x.UsingPinned && qualifiedScopeAmbiguous(scopes, i, lname, wantDBIdx, filterByDB) {
					return 0, 0, false, false, fb, fmt.Errorf("engine: ambiguous column name: %s.%s", x.Qualifier, x.Name)
				}
				// A desugared USING/NATURAL condition (x.UsingRepr) reading its representative
				// takes the RIGHT/FULL coalesce fallback too, but only from items strictly
				// earlier than its own join item, else "t1 RIGHT JOIN t2 USING(a)" would match
				// its own NULL-keyed row. coalesceOwnersCombined puts a group member's resolved
				// chain first.
				if x.UsingRepr {
					if owners := coalesceOwnersCombined(scopes, s, lname, x.UsingReprOwnItem); len(owners) > 0 {
						fb = coalesceFallbackRef{owners: owners, has: true}
					}
				}
				return s.cursor, idx + s.colBase, false, true, fb, nil
			}
			if isRowidAliasName(x.Name) && !s.noRowid {
				// Mirrors resolveColumnEx's identical rule: two
				// FROM items sharing this qualifier both offer the pseudo-rowid,
				// which no USING/NATURAL coalescing ever hides, so C SQLite
				// reports "ambiguous column name: t1.rowid" rather than picking
				// one. Returned unwrapped (not errVDBEUnsupported) like this
				// function's other ambiguity errors: it is the answer, not a
				// reason to fall back.
				for _, other := range scopes[i+1:] {
					if filterByDB && other.dbIdx != wantDBIdx {
						continue
					}
					if equalFoldName(other.name, x.Qualifier) && !other.noRowid {
						return 0, 0, false, false, fb, fmt.Errorf("engine: ambiguous column name: %s.%s", x.Qualifier, x.Name)
					}
				}
				return s.cursor, 0, true, true, fb, nil
			}
			// resolve.c:785-796 builds the message from the parts the reference was
			// WRITTEN with: "%s.%s.%s" when a schema was named, "%s.%s" with a table,
			// "%s" bare. Dropping the qualifier here made "SELECT t.nosuch FROM t"
			// report "no such column: nosuch" where 3.53.3 reports
			// "no such column: t.nosuch" -- and as an errVDBEUnsupported it arrived
			// wrapped in "VDBE-only: ... (no fallback)" too, where C rejects it at
			// prepare like any other unresolved name.
			if x.Schema != "" {
				return 0, 0, false, false, fb, semanticf("engine: no such column: %s.%s.%s", x.Schema, x.Qualifier, x.Name)
			}
			return 0, 0, false, false, fb, semanticf("engine: no such column: %s.%s", x.Qualifier, x.Name)
		}
		// No FROM item here is named the qualifier; it may be a join group's alias
		// (tableScope.groupAlias, tkt3935.test 3). Only reached when nothing matched by
		// name, so it can only turn "no such column" into an answer. Members of an
		// aliased group share no column names.
		//
		// Only the group's own internal coalesce applies, never an enclosing RIGHT/FULL
		// join's: C filters a qualified name to the one SrcItem with that alias
		// (resolve.c:424), so cnt stays 1 and the coalesce rewrite (guarded by
		// cnt!=1, resolve.c:761) never fires. An outer RIGHT JOIN null-extends the
		// group as a whole, so "j.a" reads NULL.
		for _, s := range scopes {
			if !equalFoldName(s.groupAlias, x.Qualifier) {
				continue
			}
			if idx, ok := s.colIndex[lname]; ok {
				if owners := s.resolvedFallback[lname]; len(owners) > 0 {
					fb = coalesceFallbackRef{owners: owners, has: true}
				}
				return s.cursor, idx + s.colBase, false, true, fb, nil
			}
		}
		return 0, 0, false, false, fb, nil // no table here matches the qualifier: try outer
	}

	foundCursor, foundIdx := -1, -1
	foundRowidCursor := -1
	var foundScope compileScope
	for _, s := range scopes {
		if idx, ok := s.colIndex[lname]; ok {
			if s.coalesced != nil {
				if _, hidden := s.coalesced[lname]; hidden {
					// USING/NATURAL right-hand duplicate -- see
					// resolveColumn's identical rule: doesn't
					// count as a match here at all, so it can never trigger
					// "ambiguous column name" against its own representative.
					continue
				}
			}
			if foundCursor != -1 || foundRowidCursor != -1 {
				return 0, 0, false, false, fb, fmt.Errorf("engine: ambiguous column name: %s", x.Name)
			}
			foundCursor, foundIdx = s.cursor, idx+s.colBase
			foundScope = s
			continue
		}
		if isRowidAliasName(x.Name) && !s.noRowid {
			if foundCursor != -1 || foundRowidCursor != -1 {
				return 0, 0, false, false, fb, fmt.Errorf("engine: ambiguous column name: %s", x.Name)
			}
			foundRowidCursor = s.cursor
		}
	}
	if foundRowidCursor != -1 {
		return foundRowidCursor, 0, true, true, fb, nil
	}
	if foundCursor != -1 {
		// See coalesceOwnersCombined and compileScope.resolvedFallback's doc
		// comments: a materialized parenthesized join GROUP member's
		// ALREADY-resolved owner pairs come FIRST, then whatever this level's
		// own []int/scopes-indexed fallback names.
		if owners := coalesceOwnersCombined(scopes, foundScope, lname, math.MaxInt); len(owners) > 0 {
			fb = coalesceFallbackRef{owners: owners, has: true}
		}
		return foundCursor, foundIdx, false, true, fb, nil
	}
	return 0, 0, false, false, fb, nil // not here: try outer
}

// compileLiteral loads a constant into a fresh register.
func (c *compiler) compileLiteral(v Value) int {
	r := c.alloc()
	switch v.Typ {
	case Null:
		c.emit(Instruction{Op: OpNull, P2: r})
	case Int:
		if v.I >= math.MinInt32 && v.I <= math.MaxInt32 {
			c.emit(Instruction{Op: OpInteger, P1: int(v.I), P2: r})
		} else {
			c.emit(Instruction{Op: OpInt64, P2: r, P4: v.I})
		}
	case Float:
		c.emit(Instruction{Op: OpReal, P2: r, P4: v.F})
	case Text:
		c.emit(Instruction{Op: OpString8, P2: r, P4: string(v.S)})
	case Blob:
		c.emit(Instruction{Op: OpBlob, P2: r, P4: append([]byte(nil), v.S...)})
	default:
		c.emit(Instruction{Op: OpNull, P2: r})
	}
	return r
}

func (c *compiler) compileUnary(x UnaryExpr) (int, error) {
	switch x.Op {
	case "+":
		// A true no-op in SQLite for every operand type: the operand's value
		// and storage class pass through unchanged.
		return c.compileExpr(x.X)
	case "NOT":
		r, err := c.compileExpr(x.X)
		if err != nil {
			return 0, err
		}
		d := c.alloc()
		c.emit(Instruction{Op: OpNot, P1: r, P2: d})
		return d, nil
	case "-":
		// OpNegative reuses negateValueUnary (value_arith.go), so there is
		// exactly ONE unary-minus semantics in this package, negative zero
		// included. (An earlier
		// "0 - X" OpSubtract codegen matched for every operand except -0.0,
		// where "0 - 0.0" is +0.0 but SQLite's unary "-" yields -0.0; the read
		// path's -0.0 == +0.0 comparison never caught it, but the write path's
		// byte-exact stored double did.)
		r, err := c.compileExpr(x.X)
		if err != nil {
			return 0, err
		}
		d := c.alloc()
		c.emit(Instruction{Op: OpNegative, P1: r, P2: d})
		return d, nil
	case "~":
		// OpBitNot reuses bitNotValueUnary (value_arith.go), so there is
		// exactly ONE bitwise-NOT semantics in this package.
		r, err := c.compileExpr(x.X)
		if err != nil {
			return 0, err
		}
		d := c.alloc()
		c.emit(Instruction{Op: OpBitNot, P1: r, P2: d})
		return d, nil
	}
	return 0, fmt.Errorf("%w: unary operator %q", errVDBEUnsupported, x.Op)
}

// arithOpcode maps an arithmetic/concat operator to its opcode.
var arithOpcode = map[string]OpCode{
	"+": OpAdd, "-": OpSubtract, "*": OpMultiply, "/": OpDivide, "%": OpRemainder,
	"&": OpBitAnd, "|": OpBitOr, "<<": OpShiftLeft, ">>": OpShiftRight,
}

// bitwiseOpName is arithOpcode's bitwise half inverted, so vdbe.go's single
// OpBitAnd/OpBitOr/OpShiftLeft/OpShiftRight case can hand the operator
// spelling back to the shared bitwiseBinaryValue helper.
var bitwiseOpName = map[OpCode]string{
	OpBitAnd: "&", OpBitOr: "|", OpShiftLeft: "<<", OpShiftRight: ">>",
}

// cmpOpcode maps a comparison operator to its opcode.
var cmpOpcode = map[string]OpCode{
	"<": OpLt, "<=": OpLe, ">": OpGt, ">=": OpGe,
	"=": OpEq, "==": OpEq, "!=": OpNe, "<>": OpNe,
}

// top is the compiler.inWhereConjunct value captured by compileExpr at the
// call site that reached this BinaryExpr -- see that field's doc comment.
// compileBinary only forwards it on to the two places it matters: AND's own
// re-arming (compileAndOr) and the row-value-vs-subquery "=" / "IS" collation
// choice (compileRowSubCompare); every other operator ignores it.
func (c *compiler) compileBinary(x BinaryExpr, top bool) (int, error) {
	switch x.Op {
	case "AND":
		return c.compileAndOr(x.L, x.R, true, top)
	case "OR":
		return c.compileAndOr(x.L, x.R, false, top)
	}

	if opc, ok := arithOpcode[x.Op]; ok {
		l, err := c.compileExpr(x.L)
		if err != nil {
			return 0, err
		}
		r, err := c.compileExpr(x.R)
		if err != nil {
			return 0, err
		}
		d := c.alloc()
		c.emit(Instruction{Op: opc, P1: r, P2: l, P3: d})
		return d, nil
	}
	if x.Op == "||" {
		l, err := c.compileExpr(x.L)
		if err != nil {
			return 0, err
		}
		r, err := c.compileExpr(x.R)
		if err != nil {
			return 0, err
		}
		d := c.alloc()
		c.emit(Instruction{Op: OpConcat, P1: r, P2: l, P3: d})
		return d, nil
	}

	if err := rowValueMisusedErr(c, x.Op, x.L, x.R); err != nil {
		return 0, err
	}
	if probe, sub, subOnLeft, ok := rowSubOperands(x.Op, x.L, x.R); ok {
		return c.compileRowSubCompare(x.Op, probe, sub, subOnLeft, top)
	}
	if x.Op == "IS" || x.Op == "IS NOT" {
		return c.compileCompare(x.Op, x.L, x.R, true)
	}
	if _, ok := cmpOpcode[x.Op]; ok {
		return c.compileCompare(x.Op, x.L, x.R, false)
	}
	return 0, fmt.Errorf("%w: binary operator %q", errVDBEUnsupported, x.Op)
}

// rowSubCompareOps is every operator a row value may be compared with -- the
// equality-shaped family plus the lexicographic one. It deliberately excludes
// everything else a BinaryExpr can hold ("+", "||", ...), for which a row-value
// operand is SQLite's "row value misused" and must stay declined.
var rowSubCompareOps = map[string]bool{
	"=": true, "==": true, "!=": true, "<>": true, "IS": true, "IS NOT": true,
	"<": true, "<=": true, ">": true, ">=": true,
}

// rowValueMisusedErr is "row value misused" (expr.c:5617) for a multi-column
// subquery compared with a scalar ("1 = (SELECT a,b FROM t)"). Without it the
// scalar subquery compiler answered first with "sub-select returns N columns",
// which is right only for a bare "SELECT (SELECT 1,2)".
func rowValueMisusedErr(c *compiler, op string, l, r Expr) error {
	if !rowSubCompareOps[op] || c.pager == nil {
		return nil
	}
	for _, pair := range [2][2]Expr{{l, r}, {r, l}} {
		sub, isSub := pair[0].(SubqueryExpr)
		if !isSub || sub.Stmt == nil {
			continue
		}
		if _, other := pair[1].(RowExpr); other {
			continue // the row-value spelling: compileRowSubCompare's own case
		}
		if n, verr := c.pager.subqueryResultColumns(sub.Stmt); verr == nil && n != 1 {
			return semanticf("engine: row value misused")
		}
	}
	return nil
}

// rowSubOperands recognizes the one row-value comparison the parser cannot
// desugar: a ROW VALUE on one side and a multi-column SUBQUERY on the other.
// Every other supported spelling is already a scalar boolean tree by the time
// it reaches the compiler (desugarRowCompare and friends, sql_parser.go).
//
// Both operands being subqueries ("(SELECT a,b) = (SELECT c,d)", which real
// SQLite accepts) is deliberately NOT recognized: it needs two row sources and
// OpRowSub carries one, so it stays declined rather than guessed at.
func rowSubOperands(op string, l, r Expr) (probe RowExpr, sub *SelectStmt, subOnLeft, ok bool) {
	if !rowSubCompareOps[op] {
		return RowExpr{}, nil, false, false
	}
	lr, lIsRow := l.(RowExpr)
	rr, rIsRow := r.(RowExpr)
	ls, lIsSub := l.(SubqueryExpr)
	rs, rIsSub := r.(SubqueryExpr)
	switch {
	case lIsRow && rIsSub:
		return lr, rs.Stmt, false, true
	case lIsSub && rIsRow:
		return rr, ls.Stmt, true, true
	}
	return RowExpr{}, nil, false, false
}

// compileRowSubCompare compiles "(a,b) <op> (SELECT x,y FROM t)" (either side):
// the row into a register block, the subquery into a sub-Program, then OpRowSub
// (rowSubCompare) over the subquery's first row. This cannot be desugared at
// parse time like other row comparisons, since those rewrites would run the
// subquery more than once.
//
// Per-column affinity combines both sides (subqueryOutputAffinities).
// Per-column collation is resolved left operand first. For "=", "==" and "IS"
// with the collation coming from the subquery, C answers differently by
// position; `top` (inWhereConjunct) picks, see below.
func (c *compiler) compileRowSubCompare(op string, probe RowExpr, sub *SelectStmt, subOnLeft, top bool) (int, error) {
	if c.pager == nil {
		return 0, fmt.Errorf("%w: row-value subquery comparison outside a cursor-driven scope", errVDBEUnsupported)
	}
	arity := len(probe.Elems)
	// C SQLite validates the width at PREPARE time and rejects a mismatch in
	// either direction with "row value misused" -- NOT with IN's "sub-select
	// returns N columns" wording (verified: "(1,2) = (SELECT a FROM t)",
	// "(1,2,3) = (SELECT a,b FROM t)" and "1 = (SELECT a,b FROM t)" are all
	// "row value misused"). subqueryResultColumns reproduces the check at compile
	// time so an empty outer table cannot mask it; an un-analyzable subquery
	// falls through to the sub-Program compile below, which knows its own width.
	if n, verr := c.pager.subqueryResultColumns(sub); verr != nil {
		if errors.Is(verr, errVDBESemantic) {
			return 0, verr
		}
	} else if n != arity {
		return 0, semanticf("row value misused")
	}
	prog, err := c.liveSubSelect(sub)
	if err != nil {
		return 0, err
	}
	if prog.NResultCol != arity {
		return 0, semanticf("row value misused")
	}
	// One consecutive block so OpRowSub can name it as a range, exactly like
	// OpInSub's probe. The row value is evaluated BEFORE the subquery runs,
	// matching the operand order of every other comparison this compiler emits.
	xBase := c.allocN(arity)
	for i, el := range probe.Elems {
		reg, cerr := c.compileExpr(el)
		if cerr != nil {
			return 0, cerr
		}
		c.emit(Instruction{Op: OpSCopy, P1: reg, P2: xBase + i})
	}
	subAffs, subMats, _ := c.pager.subqueryOutputAffinities(sub, c.affCtx())
	subColls, _ := c.pager.subqueryOutputCollations(sub, c.affCtx())
	affs := make([]affinity, arity)
	colls := make([]string, arity)
	// See the decline below: these three operators, and only these three, are
	// the ones C SQLite resolves the collating sequence differently for
	// depending on where the comparison sits.
	splitInWhere := op == "=" || op == "==" || op == "IS"
	for i, el := range probe.Elems {
		// affExpr/collExpr stand in for the subquery's i'th result column; their
		// zero/absent form leaves the position to be decided from el alone, which
		// is what an unanalyzable subquery yields (same as compileInSubquery).
		var subAff Expr = affExpr{}
		if i < len(subAffs) {
			subAff = affExpr{aff: subAffs[i], materialized: subMats[i]}
		}
		var subColl Expr
		if i < len(subColls) {
			subColl = subColls[i]
		}
		// comparisonAffinity is symmetric, so operand order matters only to the
		// collation.
		affs[i] = comparisonAffinity(c.affCtx(), el, subAff)
		withSub, withoutSub := resolveCompareCollation(c.affCtx(), el, subColl), resolveCompareCollation(c.affCtx(), el, nil)
		if subOnLeft {
			withSub, withoutSub = resolveCompareCollation(c.affCtx(), subColl, el), resolveCompareCollation(c.affCtx(), nil, el)
		}
		if splitInWhere && withSub != withoutSub {
			// A vector "="/"=="/"IS" that is a top-level AND conjunct of WHERE is split
			// into per-field terms (tag-20220128a, whereexpr.c:1467), and each field
			// becomes an opaque TK_SELECT_COLUMN with no collation
			// (sqlite3ExprForVectorField, expr.c:574), so the left operand's collation
			// governs (withoutSub). Everywhere else (select list, HAVING, OR operands, the
			// other seven operators) the general vector codegen reads the real subquery
			// column (expr.c:659), so its collation is seen (withSub). For example, with
			// hh(a TEXT COLLATE NOCASE) = 'ABC':
			//
			//   sqlite> SELECT c, (a,b) = (SELECT 'abc' COLLATE BINARY,'x') FROM hh
			//      ...>   WHERE (a,b) = (SELECT 'abc' COLLATE BINARY,'x');
			//   hit|0
			//
			// All ten operators were measured in both orders; only these three split.
			if top {
				colls[i] = withoutSub
				continue
			}
			// top is false: fall through to the unconditional withSub below,
			// same as the non-conflicting case.
		}
		colls[i] = withSub
	}
	plan := &rowSubPlan{prog: prog, op: op, subOnLeft: subOnLeft, affs: affs, colls: colls, correlated: prog.Correlated}
	d := c.alloc()
	// A correlated subquery re-runs per evaluation (no cache slot); an
	// uncorrelated one runs once and caches, keyed by a fresh slot.
	slot := 0
	if !prog.Correlated {
		slot = c.allocSub()
	}
	c.emit(Instruction{Op: OpRowSub, P1: xBase, P2: d, P3: slot, P4: plan})
	return d, nil
}

// compileCompare emits a store-form comparison producing a boolean (or NULL)
// value. For IS / IS NOT (isKind), the NULLEQ flag makes NULLs compare equal
// and the result a definite boolean; the operator becomes OpEq (IS) / OpNe (IS
// NOT). Comparison affinity is decided at compile time from the operand
// expressions (compAffinity) and carried in P5, reproducing the run-time
// comparison's own affinity rule within the FROM-less domain.
func (c *compiler) compileCompare(op string, le, re Expr, isKind bool) (int, error) {
	l, err := c.compileExpr(le)
	if err != nil {
		return 0, err
	}
	r, err := c.compileExpr(re)
	if err != nil {
		return 0, err
	}
	d := c.alloc()
	aff := c.compAffinity(le, re)
	p5 := uint16(aff) | p5StoreP2
	var opc OpCode
	if isKind {
		p5 |= p5NullEq
		if op == "IS" {
			opc = OpEq
		} else {
			opc = OpNe
		}
	} else {
		opc = cmpOpcode[op]
	}
	c.emit(Instruction{Op: opc, P1: r, P2: d, P3: l, P4: c.compCollation(le, re), P5: p5})
	return d, nil
}

// compCollation is the collation a comparison opcode carries in P4 for
// "le <op> re", resolveCompareCollation's compile-time analogue, resolved
// against affCtx so a correlated column's declared collation counts.
func (c *compiler) compCollation(le, re Expr) string {
	return resolveCompareCollation(c.affCtx(), le, re)
}

// compileAndOr emits short-circuiting three-valued AND/OR: the right operand is
// skipped once the left decides, so an erroring right operand is not
// evaluated. top is inWhereConjunct for this node; a top-level WHERE AND re-arms
// it for both operands (sqlite3WhereSplit's recursion through ANDs). OR never
// does (exprAnalyzeOrTerm, whereexpr.c:721).
func (c *compiler) compileAndOr(le, re Expr, isAnd, top bool) (int, error) {
	d := c.alloc()
	// The "decides it" opcode: for AND, a FALSE operand short-circuits to
	// FALSE; for OR, a TRUE operand short-circuits to TRUE.
	decideOp := OpIf // OR: jump-on-true
	if isAnd {
		decideOp = OpIfNot // AND: jump-on-false
	}

	if isAnd && top {
		c.inWhereConjunct = true
	}
	lReg, err := c.compileExpr(le)
	if err != nil {
		return 0, err
	}
	jL := c.emit(Instruction{Op: decideOp, P1: lReg}) // -> short-circuit block
	if isAnd && top {
		c.inWhereConjunct = true
	}
	rReg, err := c.compileExpr(re)
	if err != nil {
		return 0, err
	}
	jR := c.emit(Instruction{Op: decideOp, P1: rReg}) // -> short-circuit block
	// Neither operand decided it: the result is NULL if either is NULL, else
	// the non-short-circuit constant (TRUE for AND, FALSE for OR).
	jLNull := c.emit(Instruction{Op: OpIsNull, P1: lReg})
	jRNull := c.emit(Instruction{Op: OpIsNull, P1: rReg})
	bothKnown := 1 // AND: both true -> TRUE
	if !isAnd {
		bothKnown = 0 // OR: both false -> FALSE
	}
	c.emit(Instruction{Op: OpInteger, P1: bothKnown, P2: d})
	jEnd := c.emit(Instruction{Op: OpGoto})
	// NULL block.
	nullAddr := c.here()
	c.patch(jLNull, nullAddr)
	c.patch(jRNull, nullAddr)
	c.emit(Instruction{Op: OpNull, P2: d})
	jEnd2 := c.emit(Instruction{Op: OpGoto})
	// Short-circuit block: the deciding constant (FALSE for AND, TRUE for OR).
	scAddr := c.here()
	c.patch(jL, scAddr)
	c.patch(jR, scAddr)
	decided := 0 // AND short-circuits to FALSE
	if !isAnd {
		decided = 1 // OR short-circuits to TRUE
	}
	c.emit(Instruction{Op: OpInteger, P1: decided, P2: d})
	end := c.here()
	c.patch(jEnd, end)
	c.patch(jEnd2, end)
	return d, nil
}

// compileIsNull emits code for "X IS [NOT] NULL" producing a 1/0 boolean.
func (c *compiler) compileIsNull(x IsNullExpr) (int, error) {
	r, err := c.compileExpr(x.X)
	if err != nil {
		return 0, err
	}
	d := c.alloc()
	notNullVal, isNullVal := 0, 1 // "IS NULL": null -> 1, non-null -> 0
	if x.Not {
		notNullVal, isNullVal = 1, 0 // "IS NOT NULL": null -> 0, non-null -> 1
	}
	jNull := c.emit(Instruction{Op: OpIsNull, P1: r})
	c.emit(Instruction{Op: OpInteger, P1: notNullVal, P2: d})
	jEnd := c.emit(Instruction{Op: OpGoto})
	c.patch(jNull, c.here())
	c.emit(Instruction{Op: OpInteger, P1: isNullVal, P2: d})
	c.patch(jEnd, c.here())
	return d, nil
}

// compileBetween compiles "X BETWEEN Lo AND Hi" as (X>=Lo) AND (X<=Hi), with X
// evaluated up to twice under AND's short-circuit, then negates
// (NULL-preserving) for NOT BETWEEN.
func (c *compiler) compileBetween(x BetweenExpr) (int, error) {
	// The identity below evaluates X twice; a row-value subquery X would run twice
	// and could compare two different rows. Decline, as desugarRowLex does.
	if _, isSub := x.X.(SubqueryExpr); isSub {
		_, loRow := x.Lo.(RowExpr)
		_, hiRow := x.Hi.(RowExpr)
		if loRow || hiRow {
			return 0, fmt.Errorf("%w: row-value BETWEEN over a subquery (would evaluate it twice)", errVDBEUnsupported)
		}
	}
	ge := BinaryExpr{Op: ">=", L: x.X, R: x.Lo}
	le := BinaryExpr{Op: "<=", L: x.X, R: x.Hi}
	// top: false, not propagated from this BETWEEN's own compileExpr entry --
	// this AND is a desugaring artifact synthesized here, not literally the
	// source-level WHERE AST sqlite3WhereSplit walks. It is also harmless
	// either way: ">=" and "<=" are never in splitInWhere (compileRowSubCompare),
	// so this value can never change either operand's answer.
	r, err := c.compileAndOr(ge, le, true, false)
	if err != nil {
		return 0, err
	}
	if !x.Not {
		return r, nil
	}
	d := c.alloc()
	c.emit(Instruction{Op: OpNot, P1: r, P2: d})
	return d, nil
}

// compileLike compiles "X [NOT] LIKE Pattern [ESCAPE Escape]"; the VM's
// OpLike body reuses likeMatch/likeMatchEscape and propagates NULL, with the
// ESCAPE validity error taking precedence over an X/Pattern NULL, as in C. When
// there is no ESCAPE clause, P4 is set to -1 (an always-invalid register
// number) as the sentinel OpLike's body checks -- see OpLike's own doc
// comment (vdbe_op.go).
func (c *compiler) compileLike(x LikeExpr) (int, error) {
	xr, err := c.compileExpr(x.X)
	if err != nil {
		return 0, err
	}
	pr, err := c.compileExpr(x.Pattern)
	if err != nil {
		return 0, err
	}
	escReg := -1
	if x.Escape != nil {
		escReg, err = c.compileExpr(x.Escape)
		if err != nil {
			return 0, err
		}
	}
	d := c.alloc()
	var p5 uint16
	if x.Not {
		p5 |= p5LikeNot
	}
	c.emit(Instruction{Op: OpLike, P1: xr, P2: d, P3: pr, P4: escReg, P5: p5})
	return d, nil
}

// compileGlob compiles "X [NOT] GLOB Pattern"; the VM's OpGlob body reuses
// globMatch and propagates NULL. Structurally identical to compileLike -- just a different opcode/flag pair.
func (c *compiler) compileGlob(x GlobExpr) (int, error) {
	xr, err := c.compileExpr(x.X)
	if err != nil {
		return 0, err
	}
	pr, err := c.compileExpr(x.Pattern)
	if err != nil {
		return 0, err
	}
	d := c.alloc()
	var p5 uint16
	if x.Not {
		p5 |= p5GlobNot
	}
	c.emit(Instruction{Op: OpGlob, P1: xr, P2: d, P3: pr, P5: p5})
	return d, nil
}

// compileMatchExpr compiles "X MATCH Pattern". X names an fts5 table or one of
// its columns, so OpMatch needs the current row's table scopes (gathered at run
// time by gatherScopesRow from the statically known c.scopes). On the fts3/4
// route the pattern is compiled to a register (matchCompileInfo.patReg), as C
// codes a vtab constraint (wherecode.c:1584); fts5 evaluates it inside
// evalMatch.
func (c *compiler) compileMatchExpr(x MatchExpr) (int, error) {
	scopes := make([]tableScope, len(c.scopes))
	cursors := make([]int, len(c.scopes))
	// colBases is non-nil only when a materialized join group is present: its
	// members share one wider cursor row, so gatherScopesRow needs each member's
	// offset to copy its own slice.
	var colBases []int
	for _, s := range c.scopes {
		if s.colBase != 0 {
			colBases = make([]int, len(c.scopes))
			for j, s2 := range c.scopes {
				colBases[j] = s2.colBase
			}
			break
		}
	}
	for i, s := range c.scopes {
		scopes[i] = s.tableScope
		cursors[i] = s.cursor
	}
	// Resolve the MATCH target at compile time: a per-row check never runs on an
	// empty table, but C rejects an unresolvable MATCH at prepare time
	// (fts3expr.test). A malformed fts3 query is likewise a prepare-time
	// "malformed MATCH expression", so it is parsed here.
	if ms, ok := fts3ResolveMatchTarget(c.pager, scopes, x.X); ok {
		ms, lerr := c.fts3WithLangid(ms)
		if lerr != nil {
			return 0, lerr
		}
		if err := c.checkFts3Match(ms, x); err != nil {
			return 0, err
		}
		// Resolved again against c.scopes (cheap -- see fts3MatchScopeIndex's
		// own doc comment for why every caller here does this rather than
		// share one result): checkFts3Match's success guarantees this
		// succeeds too, so a miss just leaves contentUnneeded at its safe
		// false zero value.
		idx, _ := c.fts3MatchScopeIndex(x.X)
		if matchGroupAfterVtab(colBases, idx) {
			return 0, fmt.Errorf("%w: MATCH over a vtab followed by a parenthesized join group", errVDBEUnsupported)
		}
		// The QUERY is coded into a register here, immediately before the
		// OpMatch that reads it back as a value -- so it is recomputed once
		// per row of whatever loop this MATCH sits in, which is the frequency
		// C fts3 evaluates it at (its xFilter reads apVal[0] on every
		// call). SQLite codes a virtual table's constraint value the same way
		// and in the same place: codeExprOrVector(pParse, pRight, iTarget, 1)
		// at wherecode.c:1584, inside the loop body that then runs OP_VFilter.
		// See matchCompileInfo.patReg (vdbe_op.go).
		patReg, perr := c.compileExpr(x.Pattern)
		if perr != nil {
			return 0, perr
		}
		d := c.alloc()
		c.emit(Instruction{Op: OpMatch, P2: d, P4: &matchCompileInfo{expr: x, scopes: scopes, cursors: cursors, colBases: colBases, langid: ms.langid, contentUnneeded: c.fts3ContentUnneeded[idx], patReg: patReg, patCompiled: true}})
		return d, nil
	}
	// An rtree operand: the module rejects a MATCH constraint at prepare time, so
	// it errors even over an empty table. A MATCH on an ordinary column is a
	// per-row error in C too. Declined: C's error carries no message.
	if c.matchOperandIsRtree(scopes, x.X) {
		return 0, fmt.Errorf("%w: MATCH against an rtree table (its module rejects a MATCH constraint at prepare time, with no message this engine can reproduce)", errVDBEUnsupported)
	}
	return c.compileFts5Match(c.fts5MatchGood, scopes, cursors, colBases, x)
}

// matchOperandIsRtree reports whether a MATCH's left operand names an rtree
// table -- either one of its columns ("x0 MATCH ...") or the table itself
// ("rt MATCH ..."), the two spellings the resolver accepts.
func (c *compiler) matchOperandIsRtree(scopes []tableScope, x Expr) bool {
	ce, ok := x.(ColumnExpr)
	if !ok || c.pager == nil {
		return false
	}
	table := ""
	for _, s := range scopes {
		switch {
		case ce.Qualifier != "":
			if equalFoldName(s.name, ce.Qualifier) {
				table = s.tableName
			}
		case equalFoldName(s.name, ce.Name):
			table = s.tableName
		default:
			if _, hit := s.colIndex[r33sFoldIdent(ce.Name)]; hit {
				table = s.tableName
			}
		}
		if table != "" {
			break
		}
	}
	if table == "" {
		return false
	}
	module, _, found, err := c.pager.createdVtabDef(table)
	if err != nil || !found {
		return false
	}
	switch r33sFoldIdent(module) {
	case "rtree", "rtree_i32":
		return true
	}
	return false
}

// matchGroupAfterVtab reports whether a materialized join group comes after
// vtabIdx in c.scopes; its members are contiguous, so any colBase != 0 entry
// past vtabIdx proves it.
//
// A group after the vtab answers wrong values (e.g. "t10, (a LEFT JOIN b ON
// a.id=b.id) WHERE t10 MATCH 'apple'" repeats the wrong row), while before the
// vtab it is correct. Root cause unknown, so this shape declines.
func matchGroupAfterVtab(colBases []int, vtabIdx int) bool {
	for i := vtabIdx + 1; i < len(colBases); i++ {
		if colBases[i] != 0 {
			return true
		}
	}
	return false
}

// compileIn compiles "X [NOT] IN (value-list)" with IN's three-valued
// semantics (finalizeIn): every list element is evaluated (so an error in any element still
// surfaces, even after a match); a definite match wins over NULL-ambiguity; a
// NULL on either side with no match yields NULL; NOT negates a definite result
// but leaves NULL as NULL. The list-element affinity is X's own affinity only
// (the asymmetric IN rule), carried in each comparison's P5.
func (c *compiler) compileIn(x InExpr) (int, error) {
	if x.Sub != nil {
		return c.compileInSubquery(x)
	}
	xReg, err := c.compileExpr(x.X)
	if err != nil {
		return 0, err
	}
	d := c.alloc()
	if len(x.List) == 0 {
		// Empty set: IN is always false, NOT IN always true, even for NULL X.
		v := 0
		if x.Not {
			v = 1
		}
		c.emit(Instruction{Op: OpInteger, P1: v, P2: d})
		return d, nil
	}
	aff := c.inAffinity(x.X)
	// X's own collation only (the asymmetric IN rule -- a list element's own
	// expression never contributes one: resolveCompareCollation(ctx, x.X,
	// nil)).
	coll := c.compCollation(x.X, nil)

	rNull := c.alloc()
	c.emit(Instruction{Op: OpInteger, P1: 0, P2: rNull})
	rMatch := c.alloc()
	c.emit(Instruction{Op: OpInteger, P1: 0, P2: rMatch})

	// X NULL contributes to the NULL-ambiguity flag.
	jXnn := c.emit(Instruction{Op: OpNotNull, P1: xReg})
	c.emit(Instruction{Op: OpInteger, P1: 1, P2: rNull})
	c.patch(jXnn, c.here())

	for _, item := range x.List {
		iReg, err := c.compileExpr(item) // always evaluated
		if err != nil {
			return 0, err
		}
		// item NULL -> set NULL flag, skip comparison.
		jItemNN := c.emit(Instruction{Op: OpNotNull, P1: iReg})
		c.emit(Instruction{Op: OpInteger, P1: 1, P2: rNull})
		jAfterNull := c.emit(Instruction{Op: OpGoto})
		c.patch(jItemNN, c.here())
		// item non-null: compare X vs item (jump-on-equal to the match setter).
		jEq := c.emit(Instruction{Op: OpEq, P1: iReg, P3: xReg, P4: coll, P5: uint16(aff)})
		jAfterCmp := c.emit(Instruction{Op: OpGoto})
		c.patch(jEq, c.here())
		c.emit(Instruction{Op: OpInteger, P1: 1, P2: rMatch})
		after := c.here()
		c.patch(jAfterNull, after)
		c.patch(jAfterCmp, after)
	}

	// Finalize: match -> (Not?0:1); else NULL-seen -> NULL; else -> (Not?1:0).
	jMatch := c.emit(Instruction{Op: OpIf, P1: rMatch})
	jNull := c.emit(Instruction{Op: OpIf, P1: rNull})
	noMatch := 0
	if x.Not {
		noMatch = 1
	}
	c.emit(Instruction{Op: OpInteger, P1: noMatch, P2: d})
	jEnd1 := c.emit(Instruction{Op: OpGoto})
	c.patch(jNull, c.here())
	c.emit(Instruction{Op: OpNull, P2: d})
	jEnd2 := c.emit(Instruction{Op: OpGoto})
	c.patch(jMatch, c.here())
	matchVal := 1
	if x.Not {
		matchVal = 0
	}
	c.emit(Instruction{Op: OpInteger, P1: matchVal, P2: d})
	end := c.here()
	c.patch(jEnd1, end)
	c.patch(jEnd2, end)
	return d, nil
}

// rowSubqueryProbe recognizes a multi-column scalar subquery used as a row
// value and returns it with per-column affinity and collation stand-ins, or nil
// for anything else (a one-column subquery stays scalar). Both must be known:
// with no element expression on the other side, defaulting to BINARY would be a
// guess. A compound subquery has neither affinity nor collation in C, so it is
// neutral (measured). Anything else unanalyzable declines.
func (c *compiler) rowSubqueryProbe(x Expr) (sub *SelectStmt, affs []affinity, mats []bool, colls []Expr, err error) {
	sq, isSub := x.(SubqueryExpr)
	if !isSub {
		return nil, nil, nil, nil, nil
	}
	n, verr := c.pager.subqueryResultColumns(sq.Stmt)
	if verr != nil {
		if errors.Is(verr, errVDBESemantic) {
			return nil, nil, nil, nil, verr
		}
		return nil, nil, nil, nil, nil // unanalyzable width: not this shape
	}
	if n < 2 {
		return nil, nil, nil, nil, nil
	}
	if len(sq.Stmt.Compound) != 0 {
		// Neutral per the table above: affNone is the affinity zero value and a
		// nil collExpr is "contributes no collating sequence", which is exactly
		// what comparisonAffinity/resolveCompareCollation already treat as "let
		// the other operand decide".
		return sq.Stmt, make([]affinity, n), make([]bool, n), make([]Expr, n), nil
	}
	affs, mats, aok := c.pager.subqueryOutputAffinities(sq.Stmt, c.affCtx())
	colls, cok := c.pager.subqueryOutputCollations(sq.Stmt, c.affCtx())
	if !aok || !cok || len(affs) != n || len(colls) != n {
		return nil, nil, nil, nil, fmt.Errorf("%w: a row-value subquery operand whose per-column affinity/collation cannot be analyzed; comparing it under BINARY would be indistinguishable from a real answer", errVDBEUnsupported)
	}
	return sq.Stmt, affs, mats, colls, nil
}

// emitRowSubqueryProbe compiles a row-value subquery operand (see
// rowSubqueryProbe) and emits the one OpSubquery that lands its FIRST row's
// width values in the consecutive block at base. OpSubquery's P3 carries that
// width; a subquery that produces NO rows leaves the whole block NULL, which
// is C SQLite's answer for a row-value operand (an empty scalar subquery is
// a row of NULLs -- the OPPOSITE of IN's empty-set rule; see rowSubCompare).
func (c *compiler) emitRowSubqueryProbe(sub *SelectStmt, base, width int) error {
	prog, err := c.liveSubSelect(sub)
	if err != nil {
		return err
	}
	if prog.NResultCol != width {
		return semanticf("sub-select returns %d columns - expected %d", prog.NResultCol, width)
	}
	if prog.Correlated {
		c.emit(Instruction{Op: OpSubquery, P1: base, P3: width, P4: prog, P5: p5Correlated})
	} else {
		c.emit(Instruction{Op: OpSubquery, P1: base, P2: c.allocSub(), P3: width, P4: prog})
	}
	return nil
}

// compileInSubquery compiles "X [NOT] IN (SELECT ...)": X into a register block
// (a row value has one per element), the subquery into a sub-Program, then
// OpInSub (inSubMembership) with three-valued logic. X is compiled first. The
// value-list row form is desugared by the parser (desugarRowIn). Per-column
// affinity and collation combine X[i] with the subquery's column i
// (subqueryOutputAffinities / subqueryOutputCollations).
func (c *compiler) compileInSubquery(x InExpr) (int, error) {
	if c.pager == nil {
		return 0, fmt.Errorf("%w: IN subquery outside a cursor-driven scope", errVDBEUnsupported)
	}
	// C validates the IN subquery at prepare time (one column per probe element,
	// tables exist, compound arms agree) regardless of outer rows;
	// subqueryResultColumns does the same. errVDBESemantic propagates; an
	// unanalyzable shape falls through to the sub-program compile.
	// A width mismatch either way is a prepare-time error.
	probe := []Expr{x.X}
	if rv, isRow := x.X.(RowExpr); isRow {
		probe = rv.Elems
	}
	// A multi-column subquery on the left is a row value of that width
	// ("SELECT (SELECT 3,4) IN (SELECT 3,4)" is 1). Its affinity and collation must
	// come from its own output columns; when unknown (a compound), decline.
	lhsSub, lhsAffs, lhsMats, lhsColls, lerr := c.rowSubqueryProbe(x.X)
	if lerr != nil {
		return 0, lerr
	}
	if lhsSub != nil {
		probe = make([]Expr, len(lhsAffs))
	}
	arity := len(probe)
	if n, verr := c.pager.subqueryResultColumns(x.Sub); verr != nil {
		if errors.Is(verr, errVDBESemantic) {
			return 0, verr
		}
	} else if n != arity {
		return 0, semanticf("sub-select returns %d columns - expected %d", n, arity)
	}
	prog, err := c.liveSubSelect(x.Sub)
	if err != nil {
		return 0, err
	}
	if prog.NResultCol != arity {
		return 0, semanticf("sub-select returns %d columns - expected %d", prog.NResultCol, arity)
	}
	// The probe occupies ONE consecutive block so OpInSub can name it as a range
	// (P1..P1+arity-1), the same shape compileSelectNoFrom's result block uses.
	// A single-column probe still goes through the block, so scalar and
	// row-value membership share one code path rather than two.
	xBase := c.allocN(arity)
	if lhsSub != nil {
		if cerr := c.emitRowSubqueryProbe(lhsSub, xBase, arity); cerr != nil {
			return 0, cerr
		}
	} else {
		for i, el := range probe {
			reg, cerr := c.compileExpr(el)
			if cerr != nil {
				return 0, cerr
			}
			c.emit(Instruction{Op: OpSCopy, P1: reg, P2: xBase + i})
		}
	}
	// Per-column affinity and collation combine both sides (unlike the IN-list
	// form), e.g. TEXT x IN (SELECT intcol) coerces numerically (subquery.test).
	// See inSubCollation. An unanalyzable subquery leaves each column to X[i].
	subAffs, subMats, _ := c.pager.subqueryOutputAffinities(x.Sub, c.affCtx())
	subColls, _ := c.pager.subqueryOutputCollations(x.Sub, c.affCtx())
	// A SUBQUERY probe has no element expressions, so its per-column operands
	// are the synthetic affExpr/collExpr its own output columns yielded -- the
	// same stand-ins the RIGHT side already uses, just on the left.
	if lhsSub != nil {
		for i := range probe {
			probe[i] = affExpr{aff: lhsAffs[i], materialized: lhsMats[i]}
		}
	}
	affs := make([]affinity, arity)
	for i, el := range probe {
		rhs := affExpr{}
		if i < len(subAffs) {
			rhs = affExpr{aff: subAffs[i], materialized: subMats[i]}
		}
		affs[i] = comparisonAffinity(c.affCtx(), el, rhs)
	}
	lhsCollOperands := probe
	if lhsSub != nil {
		lhsCollOperands = lhsColls
	}
	colls := inSubRowCollations(c.affCtx(), lhsCollOperands, subColls)
	plan := &inSubPlan{prog: prog, affs: affs, colls: colls, not: x.Not, correlated: prog.Correlated}
	d := c.alloc()
	// A correlated membership subquery re-runs per evaluation (no cache slot);
	// an uncorrelated one runs once and caches, keyed by a fresh slot.
	slot := 0
	if !prog.Correlated {
		slot = c.allocSub()
	}
	c.emit(Instruction{Op: OpInSub, P1: xBase, P2: d, P3: slot, P4: plan})
	return d, nil
}

// compileCase compiles both CASE forms via a jump chain: only WHENs up to the
// first match are evaluated, and only the matching THEN.
func (c *compiler) compileCase(x CaseExpr) (int, error) {
	if operand, kw, ok := isBoolParts(x); ok {
		isCol, err := c.boolKeywordIsColumn(kw)
		if err != nil {
			return 0, err
		}
		if isCol {
			// resolveExprStep resolved the keyword to a column, so the node
			// never became TK_TRUTH (resolve.c:1427-1431) and stays the
			// ordinary TK_IS comparison parse.y:1435 built. An IS NOT form is
			// the NOT desugarIsBool wrapped around this node; IS is never
			// NULL, so that NOT is exactly TK_ISNOT.
			return c.compileExpr(BinaryExpr{Op: "IS", L: operand, R: kw})
		}
	}
	d := c.alloc()
	var endJumps []int

	if x.Base == nil {
		// Searched CASE: first truthy WHEN wins.
		for _, w := range x.Whens {
			cond, err := c.compileExpr(w.When)
			if err != nil {
				return 0, err
			}
			// Skip this arm unless the condition is truthy (NULL is not truthy,
			// so jumpIfNull=1 sends NULL to the next arm too).
			jNext := c.emit(Instruction{Op: OpIfNot, P1: cond, P3: 1})
			then, err := c.compileExpr(w.Then)
			if err != nil {
				return 0, err
			}
			c.emit(Instruction{Op: OpSCopy, P1: then, P2: d})
			endJumps = append(endJumps, c.emit(Instruction{Op: OpGoto}))
			c.patch(jNext, c.here())
		}
	} else {
		// Simple CASE: Base compared against each WHEN with "=" semantics; a
		// NULL Base matches nothing.
		base, err := c.compileExpr(x.Base)
		if err != nil {
			return 0, err
		}
		jBaseNull := c.emit(Instruction{Op: OpIsNull, P1: base}) // -> ELSE
		for _, w := range x.Whens {
			when, err := c.compileExpr(w.When)
			if err != nil {
				return 0, err
			}
			jWNull := c.emit(Instruction{Op: OpIsNull, P1: when}) // NULL WHEN never matches
			aff := c.compAffinity(x.Base, w.When)
			coll := c.compCollation(x.Base, w.When)
			jEq := c.emit(Instruction{Op: OpEq, P1: when, P3: base, P4: coll, P5: uint16(aff)})
			jNotEq := c.emit(Instruction{Op: OpGoto})
			c.patch(jEq, c.here())
			then, err := c.compileExpr(w.Then)
			if err != nil {
				return 0, err
			}
			c.emit(Instruction{Op: OpSCopy, P1: then, P2: d})
			endJumps = append(endJumps, c.emit(Instruction{Op: OpGoto}))
			next := c.here()
			c.patch(jWNull, next)
			c.patch(jNotEq, next)
		}
		c.patch(jBaseNull, c.here())
	}

	// ELSE (or NULL when there is no ELSE).
	if x.Else != nil {
		els, err := c.compileExpr(x.Else)
		if err != nil {
			return 0, err
		}
		c.emit(Instruction{Op: OpSCopy, P1: els, P2: d})
	} else {
		c.emit(Instruction{Op: OpNull, P2: d})
	}
	end := c.here()
	for _, j := range endJumps {
		c.patch(j, end)
	}
	return d, nil
}

// boolKeywordIsColumn reports whether TRUE/FALSE in an isBool CASE resolves to a
// column, which C checks before sqlite3ExprIdToTrueFalse (resolve.c:719,
// 747). It compiles the reference without its FallbackLiteral: only "no such
// column" means the keyword; any other error is the statement's. Anything but a
// ColumnExpr there stays a truth test.
func (c *compiler) boolKeywordIsColumn(kw Expr) (bool, error) {
	var ce ColumnExpr
	switch k := kw.(type) {
	case ColumnExpr:
		ce = k
	case groupBareColExpr, whereFixedCol:
		// Placeholders only a RESOLVED column is ever rewritten to (a bare
		// column of an aggregate query, a WHERE-propagated constant column).
		return true, nil
	default:
		return false, nil
	}
	if ce.Qualifier != "" || ce.FallbackLiteral == nil {
		return false, nil
	}
	probe := ce
	probe.FallbackLiteral = nil
	nInsn, nReg := len(c.insns), c.nReg
	_, err := c.compileExpr(probe)
	c.insns, c.nReg = c.insns[:nInsn], nReg
	if err != nil {
		if unqualifiedColumnNotFoundErr(err, ce.Name) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// unknownFunctionName reports whether C also refuses the name ("no such
// function", resolve.c:1290), as opposed to a known function this call site did
// not lower: aggregates and window functions the aggregate/window codegen
// declined, and fts auxiliary/optimize() calls whose first argument named no fts
// table. Those keep the plain decline.
func unknownFunctionName(name string) bool {
	n := r33sFoldIdent(name)
	switch {
	case isAggregateFuncName(n), isWindowFunctionName(n),
		fts5AuxFuncName(n), fts3AuxFuncName(n), fts3OptimizeFuncName(n),
		fts5ScalarFuncName(n), isConnStateFunc(n):
		return false
	}
	return true
}

// unknownOrUnsupportedFuncErr is checkExprSupported's verdict on a name outside
// supportedFuncs: errVDBESemantic "no such function" for an unknown name
// (resolve.c:1290), the old wording otherwise. checkExprSupported runs before
// compileFunc for a select list and WHERE, so this is where e.g.
// "INSERT INTO t SELECT nosuchfn(a) FROM s" lands.
func unknownOrUnsupportedFuncErr(name string) error {
	if unknownFunctionName(name) {
		return semanticf("engine: no such function: %s", name)
	}
	if err := misuseOfFuncErr(name, false); err != nil {
		return err
	}
	return fmt.Errorf("engine: unsupported function %s()", name)
}

// misuseOfFuncErr is "misuse of %s function %#T()" (resolve.c:1265-1277), or
// nil. An aggregate or window function reaching a scalar context was written
// where it is not allowed. It is "window" for a window function even without
// OVER, and for an aggregate written with OVER, hence hasOver.
//
// An aggregate has two messages: resolve.c:1274's "misuse of aggregate function
// sum()" where no aggregate is allowed at all (e.g. nested in another
// aggregate), and expr.c:5333's "misuse of aggregate: sum()" at codegen (e.g.
// in the WHERE of an aggregate query). The scalar compiler is expr.c's site, the
// aggregate rewriter resolve.c's. Window functions have one message.
func misuseOfFuncErr(name string, hasOver bool) error {
	return misuseOfFuncErrAt(name, hasOver, false)
}

func misuseOfFuncErrAt(name string, hasOver, codegen bool) error {
	folded := r33sFoldIdent(name)
	switch {
	case hasOver || isWindowFunctionName(folded):
		return semanticf("engine: misuse of window function %s()", name)
	case isAggregateFuncName(folded):
		if codegen {
			return semanticf("engine: misuse of aggregate: %s()", name)
		}
		return semanticf("engine: misuse of aggregate function %s()", name)
	}
	return nil
}

// declineOrSemantic wraps an error a compile-path validator did not classify: a
// prepare-time rejection (errVDBESemantic) passes through unchanged; anything
// else becomes a decline.
func declineOrSemantic(err error) error {
	if errors.Is(err, errVDBESemantic) {
		return err
	}
	// NOT declineOrSemantic(err) -- this IS it. A mechanical migration of
	// every `fmt.Errorf("%w: %v", errVDBEUnsupported, err)` call site rewrote
	// this function's own body too, which made it call itself unconditionally:
	// a stack overflow, and a PANIC, on the first decline any statement made.
	return fmt.Errorf("%w: %v", errVDBEUnsupported, err)
}

// compileFunc compiles a scalar function call: arguments into a register block,
// then OpFunction (callScalarFuncEnc). Arity is checked here against funcArity, so
// a wrong-arity call errors even when no row runs it ("SELECT coalesce(1) FROM
// t LIMIT 0").
func (c *compiler) compileFunc(x FuncExpr) (int, error) {
	// "f(*)" on a non-aggregate is "f()": the grammar passes zero arguments and the
	// arity check decides (abs(*) is an arity error).
	// likelihood()'s probability is checked when the call is coded, as C does: a
	// view body is stored but not prepared, so "CREATE VIEW v AS SELECT
	// likelihood(1, 9)" succeeds and fails only on use. NameAsWritten keeps the
	// spelling the message echoes.
	if x.Name == "likelihood" && len(x.Args) == 2 {
		spelling := x.NameAsWritten
		if spelling == "" {
			spelling = x.Name
		}
		if lerr := checkLikelihoodLiteralArgNamed(spelling, x.Args[1]); lerr != nil {
			return 0, lerr
		}
	}
	if x.Star && len(x.Args) != 0 {
		return 0, fmt.Errorf("%w: %s(*) with arguments", errVDBEUnsupported, x.Name)
	}
	// An fts3/fts4 auxiliary call ("offsets(<tab>)", "matchinfo(<tab>)") is
	// not a scalar function at all -- its first argument names a table, and
	// its value comes from the statement's MATCH query plus the current row
	// (fts3_search.go). It is tried first, and only when that argument really
	// does name an fts3 table; otherwise the name falls through to the
	// ordinary (rejecting) path, so an "offsets" that is not fts3's is still
	// unsupported.
	if fts3AuxFuncName(x.Name) {
		if reg, ok, err := c.compileFts3Aux(x); ok || err != nil {
			return reg, err
		}
	}
	// fts3/fts4's optimize() is not an auxiliary function -- it reads no row --
	// but it resolves its table argument the same way and falls through the
	// same way when the argument does not name one (fts3_optimize.go).
	if fts3OptimizeFuncName(x.Name) {
		if reg, ok, err := c.compileFts3Optimize(x); ok || err != nil {
			return reg, err
		}
	}
	// An fts5 auxiliary call (bm25()/highlight()/snippet(), fts5_vdbe_aux.go)
	// is likewise not a scalar function -- its first argument names the fts5
	// table, and its value comes from this query's own MATCH plus the current
	// row. Same fallthrough shape as the fts3 case above: tried first, and
	// only when c.fts5Aux was actually resolved for this query AND the call's
	// own first argument names that table.
	if fts5AuxFuncName(x.Name) {
		if reg, ok, err := c.compileFts5Aux(x); ok || err != nil {
			return reg, err
		}
	}
	// A connection-state function (sqlite_version/changes/total_changes/
	// last_insert_rowid) is not a value function at all -- see conn_state.go.
	// It is lowered to its own opcode, which reads the connection at RUN time.
	if isConnStateFunc(x.Name) {
		if len(x.Args) != 0 {
			// C SQLite rejects this at PREPARE time ("wrong number of
			// arguments to function changes()" -- verified directly), so it is
			// errVDBESemantic: surfacing it is the statement's real error, not
			// a decline.
			return 0, semanticf("wrong number of arguments to function %s()", x.Name)
		}
		d := c.alloc()
		c.emit(Instruction{Op: OpConnState, P3: d, P4: x.Name})
		return d, nil
	}
	// fts5's scalar functions (fts5_locale, fts5_source_id, fts5,
	// fts5_insttoken) are the fts5 extension's own, admitted by a PREDICATE
	// rather than by supportedFuncs entries (fts5_locale.go says why an
	// unconditional entry would be a wrong answer in the default build). Their
	// arity is fixed by sqlite3_create_function (fts5_main.c:3821-3845), so any
	// other count falls through to the ordinary rejection below.
	lo, hi, fts5Scalar := fts5ScalarFuncArity(x.Name)
	fts5Scalar = fts5Scalar && len(x.Args) >= lo && len(x.Args) <= hi
	if fts3TokenizerCallDeclined(x) {
		return 0, fmt.Errorf("%w: fts3_tokenizer() over a bound parameter, whose sqlite3_value_frombind() form returns a tokenizer address", errVDBEUnsupported)
	}
	if !supportedFuncs[x.Name] && !fts5Scalar {
		if unknownFunctionName(x.Name) {
			// A name no SQLite build registers: "no such function" (resolve.c:1290), as
			// errVDBESemantic so it propagates (see compileColumn's "no such column").
			return 0, semanticf("engine: no such function: %s", x.Name)
		}
		if err := misuseOfFuncErrAt(x.Name, x.Over != nil, true); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w: function %s()", errVDBEUnsupported, x.Name)
	}
	if min, max, ok := funcArity(x.Name); ok {
		n := len(x.Args)
		if n < min || (max >= 0 && n > max) {
			if isAggregateFuncName(x.Name) {
				// An aggregate reaching the scalar compiler was written where aggregates are
				// not allowed: "misuse of aggregate: max()" (expr.c:5333), not the scalar
				// max()'s arity error.
				return 0, semanticf("misuse of aggregate: %s()", x.Name)
			}
			// semanticf, exactly like the "no such function" a few lines up:
			// a wrong ARGUMENT COUNT is a prepare-time rejection C SQLite
			// makes too, so it is the STATEMENT's error. As an ordinary
			// decline it reached the caller inside "VDBE-only: ... not
			// compilable to bytecode ... (no fallback)", so "SELECT abs(1,2)"
			// read as an engine limitation where 3.53.3 says plainly
			// "wrong number of arguments to function abs()".
			return 0, semanticf("wrong number of arguments to function %s()", x.Name)
		}
	}
	// FILTER on a non-aggregate is refused by resolve.c (:1297-1299, "FILTER may not
	// be used with non-aggregate"), after the no-such-function and arity checks
	// (:1290, :1292). Otherwise the clause would be silently ignored.
	if x.Filter != nil {
		return 0, semanticf("engine: FILTER may not be used with non-aggregate %s()", x.Name)
	}
	// ...and an ORDER BY on one, the very next arm of the same chain:
	//
	//	resolve.c:1306  "else if( is_agg==0 && pExpr->pLeft ){"
	//	resolve.c:1307  "  sqlite3ExprOrderByAggregateError(pParse, pExpr);"
	//
	// ("ORDER BY may not be used with non-aggregate %#T()", expr.c:1205).
	// Behind FILTER's, so "abs(x ORDER BY x) FILTER (WHERE 1)" is the FILTER
	// message there too.
	if len(x.OrderBy) > 0 {
		return 0, semanticf("engine: ORDER BY may not be used with non-aggregate %s()", x.Name)
	}
	// sqlite_compileoption_used(sqlite_compileoption_get(0)) is 1 on every build
	// (ctime.test:190), so fold it to that constant instead of compiling get(0),
	// which declines. See compileOptionUsedOfGetIsOne for why only index 0. After
	// the arity checks; the DDL gates read the unfolded AST by name, since C rejects
	// both functions in index expressions, partial-index WHERE and generated
	// columns (resolve.c:1220).
	if compileOptionUsedOfGetIsOne(x) {
		return c.compileExpr(LiteralExpr{Val: Value{Typ: Int, I: 1}})
	}
	if x.Name == "likelihood" && len(x.Args) == 2 {
		// likelihood()'s second argument must be a constant literal (func3.test),
		// checked here as checkExprSupported does, as errVDBESemantic.
		if err := checkLikelihoodLiteralArg(x.Args[1]); err != nil {
			return 0, semanticf("%v", err)
		}
	}
	base := c.allocN(len(x.Args))
	for i, a := range x.Args {
		r, err := c.compileExpr(a)
		if err != nil {
			return 0, err
		}
		c.emit(Instruction{Op: OpSCopy, P1: r, P2: base + i})
	}
	d := c.alloc()
	// P5 is C SQLite's OP_PureFunc: when this call is being compiled INTO
	// a schema expression whose value must be deterministic, the opcode
	// carries which one, and the date/time functions then refuse to read the
	// clock under it (pureFuncGuard). C spells the same thing as a different
	// OPCODE with p5 = NC_IsCheck/NC_GenCol (expr.c's OP_PureFunc,
	// vdbeaux.c:5627); one opcode with a p5 is the same instruction stream.
	pure := c.pureFuncP5
	if x.CurrentTimeKw {
		// currentTimeFunc (date.c) is its own registered function and never
		// calls sqlite3NotPureFunc, so CURRENT_DATE inside a CHECK is legal
		// where date() is not. See FuncExpr.CurrentTimeKw.
		pure = pureCtxNone
	}
	c.emit(Instruction{Op: OpFunction, P1: base, P2: len(x.Args), P3: d, P4: x.Name, P5: pure})
	return d, nil
}

// ---- schema-time row expressions ----
//
// CHECK constraints, generated columns, partial-index WHERE and expression-index
// keys are schema expressions evaluated against one row. C compiles them all
// against a register block holding the row, via pParse->iSelfTab (expr.c:5047;
// CHECK insert.c:2066, generated columns insert.c:285/353/372, partial index
// insert.c:2416, index keys insert.c:2431).
//
// selfRowExpr does the same for callers with no enclosing program (bulk index
// build, ALTER ADD COLUMN's CHECK back-fill, an FK SET NULL cascade): the
// expression is compiled once when the schema object is built, and eval seeds
// the register block from a row and runs it.
type selfRowExpr struct {
	// expr is the source tree. It survives for exactly one purpose: eval's
	// RE-STAMP, which compiles it again against the scope the row it is handed
	// actually belongs to (selfRowExpr.restamp, vdbe_run.go). Nothing walks it.
	expr Expr

	// pureCtx names the schema expression this is, for C SQLite's
	// OP_PureFunc rule: pureCtxCheck, pureCtxGenerated or pureCtxIndex
	// (sqlite3NotPureFunc, vdbeaux.c:5634-5641). Zero for the selfRowExpr
	// kinds the rule does not cover -- a DEFAULT clause, whose "DEFAULT
	// CURRENT_TIMESTAMP" is exactly the per-INSERT clock read the rule exists
	// to allow, and an ordinary WHERE disjunct. Baked into the compiled
	// OpFunction's P5; kept here only so restamp can compile it again the
	// same way.
	pureCtx uint16

	// prog is nil when the expression did not compile. That is deliberately NOT
	// an error HERE: rejecting a schema object C SQLite accepts would be a
	// spurious error at CREATE TABLE/INDEX time, which invariant 2 ("never
	// wrong") rules out in both directions. It becomes one only if the
	// expression still will not lower when a row arrives to evaluate it against
	// (selfRowExpr.restamp, vdbe_run.go), which is RULE #1's own answer.
	prog *Program

	// cols is the column-list slice this program was compiled against, kept by
	// identity. The program addresses columns by position, so a reordered list
	// (DROP COLUMN then ADD COLUMN keeps the width) would read a neighbour's value.
	// eval runs only when handed this very slice (sameColumnList); sites that
	// rebuild a column list re-stamp the programs (refreshRowPrograms). RENAME
	// COLUMN edits in place and keeps the identity, which is correct.
	cols []columnInfo

	// colBase is the register holding column 0; rowidReg is the one
	// "immediately prior to the first column" (expr.c:5051), holding the
	// rowid pseudo-column; resultReg is where the expression's value lands.
	// nCols is len(cols) at compile time -- the number of registers the row is
	// copied into, which is why eval also requires a row at least that wide.
	colBase   int
	rowidReg  int
	resultReg int
	nCols     int

	// ignoreDbQualifier is the name context this expression was compiled under
	// (compiler.ignoreDbQualifier), kept only so eval's re-stamp reproduces it
	// rather than compiling a CHECK body under an index expression's rules.
	ignoreDbQualifier bool
}

// compileSelfRowExpr compiles e against scope's columns held in registers. A
// shape compileExpr cannot lower yields a nil Program, an error only when a row
// arrives (selfRowExpr.eval). No pager and no cursors, so subqueries decline;
// C forbids them in these contexts anyway. ignoreDbQualifier is the caller's
// name context: true for CHECK bodies and partial-index WHERE (resolve.c:316).
func compileSelfRowExpr(scope tableScope, e Expr, ignoreDbQualifier bool, pureCtx uint16) *selfRowExpr {
	return compileSelfRowExprPaged(scope, e, ignoreDbQualifier, nil, pureCtx)
}

// compileSelfRowExprPaged is compileSelfRowExpr with a pager, so the expression
// may hold a subquery over a real table. Schema-time callers pass nil (C forbids
// subqueries there, NC_SelfRef, resolve.c:1389). The exception is a virtual
// table's RETURNING list (buildVtabReturningPlan), e.g. "... RETURNING
// (SELECT b FROM t2)".
func compileSelfRowExprPaged(scope tableScope, e Expr, ignoreDbQualifier bool, pager *ReadOnlyPager, pureCtx uint16) *selfRowExpr {
	p := &selfRowExpr{expr: e, cols: scope.cols, nCols: len(scope.cols), ignoreDbQualifier: ignoreDbQualifier, pureCtx: pureCtx}
	if e == nil {
		return p
	}
	// rowLive only with a pager: eval seeds the register block before running, so
	// a subquery beneath can read it across the frame. That is what a vtab
	// RETURNING subquery naming the returned row needs (resolve.c:528,
	// trigger.c:1074; returning1.test 13.1). Schema-time callers pass nil and keep
	// declining.
	c := &compiler{ignoreDbQualifier: ignoreDbQualifier, pager: pager, rowLive: pager != nil, pureFuncP5: pureCtx}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	// The rowid register comes first, so it sits "immediately prior to the
	// first column" exactly as expr.c:5051 describes its own layout.
	p.rowidReg = c.alloc()
	p.colBase = c.allocN(len(scope.cols))
	sc := scope
	c.pushRegScope(regScope{scope: &sc, regs: rowRegsFor(p.colBase, len(sc.cols)), rowidReg: p.rowidReg})
	reg, err := c.compileExpr(e)
	c.popRegScope()
	if err != nil {
		return p
	}
	// eval gives this program only registers and the run-once subquery cache; its
	// pooled machine has no cursors, record registers, sorters or distinct sets, so
	// a compile that allocated one would panic at run time. Decline those. With no
	// pager, only FROM-less subqueries survive compileExpr (C would reject them at
	// DDL time, NC_SelfRef, which this engine does not yet do), so nSub is served.
	// With a pager, cursors and record registers are expected and sized by eval.
	// Sorters and distinct sets stay refused.
	if c.nSorter != 0 || c.nDistinct != 0 {
		return p
	}
	if pager == nil && (c.nCursor != 0 || c.nRec != 0) {
		return p
	}
	// No OpResultRow: this program yields ONE value, and eval reads it straight
	// out of its register. Emitting a result row instead would allocate a row
	// and a row list per evaluation, and these run once per row of a scan.
	c.emit(Instruction{Op: OpHalt})
	p.resultReg = reg
	p.prog = &Program{Insns: c.insns, NReg: c.nReg, NSubCache: c.nSub}
	return p
}

// sameColumnList reports whether a and b are the same slice (same backing array
// and length), not equal values: a program addressing columns by position is
// valid only for the list it was compiled against (see selfRowExpr.cols). A
// false negative just recompiles (selfRowExpr.restamp);
// TestSelfRowExprProgramsStayCurrent guards the re-stamp sites.
func sameColumnList(a, b []columnInfo) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || &a[0] == &b[0]
}

// selfRowScope is the register-backed scope a schema-time expression of tbl's
// resolves against. It is built to match rowEvalCtxAs's own scope field for
// field (write_update_delete.go), which is what guarantees the compiled
// program and the row context it is seeded from cannot disagree about what a
// bare name means.
func selfRowScope(tbl *tableMeta) tableScope {
	return tableScope{name: tbl.name, cols: tbl.cols, colIndex: buildColIndex(tbl.cols), noRowid: tbl.withoutRowid}
}
