// This file implements SQLite's result-set-alias rule for WHERE / ON / GROUP
// BY / HAVING (and expressions inside ORDER BY): a clause may name a
// select-list alias ("SELECT a+b AS s FROM t1 WHERE s>15").
//
// It is a syntactic substitution before codegen: a bare name that binds to no
// column of this statement's FROM is replaced by the aliased expression, which
// then compiles as if written out, so declines stay declines.
//
// The rules, over t1(a,b,c) and t2(a,d):
//
//   - A real column always wins: "SELECT a+0 AS a FROM t1 WHERE a>1" reads the
//     column. So substitution only fires where resolution would have failed.
//   - An alias beats an outer-scope column: "SELECT (SELECT m+100 AS q FROM
//     innerT WHERE q>101 LIMIT 1) FROM outerT" with outerT(q) answers 102. So
//     the pass runs per compile level, before compileColumn's outer walk.
//   - An alias beats the TRUE/FALSE keyword: "SELECT 0 AS true FROM t1 WHERE
//     true" returns no rows, so it precedes ColumnExpr's FallbackLiteral.
//   - Only an explicit alias counts: `SELECT a+b FROM t1 WHERE "a+b">15`
//     compares the string 'a+b' (all rows), while `SELECT a+b AS "a+b" ...`
//     uses the alias. So it matches SelectColumn.Alias, never derived names.
//   - The first of two identical aliases wins.
//   - The expression is substituted, not the computed value: "SELECT random()
//     AS r FROM t1 WHERE r=r" returns no rows.
//   - An aggregate alias is legal only in HAVING ("WHERE n>0" and "GROUP BY s"
//     over an aggregate alias are "misuse of aggregate"). A window alias is
//     never substituted (an error everywhere, HAVING included).
//
// Nested subqueries are handled by substituteAliasesIntoSub, within the limits
// documented there.
package engine

import "slices"

// substituteResultAliases applies this file's rule to stmt's WHERE, GROUP BY
// and HAVING clauses and to every join source's ON condition, returning the
// statement to compile. scopes must be THIS compile level's own scopes
// (compiler.scopes) -- see the file comment for why the outer chain is
// deliberately not consulted.
//
// srcs' ON conditions are rewritten in place (resolveJoinSources builds a fresh
// slice per compile, so nothing outside this compile can observe it); stmt
// itself is never mutated -- a shallow copy is returned when, and only when,
// something actually changed, so the overwhelmingly common alias-free statement
// costs one scan of its clauses and no allocation.
func substituteResultAliases(stmt *SelectStmt, srcs []joinSource, scopes []compileScope, pager *ReadOnlyPager) *SelectStmt {
	if !hasResultAlias(stmt.Columns) {
		return stmt
	}
	changed := false
	out := *stmt
	out.Where = substituteAliasRefs(stmt.Where, stmt.Columns, scopes, false, false, &changed, pager)
	out.Having = substituteAliasRefs(stmt.Having, stmt.Columns, scopes, true, false, &changed, pager)
	if len(stmt.GroupBy) != 0 {
		gb := make([]Expr, len(stmt.GroupBy))
		for i, g := range stmt.GroupBy {
			gb[i] = substituteAliasRefs(g, stmt.Columns, scopes, false, false, &changed, pager)
		}
		out.GroupBy = gb
	}
	for i := range srcs {
		srcs[i].on = substituteAliasRefs(srcs[i].on, stmt.Columns, scopes, false, false, &changed, pager)
	}
	if len(stmt.OrderBy) != 0 {
		ob := make([]OrderTerm, len(stmt.OrderBy))
		copy(ob, stmt.OrderBy)
		for i := range ob {
			ob[i].Expr = substituteOrderAliasRef(ob[i].Expr, stmt.Columns, scopes, &changed, pager)
		}
		out.OrderBy = ob
	}
	if !changed {
		return stmt
	}
	return &out
}

// hasResultAlias reports whether any select-list item carries an explicit
// alias -- the cheap precondition for the whole pass.
func hasResultAlias(cols []SelectColumn) bool {
	for _, c := range cols {
		if !c.Star && c.HasAlias {
			return true
		}
	}
	return false
}

