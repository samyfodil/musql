// This file implements the OUTWARD half of C SQLite's aggregate-association
// rule: an aggregate written inside a subquery whose argument names only an
// ENCLOSING query's columns is that ENCLOSING query's aggregate, not the
// subquery's.
//
// The rule is SQLite's own, read out of sqlite3-binding.c (resolve.c's
// sqlite3ResolveExprNames, TK_AGG_FUNCTION arm), which walks OUT through the
// NameContext chain --
//
//	pNC2 = pNC;
//	while( pNC2 && sqlite3ReferencesSrcList(pParse, pExpr, pNC2->pSrcList)==0 ){
//	  pExpr->op2 += (1 + pNC2->nNestedSelect); pNC2 = pNC2->pNext; }
//	if( pNC2 && pDef ) pNC2->ncFlags |= NC_HasAgg | ...;
//
// -- with sqlite3ReferencesSrcList tri-state: 1 (a table of THIS context's own
// FROM is named -> the aggregate stays), 0 (only tables of some OTHER context
// -> it moves one level out) and -1 (no table at all, or only tables of the
// argument's own subqueries -> it stays). checkAggregateAssociation (sql_agg.go)
// already computes the tri-state verdict at the level the aggregate is WRITTEN;
// this file answers the question that verdict leaves open -- WHICH enclosing
// query owns it -- and then delivers the value.
//
// Why the ownership question cannot be answered by the enclosing compiler on
// its own: deciding that "count(a1)" in "SELECT (SELECT count(a1) FROM t2) FROM
// t1" is t1's requires knowing that t2 has no a1, and t2 may be a view, a CTE or
// a derived table whose columns only exist once resolveJoinSources has run
// INSIDE that subquery's own compile. So the verdict is computed where it is
// exact -- in the inner compile -- and travels outward:
//
//  1. the inner planner reports the offending accumulators
//     (aggAssociationError, sql_agg.go);
//  2. hoistOwnerFor walks the enclosing COMPILER chain applying exactly
//     sqlite3ReferencesSrcList's own test (aggArgHasLocalColumnRef) at each
//     level, so the owner is the CLOSEST enclosing query whose FROM supplies at
//     least one referenced column;
//  3. the spec is recorded on that compiler's hoistBox and the compile fails;
//  4. compileSelectScanRow (vdbe_scan.go) sees its own box filled and recompiles
//     the statement AS AN AGGREGATE QUERY, with planHoistedAggs adding one
//     accumulator per hoisted call;
//  5. at finalize time aggResult (vdbe_agg.go) publishes each accumulator's
//     value on the evalCtx, and rewriteSelectOuterRefs substitutes
//     it into the subquery body as a literal -- which is exact, because
//     aggResult finalizes EVERY accumulator before it evaluates the item, so the
//     hoisted value is already a constant by the time the body runs.
//
// Step 4 is where the ROW COUNT changes, and that is the whole risk of this
// rule: "SELECT (SELECT count(a1) FROM t2) FROM t1" is ONE row in C SQLite
// (verified over t1(a1)=(1,2,3), t2(b1)=(4,5): the single row 3), not three.
// Getting the direction wrong in either direction is a silent wrong answer, so
// the hoist only ever happens off the inner compile's own exact verdict --
// never off a guess made from the outside -- and every level of the walk that
// cannot be trusted to have its final scopes installed (an ON-clause compile,
// which narrows c.scopes for the duration; a trigger/register-backed row scope,
// where compileColumn resolves names ahead of the cursor scopes) refuses
// outright, leaving the pre-existing decline in place.
package engine

import (
	"errors"
	"fmt"
)

// asAggAssociationError extracts checkAggregateAssociation's verdict from a
// planner error, however it was wrapped on the way out.
func asAggAssociationError(err error) (*aggAssociationError, bool) {
	var ae *aggAssociationError
	return ae, errors.As(err, &ae)
}

