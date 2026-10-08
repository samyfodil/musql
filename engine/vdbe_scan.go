// Compilation of a SELECT with a FROM clause into a cursor-driven Program:
// joins of any kind (N nested cursor loops, emitJoinLoops,
// vdbe_join_codegen.go; NATURAL/USING desugared by desugarJoinItem;
// RIGHT/FULL via OpRightJoinMark and an appended sweep, emitRightOuterSweep),
// derived tables, views, CTEs, DISTINCT, ORDER BY (compileScanSorted,
// vdbe_sort_codegen.go), aggregates and GROUP BY (vdbe_agg_codegen.go) and
// windows (vdbe_window.go). A shape the compiler cannot lower is an error
// (errVDBEUnsupported); there is no fallback.
//
// Without ORDER BY the program is SQLite's full-scan skeleton generalized to N
// loops: OpenRead (every cursor); Rewind->end (per level); WHERE/OFFSET/LIMIT
// checks, select list, ResultRow (innermost); Next->loop (per level); end:
// Close; Halt. LIMIT/OFFSET are counter registers, and the scan stops once
// LIMIT is satisfied, as C does. With ORDER BY that early stop no longer
// holds, so compileScanSorted emits a two-phase skeleton.
package engine

import (
	"errors"
	"fmt"
	"math"
)

// distinctCollations returns the collation DISTINCT dedups each output column
// under, or nil when all are BINARY (OpGroup's P4 convention; keysEqualGrouping
// then uses keysEqual). The rule is topExprCollation's; over a NOCASE column
// holding one/ONE/One/two/TWO:
//
//	SELECT DISTINCT a                 -> 2 rows   (the declared collation)
//	SELECT DISTINCT a COLLATE BINARY  -> 5 rows   (an explicit one overrides)
//	SELECT DISTINCT a||''             -> 5 rows   (an expression loses it)
//	SELECT DISTINCT upper(a)          -> 2 rows   (upper() folds, BINARY dedup)
//
// per column independently. The first row seen is kept (checkAndAdd).
func distinctCollations(c *compiler, outCols []outputColumn) []string {
	colls := make([]string, len(outCols))
	any := false
	for i, oc := range outCols {
		if n, ok := topExprCollation(c.affCtx(), oc.expr); ok && !equalFoldName(n, "BINARY") {
			colls[i] = n
			any = true
		}
	}
	if !any {
		return nil
	}
	return colls
}

// compileSelectScan compiles stmt against p's schema: it resolves every FROM
// item into a joinSource (vdbe_join_codegen.go), builds the compiler with its
// scopes, and dispatches to the GROUP BY, whole-table aggregate, window, plain
// or sorted compiler (GROUP BY wins over plain aggregate mode).
func compileSelectScan(p *ReadOnlyPager, stmt *SelectStmt, outer *compiler) (*Program, error) {
	return compileSelectScanRow(p, stmt, outer, nil)
}

// compileSelectScanRow is compileSelectScan with a live enclosing-row evalCtx
// as a last resort (compiler.rowOuter, as compileSelectNoFromRow has), so an
// unqualified correlated reference resolves: "SELECT a, (SELECT d FROM tb
// WHERE a=c) FROM ta GROUP BY a". C's lookupName walks out through the
// NameContext chain whether or not a qualifier is written.
//
// compileColumn consults rowOuter only after this compile's own scopes
// (derived tables, views, CTEs) fail, so a name the subquery's FROM provides
// binds there first. The value is baked in as a literal, which is sound only
// because such a compile is never cached: pass rowOuter only for a fresh
// per-enclosing-row compile.
func compileSelectScanRow(p *ReadOnlyPager, stmt *SelectStmt, outer *compiler, rowOuter *evalCtx) (*Program, error) {
	// SQLite's count-of-view optimization runs BEFORE anything else looks at
	// the FROM clause (sqlite3Select, select.c:7938), and it is not optional
	// here: it DELETES each compound arm's own select list, which is the only
	// reason "SELECT count(*) FROM (SELECT 1 UNION ALL SELECT sum(DISTINCT
	// c1))" answers in C SQLite rather than raising "misuse of aggregate".
	// See count_of_view.go. It yields a FROM-LESS statement, so the dispatch
	// this function is the entry to no longer applies and the FROM-less
	// compiler takes it -- exactly as compileSubProgram routes one.
	if rw := countOfViewRewrite(p, stmt, outer, rowOuter); rw != stmt {
		return compileSelectNoFromTrig(p, rw, outer, enclosingTriggerCtx(outer), rowOuter, nil)
	}
	// Attempt 1 compiles the statement as written. If a subquery inside holds an
	// aggregate C associates with this query (vdbe_agg_hoist.go), that compile
	// records it in the box and fails; attempt 2 compiles the same statement as
	// an aggregate query owning those calls, so "SELECT (SELECT count(a1) FROM
	// t2) FROM t1" is one row. The retry keys off the box rather than an error
	// (errors are rewrapped with %v on the way out), and only after a failed
	// attempt.
	box := &hoistBox{}
	prog, err := compileScanAttempt(p, stmt, outer, rowOuter, box, nil)
	if err != nil && len(box.specs) > 0 {
		prog, err = compileScanAttempt(p, stmt, outer, rowOuter, &hoistBox{}, box.specs)
	}
	if err == nil {
		// The JIT peepholes over the finished program (segment_peephole.go), each
		// leaving it untouched when it does not match. They read different
		// skeletons (one loop with a result register; a sorter filled by one loop
		// and drained by another). The vector kernel goes first since it is
		// fastest for its one shape; the compiled program covers the rest built
		// from comparisons. (segPeepholesOffForTest: a differential test's
		// reference is the plain loop.)
		if !segPeepholesOffForTest && !segPeephole(prog) && !segOrderPeephole(prog) &&
			!segGroupPeephole(prog) {
			segProgPeephole(prog)
		}
		// And the ROW recogniser, which is not one of the alternatives above:
		// those four REPLACE a loop with a guarded fast path that answers the
		// statement, so at most one can apply. This one replaces nothing. It
		// hangs a candidate-set restriction on an OpRewind that stays exactly
		// where it was, driving the loop the compiler already emitted, so it
		// composes with whatever did or did not match (segment_row_filter.go).
		if !segPeepholesOffForTest {
			segRowFilterPeephole(prog)
			// A correlated EXISTS as a subroutine of this program rather than
			// a machine per outer row (exists_inline.go).
			existsInlinePeephole(prog)
		}
	}
	return prog, err
}

