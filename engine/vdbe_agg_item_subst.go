// Compile-time handling of a subquery written in an aggregate query's result
// expression. Every correlated reference in such a body to the aggregate
// query's own FROM becomes the groupBareColExpr placeholder rewriteGroupExpr
// (sql_group.go) produces for a bare column at the item's top level, and every
// hoisted aggregate call a groupAggExpr. The body then names nothing of the
// enclosing row and compiles once as a sub-Program, rather than being
// re-substituted per group (rewriteSelectOuterRefs, outer_ref.go).
//
// This is C's model. The aggregate analysis descends into subqueries:
//
//	expr.c:7570-7572   sqlite3ExprAnalyzeAggregates sets xExprCallback =
//	                   analyzeAggregate and walks with sqlite3WalkExpr;
//	walker.c:80-82     sqlite3WalkExprNN's ExprUseXSelect arm recurses into
//	                   pExpr->x.pSelect -- so the aggregate analysis DOES
//	                   enter a subquery body;
//	expr.c:7458-7469   analyzeAggregate's TK_COLUMN case converts any column
//	                   whose iTable is one of the AGGREGATE query's own
//	                   SrcList cursors via findOrCreateAggInfoColumn,
//	expr.c:7390-7392   whose fix_up_expr tail rewrites that node's op to
//	                   TK_AGG_COLUMN,
//	expr.c:4994-4996   which sqlite3ExprCodeTarget then answers with a bare
//	                   "return AggInfoColumnReg(pAggInfo, pExpr->iAgg);" --
//	                   no instruction at all, just the register.
//
// so such a reference reads the AggInfo register block, the group's anchor row.
// C has one flat program; here a body is a separate Program, so the
// placeholder is read across frames by OpOuterAggReg (OpOuterColumn's P5
// convention, pointed at the item program's registers).
//
// The materializing twin (rewriteExprOuterRefs) answers the same "local or
// enclosing?" question the same way, but per row with values; it may leave a
// node alone, this pass may not. Every arm here substitutes, provably leaves
// alone, or declines (aggItemSubst.ok false), the fail-safe.
//
// An unqualified reference is left for the item compiler to resolve: deciding
// whether an intervening body's FROM shadows it needs column lists, which this
// pass lacks (addFromScopeNames records names only). compileAggItemProgram
// publishes the aggregate query's FROM items as register scopes over the anchor
// row, so compileColumn's outward walk is lookupName ("if( cnt ) break;"
// before "pNC = pNC->pNext;", resolve.c:703-704) and lands on the same
// OpOuterAggReg read the qualified spelling gets (bodyColumn).
// TestAggItemProgramAnswers holds the two to the same answers.
//
// A placeholder is only answerable where the sub-compile reaches compileExpr's
// placeholder cases; elsewhere it would be read with no group (a panic, as
// window1.test#25's "SELECT (0,0) IN (SELECT MIN(c0), NTILE(1) OVER()) FROM t0"
// once was). One refused region remains (refusedRegion): an fts5 auxiliary
// function's arguments. Regions are retired only after the off-program reader
// they name is deleted. Window projections (lifted into batch columns,
// window.c:756-771, 802-819), inner aggregate queries' result clauses, plain
// and window aggregate arguments, and scope-less compound arms are served: a
// placeholder names its owner (groupKeyExpr.owner etc.), as C's
// pExpr->pAggInfo does (expr.c:4996, 5342; chosen by resolve.c:1355-1361), and
// aggResultReg walks past blocks that are not the owner's.
package engine

// refusedRegion is the set of refused regions (see the file doc) a node stands
// under; any substitution under a non-zero set declines. A set, so a decline
// census can say which region caused it.
type refusedRegion uint8

const (
	// regionFts5Aux is an fts5 auxiliary function's arguments. The separate
	// evaluator that justified it is gone (compileFts5Aux compiles each
	// argument into OpFts5Aux's argRegs), but retiring it needs a measured
	// count and an oracle gate, so it stays as a conservative decline.
	// Aggregate arguments are not regions: a plain one's register is
	// filled by emitAggArgRegs or the scan declines, and the sorted drain
	// lowers placeholders and anchor-row names as register reads
	// (aggDrainRow.lowerable); a window aggregate's is covered in bodyExpr.
	regionFts5Aux refusedRegion = 1 << iota
)

