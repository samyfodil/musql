// Correlation rewrites: turning a subquery's references to an ENCLOSING
// query's row into something the enclosing compile can supply.
//
// A correlated body cannot be compiled in isolation, because the names it
// carries belong to a query one or more levels out. These rewrites resolve
// each such reference against the outer scopes and replace it in place, which
// is the AST-level counterpart of what SQLite does by binding an expression to
// its NameContext once and never re-resolving (resolve.c's lookupName, and
// selectExpander's single bind at select.c:6000).
//
// This is a rewrite over the tree BEFORE code generation, not an evaluation of
// it. Nothing here produces a value.
package engine

// rewriteSelectOuterRefs copies stmt (never mutating it: the parsed node is
// reused for every row the subquery runs for) and rewrites every reachable
// expression (WHERE, select list, GROUP BY, HAVING, ORDER BY, each FROM item's
// ON) via rewriteExprOuterRefs. local accumulates the FROM-item names of every
// level descended into, so a qualifier shadowed anywhere in between is never
// taken for an outer reference. A compound arm and a FROM item's derived
// table recurse with the pre-this-level local set: each has its own FROM, and
// a derived table cannot see its siblings (no LATERAL) but can see everything
// enclosing the query. A CTE body is left untouched (it then declines).
//
// compoundArm is true only when stmt is itself a compound arm; with
// isCompoundMember (which also covers a compound's first arm) it sets
// emptyArmScope, the one context where an unqualified reference is
// substituted with the outer anchor row's value: window4.test 12.3's "min(a)
// OVER ()". A window function's argument never goes through aggregate
// association (resolve.c's TK_AGG_FUNCTION arm: "if (pWin) {...} else
// {...sqlite3ReferencesSrcList...}"), so ordinary lookupName -- this
// substitution -- is what resolves it. Other recursive entries pass false.
func rewriteSelectOuterRefs(stmt *SelectStmt, outer *evalCtx, local map[string]bool, changed *bool, compoundArm bool) *SelectStmt {
	if stmt == nil {
		return nil
	}
	newLocal := make(map[string]bool, len(local)+len(stmt.From))
	for k := range local {
		newLocal[k] = true
	}
	addFromScopeNames(stmt.From, newLocal)

	// emptyArmScope: see this function's own doc above, and
	// materializedOuterRefInAggArgArm's identical isCompoundMember/empty-scope
	// pairing (sql_group.go) for the AGGREGATE-HOIST half of this same
	// statement's fix. len(stmt.From)==0 is checked directly (not
	// len(newLocal)==0): an UNALIASED derived-table FROM item contributes no
	// name to newLocal (addFromScopeNames skips it) even though it DOES supply
	// columns an unqualified reference could resolve to locally.
	isCompoundMember := compoundArm || len(stmt.Compound) > 0
	emptyArmScope := isCompoundMember && len(local) == 0 && len(stmt.From) == 0

	// Aggregate calls written HERE that belong to the enclosing aggregate query
	// (vdbe_agg_hoist.go). Matched by AST pointer against stmt itself, so only
	// this exact body's own expressions are substituted -- a textually identical
	// call at any other level computes its own value, as it must.
	subs := hoistSubsFor(outer, stmt)

	newStmt := *stmt
	if stmt.Where != nil {
		newStmt.Where = rewriteExprOuterRefs(stmt.Where, outer, newLocal, subs, changed, emptyArmScope)
	}
	if stmt.Columns != nil {
		cols := make([]SelectColumn, len(stmt.Columns))
		for i, c := range stmt.Columns {
			c.Expr = rewriteExprOuterRefs(c.Expr, outer, newLocal, subs, changed, emptyArmScope)
			cols[i] = c
		}
		newStmt.Columns = cols
	}
	if stmt.GroupBy != nil {
		gb := make([]Expr, len(stmt.GroupBy))
		for i, g := range stmt.GroupBy {
			gb[i] = keepUnlessOrdinal(g, rewriteExprOuterRefs(g, outer, newLocal, subs, changed, emptyArmScope))
		}
		newStmt.GroupBy = gb
	}
	if stmt.Having != nil {
		newStmt.Having = rewriteExprOuterRefs(stmt.Having, outer, newLocal, subs, changed, emptyArmScope)
	}
	if stmt.OrderBy != nil {
		ob := make([]OrderTerm, len(stmt.OrderBy))
		for i, o := range stmt.OrderBy {
			o.Expr = keepUnlessOrdinal(o.Expr, rewriteExprOuterRefs(o.Expr, outer, newLocal, subs, changed, emptyArmScope))
			ob[i] = o
		}
		newStmt.OrderBy = ob
	}
	if stmt.From != nil {
		items := make([]FromItem, len(stmt.From))
		for i, it := range stmt.From {
			if it.On != nil {
				it.On = rewriteExprOuterRefs(it.On, outer, newLocal, subs, changed, emptyArmScope)
			}
			if it.Subquery != nil {
				// A derived table's body gets the pre-this-level local set, as a
				// compound arm does: it cannot see sibling FROM items (no LATERAL),
				// only the enclosing query's scopes. "SELECT b, (SELECT c FROM
				// (SELECT t2.b AS c FROM t1 LIMIT 1)) FROM t2" sees each t2 row, while
				// "SELECT c FROM (SELECT t2.b AS c FROM t1), t2" is "no such column:
				// t2.b".
				it.Subquery = rewriteSelectOuterRefs(it.Subquery, outer, local, changed, false)
			}
			items[i] = it
		}
		newStmt.From = items
	}
	if stmt.Compound != nil {
		arms := make([]CompoundArm, len(stmt.Compound))
		for i, a := range stmt.Compound {
			a.Stmt = rewriteSelectOuterRefs(a.Stmt, outer, local, changed, true)
			arms[i] = a
		}
		newStmt.Compound = arms
	}
	return &newStmt
}