// hoistSpec is one aggregate call that belongs to the query holding the
// hoistBox this spec was recorded on, but is WRITTEN inside body -- a subquery
// (or derived-table) SELECT reachable from that query's select list.
type hoistSpec struct {
	call FuncExpr
	body *SelectStmt

	// aliasName is set only for a hoist recorded from a BARE reference to an
	// OUTER select-list alias (materializedOuterRefInAggArgArm's ColumnExpr
	// branch, sql_group.go) rather than from a literally-spelled aggregate
	// call written at body's own level -- "" for the ordinary case. call is
	// then still the OUTER level's own (already-resolved) alias expression,
	// exactly the COPY resolve.c's resolveAlias splices in (resolve.c:69-101):
	// there is no call written at body's position to match sameAggCall
	// against, so the runtime substitution (rewriteExprOuterRefs)
	// keys off this name instead.
	aliasName string
}

// hoistBox is one scan compile attempt's collection point. compileSelectScanRow
// creates one per attempt and hands it to the compiler it builds; an inner
// compile that finds this compiler owns an aggregate appends the spec here.
// A non-empty box after a FAILED attempt is the signal to recompile.
type hoistBox struct {
	specs []hoistSpec
	// cols is the owning statement's select list, as the retry compile will see
	// it. A hoist is only recorded for a body reachable from one of these items:
	// that is the only position planHoistedAggs can attach an accumulator to, and
	// the only position aggResult later evaluates. A body anywhere else (a WHERE
	// clause's EXISTS, say) is left to decline with its original message rather
	// than provoke a retry that can only fail.
	cols []SelectColumn
	// narrowed is set while this compiler's scopes are temporarily restricted
	// to a JOIN ON clause's "already bound" prefix (emitJoinLevel's compileOn,
	// vdbe_join_codegen.go). The owner walk must not run against a narrowed
	// scope set: a column the full FROM does supply would look absent, and the
	// aggregate would be handed to a query further out -- a wrong row count.
	narrowed bool
}

// recordHoist appends spec unless an equal one is already present. The same
// aggregate can be reported twice when a body is compiled more than once during
// one attempt (a correlated subquery under a join level, say); planning it twice
// would add a duplicate accumulator, which is harmless but pointless.
func (b *hoistBox) recordHoist(spec hoistSpec) bool {
	reachable := false
	for _, c := range b.cols {
		if !c.Star && hoistBodyInExpr(c.Expr, spec.body) {
			reachable = true
			break
		}
	}
	if !reachable {
		return false
	}
	for _, s := range b.specs {
		if s.body == spec.body && sameAggCall(s.call, spec.call) {
			return true
		}
	}
	b.specs = append(b.specs, spec)
	return true
}