// aggItemSubst rewrites one itemPlan's subquery bodies against the aggregate
// query's own scopes. ok is the running verdict: any arm that cannot prove what
// a node means clears it, and the whole item is then left to its existing
// route rather than compiled to something that might not be the same thing.
type aggItemSubst struct {
	// owner is the aggregate query these placeholders belong to -- C's
	// pExpr->pAggInfo (see the owner note above groupKeyExpr, sql_group.go).
	// Stamped on every placeholder this pass builds, because every one of them
	// is substituted into a BODY, and a body may itself be an aggregate query
	// with a register block of its own that the index must not be read against.
	owner *aggPlan

	// scopes is the aggregate query's FROM clause -- what a reference has to
	// resolve against to BE a reference to the group's anchor row. probe is the
	// schema-only evalCtx those scopes are looked up through (no vals: this
	// runs at compile time, and nothing here reads a value).
	scopes []tableScope
	probe  *evalCtx

	// hoists are the aggregate calls written inside one of these bodies that
	// belong to the ENCLOSING query (vdbe_agg_hoist.go), matched by AST POINTER
	// against the body they were found in exactly as hoistSubsFor matches them.
	hoists []hoistedAggRef

	// anchorRegs reports that the item compiler will publish the aggregate
	// query's own FROM items as REGISTER-backed row scopes over the anchor-row
	// block (aggAnchorRegScopesUsable / compileAggItemProgram). When it does,
	// this pass no longer has to settle an UNQUALIFIED reference by name --
	// compileColumn's own chain walk does it, inner-first, which is the one
	// thing a name-only pass cannot do. See bodyColumn.
	anchorRegs bool

	ok bool
}

// rewriteAggItemBodies returns it.rewritten with every subquery body's
// references to the aggregate query's row replaced by placeholders, or (nil,
// false) when any part could not be settled. rewriteGroupExpr already handled
// the item's own level; only the bodies (SubqueryExpr.Stmt, ExistsExpr.Stmt,
// InExpr.Sub) remain. Nothing is mutated: an aggPlan rides in a cached Program
// shared by executions, so changed nodes are rebuilt.
func rewriteAggItemBodies(plan *aggPlan, it *itemPlan, anchorRegs bool) (Expr, bool) {
	s := &aggItemSubst{
		owner:      plan,
		scopes:     plan.scopes,
		probe:      &evalCtx{tables: plan.scopes},
		hoists:     it.hoisted,
		anchorRegs: anchorRegs,
		ok:         true,
	}
	out := s.expr(it.rewritten)
	if !s.ok {
		return nil, false
	}
	return out, true
}

