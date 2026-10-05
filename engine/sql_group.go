// GROUP BY / HAVING planning: the aggregate planner in sql_agg.go extended to one
// output row per group. planGroupByStmt is the planner compileScanGroupBy
// (vdbe_agg_codegen.go) builds on.
//
//	SELECT <select-list> FROM <table> [WHERE ...]
//	  GROUP BY <expr> {, <expr>} [HAVING <expr>] [ORDER BY ...] [LIMIT/OFFSET]
//
// Each select-list item, HAVING and non-ordinal ORDER BY term is rewritten once
// (planGroupItem / rewriteGroupExpr): every aggregate call and every
// subexpression structurally equal to a GROUP BY expression is replaced by a
// placeholder (groupAggExpr / groupKeyExpr) without descending into it, so
// "count(*) > 1" and "category || '!'" (with GROUP BY category) work. Any other
// bare column reads the group's anchor row (groupBareColExpr). Placeholders are
// resolved once per group at finalize time; the rest of the tree compiles
// normally.
//
// Two rows share a group iff every GROUP BY expression compares equal with
// compareValues, so NULLs group together (unlike "=" in WHERE). A positive
// integer literal GROUP BY term is an output-column ordinal, rejected if that
// column contains an aggregate. No input rows means no output rows, unlike a
// whole-table aggregate.
package engine

import (
	"fmt"
	"strings"
)

// The placeholder nodes below carry an owner: which aggregate query's register
// block their index belongs to, C's pExpr->pAggInfo (expr.c:4996, 5342). C ties
// each aggregate to a specific AggInfo by climbing the NameContext chain
// (resolve.c:1355; expr.c:7212), so nested aggregate queries each have their
// own. nil means the query whose item this is; a non-nil owner appears only on a
// placeholder aggItemSubst put into a subquery body, and aggResultReg's chain
// walk skips every other block.

// groupKeyExpr is a placeholder for a subexpression equal to a GROUP BY
// expression: the group's key value at idx. aff, coll and collExplicit preserve
// that expression's affinity and collation (and whether the collation was
// explicit, for resolveCompareCollation's ranking), so comparisons in the item
// behave as on the original (tkt3493.test):
//
//	SELECT a='abc' FROM t2 GROUP BY a     -> 1   (BINARY would say 0)
//	SELECT a>b    FROM t2 GROUP BY a, b   -> 0   (BINARY would say 1)
type groupKeyExpr struct {
	idx          int
	aff          affinity
	coll         string
	collExplicit bool

	// Which aggregate query's block idx counts against; see the owner note
	// above this type. nil means "the query this expression is an item of".
	owner *aggPlan
}

func (groupKeyExpr) exprNode() {}

// groupAggExpr is a placeholder for an aggregate call: the group's accIdx'th
// accumulator's finalized value. coll preserves an explicit COLLATE inside the
// call's arguments, which sqlite3ExprCollSeq propagates out (expr.c:248):
// "SELECT max(c1 COLLATE nocase) IN (SELECT 'aBCd') FROM t1" is 1
// (window1.test). A declared column collation inside does not propagate.
type groupAggExpr struct {
	accIdx int
	coll   string

	// Which aggregate query's block accIdx counts against; see the owner
	// note above groupKeyExpr.
	owner *aggPlan
}

func (groupAggExpr) exprNode() {}

// groupBareColExpr is a placeholder for a bare column that is neither a GROUP BY
// key nor in an aggregate argument (SQLite's bare-column extension, see
// magnetPlan): read from the group's anchor row, the last row the min()/max()
// census left the magnet clear on (magnetWalk). idx is the absolute vals index,
// or isRowid/rowidTableIdx for a rowid pseudo-column. aff and coll preserve the
// column's affinity and declared collation for comparisons in the same item.
type groupBareColExpr struct {
	idx           int
	isRowid       bool
	rowidTableIdx int
	aff           affinity
	coll          string

	// noAff carries columnInfo.NoAffinity for the column this stands in for --
	// a COMPUTED derived-table or view column, whose SQLite affinity is
	// AFF_NONE rather than the AFF_BLOB of a typeless real column. It is what
	// makes isMaterializedRef answer false for this node, so
	// sqlite3CompareAffinity's "one side is not a column" arm (expr.c:356)
	// fires and a TEXT comparand coerces the other side. Without it a bare
	// reference to such a column defended its storage class and ranked
	// differently -- the reason this whole shape used to DECLINE.
	noAff bool

	// Which aggregate query's anchor row idx/rowidTableIdx count against;
	// see the owner note above groupKeyExpr.
	owner *aggPlan
}

func (groupBareColExpr) exprNode() {}

// exprEqual reports whether a and b are structurally identical: same shape and
// operators, case-insensitive identifiers, equal literals. No semantic
// equivalence ("1+1" is not "2"). Used to detect a subexpression that is a GROUP
// BY expression verbatim.
func exprEqual(a, b Expr) bool {
	switch x := a.(type) {
	case nil:
		// Only reachable via LikeExpr.Escape, which is nil when no ESCAPE
		// clause was written; two such LikeExprs are equal in that field
		// exactly when both omit it.
		return b == nil
	case LiteralExpr:
		y, ok := b.(LiteralExpr)
		return ok && valueExactEqual(x.Val, y.Val)
	case ParamExpr:
		y, ok := b.(ParamExpr)
		return ok && x.Index == y.Index
	case ColumnExpr:
		y, ok := b.(ColumnExpr)
		return ok && equalFoldName(x.Qualifier, y.Qualifier) && equalFoldName(x.Name, y.Name)
	case UnaryExpr:
		y, ok := b.(UnaryExpr)
		return ok && x.Op == y.Op && exprEqual(x.X, y.X)
	case BinaryExpr:
		y, ok := b.(BinaryExpr)
		return ok && x.Op == y.Op && exprEqual(x.L, y.L) && exprEqual(x.R, y.R)
	case IsNullExpr:
		y, ok := b.(IsNullExpr)
		return ok && x.Not == y.Not && exprEqual(x.X, y.X)
	case InExpr:
		y, ok := b.(InExpr)
		if !ok || x.Not != y.Not || len(x.List) != len(y.List) || !exprEqual(x.X, y.X) {
			return false
		}
		for i := range x.List {
			if !exprEqual(x.List[i], y.List[i]) {
				return false
			}
		}
		return true
	case BetweenExpr:
		y, ok := b.(BetweenExpr)
		return ok && x.Not == y.Not && exprEqual(x.X, y.X) && exprEqual(x.Lo, y.Lo) && exprEqual(x.Hi, y.Hi)
	case LikeExpr:
		y, ok := b.(LikeExpr)
		return ok && x.Not == y.Not && exprEqual(x.X, y.X) && exprEqual(x.Pattern, y.Pattern) && exprEqual(x.Escape, y.Escape)
	case GlobExpr:
		y, ok := b.(GlobExpr)
		return ok && x.Not == y.Not && exprEqual(x.X, y.X) && exprEqual(x.Pattern, y.Pattern)
	case CollateExpr:
		y, ok := b.(CollateExpr)
		return ok && equalFoldName(x.Name, y.Name) && exprEqual(x.X, y.X)
	case FuncExpr:
		y, ok := b.(FuncExpr)
		if !ok || !equalFoldName(x.Name, y.Name) || x.Star != y.Star || x.Distinct != y.Distinct || len(x.Args) != len(y.Args) {
			return false
		}
		for i := range x.Args {
			if !exprEqual(x.Args[i], y.Args[i]) {
				return false
			}
		}
		// An aggregate's own ORDER BY is part of what it computes:
		// "group_concat(x ORDER BY y)" is not "group_concat(x)", and
		// sqlite3ExprCompare says so too (it compares pLeft).
		if len(x.OrderBy) != len(y.OrderBy) {
			return false
		}
		for i := range x.OrderBy {
			if x.OrderBy[i].Desc != y.OrderBy[i].Desc || x.OrderBy[i].Nulls != y.OrderBy[i].Nulls ||
				!exprEqual(x.OrderBy[i].Expr, y.OrderBy[i].Expr) {
				return false
			}
		}
		return true
	case CastExpr:
		y, ok := b.(CastExpr)
		return ok && x.Type == y.Type && exprEqual(x.X, y.X)
	case CaseExpr:
		// sqlite3ExprCompare compares a TK_CASE's operand and WHEN/THEN/ELSE
		// list like any other node's (expr.c:6584, 6632-6635). An isBool node is C's
		// TK_TRUTH, a different op from a written CASE of the same shape.
		y, ok := b.(CaseExpr)
		if !ok || x.isBool != y.isBool || x.isBoolFalse != y.isBoolFalse || len(x.Whens) != len(y.Whens) ||
			(x.Base == nil) != (y.Base == nil) || (x.Else == nil) != (y.Else == nil) {
			return false
		}
		if x.Base != nil && !exprEqual(x.Base, y.Base) || x.Else != nil && !exprEqual(x.Else, y.Else) {
			return false
		}
		for i := range x.Whens {
			if !exprEqual(x.Whens[i].When, y.Whens[i].When) || !exprEqual(x.Whens[i].Then, y.Whens[i].Then) {
				return false
			}
		}
		return true
	case SubqueryExpr:
		// Identity, not structure: C's sqlite3ExprCompare refuses subqueries outright
		// (expr.c), so only the same parsed body counts. sameAggCall needs this to
		// match a recorded hoist spec back to its call (aggItemSubst).
		y, ok := b.(SubqueryExpr)
		return ok && x.Stmt != nil && x.Stmt == y.Stmt
	case ExistsExpr:
		y, ok := b.(ExistsExpr)
		return ok && x.Not == y.Not && x.Stmt != nil && x.Stmt == y.Stmt
	default:
		return false
	}
}

// valueExactEqual reports whether a and b are the exact same storage class
// and bit-identical content -- used by exprEqual for LiteralExpr, where
// "same literal" should mean exactly that, not SQLite's looser cross-type
// comparison rules (compareValues).
func valueExactEqual(a, b Value) bool {
	if a.Typ != b.Typ {
		return false
	}
	switch a.Typ {
	case Null:
		return true
	case Int:
		return a.I == b.I
	case Float:
		return a.F == b.F
	default: // Text, Blob
		return string(a.S) == string(b.S)
	}
}

// keysEqual reports whether two group-key tuples are equal per GROUP BY's
// grouping semantics: elementwise compareValues equality (which, unlike "=",
// treats two NULLs as equal -- see this file's package doc comment).
func keysEqual(a, b []Value) bool {
	for i := range a {
		if compareValues(a[i], b[i]) != 0 {
			return false
		}
	}
	return true
}

// itemPlan is one select-list item's, HAVING's, or non-ordinal ORDER BY term's
// plan for a GROUP BY query: rewritten has aggregate calls and GROUP BY matches
// replaced by placeholders, and aggTemplates holds the item's accumulators in
// accIdx order. usesGroupBare is true when the rewrite produced a
// groupBareColExpr, so the item reads the group's anchor row.
type itemPlan struct {
	rewritten     Expr
	aggTemplates  []*aggItem
	usesGroupBare bool

	// bareTables is WHICH FROM items the bare columns counted by usesGroupBare
	// were read from: one bit per scope position. bareWide instead says one of
	// them resolved to a position this plan cannot name (a scope past 63, or an
	// absolute value index no scope covers), so every item must be assumed read.
	// anchorIdentityFixed (vdbe_agg_codegen.go) is the only reader: a bare
	// column of a FROM item the GROUP BY key pins to ONE ROW per group holds the
	// same value whichever row of the group turns out to be the anchor, so for
	// that item the arrival order does not have to be provable at all.
	bareTables uint64
	bareWide   bool

	// hoisted names the aggregates this item's own SUBQUERY BODIES contain that
	// belong to THIS query rather than to the subquery -- C SQLite's outward
	// aggregate association. Each carries an accIdx into aggTemplates (and so
	// into the finalized values), which aggResult publishes on the evalCtx so
	// rewriteSelectOuterRefs can substitute it into the body. Only ever non-empty
	// on a plan built by a hoist RETRY compile; see vdbe_agg_hoist.go.
	hoisted []hoistedAggRef

	// foreignAgg says rewritten holds a placeholder owned by an enclosing aggregate
	// query (substituted by aggItemSubst). Nothing reads it now: aggResultReg's chain
	// walk honours the owner. Kept as the plan-time fact.
	foreignAgg bool

	// prog is rewritten COMPILED against a register block holding this group's
	// finalized accumulators, its key tuple and its anchor row -- what
	// OpAggResult runs instead of walking the tree (compileAggItemProgram,
	// vdbe_agg_item_codegen.go). Stamped once by the compiler that assembles
	// the aggPlan; nil for an item that did not lower, which compileAggItemPrograms
	// then refuses -- the statement fails rather than being answered some other way.
	prog *aggItemProgram
}