// compileScanAttempt is one pass of the above: box collects the aggregates an
// inner compile decides THIS query owns, and hoisted is what a previous pass
// collected (empty on the first). Everything else is compileSelectScanRow's own
// documented behavior.
func compileScanAttempt(p *ReadOnlyPager, stmt *SelectStmt, outer *compiler, rowOuter *evalCtx, box *hoistBox, hoisted []hoistSpec) (prog *Program, err error) {
	// noSolution is where.c's "no query solution" for this statement
	// (selectIndexedByNoSolution below). C raises it from sqlite3WhereBegin,
	// AFTER sqlite3SelectPrep has resolved every name, so any error the rest
	// of this compile finds first is the one C reports -- "SELECT nosuch FROM
	// t INDEXED BY px WHERE a=1" is "no such column: nosuch" there.
	var noSolution error
	defer func() {
		if err == nil && noSolution != nil {
			prog, err = nil, noSolution
		}
	}()
	if len(stmt.Compound) != 0 {
		return nil, fmt.Errorf("%w: compound SELECT", errVDBEUnsupported)
	}
	if len(stmt.From) == 0 {
		return nil, fmt.Errorf("%w: FROM-less SELECT", errVDBEUnsupported)
	}
	// SQLite's query flattener moves a FROM-clause subquery's LIMIT onto the
	// query that reads it (select.c:4695), so the bound lands AFTER the outer
	// ORDER BY rather than before it: "SELECT * FROM (SELECT a FROM t LIMIT 2)
	// ORDER BY 1 DESC" over 1..5 answers 5,4 there and answered 2,1 here. Done
	// FIRST, so every dispatch below plans the already-transferred shape. It is
	// a no-op outside that one rewritable shape -- see flatten_limit_r35c.go.
	stmt = r35cFlattenSubqueryLimit(stmt)
	// "HAVING clause on a non-aggregate query" -- checked here, before any
	// path that would otherwise IGNORE a GROUP-BY-less HAVING outright (the
	// plain/sorted/window row-mode compilers below never look at it). See
	// requireAggregateForHaving, sql_agg.go.
	if err := requireAggregateForHaving(stmt); err != nil {
		return nil, err
	}
	// maxJoinTables caps the FROM items this compiler accepts. emitJoinLoops does
	// per-conjunct WHERE/ON pushdown (planJoinPushdown: reordering, per-level
	// buckets, deferred residue), so a wide join prunes deeper loops instead of
	// building the cross product. select5.test's 4..64-table joins (chains of
	// column=column links plus one anchoring equality) run in the same time class
	// as before (TestVDBEWideJoinTiming), hence 64. A genuinely unfilterable wide
	// join is slow at any cap. Register allocation and emitJoinLevel are general
	// in N; this is a performance cap. Re-measure against select4/select5 before
	// raising it.
	const maxJoinTables = 64
	if len(stmt.From) > maxJoinTables {
		return nil, fmt.Errorf("%w: join over more than %d tables", errVDBEUnsupported, maxJoinTables)
	}

	c := &compiler{pager: p, outer: outer, rowOuter: rowOuter, hoist: box, hoisted: hoisted,
		trig: enclosingTriggerCtx(outer)}
	c.nQueryLoop, c.nQueryLoopKnown = inheritedNQueryLoop(outer)
	c.fts3Conjuncts, c.fts3Elsewhere = fts3LangidContext(stmt)
	// A recursive CTE consumed only through a LIMIT can stop early. SQLite runs
	// it as a co-routine feeding the outer query (generateWithRecursiveQuery,
	// fromClauseTermCanBeCoroutine, select.c), so "WITH RECURSIVE i(x) AS
	// (VALUES(1) UNION ALL SELECT x+1 FROM i) SELECT x FROM i LIMIT 10" ends
	// when the LIMIT does. This engine materializes the CTE, so the consumer's
	// bound is published on the reference (resolveCTESource ->
	// compileRecursiveCTE) and the queue program stops at that row; rows come
	// in output order and the consumer takes a prefix, so the answer is the
	// same.
	//
	// It is done here, not per statement, because derived tables and
	// scalar/IN/EXISTS subqueries are sub-Programs resolving their FROM through
	// resolveJoinSources.
	//
	// recursiveCTEOuterCap is conservative: anything that could read past the
	// first limit+offset rows (WHERE, ORDER BY, GROUP BY, DISTINCT, a join, a
	// compound, a select-list item that might read the CTE again) leaves the
	// bound unset. Over-capping would be a wrong answer.
	if name, capRows, ok := recursiveCTEOuterCap(stmt); ok {
		c.cteOuterCapName, c.cteOuterCapPlus1 = name, capRows+1
	}
	// withVtabWhere (vtab.go) copies the WHERE's top-level conjuncts onto
	// every FROM item's tvfWhere, for buildVtabConstraints
	// (resolveVtabSource) to push hidden-column equalities, an fts5 MATCH or
	// rtree constraints into the module's BestIndex/Filter. Applied before
	// resolveJoinSources, which may recurse into a group's span
	// (resolveGroupSource) and must find tvfWhere already set; a no-op with
	// no WHERE or no FROM.
	from := withVtabWhere(p, stmt)
	c.flatOuter = stmt
	srcs, serr := resolveJoinSources(p, c, from)
	c.flatOuter = nil
	if serr != nil {
		return nil, serr
	}
	// A module that CONSUMES a WHERE conjunct and guarantees it -- C's
	// aConstraintUsage[].omit -- gets it removed from the residual WHERE here,
	// before anything else reads stmt.Where: the planner, the seek detectors and
	// every dispatch below must all see the same clause. Returns stmt itself
	// (no copy, no work) unless a source actually claims one, which today is
	// only fts3tokenize. See vtab_omit.go for the mechanism and why it fails
	// safe; the item's own tvfWhere is left whole, since the constraint still
	// has to be OFFERED to BestIndex.
	stmt = vtabOmitConjuncts(stmt, srcs)
	// checkFromSupported (join.go) checks every FROM item's effective join
	// condition, which for USING/NATURAL exists only after
	// resolveJoinSources desugared it.
	if err := checkFromSupported(onsOfSources(srcs)); err != nil {
		return nil, declineOrSemantic(err)
	}
	// Whether the PORTED SQLite query planner may choose this statement's loop
	// order (where_plan.go). Decided once, here, because this is the only
	// place that has the pager, the compiler and the whole statement at the
	// same time; every dispatch below reaches the planner through these same
	// srcs. See markWherePlanEligibility for what it rules out and why.
	markWherePlanEligibility(p, c, srcs, stmt)
	// An INDEXED BY partial index this WHERE does not imply leaves the table
	// no loop, and C refuses the statement (where_plan_indexed_by.go). A
	// decline is returned now; the error waits for the compile to finish.
	if nerr := selectIndexedByNoSolution(p, c, srcs, stmt); errors.Is(nerr, errVDBEUnsupported) {
		return nil, nerr
	} else if nerr != nil {
		noSolution = nerr
	}
	c.scopes = joinScopes(srcs)
	scopes := tableScopesOf(srcs)
	// joinedTablesFor(srcs) hands fts3MatchBindings the .left/.on/.rightOuter
	// facts ftsMatchLeftJoinUnusable needs (where_plan_fts_forced_order.go) --
	// srcs is already fully resolved here (resolveJoinSources, above), the
	// same projection computeExecOrder/planJoinPushdown themselves consume.
	c.fts3MatchGood = fts3MatchBindings(p, stmt, scopes, joinedTablesFor(srcs))
	c.fts5MatchGood = fts5MatchBindings(p, stmt, scopes)
	// fts3ContentUnneeded's OTHER call site (see FromItem.fts3ContentUnneeded,
	// sql_ast.go, and withVtabWhere, vtab.go): one MatchExpr's scope index
	// bound above only ever needs the strict %_content existence check
	// skipped when fts3StmtContentUnneeded proves the WHOLE statement never
	// reads a real column of it -- see that function's own doc comment
	// (fts3_search.go) for the rule and its C citations.
	c.fts3ContentUnneeded = map[int]bool{}
	for idx := range c.fts3MatchGood {
		if idx < 0 || idx >= len(scopes) {
			continue
		}
		t := &scopes[idx]
		name := t.tableName
		if name == "" {
			name = t.name
		}
		c.fts3ContentUnneeded[idx], _ = fts3StmtContentUnneeded(p, stmt, name, t.name)
	}

	// SQLite's result-set-alias rule: a WHERE / ON / GROUP BY / HAVING name that
	// binds to no FROM column may name a select-list alias and is compiled as
	// its expression (sql_alias.go). Done here, once c.scopes exists, so every
	// downstream compiler inherits it. Skipped inside a register-backed row
	// scope, where compileColumn binds a bare name against the registers first.
	//
	// A trigger body is not skipped: lookupName's trigger arm needs an explicit
	// qualifier,
	//
	//	}else if( op!=TK_DELETE && zTab && sqlite3StrICmp("new",zTab) == 0 ){
	//	  pExpr->iTable = 1;
	//	  pTab = pParse->pTriggerTab;
	//	}else if( op!=TK_INSERT && zTab && sqlite3StrICmp("old",zTab)==0 ){
	//	                                              -- resolve.c:539-543
	//
	// (only RETURNING's NC_UBaseReg arm, resolve.c:528-536, admits an
	// unqualified one, and that is register-backed), and resolveTriggerParam
	// likewise requires a qualifier. The body's SELECT reaches the alias branch
	// like any other (NC_UEList, resolve.c:1997; resolve.c:657-698). Skipping it
	// broke e.g. a trigger body's "INSERT ... SELECT b AS z, count(*) FROM t1
	// GROUP BY z HAVING z IS NOT NULL".
	if len(c.regScopes) == 0 {
		stmt = substituteResultAliases(stmt, srcs, c.scopes, c.pager)
	}
	// The select list an inner compile may hoist an aggregate INTO -- recorded
	// after the alias substitution above, so it is the same tree planning will
	// see. See hoistBox.cols (vdbe_agg_hoist.go).
	if box != nil {
		box.cols = stmt.Columns
	}

	// The cross-table declared-collation OR shape is resolved HERE, before
	// any dispatch, so every downstream path (GROUP BY, aggregate, window,
	// plain, sorted -- and UPDATE ... FROM through them) inherits one
	// decision; it used to live only on the plain/sorted path below and only
	// read stmt.Where, which let the GROUP BY/aggregate paths and every JOIN
	// ON spelling of the same shape run per-term -- a measured WRONG. See
	// collation_or_plan.go for the classification and its oracle evidence.
	if nw, nons, cerr := applyRiskyCollationOrPlan(p, c, stmt, srcs); cerr != nil {
		return nil, cerr
	} else if nw != nil || nons != nil {
		if nw != nil {
			s2 := *stmt
			s2.Where = nw
			stmt = &s2
			if box != nil {
				box.cols = stmt.Columns
			}
		}
		for i := range nons {
			if nons[i] != nil {
				srcs[i].on = nons[i]
			}
		}
	}

	// A retry compile owns aggregates written inside its select list's
	// subqueries (vdbe_agg_hoist.go), so it is an AGGREGATE query even though
	// nothing at the top level of its select list says so -- that is precisely
	// what SQLite's outward association walk decided. Everything the aggregate
	// compilers already decline (a parenthesized join group, a window function)
	// declines here too rather than being answered by the row-mode path, which
	// would silently drop the hoist and answer the wrong ROW COUNT.
	if len(c.hoisted) > 0 {
		if groupPresent(srcs) {
			return nil, fmt.Errorf("%w: hoisted aggregate over a parenthesized join group", errVDBEUnsupported)
		}
		if selectHasWindow(stmt) || orderByHasWindow(stmt) {
			return nil, fmt.Errorf("%w: hoisted aggregate combined with a window function", errVDBEUnsupported)
		}
		if len(stmt.GroupBy) != 0 {
			// Unreachable today and deliberately a decline rather than a
			// dispatch: nothing inside a GROUP BY query's select-list
			// subqueries fills this box in the first
			// place. Should some future path fill it, declining keeps the row
			// count honest instead of planning a hoist nothing delivers.
			return nil, fmt.Errorf("%w: hoisted aggregate into a GROUP BY query", errVDBEUnsupported)
		}
		return compileScanAggregate(c, stmt, srcs, scopes)
	}

	// GROUP BY routes to a GROUP BY compiler; an aggregate in the select list
	// with no GROUP BY routes to the whole-table aggregate compiler
	// (vdbe_agg_codegen.go). Either one combined with a window function goes to
	// compileScanGroupedWindow (vdbe_window_group.go), since the aggregate
	// planner has no window opcode and compileScanWindow's batch is fed by the
	// scan, not the groups.
	//
	// A materialized parenthesized join group is supported: the row gather
	// reads one entry per physical cursor (aggPlan.gathers), since a group is
	// several scopes over one cursor. count(*), sums and max() over "u JOIN (t
	// JOIN w USING(a))" and the ON, comma, LEFT-inner and leading-group
	// spellings match C.
	hasWindow := selectHasWindow(stmt) || orderByHasWindow(stmt)
	if len(stmt.GroupBy) != 0 {
		// GROUP BY over a materialized join group works: resolveJoinSources
		// rebases members' scopes into the join's flat row space, so a group
		// not placed first no longer overwrites earlier sources' slots.
		//
		// One exception: a key resolving to a USING/NATURAL column coalesced by
		// a RIGHT/FULL JOIN inside the group, whose key record would read the
		// member's raw (NULL) column: "SELECT id FROM (t4 RIGHT JOIN t5
		// USING(id)) LEFT JOIN t1 USING(id) GROUP BY id" (C: 0,3,4,5,9;
		// joink_r27_coalesce_chain_test.go). Qualified keys and keys resolving
		// outside the group are fine, so this checks the resolved scope.
		for _, g := range stmt.GroupBy {
			if groupKeyNeedsGroupCoalesce(scopes, srcs, g) {
				return nil, fmt.Errorf("%w: GROUP BY a USING/NATURAL column that a RIGHT/FULL JOIN inside a parenthesized join group coalesces (the key record would read the member's raw NULL)", errVDBEUnsupported)
			}
		}
		if hasWindow {
			return compileScanGroupedWindow(p, stmt, srcs, scopes, outer)
		}
		return compileScanGroupBy(p, c, stmt, srcs, scopes)
	}
	for _, sc := range stmt.Columns {
		if !sc.Star && containsAggregate(sc.Expr) {
			if hasWindow {
				// The COMPOSED window+aggregate path over a group is not
				// covered by the gather change and is not measured, so it
				// keeps the decline rather than being assumed to work.
				if groupPresent(srcs) {
					return nil, fmt.Errorf("%w: aggregate combined with a window function over a parenthesized join group", errVDBEUnsupported)
				}
				return compileScanGroupedWindow(p, stmt, srcs, scopes, outer)
			}
			return compileScanAggregate(c, stmt, srcs, scopes)
		}
	}

	// "*" expansion and result-column naming (including a rowid alias's
	// real IPK-column name), schema-only.
	outCols, oerr := expandSelectList(stmt.Columns, scopes, p.colNameMode())
	if oerr != nil {
		return nil, oerr
	}
	// A parenthesized/derived FROM producing duplicate output column names is
	// declined here too, rather than answered with the wrong ":N"-less column
	// names -- see errIfDuplicateOutputNames (query.go), which raises it.
	if err := errIfDuplicateOutputNames(stmt, outCols, scopes); err != nil {
		return nil, declineOrSemantic(err)
	}
	// A window function needs the WHOLE result set materialized before any
	// value can be assigned to any row (see vdbe_window.go), so it takes its
	// own compile path -- which also owns this query's DISTINCT / ORDER BY /
	// LIMIT, since all three apply AFTER the window values exist.
	if selectHasWindow(stmt) || orderByHasWindow(stmt) {
		return compileScanWindow(c, stmt, srcs, scopes, outCols)
	}

	// fts5's "rank" hidden column and its bm25()/highlight()/snippet()
	// auxiliary functions need a per-query corpus scan done ONCE, here,
	// rather than per row -- see fts5_vdbe_aux.go's package comment. A no-op
	// (c.fts5Aux stays nil) unless the select list or ORDER BY actually
	// references one of them. Deliberately placed AFTER the GROUP BY/
	// aggregate/window returns above, never before: c.fts5Aux's own opcode
	// (compileFts5Rank/compileFts5Aux) gathers the CURRENT ROW from this
	// compile's live cursors (gatherRowScopes), which only holds during a
	// plain/sorted row-mode scan body -- not those other paths' post-scan
	// result phase, which was never analyzed against this and could read the
	// wrong row if it were reachable there.
	fts5Aux, aerr := c.buildFts5AuxState(stmt, scopes)
	if aerr != nil {
		return nil, aerr
	}
	c.fts5Aux = fts5Aux

	// declineOrSemantic, not a blanket errVDBEUnsupported wrap: this gate runs
	// BEFORE any of these expressions is compiled, so it -- not compileFunc --
	// is where an unknown function name in a select list or WHERE is found,
	// and folding C SQLite's own prepare-time rejection into "the VDBE
	// cannot lower this" is what routed "INSERT INTO t SELECT nosuchfn(a) FROM
	// s" into a decline rather than the error it is -- and a decline clobbered
	// changes(). See unknownOrUnsupportedFuncErr (vdbe_codegen.go).
	for _, oc := range outCols {
		if err := checkExprSupported(oc.expr); err != nil {
			return nil, declineOrSemantic(err)
		}
	}
	if err := checkExprSupported(stmt.Where); err != nil {
		return nil, declineOrSemantic(err)
	}
	if len(stmt.OrderBy) == 0 {
		return compileScanPlain(c, stmt, srcs, outCols)
	}
	orderKeys, kerr := resolveOrderKeys(stmt, outCols)
	if kerr != nil {
		return nil, kerr
	}
	return compileScanSorted(c, stmt, srcs, outCols, orderKeys)
}