// expr walks the ITEM's own tree looking for the three body-carrying nodes. It
// is not a substitution pass: at this level rewriteGroupExpr has already done
// the work, so every node is passed through unchanged except the bodies.
func (s *aggItemSubst) expr(e Expr) Expr {
	if !s.ok {
		return e
	}
	switch x := e.(type) {
	case SubqueryExpr:
		x.Stmt = s.selectStmt(x.Stmt, map[string]bool{}, false, 0)
		return x
	case ExistsExpr:
		x.Stmt = s.selectStmt(x.Stmt, map[string]bool{}, false, 0)
		return x
	case InExpr:
		x.X = s.expr(x.X)
		if x.List != nil {
			list := make([]Expr, len(x.List))
			for i, li := range x.List {
				list[i] = s.expr(li)
			}
			x.List = list
		}
		if x.Sub != nil {
			x.Sub = s.selectStmt(x.Sub, map[string]bool{}, false, 0)
		}
		return x
	case UnaryExpr:
		x.X = s.expr(x.X)
		return x
	case IsNullExpr:
		x.X = s.expr(x.X)
		return x
	case CollateExpr:
		x.X = s.expr(x.X)
		return x
	case CastExpr:
		x.X = s.expr(x.X)
		return x
	case BinaryExpr:
		x.L = s.expr(x.L)
		x.R = s.expr(x.R)
		return x
	case BetweenExpr:
		x.X, x.Lo, x.Hi = s.expr(x.X), s.expr(x.Lo), s.expr(x.Hi)
		return x
	case LikeExpr:
		x.X, x.Pattern = s.expr(x.X), s.expr(x.Pattern)
		if x.Escape != nil {
			x.Escape = s.expr(x.Escape)
		}
		return x
	case GlobExpr:
		x.X, x.Pattern = s.expr(x.X), s.expr(x.Pattern)
		return x
	case RowExpr:
		elems := make([]Expr, len(x.Elems))
		for i, el := range x.Elems {
			elems[i] = s.expr(el)
		}
		x.Elems = elems
		return x
	case CaseExpr:
		if x.Base != nil {
			x.Base = s.expr(x.Base)
		}
		if x.Whens != nil {
			whens := make([]WhenClause, len(x.Whens))
			for i, w := range x.Whens {
				whens[i] = WhenClause{When: s.expr(w.When), Then: s.expr(w.Then)}
			}
			x.Whens = whens
		}
		if x.Else != nil {
			x.Else = s.expr(x.Else)
		}
		return x
	case FuncExpr:
		if x.Args != nil {
			args := make([]Expr, len(x.Args))
			for i, a := range x.Args {
				args[i] = s.expr(a)
			}
			x.Args = args
		}
		if len(x.OrderBy) > 0 {
			ob := make([]OrderTerm, len(x.OrderBy))
			for i, t := range x.OrderBy {
				t.Expr = s.expr(t.Expr)
				ob[i] = t
			}
			x.OrderBy = ob
		}
		return x
	}
	return e
}

// selectStmt is rewriteSelectOuterRefs' compile-time twin, arm for arm: the
// same accumulated FROM-scope set, the same "a compound arm and a derived
// table restart from the PRE-this-level set because SQLite has no LATERAL",
// the same untouched WITH list, and the same set of rewritten clauses.
// compoundArm and the emptyArmScope it feeds mean exactly what they mean
// there.
func (s *aggItemSubst) selectStmt(stmt *SelectStmt, local map[string]bool, compoundArm bool, refused refusedRegion) *SelectStmt {
	if !s.ok || stmt == nil {
		return stmt
	}
	newLocal := make(map[string]bool, len(local)+len(stmt.From))
	for k := range local {
		newLocal[k] = true
	}
	addFromScopeNames(stmt.From, newLocal)

	// emptyArmScope: rewriteSelectOuterRefs' own flag, and the one context
	// where the materializing twin substitutes an UNQUALIFIED reference -- a
	// compound arm with no scope of its own has nothing local a bare name could
	// bind to (window4.test 12.3's "min(a) OVER ()"). It is DECLINED here
	// rather than reproduced, but only where it BITES: the decline is taken in
	// bodyColumn, when such an arm actually contains a bare name, so an arm
	// like "UNION ALL SELECT 9" -- which has no column reference to get wrong
	// -- still compiles. Reproducing the substitution instead would need the
	// arm's own resolution to have failed first, which is the one thing this
	// compile-time pass cannot see.
	isCompoundMember := compoundArm || len(stmt.Compound) > 0
	emptyArmScope := isCompoundMember && len(local) == 0 && len(stmt.From) == 0

	subs := s.hoistsFor(stmt)

	// A body that is itself an aggregate query: its result clauses become
	// its own itemPlans, compiled against its own register block. A
	// placeholder names its owner (C's pExpr->pAggInfo, expr.c:4996/5342,
	// chosen by resolve.c:1355-1361) and aggResultReg walks past blocks that
	// are not it, so an outer accumulator index is never read against inner
	// accumulators; and an inner item that does not compile is an error
	// (vdbe_agg.go). rewriteGroupExpr passes the foreign placeholder through
	// as a constant (the enclosing group's finalized value), and the inner
	// item compiler emits OpOuterAggReg one frame further out. WHERE and ON
	// are compiled by the body's own scan compiler, with no block of its
	// own, so a placeholder there walks out to the owner.
	newStmt := *stmt
	if stmt.Where != nil {
		newStmt.Where = s.bodyExpr(stmt.Where, newLocal, subs, emptyArmScope, refused)
	}
	if stmt.Columns != nil {
		cols := make([]SelectColumn, len(stmt.Columns))
		for i, c := range stmt.Columns {
			c.Expr = s.bodyExpr(c.Expr, newLocal, subs, emptyArmScope, refused)
			cols[i] = c
		}
		newStmt.Columns = cols
	}
	if stmt.GroupBy != nil {
		gb := make([]Expr, len(stmt.GroupBy))
		for i, g := range stmt.GroupBy {
			gb[i] = keepUnlessOrdinal(g, s.bodyExpr(g, newLocal, subs, emptyArmScope, refused))
		}
		newStmt.GroupBy = gb
	}
	if stmt.Having != nil {
		newStmt.Having = s.bodyExpr(stmt.Having, newLocal, subs, emptyArmScope, refused)
	}
	if stmt.OrderBy != nil {
		ob := make([]OrderTerm, len(stmt.OrderBy))
		for i, o := range stmt.OrderBy {
			o.Expr = keepUnlessOrdinal(o.Expr, s.bodyExpr(o.Expr, newLocal, subs, emptyArmScope, refused))
			ob[i] = o
		}
		newStmt.OrderBy = ob
	}
	if stmt.From != nil {
		items := make([]FromItem, len(stmt.From))
		for i, fi := range stmt.From {
			if fi.On != nil {
				fi.On = s.bodyExpr(fi.On, newLocal, subs, emptyArmScope, refused)
			}
			if fi.Subquery != nil {
				fi.Subquery = s.selectStmt(fi.Subquery, local, false, refused)
			}
			items[i] = fi
		}
		newStmt.From = items
	}
	if stmt.Compound != nil {
		arms := make([]CompoundArm, len(stmt.Compound))
		for i, a := range stmt.Compound {
			a.Stmt = s.selectStmt(a.Stmt, local, true, refused)
			arms[i] = a
		}
		newStmt.Compound = arms
	}
	return &newStmt
}

