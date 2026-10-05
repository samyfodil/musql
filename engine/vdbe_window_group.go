// This file compiles the ONE shape vdbe_scan.go's aggregate dispatch and
// vdbe_window.go's compileScanWindow each used to decline on sight: a WINDOW
// function combined with an AGGREGATE query -- a GROUP BY, or the single
// implicit group of a whole-table aggregate. It is one rule, and it used to
// scatter across half a dozen unrelated-looking errors ("unsupported function
// sum()", "wrong number of arguments to max()", "unsupported use of * in
// count()", ...) purely because the aggregate planner reached the window call
// first and tried to plan it as an ordinary aggregate or scalar.
//
// The semantics are a composition of two things this engine already had, and
// the composition is the whole implementation. The aggregate query produces
// one row per group, in which an aggregate call takes the group's aggregate
// value and a BARE column takes the group's ANCHOR row's value
// (groupBareColExpr / aggAccumulators.anchorRow); the window functions then
// run over THOSE rows. Verified directly against C SQLite over
// (1,50,'z'),(1,10,'a'),(2,1,'m'),(2,20,'b'),(3,30,'k') GROUP BY a:
//
//	sum(b)                      60, 21, 30   the group's own aggregate
//	sum(b)       OVER ()        81, 81, 81   50+1+30: each group's FIRST row's
//	                                         b -- the anchor -- NOT 111
//	max(b), sum(b) OVER ()      100 (=50+20+30): adding a min/max makes the
//	                                         WINNING row the anchor instead
//	sum(sum(b))  OVER ()       111, 111, 111
//	count(*)     OVER ()         3,  3,  3   groups, not rows
//	row_number() OVER (ORDER BY a) 1, 2, 3
//	sum(sum(b))  OVER (ORDER BY a) 60, 81, 111
//
// and, over the same table with NO GROUP BY (one implicit group, so exactly
// one row out, and the window sees exactly that one row):
//
//	sum(sum(b))  OVER ()             111
//	count(*)     OVER (), sum(b)     1, 111    one GROUP, not five rows
//	min(b),      sum(b) OVER ()      1, 1      the min row's b, not the first's
//	max(b) OVER (), count(*)         50, 5     a WINDOW max is not a census
//	                                           site, so b is the FIRST row's
//	sum(b) OVER (), count(*) FROM <empty>  NULL, 0   still exactly one row
//
// So the compile is a rewrite into the DERIVED-TABLE form -- which is exactly
// what C SQLite itself compiles this shape to (sqlite3WindowRewrite,
// window.c: it lifts every TK_AGG_FUNCTION and TK_COLUMN out into a
// sub-select over an ephemeral table and rewrites each into a reference to
// that table's column):
//
//	SELECT a, sum(b) OVER () FROM t WHERE ... GROUP BY a HAVING ...
//	  =>
//	SELECT _w0, sum(_w1) OVER ()
//	  FROM (SELECT a AS _w0, b AS _w1 FROM t WHERE ... GROUP BY a HAVING ...)
//
// Both halves already existed and are reused verbatim: the derived query goes
// back through compileSelectScan (reaching compileScanGroupBy, or
// compileScanAggregate when there is no GROUP BY) as an ordinary derived-table
// row source, and compileScanWindow compiles the outer one. The only thing the
// rewrite must NOT inherit from the rewritten statement is its result-column
// NAMES -- those are computed from the ORIGINAL select list here and handed to
// compileScanWindow, so "SELECT a, sum(b) OVER () FROM t GROUP BY a" still
// reports "a" and "sum(b) OVER ()".
package engine

import (
	"fmt"
)

