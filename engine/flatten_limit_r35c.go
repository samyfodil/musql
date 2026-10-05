// The observable halves of SQLite's query flattener (flattenSubquery,
// select.c). This package materializes a FROM-clause subquery either way, so
// most of flattening is invisible -- except two effects that change answers.
//
// The LIMIT transfer. select.c:4695 moves the subquery's LIMIT to the parent:
//
//	if( pSub->pLimit ){ pParent->pLimit = pSub->pLimit; pSub->pLimit = 0; }
//
// so it applies after the parent's ORDER BY. Over t = 1..5,
// "SELECT * FROM (SELECT a FROM t LIMIT 2) ORDER BY 1 DESC" is 5, 4, not 2, 1.
// It is ported only where it changes the answer (an outer ORDER BY), and every
// select.c restriction that blocks it blocks it here too:
//
//	sub ORDER BY too                (11)
//	sub OFFSET                      (14)
//	outer LIMIT                     (13)
//	outer WHERE                     (19)
//	outer DISTINCT                  (21)
//	outer aggregate / GROUP BY      (9)
//	outer is a join (>1 FROM item)  (8)
//	sub DISTINCT                    (4)
//	sub has no FROM clause          (7)
//	outer is a compound arm         (15)
//	either side has a window fn     (25)
//
// It also covers a compound subquery (restriction 17, r37cCompoundFlattenable)
// and a stored view's body (r37cExpandViewRef). The second effect is the plan:
// see r37cFlattenTransparent.
package engine

// r35cFlattenSubqueryLimit returns stmt with its single FROM-clause subquery's
// LIMIT transferred onto stmt itself, when SQLite's flattener would do the
// same. It returns stmt unchanged (the same pointer) in every other case, so a
// caller can apply it unconditionally.
//
// Both the statement and the subquery are COPIED before being modified: the
// same *SelectStmt AST node is reentered for a correlated subquery, once per
// enclosing row, and must never accumulate a rewrite.
func r35cFlattenSubqueryLimit(stmt *SelectStmt) *SelectStmt {
	return r35cFlattenLimit(nil, stmt)
}

// r35cFlattenLimit is r35cFlattenSubqueryLimit with the pager restriction (17)
// needs. p may be nil -- the compiler's own call site has no pager to offer --
// in which case a COMPOUND subquery is left alone exactly as before.
func r35cFlattenLimit(p *ReadOnlyPager, stmt *SelectStmt) *SelectStmt {
	if stmt == nil || len(stmt.OrderBy) == 0 {
		// Without an outer ORDER BY the parent takes the same prefix whichever
		// side holds the LIMIT, so moving it ALONE changes nothing. It is not
		// unobservable, though -- flattening also changes the PLAN, and
		// r37cFlattenTransparent's own path handles exactly that pairing (a
		// transparent body that also carries a LIMIT).
		return stmt
	}
	if !r38cOuterAllowsLimitTransfer(stmt) {
		return stmt
	}
	it := stmt.From[0]
	sub := it.Subquery
	if sub == nil || sub.Limit == nil || sub.LimitParam != nil {
		return stmt
	}
	if sub.Offset != nil || sub.OffsetParam != nil { // (14)
		return stmt
	}
	if sub.Distinct { // (4)
		return stmt
	}
	if len(sub.From) == 0 { // (7)
		return stmt
	}
	if len(sub.OrderBy) != 0 { // (11)
		return stmt
	}
	if len(sub.Compound) != 0 && !p.r37cCompoundFlattenable(stmt, sub) { // (17), (18), (20)
		return stmt
	}
	for _, c := range sub.Columns { // (25)
		if !c.Star && exprHasWindow(c.Expr) {
			return stmt
		}
	}

	subCopy := *sub
	subCopy.Limit = nil
	itemCopy := it
	itemCopy.Subquery = &subCopy
	out := *stmt
	out.From = []FromItem{itemCopy}
	out.Limit = sub.Limit
	return &out
}

// r38cOuterAllowsLimitTransfer is every restriction flattenSubquery puts on the
// OUTER query before a FROM-clause subquery's LIMIT may move onto it. Split out
// of r35cFlattenLimit because r37cFlattenTransparent needs the same list for the
// pairing r35cFlattenLimit's own ORDER BY guard skips: a TRANSPARENT body that
// also carries a LIMIT, where the transfer is a no-op on its own but the base
// substitution beside it changes the plan.
func r38cOuterAllowsLimitTransfer(stmt *SelectStmt) bool {
	if len(stmt.From) != 1 || len(stmt.Compound) != 0 { // (8), (15)
		return false
	}
	// A lone FROM item carries no join connector, so these are all already
	// zero -- asserted rather than assumed, since the rewrite's whole safety
	// argument is that the parent reads exactly the subquery's rows in order.
	it := stmt.From[0]
	if it.On != nil || len(it.Using) != 0 || it.Natural || it.GroupLen != 0 {
		return false
	}
	if stmt.Limit != nil || stmt.LimitParam != nil || stmt.Offset != nil || stmt.OffsetParam != nil { // (13)
		return false
	}
	if stmt.Where != nil { // (19)
		return false
	}
	if stmt.Distinct { // (21)
		return false
	}
	if len(stmt.GroupBy) != 0 || stmt.Having != nil { // (9), via SF_Aggregate
		return false
	}
	// (9) again, and (25): an aggregate anywhere in the outer query makes it
	// SF_Aggregate; a window function on either side stops flattening outright.
	for _, c := range stmt.Columns {
		if c.Star {
			continue
		}
		if containsAggregate(c.Expr) || exprHasWindow(c.Expr) {
			return false
		}
	}
	for _, ot := range stmt.OrderBy {
		if containsAggregate(ot.Expr) || exprHasWindow(ot.Expr) {
			return false
		}
	}
	return true
}