// hoistsFor is hoistSubsFor's compile-time twin: the hoisted calls recorded
// against THIS EXACT body, matched by AST pointer so a textually identical
// call at any other level is never touched.
func (s *aggItemSubst) hoistsFor(stmt *SelectStmt) []hoistedAggRef {
	var out []hoistedAggRef
	for _, h := range s.hoists {
		if h.body == stmt {
			out = append(out, h)
		}
	}
	return out
}

// bodyExpr rewrites one expression written INSIDE a body: a reference naming
// the aggregate query's own row becomes a placeholder, a hoisted aggregate call
// becomes the groupAggExpr standing for its already-finalized value, and
// anything whose meaning this pass cannot settle declines.
func (s *aggItemSubst) bodyExpr(e Expr, local map[string]bool, subs []hoistedAggRef, emptyArmScope bool, refused refusedRegion) Expr {
	if !s.ok {
		return e
	}
	// A HOISTED aggregate call, matched exactly as rewriteExprOuterRefs matches
	// it (sameAggCall against the recorded call, or -- for a reference to an
	// outer select-list ALIAS whose expression is the aggregate, which
	// resolve.c:69-101's resolveAlias splices in before there is ever a call
	// written here -- by that alias's NAME). Its value belongs to the enclosing
	// query and is finalized before the body runs, so the placeholder standing
	// for the finalized accumulator is exactly the substitution.
	if len(subs) > 0 {
		if fc, isFunc := e.(FuncExpr); isFunc {
			for _, h := range subs {
				if h.aliasName == "" && sameAggCall(fc, h.call) {
					return s.placeholder(groupAggExpr{accIdx: h.accIdx, owner: s.owner}, e, refused)
				}
			}
		}
		if col, isCol := e.(ColumnExpr); isCol && col.Qualifier == "" {
			for _, h := range subs {
				if h.aliasName != "" && equalFoldName(h.aliasName, col.Name) {
					return s.placeholder(groupAggExpr{accIdx: h.accIdx, owner: s.owner}, e, refused)
				}
			}
		}
	}
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr, groupAggExpr, groupKeyExpr, groupBareColExpr:
		return e
	case ColumnExpr:
		return s.bodyColumn(x, local, emptyArmScope, refused)
	case FuncExpr:
		// An aggregate call whose arguments name an ENCLOSING table but none of
		// this body's own belongs to the enclosing query, not here -- ported
		// from resolve.c's TK_FUNCTION walk (see rewriteExprOuterRefs' own arm
		// for the C and for the writes that substituting it wrongly performed).
		// The materializing twin leaves such a call ALONE, which leaves its
		// argument unresolvable and produces a decline; declining outright is
		// that same answer reached one step earlier. A hoisted call never gets
		// here -- it was substituted above.
		if isAggregateCall(x) && !aggCallHasLocalColumnArg(x, local) &&
			aggCallHasLocalColumnArg(x, outerTableScopeNames(s.probe)) {
			s.ok = false
			return e
		}
		// An fts5 auxiliary function's arguments are a refused region (see
		// regionFts5Aux).
		//
		// A plain aggregate's argument is not: the sorted drain lowers a
		// placeholder or an anchor-row bare name as a register read
		// (aggDrainRow.lowerable; TestAggItemAggArgRegion).
		//
		// A window aggregate's argument / FILTER / separator is not either:
		// aggItem.rowValue reads a register or errors, so an operand that loses
		// its column ends at windowAggOperandsServed's compile error while this
		// body is compiled (planWindowOperands may drop an argument or FILTER
		// added non-mandatory; the SUBTYPE aggregates are re-coded at step time,
		// pWin->bExprArgs, window.c:1041-1044, by compileWindowStepArgs, which
		// cannot reach the item's registers). Either way compileAggItemProgram
		// gets an error, never a placeholder read without its block
		// (compat-harness/agg_item_window_agg_arg_test.go).
		inner := refused
		if fts5AuxFuncName(x.Name) {
			inner |= regionFts5Aux
		}
		if x.Args != nil {
			args := make([]Expr, len(x.Args))
			for i, a := range x.Args {
				args[i] = s.bodyExpr(a, local, subs, emptyArmScope, inner)
			}
			x.Args = args
		}
		if x.Filter != nil {
			x.Filter = s.bodyExpr(x.Filter, local, subs, emptyArmScope, inner)
		}
		if len(x.OrderBy) > 0 {
			ob := make([]OrderTerm, len(x.OrderBy))
			for i, t := range x.OrderBy {
				t.Expr = s.bodyExpr(t.Expr, local, subs, emptyArmScope, inner)
				ob[i] = t
			}
			x.OrderBy = ob
		}
		if x.Over != nil {
			// A window spec's own PARTITION BY / ORDER BY operands are NOT a
			// region of their own: planWindowOperands lowers each into a batch
			// column and emitWindowOperands compiles it in the window query's
			// SCAN compiler (vdbe_window_codegen.go), whose chain reaches the
			// item's register block -- so a placeholder there is an ordinary
			// OpOuterAggReg read. They carry the region the CALL stands
			// under, NOT the argument region computed above: a spec key is
			// added with mustLower=TRUE,
			// so a failure to compile it is planWindowOperands' hard "refused"
			// and never a silently dropped column, which is what makes an
			// argument dangerous and a key not.
			spec := *x.Over
			if spec.PartitionBy != nil {
				parts := make([]Expr, len(spec.PartitionBy))
				for i, p := range spec.PartitionBy {
					parts[i] = s.bodyExpr(p, local, subs, emptyArmScope, refused)
				}
				spec.PartitionBy = parts
			}
			if spec.OrderBy != nil {
				terms := make([]OrderTerm, len(spec.OrderBy))
				copy(terms, spec.OrderBy)
				for i := range terms {
					terms[i].Expr = s.bodyExpr(terms[i].Expr, local, subs, emptyArmScope, refused)
				}
				spec.OrderBy = terms
			}
			x.Over = &spec
		}
		return x
	case UnaryExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		return x
	case BinaryExpr:
		x.L = s.bodyExpr(x.L, local, subs, emptyArmScope, refused)
		x.R = s.bodyExpr(x.R, local, subs, emptyArmScope, refused)
		return x
	case IsNullExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		return x
	case CollateExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		return x
	case CastExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		return x
	case BetweenExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		x.Lo = s.bodyExpr(x.Lo, local, subs, emptyArmScope, refused)
		x.Hi = s.bodyExpr(x.Hi, local, subs, emptyArmScope, refused)
		return x
	case LikeExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		x.Pattern = s.bodyExpr(x.Pattern, local, subs, emptyArmScope, refused)
		if x.Escape != nil {
			x.Escape = s.bodyExpr(x.Escape, local, subs, emptyArmScope, refused)
		}
		return x
	case GlobExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		x.Pattern = s.bodyExpr(x.Pattern, local, subs, emptyArmScope, refused)
		return x
	case RowExpr:
		elems := make([]Expr, len(x.Elems))
		for i, el := range x.Elems {
			elems[i] = s.bodyExpr(el, local, subs, emptyArmScope, refused)
		}
		x.Elems = elems
		return x
	case CaseExpr:
		if x.Base != nil {
			x.Base = s.bodyExpr(x.Base, local, subs, emptyArmScope, refused)
		}
		if x.Whens != nil {
			whens := make([]WhenClause, len(x.Whens))
			for i, w := range x.Whens {
				whens[i] = WhenClause{
					When: s.bodyExpr(w.When, local, subs, emptyArmScope, refused),
					Then: s.bodyExpr(w.Then, local, subs, emptyArmScope, refused),
				}
			}
			x.Whens = whens
		}
		if x.Else != nil {
			x.Else = s.bodyExpr(x.Else, local, subs, emptyArmScope, refused)
		}
		return x
	case InExpr:
		x.X = s.bodyExpr(x.X, local, subs, emptyArmScope, refused)
		if x.List != nil {
			list := make([]Expr, len(x.List))
			for i, li := range x.List {
				list[i] = s.bodyExpr(li, local, subs, emptyArmScope, refused)
			}
			x.List = list
		}
		if x.Sub != nil {
			x.Sub = s.selectStmt(x.Sub, local, false, refused)
		}
		return x
	case SubqueryExpr:
		x.Stmt = s.selectStmt(x.Stmt, local, false, refused)
		return x
	case ExistsExpr:
		x.Stmt = s.selectStmt(x.Stmt, local, false, refused)
		return x
	}
	// A node kind this pass does not model. Unlike the materializing twin's
	// "default: return e", leaving it alone here is not safe -- it would be
	// COMPILED, and if it contained a reference to the enclosing row that
	// reference would resolve to something else. Decline instead.
	s.ok = false
	return e
}