// hoistAggregatesOutward is the single entry point every aggregate compile uses
// when its planner reports an aggAssociationError. body is the SelectStmt being
// compiled (the level the aggregates are WRITTEN in) and outer its enclosing
// compiler chain.
//
// It returns true once it has recorded at least one spec, meaning the caller
// must fail (any error will do -- compileSelectScanRow keys the retry off the
// BOX, not off the error, so no sentinel has to survive being wrapped on its way
// out). It returns false when the aggregates cannot be placed, in which case the
// caller declines exactly as it always did.
//
// All-or-nothing per call: if ANY reported aggregate has no owner in the chain,
// nothing is recorded. A partially-hoisted statement would leave the remainder
// to be re-discovered at run time, and the honest decline is better than an
// error raised halfway through a scan.
func hoistAggregatesOutward(outer *compiler, body *SelectStmt, ae *aggAssociationError) bool {
	if outer == nil || body == nil || ae == nil || len(ae.movers) == 0 {
		return false
	}
	// A compound's FIRST arm is compiled from a private copy (compileCompound,
	// vdbe_compound_codegen.go), which no enclosing select list can contain --
	// so the spec has to name the COMPOUND, whose own Columns/Where/Having ARE
	// the first arm's. See SelectStmt.r35dCompoundOf. Verified against real C
	// SQLite over t1(a)=(1,2,3): "SELECT (SELECT avg(a) UNION SELECT min(a)
	// OVER()) FROM t1" (colname.test) is the ONE row 1 there -- avg(a)
	// re-associated to t1 makes the outer query an aggregate -- and was "no such
	// column: a" here purely because the spec had nowhere to attach.
	if body.r35dCompoundOf != nil {
		body = body.r35dCompoundOf
	}
	// depth of the outermost owner found so far, and the owner itself. SQLite
	// re-associates EACH aggregate with its OWN closest owner, so two aggregates
	// in one body can belong to two different enclosing queries ("SELECT (SELECT
	// (SELECT max(c7)+max(c8)+max(c9) FROM t9) FROM t8) FROM t7" has one at each
	// of the three levels). Only one compile can be restarted here, so the
	// OUTERMOST owner is chosen: restarting it re-runs every compile inside it,
	// and each inner owner is then rediscovered by exactly this same path.
	var (
		target   *compiler
		depth    = -1
		perOwner = make([]*compiler, len(ae.movers))
	)
	for i, m := range ae.movers {
		oc, d := hoistOwnerFor(outer, m)
		if oc == nil {
			return false
		}
		perOwner[i] = oc
		if d > depth {
			target, depth = oc, d
		}
	}
	if target.hoist == nil {
		return false
	}
	recorded := false
	for i, m := range ae.movers {
		if perOwner[i] != target {
			continue
		}
		if target.hoist.recordHoist(hoistSpec{call: m.srcCall, body: body}) {
			recorded = true
		}
	}
	return recorded
}

// hoistOwnerFor finds the compiler SQLite would associate agg with: walking out
// from outer, the first level whose OWN FROM clause supplies at least one column
// the argument list references (sqlite3ReferencesSrcList == 1). It returns that
// compiler and its distance from outer, or (nil, -1) when there is none, or when
// some level on the way is not in a state whose scopes can be trusted -- see
// hoistBox.narrowed, and the trig/regScopes refusal below.
func hoistOwnerFor(outer *compiler, agg *aggItem) (*compiler, int) {
	exprs := aggArgExprs(agg)
	if len(exprs) == 0 || agg.srcCall.Name == "" {
		// count(*) names nothing, so it is SQLite's -1 and never moves; an
		// accumulator with no recorded call cannot be re-planned elsewhere.
		return nil, -1
	}
	for oc, d := outer, 0; oc != nil; oc, d = oc.outer, d+1 {
		// The aggregate ITEM compiler is the one register-scoped level this
		// walk passes THROUGH rather than refusing at (compileAggItemProgram,
		// vdbe_agg_item_codegen.go). Its register scopes are the aggregate
		// query's own FROM items published over the anchor-row block, and this
		// walk cannot own an aggregate there: every call in a body that belongs
		// to that query has already been either substituted for its finalized
		// value or DECLINED before the body is compiled (aggItemSubst.bodyExpr's
		// isAggregateCall arm, vdbe_agg_item_subst.go), so what reaches here
		// names nothing of it. Skipping the level is exactly what this chain did
		// before those scopes were published, which is why publishing them may
		// not change a single hoist decision.
		if oc.trig != nil || (len(oc.regScopes) > 0 && oc.aggRegs == nil) {
			return nil, -1
		}
		if oc.hoist != nil && (oc.hoist.narrowed || len(oc.hoist.cols) == 0) {
			// A SCAN compile whose scopes are either temporarily narrowed to an
			// ON clause's visible prefix, or not installed yet at all -- the
			// latter is any subquery compiled from INSIDE resolveJoinSources
			// (a view or CTE body), where c.scopes is still nil even though this
			// query's FROM will supply plenty of columns. Both would make a
			// column that IS in scope look absent and hand the aggregate to a
			// query further out, which is a wrong row count; refuse instead.
			// hoistBox.cols is set at exactly the point c.scopes becomes final,
			// so it doubles as that readiness flag.
			return nil, -1
		}
		scopes := compileTableScopes(oc.scopes)
		if len(scopes) == 0 {
			// A FROM-less level, or the stand-in barrier compiler a derived
			// table is compiled beneath (derivedOuterBarrier,
			// vdbe_join_codegen.go): it names no table, so
			// sqlite3ReferencesSrcList can only answer 0 for it -- keep walking.
			continue
		}
		for _, e := range exprs {
			if aggArgHasLocalColumnRef(e, scopes) {
				return oc, d
			}
		}
	}
	return nil, -1
}