// compileScanPlain compiles the ORDER-BY-less shape: one scan whose body
// evaluates WHERE, counts LIMIT/OFFSET and emits each surviving row. c has
// every source installed (compileSelectScan); emitJoinLoops owns the loop
// skeleton, and this supplies the innermost body, plus a dedup gate with
// DISTINCT.
//
// DISTINCT reorders the body: whether a row counts toward OFFSET/LIMIT
// depends on its distinctness, so the select list is evaluated first and
// checked against the distinct set (OpDistinct), a duplicate jumping past
// OFFSET/LIMIT/ResultRow like a WHERE miss. The early LIMIT stop stays
// valid: once LIMIT distinct rows are out, every further row is discarded
// anyway.
func compileScanPlain(c *compiler, stmt *SelectStmt, srcs []joinSource, outCols []outputColumn) (prog *Program, err error) {
	// WHICH ROWS a LIMIT or OFFSET keeps, with no ORDER BY, is decided by the
	// order they arrive in -- as is a subquery's first row -- so where the
	// ported planner could not decide the scan (a WHERE term it does not
	// price, say, beside an index C walks) the answer is not provably C's.
	// Measured over t(a,b,c,d) indexed on c: "SELECT a FROM t WHERE c > 3 AND
	// b LIKE 'x1%' LIMIT 2" is 25,21 in C and 1,5 in a rowid scan. Asked before
	// the nQueryLoop bump below, with the trust the FROM clause was planned
	// under. Plain tables only, as for the aggregates (aggPlanOrderProvable): a
	// derived table or CTE arrives in an order its own compile settled.
	unproven := !c.planNRowOK && aggSourcesArePlainTables(srcs) && !joinHasRightOuter(srcs) && (stmt.Limit != nil || stmt.Offset != nil ||
		stmt.LimitParam != nil || stmt.OffsetParam != nil || c.outer != nil) &&
		!anchorPlanOrderProvable(c.pager, c, srcs, tableScopesOf(srcs), stmt)
	if unproven && (stmt.Limit != nil || stmt.Offset != nil || stmt.LimitParam != nil || stmt.OffsetParam != nil) {
		return nil, fmt.Errorf("%w: LIMIT/OFFSET with no ORDER BY over rows whose arrival order is not provably C SQLite's (see anchorPlanOrderProvable)", errVDBEUnsupported)
	}
	defer func() {
		if prog != nil && unproven {
			prog.OrderUnproven = true
		}
	}()
	// Bump nQueryLoop for this whole body (WHERE and select list compile in
	// the same loop window), as sqlite3WhereBegin does after planning
	// ("pWInfo->pParse->nQueryLoop += pWInfo->nRowOut", where.c:7198) and
	// sqlite3WhereEnd restores (where.c:7484/7881).
	//
	// markWherePlanEligibility (run in resolveJoinSources) stashed
	// c.planNRow/planNRowOK when the ported planner decided the plan,
	// including a plain rowid scan. Otherwise C would still have some
	// estimate this package cannot reconstruct, so nQueryLoop is marked
	// unknown for nested compiles rather than left too low.
	savedNQueryLoop, savedNQueryLoopKnown := c.nQueryLoop, c.nQueryLoopKnown
	if c.planNRowOK {
		c.nQueryLoop += c.planNRow
	} else {
		c.nQueryLoopKnown = false
	}
	defer func() { c.nQueryLoop, c.nQueryLoopKnown = savedNQueryLoop, savedNQueryLoopKnown }()

	// Rowid point lookup: if WHERE pins some ordinary rowid source's rowid
	// (or INTEGER PRIMARY KEY) to a constant or parameter, that cursor
	// seeks the one row (OpSeekRowidHint). The full WHERE is still
	// evaluated, so this only restricts candidates, at any FROM position
	// (the key needs no other cursor). One source per query (the first
	// eligible conjunct); others may still get a correlated seek
	// (annotateJoinSeeks).
	if idx, key, ok := detectRowidSeekKey(c, srcs, stmt.Where); ok {
		srcs[idx].seekKeyExpr = key
	} else if idx, plan, ok := detectIndexSeekKey(c, srcs, stmt.Where); ok {
		// No rowid lookup applied; try a secondary-index leading-column equality
		// seek. Same pure candidate-set restriction (the conjunct stays in
		// WHERE and is re-tested on every fetched row), so it too is
		// result-neutral. See detectIndexSeekKey.
		srcs[idx].idxSeek = plan
	}

	// A row-mode scan evaluates WHERE and the select list with every cursor
	// positioned at the current row, so a correlated subquery compiled beneath
	// this body may read these cursors as its live outer source (see
	// compiler.rowLive, vdbe_codegen.go).
	c.rowLive = true
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)

	// Predicate pushdown: split WHERE into conjuncts and place each at the
	// deepest join level that binds all its tables (plan.buckets, emitted by
	// emitJoinLevel), leaving only the conjuncts that must wait for the fully-
	// assembled innermost row (plan.deferred -- LEFT-join-dependent or
	// subquery-carrying) for this body. The two are a complete partition of
	// WHERE, so the row set and order are identical to testing all of WHERE
	// here; only pruning happens earlier. See joinPushdownPlan/buildJoinPlan.
	plan := buildJoinPlan(srcs, stmt.Where)
	// Index-driven joins: seek each INNER source's rowid/index by its join key
	// instead of full-scanning it per outer row. Pure candidate-set restriction
	// (the join condition is still re-evaluated on every fetched inner row) --
	// result- and order-neutral. See annotateJoinSeeks (vdbe_join_seek.go).
	annotateJoinSeeks(c, srcs, plan)
	deferredWhere := andConjuncts(plan.deferred)

	// LIMIT/OFFSET counters: a positive OFFSET gets a register counting down
	// to 0 (each matching row is skipped -- not output, not counted against
	// LIMIT -- while it's still positive); a non-negative LIMIT (nil or
	// negative means unlimited, matching every other LIMIT handling in this
	// package -- see limitOffsetRange) gets its own register, checked BEFORE
	// each candidate row's select-list is evaluated (or, with DISTINCT,
	// immediately after -- see this function's doc comment) and decremented
	// AFTER that row is output, so the scan stops as soon as it reaches 0
	// (see the loop body below).
	var offsetReg, limitReg, oneReg int
	haveOffset, haveLimit := false, false
	// An unresolved LIMIT/OFFSET (SelectStmt.LimitParam/OffsetParam, e.g. a
	// trigger body's "LIMIT new.a") is coded into the counter register a
	// constant would use, as computeLimitRegisters does (select.c:2547-2557),
	// LIMIT before OFFSET. A run-time LIMIT 0 gives no rows (select.c:2531);
	// a negative one is unlimited (select.c:2528); a negative OFFSET is
	// clamped to 0 by OpLimitCounter (OP_IfPos never fires on one).
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

	resultBase := c.allocN(len(outCols))

	var distinctNum, distinctRecReg int
	if stmt.Distinct {
		distinctNum = c.allocDistinct()
		distinctRecReg = c.allocRec()
		c.emit(Instruction{Op: OpDistinctOpen, P1: distinctNum, P4: distinctCollations(c, outCols)})
	}

	// limExhausted collects every LIMIT-reached jump the body emits; each must
	// leave every loop level (progEnd, patched after emitJoinLoops). The early
	// exit skips OpClose, which only frees materialized rows for GC.
	//
	// It is a slice because the body is emitted more than once for outer joins
	// (the LEFT NULL-extension continuation, each RIGHT/FULL sweep). Keeping
	// only the last address left the others jumping to instruction 0, an
	// infinite restart:
	//
	//	CREATE TABLE ja(a1,a2); INSERT INTO ja VALUES('a1',1),('a2',2);
	//	CREATE TABLE jb(b1,b2); INSERT INTO jb VALUES(2,'b0'),(1,'b1'),(2,'b4'),(1,'b5');
	//	SELECT ja.a1, jb.b2 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LIMIT 3;
	//
	// hung (C: a1|b1, a1|b5, a2|b0).
	var limExhausted []int

	body := func() error {
		// WHERE: a false/NULL result skips this combination entirely --
		// jumping straight past the rest of this body (patched in once that
		// address is known), exactly like a non-matching row never reaching
		// OFFSET/LIMIT/the select-list in an ordinary per-row scan.
		whereJump := -1
		if deferredWhere != nil {
			// deferredWhere is andConjuncts of plan.deferred -- itself a set of
			// genuine top-level WHERE AND-conjuncts (splitTopLevelAnd, join.go)
			// recombined into one AND tree, so this IS the query's own WHERE
			// clause membership whereexpr.c's tag-20220128a guards on
			// ("pWC->op==TK_AND"); compileAndOr's re-arming (if deferredWhere is
			// itself an AND) recurses this down to each real leaf conjunct. See
			// compiler.inWhereConjunct's doc comment.
			c.inWhereConjunct = true
			wReg, werr := c.compileExpr(deferredWhere)
			if werr != nil {
				return werr
			}
			whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
		}

		// DISTINCT: compute the select list now (needed to test distinctness
		// -- see this function's doc comment) and skip straight to bodyEnd
		// on a duplicate, before OFFSET/LIMIT are ever consulted.
		distinctJump := -1
		if stmt.Distinct {
			for i, oc := range outCols {
				reg, cerr := c.compileExpr(oc.expr)
				if cerr != nil {
					return cerr
				}
				c.emit(Instruction{Op: OpSCopy, P1: reg, P2: resultBase + i})
			}
			c.emit(Instruction{Op: OpMakeRecord, P1: resultBase, P2: len(outCols), P3: distinctRecReg})
			distinctJump = c.emit(Instruction{Op: OpDistinct, P1: distinctNum, P3: distinctRecReg})
		}

		// OFFSET: while offsetReg is still positive, this (WHERE-matching,
		// and -- with DISTINCT -- newly distinct) combination is skipped --
		// decrement and jump straight past the rest of this body, same
		// target as a WHERE miss/duplicate.
		offGotoNext := -1
		if haveOffset {
			offHas := c.emit(Instruction{Op: OpIf, P1: offsetReg}) // offsetReg != 0 -> skip block
			offPast := c.emit(Instruction{Op: OpGoto})             // offsetReg == 0 -> past the skip, to LIMIT/output
			c.patch(offHas, c.here())
			c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: offsetReg, P3: offsetReg})
			offGotoNext = c.emit(Instruction{Op: OpGoto}) // patched below
			c.patch(offPast, c.here())
		}

		// LIMIT: once limitReg has reached 0, every remaining combination
		// (this one included) is beyond the requested count -- stop scanning
		// immediately (see limExhausted's doc comment above) rather than
		// continuing to loop and re-checking. This is what makes LIMIT 0
		// emit zero rows without ever evaluating a single select-list
		// expression (except, with DISTINCT, which must evaluate it anyway
		// to test distinctness -- see this function's doc comment).
		if haveLimit {
			limHas := c.emit(Instruction{Op: OpIf, P1: limitReg})                // limitReg != 0 -> continue to output
			limExhausted = append(limExhausted, c.emit(Instruction{Op: OpGoto})) // limitReg == 0 -> progEnd, patched below
			c.patch(limHas, c.here())
		}

		if !stmt.Distinct {
			for i, oc := range outCols {
				reg, cerr := c.compileExpr(oc.expr)
				if cerr != nil {
					return cerr
				}
				c.emit(Instruction{Op: OpSCopy, P1: reg, P2: resultBase + i})
			}
		}
		c.emit(Instruction{Op: OpResultRow, P1: resultBase, P2: len(outCols)})
		if haveLimit {
			c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: limitReg, P3: limitReg})
		}

		bodyEnd := c.here()
		if whereJump >= 0 {
			c.patch(whereJump, bodyEnd)
		}
		if distinctJump >= 0 {
			c.patch(distinctJump, bodyEnd)
		}
		if offGotoNext >= 0 {
			c.patch(offGotoNext, bodyEnd)
		}
		return nil
	}

	if err := emitJoinLoops(c, srcs, plan, body); err != nil {
		return nil, err
	}

	progEnd := c.here()
	for _, j := range limExhausted {
		c.patch(j, progEnd)
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
		NSubCache:  c.nSub,
		NDistinct:  c.nDistinct,
		NResultCol: len(outCols),
		ColNames:   cols,
		Correlated: c.correlated,
	}, nil
}