// keepUnlessOrdinal guards one hazard: a bare integer ORDER BY or GROUP BY term
// is an ordinal, so a correlated reference that becomes one changes the
// clause's meaning. "SELECT k, (SELECT x FROM it ORDER BY ot.a LIMIT 1) FROM
// ot GROUP BY k" with ot.a = 1 orders by a constant in C, but "ORDER BY 1"
// would sort by x. The term keeps its original expression and the compile
// declines it, as materializedOuterRefInAggArg does. A literal ordinal in the
// SQL is untouched.
func keepUnlessOrdinal(orig, rewritten Expr) Expr {
	if readsAsOrdinal(rewritten) && !readsAsOrdinal(orig) {
		return orig
	}
	return rewritten
}

// readsAsOrdinal reports whether e sits in ORDER BY / GROUP BY as an output
// column POSITION. It over-approximates the three resolvers on purpose (query.go's
// orderByOrdinal, which also accepts a unary +/- sign; sql_group.go's GROUP BY
// term resolution, bare only; cte.go's compound ORDER BY, which strips a COLLATE
// first), because every extra hit is one more DECLINE, never a wrong answer.
func readsAsOrdinal(e Expr) bool {
	switch x := e.(type) {
	case LiteralExpr:
		return x.Val.Typ == Int
	case UnaryExpr:
		return (x.Op == "+" || x.Op == "-") && readsAsOrdinal(x.X)
	case CollateExpr:
		return readsAsOrdinal(x.X)
	}
	return false
}