// bodyColumn settles one column reference inside a body, with the same tests
// as rewriteExprOuterRefs:
//
//   - a qualified reference naming one of the body's (or an enclosing body's)
//     FROM items is local, left for the sub-compile;
//   - an unqualified reference is left as written; the sub-compile resolves it
//     against the aggregate query's FROM items as registers over the anchor
//     row (compileAggItemProgram), the only place with the column lists to
//     apply lookupName's inner-first walk (see the arm for its two declines);
//   - a qualified reference resolving against the aggregate query's scopes is
//     the anchor row and becomes the groupBareColExpr rewriteGroupExpr builds,
//     carrying the column's affinity and declared collation as the twin's
//     LiteralExpr does.
//
// A qualified reference resolving nowhere is left alone (the sub-compile
// reports "no such column"). The rowid pseudo-column resolves via
// rowidColumnInfo with a non-negative rowidTableIdx and gets groupBareColExpr's
// isRowid form (see the rowid arm).
func (s *aggItemSubst) bodyColumn(x ColumnExpr, local map[string]bool, emptyArmScope bool, refused refusedRegion) Expr {
	if x.Qualifier == "" {
		// First ask whether the aggregate query itself has a column of this
		// name. If not, it cannot refer to the group: leave it alone, as the
		// qualified arm does for a qualifier naming no scope here.
		// unqualifiedColumnNotFoundErr is the exact "found nothing" test;
		// "ambiguous column name" means the query does have the name. (Testing
		// "err != nil || col != nil" was true for every name and declined e.g.
		// "(SELECT sum(w) FROM s2 WHERE ...)".)
		if _, _, _, _, perr := resolveColumn(s.probe, "", x.Name); unqualifiedColumnNotFoundErr(perr, x.Name) {
			return x
		}
		// A bare name the aggregate query's FROM answers must not be left to a
		// compile that skips this query's level: the item compiler's chain drops
		// it ("outer, rowOuter = enclosing.outer, enclosing.outerRowCtx()"), so
		// the name would bind in the query enclosing this one, which lookupName
		// never does (resolve.c:703-704):
		//
		//	SELECT (SELECT (SELECT count(*) FROM u WHERE u.k > x)
		//	          FROM a JOIN b USING(x) GROUP BY y) FROM o
		//	  oracle 2 (x bound to a/b, not o.x)
		//	SELECT (SELECT (SELECT count(*) FROM u WHERE u.k > c)
		//	          FROM t1,t2 GROUP BY g) FROM o2
		//	  oracle "ambiguous column name: c"
		//
		// With anchorRegs, compileAggItemProgram publishes this query's FROM
		// items as register scopes over the anchor row, so the body's
		// compileColumn walks outward one level at a time and stops at the first
		// level that answers; reaching this query's scopes, resolveRowReg emits
		// the OpOuterAggReg read (or raises "ambiguous column name",
		// resolve.c:784).
		//
		// Declined otherwise:
		//   - under a refused region, where the reference would be read with no
		//     group;
		//   - when the scopes could not be published (aggAnchorRegScopesUsable:
		//     USING/NATURAL, RIGHT/FULL, or a non-contiguous layout), since the
		//     walk would then skip the owning query.
		if !s.anchorRegs || refused != 0 {
			s.ok = false
		}
		return x
	}
	if local[r33sFoldIdent(x.Qualifier)] {
		return x
	}
	_, idx, col, rowidTableIdx, err := resolveColumn(s.probe, x.Qualifier, x.Name)
	if err != nil || col == nil {
		return x
	}
	// A pseudo-rowid is not an ordinary column: resolveColumn answers
	// "t.rowid" with rowidColumnInfo and a non-negative rowidTableIdx, and
	// groupBareColExpr{idx: idx} would read a column (non-NULL even on a LEFT
	// JOIN's NULL-extended side). It gets groupBareColExpr's isRowid form,
	// indexing the anchor row's trailing rowid block (aggResultReg(aggRegRowid,
	// ...); the twin reads ctx.rowids[rowidTableIdx]).
	//
	// It carries INTEGER affinity, as the twin's LiteralExpr{aff: col.Aff} does
	// (rowidColumnInfo.Aff is affInteger):
	//
	//	expr.c:24  char sqlite3TableColumnAffinity(const Table *pTab, int iCol){
	//	expr.c:25    if( iCol<0 || NEVER(iCol>=pTab->nCol) ) return SQLITE_AFF_INTEGER;
	//
	// reached through sqlite3ExprAffinity's TK_COLUMN / TK_AGG_COLUMN arm
	// (expr.c:49-52), iCol<0 being the rowid (resolve.c:564/567). Over ta(x TEXT,
	// a INTEGER) with rowid 1 -> ('1',10), rowid 2 -> ('02',20), "SELECT a,
	// sum(a), (SELECT ta.rowid = '1') FROM ta GROUP BY a" is 1 then 0
	// (attack_row_scope_rowid_test.go). It has no collation: sqlite3ExprCollSeq
	// uses a column's only when iColumn>=0 (expr.c:261).
	if rowidTableIdx >= 0 {
		return s.placeholder(groupBareColExpr{isRowid: true, rowidTableIdx: rowidTableIdx, aff: affInteger, owner: s.owner}, x, refused)
	}
	// NoAffinity travels ON the placeholder (groupBareColExpr.noAff), which is
	// what isMaterializedRef reads. This used to DECLINE instead: the node
	// answered "materialized" unconditionally while the materializing twin
	// substituted a literal carrying materialized = !NoAffinity, so a COMPUTED
	// derived-table column would have defended its storage class here and not
	// there. With the twin gone there is one answer to give, and it is the
	// column's own -- the same bit isMaterializedRef's ColumnExpr case reads,
	// with the same oracle behind it (view.test 27.*).
	return s.placeholder(groupBareColExpr{idx: idx, aff: col.Aff, coll: effectiveCollation(col.Collation), noAff: col.NoAffinity, owner: s.owner}, x, refused)
}