// planGroupItem builds e's itemPlan against the resolved GROUP BY expressions and
// their affinities and collations (parallel slices). scopes resolves a bare
// column's identity; nil when there are no GROUP BY expressions. allowBare lets a
// bare column read the anchor row instead of being rejected. outerCols,
// outerFrom and pager seed checkItemSubqueries' outer-alias chain (nil disables
// the outer-alias hoist). aggregateOwnedHere is C's SF_Aggregate for the owning
// query (resolve.c:1976). outerCompiler is the live compiler chain enclosing
// the owning query, so a call this item does not own can be escalated further
// out (recordSpec).
func planGroupItem(e Expr, groupExprs []Expr, groupAffs []affinity, groupColls []groupKeyCollation, scopes []tableScope, allowBare bool, outerCols []SelectColumn, outerFrom []FromItem, pager *ReadOnlyPager, aggregateOwnedHere bool, outerCompiler *compiler, anchorObservable bool) (*itemPlan, error) {
	hoists, err := checkItemSubqueries(e, scopes, outerCols, outerFrom, pager, aggregateOwnedHere, outerCompiler, anchorObservable)
	if err != nil {
		return nil, err
	}
	var aggs []*aggItem
	var usesBare bareRead
	rewritten, err := rewriteGroupExpr(e, groupExprs, groupAffs, groupColls, scopes, &aggs, allowBare, &usesBare)
	if err != nil {
		return nil, err
	}
	it := &itemPlan{rewritten: rewritten, aggTemplates: aggs,
		usesGroupBare: usesBare.used, bareTables: usesBare.tables, bareWide: usesBare.wide,
		foreignAgg: exprHasForeignAggPlaceholder(rewritten)}
	if err := planItemHoists(it, hoists); err != nil {
		return nil, err
	}
	return it, nil
}

// checkItemSubqueries rejects item shapes containing a subquery that the
// finalize-time evaluation would answer wrongly, and returns the aggregate calls
// written inside those subqueries that belong to this query (scopes), for
// planGroupItem to plan as its own accumulators.
//
// aggregateOwnedHere says whether this query is aggregate on its own (a GROUP
// BY or a direct aggregate call, selectIsAggregateQuery) rather than only via a
// compound-arm aggregate hoisted from one of these subqueries. C resolves both in
// one pass (resolve.c:1346-1373); here an already-aggregate query is planned
// directly, so this AST walk is the only chance to place the hoist, while a
// non-aggregate query gets a row-mode attempt whose aggAssociationError triggers
// hoistOwnerFor's retry. outerCompiler: see itemAggHoists.outerCompiler.
func checkItemSubqueries(e Expr, scopes []tableScope, outerCols []SelectColumn, outerFrom []FromItem, pager *ReadOnlyPager, aggregateOwnedHere bool, outerCompiler *compiler, anchorObservable bool) ([]hoistSpec, error) {
	if !exprContainsSubquery(e) {
		return nil, nil
	}
	// (1) An explicit non-BINARY COLLATE inside an aggregate argument is fine: the
	// groupAggExpr carries it (expr.c:248).
	//
	// (2) An aggregate call inside the subquery whose argument names only the
	// enclosing query's columns is this query's own aggregate and is hoisted here
	// when this query's FROM supplies the column; one belonging further out
	// declines (see materializedOuterRefInAggArg).
	h := &itemAggHoists{scopes: scopes, pager: pager, aggregateOwnedHere: aggregateOwnedHere, outerCompiler: outerCompiler, anchorObservable: anchorObservable}
	// Same walk minMaxCensus itself uses over the select list (it also folds
	// in HAVING/ORDER BY, which this function has no access to -- an
	// under-approximation, not an over-approximation, and therefore still
	// safe; see recordSpec's own doc comment for why only an under-count
	// matters here).
	for _, sc := range outerCols {
		if !sc.Star {
			collectMinMaxCalls(sc.Expr, &h.census)
		}
	}
	// seedChain is this query's own level -- the ONE enclosing level a bare
	// alias inside a subquery written DIRECTLY in e can reach -- carried as
	// the seed of outerAliasAggCall's chain walk. A subquery nested further
	// still (inside the ones reached here) PREPENDS its own level onto this
	// same chain before recursing further; see outerScopeLevel's doc comment.
	seedChain := []outerScopeLevel{{cols: outerCols, from: outerFrom}}
	var walk func(Expr)
	walk = func(x Expr) {
		if x == nil {
			return
		}
		switch t := x.(type) {
		case SubqueryExpr:
			materializedOuterRefInAggArg(t.Stmt, nil, nil, h, seedChain)
		case ExistsExpr:
			materializedOuterRefInAggArg(t.Stmt, nil, nil, h, seedChain)
		case InExpr:
			walk(t.X)
			for _, it := range t.List {
				walk(it)
			}
			materializedOuterRefInAggArg(t.Sub, nil, nil, h, seedChain)
		case UnaryExpr:
			walk(t.X)
		case BinaryExpr:
			walk(t.L)
			walk(t.R)
		case IsNullExpr:
			walk(t.X)
		case BetweenExpr:
			walk(t.X)
			walk(t.Lo)
			walk(t.Hi)
		case LikeExpr:
			walk(t.X)
			walk(t.Pattern)
			walk(t.Escape)
		case GlobExpr:
			walk(t.X)
			walk(t.Pattern)
		case MatchExpr:
			walk(t.X)
			walk(t.Pattern)
		case CollateExpr:
			walk(t.X)
		case CastExpr:
			walk(t.X)
		case FuncExpr:
			for _, a := range t.Args {
				walk(a)
			}
			walk(t.Filter)
			for _, ob := range t.orderByExprs() {
				walk(ob)
			}
		case CaseExpr:
			walk(t.Base)
			for _, w := range t.Whens {
				walk(w.When)
				walk(w.Then)
			}
			walk(t.Else)
		}
	}
	walk(e)
	if h.blocked {
		return nil, fmt.Errorf("engine: unsupported: aggregate inside a select-list subquery whose argument belongs to the enclosing query")
	}
	return h.specs, nil
}

// itemAggHoists collects, for one item of an aggregate query, the aggregate calls
// inside its subqueries that C associates with the item's own query. record
// applies C's ownership test (sqlite3ReferencesSrcList == 1, i.e.
// aggArgHasLocalColumnRef, as hoistOwnerFor does): the call belongs to the
// closest enclosing query whose FROM supplies a referenced column. If that is not
// this query, it is escalated through the live compiler chain (recordSpec), or
// left to decline.
type itemAggHoists struct {
	scopes  []tableScope
	specs   []hoistSpec
	blocked bool

	// pager is consulted ONLY by the bare-alias branch
	// (materializedOuterRefInAggArgArm's ColumnExpr case, via
	// outerAliasAggCall) to prove a candidate name is NOT also a real column
	// of some enclosing level's own FROM (see aliasMightBeShadowedLocally).
	// nil at every call site that has none on hand, in which case the branch
	// never fires and behavior is unchanged from before it existed.
	pager *ReadOnlyPager

	// outerCompiler is the live compiler chain enclosing the query these scopes
	// belong to (the same chain hoistAggregatesOutward walks, c.outer). nil disables
	// recordSpec's escalation. hoistOwnerFor takes it as is: it is already one level
	// out from h.scopes.
	outerCompiler *compiler

	// anchorObservable is false when NOTHING in the owning statement can see
	// the min()/max() ANCHOR ROW (stmtReadsAnchorRow), in which case
	// recordSpec's census gate cannot matter and is skipped. Left TRUE at
	// every call site that cannot compute it, so behaviour there is unchanged.
	anchorObservable bool

	// census is the owning query's own TOP-LEVEL min()/max() call sites --
	// collectMinMaxCalls applied to outerCols alone, deliberately never
	// descending into a SubqueryExpr/ExistsExpr (collectMinMaxCalls' own
	// rule) -- so a call found only INSIDE the subquery this walk is
	// currently examining is never trivially "found" in its own text. See
	// recordSpec's own doc comment for what membership in this list proves.
	census []FuncExpr

	// aggregateOwnedHere is checkItemSubqueries' parameter of the same name, constant
	// for the whole walk. When false, this query became aggregate only via a hoist,
	// so hoistOwnerFor's retry is reachable and handles a min/max call. When true,
	// that retry never runs, and leaving the call unclaimed would fall through to
	// rewriteExprOuterRefs' per-row substitution, a wrong answer for a plain
	// aggregate (it reads one row, not the group).
	aggregateOwnedHere bool
}

// record notes that call, written in body, reads a column body's own FROM (and
// every FROM between body and here) does not supply.
func (h *itemAggHoists) record(call FuncExpr, body *SelectStmt) {
	h.recordSpec(call, body, "")
}

// recordNamed is record's counterpart for a hoist discovered through a BARE
// outer-alias reference rather than a literally-spelled call -- see
// hoistSpec.aliasName's doc comment for why the runtime substitution needs
// the name threaded through separately from call.
func (h *itemAggHoists) recordNamed(call FuncExpr, body *SelectStmt, aliasName string) {
	h.recordSpec(call, body, aliasName)
}

// callOwnedByScope reports whether call's own argument list names at least
// one column resolvable against scopes ALONE -- i.e. the same "does this
// actually belong here" test recordSpec uses to block a hoist whose argument
// turns out not to be one of this query's own columns. Shared with
// materializedOuterRefInAggArgArm's own escape-detection walk (see its
// isCompoundMember-with-empty-scope case), which needs to ask the identical
// question BEFORE deciding a bare aggregate call escapes to h's owner, not
// just after -- see that call site's own doc comment for why.
func callOwnedByScope(call FuncExpr, scopes []tableScope) bool {
	for _, e := range append(append(call.Args[:len(call.Args):len(call.Args)], call.Filter), call.orderByExprs()...) {
		if e != nil && aggArgHasLocalColumnRef(e, scopes) {
			return true
		}
	}
	return false
}

// isMinMaxAggName reports whether name is min or max: recordSpec refuses to
// hoist these through itemPlan.hoisted, since that would make the call one of the
// owning query's census sites, which this per-item collector cannot reconcile
// (hoistOwnerFor's compiler-chain delivery does, via honoredWithHoisted). The
// non-compound escape case also leaves min/max unclaimed so hoistOwnerFor's
// working path is not pre-empted (window_r24_fromless_assoc_test.go: "SELECT
// (SELECT max(a) + row_number() OVER ()) FROM t1").
func isMinMaxAggName(name string) bool {
	switch r33sFoldIdent(name) {
	case "min", "max":
		return true
	}
	return false
}

// minMaxCallInCensus reports whether call is sameAggCall to one of census --
// see recordSpec's own doc comment for why this membership test is the one
// narrow, provably-safe exception to that function's otherwise-unconditional
// min/max hoist refusal.
func minMaxCallInCensus(call FuncExpr, census []FuncExpr) bool {
	for _, c := range census {
		if sameAggCall(call, c) {
			return true
		}
	}
	return false
}

// recordSpec is record/recordNamed's shared body.
//
// When call does not belong to h.scopes either, it escapes further out, as C's
// climb continues as many levels as needed (resolve.c:1346-1373,
// expr.c:7200-7233). With h.outerCompiler set this runs at compile time against
// a real chain, so hoistOwnerFor walks it exactly as for a directly written call,
// delivering through oc.hoist.recordHoist and compileSelectScanRow's retry;
// hoistBodyInExpr checks reachability first (aggnested.test 9.1). Bare
// outer-alias references (aliasName != "") are not escalated.
//
// Known gap (TestAggNestedMultihopMinMaxStillDeclines): when escalation is what
// triggers the retry and the call is min()/max(), the retry's recompile of the
// owner sees the call as a new census site and declines. Safe, not yet fixed.
func (h *itemAggHoists) recordSpec(call FuncExpr, body *SelectStmt, aliasName string) {
	if !callOwnedByScope(call, h.scopes) {
		if aliasName == "" && h.outerCompiler != nil {
			if agg, aerr := planAggregateCall(call); aerr == nil {
				if oc, _ := hoistOwnerFor(h.outerCompiler, agg); oc != nil && oc.hoist != nil &&
					oc.hoist.recordHoist(hoistSpec{call: call, body: body}) {
					return
				}
			}
		}
		h.blocked = true
		return
	}
	// A hoisted min()/max() would be a census site for the anchor row that bare
	// columns and correlated subqueries in the item read, which minMaxCensus cannot
	// see (it does not descend into subqueries). This per-item collector cannot
	// reconcile that, so it declines: "SELECT k, v, (SELECT max(g1.v) FROM g2) FROM
	// g1 GROUP BY k" makes the bare v follow max's row.
	//
	// Except when the call is already, verbatim, in h.census (the query's own select
	// list): C reuses the same AggInfo slot for an equal call (expr.c:7478), and a
	// separate accumulator over the same rows reaches the same value, so no census
	// changes (aggnested-3.11: "SELECT max(value1), (SELECT count(*) FROM t2 WHERE
	// value2=max(value1)) FROM t1 GROUP BY id1").
	if isMinMaxAggName(call.Name) && !minMaxCallInCensus(call, h.census) && h.anchorObservable {
		// ...unless the ANCHOR ROW is unobservable in this statement, in which
		// case the census cannot be wrong about anything: the magnet only
		// changes which row a BARE column read reports, and there is none.
		// See stmtReadsAnchorRow.
		h.blocked = true
		return
	}
	for _, s := range h.specs {
		if s.body == body && ((aliasName == "" && sameAggCall(s.call, call)) ||
			(aliasName != "" && equalFoldName(s.aliasName, aliasName))) {
			return
		}
	}
	h.specs = append(h.specs, hoistSpec{call: call, body: body, aliasName: aliasName})
}