// compileScanGroupedWindow compiles an AGGREGATE SELECT (GROUP BY, or a
// whole-table aggregate) whose select list or ORDER BY contains a window
// function, via the derived-table rewrite this file's package comment
// describes. baseSrcs/scopes are the ORIGINAL statement's own FROM sources
// (already resolved by compileSelectScan), needed to expand and NAME the
// original select list; enclosing is the compile-time outer scope chain,
// threaded onto the outer (window) compile exactly as compileSelectScan would
// have.
//
// Shapes deliberately left declined here rather than approximated:
//
//   - a window function anywhere but the select list / ORDER BY -- real
//     SQLite's own "misuse of window function <name>()";
//   - a subquery (scalar/EXISTS/IN) inside an expression that must be lifted:
//     the lift would have to decide whether the subquery belongs to the inner
//     (per-group) or the outer (per-window-row) query, which is exactly the
//     analysis SQLite's selectWindowRewriteSelectCb does and this rewrite
//     does not model -- EXCEPT when baseSrcs is empty (no FROM of this
//     query's own to belong to at all), which groupWindowHoist.noOwnFrom
//     answers without that analysis; see its own doc comment;
//   - a statement whose min()/max() CENSUS the rewrite would reorder, moving
//     the anchor row, or one that names a generated derived column
//     (both guarded below).
//
// One shape does not even reach here: an aggregate that appears ONLY inside a
// window's own PARTITION BY / ORDER BY ("SELECT sum(b) OVER (ORDER BY sum(c))
// FROM t") does not make compileSelectScan's dispatch see an aggregate query
// at all, so it still declines at compileScanWindow's own "aggregate inside a
// window's own ORDER BY/PARTITION BY" check. A window over a parenthesized
// join group likewise never reaches here (compileSelectScan declines both
// GROUP BY and aggregates over one before dispatching).
func compileScanGroupedWindow(p *ReadOnlyPager, stmt *SelectStmt, baseSrcs []joinSource, scopes []tableScope, enclosing *compiler) (*Program, error) {
	// A window function in WHERE/GROUP BY/HAVING is "misuse of window function
	// sum()" in C SQLite -- verified directly for both "SELECT a,
	// sum(sum(b) OVER ()) FROM t GROUP BY a" and "SELECT a, count(*) FROM t
	// GROUP BY a HAVING sum(b) OVER () > 3". Declined here so the rewrite
	// never hands one to the aggregate planner, which has no window opcode and
	// would plan it as a plain aggregate.
	for _, e := range append([]Expr{stmt.Where, stmt.Having}, stmt.GroupBy...) {
		if exprHasWindow(e) {
			return nil, fmt.Errorf("%w: window function in WHERE/GROUP BY/HAVING", errVDBEUnsupported)
		}
	}

	// The ORIGINAL statement's output columns: their EXPRESSIONS drive the
	// lift, and their NAMES are what the rewritten program must report.
	outCols, err := expandSelectList(stmt.Columns, scopes, p.colNameMode())
	if err != nil {
		return nil, err
	}

	h := &groupWindowHoist{noOwnFrom: len(baseSrcs) == 0}
	winCols := make([]outputColumn, len(outCols))
	for i, oc := range outCols {
		e, herr := h.rewrite(oc.expr)
		if herr != nil {
			return nil, herr
		}
		winCols[i] = outputColumn{expr: e, name: oc.name}
	}

	// ORDER BY. An ordinal ("ORDER BY 2") and a bare name matching an output
	// alias both name an OUTPUT column, which compileScanWindow resolves
	// itself against the outCols passed to it -- so those terms pass through
	// UNTOUCHED (lifting them would turn "ORDER BY 2" into a constant and
	// "ORDER BY w" into a lookup of a column the derived table doesn't have).
	// Every other term is lifted like a select-list item: C SQLite orders
	// by the GROUPED value, e.g. "... GROUP BY a ORDER BY c" sorts by each
	// group's ANCHOR c and "... ORDER BY sum(b)" by the group's sum (both
	// verified directly).
	orderTerms := make([]OrderTerm, len(stmt.OrderBy))
	for i, ot := range stmt.OrderBy {
		stripped := stripOrderCollate(ot.Expr)
		if _, isOrd := orderByOrdinal(stripped); isOrd {
			orderTerms[i] = ot
			continue
		}
		if idx := outputAliasIndex(outCols, stripped); idx >= 0 {
			orderTerms[i] = ot
			continue
		}
		e, herr := h.rewrite(ot.Expr)
		if herr != nil {
			return nil, herr
		}
		ot.Expr = e
		orderTerms[i] = ot
	}

	// A "WINDOW <name> AS (...)" clause's own PARTITION BY / ORDER BY
	// expressions name the same pre-window values every OVER(...) written
	// inline does, so they are lifted identically.
	var windows []NamedWindow
	if len(stmt.Windows) > 0 {
		windows = make([]NamedWindow, len(stmt.Windows))
		for i, nw := range stmt.Windows {
			spec, serr := h.rewriteSpec(nw.Spec)
			if serr != nil {
				return nil, serr
			}
			windows[i] = NamedWindow{Name: nw.Name, Spec: spec}
		}
	}

	// Nothing to lift at all -- e.g. "SELECT count(*) OVER () FROM t GROUP BY
	// a", whose outer expressions reference no column and no aggregate. The
	// derived query must still produce ONE ROW PER GROUP, so it selects a
	// constant; SQLite's own rewrite appends the same literal 0 when its
	// sub-select expression list comes out empty (window.c).
	if len(h.exprs) == 0 {
		h.ref(LiteralExpr{Val: Value{Typ: Int}})
	}

	inner := &SelectStmt{
		Columns: make([]SelectColumn, len(h.exprs)),
		From:    stmt.From,
		Where:   stmt.Where,
		GroupBy: stmt.GroupBy,
		Having:  stmt.Having,
	}
	for i, e := range h.exprs {
		inner.Columns[i] = SelectColumn{Expr: e, Alias: h.names[i], HasAlias: true, Text: h.names[i]}
	}

	// compileSelectScan runs substituteResultAliases over the derived query
	// too -- SQLite's rule that a WHERE/GROUP BY/HAVING name binding to no
	// column of the FROM clause may name a select-list ALIAS instead
	// (sql_alias.go) -- but THIS select list's aliases are GENERATED, not the
	// user's. A statement that happens to name one of them ("... GROUP BY
	// _w0") would bind to a derived column where C SQLite raises "no such
	// column". So run the very same pass here, over a throwaway copy of the
	// sources (it rewrites their ON conditions in place), and decline if it
	// would change anything at all. The user's OWN aliases were already
	// substituted before this compile path was even reached, so a firing here
	// can only be a generated-name collision.
	if substituteResultAliases(inner, append([]joinSource(nil), baseSrcs...), joinScopes(baseSrcs), p) != inner {
		return nil, fmt.Errorf("%w: aggregate query with a window function whose WHERE/GROUP BY/HAVING names a generated derived column", errVDBEUnsupported)
	}

	// minMaxCensus (sql_group.go) is the ORDERED list of min()/max() call sites
	// a bare column's anchor row is decided by. The rewrite folds the outer
	// ORDER BY's call sites into the derived query's SELECT LIST, ahead of the
	// select list's own later items -- a reordering that is invisible unless a
	// statement has distinct min/max sites in more than one clause, and wrong
	// when it does. That the anchor is observable THROUGH a window at all was
	// verified directly: over (1,50,'z'),(1,10,'a'),(2,1,'m'),(2,20,'b'),
	// (3,30,'k'), "SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY max(b)"
	// is 100 (the max rows' b: 50+20+30) where the same query without that
	// ORDER BY is 81 (the first rows': 50+1+30).
	//
	// windowQueryMinMaxCensus, not minMaxCensus, is the left-hand side: a
	// min/max written in a WINDOW'S OWN spec is a call site too, and one that
	// comes LAST. See its doc comment.
	if !sameMinMaxCensus(windowQueryMinMaxCensus(stmt), minMaxCensus(inner)) {
		return nil, fmt.Errorf("%w: aggregate query with a window function whose min()/max() call sites the derived-table rewrite reorders", errVDBEUnsupported)
	}

	// The outer (window) statement. Its FROM is the derived query and nothing
	// else, so DISTINCT / ORDER BY / LIMIT / OFFSET all apply exactly where
	// they do for any other window query: after the window values exist.
	winStmt := &SelectStmt{
		Columns:     make([]SelectColumn, len(winCols)),
		Distinct:    stmt.Distinct,
		From:        []FromItem{{Subquery: inner}},
		OrderBy:     orderTerms,
		Limit:       stmt.Limit,
		Offset:      stmt.Offset,
		LimitParam:  stmt.LimitParam,
		OffsetParam: stmt.OffsetParam,
		Windows:     windows,
	}
	for i, oc := range winCols {
		winStmt.Columns[i] = SelectColumn{Expr: oc.expr, Alias: oc.name, HasAlias: true, Text: oc.name}
	}

	c := &compiler{pager: p, outer: enclosing}
	c.nQueryLoop, c.nQueryLoopKnown = inheritedNQueryLoop(enclosing)
	srcs, serr := resolveJoinSources(p, c, winStmt.From)
	if serr != nil {
		return nil, serr
	}
	c.scopes = joinScopes(srcs)
	return compileScanWindow(c, winStmt, srcs, tableScopesOf(srcs), winCols)
}