// compileTableScopes projects a compile's cursor scopes onto the []tableScope
// shape the shared planners take (tableScopesOf's projection, vdbe_join_codegen.go,
// applied to an already-built scope list rather than to joinSources).
func compileTableScopes(cs []compileScope) []tableScope {
	if len(cs) == 0 {
		return nil
	}
	ts := make([]tableScope, len(cs))
	for i, s := range cs {
		ts[i] = s.tableScope
	}
	return ts
}

// sameAggCall reports whether two aggregate calls are the same call. exprEqual
// alone is not enough: it compares name/DISTINCT/*/arguments but NOT the FILTER
// clause or the OVER clause, and "count(a) FILTER (WHERE x)" must never be
// substituted with plain "count(a)"'s value (filter1.test writes both).
func sameAggCall(a, b FuncExpr) bool {
	if !exprEqual(a, b) {
		return false
	}
	if (a.Filter == nil) != (b.Filter == nil) {
		return false
	}
	if a.Filter != nil && !exprEqual(a.Filter, b.Filter) {
		return false
	}
	return (a.Over == nil) == (b.Over == nil)
}

// planHoistedAggs adds one accumulator per hoisted call to the select-list item
// whose subquery the call is written in, and records where the finalized value
// must reappear (itemPlan.hoisted -> aggResult -> rewriteSelectOuterRefs).
//
// The item is found by hoistBodyInExpr, which walks EXACTLY the subquery
// positions rewriteSelectOuterRefs' own descent reaches, so a body
// this can plan is a body the runtime substitution can reach. A body it cannot
// find -- one inside a WITH-clause CTE, which that rewrite deliberately leaves
// alone -- declines here rather than compiling into an aggregate whose value
// would never be delivered.
func planHoistedAggs(items []*itemPlan, specs []hoistSpec) ([]*aggItem, error) {
	var added []*aggItem
	for _, sp := range specs {
		idx := -1
		for i, it := range items {
			if it != nil && hoistBodyInExpr(it.rewritten, sp.body) {
				idx = i
				break
			}
		}
		if idx >= 0 && itemAlreadyHoists(items[idx], sp) {
			// planItemHoists (sql_group.go) already gave this item its own
			// accumulator for the same call: the two collectors overlap
			// wherever the RETRY compile's select list reaches the same body
			// this compile's inner planner reported. A second accumulator
			// would compute the identical value and only the first would ever
			// be substituted, so skip it rather than pay for it.
			continue
		}
		if idx < 0 {
			return nil, fmt.Errorf("%w: hoisted aggregate's subquery is not reachable from this query's select list", errVDBEUnsupported)
		}
		tmpl, err := planAggregateCall(sp.call)
		if err != nil {
			return nil, declineOrSemantic(err)
		}
		if err := checkExprSupported(tmpl.expr); err != nil {
			return nil, declineOrSemantic(err)
		}
		if tmpl.sepExpr != nil {
			if err := checkExprSupported(tmpl.sepExpr); err != nil {
				return nil, declineOrSemantic(err)
			}
		}
		it := items[idx]
		it.hoisted = append(it.hoisted, hoistedAggRef{body: sp.body, call: sp.call, accIdx: len(it.aggTemplates)})
		it.aggTemplates = append(it.aggTemplates, tmpl)
		added = append(added, tmpl)
	}
	return added, nil
}