// resultAliasExpr returns the expression named by an unqualified reference to
// select-list alias name, and whether one was found. allowAgg admits an alias
// whose expression contains an aggregate (HAVING only); a window-function alias
// is never returned. First match wins.
func resultAliasExpr(cols []SelectColumn, name string, allowAgg, allowWindow bool) (Expr, bool) {
	for _, c := range cols {
		if c.Star || !c.HasAlias || !equalFoldName(c.Alias, name) {
			continue
		}
		if !allowWindow && exprHasWindow(c.Expr) {
			return nil, false
		}
		if !allowAgg && containsAggregate(c.Expr) {
			return nil, false
		}
		return c.Expr, true
	}
	return nil, false
}

// hasAliasNamed reports whether cols defines an explicit alias by name at all,
// even one resultAliasExpr would disqualify (a window function, or an
// aggregate where forbidden). outerAliasAggCall walks levels outward and must
// stop at the first level defining the name: lookupName/resolveAlias
// (resolve.c:648-692, 69-101) splice it unconditionally ("cnt=1; goto
// lookupname_end"), and C then raises its own error (e.g. "misuse of aliased
// window function") rather than using a farther level's alias.
func hasAliasNamed(cols []SelectColumn, name string) bool {
	for _, c := range cols {
		if !c.Star && c.HasAlias && equalFoldName(c.Alias, name) {
			return true
		}
	}
	return false
}