// windowQueryMinMaxCensus is minMaxCensus for an aggregate query that ALSO has
// window functions: the query's own call sites over the select list, ORDER BY
// and HAVING -- and then, after all of those, every WINDOW SPECIFICATION's own
// PARTITION BY / ORDER BY.
//
// Both halves are SQLite's own construction order. sqlite3WindowRewrite
// (window.c) runs selectWindowRewriteEList over p->pEList and p->pOrderBy
// first, which lifts each TK_AGG_FUNCTION it meets into the generated
// sub-select's expression list, and only then appends pMWin->pPartition and
// pMWin->pOrderBy to that same list. So a min/max in a spec IS one of the
// query's call sites (minMaxCensus cannot see it: collectMinMaxCalls stops at
// an OVER clause, like containsAggregate and walkExprShallow), and it sits
// after every select-list one. Verified over t(g,a,c)=(1,50,'z'),(1,10,'a'),
// (2,1,'m'),(2,20,'b'),(3,30,'k'), GROUP BY g, reading the bare c:
//
//	count(*) OVER ()                            ->  z, m, k  (first rows)
//	count(*) OVER (ORDER BY max(a))             ->  z, b, k  (max's rows)
//	count(*) OVER (ORDER BY min(a))             ->  a, m, k  (min's rows)
//	count(*) OVER (ORDER BY min(a)), max(a)     ->  a, m, k  (min's -- the SPEC
//	                                                site wins over the later
//	                                                select-list one)
//
// That last row is why this exists: with the plain census on the left the
// guard compared max(a) against the rewrite's max(a), agreed, and answered
// z, b, k -- a wrong answer, and one no window test had. It now disagrees with
// the rewrite's own order (which lifts each item's spec inline rather than
// last) and declines instead.
func windowQueryMinMaxCensus(stmt *SelectStmt) []FuncExpr {
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
	for _, c := range stmt.Columns {
		if !c.Star {
			collectSpecMinMaxCalls(c.Expr, stmt.Windows, &found)
		}
	}
	for _, ot := range stmt.OrderBy {
		collectSpecMinMaxCalls(ot.Expr, stmt.Windows, &found)
	}
	return dedupMinMaxCalls(found)
}