// planItemHoists turns each collected spec into another accumulator of it and
// records where its value must reappear (itemPlan.hoisted), for an enclosing
// query that is already aggregate, so nothing is recompiled (planHoistedAggs is
// the other case). hoistBodyInExpr is the reachability gate: the collector also
// walks positions the substitution does not reach (a MATCH operand, a FILTER),
// and an unreachable body would compute the aggregate over its own scan, so it
// declines.
func planItemHoists(it *itemPlan, specs []hoistSpec) error {
	for _, sp := range specs {
		if !hoistBodyInExpr(it.rewritten, sp.body) {
			return fmt.Errorf("%w: hoisted aggregate's subquery is not reachable from this item", errVDBEUnsupported)
		}
		tmpl, err := planAggregateCall(sp.call)
		if err != nil {
			return declineOrSemantic(err)
		}
		if err := checkExprSupported(tmpl.expr); err != nil {
			return declineOrSemantic(err)
		}
		if tmpl.sepExpr != nil {
			if err := checkExprSupported(tmpl.sepExpr); err != nil {
				return declineOrSemantic(err)
			}
		}
		it.hoisted = append(it.hoisted, hoistedAggRef{body: sp.body, call: sp.call, accIdx: len(it.aggTemplates), aliasName: sp.aliasName})
		it.aggTemplates = append(it.aggTemplates, tmpl)
	}
	return nil
}

// materializedOuterRefInAggArg finds, in stmt (a subquery in an aggregate query's
// item), every aggregate call whose argument reads a qualified column stmt's own
// FROM does not supply, and reports each to h, which records it as the enclosing
// query's aggregate or blocks. Returns whether any were found.
//
// Substituting the enclosing anchor row's value there would be wrong: C
// associates such a call with the enclosing query, so it is that query's
// aggregate over the whole group (subquery.test 3.4.1):
//
//	SELECT a.x, avg(a.y) FROM t34 AS a GROUP BY a.x
//	 HAVING NOT EXISTS(SELECT b.x, avg(b.y) FROM t34 AS b
//	                    GROUP BY b.x HAVING avg(a.y) > avg(b.y))
//
// returns only (107, 4.0).
//
// local is the set of FROM names in scope from enclosing subquery levels.
// owner is the aggregate call whose argument the walk is inside (sticky across
// nesting); a reference found under it in a deeper body is reported with a nil
// body, which blocks. chain is the enclosing SELECTs' Columns/From, nearest
// first, for the bare-alias branch (outerAliasAggCall).
func materializedOuterRefInAggArg(stmt *SelectStmt, local map[string]bool, owner *FuncExpr, h *itemAggHoists, chain []outerScopeLevel) bool {
	return materializedOuterRefInAggArgArm(stmt, local, owner, h, false, chain)
}

// materializedOuterRefInAggArgArm is the worker; compoundArm is true when stmt is
// a compound arm. Two contexts treat an unqualified reference in a plain
// aggregate's argument as escaping: a compound member, and a non-compound
// FROM-less body (see the case comments below; a windowed call is handled
// separately by rewriteExprOuterRefs). Nested subqueries inside an arm start
// fresh with compoundArm=false.
//
// The empty-scope rule additionally requires the reference to resolve against
// h.scopes (callOwnedByScope / aggArgHasLocalColumnRef, recordSpec's check)
// before treating it as escaping, since after a derived-table hop it may belong
// further out (aggnested.test: "... HAVING (SELECT count(*) FROM (SELECT 1 UNION
// ALL SELECT sum(DISTINCT c1)))", where c1 is t0's) while still hoisting where
// it does belong to h ("SELECT grp, (SELECT v FROM (SELECT avg(val) AS v UNION
// ALL SELECT max(val) AS v)) FROM g GROUP BY grp").
func materializedOuterRefInAggArgArm(stmt *SelectStmt, local map[string]bool, owner *FuncExpr, h *itemAggHoists, compoundArm bool, chain []outerScopeLevel) bool {
	if stmt == nil {
		return false
	}
	inner := make(map[string]bool, len(local)+len(stmt.From))
	for k := range local {
		inner[k] = true
	}
	addFromScopeNames(stmt.From, inner)

	// innerChain is the outer chain a subquery inside stmt's body sees: stmt's own
	// level prepended, as C passes the current NameContext as pOuterNC (resolve.c:1875).
	// A compound arm does not get it: arms share one NameContext.
	innerChain := append([]outerScopeLevel{{cols: stmt.Columns, from: stmt.From}}, chain...)

	// isCompoundMember also covers stmt being a compound's first arm
	// (len(stmt.Compound) > 0), which the top-level entry cannot tell apart from a
	// plain body. Otherwise window4.test 12.3's first arm ("avg(a)") went
	// undiscovered and the query never became aggregate.
	isCompoundMember := compoundArm || len(stmt.Compound) > 0

	found := false
	// escaped reports one offending reference: own is the aggregate call it sits
	// inside, here says that call is written at THIS level -- so the pair (call,
	// stmt) is what the runtime substitution will match on -- and hoistable says
	// the call sits in a CLAUSE the value may be substituted into. Anything else
	// blocks the item, leaving the pre-existing decline in place.
	escaped := func(own *FuncExpr, here, hoistable bool) {
		found = true
		if h == nil {
			return
		}
		if own == nil || !here || !hoistable {
			h.blocked = true
			return
		}
		h.record(*own, stmt)
	}
	// walk's hoistable is false where re-associating the call would not reproduce C:
	//
	//   - WHERE and JOIN ON allow an aggregate exactly when stmt itself is an
	//     aggregate query (selectIsAggregateQuery): otherwise C clears NC_AllowAgg
	//     before resolving them (resolve.c:1976-1984), giving "misuse of aggregate
	//     function". So "(SELECT w FROM g2 WHERE sum(g1.v) > w)" errors while
	//     "(SELECT count(*) FROM g2 WHERE sum(g1.v) > w)" works.
	//   - GROUP BY and ORDER BY allow one, but the substituted integer literal would
	//     become a column ordinal.
	//
	// aliasOK is the bare-alias branch's own gate: C's alias fallback
	// (resolve.c:648-692) depends on NC_UEList, set before HAVING and still set
	// through WHERE (resolve.c:1997), and its misuse check looks at the outer level
	// where the alias was found. So an outer alias in a non-aggregate subquery's
	// WHERE is fine ("... HAVING EXISTS(SELECT 1 FROM t2 WHERE c<s1)"). GROUP BY and
	// ORDER BY keep aliasOK false (unverified ordinal/alias matching).
	//
	// wherePos is true only while walking stmt's WHERE or an ON clause, gating the
	// schema-aware escape case below.
	var walk func(Expr, *FuncExpr, bool, bool, bool, bool)
	// walkTransparentBody walks a FROM-less subquery body inside an aggregate call
	// this level already claimed as part of this level, and reports whether it did.
	// That is sqlite3ReferencesSrcList's construction: a nested SELECT with no
	// FROM pushes nothing onto the exclude list (expr.c:7131), so its columns are
	// matched against this query's FROM as if written in the argument directly
	// (r35dFromlessSubHasLocalRef ports that for ownership; this ports it for
	// delivery). Recording at the call's own level makes (call, stmt) match what the
	// substitution expects.
	//
	// Guards: every arm must be r35dNoFromAnywhere-eligible (else fall back to
	// ordinary recursion), and expressions are enumerated by r35dArmExprs, skipping a
	// compound's ORDER BY and alias-captured names (window1.test 63.3).
	walkTransparentBody := func(sel *SelectStmt, own *FuncExpr, here, hoistable, aliasOK, wherePos bool) bool {
		if own == nil || !here || h == nil || sel == nil {
			return false
		}
		arms := append([]*SelectStmt{sel}, compoundArmStmts(sel)...)
		for _, a := range arms {
			if !r35dNoFromAnywhere(a) {
				return false
			}
		}
		// ...and every name the body carries has to RESOLVE here, because
		// hoisting the call replaces its whole argument with a finalized value
		// and nothing resolves those names again afterwards. See
		// r35dArmRefsResolve (sql_agg.go) for the measured case.
		if !r35dArmRefsResolve(sel, h.scopes) {
			return false
		}
		for _, a := range arms {
			r35dArmExprs(a, func(e Expr) {
				walk(e, own, here, hoistable, aliasOK, wherePos)
			})
		}
		return true
	}
	walk = func(x Expr, own *FuncExpr, here, hoistable, aliasOK, wherePos bool) {
		if x == nil {
			return
		}
		switch t := x.(type) {
		case ColumnExpr:
			if own != nil && t.Qualifier != "" && !inner[r33sFoldIdent(t.Qualifier)] {
				escaped(own, here, hoistable)
			} else if own != nil && t.Qualifier == "" && len(local) == 0 && len(stmt.From) == 0 && h != nil && aggArgHasLocalColumnRef(t, h.scopes) {
				// An unqualified reference in a body with an empty scope (no enclosing local
				// names and no FROM, e.g. "SELECT min(a)") can only bind outward; C's ownership
				// walk matches resolved cursors, not qualification (expr.c:7200). window4.test
				// 12.3: "SELECT (SELECT avg(a) UNION SELECT min(a) OVER ()) FROM t2 GROUP BY a"
				// is 1,2,3, hoisting avg(a).
				//
				// This also applies to a non-compound FROM-less body: when the owning query is
				// already aggregate (aggnested.test 7.1: "... GROUP BY name HAVING (select v>6
				// from (select sum(amount) v) t)"), its items have no live enclosing compiler,
				// so hoistOwnerFor's retry cannot run and this walk is the only place to act.
				//
				// min()/max() is excluded in that non-compound case: recordSpec refuses to
				// deliver one, and hoistOwnerFor's path often already works for it
				// (window_r24_fromless_assoc_test.go).
				//
				// Test stmt.From, not len(inner): an unaliased derived table adds no name to
				// inner but does supply columns.
				escaped(own, here, hoistable)
			} else if t.Qualifier == "" && h != nil && aliasOK {
				// A bare name that is some enclosing level's select-list alias (see
				// outerAliasAggCall): a separate resolution path from aggregate association
				// (resolveAlias, resolve.c:69-101), checked after the compound-member escape so
				// it never overrides it.
				if aliasCall, ok := outerAliasAggCall(h, stmt, t.Name, chain); ok {
					found = true
					h.recordNamed(aliasCall, stmt, t.Name)
				}
			}
		case FuncExpr:
			o, hr := own, here
			// Unless the call's argument also names a column stmt's FROM supplies: then
			// sqlite3ReferencesSrcList returns 1, the aggregate stays with stmt, and the
			// outer reference is a per-group constant the anchor row reproduces (as
			// planNoGroupAggregate's aggArgHasLocalColumnRef relaxation): "(SELECT
			// sum(t2.value2=t1.value1) FROM t2) FROM t1 GROUP BY id1".
			switch {
			case own == nil && isAggregateCall(t) && isCompoundMember && len(local) == 0 && len(stmt.From) == 0 && h != nil && !isMinMaxAggName(t.Name) && callOwnedByScope(t, h.scopes):
				// A plain aggregate call in a compound member with an empty scope is escaping:
				// len(stmt.From)==0 proves nothing local exists (window4.test 12.3's "avg(a)").
				//
				// min()/max() is excluded, as in the non-compound case below: recordSpec refuses
				// to deliver it, which would turn a decline into a hard block and pre-empt
				// hoistOwnerFor's working delivery for a compound-arm min/max (distinct.test
				// 6.1/6.2). C itself makes no min/max distinction in ownership (resolve.c:1346;
				// SQLITE_FUNC_MINMAX only feeds an index hint), so the asymmetry is only this
				// engine's two delivery mechanisms.
				fc := t
				o, hr = &fc, true
			case own == nil && isAggregateCall(t) && isCompoundMember && len(local) == 0 && len(stmt.From) == 0 && h != nil && isMinMaxAggName(t.Name) && h.aggregateOwnedHere && callOwnedByScope(t, h.scopes):
				// But when h.aggregateOwnedHere, hoistOwnerFor's retry never runs (the query is
				// planned as aggregate directly), and leaving a min/max call unclaimed would fall
				// through to rewriteExprOuterRefs' per-row substitution, a wrong answer: "SELECT
				// g, (SELECT 999 UNION SELECT max(v)) FROM t1 GROUP BY g" returned the first
				// row's v instead of the group max. So block here; escaped() with own==nil sets
				// h.blocked without calling h.record.
				escaped(nil, false, false)
			case own == nil && isAggregateCall(t) && !isCompoundMember && len(local) == 0 && len(stmt.From) == 0 && h != nil && (!isMinMaxAggName(t.Name) || h.aggregateOwnedHere) && callOwnedByScope(t, h.scopes):
				// A min()/max() is claimed only when this query is already aggregate: then
				// hoistOwnerFor's retry never runs, and recordSpec's census gate guards it
				// ("SELECT (SELECT max(x)) FROM t3 GROUP BY x"). The non-compound sibling of the
				// case above; see the matching ColumnExpr branch.
				fc := t
				o, hr = &fc, true
			case own == nil && isAggregateCall(t) && !aggCallHasLocalColumnArg(t, inner):
				fc := t
				o, hr = &fc, true
			case own == nil && isAggregateCall(t) && wherePos && hoistable && h != nil && aggCallArgsSurelyOuter(t, inner, stmt.From, h.pager):
				// The schema-aware sibling of the case above, for WHERE/ON only: an unqualified
				// argument counts as possibly local there too, but no later stage re-checks
				// ownership in WHERE (checkAggregateAssociation walks only select-list
				// templates), so such a call fell through to the scalar compiler ("wrong number
				// of arguments to function max()"). aggCallArgsSurelyOuter proves from schema
				// that every FROM table lacks the column, so the call can only belong outward,
				// as C resolves columns before asking who owns the call (resolve.c:648;
				// expr.c:7200). Gated on wherePos; escaped() is called directly because the
				// empty-FROM guard used above does not apply. aggnested-3.11: "SELECT
				// max(value1), (SELECT count(*) FROM t2 WHERE value2=max(value1)) FROM t1 GROUP
				// BY id1" is (12,2),(34,4).
				fc := t
				escaped(&fc, true, hoistable)
			}
			for _, arg := range t.Args {
				walk(arg, o, hr, hoistable, aliasOK, wherePos)
			}
			walk(t.Filter, o, hr, hoistable, aliasOK, wherePos)
			for _, ob := range t.orderByExprs() {
				walk(ob, o, hr, hoistable, aliasOK, wherePos)
			}
			// A window's inline spec is walked under the call's own NameContext
			// (resolve.c:1339-1340), so an aggregate in its PARTITION BY / ORDER BY is
			// attributed outward like any other ("SELECT b, (SELECT lead(b) OVER (ORDER BY
			// sum(b))) FROM a GROUP BY b", window1.test 55.1). A named window's definition is
			// not walked (it may be shared by two calls). All or nothing: if the spec walk
			// ends up blocked, h is restored and the call left unclaimed, since leaving it
			// was the working answer for e.g. rowvalue.test 30.1's "PARTITION BY
			// sum((SELECT t1.y))".
			if t.Over != nil && t.Over.Ref == "" && h != nil {
				savedBlocked, savedSpecs, savedFound := h.blocked, len(h.specs), found
				for _, pe := range t.Over.PartitionBy {
					walk(pe, own, here, hoistable, aliasOK, wherePos)
				}
				for _, ot := range t.Over.OrderBy {
					walk(ot.Expr, own, here, hoistable, aliasOK, wherePos)
				}
				if h.blocked && !savedBlocked {
					h.blocked, h.specs, found = savedBlocked, h.specs[:savedSpecs], savedFound
				}
			}
		case UnaryExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
		case BinaryExpr:
			walk(t.L, own, here, hoistable, aliasOK, wherePos)
			walk(t.R, own, here, hoistable, aliasOK, wherePos)
		case IsNullExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
		case InExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
			for _, it := range t.List {
				walk(it, own, here, hoistable, aliasOK, wherePos)
			}
			if materializedOuterRefInAggArgArm(t.Sub, inner, own, h, false, innerChain) {
				found = true
			}
		case BetweenExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
			walk(t.Lo, own, here, hoistable, aliasOK, wherePos)
			walk(t.Hi, own, here, hoistable, aliasOK, wherePos)
		case LikeExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
			walk(t.Pattern, own, here, hoistable, aliasOK, wherePos)
			walk(t.Escape, own, here, hoistable, aliasOK, wherePos)
		case GlobExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
			walk(t.Pattern, own, here, hoistable, aliasOK, wherePos)
		case MatchExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
			walk(t.Pattern, own, here, hoistable, aliasOK, wherePos)
		case CollateExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
		case CastExpr:
			walk(t.X, own, here, hoistable, aliasOK, wherePos)
		case CaseExpr:
			walk(t.Base, own, here, hoistable, aliasOK, wherePos)
			for _, w := range t.Whens {
				walk(w.When, own, here, hoistable, aliasOK, wherePos)
				walk(w.Then, own, here, hoistable, aliasOK, wherePos)
			}
			walk(t.Else, own, here, hoistable, aliasOK, wherePos)
		case SubqueryExpr:
			if walkTransparentBody(t.Stmt, own, here, hoistable, aliasOK, wherePos) {
				break
			}
			if materializedOuterRefInAggArgArm(t.Stmt, inner, own, h, false, innerChain) {
				found = true
			}
		case ExistsExpr:
			if walkTransparentBody(t.Stmt, own, here, hoistable, aliasOK, wherePos) {
				break
			}
			if materializedOuterRefInAggArgArm(t.Stmt, inner, own, h, false, innerChain) {
				found = true
			}
		}
	}

	stmtIsAgg := selectIsAggregateQuery(stmt)
	for _, c := range stmt.Columns {
		walk(c.Expr, owner, false, true, true, false)
	}
	walk(stmt.Where, owner, false, stmtIsAgg, true, true)
	for _, g := range stmt.GroupBy {
		walk(g, owner, false, false, false, false)
	}
	walk(stmt.Having, owner, false, true, true, false)
	for _, o := range stmt.OrderBy {
		walk(o.Expr, owner, false, false, false, false)
	}
	for _, it := range stmt.From {
		walk(it.On, owner, false, stmtIsAgg, true, true)
		if it.Subquery != nil {
			// A FROM-clause derived table's body is walked with the original local/chain,
			// not inner/innerChain: C resolves a FROM subquery with the owning query's
			// outer NameContext (resolve.c:1941), so it cannot see its FROM siblings but can
			// see everything enclosing. Without this, an aggregate escaping through a
			// derived table was never found: "SELECT grp, (SELECT v FROM (SELECT
			// sum(val)*2 AS v)) FROM g GROUP BY grp" is (1,60),(2,140), hoisting sum(val)
			// just as the unwrapped form does. hoistBodyInSelect and the substitution already
			// descend here.
			if materializedOuterRefInAggArgArm(it.Subquery, local, owner, h, false, chain) {
				found = true
			}
		}
	}
	// A compound arm is checked with the original local set and chain (the same
	// NameContext level as stmt), and compoundArm=true only here. A WITH clause's
	// CTE bodies are not walked.
	for _, a := range stmt.Compound {
		if materializedOuterRefInAggArgArm(a.Stmt, local, owner, h, true, chain) {
			found = true
		}
	}
	return found
}