// r37cCompoundFlattenable is flattenSubquery's restriction (17) -- the extra
// conditions a compound FROM-clause subquery must meet before its LIMIT moves
// to the parent -- plus (18) and (20). sub is stmt's single FROM subquery and
// carries a LIMIT; what r35cFlattenLimit already checked is not rechecked.
//
// e.g. over t=1..5, u=10,20,
// "SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3) ORDER BY 1 DESC"
// is 20, 10, 5.
//
// A nil pager cannot answer (17h), so it declines.
func (p *ReadOnlyPager) r37cCompoundFlattenable(stmt, sub *SelectStmt) bool {
	if p == nil {
		return false
	}
	// (23): the flattener refuses when the OUTER query is a recursive CTE,
	// because rewriting the parent into a compound confuses multiSelect's
	// recursion handling. Which CTE body this compile belongs to is not
	// available here, so any compile reached from inside one declines.
	if len(p.cteExpansion) != 0 {
		return false
	}
	arms := make([]*SelectStmt, 0, len(sub.Compound)+1)
	arms = append(arms, sub)
	for _, arm := range sub.Compound {
		if arm.Op != "UNION ALL" { // (17a)
			return false
		}
		if arm.Stmt == nil {
			return false
		}
		arms = append(arms, arm.Stmt)
	}
	// A multi-row VALUES clause is stored as leading UNION ALL arms of its own
	// (SelectStmt.ValuesArms) but is ONE arm to SQLite, and a VALUES arm has no
	// FROM clause -- (17c) rejects it either way, but only if it is seen as an
	// arm at all. Rejected outright rather than folded.
	if sub.ValuesArms != 0 {
		return false
	}
	for _, a := range arms {
		if a.Distinct || len(a.GroupBy) != 0 || a.Having != nil { // (17b)
			return false
		}
		if len(a.From) == 0 { // (17c)
			return false
		}
		for _, it := range a.From {
			if it.Join == JoinRight || it.Join == JoinFull { // (17g), (27b)
				return false
			}
		}
		for _, c := range a.Columns {
			if c.Star {
				continue
			}
			if containsAggregate(c.Expr) { // (17b)
				return false
			}
			if exprHasWindow(c.Expr) { // (17e)
				return false
			}
		}
	}
	// (17h): the corresponding result-set expressions in all arms must have the
	// SAME affinity -- compoundHasDifferentAffinities (select.c:4092), which
	// compares raw sqlite3ExprAffinity values.
	first, ok := p.r37cArmAffinities(sub)
	if !ok {
		return false
	}
	for _, a := range arms[1:] {
		affs, aok := p.r37cArmAffinities(a)
		if !aok || len(affs) != len(first) {
			return false
		}
		for i := range affs {
			if affs[i] != first[i] {
				return false
			}
		}
	}
	// (18): every term of the parent's ORDER BY must be a copy of a term the
	// parent RETURNS (sqlite3 sets ExprList.a[].u.x.iOrderByCol only then). The
	// two spellings that always are: an ordinal, and a bare column name over a
	// select list that is itself nothing but bare column names (or a lone "*",
	// which returns every column of the sole FROM item).
	star := len(stmt.Columns) == 1 && stmt.Columns[0].Star && stmt.Columns[0].StarQualifier == ""
	names := map[string]bool{}
	if !star {
		for _, c := range stmt.Columns {
			if c.Star {
				return false
			}
			if c.HasAlias {
				names[r33sFoldIdent(c.Alias)] = true
				continue
			}
			col, isCol := c.Expr.(ColumnExpr)
			if !isCol {
				return false
			}
			names[r33sFoldIdent(col.Name)] = true
		}
	}
	for _, ot := range stmt.OrderBy {
		switch x := ot.Expr.(type) {
		case LiteralExpr:
			if x.Val.Typ != Int {
				return false
			}
		case ColumnExpr:
			if x.Qualifier != "" || x.Schema != "" {
				return false
			}
			if !star && !names[r33sFoldIdent(x.Name)] {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// r37cRawAffinity is an output expression's affinity as restriction (17h)
// compares it: sqlite3ExprAffinity's raw byte, where "not a column" is 0,
// distinct from SQLITE_AFF_BLOB. This package's own affinity folds NONE and
// BLOB together, so a text literal and an untyped column look alike -- and
// (17h) is where that shows: "SELECT a FROM t UNION ALL SELECT 'x' FROM u"
// does not flatten, while "... SELECT b FROM u" does.
type r37cRawAffinity struct {
	isColumn bool
	aff      affinity // meaningful only when isColumn
}

// r37cArmAffinities is one compound arm's per-output r37cRawAffinity, or false
// for an arm it cannot classify faithfully. A column reference takes its
// declared affinity (the NONE/BLOB fold is harmless there, since
// sqlite3TableColumnAffinity never returns NONE). Anything else must be a node
// sqlite3ExprAffinity returns 0 for, which rules out TK_CAST, TK_SELECT,
// TK_VECTOR, TK_COLLATE and TK_FUNCTION; an arm holding one declines.
func (p *ReadOnlyPager) r37cArmAffinities(core *SelectStmt) ([]r37cRawAffinity, bool) {
	if core == nil {
		return nil, false
	}
	for _, it := range core.From {
		// A column of a DERIVED table gets its affinity from
		// sqlite3SubqueryColumnTypes rather than from a declaration; keep this
		// to the case where both engines read one straight off the schema.
		if it.Subquery != nil || it.TableFunc || it.Table == "" {
			return nil, false
		}
	}
	jts, _, err := p.resolveFrom(core.From, nil)
	if err != nil {
		return nil, false
	}
	scopes := buildScopes(jts)
	outCols, err := expandSelectList(core.Columns, scopes, defaultColNameMode)
	if err != nil {
		return nil, false
	}
	ctx := &evalCtx{tables: scopes}
	out := make([]r37cRawAffinity, len(outCols))
	for i, oc := range outCols {
		switch oc.expr.(type) {
		case ColumnExpr:
			out[i] = r37cRawAffinity{isColumn: true, aff: exprAffinity(ctx, oc.expr)}
		case LiteralExpr, ParamExpr, UnaryExpr, BinaryExpr, IsNullExpr,
			InExpr, BetweenExpr, LikeExpr, GlobExpr, MatchExpr, CaseExpr, ExistsExpr:
			out[i] = r37cRawAffinity{} // sqlite3ExprAffinity returns affExpr == 0
		default:
			return nil, false
		}
	}
	return out, true
}

// r37cFlattenTransparent is the other observable half of flattenSubquery: row
// order. This package scans a materialized derived table in arrival order;
// SQLite merges the subquery into its parent and plans the result, so the
// parent's WHERE can drive an index on the base table. e.g. with an index on
// t(a,b,c), "SELECT a FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3) LIMIT 2"
// returns rows in index order, exactly as "FROM t AS d" does.
//
// Only a transparent body -- "SELECT * FROM <ordinary table>" -- is rewritten,
// since that cannot change which rows or columns exist, only the plan.
// Anything more would need flattenSubquery's full expression substitution.
//
// Restrictions from flattenSubquery's list (the rest are vacuous for a
// transparent body):
//
//	(3a)(3d)(26) not the right operand of an outer join, nor in the left
//	             operand of a RIGHT JOIN (skipped outright here)
//	(22)(28)     not a CTE reference (recursive or MATERIALIZED)
//	(25)         no window function in the parent's select list or ORDER BY
//
// Two extra restrictions, because SQLite resolves names before flattening and
// this package flattens before resolving:
//
//   - rowid: a derived table has no rowid, so "SELECT rowid FROM (SELECT * FROM
//     t)" is an error in C; any rowid reference disables the rewrite.
//   - full_column_names=ON with short_column_names=OFF names the subquery's
//     columns "t.a", so "SELECT a FROM (...)" is an error in C. Only the
//     default naming mode is rewritten.
func (p *ReadOnlyPager) r37cFlattenTransparent(stmt *SelectStmt) *SelectStmt {
	if p == nil || stmt == nil || len(stmt.From) == 0 {
		return stmt
	}
	if p.fullColumnNames || p.shortColumnNamesOff {
		return stmt
	}
	// No outer join anywhere in the FROM clause -- narrower than (26)/(3),
	// because this planner already disagrees with SQLite on an outer join with
	// no derived table in it (it treats a LEFT JOIN's ON conjunct as a filter on
	// the left table). Flattening into such a join would trade an agreeing answer
	// for a wrong one. Widen only together with that planner gap.
	for _, it := range stmt.From {
		if it.Join == JoinLeft || it.Join == JoinRight || it.Join == JoinFull {
			return stmt
		}
	}
	for _, c := range stmt.Columns { // (25)
		if !c.Star && exprHasWindow(c.Expr) {
			return stmt
		}
	}
	for _, ot := range stmt.OrderBy { // (25)
		if exprHasWindow(ot.Expr) {
			return stmt
		}
	}
	if r37cMayReferenceRowid(stmt) {
		return stmt
	}
	out := stmt
	for i := range stmt.From {
		it := stmt.From[i]
		if it.Subquery == nil {
			// A CTE reference and a stored VIEW are BOTH turned into exactly this
			// FROM-clause subquery before flattenSubquery ever sees it -- by
			// resolveFromTermToCte and sqlite3SelectExpand respectively -- so the same
			// rewrite applies to each. See r38cExpandCTERef / r37cExpandViewRef.
			// The CTE is tried first: it is a map probe, where the view lookup
			// walks the schema and re-parses a stored CREATE, and a CTE shadows a
			// same-named view anyway.
			ex, exOK := p.r38cExpandCTERef(it)
			if !exOK {
				ex, exOK = p.r37cExpandViewRef(it)
			}
			if !exOK {
				continue
			}
			it = ex
		}
		base, lim, ok := p.r37cTransparentBase(it)
		if !ok {
			continue
		}
		// A transparent body may carry its own LIMIT, which flattening moves onto
		// the parent (select.c:4695) -- and only then, because the substitution
		// beside it is what makes the move observable at all. Every restriction
		// the outer query must meet is r35cFlattenLimit's own list.
		if lim != nil && !r38cOuterAllowsLimitTransfer(stmt) {
			continue
		}
		if out == stmt {
			cp := *stmt
			cp.From = append([]FromItem(nil), stmt.From...)
			out = &cp
		}
		out.From[i] = base
		if lim != nil {
			out.Limit = lim
		}
	}
	if out != stmt {
		return out
	}
	// The LIMIT-transfer half runs in the compiler too (compileScanAttempt,
	// vdbe_scan.go), but with no pager -- so restriction (17h)'s cross-arm
	// affinity comparison, and a stored view's or CTE's body, are only reachable
	// from here.
	if f := r35cFlattenLimit(p, stmt); f != stmt {
		return f
	}
	if len(stmt.From) == 1 && stmt.From[0].Subquery == nil && len(stmt.OrderBy) > 0 {
		// Offer the transfer the EXPANDED view/CTE, and keep the expansion only
		// if it actually fires: expanding for nothing would trade viewOutputRows'
		// own column naming and "no such table: main.x" error text (and, for a
		// CTE, applyCTEColNames' own duplicate-name decline) for a plain derived
		// table's, for no gain.
		ex, ok := p.r38cExpandCTERef(stmt.From[0])
		if !ok {
			ex, ok = p.r37cExpandViewRef(stmt.From[0])
		}
		if ok {
			cand := *stmt
			cand.From = []FromItem{ex}
			if f := r35cFlattenLimit(p, &cand); f != &cand {
				return f
			}
		}
	}
	return stmt
}

// r38cFlattenCompoundArms applies the transparent rewrite to every arm of a
// compound SELECT. SQLite prepares each arm as its own select-core, so
// flattenSubquery reaches a subquery inside an arm just as in a lone SELECT;
// r37cFlattenTransparent's call site is past the compound dispatch and never
// sees an arm.
//
// The LIMIT half does not reach an arm: restriction (15) blocks it when the
// outer query is part of a compound.
//
// Only the sequential compound (UNION ALL without ORDER BY) is rewritten.
// multiSelect (select.c:2935) merges every other compound, imposing the ORDER
// BY on each arm separately; this package concatenates and stable-sorts. They
// agree on rows but not on the order of equal keys, and changing an arm's
// plan changes its arrival order -- which would widen that gap. Porting the
// merge is what unlocks widening this.
func (p *ReadOnlyPager) r38cFlattenCompoundArms(stmt *SelectStmt) *SelectStmt {
	if p == nil || stmt == nil || len(stmt.Compound) == 0 {
		return stmt
	}
	if len(stmt.OrderBy) != 0 {
		return stmt
	}
	for _, arm := range stmt.Compound {
		if arm.Op != "UNION ALL" {
			return stmt
		}
	}
	out := stmt
	if from, ok := p.r38cFlattenedArmFrom(stmt, stmt); ok {
		cp := *stmt
		cp.From = from
		out = &cp
	}
	for i := range stmt.Compound {
		arm := stmt.Compound[i].Stmt
		if arm == nil {
			continue
		}
		from, ok := p.r38cFlattenedArmFrom(stmt, arm)
		if !ok {
			continue
		}
		if out == stmt {
			cp := *stmt
			out = &cp
		}
		if &out.Compound[0] == &stmt.Compound[0] {
			out.Compound = append([]CompoundArm(nil), stmt.Compound...)
		}
		armCopy := *arm
		armCopy.From = from
		out.Compound[i].Stmt = &armCopy
	}
	return out
}

// r38cFlattenedArmFrom is core's FROM clause with every transparent derived
// table replaced by its base table, or ok=false when nothing changed. whole is
// the compound core belongs to.
//
// The synthetic statement handed to r37cFlattenTransparent carries the whole
// compound's tail. Carrying the Compound list matters twice: it makes
// restriction (15) fire (no arm can take a subquery's LIMIT), and it exposes
// the sibling arms to the (25) window and rowid scans.
func (p *ReadOnlyPager) r38cFlattenedArmFrom(whole, core *SelectStmt) ([]FromItem, bool) {
	if core == nil || len(core.From) == 0 {
		return nil, false
	}
	syn := *core
	syn.Compound = whole.Compound
	syn.OrderBy = whole.OrderBy
	syn.Limit, syn.LimitParam = whole.Limit, whole.LimitParam
	syn.Offset, syn.OffsetParam = whole.Offset, whole.OffsetParam
	f := p.r37cFlattenTransparent(&syn)
	if f == &syn {
		return nil, false
	}
	return f.From, true
}

// r38cExpandCTERef turns a FROM item naming an in-scope CTE into the derived
// table resolveFromTermToCte (select.c) makes of it, so the flattener's
// rewrites reach the CTE spelling as r37cExpandViewRef reaches the view one.
// CTE references and inline subqueries are already named alike here, so no
// naming reconciliation is needed. Its own restrictions:
//
//   - "AS MATERIALIZED" is an optimization fence (select.c:7797, restriction
//     28) and is never flattened. "NOT MATERIALIZED" and a CTE used twice
//     flatten like the default (nUse>=2 only picks coroutine vs transient
//     table, fromClauseTermCanBeCoroutine).
//   - restriction (22): a recursive CTE has no derived-table form here.
//
// The body must resolve in exactly the enclosing statement's scope. A CTE body
// normally resolves at its defining depth (cteBinding.scopeDepth), so a deeper
// caller declines rather than risk binding the body's FROM to the wrong
// same-named CTE.
func (p *ReadOnlyPager) r38cExpandCTERef(it FromItem) (FromItem, bool) {
	if p == nil || it.Subquery != nil || it.Table == "" || it.TableFunc {
		return FromItem{}, false
	}
	// A CTE lives in no database, so a qualified name always means the TABLE --
	// exactly how the resolution side already reads it (cteFromItemRefs, cte.go).
	if it.Schema != "" {
		return FromItem{}, false
	}
	// A CTE has no b-tree, so "INDEXED BY" over one is an error the expansion
	// would silently drop -- the same reasoning as r37cViewBody's.
	if it.IndexedBy != "" || it.NotIndexed || len(it.Using) != 0 || it.Natural ||
		it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias {
		return FromItem{}, false
	}
	b, ok := p.lookupCTE(it.Table)
	if !ok || b.core == nil {
		return FromItem{}, false
	}
	if b.m10d == cteM10dYes { // (28)
		return FromItem{}, false
	}
	if b.recursive != nil || b.selfRef != nil { // (22)
		return FromItem{}, false
	}
	if b.scopeDepth != len(p.cteScopes) {
		return FromItem{}, false
	}
	body := b.core
	if b.colNames != nil {
		renamed, rok := r38cRenameBodyColumns(body, b.colNames)
		if !rok {
			return FromItem{}, false
		}
		body = renamed
	}
	return r37cViewItem(it, body), true
}

// r38cRenameBodyColumns is resolveFromTermToCte's "pEList = pCte->pCols"
// substitution: a CTE's "(col, ...)" list renames its body's outputs, so the
// inlined derived table carries them as aliases. It declines where an alias
// would not reproduce applyCTEColNames: a count mismatch (that is an error
// the expansion must not swallow), a "*", and a repeated name (which gets a
// ":N" suffix an alias cannot spell).
//
// Only the leading select-core is renamed: a compound's column names come from
// its first arm.
func r38cRenameBodyColumns(body *SelectStmt, colNames []string) (*SelectStmt, bool) {
	if len(body.Columns) != len(colNames) {
		return nil, false
	}
	seen := make(map[string]bool, len(colNames))
	for _, n := range colNames {
		l := r33sFoldIdent(n)
		if seen[l] {
			return nil, false
		}
		seen[l] = true
	}
	cols := make([]SelectColumn, len(body.Columns))
	for i, c := range body.Columns {
		if c.Star {
			return nil, false
		}
		c.Alias, c.HasAlias = colNames[i], true
		cols[i] = c
	}
	out := *body
	out.Columns = cols
	return &out, true
}

// r37cExpandViewRef turns a FROM item naming a stored view into the derived
// table sqlite3SelectExpand makes of it, so the flattener's rewrites reach the
// view spelling too.
//
// The expansion must preserve the view's column names, and the two naming
// rules differ (subqueryColumnNames): a view peels likely()/unlikely()/
// likelihood() off an unaliased item and names a rowid alias by its INTEGER
// PRIMARY KEY column. A body carrying either is left alone, as is one with
// repeated output names. A lone "*" body needs none of those checks.
func (p *ReadOnlyPager) r37cExpandViewRef(it FromItem) (FromItem, bool) {
	pcv, ok := p.r37cViewBody(it)
	if !ok {
		return FromItem{}, false
	}
	body := pcv.selectStmt
	if len(body.Columns) == 0 {
		return FromItem{}, false
	}
	if len(body.Columns) == 1 && body.Columns[0].Star && body.Columns[0].StarQualifier == "" {
		return r37cViewItem(it, body), true
	}
	for _, c := range body.Columns {
		if c.Star {
			return FromItem{}, false // "*" mixed with other items: positions shift
		}
		if c.HasAlias {
			continue
		}
		peeled, _ := skipCollateOnly(c.Expr)
		switch x := peeled.(type) {
		case FuncExpr:
			switch r33sFoldIdent(x.Name) {
			case "likely", "unlikely", "likelihood":
				return FromItem{}, false
			}
		case ColumnExpr:
			if equalFoldName(x.Name, "rowid") || equalFoldName(x.Name, "_rowid_") || equalFoldName(x.Name, "oid") {
				return FromItem{}, false
			}
		}
	}
	var scopes []tableScope
	if len(body.From) > 0 {
		jts, _, err := p.resolveFrom(body.From, nil)
		if err != nil {
			return FromItem{}, false
		}
		scopes = buildScopes(jts)
	}
	outCols, err := expandSelectList(body.Columns, scopes, defaultColNameMode)
	if err != nil {
		return FromItem{}, false
	}
	seen := make(map[string]bool, len(outCols))
	for _, oc := range outCols {
		l := r33sFoldIdent(oc.name)
		if seen[l] {
			return FromItem{}, false
		}
		seen[l] = true
	}
	return r37cViewItem(it, body), true
}

// r37cViewBody is the shared half of the two expanders: it resolves it to a
// stored view whose body may stand in for it, or reports false.
func (p *ReadOnlyPager) r37cViewBody(it FromItem) (*parsedCreateView, bool) {
	if it.Subquery != nil || it.Table == "" || it.TableFunc {
		return nil, false
	}
	// A view has no b-tree, so "INDEXED BY" over one is "no such index"
	// (indexedby.test) -- a rejection the expansion would silently drop.
	if it.IndexedBy != "" || it.NotIndexed || len(it.Using) != 0 || it.Natural ||
		it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias {
		return nil, false
	}
	if _, isCTE := p.lookupCTE(it.Table); isCTE {
		return nil, false // a CTE SHADOWS a same-named view
	}
	// A database qualifier that does NOT resolve in this file is "no such
	// table" on the ordinary path (resolveFrom, join.go); expanding here would
	// answer it out of the local schema instead.
	if it.Schema != "" {
		ok, qerr := p.qualifierResolvesLocally(it.Schema)
		if qerr != nil || !ok {
			return nil, false
		}
	}
	// An ordinary table is the overwhelmingly common case and resolveTableIn
	// memoizes, where resolveViewByNameIn below walks the schema AND re-parses
	// the view's stored CREATE text on every call -- and this runs once per
	// FROM item per execSelect.
	if _, terr := p.resolveTableIn(fromItemScope(it), it.Table); terr == nil {
		return nil, false
	}
	pcv, ok, err := p.resolveViewByNameIn(fromItemScope(it), it.Table)
	if err != nil || !ok || pcv.selectStmt == nil {
		return nil, false
	}
	if pcv.colNames != nil {
		return nil, false // an explicit "(col, ...)" list renames the body's columns
	}
	// A compound view body goes through declineIfCompoundAffinityUnreproducible
	// (view.go) on the view path and would bypass it inline.
	if len(pcv.selectStmt.Compound) != 0 {
		return nil, false
	}
	// "PRAGMA trusted_schema=OFF" reaches a view body and not an inline
	// subquery, so the expansion has to make the same check viewOutputRows does.
	if err := p.checkTrustedSchemaSelect(pcv.isTemp, pcv.selectStmt); err != nil {
		return nil, false
	}
	// Same reasoning, for the deferred half of parseCreateViewStmt's own
	// permissiveness (view_collate.go): an unrecognized COLLATE name
	// anywhere in this view's body must not silently inline into the
	// caller's query as though it were BINARY. Declining the FLATTEN here
	// (rather than erroring) is enough -- resolveViewColumns/viewOutputRows
	// run this identical check on the un-flattened reference and raise the
	// real "no such collation sequence" error there.
	if firstUnknownViewCollation(pcv.selectStmt) != "" {
		return nil, false
	}
	return pcv, true
}

// r37cViewItem is the derived-table FROM item that stands in for a view
// reference: the view's own body, under the name the reference already had, so
// every qualified column reference in the enclosing query still resolves.
func r37cViewItem(it FromItem, body *SelectStmt) FromItem {
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	return FromItem{
		Alias:    name,
		Join:     it.Join,
		On:       it.On,
		Subquery: body,
	}
}

// r37cTransparentBase reports the base-table FROM item that it -- a derived
// table whose body is exactly "SELECT * FROM <ordinary table>", optionally with
// a LIMIT -- flattens into, keeping its alias so qualified references resolve
// unchanged. ok is false for anything else.
//
// limit is the body's own LIMIT, which flattening moves onto the parent
// (select.c:4695), so the caller may only accept it where
// r38cOuterAllowsLimitTransfer does; nil when there is none. Even without an
// outer ORDER BY the pairing matters: the substitution hands the parent an
// index the materialized table never offered, so
// "SELECT a FROM (SELECT * FROM t LIMIT 3) AS d" returns the first three rows
// in index order, not rowid order.
func (p *ReadOnlyPager) r37cTransparentBase(it FromItem) (FromItem, *int64, bool) {
	sub := it.Subquery
	if sub == nil {
		return FromItem{}, nil, false
	}
	// An UNALIASED derived table is unnameable in SQLite (lookupName matches a
	// subquery item against its synthetic "sqlite_sq_NNN" name, so "t.a" over
	// "FROM (SELECT * FROM t)" is "no such column"). Substituting the base
	// table would make that name resolve, so only an aliased item is rewritten
	// -- the alias goes on the base item and nothing new becomes nameable.
	if it.Alias == "" {
		return FromItem{}, nil, false
	}
	// A lone FROM item carries no join connector; asserted, not assumed.
	if it.Join != JoinCross && it.Join != JoinInner {
		return FromItem{}, nil, false
	}
	if len(it.Using) != 0 || it.Natural {
		return FromItem{}, nil, false
	}
	if it.IndexedBy != "" || it.NotIndexed || it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias {
		return FromItem{}, nil, false
	}
	// Transparent body: no clause of its own but a LIMIT, and a lone bare "*".
	// (14) rules out an OFFSET, and a bound-parameter LIMIT is left alone
	// because the parent it would move to is compiled before the parameter is
	// resolved (execSelect resolves LimitParam only for the statement it runs).
	if sub.Distinct || sub.Where != nil || len(sub.GroupBy) != 0 || sub.Having != nil ||
		len(sub.Compound) != 0 || len(sub.OrderBy) != 0 || len(sub.Windows) != 0 ||
		len(sub.CTEs) != 0 || sub.ValuesArms != 0 ||
		sub.LimitParam != nil || sub.Offset != nil || sub.OffsetParam != nil ||
		sub.FromParenthesized || sub.FromNestedParenJoin {
		return FromItem{}, nil, false
	}
	if len(sub.Columns) != 1 || !sub.Columns[0].Star || sub.Columns[0].StarQualifier != "" {
		return FromItem{}, nil, false
	}
	if len(sub.From) != 1 {
		return FromItem{}, nil, false
	}
	b := sub.From[0]
	if _, ok := p.r37cOrdinaryBase(b); !ok {
		return FromItem{}, nil, false
	}

	out := b // keeps the body item's own INDEXED BY / NOT INDEXED hints
	out.Alias = it.Alias
	out.Join = it.Join
	out.On = it.On
	return out, sub.Limit, true
}

// r37cOrdinaryBase reports the table b -- a derived table body's lone FROM
// item -- names, when it is an ORDINARY table the flattener may substitute for
// the derived table, and false for anything else.
func (p *ReadOnlyPager) r37cOrdinaryBase(b FromItem) (*resolvedTable, bool) {
	if b.Table == "" || b.Subquery != nil || b.TableFunc ||
		b.On != nil || len(b.Using) != 0 || b.Natural ||
		b.GroupLen != 0 || b.GroupAlias != "" || b.HasGroupAlias {
		return nil, false
	}
	// A qualifier is carried through unchanged (a MAIN view's body has every
	// unqualified FROM item pinned to the owning schema by
	// qualifyUnqualifiedFromItems, view.go), but only one that resolves in THIS
	// file: a page number is file-local, so handing an ATTACHed database's
	// table to this pager's planner would seek the wrong b-tree.
	if b.Schema != "" {
		ok, qerr := p.qualifierResolvesLocally(b.Schema)
		if qerr != nil || !ok {
			return nil, false
		}
	}
	// The base must be an ORDINARY table. A CTE (22)/(28), a view and a virtual
	// table each expose a DIFFERENT column set through "SELECT *" than a direct
	// reference does -- a vtab's HIDDEN columns are the sharpest case: they are
	// invisible through the derived table and referenceable through the base
	// one. The three catalog names are excluded for the same reason: their row
	// source is chosen per catalog (temp_catalog.go), not by resolveTableIn
	// alone.
	if isMainSchemaCatalogName(b.Table) || isTempSchemaCatalogName(b.Table) ||
		equalFoldName(b.Table, "sqlite_sequence") {
		return nil, false
	}
	if _, isCTE := p.lookupCTE(b.Table); isCTE {
		return nil, false
	}
	if isV, verr := p.isVtabItem(b); verr != nil || isV {
		return nil, false
	}
	if _, isView, verr := p.resolveViewByNameIn(fromItemScope(b), b.Table); verr != nil || isView {
		return nil, false
	}
	tbl, terr := p.resolveTableIn(fromItemScope(b), b.Table)
	if terr != nil || tbl == nil {
		return nil, false
	}
	return tbl, true
}

// r37cMayReferenceRowid reports whether stmt could name a rowid pseudo-column
// anywhere -- see r37cFlattenTransparent for why that blocks the rewrite.
//
// Every node kind it does not recognize answers TRUE: an unhandled shape must
// suppress the optimization, never silently pass a hidden "rowid" through it.
func r37cMayReferenceRowid(stmt *SelectStmt) bool {
	if stmt == nil {
		return false
	}
	for _, it := range stmt.From {
		if it.Subquery != nil && r37cMayReferenceRowid(it.Subquery) {
			return true
		}
		if r37cExprMayReferenceRowid(it.On) {
			return true
		}
		for _, a := range it.TableFuncArgs {
			if r37cExprMayReferenceRowid(a) {
				return true
			}
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star && r37cExprMayReferenceRowid(c.Expr) {
			return true
		}
	}
	if r37cExprMayReferenceRowid(stmt.Where) || r37cExprMayReferenceRowid(stmt.Having) {
		return true
	}
	for _, g := range stmt.GroupBy {
		if r37cExprMayReferenceRowid(g) {
			return true
		}
	}
	for _, ot := range stmt.OrderBy {
		if r37cExprMayReferenceRowid(ot.Expr) {
			return true
		}
	}
	for _, arm := range stmt.Compound {
		if r37cMayReferenceRowid(arm.Stmt) {
			return true
		}
	}
	for _, cte := range stmt.CTEs {
		if r37cMayReferenceRowid(cte.Select) {
			return true
		}
	}
	// A named WINDOW clause carries PARTITION BY/ORDER BY expressions of its
	// own; rather than reach into WindowSpec here, its mere presence suppresses
	// the rewrite (restriction (25) already excludes an inline OVER anyway).
	return len(stmt.Windows) != 0
}

func r37cExprMayReferenceRowid(e Expr) bool {
	if e == nil {
		return false
	}
	switch x := e.(type) {
	case LiteralExpr, ParamExpr:
		return false
	case ColumnExpr:
		return equalFoldName(x.Name, "rowid") || equalFoldName(x.Name, "_rowid_") || equalFoldName(x.Name, "oid")
	case UnaryExpr:
		return r37cExprMayReferenceRowid(x.X)
	case BinaryExpr:
		return r37cExprMayReferenceRowid(x.L) || r37cExprMayReferenceRowid(x.R)
	case IsNullExpr:
		return r37cExprMayReferenceRowid(x.X)
	case InExpr:
		if x.Sub != nil && r37cMayReferenceRowid(x.Sub) {
			return true
		}
		if r37cExprMayReferenceRowid(x.X) {
			return true
		}
		for _, el := range x.List {
			if r37cExprMayReferenceRowid(el) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return r37cExprMayReferenceRowid(x.X) || r37cExprMayReferenceRowid(x.Lo) || r37cExprMayReferenceRowid(x.Hi)
	case LikeExpr:
		return r37cExprMayReferenceRowid(x.X) || r37cExprMayReferenceRowid(x.Pattern) || r37cExprMayReferenceRowid(x.Escape)
	case GlobExpr:
		return r37cExprMayReferenceRowid(x.X) || r37cExprMayReferenceRowid(x.Pattern)
	case MatchExpr:
		return r37cExprMayReferenceRowid(x.X) || r37cExprMayReferenceRowid(x.Pattern)
	case CollateExpr:
		return r37cExprMayReferenceRowid(x.X)
	case CastExpr:
		return r37cExprMayReferenceRowid(x.X)
	case FuncExpr:
		if x.Over != nil {
			return true // a window spec's own expressions; see above
		}
		for _, a := range x.walkArgs() {
			if r37cExprMayReferenceRowid(a) {
				return true
			}
		}
		return false
	case RowExpr:
		for _, el := range x.Elems {
			if r37cExprMayReferenceRowid(el) {
				return true
			}
		}
		return false
	case CaseExpr:
		if x.Base != nil && r37cExprMayReferenceRowid(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if r37cExprMayReferenceRowid(w.When) || r37cExprMayReferenceRowid(w.Then) {
				return true
			}
		}
		return x.Else != nil && r37cExprMayReferenceRowid(x.Else)
	case SubqueryExpr:
		return r37cMayReferenceRowid(x.Stmt)
	case ExistsExpr:
		return r37cMayReferenceRowid(x.Stmt)
	default:
		return true
	}
}