// itemAlreadyHoists reports whether it already carries an accumulator for sp's
// exact (body, call) pair -- see planHoistedAggs' use of it.
func itemAlreadyHoists(it *itemPlan, sp hoistSpec) bool {
	for _, h := range it.hoisted {
		if h.body == sp.body && sameAggCall(h.call, sp.call) {
			return true
		}
	}
	return false
}

// honoredWithHoisted reconciles the enclosing query's MIN/MAX CENSUS -- the one
// that decides which row every bare column and every correlated subquery in the
// same item reads from (magnetPlan/anchorRow) -- with the calls hoisted INTO
// that query. A hoisted min()/max() counts as one of the query's own
// aggregates, and minMaxCensus cannot see it: it walks the select list without
// descending into subqueries, which is exactly where a hoisted call lives.
//
// Getting this wrong is a WRONG ANSWER, not a decline. Verified directly against
// C SQLite over t1(a1,b)=(1,10),(2,20),(3,30) and t2(b1,x)=(4,1),(5,0):
//
//	SELECT b, (SELECT max(a1) FROM t2) FROM t1  ->  30, 3   (max's row)
//	SELECT b, (SELECT min(a1) FROM t2) FROM t1  ->  10, 1   (min's row)
//	SELECT b, (SELECT count(a1) FROM t2) FROM t1 -> 10, 3   (the FIRST row)
//	SELECT a1,(SELECT max(a1) FROM t2) FROM t1  ->   3, 3   (max's row)
//
// -- so with the hoisted max() ignored, "SELECT b, (SELECT max(a1) FROM t2) FROM
// t1" answered 10 where SQLite answers 30.
//
// WHERE a hoisted call sits in the census ORDER (minMaxCensus) is not something
// this plumbing knows -- and the census order is exactly what decides the anchor
// once a query has more than one site -- so anything beyond a single distinct
// min/max call across the whole statement declines rather than guesses.
func honoredWithHoisted(stmt *SelectStmt, specs []hoistSpec, existing *magnetPlan) (*magnetPlan, error) {
	var hoisted []FuncExpr
	for _, sp := range specs {
		collectMinMaxCalls(sp.call, &hoisted)
	}
	if len(hoisted) == 0 {
		return existing, nil
	}
	all := dedupMinMaxCalls(append(minMaxCensus(stmt), hoisted...))
	if len(all) != 1 {
		return nil, fmt.Errorf("%w: hoisted min/max combined with another min/max call", errVDBEUnsupported)
	}
	// A hoist only ever happens for a whole-table aggregate (a GROUP BY query's
	// own hoist path declines earlier), so hasGroupBy is false here.
	return magnetPlanForCalls(all, false), nil
}

// hoistedAggRef records, for one planned item, that body's `call` is THIS
// query's aggregate and its finalized value is accs[accIdx] -- accIdx indexing
// the item's own aggTemplates, exactly as a groupAggExpr placeholder's accIdx
// does (sql_group.go).
type hoistedAggRef struct {
	body   *SelectStmt
	call   FuncExpr
	accIdx int

	// aliasName mirrors hoistSpec.aliasName -- see its doc comment. Threaded
	// through so aggResult (vdbe_agg.go) can publish it onto the
	// hoistedAggVal it builds for this ref.
	aliasName string
}