// detectRowidSeekKey looks for a WHERE conjunct pinning some source's rowid to
// a constant, enabling a point lookup instead of a scan. It returns the source
// index i and key expression E when:
//
//   - srcs[i] is an ordinary rowid base table (not derived, WITHOUT ROWID, or
//     an outer join target);
//   - a top-level AND conjunct is "="/"==" between srcs[i]'s rowid
//     (rowid/_rowid_/oid) or INTEGER PRIMARY KEY and a column-free E (a
//     literal or bound parameter), or a column of an ENCLOSING query
//     (isOuterSeekKey), which is as constant for one run of a correlated
//     subquery as a literal. Collation cannot matter: the seek fires only for
//     an integer key, and any coercion the comparison would apply leaves a
//     non-integer key and so a full scan.
//
// E needs no cursor, so it is evaluated once after srcs[i]'s OpOpenRead,
// before any Rewind, at any FROM or execution position. Outer join targets
// are excluded because pruning would happen before the level's
// match/NULL-extension bookkeeping (annotateJoinSeeks excludes them too).
//
// The conjunct stays in WHERE, so the seek only restricts candidates: the seek
// fires only for an integer K (OpSeekRowidHint), and "rowid = K" holds for
// exactly the row with that rowid, a superset of the answer. A non-integer key
// leaves a full scan.
func detectRowidSeekKey(c *compiler, srcs []joinSource, where Expr) (int, Expr, bool) {
	if where == nil {
		return 0, nil, false
	}
	conjuncts := splitTopLevelAnd(where)
	for i, s := range srcs {
		if s.derived != nil || s.tbl == nil || s.tbl.withoutRowid || s.left || s.rightOuter {
			continue
		}
		for _, cj := range conjuncts {
			be, ok := cj.(BinaryExpr)
			if !ok || (be.Op != "=" && be.Op != "==") {
				continue
			}
			if isScopeRowidRef(c, s, be.L) && (isSeekKeyCandidate(be.R) || isOuterSeekKey(c, be.R)) {
				return i, be.R, true
			}
			if isScopeRowidRef(c, s, be.R) && (isSeekKeyCandidate(be.L) || isOuterSeekKey(c, be.L)) {
				return i, be.L, true
			}
		}
	}
	return 0, nil, false
}