// collectSpecMinMaxCalls appends the min()/max() call sites written in the
// PARTITION BY / ORDER BY of every window call reachable from e -- the ones
// collectMinMaxCalls deliberately walks past. A base-window REFERENCE is
// resolved against windows first, so "OVER w" contributes w's own sites; a
// reference that does not resolve contributes nothing and is left to
// compileScanWindow's resolve to report.
func collectSpecMinMaxCalls(e Expr, windows []NamedWindow, out *[]FuncExpr) {
	walkExprShallow(e, func(fc FuncExpr) bool {
		if fc.Over == nil {
			return true
		}
		spec := fc.Over
		if spec.Ref != "" {
			r, err := resolveWindowSpec(spec, windows, 0)
			if err != nil {
				return true
			}
			spec = r
		}
		for _, pe := range spec.PartitionBy {
			collectMinMaxCalls(pe, out)
			collectSpecMinMaxCalls(pe, windows, out)
		}
		for _, ot := range spec.OrderBy {
			collectMinMaxCalls(ot.Expr, out)
			collectSpecMinMaxCalls(ot.Expr, windows, out)
		}
		return true
	})
}

// groupWindowHoist accumulates the DERIVED query's select list: every
// aggregate call and every column reference lifted out of the outer
// expressions, in appearance order, each named "_wN". Structurally identical
// lifts share one slot (exprEqual), mirroring SQLite's own
// sqlite3ExprCompare-keyed de-duplication -- it keeps the derived row narrow
// and can never be wrong, since two exprEqual expressions evaluate to the
// same value in the same group.
type groupWindowHoist struct {
	exprs []Expr
	names []string
	// noOwnFrom is set when the governing aggregate+window query has NO FROM
	// of its own (compileFromlessAggWindow's caller passes baseSrcs==nil).
	// It lets the subquery guards below skip the "names a column" refusal:
	// see their own doc comments for the citation and reasoning.
	noOwnFrom bool
	// specTerm is set while rewriteSpec is lifting a window SPECIFICATION's
	// own PARTITION BY / ORDER BY term, and it changes exactly one thing: a
	// subquery met there is lifted WHOLE instead of being refused.
	//
	// That is not a relaxation, it is C's own construction. sqlite3WindowRewrite
	// walks the outer select list and outer ORDER BY with
	// selectWindowRewriteEList (window.c:1022-1023) -- the leaf-lifting walk
	// whose sub-select rule this rewriter cannot model -- but it does NOT walk
	// the window's own specification at all: the callback PRUNES at the window
	// function that owns pMWin (window.c:774-783, "return WRC_Prune"), and the
	// spec's clauses are instead appended to the generated sub-select's
	// expression list VERBATIM,
	//
	//	window.c:1029   pSublist = exprListAppendList(pParse, pSublist, pMWin->pPartition, 0);
	//	window.c:1030   pSublist = exprListAppendList(pParse, pSublist, pMWin->pOrderBy, 0);
	//
	// so they are evaluated INSIDE the sub-select, which still carries the
	// original FROM/WHERE/GROUP BY/HAVING (window.c:968-971 captures them,
	// window.c:1068 rebuilds the sub-select from them). A correlated name in
	// there resolves against the query it was written in, and that is precisely
	// what lifting the node whole reproduces: h.ref puts it in the derived
	// query's select list, whose FROM is that same original FROM.
	//
	// Lifting the SUBQUERY NODE rather than the whole term is equivalent to
	// appending the whole term, because every operator left outside is a
	// deterministic function of values that are themselves lifted -- the same
	// argument every other case in rewrite already rests on.
	specTerm bool
}