// outerScopeLevel is one enclosing SELECT's alias list and FROM clause, carried
// nearest-first for outerAliasAggCall. A single top-level alias list was wrong:
// lookupName climbs one level at a time, trying each level's columns then its
// aliases (resolve.c:648-692), so a middle level's alias shadows an outer one:
//
//	SELECT a.x, avg(a.y) AS avg1 FROM t34 AS a GROUP BY a.x
//	 HAVING NOT EXISTS(SELECT b.x, avg(b.y) AS avg1 FROM t34 AS b
//	                    GROUP BY b.x HAVING EXISTS(SELECT 1 WHERE avg1 > 4.2))
//
// is 0 rows: the inner avg1 is the middle level's.
type outerScopeLevel struct {
	cols []SelectColumn
	from []FromItem
}

// outerAliasAggCall reports whether name, a bare reference inside stmt, is an
// enclosing query's select-list alias that C's resolveAlias would splice in
// (resolve.c:69-101), the fallback after FROM columns at each level
// (resolve.c:648-692). Levels are walked nearest first (stmt's, then chain's),
// and at each:
//
//   - if the level's FROM might supply a real column of that name
//     (aliasMightBeShadowedLocally, schema only), the whole search stops:
//     C would bind there;
//   - if the level defines an alias of that name at all, the search stops there
//     too (a closer alias always shadows);
//   - the matched alias must be a spelled-out aggregate call defined at the last
//     level, h's own query (the only one this collector plans for).
//
// It returns a copy of that aggregate call. Whether its argument could bind at
// the reference's level is not asked: resolveAlias splices an already-resolved
// copy, so "... HAVING EXISTS (SELECT 1 WHERE av>3)" works. Ownership is still
// checked by recordSpec's callOwnedByScope.
func outerAliasAggCall(h *itemAggHoists, stmt *SelectStmt, name string, chain []outerScopeLevel) (FuncExpr, bool) {
	levels := make([]outerScopeLevel, 0, len(chain)+1)
	levels = append(levels, outerScopeLevel{cols: stmt.Columns, from: stmt.From})
	levels = append(levels, chain...)
	for i, lvl := range levels {
		if h.pager.aliasMightBeShadowedLocally(lvl.from, name) {
			return FuncExpr{}, false
		}
		outerExpr, ok := resultAliasExpr(lvl.cols, name, true, false)
		if !ok {
			if hasAliasNamed(lvl.cols, name) {
				// This level defines an alias by this name but it is disqualified (a window
				// function); a same-named alias here still shadows anything further out
				// (resolve.c:69-101), so stop (see hasAliasNamed).
				return FuncExpr{}, false
			}
			continue
		}
		if i != len(levels)-1 {
			// The alias is defined at an INTERMEDIATE level, so it is that
			// query's own aggregate, not h's -- and h can only ever plan its
			// OWN item's accumulators (planItemHoists). Stop rather than
			// continue: a closer alias shadows every one further out, so
			// there is nothing left to find. That level's own planning pass
			// reaches this same alias with itself as levels' LAST entry.
			return FuncExpr{}, false
		}
		fc, ok := outerExpr.(FuncExpr)
		if !ok || !isAggregateCall(fc) {
			return FuncExpr{}, false
		}
		return fc, true
	}
	return FuncExpr{}, false
}

// aliasMightBeShadowedLocally reports whether one level's FROM could define a
// real column named alias, which lookupName would bind before that level's
// aliases (resolve.c:648-692). It clears an item only when the schema proves the
// negative (a catalog lookup, no scan); a derived table, table function,
// schema-qualified name, view, CTE, vtab or unknown name counts as "maybe",
// which only declines.
//
// The implicit rowid counts too: an unqualified rowid/oid/_rowid_ binds to an
// ordinary table's rowid (resolve.c:564-568) when VisibleRowid (not WITHOUT
// ROWID, build.c:2731), before the search climbs
// (TestAggNestedRowidShadowStillDeclines). tableMeta.withoutRowid mirrors
// VisibleRowid for the base tables that reach this point.
func (p *ReadOnlyPager) aliasMightBeShadowedLocally(from []FromItem, alias string) bool {
	if p == nil {
		return true
	}
	isRowid := isRowidAliasName(alias)
	for _, it := range from {
		if it.Subquery != nil || it.TableFunc || it.Table == "" || it.Schema != "" {
			return true
		}
		rt, err := p.resolveTable(it.Table)
		if err != nil {
			return true
		}
		for _, c := range rt.cols {
			if equalFoldName(c.Name, alias) {
				return true
			}
		}
		if isRowid && !rt.withoutRowid {
			return true
		}
	}
	return false
}

// aggCallHasLocalColumnArg reports whether fc's argument list (arguments and
// FILTER, not nested subqueries, as selectRefEnter excludes them) names a column
// the subquery it is in can supply. inner holds FROM names, so only qualifiers
// match exactly; an unqualified reference counts as possibly local, which is safe
// because it reaches the subquery's own compile, where checkAggregateAssociation
// re-decides it against the real scopes.
func aggCallHasLocalColumnArg(fc FuncExpr, inner map[string]bool) bool {
	found := false
	var walk func(Expr)
	walk = func(x Expr) {
		if found || x == nil {
			return
		}
		switch t := x.(type) {
		case ColumnExpr:
			if t.Qualifier == "" || inner[r33sFoldIdent(t.Qualifier)] {
				found = true
			}
		case FuncExpr:
			for _, a := range t.Args {
				walk(a)
			}
			walk(t.Filter)
			for _, ob := range t.orderByExprs() {
				walk(ob)
			}
		case UnaryExpr:
			walk(t.X)
		case BinaryExpr:
			walk(t.L)
			walk(t.R)
		case IsNullExpr:
			walk(t.X)
		case InExpr:
			walk(t.X)
			for _, it := range t.List {
				walk(it)
			}
		case BetweenExpr:
			walk(t.X)
			walk(t.Lo)
			walk(t.Hi)
		case LikeExpr:
			walk(t.X)
			walk(t.Pattern)
			walk(t.Escape)
		case GlobExpr:
			walk(t.X)
			walk(t.Pattern)
		case MatchExpr:
			walk(t.X)
			walk(t.Pattern)
		case CollateExpr:
			walk(t.X)
		case CastExpr:
			walk(t.X)
		case CaseExpr:
			walk(t.Base)
			for _, w := range t.Whens {
				walk(w.When)
				walk(w.Then)
			}
			walk(t.Else)
		// SubqueryExpr/ExistsExpr: the aggregate's argument may contain a whole
		// independent query, whose own FROM tables are EXCLUDED from the scan
		// (selectRefEnter) -- a column there proves nothing about this call.
		default:
		}
	}
	for _, a := range fc.Args {
		walk(a)
	}
	walk(fc.Filter)
	for _, ob := range fc.orderByExprs() {
		walk(ob)
	}
	return found
}

// aggCallArgsSurelyOuter reports whether fc references at least one column and
// none of them can be supplied by from: a qualified one matching inner, or an
// unqualified one from's schema declares. Needed for WHERE/ON arguments, where
// the "possibly local" default of aggCallHasLocalColumnArg has no later
// resolution (C resolves every argument column before sqlite3ReferencesSrcList
// asks who owns the call, resolve.c:1346; expr.c:7200). A call with no column
// references (count(*)) is never surely outer: it stays with the closest level
// (sqlite3ReferencesSrcList returns -1). Schema lookups reuse
// aliasMightBeShadowedLocally, so anything unprovable counts as possibly local,
// and any local reference claims the whole call (expr.c:7178).
func aggCallArgsSurelyOuter(fc FuncExpr, inner map[string]bool, from []FromItem, pager *ReadOnlyPager) bool {
	hasColumn := false
	hasLocal := false
	var walk func(Expr)
	walk = func(x Expr) {
		if hasLocal || x == nil {
			return
		}
		switch t := x.(type) {
		case ColumnExpr:
			hasColumn = true
			if t.Qualifier != "" {
				if inner[r33sFoldIdent(t.Qualifier)] {
					hasLocal = true
				}
			} else if pager.aliasMightBeShadowedLocally(from, t.Name) {
				hasLocal = true
			}
		case FuncExpr:
			for _, a := range t.Args {
				walk(a)
			}
			walk(t.Filter)
			for _, ob := range t.orderByExprs() {
				walk(ob)
			}
		case UnaryExpr:
			walk(t.X)
		case BinaryExpr:
			walk(t.L)
			walk(t.R)
		case IsNullExpr:
			walk(t.X)
		case InExpr:
			walk(t.X)
			for _, it := range t.List {
				walk(it)
			}
		case BetweenExpr:
			walk(t.X)
			walk(t.Lo)
			walk(t.Hi)
		case LikeExpr:
			walk(t.X)
			walk(t.Pattern)
			walk(t.Escape)
		case GlobExpr:
			walk(t.X)
			walk(t.Pattern)
		case MatchExpr:
			walk(t.X)
			walk(t.Pattern)
		case CollateExpr:
			walk(t.X)
		case CastExpr:
			walk(t.X)
		case CaseExpr:
			walk(t.Base)
			for _, w := range t.Whens {
				walk(w.When)
				walk(w.Then)
			}
			walk(t.Else)
		// SubqueryExpr/ExistsExpr: same exclusion as aggCallHasLocalColumnArg,
		// above -- a column inside a nested independent query proves nothing
		// about fc's own local-ness.
		default:
		}
	}
	for _, a := range fc.Args {
		walk(a)
	}
	walk(fc.Filter)
	for _, ob := range fc.orderByExprs() {
		walk(ob)
	}
	return hasColumn && !hasLocal
}