// substituteAliasRefs rewrites e, replacing every unqualified column reference
// that binds to no column in scopes but does name a select-list alias with that
// alias's own expression. See the file comment for the rule and its evidence.
//
// The spliced-in expression is not itself re-walked: its names resolve as
// ordinary column references against the FROM clause, which is exactly what
// SQLite does (an alias may not reference another alias -- "SELECT a AS x, x AS
// y FROM t1 WHERE y>1" is an error there), and it is also what makes a
// self-referential alias unable to loop.
func substituteAliasRefs(e Expr, cols []SelectColumn, scopes []compileScope, allowAgg, allowWindow bool, changed *bool, pager *ReadOnlyPager) Expr {
	switch x := e.(type) {
	case nil:
		return nil
	case ColumnExpr:
		if x.Qualifier != "" {
			return x
		}
		// nil pager: x.Qualifier=="" is guaranteed by the check just above
		// (x.Schema is never set without Qualifier -- ColumnExpr.Schema's own
		// doc comment, sql_ast.go), so resolveInScopes never needs to resolve
		// a dbIdx here (dbIdxForSchema short-circuits on x.Schema=="" before
		// ever consulting pager).
		if _, _, _, found, _, hard := resolveInScopes(scopes, x, nil); found || hard != nil {
			// Binds to a real column of this statement's own FROM clause (or is
			// ambiguous among them): C SQLite resolves the same column, or
			// reports the same ambiguity, never the alias.
			return x
		}
		repl, ok := resultAliasExpr(cols, x.Name, allowAgg, allowWindow)
		if !ok {
			return x
		}
		*changed = true
		return repl
	case UnaryExpr:
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		return x
	case BinaryExpr:
		x.L = substituteAliasRefs(x.L, cols, scopes, allowAgg, allowWindow, changed, pager)
		x.R = substituteAliasRefs(x.R, cols, scopes, allowAgg, allowWindow, changed, pager)
		return x
	case IsNullExpr:
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		return x
	case InExpr:
		// The tested value, any value-list entry, and -- only in the one shape
		// substituteAliasesIntoSub serves -- a subquery RHS.
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		if x.List != nil {
			list := make([]Expr, len(x.List))
			for i, it := range x.List {
				list[i] = substituteAliasRefs(it, cols, scopes, allowAgg, allowWindow, changed, pager)
			}
			x.List = list
		}
		x.Sub = substituteAliasesIntoSub(x.Sub, cols, changed, pager)
		return x
	case SubqueryExpr:
		x.Stmt = substituteAliasesIntoSub(x.Stmt, cols, changed, pager)
		return x
	case ExistsExpr:
		x.Stmt = substituteAliasesIntoSub(x.Stmt, cols, changed, pager)
		return x
	case BetweenExpr:
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		x.Lo = substituteAliasRefs(x.Lo, cols, scopes, allowAgg, allowWindow, changed, pager)
		x.Hi = substituteAliasRefs(x.Hi, cols, scopes, allowAgg, allowWindow, changed, pager)
		return x
	case LikeExpr:
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		x.Pattern = substituteAliasRefs(x.Pattern, cols, scopes, allowAgg, allowWindow, changed, pager)
		if x.Escape != nil {
			x.Escape = substituteAliasRefs(x.Escape, cols, scopes, allowAgg, allowWindow, changed, pager)
		}
		return x
	case GlobExpr:
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		x.Pattern = substituteAliasRefs(x.Pattern, cols, scopes, allowAgg, allowWindow, changed, pager)
		return x
	case CollateExpr:
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		return x
	case CastExpr:
		x.X = substituteAliasRefs(x.X, cols, scopes, allowAgg, allowWindow, changed, pager)
		return x
	case FuncExpr:
		// An aggregate's own ARGUMENT may reference an alias wherever the
		// aggregate itself is legal, so allowAgg is passed through unchanged;
		// a window function's OVER spec is the planner's (see walkExprShallow,
		// sql_window.go) and is left alone.
		if x.Args != nil {
			args := make([]Expr, len(x.Args))
			for i, a := range x.Args {
				args[i] = substituteAliasRefs(a, cols, scopes, allowAgg, allowWindow, changed, pager)
			}
			x.Args = args
		}
		if len(x.OrderBy) > 0 {
			ob := make([]OrderTerm, len(x.OrderBy))
			for i, t := range x.OrderBy {
				t.Expr = substituteAliasRefs(t.Expr, cols, scopes, allowAgg, allowWindow, changed, pager)
				ob[i] = t
			}
			x.OrderBy = ob
		}
		return x
	case CaseExpr:
		_, kw0, isBool := isBoolParts(x)
		_, kwWasCol := kw0.(ColumnExpr)
		if x.Base != nil {
			x.Base = substituteAliasRefs(x.Base, cols, scopes, allowAgg, allowWindow, changed, pager)
		}
		if x.Whens != nil {
			whens := make([]WhenClause, len(x.Whens))
			for i, w := range x.Whens {
				whens[i] = WhenClause{
					When: substituteAliasRefs(w.When, cols, scopes, allowAgg, allowWindow, changed, pager),
					Then: substituteAliasRefs(w.Then, cols, scopes, allowAgg, allowWindow, changed, pager),
				}
			}
			x.Whens = whens
		}
		if x.Else != nil {
			x.Else = substituteAliasRefs(x.Else, cols, scopes, allowAgg, allowWindow, changed, pager)
		}
		// "X IS true" whose keyword just became an alias's expression: the
		// alias is what resolveExprStep's resolution of the right operand
		// found (resolve.c:1427), so the node is no TK_TRUTH and stays the
		// ordinary comparison -- "SELECT a, 0 AS true FROM t1 WHERE a IS
		// true" keeps only a=0, measured.
		if operand, kw, ok := isBoolParts(x); ok && isBool && kwWasCol {
			if _, still := kw.(ColumnExpr); !still {
				return BinaryExpr{Op: "IS", L: operand, R: kw}
			}
		}
		return x
	default:
		// LiteralExpr, ParamExpr, RowExpr, MatchExpr, ...: nothing to rewrite.
		return e
	}
}