// ref lifts e into the derived select list (or reuses an existing slot) and
// returns the reference the outer query reads it back through.
//
// A derived-table column has no DECLARED collation, so a naive "_wN" would
// run every outer comparison BINARY even when e carried an EXPLICIT one --
// "max(b COLLATE nocase)" concatenated with the empty string lifts to "_w0"
// concatenated with the empty string, losing the NOCASE C SQLite's
// exprCollation-style propagation (sqlite3ExprCollSeq) still applies to the
// ORIGINAL expression. exprCollation is the exact function this
// engine's own comparisons already consult for that propagation, so wrapping
// the substituted reference in the SAME explicit COLLATE it reports
// reproduces it: the wrapped reference's own exprCollation is then identical
// to e's, for every caller (ORDER BY, DISTINCT, a window's PARTITION BY /
// ORDER BY) that walks the rewritten tree the same way. This changes only
// which collating sequence a COMPARISON uses, never any value, so it is
// sound regardless of dedup (two exprEqual lifts still share one slot; each
// reference is wrapped independently). Verified directly over
// ('abcd'),('BCDE'),('cdef'),('DEFG'): a query selecting count() OVER (),
// rowid and max(b COLLATE nocase) concatenated with the empty string, grouped
// by rowid and ordered by that same concatenation, answers abcd,BCDE,cdef,
// DEFG -- see TestGroupedWindowCollateAnswers (compat-harness) for the exact
// statement text.
func (h *groupWindowHoist) ref(e Expr) Expr {
	var ref Expr
	name := ""
	for i, x := range h.exprs {
		if exprEqual(x, e) {
			name = h.names[i]
			break
		}
	}
	if name == "" {
		name = fmt.Sprintf("_w%d", len(h.exprs))
		h.exprs = append(h.exprs, e)
		h.names = append(h.names, name)
	}
	ref = ColumnExpr{Name: name}
	if n, ok := exprCollation(e); ok && !equalFoldName(n, "BINARY") {
		ref = CollateExpr{X: ref, Name: n}
	}
	return ref
}