// rowidIdentityBase is subtracted (times 1, i.e. offset) from a pseudo
// rowid/oid/_rowid_ column's table index to build resolveColumnIndex's
// synthetic identity for it (see below): chosen far below any real vals
// offset a query's flattened columns could ever reach, so a pseudo-rowid
// identity can never collide with a real column's.
const rowidIdentityBase = -1_000_000

// resolveColumnIndex statically resolves ce against scopes to the absolute index
// a row-bound evalCtx would store its value at, or ok==false (missing or
// ambiguous). Two references resolving to the same index name the same column
// however spelled. A rowid pseudo-column gets a synthetic identity
// (rowidIdentityBase - table index), so "GROUP BY rowid" also legalizes
// "_rowid_".
func resolveColumnIndex(scopes []tableScope, ce ColumnExpr) (int, bool) {
	ctx := &evalCtx{tables: scopes}
	_, idx, _, rowidTableIdx, err := resolveColumn(ctx, ce.Qualifier, ce.Name)
	if err != nil {
		return 0, false
	}
	if rowidTableIdx >= 0 {
		return rowidIdentityBase - rowidTableIdx, true
	}
	return idx, true
}

// bareRead is what rewriteGroupExpr's bare-column resolutions read, accumulated
// across one item: used is "at least one groupBareColExpr was produced for a
// column the GROUP BY does NOT name", and tables/wide carry WHICH FROM item each
// came from -- see itemPlan.bareTables, which is where they end up and which
// explains what reads them. A groupBareColExpr standing in for a column the
// GROUP BY DOES name reads the very same anchor row (the GROUP-BY-KEY REFERENCE
// RULE below) but is deliberately NOT recorded here: see anchorIsRead
// (vdbe_agg_codegen.go) for why the guard does not price the two the same.
type bareRead struct {
	used   bool
	tables uint64
	wide   bool
}

// mark records one resolved bare column, read from FROM item at scope position
// scope (or -1 when this walk could not attribute it to one).
func (b *bareRead) mark(scope int) {
	b.used = true
	if scope < 0 || scope >= 64 {
		b.wide = true
		return
	}
	b.tables |= uint64(1) << uint(scope)
}

// THE GROUP-BY-KEY REFERENCE RULE: where a reference to a GROUP BY key reads its
// value from. select.c's GROUP BY codegen, after computing the row's key:
//
//	if( iOrderByCol ){
//	  Expr *pX = p->pEList->a[iOrderByCol-1].pExpr;
//	  Expr *pBase = sqlite3ExprSkipCollateAndLikely(pX);
//	  while( ALWAYS(pBase!=0) && pBase->op==TK_IF_NULL_ROW ){ ... }
//	  if( ALWAYS(pBase!=0) && pBase->op!=TK_AGG_COLUMN && pBase->op!=TK_REGISTER ){
//	    sqlite3ExprToRegister(pX, iAMem+j);
//	  }
//	}
//
// iOrderByCol points at a select-list entry structurally equal to the GROUP BY
// term; iAMem holds the group's first row's key. Bare columns are already
// TK_AGG_COLUMN, read from registers reloaded under the min()/max() magnet. So:
//
//   - a bare column reference (COLLATE skipped) reads the group's anchor row,
//     everywhere (select list, HAVING, ORDER BY);
//   - anything else ("a+0", a CAST) reads the first row's key, but only in the
//     one select-list entry iOrderByCol points at.
//
// They differ when a group holds byte-distinct equal keys (INTEGER 1 vs REAL
// 1.0). Over n(a,b) = (1,'y'),(1.0,'z'),(2,'x'),(2.0,'w'),(1.0,'v'),(0,'u'):
//
//	SELECT a, min(b)          FROM n GROUP BY a    ->  0|u,  1.0|v,  2.0|w
//	SELECT a, max(b)          FROM n GROUP BY a    ->  0|u,  1.0|z,  2  |x
//	SELECT a, max(NULL)       FROM n GROUP BY a    ->  0| ,  1.0| ,  2.0|
//	SELECT a, count(*)        FROM n GROUP BY a    ->  0|1,  1  |3,  2  |2
//	SELECT a+0, max(b)        FROM n GROUP BY a+0  ->  0|u,  1  |z,  2  |x
//	SELECT a, a+0, max(b)     FROM n GROUP BY a    ->  ..., 1.0|1.0|z, ...
//	SELECT a+0, a+0, max(b)   FROM n GROUP BY a+0  ->  ..., 1.0|1  |z, ...
//
// The first rule is reproduced by resolving a bare key reference to
// groupBareColExpr, the second by groupKeyExpr (the group's first row). The last
// line's split (only the last matching entry is rewritten) is not reproduced: a
// known two-cell divergence.

// joinHasCoalescedColumns reports whether any FROM item carries a RIGHT/FULL
// JOIN USING/NATURAL coalesced column: refIsRightJoinCoalesced asked of the whole
// join, for a plan decision with no particular reference.
func joinHasCoalescedColumns(scopes []tableScope) bool {
	for i := range scopes {
		if len(scopes[i].coalesceFallback) > 0 {
			return true
		}
	}
	return false
}

// refIsRightJoinCoalesced reports whether ce names a USING/NATURAL column a
// RIGHT or FULL JOIN coalesces (tableScope.coalesceFallback). C builds such a
// reference as a COALESCE, so it is not a TK_AGG_COLUMN and reads the group's
// first-row key register, and which side supplies it is decided per row; an
// anchor placeholder would freeze that choice (joink_r27 coalesce-chain shapes).
// Asked of every scope; a spurious true keeps the first-row answer.
func refIsRightJoinCoalesced(scopes []tableScope, ce ColumnExpr) bool {
	lname := r33sFoldIdent(ce.Name)
	for i := range scopes {
		if len(scopes[i].coalesceFallback[lname]) > 0 {
			return true
		}
	}
	return false
}

// scopeOfValueIndex maps an ABSOLUTE row-value index -- resolveColumn's idx, the
// one groupBareColExpr carries -- back to the FROM item that supplies it, or -1
// when no scope covers it.
func scopeOfValueIndex(scopes []tableScope, idx int) int {
	for i := range scopes {
		if idx >= scopes[i].offset && idx < scopes[i].offset+len(scopes[i].cols) {
			return i
		}
	}
	return -1
}

// exprHasForeignAggPlaceholder reports whether e holds an aggregate-result
// placeholder that names an ENCLOSING aggregate query (see the owner note above
// groupKeyExpr). It walks the item's OWN level only, which is where such a
// placeholder lands: aggItemSubst substitutes into a subquery BODY, and the
// body's clauses become the inner query's own items -- so by the time this runs
// on that inner item, the node is at its top level. A body this item still
// carries verbatim is compiled, never walked, so it needs no answer here.
func exprHasForeignAggPlaceholder(e Expr) bool {
	found := false
	var walk func(Expr)
	walk = func(x Expr) {
		if found || x == nil {
			return
		}
		switch t := x.(type) {
		case groupKeyExpr:
			found = t.owner != nil
		case groupAggExpr:
			found = t.owner != nil
		case groupBareColExpr:
			found = t.owner != nil
		}
		if !found {
			walkExprOperands(x, walk)
		}
	}
	walk(e)
	return found
}