// placeholder returns ph, or DECLINES when this position stands under a
// REFUSED REGION (see this file's header): a placeholder there would be read
// against a context that has no group, which is a panic or a wrong answer, not
// a decline. orig is returned unchanged on the decline so the tree stays
// well-formed for whatever inspects it on the way out.
func (s *aggItemSubst) placeholder(ph, orig Expr, refused refusedRegion) Expr {
	if refused != 0 {
		s.ok = false
		return orig
	}
	return ph
}

// selectLevelIsAggregate reports whether stmt's OWN result clauses will be
// planned as an aggregate query's itemPlans -- a GROUP BY, or an aggregate call
// standing in the select list, HAVING or ORDER BY. Nested bodies are not this
// level's business; each is asked the same question when the walk reaches it.
//
// A hoisted call is NOT counted, and the order matters: hoistsFor's entries are
// calls that belong to the ENCLOSING query and are replaced by a placeholder
// before this body is ever planned, so "(SELECT count(v))" is not an aggregate
// query by the time it compiles -- which is what lets the hoist substitution
// keep working.
func selectLevelIsAggregate(stmt *SelectStmt, subs []hoistedAggRef) bool {
	if stmt == nil {
		return false
	}
	if len(stmt.GroupBy) != 0 {
		return true
	}
	found := false
	var walk func(Expr)
	walk = func(e Expr) {
		if found || e == nil {
			return
		}
		if fc, ok := e.(FuncExpr); ok && isAggregateCall(fc) {
			for _, h := range subs {
				if h.aliasName == "" && sameAggCall(fc, h.call) {
					return
				}
			}
			found = true
			return
		}
		walkExprOperands(e, walk)
	}
	for _, c := range stmt.Columns {
		walk(c.Expr)
	}
	walk(stmt.Having)
	for _, o := range stmt.OrderBy {
		walk(o.Expr)
	}
	return found
}