// rewrite returns e with every aggregate call and every column reference
// replaced by a reference to a derived-query column (lifting each on the
// way), leaving the window calls themselves -- and everything the outer query
// computes per window row (operators, scalar functions, CASE, literals) --
// where they are. A node kind it cannot classify is declined rather than
// passed through: passing one through would leave a reference to a table the
// outer query no longer has in its FROM.
func (h *groupWindowHoist) rewrite(e Expr) (Expr, error) {
	switch x := e.(type) {
	case nil:
		return nil, nil
	case LiteralExpr, ParamExpr:
		return e, nil
	case ColumnExpr:
		return h.ref(x), nil
	case groupBareColExpr:
		// A bare reference to the GROUP's anchor row, already resolved by the
		// aggregate-item substitution pass. It lifts exactly like a ColumnExpr
		// and for the same reason: C's selectWindowRewriteExprCb (window.c:756-771)
		// lifts the TK_COLUMN nodes that name the OUTER SrcList out of the
		// window's own expressions, and this node IS such a reference -- one
		// that an earlier pass already bound to the anchor row. It is constant
		// across the whole partition, because a group has one anchor row, so
		// evaluating it once outside the window is not merely safe but exact.
		//
		// It used to fall through to the default arm and decline. Nothing was
		// wrong with the shape; the rewriter simply had no case for a node this
		// engine synthesizes, so "SELECT x, (SELECT max(w2.x) OVER (PARTITION BY
		// sum((SELECT w2.y)))) FROM w2" -- which 3.53.3 answers (1000, 1000) --
		// was refused.
		return h.ref(x), nil
	case FuncExpr:
		switch {
		case x.Over != nil:
			// A window call stays in the outer query; its ARGUMENTS, FILTER
			// and its own PARTITION BY / ORDER BY name pre-window values, so
			// they are lifted. This is also what makes an aggregate inside a
			// window's own ORDER BY exact here even though compileScanWindow
			// declines that shape for a GROUP-BY-less query: with a GROUP BY
			// the two-stage evaluation it could not model IS the rewrite --
			// "SELECT a, sum(b) OVER (ORDER BY count(*)) FROM t GROUP BY a"
			// over the table in this file's package comment is
			// (3,30),(1,81),(2,81), matching C SQLite exactly.
			out := x
			args, err := h.rewriteList(x.Args)
			if err != nil {
				return nil, err
			}
			out.Args = args
			f, err := h.rewrite(x.Filter)
			if err != nil {
				return nil, err
			}
			out.Filter = f
			spec, err := h.rewriteSpec(x.Over)
			if err != nil {
				return nil, err
			}
			out.Over = spec
			return out, nil
		case isAggregateCall(x):
			// A window call inside an aggregate's argument ("sum(sum(b) OVER
			// ())") is "misuse of window function sum()" in C SQLite
			// (verified directly), so it is never lifted into the derived
			// query, where it would be planned as a plain nested aggregate.
			if exprHasWindow(x) {
				return nil, fmt.Errorf("%w: window function inside an aggregate's argument", errVDBEUnsupported)
			}
			return h.ref(x), nil
		case x.Filter != nil || len(x.OrderBy) > 0:
			// FILTER belongs to an aggregate or a window aggregate; anywhere
			// else this compiler has no rule for it.
			return nil, fmt.Errorf("%w: FILTER on a non-aggregate function combined with an aggregate query and a window", errVDBEUnsupported)
		default:
			out := x
			args, err := h.rewriteList(x.Args)
			if err != nil {
				return nil, err
			}
			out.Args = args
			return out, nil
		}
	case UnaryExpr:
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		x.X = xx
		return x, nil
	case BinaryExpr:
		l, err := h.rewrite(x.L)
		if err != nil {
			return nil, err
		}
		r, err := h.rewrite(x.R)
		if err != nil {
			return nil, err
		}
		x.L, x.R = l, r
		return x, nil
	case IsNullExpr:
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		x.X = xx
		return x, nil
	case InExpr:
		if x.Sub != nil && h.specTerm {
			// Inside a window spec the whole "<x> IN (subquery)" node is lifted
			// rather than walked -- see groupWindowHoist.specTerm. It has to be
			// the WHOLE node here rather than the subquery alone: x.Sub is a
			// *SelectStmt, not an Expr, so there is nothing narrower to lift,
			// and the left operand is an ordinary liftable expression anyway.
			return h.ref(x), nil
		}
		if x.Sub != nil && !h.noOwnFrom && !subqueryNamesNoColumn(x.Sub) {
			return nil, fmt.Errorf("%w: subquery combined with an aggregate query and a window function", errVDBEUnsupported)
		}
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		list, err := h.rewriteList(x.List)
		if err != nil {
			return nil, err
		}
		x.X, x.List = xx, list
		return x, nil
	case BetweenExpr:
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		lo, err := h.rewrite(x.Lo)
		if err != nil {
			return nil, err
		}
		hi, err := h.rewrite(x.Hi)
		if err != nil {
			return nil, err
		}
		x.X, x.Lo, x.Hi = xx, lo, hi
		return x, nil
	case LikeExpr:
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		pat, err := h.rewrite(x.Pattern)
		if err != nil {
			return nil, err
		}
		esc, err := h.rewrite(x.Escape)
		if err != nil {
			return nil, err
		}
		x.X, x.Pattern, x.Escape = xx, pat, esc
		return x, nil
	case GlobExpr:
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		pat, err := h.rewrite(x.Pattern)
		if err != nil {
			return nil, err
		}
		x.X, x.Pattern = xx, pat
		return x, nil
	case CollateExpr:
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		x.X = xx
		return x, nil
	case CastExpr:
		xx, err := h.rewrite(x.X)
		if err != nil {
			return nil, err
		}
		x.X = xx
		return x, nil
	case CaseExpr:
		base, err := h.rewrite(x.Base)
		if err != nil {
			return nil, err
		}
		whens := make([]WhenClause, len(x.Whens))
		for i, w := range x.Whens {
			cond, err := h.rewrite(w.When)
			if err != nil {
				return nil, err
			}
			then, err := h.rewrite(w.Then)
			if err != nil {
				return nil, err
			}
			whens[i] = WhenClause{When: cond, Then: then}
		}
		els, err := h.rewrite(x.Else)
		if err != nil {
			return nil, err
		}
		x.Base, x.Whens, x.Else = base, whens, els
		return x, nil
	case SubqueryExpr:
		if h.specTerm {
			return h.ref(x), nil // see groupWindowHoist.specTerm
		}
		if !h.noOwnFrom && !subqueryNamesNoColumn(x.Stmt) {
			return nil, fmt.Errorf("%w: subquery combined with an aggregate query and a window function", errVDBEUnsupported)
		}
		return x, nil
	case ExistsExpr:
		if h.specTerm {
			return h.ref(x), nil // see groupWindowHoist.specTerm
		}
		if !h.noOwnFrom && !subqueryNamesNoColumn(x.Stmt) {
			return nil, fmt.Errorf("%w: subquery combined with an aggregate query and a window function", errVDBEUnsupported)
		}
		return x, nil
	default:
		// RowExpr/MatchExpr/RaiseExpr: see this file's decline list.
		return nil, fmt.Errorf("%w: expression combined with an aggregate query and a window function", errVDBEUnsupported)
	}
}