// isScopeRowidRef reports whether e is a reference to s's rowid: either the
// rowid/_rowid_/oid pseudo-column, or s's INTEGER PRIMARY KEY column (whose
// stored value normalizeRow substitutes with the rowid). Resolution reuses the
// compiler's own resolveInScopes so the rowid-vs-real-column and IPK rules match
// the rest of the compile exactly (a real column literally named "rowid" that is
// NOT the IPK correctly does not qualify -- it resolves to that column, not the
// pseudo-rowid). A three-part schema-qualified reference is conservatively
// declined (resolveInScopes does not resolve a schema qualifier).
func isScopeRowidRef(c *compiler, s joinSource, e Expr) bool {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.UsingRepr || ce.Schema != "" {
		return false
	}
	cursor, colIdx, isRowid, found, _, hard := resolveInScopes(c.scopes, ce, c.pager)
	if hard != nil || !found || cursor != s.scope.cursor {
		return false
	}
	if isRowid {
		return true
	}
	return s.tbl.ipkIndex >= 0 && colIdx == s.tbl.ipkIndex
}

// isSeekKeyCandidate reports whether e is a column-free constant seek key: a
// literal or a bound parameter. Restricting to these two forms keeps the key
// trivially evaluable before the scan starts (no cursor row is positioned yet)
// and independent of the row being sought -- "rowid = othercol" (a per-row
// value, never a single seek key) is thereby excluded. The literal's/parameter's
// actual storage class is checked at runtime by OpSeekRowidHint: a non-integer
// value falls back to a full scan.
func isSeekKeyCandidate(e Expr) bool {
	switch e.(type) {
	case LiteralExpr, ParamExpr:
		return true
	}
	return false
}