// walkExprOperands calls fn on each direct operand of e that can hold an
// ordinary expression, WITHOUT entering a subquery body -- the shape both
// selectLevelIsAggregate needs, since it is a
// question about ONE level.
func walkExprOperands(e Expr, fn func(Expr)) {
	switch x := e.(type) {
	case FuncExpr:
		for _, a := range x.Args {
			fn(a)
		}
		fn(x.Filter)
		for _, ob := range x.orderByExprs() {
			fn(ob)
		}
	case UnaryExpr:
		fn(x.X)
	case IsNullExpr:
		fn(x.X)
	case CollateExpr:
		fn(x.X)
	case CastExpr:
		fn(x.X)
	case BinaryExpr:
		fn(x.L)
		fn(x.R)
	case BetweenExpr:
		fn(x.X)
		fn(x.Lo)
		fn(x.Hi)
	case LikeExpr:
		fn(x.X)
		fn(x.Pattern)
		fn(x.Escape)
	case GlobExpr:
		fn(x.X)
		fn(x.Pattern)
	case RowExpr:
		for _, el := range x.Elems {
			fn(el)
		}
	case InExpr:
		fn(x.X)
		for _, li := range x.List {
			fn(li)
		}
	case CaseExpr:
		fn(x.Base)
		for _, w := range x.Whens {
			fn(w.When)
			fn(w.Then)
		}
		fn(x.Else)
	}
}