// subqueryNamesNoColumn reports whether stmt -- a subquery reached while lifting
// an aggregate query's expressions into the derived table -- names no column at
// all, anywhere, so it can be left OUTSIDE the lift verbatim.
//
// That is exactly what C SQLite does with the subquery itself:
// selectWindowRewriteExprCb (window.c), once p->pSubSelect is set, "does not
// process aggregates or window functions at all, as they belong to the scalar
// sub-select", and lifts only the TK_COLUMN nodes that refer to the OUTER
// SrcList. A subquery holding no column reference has nothing to lift, so
// leaving it where it is reproduces SQLite exactly -- it is then evaluated once
// per window row, which for a column-free body is the same value every time.
//
// The rest -- lifting a correlated column OUT of a subquery, which for a GROUP
// BY query means reading the group's anchor row -- would have to know whether
// each name binds to this query's FROM or to the subquery's own, which needs
// that FROM resolved (views, CTEs and derived tables included). So anything
// naming a column at all is refused, along with "SELECT *" (which names every
// column of whatever it scans) and any expression node this walk does not
// recognize: over-refusing costs a decline, under-refusing is a wrong answer.
//
// groupWindowHoist.noOwnFrom is the one case where that ambiguity provably
// cannot arise, so callers skip this check entirely rather than call it: with
// NO FROM of its own (compileFromlessAggWindow's baseSrcs==nil, e.g. "SELECT
// max(1 IN(SELECT x ...)) OVER (PARTITION BY sum(...))" with no FROM anywhere
// in that arm), there is no "this query's own FROM" a column could bind to in
// the first place. selectWindowRewriteExprCb (window.c:759-770) confirms this
// is exactly what C SQLite does, not an approximation of it: once inside a
// nested sub-select (p->pSubSelect set), it lifts a TK_COLUMN only if its
// cursor matches one of p->pSrc's entries (window.c:763-768) --
// "for(i=0;i<nSrc;i++) ... if(i==nSrc) return WRC_Continue" -- and with
// p->pSrc->nSrc==0 (the FROM-less case: sqlite3WindowRewrite captures the
// ORIGINAL p->pSrc into a local (window.c:968) before zeroing the field
// itself at window.c:994, and a FROM-less Select's pSrc has zero entries to
// begin with), that loop is vacuous for every column, on every pass, so
// NOTHING inside such a subquery is ever lifted -- it is left exactly where
// it is, which is what leaving x untouched below already does.
func subqueryNamesNoColumn(stmt *SelectStmt) bool {
	if stmt == nil {
		return true
	}
	for _, c := range stmt.Columns {
		if c.Star || !exprNamesNoColumn(c.Expr) {
			return false
		}
	}
	for _, it := range stmt.From {
		if !exprNamesNoColumn(it.On) || !subqueryNamesNoColumn(it.Subquery) {
			return false
		}
	}
	if !exprNamesNoColumn(stmt.Where) || !exprNamesNoColumn(stmt.Having) {
		return false
	}
	for _, g := range stmt.GroupBy {
		if !exprNamesNoColumn(g) {
			return false
		}
	}
	for _, ot := range stmt.OrderBy {
		if !exprNamesNoColumn(ot.Expr) {
			return false
		}
	}
	for _, nw := range stmt.Windows {
		if nw.Spec == nil {
			continue
		}
		for _, pe := range nw.Spec.PartitionBy {
			if !exprNamesNoColumn(pe) {
				return false
			}
		}
		for _, ot := range nw.Spec.OrderBy {
			if !exprNamesNoColumn(ot.Expr) {
				return false
			}
		}
	}
	for _, arm := range stmt.Compound {
		if !subqueryNamesNoColumn(arm.Stmt) {
			return false
		}
	}
	for _, cte := range stmt.CTEs {
		if !subqueryNamesNoColumn(cte.Select) {
			return false
		}
	}
	return true
}