// rewriteGroupExpr is the recursive worker behind planGroupItem: see this
// file's package doc comment for the overall strategy. aggs accumulates this
// item's aggregate templates (in appearance order); groupExprs/groupAffs are
// the query's GROUP BY expressions and their plan-time affinities. allowBare
// says whether a bare column may resolve to the group's anchor row at all;
// usesBare accumulates every one that does, and which FROM item it came from
// (see the ColumnExpr case below, and bareRead).
func rewriteGroupExpr(e Expr, groupExprs []Expr, groupAffs []affinity, groupColls []groupKeyCollation, scopes []tableScope, aggs *[]*aggItem, allowBare bool, usesBare *bareRead) (Expr, error) {
	// A structural match against a GROUP BY expression wins over the aggregate check
	// (a GROUP BY expression is never an aggregate). A bare column is excluded here;
	// the ColumnExpr case handles textual and resolved-identity matches together and
	// reads the anchor row (the GROUP-BY-KEY REFERENCE RULE).
	if _, isBareCol := e.(ColumnExpr); !isBareCol {
		for i, ge := range groupExprs {
			if exprEqual(e, ge) {
				return groupKeyExpr{idx: i, aff: groupAffs[i], coll: collAt(groupColls, i).name, collExplicit: collAt(groupColls, i).explicit}, nil
			}
		}
	}
	if fc, ok := e.(FuncExpr); ok && isAggregateCall(fc) {
		tmpl, err := planAggregateCall(fc)
		if err != nil {
			return nil, err
		}
		idx := len(*aggs)
		*aggs = append(*aggs, tmpl)
		// The call's own explicit collation travels with the placeholder -- see
		// groupAggExpr.coll for the C rule and the wrong answer losing it gave.
		coll, _ := exprCollation(fc)
		return groupAggExpr{accIdx: idx, coll: coll}, nil
	}

	switch x := e.(type) {
	case nil:
		return nil, nil
	case LiteralExpr:
		return x, nil
	case ParamExpr:
		return x, nil
	case windowResultExpr:
		// A window-function placeholder (sql_window.go) already stands in for a
		// window call whose own argument/partition/order expressions were
		// rewritten separately -- pass it straight through so a GROUP BY query
		// whose select list mixes windows with group aggregates/keys works.
		return x, nil
	case groupKeyExpr:
		return x, nil
	case groupAggExpr:
		return x, nil
	case groupBareColExpr:
		// A placeholder arriving as input belongs to an enclosing aggregate query
		// (substituted by aggItemSubst): a constant for this query's whole run, passed
		// through as is; its owner keeps aggResultReg honest. C likewise converts only
		// columns of this query's SrcList (expr.c:7458) and leaves another AggInfo's
		// nodes alone (expr.c:4996).
		return x, nil
	case ColumnExpr:
		// Which GROUP BY term this reference is: a textual match, or the same resolved
		// column however qualified ("GROUP BY cor0.col1" legalizes a bare col1).
		keyIdx := -1
		for i, ge := range groupExprs {
			if exprEqual(x, ge) {
				keyIdx = i
				break
			}
		}
		if keyIdx < 0 {
			if idx, ok := resolveColumnIndex(scopes, x); ok {
				for i, ge := range groupExprs {
					geCol, isCol := ge.(ColumnExpr)
					if !isCol {
						continue
					}
					if gidx, gok := resolveColumnIndex(scopes, geCol); gok && gidx == idx {
						keyIdx = i
						break
					}
				}
			}
		}
		if keyIdx >= 0 && refIsRightJoinCoalesced(scopes, x) {
			// Not a plain column in SQLite either -- see this helper. Answer it
			// from the key tuple, which is exactly what the C's iAMem rewrite
			// does for a non-column GROUP BY term.
			return groupKeyExpr{idx: keyIdx, aff: groupAffs[keyIdx], coll: collAt(groupColls, keyIdx).name, collExplicit: collAt(groupColls, keyIdx).explicit}, nil
		}
		// A bare column of this query's FROM reads the group's anchor row
		// (groupBareColExpr), decided per group at run time by magnetWalk; a reference
		// to a GROUP BY key reads there too (the GROUP-BY-KEY REFERENCE RULE).
		bctx := &evalCtx{tables: scopes}
		_, bidx, bcol, browidTableIdx, berr := resolveColumn(bctx, x.Qualifier, x.Name)
		if berr != nil && strings.HasPrefix(berr.Error(), "engine: ambiguous column name: ") {
			// Not "resolves nowhere": this query's own FROM offers the name
			// twice, which lookupName reports as "ambiguous column name"
			// (resolve.c:785) during name resolution, before any aggregate
			// analysis runs -- so it is C SQLite's error to raise here, not a
			// correlated reference to pass through (which then declined as a
			// capability gap). semanticf keeps it out of declineOrSemantic's
			// "unsupported" rewording.
			return nil, semanticf("%s", berr.Error())
		}
		if berr != nil {
			// A GROUP BY term this query's own scopes cannot resolve -- a
			// correlated one, matched textually above -- still answers from the
			// key tuple: there is no local column to read an anchor row for, and
			// its value is a per-group constant either way.
			if keyIdx >= 0 {
				return groupKeyExpr{idx: keyIdx, aff: groupAffs[keyIdx], coll: collAt(groupColls, keyIdx).name, collExplicit: collAt(groupColls, keyIdx).explicit}, nil
			}
			// x names no column of this query's FROM: either a correlated reference to an
			// enclosing query (a per-group constant, which C allows: "SELECT (SELECT
			// max(c7)+c8 FROM t7) FROM t8") or a bad name, reported as "no such column" where
			// other references are resolved. Reachability is checked by bindAggOuterRefs.
			return x, nil
		}
		// allowBare governs only a column the GROUP BY does NOT name; a
		// reference to a key is a legal grouped reference whatever it says.
		if allowBare || keyIdx >= 0 {
			if browidTableIdx >= 0 {
				if keyIdx < 0 {
					usesBare.mark(browidTableIdx)
				}
				// The rowid pseudo-column has INTEGER affinity (sqlite3TableColumnAffinity,
				// expr.c:24-27: iCol<0 is INTEGER; resolve.c:564 maps rowid and the IPK to -1).
				// Dropping it made "ta.rowid = ta.x" compare without numeric coercion in a
				// grouped query. No collation: expr.c:261 only adds one for iColumn >= 0.
				return groupBareColExpr{isRowid: true, rowidTableIdx: browidTableIdx, aff: affInteger}, nil
			}
			if keyIdx < 0 {
				usesBare.mark(scopeOfValueIndex(scopes, bidx))
			}
			var baff affinity
			var bcoll string
			var bnoaff bool
			if bcol != nil {
				baff = bcol.Aff
				bcoll = effectiveCollation(bcol.Collation)
				bnoaff = bcol.NoAffinity
			}
			return groupBareColExpr{idx: bidx, aff: baff, coll: bcoll, noAff: bnoaff}, nil
		}
		return nil, fmt.Errorf("engine: unsupported: column reference %q is neither an aggregate argument nor one of the GROUP BY expressions", x.Name)
	case UnaryExpr:
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return UnaryExpr{Op: x.Op, X: xx}, nil
	case BinaryExpr:
		l, err := rewriteGroupExpr(x.L, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		r, err := rewriteGroupExpr(x.R, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return BinaryExpr{Op: x.Op, L: l, R: r}, nil
	case IsNullExpr:
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return IsNullExpr{X: xx, Not: x.Not}, nil
	case InExpr:
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		list := make([]Expr, len(x.List))
		for i, item := range x.List {
			li, err := rewriteGroupExpr(item, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
			if err != nil {
				return nil, err
			}
			list[i] = li
		}
		// Keep Sub: rebuilding the node without it turned "x IN (SELECT ...)" into
		// "x IN ()", always false. The subquery body is its own scope and is not
		// rewritten.
		return InExpr{X: xx, List: list, Sub: x.Sub, Not: x.Not}, nil
	case BetweenExpr:
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		lo, err := rewriteGroupExpr(x.Lo, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		hi, err := rewriteGroupExpr(x.Hi, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return BetweenExpr{X: xx, Lo: lo, Hi: hi, Not: x.Not}, nil
	case LikeExpr:
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		pat, err := rewriteGroupExpr(x.Pattern, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		esc, err := rewriteGroupExpr(x.Escape, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return LikeExpr{X: xx, Pattern: pat, Escape: esc, Not: x.Not}, nil
	case GlobExpr:
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		pat, err := rewriteGroupExpr(x.Pattern, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return GlobExpr{X: xx, Pattern: pat, Not: x.Not}, nil
	case CollateExpr:
		// An explicit COLLATE here did not match a GROUP BY key (that would have been
		// replaced above), so it only affects comparisons in this item, not the grouping:
		// "SELECT b>a COLLATE NOCASE FROM t2 GROUP BY a, b" is 1.
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return CollateExpr{X: xx, Name: x.Name}, nil
	case FuncExpr:
		// Not an aggregate call (already checked above): an ordinary scalar
		// function, validated exactly like checkExprSupported
		// but rewriting each argument along the way.
		if x.Star {
			return nil, fmt.Errorf("engine: unsupported use of * in %s()", x.Name)
		}
		// A connection-state function (changes(), total_changes(),
		// last_insert_rowid(), sqlite_version()) is not in supportedFuncs but is
		// allowed, as checkExprSupported allows it, with its arity check.
		if isConnStateFunc(r33sFoldIdent(x.Name)) {
			if len(x.Args) != 0 {
				return nil, fmt.Errorf("engine: wrong number of arguments to function %s()", x.Name)
			}
			return FuncExpr{Name: x.Name}, nil
		}
		if !supportedFuncs[r33sFoldIdent(x.Name)] {
			// An aggregate or window name here was written where that kind of
			// call is not allowed -- inside another aggregate's own argument,
			// for one ("SELECT sum(sum(a)) FROM t"). That is C's own
			// prepare-time "misuse of %s function" (resolve.c:1265-1277), not
			// a function this engine lacks.
			if err := misuseOfFuncErr(x.Name, x.Over != nil); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("engine: unsupported function %s()", x.Name)
		}
		// A SCALAR call is rebuilt below from its name and arguments alone, so
		// a FILTER or an ORDER BY on it would be dropped rather than refused.
		// C refuses both at resolve time (resolve.c:1297-1307), FILTER first.
		if x.Filter != nil {
			return nil, semanticf("engine: FILTER may not be used with non-aggregate %s()", x.Name)
		}
		if len(x.OrderBy) > 0 {
			return nil, semanticf("engine: ORDER BY may not be used with non-aggregate %s()", x.Name)
		}
		args := make([]Expr, len(x.Args))
		for i, a := range x.Args {
			ai, err := rewriteGroupExpr(a, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
			if err != nil {
				return nil, err
			}
			args[i] = ai
		}
		return FuncExpr{Name: x.Name, Args: args, Star: false}, nil
	case CastExpr:
		xx, err := rewriteGroupExpr(x.X, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
		if err != nil {
			return nil, err
		}
		return CastExpr{X: xx, Type: x.Type}, nil
	case CaseExpr:
		var base Expr
		if x.Base != nil {
			b, err := rewriteGroupExpr(x.Base, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
			if err != nil {
				return nil, err
			}
			base = b
		}
		whens := make([]WhenClause, len(x.Whens))
		for i, w := range x.Whens {
			when, err := rewriteGroupExpr(w.When, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
			if err != nil {
				return nil, err
			}
			then, err := rewriteGroupExpr(w.Then, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
			if err != nil {
				return nil, err
			}
			whens[i] = WhenClause{When: when, Then: then}
		}
		var elseE Expr
		if x.Else != nil {
			e2, err := rewriteGroupExpr(x.Else, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
			if err != nil {
				return nil, err
			}
			elseE = e2
		}
		// x itself, so an isBool node keeps its flags (desugarIsBool).
		x.Base, x.Whens, x.Else = base, whens, elseE
		return x, nil
	case RowExpr:
		// A row value's elements are ordinary children, walked like function arguments
		// (walker.c:84; resolve.c:977), so rewrite them: e.g. the probe of
		// "(0, 0) IN (SELECT min(c0), ntile(1) OVER())" once the query becomes
		// aggregate. Only row-vs-subquery probes still have a RowExpr here; the subquery
		// on the other side is untouched.
		elems := make([]Expr, len(x.Elems))
		for i, el := range x.Elems {
			ee, err := rewriteGroupExpr(el, groupExprs, groupAffs, groupColls, scopes, aggs, allowBare, usesBare)
			if err != nil {
				return nil, err
			}
			elems[i] = ee
		}
		return RowExpr{Elems: elems}, nil
	case SubqueryExpr:
		// Nothing to rewrite structurally: x.Stmt is its own scope,
		// compiled on its own terms. A correlated reference inside it
		// resolves through the outer chain, not through this rewrite.
		return x, nil
	case ExistsExpr:
		// Same reasoning as SubqueryExpr above.
		return x, nil
	default:
		return nil, fmt.Errorf("engine: unsupported expression %T", e)
	}
}

// collectMinMaxCalls appends every 1-argument min()/max() aggregate call site
// (isAggregateCall's min/max case) found anywhere in e's expression tree into
// *out, in traversal order -- the same recursive shape as containsAggregate
// (sql_agg.go), generalized to COLLECT call sites instead of just reporting
// whether any exists, and, like containsAggregate, deliberately NOT
// descending into a SubqueryExpr/ExistsExpr's own independent scope. Used
// only to build a statement's min/max CENSUS (minMaxCensus).
func collectMinMaxCalls(e Expr, out *[]FuncExpr) {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return
	case FuncExpr:
		if isAggregateCall(x) {
			name := r33sFoldIdent(x.Name)
			if name == "min" || name == "max" {
				*out = append(*out, x)
			}
			return // aggregates never nest (rejected elsewhere), so no need to recurse into args
		}
		for _, a := range x.walkArgs() {
			collectMinMaxCalls(a, out)
		}
	case UnaryExpr:
		collectMinMaxCalls(x.X, out)
	case BinaryExpr:
		collectMinMaxCalls(x.L, out)
		collectMinMaxCalls(x.R, out)
	case IsNullExpr:
		collectMinMaxCalls(x.X, out)
	case InExpr:
		collectMinMaxCalls(x.X, out)
		for _, a := range x.List {
			collectMinMaxCalls(a, out)
		}
	case BetweenExpr:
		collectMinMaxCalls(x.X, out)
		collectMinMaxCalls(x.Lo, out)
		collectMinMaxCalls(x.Hi, out)
	case LikeExpr:
		collectMinMaxCalls(x.X, out)
		collectMinMaxCalls(x.Pattern, out)
		collectMinMaxCalls(x.Escape, out)
	case GlobExpr:
		collectMinMaxCalls(x.X, out)
		collectMinMaxCalls(x.Pattern, out)
	case CollateExpr:
		collectMinMaxCalls(x.X, out)
	case CastExpr:
		collectMinMaxCalls(x.X, out)
	case CaseExpr:
		if x.Base != nil {
			collectMinMaxCalls(x.Base, out)
		}
		for _, w := range x.Whens {
			collectMinMaxCalls(w.When, out)
			collectMinMaxCalls(w.Then, out)
		}
		if x.Else != nil {
			collectMinMaxCalls(x.Else, out)
		}
	// SubqueryExpr/ExistsExpr: an independent scope, deliberately not
	// descended into (matches containsAggregate's identical reasoning).
	default:
		return
	}
}

// minMaxCensus is the ordered, de-duplicated list of min()/max() calls C's
// aggregate analysis builds for stmt: the census the anchor walk uses
// (magnetPlan). Order is select list, then ORDER BY, then HAVING, as C visits
// them; it matters with sites in both HAVING and ORDER BY:
//
//	SELECT count(*), a, b FROM t0 HAVING min(a)>0 ORDER BY max(b)
//	-- C SQLite 3|1|7 (min(a) is the LAST site, and 2 does not beat 1)
//
// Duplicates keep the first position (C reuses the aFunc slot via
// sqlite3ExprCompare); the test is sameAggCall, which, like C, distinguishes a
// FILTER (TestR26AnchorFuzz).
func minMaxCensus(stmt *SelectStmt) []FuncExpr {
	var found []FuncExpr
	for _, c := range stmt.Columns {
		if !c.Star {
			collectMinMaxCalls(c.Expr, &found)
		}
	}
	for _, ot := range stmt.OrderBy {
		collectMinMaxCalls(ot.Expr, &found)
	}
	collectMinMaxCalls(stmt.Having, &found)
	return dedupMinMaxCalls(found)
}

// dedupMinMaxCalls drops every call site sameAggCall to an earlier one, keeping
// the first occurrence in place. Shared with the window-query variant
// (windowQueryMinMaxCensus, vdbe_window_group.go), whose only difference is
// what it feeds in and in what order.
func dedupMinMaxCalls(found []FuncExpr) []FuncExpr {
	var distinct []FuncExpr
scan:
	for _, f := range found {
		for _, d := range distinct {
			if sameAggCall(f, d) {
				continue scan
			}
		}
		distinct = append(distinct, f)
	}
	return distinct
}

// sameMinMaxCensus reports whether two censuses are the same call sites in the
// same order -- the comparison a guard that must not let a REWRITE move the
// anchor uses (compileScanGroupedWindow, vdbe_window_group.go).
func sameMinMaxCensus(a, b []FuncExpr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameAggCall(a[i], b[i]) {
			return false
		}
	}
	return true
}

// magnetPlan models SQLite's bare-column extension: which row of a group a
// groupBareColExpr (and a correlated subquery in the item) reads. C copies the
// row's columns into the accumulator registers conditionally on each row, under
// one shared "magnet" register (regHit, updateAccumulator); the last row copied
// is what the group reports. magnetWalk is that register:
//
//   - sites is the min()/max() census, each with its own accumulator; only these
//     ever set the magnet.
//   - allocS mirrors whether C allocated regAcc ("seen a row of this group"):
//     always with GROUP BY, and for a whole-table aggregate only when every
//     min/max has a FILTER. It makes an all-rejecting FILTER fall back to the
//     first row.
//
// Result: no min/max -> first row; FILTER rejecting everything -> first row; an
// all-NULL min/max -> last row.
type magnetPlan struct {
	sites  []*aggItem
	allocS bool
}

// magnetPlanFor builds stmt's magnetPlan, or nil when there is nothing to anchor.
// A planAggregateCall failure drops that site (not expected). hasGroupBy decides
// allocS together with the census, as C does: always with GROUP BY; otherwise
// only if every min/max is filtered. "SELECT max(b), min(a) FILTER (WHERE b<9), a,
// b FROM t0" is 9|1|2|9, and with "GROUP BY 1=1" 9|1|1|7.
func magnetPlanFor(stmt *SelectStmt, hasGroupBy bool) *magnetPlan {
	return magnetPlanForCalls(minMaxCensus(stmt), hasGroupBy)
}

// magnetPlanForCalls is magnetPlanFor over an already-built census -- the entry
// point the hoist reconciliation (honoredWithHoisted, vdbe_agg_hoist.go) uses.
func magnetPlanForCalls(census []FuncExpr, hasGroupBy bool) *magnetPlan {
	mp := &magnetPlan{allocS: true}
	for _, fc := range census {
		tmpl, err := planAggregateCall(fc)
		if err != nil {
			continue
		}
		mp.sites = append(mp.sites, tmpl)
		if !hasGroupBy && tmpl.filter == nil {
			mp.allocS = false
		}
	}
	return mp
}

// orderPlan is one ORDER BY term's plan in GROUP BY mode: either a 1-based
// ordinal reference to an output column (ordinal > 0, item == nil), or a
// GROUP-BY-shaped expression plan (ordinal == 0), exactly as query.go's
// row-mode ORDER BY resolves ordinals against the output row.
type orderPlan struct {
	ordinal int
	item    *itemPlan
	desc    bool
	nulls   NullsOrder
	// coll is this term's collating sequence ("" for BINARY), threaded into the
	// group-order sort -- sorter2's sorterKeyInfo.coll (compileScanGroupBy,
	// vdbe_agg_codegen.go) and groupBatchPlan.orderColl (vdbe_group_distinct.go).
	// It is sqlite3ExprCollSeq's answer for the term: an EXPLICIT COLLATE
	// anywhere in the written term (exprCollation, which -- like the C -- does
	// not stop at a function-call boundary), else the DECLARED collation of the
	// column an ordinal/alias/bare term resolves to. Both were DECLINES until
	// the sort could carry a collation at all; see r35dOrderTermCollation.
	coll string
}

// r35dOrderTermCollation is sqlite3ExprCollSeq's answer (expr.c:248) for one
// ORDER BY term of a GROUP BY query ("" is BINARY). raw is the term as written,
// stripped without a trailing COLLATE. An explicit COLLATE anywhere in the term
// wins (it propagates through function calls: "ORDER BY max(b COLLATE
// nocase)||''" is NOCASE); otherwise the declared collation of the column the term
// resolves to, through the same ordinal/alias shortcuts.
func r35dOrderTermCollation(planCtx *evalCtx, raw, stripped Expr, outCols []outputColumn) string {
	if n, ok := exprCollation(raw); ok {
		return n
	}
	target := raw
	if n, ok := orderByOrdinal(stripped); ok {
		if n >= 1 && int(n) <= len(outCols) {
			target = outCols[n-1].expr
		}
	} else if colRef, ok := stripped.(ColumnExpr); ok && colRef.Qualifier == "" {
		if idx := findOutputColByName(outCols, colRef.Name); idx >= 0 {
			target = outCols[idx].expr
		}
	}
	if n, ok := declaredColumnCollation(planCtx, target); ok {
		return n
	}
	return ""
}

// groupByPlan is the fully-resolved plan for a GROUP BY SELECT: the expanded
// select list, the (ordinal-resolved) GROUP BY key expressions and their
// affinities, and the rewritten item plans for the select list, HAVING, and
// ORDER BY (each carrying its own aggregate accumulator templates -- see
// itemPlan). It is produced once by planGroupByStmt and consumed by the VDBE's
// GROUP BY compiler (compileScanGroupBy, vdbe_agg_codegen.go), which is the
// only thing that executes a GROUP BY query.
type groupByPlan struct {
	outCols    []outputColumn
	groupExprs []Expr
	groupAffs  []affinity
	outPlans   []*itemPlan
	havingPlan *itemPlan // nil if there is no HAVING clause
	orderPlans []orderPlan
	planCtx    *evalCtx // schema-only (no row) context: tables/outer/pager/params

	// magnet/usesGroupBare support the bare-column extension: magnet is the census
	// and S flag, usesGroupBare is true if any item (select list, HAVING, ORDER BY)
	// rewrote a bare column. All share one anchor. compileScanGroupBy uses
	// usesGroupBare to decide whether the hash path is allowed.
	magnet        *magnetPlan
	usesGroupBare bool

	// selectHasSubquery is true if a select-list item contains a subquery outside
	// any aggregate argument (exprHasAnchorSubquery). A correlated one reads the
	// group's anchor row at finalize time, so, like usesGroupBare, it keeps the query
	// off the hash path.
	selectHasSubquery bool

	// anchorObservable is stmtReadsAnchorRow's verdict for this statement:
	// false when NOTHING in the select list, HAVING or ORDER BY reads a column
	// the GROUP BY key does not already fix -- subqueries walked into, and a
	// subquery's own tables counted (walkSelectForAnchor's over-approximation).
	// The anchor-order guard (compileScanGroupBy, vdbe_agg_codegen.go) reads it
	// for the same reason recordSpec's min()/max() census does: with nothing
	// able to observe WHICH row of the group the anchor is, the arrival order
	// need not be provable.
	anchorObservable bool

	// groupColls[i] is the collating sequence GROUP BY key i groups under --
	// "" for BINARY, otherwise the name topExprCollation resolved (explicit
	// "... COLLATE nocase", or the DECLARED collation of a bare column
	// reference). It is the grouping EQUALITY, not merely a sort order: it
	// decides which rows fall in one group. compileScanGroupBy threads it into
	// sorter1's key comparator (which both groups and orders) and into
	// OpGroupSame's boundary test, and refuses the hash-aggregate fast path
	// while any entry is non-BINARY. nil means every key is BINARY.
	groupColls []groupKeyCollation

	// groupDesc[i] is whether GROUP BY key i sorts descending. It comes from
	// sqlite3CopySortOrder's side effect (select.c:7528, called at 8396):
	//
	//	if( sqlite3CopySortOrder(pGroupBy, sSort.pOrderBy)
	//	 && sqlite3ExprListCompare(pGroupBy, sSort.pOrderBy, -1)==0 ){
	//	  orderByGrp = 1;
	//	}
	//
	// which copies each ORDER BY term's DESC onto the GROUP BY term at the same
	// position whenever the lists have the same length, without comparing them. It is
	// visible when ORDER BY has ties:
	//
	//	SELECT g FROM u GROUP BY g ORDER BY k        ->  1,2,3,4,5,6
	//	SELECT g FROM u GROUP BY g ORDER BY k DESC   ->  6,5,4,3,2,1
	//	SELECT g FROM u GROUP BY g,k ORDER BY k DESC ->  5,6,3,4,1,2  (2 vs 1
	//	                                                  terms: no copy)
	//
	// nil means all ascending.
	groupDesc []bool

	// outColls is each OUTPUT column's collating sequence ("" == BINARY),
	// which is what a SELECT DISTINCT over a GROUP BY dedups its finished
	// rows by: sqlite3Select builds the distinct index's KeyInfo from the
	// result expression list (sqlite3KeyInfoFromExprList(pParse, pEList,0,0)),
	// so it is the RESULT column's own collation that decides, never the
	// GROUP BY key's. Filled only when stmt.Distinct; nil otherwise.
	outColls []string
}

// groupKeyCollation is one GROUP BY key's collating sequence: the resolved
// name ("" == none, i.e. BINARY) and whether the key expression wrote it
// EXPLICITLY ("... COLLATE nocase") rather than inheriting it from a bare
// column's declaration. The explicit bit is the rank resolveCompareCollation
// needs; see groupKeyExpr.
type groupKeyCollation struct {
	name     string
	explicit bool
}

// collAt is groupColls[i], degrading a short/nil slice to "no collation" --
// the shape every caller that has no per-key collation at all (a whole-table
// aggregate's planGroupItem(e, nil, nil, nil, ...)) passes.
func collAt(colls []groupKeyCollation, i int) groupKeyCollation {
	if i < 0 || i >= len(colls) {
		return groupKeyCollation{}
	}
	return colls[i]
}

// collationNames projects groupColls onto the plain []string the sorter's key
// comparator and OpGroupSame take, or NIL when every key is BINARY -- which is
// what keeps an ordinary GROUP BY on exactly the comparator it has always used
// (see keysEqualGrouping).
func collationNames(colls []groupKeyCollation) []string {
	if !anyNonBinaryCollation(colls) {
		return nil
	}
	names := make([]string, len(colls))
	for i, c := range colls {
		names[i] = c.name
	}
	return names
}

// keysEqualGrouping is keysEqual with per-key collations. A BINARY (or unset) key
// keeps raw compareValues: compareValuesCollatedEnc compares TEXT by UTF-16 units
// in a UTF-16 database and maps every invalid UTF-8 sequence to U+FFFD, which
// would merge distinct malformed keys.
func keysEqualGrouping(a, b []Value, colls []string, enc TextEncoding) bool {
	if len(colls) == 0 {
		return keysEqual(a, b)
	}
	for i := range a {
		n := atOrEmpty(colls, i)
		if n == "" || equalFoldName(n, "BINARY") {
			if compareValues(a[i], b[i]) != 0 {
				return false
			}
			continue
		}
		if compareValuesCollatedEnc(a[i], b[i], n, enc) != 0 {
			return false
		}
	}
	return true
}

// anyNonBinaryCollation reports whether colls names a collating sequence other
// than BINARY for at least one key ("" is BINARY, the default).
func anyNonBinaryCollation(colls []groupKeyCollation) bool {
	for _, c := range colls {
		if c.name != "" && !equalFoldName(c.name, "BINARY") {
			return true
		}
	}
	return false
}

// planGroupByStmt resolves a GROUP BY SELECT's select-list/GROUP-BY/HAVING/
// ORDER-BY into a groupByPlan.
//
// outerCompiler is compileScanGroupBy's own c.outer (nil at any call site
// that has none on hand), threaded through to every planGroupItem call below
// so a select-list/HAVING/ORDER-BY item's own subquery-buried escaping
// aggregate can be escalated further out -- see recordSpec's own doc comment
// (sql_group.go).
func (p *ReadOnlyPager) planGroupByStmt(stmt *SelectStmt, scopes []tableScope, outer *evalCtx, params []Value, outerCompiler *compiler) (*groupByPlan, error) {
	outCols, err := expandSelectList(stmt.Columns, scopes, p.colNameMode())
	if err != nil {
		return nil, err
	}

	// stmt.GroupBy is always non-empty here (compileScanGroupBy's own
	// dispatch precondition, vdbe_scan.go), so selectIsAggregateQuery(stmt)
	// is always true -- computed explicitly rather than assumed, both for
	// planGroupItem's own aggregateOwnedHere doc (this IS the check it
	// names) and so this stays correct if that precondition ever changes.
	aggregateOwnedHere := selectIsAggregateQuery(stmt)

	// Resolve GROUP BY ordinals (a bare positive-integer literal referencing
	// an output column by position) to the referenced output expression,
	// exactly like ORDER BY's ordinal support in query.go -- but rejected if
	// that output expression itself contains an aggregate.
	groupExprs := make([]Expr, len(stmt.GroupBy))
	for i, ge := range stmt.GroupBy {
		if lit, ok := ge.(LiteralExpr); ok && lit.Val.Typ == Int {
			n := lit.Val.I
			if n < 1 || int(n) > len(outCols) {
				// C's own wording, ordinal and all: "%r GROUP BY term out of
				// range - should be between 1 and %d" (select.c's
				// resolveOrderGroupBy, the same message query.go already
				// produces on the read path). semanticf because it is a
				// prepare-time rejection C SQLite makes too -- as a decline
				// it reached the caller inside "VDBE-only: ... (no fallback)".
				return nil, semanticf("engine: %s GROUP BY term out of range - should be between 1 and %d", ordinalWord(i+1), len(outCols))
			}
			resolved := outCols[n-1].expr
			if containsAggregate(resolved) {
				return nil, fmt.Errorf("engine: unsupported: GROUP BY ordinal %d refers to an aggregate output column", n)
			}
			groupExprs[i] = resolved
			continue
		}
		if containsAggregate(ge) {
			return nil, fmt.Errorf("engine: unsupported: aggregate function in GROUP BY")
		}
		if err := checkExprSupported(ge); err != nil {
			return nil, err
		}
		groupExprs[i] = ge
	}

	// planCtx is built here (rather than after the collation checks below,
	// where an earlier version of this function built it) purely so those
	// checks -- which need a schema-only ctx to resolve a bare column's
	// DECLARED collation -- can share it; nothing between here and its
	// original use site (exprAffinity, just below) depends on the reorder.
	planCtx := &evalCtx{tables: scopes, outer: outer, pager: p, params: params}

	// A non-BINARY GROUP BY key collation (explicit, or declared on a bare column
	// reference) applies to grouping equality, to which value is reported and to the
	// group order:
	//
	//	SELECT count(*), a FROM t2 GROUP BY a                -> ONE group, (3,aBc)
	//	SELECT count(*), b FROM t2 GROUP BY b                -> three, (1,DEF)(1,DeF)(1,def)
	//	SELECT count(*), a FROM t2 GROUP BY a COLLATE binary -> three
	//	SELECT count(*), b FROM t2 GROUP BY b COLLATE nocase -> ONE, (3,DeF)
	//
	// The reported value is the group's first row in scan order and groups come out
	// in collated key order, which sorter1 gives once its comparator carries the
	// collation. Still declined: SELECT DISTINCT (groupBatchFinal dedups with
	// keysEqual) and a trailing ORDER BY (its blocker is gone, but lifting it needs
	// separate measurement and the group-order DESC tie-break is still wrong). A
	// bare reference to a key follows the anchor row (the GROUP-BY-KEY REFERENCE
	// RULE), so min()/max() needs no decline. Checked for every key, ordinals
	// included.
	// See groupPlan.groupDesc: ORDER BY's DESC bits land on the GROUP BY keys
	// positionally when the lists have the same length.
	var groupDesc []bool
	if len(stmt.OrderBy) == len(groupExprs) {
		for i := range groupExprs {
			if stmt.OrderBy[i].Desc {
				if groupDesc == nil {
					groupDesc = make([]bool, len(groupExprs))
				}
				groupDesc[i] = true
			}
		}
	}

	var groupColls []groupKeyCollation
	for _, ge := range groupExprs {
		n, ok := topExprCollation(planCtx, ge)
		if !ok {
			n = ""
		}
		_, explicit := exprCollation(ge)
		groupColls = append(groupColls, groupKeyCollation{name: n, explicit: explicit})
	}
	// SELECT DISTINCT over GROUP BY dedups finished rows by the result columns'
	// collations (sqlite3KeyInfoFromExprList on the result list), never the GROUP BY
	// key's:
	//
	//	SELECT DISTINCT a FROM t GROUP BY a COLLATE NOCASE  -> A, b
	//	SELECT DISTINCT a COLLATE NOCASE FROM t GROUP BY a  -> A, B
	// Whether anything could observe the anchor row (stmtReadsAnchorRow), computed
	// once for the whole statement: per-item checks would miss bare reads in HAVING
	// or ORDER BY.
	anchorObservable := stmtReadsAnchorRow(stmt, groupExprs, scopes)

	var outColls []string
	if stmt.Distinct {
		outColls = make([]string, len(outCols))
		for i, oc := range outCols {
			if n, ok := topExprCollation(planCtx, oc.expr); ok {
				outColls[i] = n
			}
		}
	}

	// Plan-time affinity of each GROUP BY expression (a bare column's own
	// affinity, a CAST's target affinity, or none for anything else),
	// preserved on groupKeyExpr placeholders so comparisons against a
	// grouped column elsewhere (e.g. in HAVING) still get SQLite's affinity
	// coercion. exprAffinity only consults tables' column metadata, not row
	// values, so this is safe to compute once here with no row context.
	groupAffs := make([]affinity, len(groupExprs))
	for i, ge := range groupExprs {
		groupAffs[i] = exprAffinity(planCtx, ge)
	}

	// The magnet is computed once, at the WHOLE-STATEMENT level: the census
	// spans the select list, ORDER BY and HAVING together (see minMaxCensus),
	// and every item's bare column resolves against the one anchor row it
	// produces.
	magnet := magnetPlanFor(stmt, true)

	outPlans := make([]*itemPlan, len(outCols))
	usesGroupBare := false
	selectHasSubquery := false
	for i, oc := range outCols {
		it, ierr := planGroupItem(oc.expr, groupExprs, groupAffs, groupColls, scopes, true, stmt.Columns, stmt.From, p, aggregateOwnedHere, outerCompiler, anchorObservable)
		if ierr != nil {
			return nil, ierr
		}
		outPlans[i] = it
		if it.usesGroupBare {
			usesGroupBare = true
		}
		if exprHasAnchorSubquery(oc.expr) {
			selectHasSubquery = true
		}
	}

	var havingPlan *itemPlan
	if stmt.Having != nil {
		// A bare column is allowed in HAVING and reads the same anchor row the select
		// list does:
		//
		//	HAVING v=50                   -> group 1  (anchor is the FIRST row)
		//	SELECT min(v) ... HAVING v=10 -> group 1  (anchor MOVED to min's row)
		havingPlan, err = planGroupItem(stmt.Having, groupExprs, groupAffs, groupColls, scopes, true, stmt.Columns, stmt.From, p, aggregateOwnedHere, outerCompiler, anchorObservable)
		if err != nil {
			return nil, err
		}
		if havingPlan.usesGroupBare {
			usesGroupBare = true
		}
	}

	orderPlans := make([]orderPlan, len(stmt.OrderBy))
	for i, ot := range stmt.OrderBy {
		// stripOrderCollate lets "ORDER BY 1 COLLATE BINARY" still be an ordinal, as
		// resolveCompoundOrderIndex does. The collation travels on orderPlan.coll (see
		// r35dOrderTermCollation).
		stripped := stripOrderCollate(ot.Expr)
		coll := r35dOrderTermCollation(planCtx, ot.Expr, stripped, outCols)
		// orderByOrdinal (query.go) also recognizes the signed forms
		// ("+2"/"-1") C SQLite treats as ordinal references too -- see
		// its doc comment.
		if n, ok := orderByOrdinal(stripped); ok {
			if n < 1 || int(n) > len(outCols) {
				return nil, semanticf("%s ORDER BY term out of range - should be between 1 and %d",
					sqliteOrdinalWord(i+1), len(outCols))
			}
			orderPlans[i] = orderPlan{ordinal: int(n), desc: ot.Desc, nulls: ot.Nulls, coll: coll}
			continue
		}
		// A bare name matching an output alias resolves to that output column first, as
		// C's ORDER BY precedence does: "SELECT a AS b FROM t3 ORDER BY b" sorts by the
		// alias even when t3 has its own column b.
		if colRef, ok := stripped.(ColumnExpr); ok && colRef.Qualifier == "" {
			if idx := findOutputColByName(outCols, colRef.Name); idx >= 0 {
				orderPlans[i] = orderPlan{ordinal: idx + 1, desc: ot.Desc, nulls: ot.Nulls, coll: coll}
				continue
			}
		}
		// A BARE COLUMN is allowed here too, on the same anchor as HAVING and
		// the select list -- "SELECT k FROM t GROUP BY k ORDER BY v" sorts the
		// groups by each one's anchor v, which C SQLite answers and this
		// used to decline.
		it, ierr := planGroupItem(ot.Expr, groupExprs, groupAffs, groupColls, scopes, true, stmt.Columns, stmt.From, p, aggregateOwnedHere, outerCompiler, anchorObservable)
		if ierr != nil {
			return nil, ierr
		}
		if it.usesGroupBare {
			usesGroupBare = true
		}
		orderPlans[i] = orderPlan{item: it, desc: ot.Desc, nulls: ot.Nulls, coll: coll}
	}

	gp := &groupByPlan{
		outCols:           outCols,
		groupExprs:        groupExprs,
		groupAffs:         groupAffs,
		outPlans:          outPlans,
		havingPlan:        havingPlan,
		orderPlans:        orderPlans,
		planCtx:           planCtx,
		magnet:            magnet,
		usesGroupBare:     usesGroupBare,
		selectHasSubquery: selectHasSubquery,
		anchorObservable:  anchorObservable,
		groupColls:        groupColls,
		groupDesc:         groupDesc,
		outColls:          outColls,
	}
	// C's aggregate association applies here as for a whole-table aggregate
	// (checkAggregateAssociation): an aggregate none of whose columns resolve to this
	// query's FROM belongs to the enclosing query. Without the check it was answered
	// once per outer row:
	//
	//	SELECT (SELECT sum(a1) FROM t2 GROUP BY b1) FROM t1
	//	SELECT (SELECT sum(b1) FROM t2 GROUP BY b1 HAVING sum(a1)>2) FROM t1
	//	SELECT (SELECT b1 FROM t2 GROUP BY b1 ORDER BY sum(a1)) FROM t1
	//
	// are one row each in C. All three clauses are checked.
	aggs, _ := groupPlanRefs(gp)
	if err := checkAggregateAssociation(aggs, scopes, outer, outerCompiler); err != nil {
		return nil, err
	}
	return gp, nil
}

// cloneAggTemplates instantiates a fresh set of per-group accumulators from an
// itemPlan's aggregate templates: one new *aggItem per template, copying its
// kind/expr/sepExpr/distinct but NOT its running state or its DISTINCT dedup
// set (each group -- or each whole-table aggregate run -- starts accumulating
// from scratch). Used by the VDBE's aggregate opcodes (OpAggReset,
// vdbe_agg.go), which feed these instances into the shared
// aggItem.step/finalize logic.
func cloneAggTemplates(templates []*aggItem) []*aggItem {
	if len(templates) == 0 {
		return nil
	}
	accs := make([]*aggItem, len(templates))
	for i, tmpl := range templates {
		// orderStrict, rowRegs and stepSeg are compile-time facts about the scan body
		// (armOrderSensitive; aggItem.rowRegs; aggItem.stepSeg), the same for every
		// group, so every clone inherits them.
		accs[i] = &aggItem{kind: tmpl.kind, expr: tmpl.expr, sepExpr: tmpl.sepExpr, distinct: tmpl.distinct, jsonb: tmpl.jsonb, filter: tmpl.filter,
			orderStrict: tmpl.orderStrict, rowRegs: tmpl.rowRegs, stepSeg: tmpl.stepSeg,
			minMaxLastWins: tmpl.minMaxLastWins,
			// The ORDER BY's compile-time half (aggItem.orderBy); the buffer
			// and the collations it resolves are per group.
			orderBy: tmpl.orderBy, obUnique: tmpl.obUnique, obRegs: tmpl.obRegs}
	}
	return accs
}

// findOutputColByName returns the index of the first output column whose
// name (its alias, or default name -- see expandSelectList) case-insensitively
// matches name, or -1 if none match. Used only for the ORDER BY result-column
// alias fallback above.
func findOutputColByName(outCols []outputColumn, name string) int {
	for i, oc := range outCols {
		if equalFoldName(oc.name, name) {
			return i
		}
	}
	return -1
}

// keyTupleLess orders two group-key tuples via compareValues, elementwise --
// used only to give GROUP-BY-without-ORDER-BY output a deterministic,
// reproducible row order.
func keyTupleLess(a, b []Value) bool {
	for i := range a {
		if c := compareValues(a[i], b[i]); c != 0 {
			return c < 0
		}
	}
	return false
}

// sqliteOrdinalWord renders n the way printf's "%r" conversion does in
// SQLite's own formatter -- "1st", "2nd", "3rd", "4th", with the 11th/12th/
// 13th exceptions -- which is how an out-of-range ORDER BY term names WHICH
// term it means: "1st ORDER BY term out of range - should be between 1 and 4".
func sqliteOrdinalWord(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// keywordLiteralNotInScope reports whether e is a bare TRUE/FALSE that cannot be
// a column of this query's FROM, so it reads no row. Without this, the anchor
// census counted cursorhint.test 6.0's "(SELECT true FROM t6 AS aa ...)" as a
// bare read. One-sided: if a scope has such a column it is still counted.
func keywordLiteralNotInScope(e ColumnExpr, scopes []tableScope) bool {
	if e.FallbackLiteral == nil || e.Qualifier != "" || e.Schema != "" {
		return false
	}
	lname := strings.ToLower(e.Name)
	for i := range scopes {
		if _, ok := scopes[i].colIndex[lname]; ok {
			return false
		}
	}
	return true
}

// stmtReadsAnchorRow reports whether anything in stmt could observe the anchor
// row: a bare column read outside any aggregate argument that is not exactly a
// GROUP BY key. Lets recordSpec skip its census gate when nothing could see the
// magnet. Over-approximates (a spurious true keeps the decline); walks the select
// list, HAVING and ORDER BY.
func stmtReadsAnchorRow(stmt *SelectStmt, groupExprs []Expr, scopes []tableScope) bool {
	isKey := func(c ColumnExpr) bool {
		for _, ge := range groupExprs {
			if k, ok := ge.(ColumnExpr); ok &&
				equalFoldName(k.Name, c.Name) && equalFoldName(k.Qualifier, c.Qualifier) {
				return true
			}
		}
		return false
	}
	found := false
	var walk func(e Expr)
	walk = func(e Expr) {
		if e == nil || found {
			return
		}
		switch x := e.(type) {
		case ColumnExpr:
			if !isKey(x) && !keywordLiteralNotInScope(x, scopes) {
				found = true
			}
			return
		case FuncExpr:
			if isAggregateFuncName(x.Name) {
				// An aggregate's ARGUMENTS are accumulated over every row of
				// the group, so they observe no single row. Its FILTER is the
				// same. Not descended into.
				return
			}
		case SubqueryExpr:
			walkSelectForAnchor(x.Stmt, walk)
			return
		case ExistsExpr:
			walkSelectForAnchor(x.Stmt, walk)
			return
		case InExpr:
			walk(x.X)
			for _, l := range x.List {
				walk(l)
			}
			walkSelectForAnchor(x.Sub, walk)
			return
		}
		walkExprOperands(e, walk)
	}
	for _, sc := range stmt.Columns {
		if sc.Star {
			return true // "*" expands to bare columns
		}
		walk(sc.Expr)
	}
	walk(stmt.Having)
	for _, ot := range stmt.OrderBy {
		walk(ot.Expr)
	}
	return found
}

// walkSelectForAnchor applies walk to every expression of a nested SELECT --
// its own FROM items' ON clauses included -- so a correlated reference to the
// enclosing row is not missed. A column of the subquery's OWN tables is
// counted too, which is part of the over-approximation.
func walkSelectForAnchor(s *SelectStmt, walk func(Expr)) {
	if s == nil {
		return
	}
	for _, sc := range s.Columns {
		walk(sc.Expr)
	}
	for _, it := range s.From {
		walk(it.On)
		walkSelectForAnchor(it.Subquery, walk)
	}
	walk(s.Where)
	walk(s.Having)
	for _, g := range s.GroupBy {
		walk(g)
	}
	for _, ot := range s.OrderBy {
		walk(ot.Expr)
	}
}