// substituteAliasesIntoSub descends the alias rule into a nested subquery,
// returning sub unchanged unless something was substituted.
//
// Outer aliases are visible to a subquery in WHERE, GROUP BY, HAVING or an
// ORDER BY expression, but not in the select list:
//
//	SELECT 2 AS x WHERE (SELECT x AS y WHERE 3>y)  -> 2   (resolver01.test)
//	SELECT 2 AS x WHERE (SELECT x AS y WHERE 1>y)  -> no rows
//	SELECT 2 AS x, (SELECT x) AS q                 -> ERROR no such column: x
//
// That is C's NC_UEList, set only after the select list is resolved; this
// descent is reached only from the clauses substituteResultAliases rewrites.
//
// Guards:
//
//   - sub's own Columns/Where/Having/GroupBy are spliced only when sub has no
//     FROM (and no CTE or compound): otherwise an inner column could shadow the
//     name ("SELECT 2 AS x WHERE (SELECT 1 FROM tz WHERE x=2)" reads tz.x).
//   - A name sub itself aliases is skipped (the inner alias wins).
//   - Only a scope-free alias expression is spliced (exprIsScopeFree): one with
//     a column or subquery would carry names that resolve somewhere else.
//     "SELECT k, v+0 AS s FROM ty WHERE (SELECT s)>10" stays declined.
//
// Nesting composes: each level filters the alias list by its own aliases.
//
// A derived table in sub's FROM gets the splice even when sub has a FROM:
// resolveSelectStep resolves FROM-clause subqueries with pOuterNC, the context
// enclosing sub, before sub's own NameContext and NC_UEList exist
// (resolve.c:1930-1961, NC_UEList at :1997). So "SELECT 1 AS c WHERE EXISTS
// (SELECT * FROM (SELECT c))" sees c. A derived table cannot see its FROM
// siblings (no LATERAL), so this is safe for every such item.
//
// sub's own clauses when sub has a FROM would also see the alias in C, but
// deciding whether a real column wins needs sub's schema, which this AST pass
// lacks; that stays a gap.
//
// A CTE body in sub's WITH gets the same descent for the same reason: a CTE
// reference becomes an ordinary subquery item (select.c:5754) resolved with
// pOuterNC (with2.test 10.1). It applies whether or not sub has a FROM, and
// uses vis unfiltered by sub's own aliases, since those do not exist yet when
// the CTE resolves. A recursive CTE is a compound and is excluded by the
// compound guard.
func substituteAliasesIntoSub(sub *SelectStmt, cols []SelectColumn, changed *bool, pager *ReadOnlyPager) *SelectStmt {
	if sub == nil || len(sub.Compound) != 0 {
		return sub
	}
	vis := visibleOuterAliases(cols, sub.Columns, pager)
	if len(sub.CTEs) != 0 {
		// A closed subquery names tables, and one of sub's CTEs would take the
		// name from the table the alias's own level meant.
		vis = slices.DeleteFunc(slices.Clone(vis), func(c SelectColumn) bool { return containsSubquery(c.Expr) })
	}
	if len(vis) == 0 {
		return sub
	}
	inner := false
	out := *sub
	// spliceOwn splices aliases into sub's own clauses.
	spliceOwn := func(aliases []SelectColumn) {
		if len(sub.Columns) != 0 {
			newCols := make([]SelectColumn, len(sub.Columns))
			copy(newCols, sub.Columns)
			for i := range newCols {
				if newCols[i].Star {
					continue
				}
				newCols[i].Expr = substituteAliasRefs(newCols[i].Expr, aliases, nil, false, false, &inner, pager)
			}
			out.Columns = newCols
		}
		out.Where = substituteAliasRefs(sub.Where, aliases, nil, false, false, &inner, pager)
		out.Having = substituteAliasRefs(sub.Having, aliases, nil, false, false, &inner, pager)
		if len(sub.GroupBy) != 0 {
			gb := make([]Expr, len(sub.GroupBy))
			for i, g := range sub.GroupBy {
				gb[i] = substituteAliasRefs(g, aliases, nil, false, false, &inner, pager)
			}
			out.GroupBy = gb
		}
	}
	if len(sub.CTEs) != 0 {
		var newCTEs []CTEDef
		for i := range sub.CTEs {
			newCore := substituteAliasesIntoSub(sub.CTEs[i].Select, vis, &inner, pager)
			if newCore != sub.CTEs[i].Select {
				if newCTEs == nil {
					newCTEs = make([]CTEDef, len(sub.CTEs))
					copy(newCTEs, sub.CTEs)
				}
				newCTEs[i].Select = newCore
			}
		}
		if newCTEs != nil {
			out.CTEs = newCTEs
		}
	}
	if len(sub.From) == 0 {
		spliceOwn(vis)
	} else {
		// sub has its own FROM -- its own clauses stay untouched (see doc
		// comment above), but each FROM item that is itself a bare
		// (FROM-less/CTE-less/non-compound) derived table is isolated from
		// every one of its siblings, so the SAME splice safely reaches
		// straight through it to that item's own body, recursing to any
		// depth of such wrapping.
		var newFrom []FromItem
		for i := range sub.From {
			it := sub.From[i].Subquery
			if it == nil {
				continue
			}
			newSub := substituteAliasesIntoSub(it, vis, &inner, pager)
			if newSub != it {
				if newFrom == nil {
					newFrom = make([]FromItem, len(sub.From))
					copy(newFrom, sub.From)
				}
				newFrom[i].Subquery = newSub
			}
		}
		if newFrom != nil {
			out.From = newFrom
		}
		// ...and sub's own clauses, for the aliases no FROM item of sub offers
		// a column of: lookupName tries sub's FROM first and walks out only on
		// a miss (resolve.c:703-704), so where the schema shows that miss the
		// alias is what C binds (existsexpr.test 10.2).
		if own := pager.aliasesNotOffered(sub.From, vis); len(own) != 0 {
			spliceOwn(own)
			for i := range out.From {
				if out.From[i].On == nil {
					continue
				}
				if newFrom == nil {
					newFrom = make([]FromItem, len(sub.From))
					copy(newFrom, sub.From)
					out.From = newFrom
				}
				newFrom[i].On = substituteAliasRefs(sub.From[i].On, own, nil, false, false, &inner, pager)
			}
		}
	}
	// The nested ORDER BY is deliberately untouched: a bare term there names
	// one of the NESTED statement's own output columns, which is a different
	// rule (substituteOrderAliasRef) belonging to that level's own pass.
	if !inner {
		return sub
	}
	*changed = true
	return &out
}