// indexSeekPlan is detectIndexSeekKey's result: everything compileScanPlain/
// emitJoinLoops need to emit an OpSeekIndexHint for a secondary-index
// leading-column equality seek -- the index b-tree root page, the column-free
// constant/parameter key expression, the affinity the WHERE comparison applies
// to that key (so the runtime probe reproduces the comparison exactly), and the
// index's leading-column collation (verified equal to the WHERE comparison's
// collation, so the b-tree's ordering and the equality's collation agree).
type indexSeekPlan struct {
	root    uint32
	keyExpr Expr
	aff     affinity
	coll    string

	// leadingCol is the TABLE column the index leads on, which a b-tree seek does
	// not need (the index's own record carries it) and a SEGMENT seek does: there
	// is no index b-tree on our format, so the seek is answered from that column's
	// posting list. See segment_seek.go.
	leadingCol int
}

// detectIndexSeekKey looks for a WHERE conjunct "<indexed-col> = <const>" a
// secondary index can serve on some source, returning its index i and the seek
// plan when:
//
//   - srcs[i] is an ordinary rowid base table (see detectRowidSeekKey);
//   - a materialized secondary index on it has, as its leading column, a plain
//     column whose index collation equals the column's declared collation
//     (secondaryIndexSeekCandidates declines expression/partial indexes and
//     collation mismatches);
//   - a top-level AND conjunct is "="/"==" between that column and a
//     column-free key (isSeekKeyCandidate).
//
// As with detectRowidSeekKey the key is evaluated once after OpOpenRead, and
// the conjunct stays in WHERE, so the seek only restricts. The probe
// reproduces the equality exactly: the key is coerced with comparisonAffinity
// and compared under the index's leading collation, which equals
// resolveCompareCollation's. SeekIndexRowidsSegments returns rows in rowid
// order, matching a full scan.
func detectIndexSeekKey(c *compiler, srcs []joinSource, where Expr) (int, *indexSeekPlan, bool) {
	if c == nil || c.pager == nil || where == nil {
		return 0, nil, false
	}
	conjuncts := splitTopLevelAnd(where)
	for i, s := range srcs {
		if s.derived != nil || s.tbl == nil || s.tbl.withoutRowid || s.left || s.rightOuter {
			continue
		}
		if s.notIndexed {
			continue // NOT INDEXED takes every secondary index away (where.c:4070)
		}
		// An index's root is a PAGE NUMBER, meaningful only in the file it came
		// from. A cross-database FROM item's cursor is opened on the ATTACHED
		// reader, so its indexes must be looked up there too -- resolving them
		// on the compiling pager and then seeking in the attached file reads
		// whatever b-tree occupies that page number there. Over a main holding
		// "CREATE INDEX i2 ON t2(k,v)" and an attached unindexed t2 of the same
		// shape, "SELECT k, v FROM a.t2 WHERE k = 1" answered from main's index
		// root (TestR30CrossDatabaseSingleTableSeekReadsTheRightFile). This is
		// the single-table twin of the correlated join seek's defect, fixed the
		// same way in annotateJoinSeeks (vdbe_join_seek.go). A dbIdx naming no
		// attached reader declines to the full scan rather than guessing.
		sp := c.pager
		if s.dbIdx != 0 {
			if s.dbIdx-1 >= len(c.pager.attachedReaders) {
				continue
			}
			sp = c.pager.attachedReaders[s.dbIdx-1].pager
		}
		cands, err := sp.secondaryIndexSeekCandidates(s.scope.tableName, s.tbl.root, s.tbl.cols)
		if err != nil || len(cands) == 0 {
			continue
		}
		for _, cj := range conjuncts {
			be, ok := cj.(BinaryExpr)
			if !ok || (be.Op != "=" && be.Op != "==") {
				continue
			}
			if colIdx, ok := scopeColumnIndex(c, s, be.L); ok && (isSeekKeyCandidate(be.R) || isOuterSeekKey(c, be.R)) {
				if plan, ok := planForColumn(c, cands, colIdx, be.L, be.R); ok && outerSeekCollationAgrees(c, be, plan) {
					return i, plan, true
				}
			}
			if colIdx, ok := scopeColumnIndex(c, s, be.R); ok && (isSeekKeyCandidate(be.L) || isOuterSeekKey(c, be.L)) {
				if plan, ok := planForColumn(c, cands, colIdx, be.R, be.L); ok && outerSeekCollationAgrees(c, be, plan) {
					return i, plan, true
				}
			}
		}
	}
	return 0, nil, false
}