// exprNamesNoColumn is subqueryNamesNoColumn's expression half. Like it, it is
// FAIL-SAFE: an unrecognized node answers "names a column".
func exprNamesNoColumn(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return true
	case ColumnExpr:
		return false
	case SubqueryExpr:
		return subqueryNamesNoColumn(x.Stmt)
	case ExistsExpr:
		return subqueryNamesNoColumn(x.Stmt)
	case FuncExpr:
		for _, a := range x.Args {
			if !exprNamesNoColumn(a) {
				return false
			}
		}
		if !exprNamesNoColumn(x.Filter) {
			return false
		}
		for _, ob := range x.orderByExprs() {
			if !exprNamesNoColumn(ob) {
				return false
			}
		}
		if x.Over != nil {
			for _, pe := range x.Over.PartitionBy {
				if !exprNamesNoColumn(pe) {
					return false
				}
			}
			for _, ot := range x.Over.OrderBy {
				if !exprNamesNoColumn(ot.Expr) {
					return false
				}
			}
		}
		return true
	case UnaryExpr:
		return exprNamesNoColumn(x.X)
	case BinaryExpr:
		return exprNamesNoColumn(x.L) && exprNamesNoColumn(x.R)
	case IsNullExpr:
		return exprNamesNoColumn(x.X)
	case InExpr:
		if !exprNamesNoColumn(x.X) || !subqueryNamesNoColumn(x.Sub) {
			return false
		}
		for _, it := range x.List {
			if !exprNamesNoColumn(it) {
				return false
			}
		}
		return true
	case BetweenExpr:
		return exprNamesNoColumn(x.X) && exprNamesNoColumn(x.Lo) && exprNamesNoColumn(x.Hi)
	case LikeExpr:
		return exprNamesNoColumn(x.X) && exprNamesNoColumn(x.Pattern) && exprNamesNoColumn(x.Escape)
	case GlobExpr:
		return exprNamesNoColumn(x.X) && exprNamesNoColumn(x.Pattern)
	case CollateExpr:
		return exprNamesNoColumn(x.X)
	case CastExpr:
		return exprNamesNoColumn(x.X)
	case CaseExpr:
		if !exprNamesNoColumn(x.Base) || !exprNamesNoColumn(x.Else) {
			return false
		}
		for _, w := range x.Whens {
			if !exprNamesNoColumn(w.When) || !exprNamesNoColumn(w.Then) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (h *groupWindowHoist) rewriteList(es []Expr) ([]Expr, error) {
	if len(es) == 0 {
		return es, nil
	}
	out := make([]Expr, len(es))
	for i, e := range es {
		x, err := h.rewrite(e)
		if err != nil {
			return nil, err
		}
		out[i] = x
	}
	return out, nil
}

// rewriteSpec lifts a window specification's PARTITION BY / ORDER BY
// expressions. A base-window REFERENCE (Ref) is left alone -- the named
// window it points at is lifted in its own right -- and so is the FRAME: real
// SQLite evaluates a frame bound's offset with NO row in scope, so a column
// reference there is an error ("frame starting offset must be a non-negative
// integer") in both engines, and lifting it would turn that error into an
// answer (see planWindowFrameOffsets, vdbe_window_codegen.go, which reproduces
// sqlite3WindowOffsetExpr's own parse-time NULL substitution -- window.c:1163).
func (h *groupWindowHoist) rewriteSpec(s *WindowSpec) (*WindowSpec, error) {
	if s == nil {
		return nil, nil
	}
	// PARTITION BY and ORDER BY are the two clauses C appends to its generated
	// sub-select VERBATIM (window.c:1029-1030) instead of walking, which is
	// what lets a subquery in one of them be lifted whole rather than refused.
	// Saved and restored rather than merely set, since rewriteSpec is reached
	// from rewrite's own window-call arm and must not leak the mode back out
	// to the expression around that call. The FRAME below is deliberately NOT
	// covered -- it is not appended to the sub-select at all.
	savedSpec := h.specTerm
	h.specTerm = true
	defer func() { h.specTerm = savedSpec }()
	out := *s
	part, err := h.rewriteList(s.PartitionBy)
	if err != nil {
		return nil, err
	}
	out.PartitionBy = part
	if len(s.OrderBy) > 0 {
		terms := make([]OrderTerm, len(s.OrderBy))
		for i, ot := range s.OrderBy {
			e, oerr := h.rewrite(ot.Expr)
			if oerr != nil {
				return nil, oerr
			}
			ot.Expr = e
			terms[i] = ot
		}
		out.OrderBy = terms
	}
	return &out, nil
}