// visibleOuterAliases is the subset of an enclosing statement's select-list
// aliases that a nested FROM-less subquery may see: an explicit alias, not
// shadowed by an alias of the same name in the nested statement itself, whose
// expression is scope-free. See substituteAliasesIntoSub for each rule's
// evidence.
func visibleOuterAliases(outer, innerCols []SelectColumn, pager *ReadOnlyPager) []SelectColumn {
	var out []SelectColumn
	for _, c := range outer {
		if c.Star || !c.HasAlias || !exprIsScopeFree(c.Expr, pager) {
			continue
		}
		shadowed := false
		for _, ic := range innerCols {
			if !ic.Star && ic.HasAlias && equalFoldName(ic.Alias, c.Alias) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			out = append(out, c)
		}
	}
	return out
}

// aliasesNotOffered returns the aliases no FROM item names a column of, when
// that can be told: every item a base table this pager resolves and not a CTE.
// Otherwise nil. A rowid name is never returned -- a rowid table answers it.
func (p *ReadOnlyPager) aliasesNotOffered(from []FromItem, aliases []SelectColumn) []SelectColumn {
	if p == nil {
		return nil
	}
	var cols []columnInfo
	for _, it := range from {
		if it.Subquery != nil || it.TableFunc || it.Table == "" || len(it.NestedGroupSpan) != 0 {
			return nil
		}
		if _, isCTE := p.lookupCTE(it.Table); isCTE && it.Schema == "" {
			return nil
		}
		scope, ok := scopeOfQualifier(it.Schema)
		if it.Schema != "" && !ok {
			return nil
		}
		rt, err := p.resolveTableIn(scope, it.Table)
		if err != nil {
			return nil
		}
		cols = append(cols, rt.cols...)
	}
	var out []SelectColumn
	for _, a := range aliases {
		if isRowidAliasName(a.Alias) || slices.ContainsFunc(cols, func(c columnInfo) bool { return equalFoldName(c.Name, a.Alias) }) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// exprIsScopeFree reports whether e can be spliced into ANY scope and mean
// exactly the same thing -- it names nothing, so nothing can rebind. It is a
// FAIL-SAFE walker: every node kind it does not explicitly admit (a column
// reference, a subquery, a row value, an aggregate or window call, a MATCH, a
// RAISE, and anything added to the AST later) answers false, because
// under-detecting here would splice an expression whose names resolve at the
// wrong level, which is a wrong answer, while over-detecting only declines.
func exprIsScopeFree(e Expr, pager *ReadOnlyPager) bool { return scopeFree(e, pager, nil) }

// closedScope is one level of a closed subquery's FROM: the names its items go
// by and the columns they offer.
type closedScope struct {
	names []string
	cols  []string
}

// scopeFree is exprIsScopeFree. chain is non-empty inside a closed subquery:
// the FROM levels a column reference there may bind to -- it is scope-free
// when one of them offers it -- and whose aggregate calls are its own. A
// subquery is scope-free when it is closed (selectIsClosed): resolve.c resolves
// the alias's copy before splicing it (resolveAlias, resolve.c:69-101), and a
// subquery whose every name binds inside it resolves the same way wherever it
// stands.
func scopeFree(e Expr, pager *ReadOnlyPager, chain []closedScope) bool {
	own := len(chain) != 0
	switch x := e.(type) {
	case nil:
		return true
	case ColumnExpr:
		return x.Schema == "" && slices.ContainsFunc(chain, func(sc closedScope) bool {
			if x.Qualifier != "" {
				return slices.ContainsFunc(sc.names, func(n string) bool { return equalFoldName(n, x.Qualifier) }) &&
					slices.ContainsFunc(sc.cols, func(n string) bool { return equalFoldName(n, x.Name) })
			}
			return slices.ContainsFunc(sc.cols, func(n string) bool { return equalFoldName(n, x.Name) })
		})
	case SubqueryExpr:
		return selectIsClosed(x.Stmt, pager, chain)
	case ExistsExpr:
		return selectIsClosed(x.Stmt, pager, chain)
	case LiteralExpr, ParamExpr:
		return true
	case UnaryExpr:
		return scopeFree(x.X, pager, chain)
	case BinaryExpr:
		return scopeFree(x.L, pager, chain) && scopeFree(x.R, pager, chain)
	case IsNullExpr:
		return scopeFree(x.X, pager, chain)
	case BetweenExpr:
		return scopeFree(x.X, pager, chain) && scopeFree(x.Lo, pager, chain) && scopeFree(x.Hi, pager, chain)
	case LikeExpr:
		return scopeFree(x.X, pager, chain) && scopeFree(x.Pattern, pager, chain) && scopeFree(x.Escape, pager, chain)
	case GlobExpr:
		return scopeFree(x.X, pager, chain) && scopeFree(x.Pattern, pager, chain)
	case CollateExpr:
		return scopeFree(x.X, pager, chain)
	case CastExpr:
		return scopeFree(x.X, pager, chain)
	case InExpr:
		if x.Sub != nil && !selectIsClosed(x.Sub, pager, chain) {
			return false
		}
		if !scopeFree(x.X, pager, chain) {
			return false
		}
		for _, it := range x.List {
			if !scopeFree(it, pager, chain) {
				return false
			}
		}
		return true
	case CaseExpr:
		if !scopeFree(x.Base, pager, chain) || !scopeFree(x.Else, pager, chain) {
			return false
		}
		for _, w := range x.Whens {
			if !scopeFree(w.When, pager, chain) || !scopeFree(w.Then, pager, chain) {
				return false
			}
		}
		return true
	case FuncExpr:
		// An aggregate or window call belongs to a particular query level, so
		// it is never scope-free however constant its arguments look -- unless
		// that level is the closed subquery holding it (own); a FILTER is part
		// of that same machinery.
		if x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 || (!own && (x.Star || isAggregateCall(x))) {
			return false
		}
		for _, a := range x.Args {
			if !scopeFree(a, pager, chain) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// selectIsClosed reports whether every name in s binds inside s (chain being
// the levels of an enclosing closed subquery): its FROM items are tables the
// pager resolves, CTEs with a column list, or closed derived tables whose
// output columns are named, and every expression is scopeFree against those.
// Fail-safe like exprIsScopeFree -- anything it does not recognize answers
// false.
func selectIsClosed(s *SelectStmt, pager *ReadOnlyPager, chain []closedScope) bool {
	if s == nil || len(s.CTEs) != 0 || len(s.Windows) != 0 {
		return false
	}
	var sc closedScope
	for _, it := range s.From {
		if it.TableFunc || len(it.NestedGroupSpan) != 0 {
			return false
		}
		name := it.Alias
		if it.Subquery != nil {
			if !selectIsClosed(it.Subquery, pager, chain) {
				return false
			}
			for _, c := range it.Subquery.Columns {
				switch cx, isCol := c.Expr.(ColumnExpr); {
				case c.Star:
					return false
				case c.HasAlias:
					sc.cols = append(sc.cols, c.Alias)
				case isCol:
					sc.cols = append(sc.cols, cx.Name)
				default:
					return false
				}
			}
		} else {
			if it.Table == "" || pager == nil {
				return false
			}
			if name == "" {
				name = it.Table
			}
			if b, isCTE := pager.lookupCTE(it.Table); isCTE && it.Schema == "" {
				if b.colNames == nil {
					return false
				}
				sc.cols = append(sc.cols, b.colNames...)
			} else {
				scope, ok := scopeOfQualifier(it.Schema)
				if it.Schema != "" && !ok {
					return false
				}
				rt, err := pager.resolveTableIn(scope, it.Table)
				if err != nil {
					return false
				}
				for _, c := range rt.cols {
					sc.cols = append(sc.cols, c.Name)
				}
			}
		}
		if name != "" {
			sc.names = append(sc.names, name)
		}
		for _, u := range it.Using {
			if !slices.ContainsFunc(sc.cols, func(n string) bool { return equalFoldName(n, u) }) {
				return false
			}
		}
	}
	chain = append(slices.Clone(chain), sc)
	for _, it := range s.From {
		if !scopeFree(it.On, pager, chain) {
			return false
		}
	}
	for _, arm := range s.Compound {
		if !selectIsClosed(arm.Stmt, pager, chain[:len(chain)-1]) {
			return false
		}
	}
	for _, c := range s.Columns {
		if c.Star && c.StarQualifier == "" {
			continue
		}
		if c.Star || !scopeFree(c.Expr, pager, chain) {
			return false
		}
	}
	for _, e := range append([]Expr{s.Where, s.Having, s.LimitParam, s.OffsetParam}, s.GroupBy...) {
		if !scopeFree(e, pager, chain) {
			return false
		}
	}
	for _, o := range s.OrderBy {
		if !scopeFree(o.Expr, pager, chain) {
			return false
		}
	}
	return true
}

// substituteOrderAliasRef applies the alias rule to one ORDER BY term. A term
// that is exactly a bare name (or a bare name under COLLATE) names an output
// column, resolved by resolveOrderKeys, and sorts by the computed value
// ("ORDER BY r" over "random() AS r"); SQLite's way to force the expression is
// "ORDER BY +r". So the bare form is left alone and anything deeper is
// substituted. Inside ORDER BY both aggregate and window aliases are legal,
// and a real column still wins:
//
//	SELECT a AS x FROM t1 ORDER BY +x                        1,1,2,3
//	SELECT a-1 AS x FROM t1 ORDER BY abs(x)                  0,0,1,2
//	SELECT sum(b) AS s FROM t1 GROUP BY a ORDER BY s+1       10,30,60
//	SELECT sum(b) OVER (ORDER BY a) AS abc, a
//	  FROM t1 ORDER BY abc+5                       (60,1),(60,1),(90,2),(100,3)
//	SELECT b AS a FROM t1 ORDER BY a+0             20,40,30,10  -- REAL column
func substituteOrderAliasRef(e Expr, cols []SelectColumn, scopes []compileScope, changed *bool, pager *ReadOnlyPager) Expr {
	if _, isCol := stripOrderCollate(e).(ColumnExpr); isCol {
		return e
	}
	return substituteAliasRefs(e, cols, scopes, true, true, changed, pager)
}