// isOuterSeekKey reports a seek key that is a column of an ENCLOSING query --
// "SELECT 1 FROM u WHERE u.y = t.b" inside a correlated subquery -- which is as
// constant for one run of this program as a literal: C codes it before the
// loop from the outer cursor and seeks the index with it (codeEqualityTerm,
// wherecode.c). The key is evaluated before the Rewind, when that outer row is
// the current one. A name this FROM clause resolves is a join, not a key.
func isOuterSeekKey(c *compiler, e Expr) bool {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.Schema != "" || ce.UsingRepr || ce.UsingPinned || ce.FallbackLiteral != nil {
		return false
	}
	if c.outer == nil && c.rowOuter == nil && len(c.regScopes) == 0 {
		return false
	}
	_, _, _, found, _, hard := resolveInScopes(c.scopes, ce, c.pager)
	return hard == nil && !found
}

// outerSeekCollationAgrees: a literal key contributes no collation, so the
// equality compares under the column's own, which secondaryIndexSeekCandidates
// matched to the index. A column key can bring its own: "t.b = u.y" compares
// under t.b's declared sequence (sqlite3BinaryCompareCollSeq takes the left
// operand's, expr.c:424), and a seek of u.y's BINARY index under a NOCASE
// comparison would miss rows. Such a seek is not taken.
func outerSeekCollationAgrees(c *compiler, be BinaryExpr, plan *indexSeekPlan) bool {
	if isSeekKeyCandidate(be.L) || isSeekKeyCandidate(be.R) {
		return true
	}
	return equalFoldName(effectiveCollation(resolveCompareCollation(c.affCtx(), be.L, be.R)), effectiveCollation(plan.coll))
}