// hoistBodyInExpr reports whether body is one of the subquery bodies reachable
// from e. It is the plan-time mirror of rewriteSelectOuterRefs/
// rewriteExprOuterRefs' descent and must stay in step with it: the
// positions listed here are exactly the ones the runtime substitution visits.
func hoistBodyInExpr(e Expr, body *SelectStmt) bool {
	switch x := e.(type) {
	case nil:
		return false
	case SubqueryExpr:
		return hoistBodyInSelect(x.Stmt, body)
	case ExistsExpr:
		return hoistBodyInSelect(x.Stmt, body)
	case InExpr:
		if hoistBodyInExpr(x.X, body) || hoistBodyInSelect(x.Sub, body) {
			return true
		}
		for _, it := range x.List {
			if hoistBodyInExpr(it, body) {
				return true
			}
		}
		return false
	case FuncExpr:
		for _, a := range x.Args {
			if hoistBodyInExpr(a, body) {
				return true
			}
		}
		for _, ob := range x.orderByExprs() {
			if hoistBodyInExpr(ob, body) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return hoistBodyInExpr(x.X, body)
	case BinaryExpr:
		return hoistBodyInExpr(x.L, body) || hoistBodyInExpr(x.R, body)
	case IsNullExpr:
		return hoistBodyInExpr(x.X, body)
	case BetweenExpr:
		return hoistBodyInExpr(x.X, body) || hoistBodyInExpr(x.Lo, body) || hoistBodyInExpr(x.Hi, body)
	case LikeExpr:
		return hoistBodyInExpr(x.X, body) || hoistBodyInExpr(x.Pattern, body) || hoistBodyInExpr(x.Escape, body)
	case GlobExpr:
		return hoistBodyInExpr(x.X, body) || hoistBodyInExpr(x.Pattern, body)
	case CollateExpr:
		return hoistBodyInExpr(x.X, body)
	case CastExpr:
		return hoistBodyInExpr(x.X, body)
	case CaseExpr:
		if hoistBodyInExpr(x.Base, body) || hoistBodyInExpr(x.Else, body) {
			return true
		}
		for _, w := range x.Whens {
			if hoistBodyInExpr(w.When, body) || hoistBodyInExpr(w.Then, body) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// hoistBodyInSelect is hoistBodyInExpr's statement half: stmt itself, then every
// expression and FROM-item/compound-arm subquery rewriteSelectOuterRefs descends
// into. The WITH-clause CTE bodies that rewrite leaves untouched are left
// untouched here too, so a hoist into one is reported as not-found and declines.
func hoistBodyInSelect(stmt, body *SelectStmt) bool {
	if stmt == nil {
		return false
	}
	if stmt == body {
		return true
	}
	if hoistBodyInExpr(stmt.Where, body) || hoistBodyInExpr(stmt.Having, body) {
		return true
	}
	for _, c := range stmt.Columns {
		if hoistBodyInExpr(c.Expr, body) {
			return true
		}
	}
	for _, g := range stmt.GroupBy {
		if hoistBodyInExpr(g, body) {
			return true
		}
	}
	for _, o := range stmt.OrderBy {
		if hoistBodyInExpr(o.Expr, body) {
			return true
		}
	}
	for _, it := range stmt.From {
		if hoistBodyInExpr(it.On, body) || hoistBodyInSelect(it.Subquery, body) {
			return true
		}
	}
	for _, a := range stmt.Compound {
		if hoistBodyInSelect(a.Stmt, body) {
			return true
		}
	}
	return false
}

// hoistedAggVal is one hoisted aggregate's FINALIZED value, published on the
// evalCtx an aggregate query's item is evaluated against (aggResult, vdbe_agg.go)
// and consumed by rewriteSelectOuterRefs when it reaches the exact
// body the call is written in.
type hoistedAggVal struct {
	body *SelectStmt
	call FuncExpr
	val  Value

	// aliasName mirrors hoistedAggRef.aliasName -- see hoistSpec's doc
	// comment. rewriteExprOuterRefs matches on this instead of
	// sameAggCall(fc, h.call) when it is set, since body never had a call
	// written at the substituted position at all.
	aliasName string
}

// hoistSubsFor returns the substitutions that apply to stmt itself -- matched by
// AST POINTER, so an identical-looking call at any other level is never touched.
func hoistSubsFor(outer *evalCtx, stmt *SelectStmt) []hoistedAggVal {
	if outer == nil || stmt == nil || len(outer.hoistedAggs) == 0 {
		return nil
	}
	var subs []hoistedAggVal
	for _, h := range outer.hoistedAggs {
		if h.body == stmt {
			subs = append(subs, h)
		}
	}
	return subs
}