// rewriteExprOuterRefs is rewriteSelectOuterRefs' per-expression worker. subs
// is this level's hoisted-aggregate substitutions (hoistSubsFor), applied
// first.
//
// A qualified ColumnExpr whose qualifier matches nothing in local is resolved
// against outer via resolveColumn, which walks outer's own chain (so a trigger
// body's nested subquery reaches the firing row). A resolution failure leaves
// the node untouched for the compile-time decline. Other nodes are walked
// structurally, allocating only along rewritten paths.
//
// emptyArmScope also lets an unqualified reference through in the ColumnExpr
// case; it is threaded unchanged through structural cases and reset by a
// nested subquery's recursion.
func rewriteExprOuterRefs(e Expr, outer *evalCtx, local map[string]bool, subs []hoistedAggVal, changed *bool, emptyArmScope bool) Expr {
	if fc, ok := e.(FuncExpr); ok && len(subs) > 0 {
		// A HOISTED aggregate call: its value belongs to the enclosing query and
		// was finalized before this body was ever materialized (aggResult,
		// vdbe_agg.go), so it is a constant here. Substituting it is what turns
		// "SELECT (SELECT count(a1) FROM t2) FROM t1" into the enclosing query's
		// count over t1, read once inside the body -- and is checked ONLY at the
		// level the call was found on, never inside a nested body (which gets its
		// own hoistSubsFor lookup, and normally none at all).
		for _, h := range subs {
			if sameAggCall(fc, h.call) {
				*changed = true
				return LiteralExpr{Val: h.val}
			}
		}
	}
	// A BARE reference to an outer select-list alias whose expression is an
	// aggregate call (materializedOuterRefInAggArgArm's ColumnExpr branch,
	// sql_group.go): resolve.c's resolveAlias (resolve.c:69-101) splices a
	// COPY of the outer's already-resolved expression in at resolve time, so
	// by the time this body is materialized the substitution has already
	// happened for C SQLite -- there was never a literal call written at
	// this position for sameAggCall to match, so this matches by NAME
	// instead. Checked before the switch for the same reason the FuncExpr
	// case above is: a bare column would otherwise just fall through
	// unchanged (its own case below only ever resolves a QUALIFIED
	// reference).
	if col, ok := e.(ColumnExpr); ok && col.Qualifier == "" && len(subs) > 0 {
		for _, h := range subs {
			if h.aliasName != "" && equalFoldName(h.aliasName, col.Name) {
				*changed = true
				return LiteralExpr{Val: h.val}
			}
		}
	}
	switch x := e.(type) {
	case nil:
		return nil
	case ColumnExpr:
		if x.Qualifier != "" && local[r33sFoldIdent(x.Qualifier)] {
			return x
		}
		if x.Qualifier == "" && !emptyArmScope {
			// Out of scope unless emptyArmScope: a compound member with no scope
			// of its own has nothing an unqualified name could bind to locally, so
			// it is an outer reference -- e.g. window4.test 12.3's "min(a) OVER
			// ()". A window function's argument never goes through
			// sqlite3ReferencesSrcList, so substituting the anchor row's value is
			// right, while an aggregate hoist would be wrong: over t1(a) =
			// 17,2,99,-3,7, "SELECT (SELECT avg(a) UNION SELECT min(a) OVER())
			// FROM t1" is 17 (the anchor row's a), not -3.
			return x
		}
		// The substituted literal carries the column's affinity, declared
		// collation and storage class (LiteralExpr). C keeps a correlated
		// reference a TK_COLUMN and reads only its value at run time, so all
		// three still govern its comparisons:
		//
		//   - Collation (sqlite3ExprCollSeq/sqlite3BinaryCompareCollSeq): a
		//     column contributes its declared collation as a comparison
		//     operand. Over c1(a TEXT COLLATE NOCASE), "UPDATE c1 SET b='HIT'
		//     WHERE (SELECT c1.a='ABC')" must match. It is carried at declared
		//     rank, not as a CollateExpr, which would rank it explicit and make
		//     "c1.a = t.x COLLATE RTRIM" compare NOCASE.
		//   - Affinity (sqlite3CompareAffinity via sqlite3ExprAffinity): a
		//     literal's affExpr is 0, below SQLITE_AFF_NONE, which would move
		//     the comparison into the other branch. Over t2(a INTEGER)=1,
		//     "SELECT a,(SELECT t2.a='1.0') FROM t2 GROUP BY a" is 1.
		//
		// A reference resolving in neither stmt's scope nor outer's is left for
		// the compile-time decline.
		_, _, col, _, rerr := resolveColumnEx(outer, x)
		if rerr != nil || col == nil {
			return x
		}
		v, err := columnRowValue(outer, x)
		if err != nil {
			return x
		}
		*changed = true
		// coll is columnDeclaredCollation's own answer for this very reference
		// (BINARY for a column that declares none, nothing at all for the rowid
		// pseudo-column) so the substituted node ranks in
		// resolveCompareCollation exactly where the ColumnExpr it replaced did.
		// materialized mirrors isMaterializedRef's ColumnExpr case: a COMPUTED
		// derived-table column (NoAffinity) does not defend its storage class,
		// a real one does.
		lit := LiteralExpr{Val: v, aff: col.Aff, materialized: !col.NoAffinity}
		lit.coll, _ = columnDeclaredCollation(outer, x)
		return lit
	case FuncExpr:
		// An aggregate call whose own arguments name none of the body's tables
		// belongs to an enclosing query; substituting the outer row into it
		// would move it here. resolve.c's TK_FUNCTION arm:
		//
		//	pNC2 = pNC;
		//	while( pNC2 && sqlite3ReferencesSrcList(pParse,pExpr,pNC2->pSrcList)==0 ){
		//	  pExpr->op2 += (1 + pNC2->nNestedSelect);
		//	  pNC2 = pNC2->pNext;
		//	}
		//
		// sqlite3ReferencesSrcList answers 1 (names this SrcList) or -1 (names
		// no table) to stop the walk and 0 to walk out. aggCallHasLocalColumnArg
		// is the 1-vs-0 test; the second test covers -1 (see
		// outerTableScopeNames).
		//
		// Where the walk lands cannot be reproduced here. For an enclosing
		// aggregate SELECT the hoist above already substituted the whole call;
		// for an UPDATE/DELETE row there is no AggInfo and C raises "misuse of
		// aggregate: sum()" -- e.g. "DELETE FROM w WHERE (SELECT sum(w.x))>0".
		// Leaving the call alone leaves its argument unresolvable, the right
		// decline.
		//
		// An unqualified argument makes the first test true, so by the second
		// test every column reference in the call is qualified.
		if isAggregateCall(x) && !aggCallHasLocalColumnArg(x, local) &&
			aggCallHasLocalColumnArg(x, outerTableScopeNames(outer)) {
			return x
		}
		if x.Args != nil {
			args := make([]Expr, len(x.Args))
			for i, a := range x.Args {
				args[i] = rewriteExprOuterRefs(a, outer, local, subs, changed, emptyArmScope)
			}
			x.Args = args
		}
		// FILTER and the window's own PARTITION BY/ORDER BY are expressions of
		// this call too, so a hoisted aggregate's substitution has to reach
		// them -- without this, "count(a) OVER (ORDER BY sum(a))" kept the
		// un-substituted sum() and the body could not be evaluated. Found by
		// round twenty-four's window stream, which verified it against 3.53.3
		// ("SELECT (SELECT count(a) OVER (ORDER BY sum(a)) + total(a) OVER())
		// FROM t1" -> 2.0) and could not land it in this file.
		if x.Filter != nil {
			x.Filter = rewriteExprOuterRefs(x.Filter, outer, local, subs, changed, emptyArmScope)
		}
		if len(x.OrderBy) > 0 {
			ob := make([]OrderTerm, len(x.OrderBy))
			for i, t := range x.OrderBy {
				t.Expr = rewriteExprOuterRefs(t.Expr, outer, local, subs, changed, emptyArmScope)
				ob[i] = t
			}
			x.OrderBy = ob
		}
		if x.Over != nil {
			spec := *x.Over
			if spec.PartitionBy != nil {
				parts := make([]Expr, len(spec.PartitionBy))
				for i, p := range spec.PartitionBy {
					parts[i] = rewriteExprOuterRefs(p, outer, local, subs, changed, emptyArmScope)
				}
				spec.PartitionBy = parts
			}
			if spec.OrderBy != nil {
				terms := make([]OrderTerm, len(spec.OrderBy))
				copy(terms, spec.OrderBy)
				for i := range terms {
					terms[i].Expr = rewriteExprOuterRefs(terms[i].Expr, outer, local, subs, changed, emptyArmScope)
				}
				spec.OrderBy = terms
			}
			x.Over = &spec
		}
		return x
	case UnaryExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		return x
	case BinaryExpr:
		x.L = rewriteExprOuterRefs(x.L, outer, local, subs, changed, emptyArmScope)
		x.R = rewriteExprOuterRefs(x.R, outer, local, subs, changed, emptyArmScope)
		return x
	case IsNullExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		return x
	case InExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		if x.List != nil {
			list := make([]Expr, len(x.List))
			for i, it := range x.List {
				list[i] = rewriteExprOuterRefs(it, outer, local, subs, changed, emptyArmScope)
			}
			x.List = list
		}
		if x.Sub != nil {
			x.Sub = rewriteSelectOuterRefs(x.Sub, outer, local, changed, false)
		}
		return x
	case BetweenExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		x.Lo = rewriteExprOuterRefs(x.Lo, outer, local, subs, changed, emptyArmScope)
		x.Hi = rewriteExprOuterRefs(x.Hi, outer, local, subs, changed, emptyArmScope)
		return x
	case LikeExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		x.Pattern = rewriteExprOuterRefs(x.Pattern, outer, local, subs, changed, emptyArmScope)
		if x.Escape != nil {
			x.Escape = rewriteExprOuterRefs(x.Escape, outer, local, subs, changed, emptyArmScope)
		}
		return x
	case GlobExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		x.Pattern = rewriteExprOuterRefs(x.Pattern, outer, local, subs, changed, emptyArmScope)
		return x
	case CollateExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		return x
	case CastExpr:
		x.X = rewriteExprOuterRefs(x.X, outer, local, subs, changed, emptyArmScope)
		return x
	case CaseExpr:
		if x.Base != nil {
			x.Base = rewriteExprOuterRefs(x.Base, outer, local, subs, changed, emptyArmScope)
		}
		if x.Whens != nil {
			whens := make([]WhenClause, len(x.Whens))
			for i, w := range x.Whens {
				whens[i] = WhenClause{
					When: rewriteExprOuterRefs(w.When, outer, local, subs, changed, emptyArmScope),
					Then: rewriteExprOuterRefs(w.Then, outer, local, subs, changed, emptyArmScope),
				}
			}
			x.Whens = whens
		}
		if x.Else != nil {
			x.Else = rewriteExprOuterRefs(x.Else, outer, local, subs, changed, emptyArmScope)
		}
		return x
	case SubqueryExpr:
		x.Stmt = rewriteSelectOuterRefs(x.Stmt, outer, local, changed, false)
		return x
	case ExistsExpr:
		x.Stmt = rewriteSelectOuterRefs(x.Stmt, outer, local, changed, false)
		return x
	default:
		return e
	}
}