// planForColumn builds an indexSeekPlan if some candidate index leads with
// table column colIdx. colExpr/keyExpr are the equality's column and key
// operands (in that logical role, regardless of textual order); the affinity is
// computed from them exactly as the comparison opcode would (comparisonAffinity),
// so the runtime
// probe coercion reproduces the comparison. The candidate's collation was
// already verified (in secondaryIndexSeekCandidates) to equal the column's
// declared collation, which is the collation resolveCompareCollation gives this
// equality (the key is a bare literal/parameter, contributing no collation of
// its own), so the b-tree ordering and the equality agree.
func planForColumn(c *compiler, cands []indexSeekCandidate, colIdx int, colExpr, keyExpr Expr) (*indexSeekPlan, bool) {
	for _, cd := range cands {
		if cd.leadingCol != colIdx {
			continue
		}
		return &indexSeekPlan{
			root:       cd.root,
			keyExpr:    keyExpr,
			aff:        comparisonAffinity(c.affCtx(), colExpr, keyExpr),
			coll:       cd.coll,
			leadingCol: cd.leadingCol,
		}, true
	}
	return nil, false
}

// scopeColumnIndex reports whether e is a bare reference to a REAL column of
// source s (not its rowid pseudo-column, not an expression), returning that
// column's index into s.tbl.cols. Resolution reuses the compiler's own
// resolveInScopes so the column-vs-rowid and qualifier rules match the rest of
// the compile exactly; a rowid reference (isRowid), an ambiguous/failed
// resolution, a USING-representative reference, a schema-qualified reference, or
// a column resolving to a DIFFERENT cursor is declined. A coalesce-fallback
// reference is likewise declined (single-table plain scans never produce one,
// but guarding keeps this from ever mis-seeking a RIGHT/FULL-join column).
func scopeColumnIndex(c *compiler, s joinSource, e Expr) (int, bool) {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.UsingRepr || ce.Schema != "" {
		return 0, false
	}
	cursor, colIdx, isRowid, found, fb, hard := resolveInScopes(c.scopes, ce, c.pager)
	if hard != nil || !found || isRowid || fb.has || cursor != s.scope.cursor {
		return 0, false
	}
	return colIdx, true
}

// literalIntInstr builds the instruction that loads the int64 constant v into
// reg, using the same small-vs-wide encoding compileLiteral uses for an
// INTEGER literal (OpInteger for anything fitting a signed 32-bit immediate,
// OpInt64 otherwise).
func literalIntInstr(v int64, reg int) Instruction {
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		return Instruction{Op: OpInteger, P1: int(v), P2: reg}
	}
	return Instruction{Op: OpInt64, P2: reg, P4: v}
}

// groupKeyNeedsGroupCoalesce reports whether e is an unqualified column whose
// representative scope (the first FROM item exposing the name) is a member of
// a materialized join group and takes its value from a RIGHT/FULL coalesce
// chain (tableScope.coalesceFallback). Unqualified, because a qualified
// reference reads its raw column in C too ("GROUP BY t5.id"). The
// representative rather than resolveColumn's index, because a coalesce
// fallback redirects that index out of range (columnRefNameParts). A group
// member, because "t1 LEFT JOIN (t4 RIGHT JOIN t5 USING(id)) USING(id) GROUP
// BY id" reads t1.id and is correct.
func groupKeyNeedsGroupCoalesce(scopes []tableScope, srcs []joinSource, e Expr) bool {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.Qualifier != "" || ce.Schema != "" {
		return false
	}
	lname := r33sFoldIdent(ce.Name)
	rep := -1
	for i := range scopes {
		if _, has := scopes[i].colIndex[lname]; !has {
			continue
		}
		if _, hidden := scopes[i].coalesced[lname]; hidden {
			continue // a later USING/NATURAL copy: never the representative
		}
		rep = i
		break
	}
	if rep < 0 {
		return false
	}
	for _, src := range srcs {
		if len(src.memberScopes) == 0 {
			continue
		}
		base := src.scope.offset
		if scopes[rep].offset < base || scopes[rep].offset >= base+len(src.tbl.cols) {
			continue // the representative is not in THIS group
		}
		// Asked of the MEMBER SCOPES, not of the []tableScope handed in: a
		// materialized group's member carries its fallback RESOLVED into
		// concrete {cursor,colIdx} owners (compileScope.resolvedFallback) and
		// tableScopesOf drops that field along with the cursor, so the raw
		// coalesceFallback this would otherwise test is empty there. Both are
		// checked, since a member reached some other way still has the raw one.
		for _, ms := range src.memberScopes {
			if len(ms.resolvedFallback[lname]) > 0 || len(ms.coalesceFallback[lname]) > 0 {
				return true
			}
		}
	}
	return false
}

// firstRowDependsOnOrder reports whether another arrival order of rows could
// put a different row first: some row is not byte-identical to the first.
// joinHasRightOuter: a RIGHT or FULL join runs in literal FROM order, which
// the ported planner does not plan and the join executor already reproduces.
func joinHasRightOuter(srcs []joinSource) bool {
	for i := range srcs {
		if srcs[i].rightOuter {
			return true
		}
	}
	return false
}

func firstRowDependsOnOrder(rows [][]Value) bool {
	for _, r := range rows[min(1, len(rows)):] {
		if len(r) != len(rows[0]) {
			return true
		}
		for i := range r {
			if !valuesIdentical(r[i], rows[0][i]) {
				return true
			}
		}
	}
	return false
}

// segPeepholesOffForTest compiles every scan as the plain VDBE loop, with no
// segment fast path recognised -- the reference a differential test compares
// the fast paths against, over the same rows. A session's plan cache
// keeps what it compiled, so a test sets this before opening the session it
// wants the reference from.
var segPeepholesOffForTest bool
