package engine

// A subquery's column references contribute to the enclosing query's
// SrcItem.colUsed, and this file computes that contribution.
//
// lookupName climbs the NameContext chain until a name resolves, then marks
// whichever SrcItem it landed on, however deep the subquery (resolve.c:828):
//
//	if( pMatch ){
//	  if( pExpr->iColumn>=0 ){ pMatch->colUsed |= sqlite3ExprColUsed(pExpr); }
//	  else                   { pMatch->fg.rowidUsed = 1; }
//	}
//
// So in "SELECT a, (SELECT count(*) FROM t AS s WHERE s.a = t.a) FROM t" the
// correlated t.a sets a bit on the outer t; s.a marks only the subquery's item.
//
// colUsed decides whether an index covers (whereLoopAddBtree's "m =
// pSrc->colUsed & pProbe->colNotIdxed"), so failing to compute it declines the
// index-order plan and returns rowid order where C walks the index: "CREATE
// INDEX i1 ON t(a,b); SELECT a, (SELECT count(*) FROM t AS s) FROM t" comes
// back in i1 order in C.
//
// The walk is exact or declines: each name resolves as lookupName does,
// innermost FROM first, climbing, with the rowid fallback per level; anything
// whose shadowing cannot be enumerated (r36dShadowOf) declines the statement.

// r36dItem is one FROM item of a nested clause, reduced to the two questions
// lookupName asks of it: does it answer to this qualifier, and does it supply
// this column (or a rowid).
type r36dItem struct {
	name  string // folded alias/table name; "" for an unaliased derived table
	cols  map[string]bool
	rowid bool // VisibleRowid
}

// r36dShadow is one nested FROM clause reduced to what lookupName needs to
// decide whether a name stops there or climbs.
//
// The per-item breakdown matters for a qualified name: lookupName climbs when
// a level leaves cnt at 0 ("if( cnt ) break; pNC = pNC->pNext",
// resolve.c:703), so a qualifier matching an item that lacks the column keeps
// climbing. In "SELECT a, (SELECT count(*) FROM u AS t WHERE t.c=10) FROM t",
// "t.c" reaches the outer t and its bit stops i1(a,b) covering. (That
// statement still declines here; the rule follows the C.)
//
// cols/rowid are the level-wide union an unqualified name sees. aliases is the
// SELECT's explicit result aliases: NC_UEList matches one after the SrcList
// and rowid fallback miss, and resolveAlias substitutes the already-resolved
// expression, so such a name adds no bit and must not climb.
type r36dShadow struct {
	items   []r36dItem
	cols    map[string]bool
	aliases map[string]bool
	rowid   bool
}

// r36dShadowOf reduces a nested SELECT's FROM clause to an r36dShadow, or false
// when it holds something whose visible names cannot be enumerated exactly.
//
// USING/NATURAL joins are fine: lookupName's "nameInUsingClause" skip
// (resolve.c) only picks which of two items at the same level a shared name
// resolves to, not whether it climbs.
//
// Rejected:
//
//   - a table-valued function (columns are the module's);
//   - a parenthesized join group (its alias names every member);
//   - a schema-qualified name, unless it is this pager's own schema (as a
//     stored view's body carries);
//   - a virtual table;
//   - a CTE defined by an enclosing compile, which would shadow a same-named
//     table this walk would then read wrongly;
//   - a derived table, view or CTE whose result names are not the plain ones
//     sqlite3ColumnsFromExprList produces (alias, bare column, "*"); an
//     expression column is named by its source text.
//
// A view and a non-recursive CTE both become FROM subqueries
// (sqlite3SelectExpand, resolveFromTermToCte), answered through
// r36dDerivedNames over the stored body, with a declared "(col,...)" list
// taking precedence.
func r36dShadowOf(p *ReadOnlyPager, sel *SelectStmt, cteDefs map[string]*CTEDef, depth int) (r36dShadow, bool) {
	sh := r36dShadow{cols: map[string]bool{}, aliases: map[string]bool{}}
	if p == nil || sel == nil || depth > 8 {
		// The depth cap is a cycle brake: a view or CTE whose body names itself
		// is a "circular reference" C SQLite rejects, but this walk runs
		// before that check and must terminate regardless.
		return sh, false
	}
	rows, err := p.Schema()
	if err != nil {
		return sh, false
	}
	for _, sc := range sel.Columns {
		if sc.HasAlias {
			sh.aliases[r33sFoldIdent(sc.Alias)] = true
		}
	}
	// A FROM-less subquery introduces no SrcList of its own, so every name in it
	// resolves outward -- which is exactly what the empty shadow this loop then
	// leaves behind expresses. "SELECT a, (SELECT max(t.a)) FROM t" is that
	// shape, and it is legitimate SQL.
	for i := range sel.From {
		it := &sel.From[i]
		if it.TableFunc || it.GroupLen != 0 || it.HasGroupAlias {
			return sh, false
		}
		// A stored VIEW's body arrives with every base-table item already
		// qualified by the OWNING pager's own schema name (viewDefFromSchemaRow
		// -> qualifyUnqualifiedFromItems), so that qualifier is not a
		// cross-database reference and must not decline the walk -- it means
		// "main, not temp". Any OTHER qualifier does decline: names are
		// file-local, and this resolves them against this pager only.
		scope := scopeAny
		if it.Schema != "" {
			if !equalFoldName(it.Schema, localSchemaOr(p.localSchema)) {
				return sh, false
			}
			scope = scopeMain
		}
		// name is what a qualifier written against this item must match, and
		// cols the names it supplies. rowid stays false for every expanded
		// body: a subquery's Table carries TF_NoVisibleRowid.
		name := it.Alias
		var cols map[string]bool
		itemRowid := false

		switch {
		case it.Subquery != nil:
			names, ok := r36dDerivedNames(p, it.Subquery, nil, cteDefs, depth+1)
			if !ok {
				return sh, false
			}
			// An UNALIASED derived table gets a synthetic name in SQLite
			// ("subquery-N"), which no written qualifier can match, so it
			// contributes columns but no name.
			cols = names

		case it.Table == "":
			return sh, false

		default:
			if name == "" {
				name = it.Table
			}
			// A QUALIFIED name is never a CTE reference (resolveFrom's CTE
			// branch is gated on the item being unqualified, exactly as
			// searchWith is), so the WITH map is consulted only without one.
			key := r33sFoldIdent(it.Table)
			if def, isCTE := cteDefs[key]; isCTE && it.Schema == "" {
				if def == nil {
					return sh, false
				}
				names, ok := r36dDerivedNames(p, def.Select, def.ColNames, cteDefs, depth+1)
				if !ok {
					return sh, false
				}
				cols = names
				break
			}
			if _, isCTE := p.lookupCTE(it.Table); isCTE {
				return sh, false
			}
			tr := findSchemaTableRow(rows, scope, it.Table)
			pcv, isView, verr := p.resolveViewByNameIn(scope, it.Table)
			if verr != nil {
				return sh, false
			}
			if isView {
				// A name that is BOTH a table and a view can only happen across
				// catalogs, and the two lookups above resolve their own
				// temp-vs-main preference independently -- so which one the name
				// really means is not settled here. Decline rather than guess.
				if tr != nil {
					return sh, false
				}
				names, ok := r36dDerivedNames(p, pcv.selectStmt, pcv.colNames, cteDefs, depth+1)
				if !ok {
					return sh, false
				}
				cols = names
				break
			}
			if tr == nil || isCreateVirtualTableSQL(tr.SQL) {
				return sh, false
			}
			rt, rerr := p.resolveTableIn(scope, it.Table)
			if rerr != nil || rt == nil {
				return sh, false
			}
			cols = make(map[string]bool, len(rt.cols))
			for _, ci := range rt.cols {
				cols[r33sFoldIdent(ci.Name)] = true
			}
			itemRowid = !rt.withoutRowid
		}

		item := r36dItem{cols: cols, rowid: itemRowid}
		if name != "" {
			item.name = r33sFoldIdent(name)
		}
		sh.items = append(sh.items, item)
		for n := range cols {
			sh.cols[n] = true
		}
		if itemRowid {
			sh.rowid = true
		}
	}
	return sh, true
}

// r36dDerivedNames returns the result column names of a body expanded into a
// FROM subquery (derived table, view, non-recursive CTE) -- what
// sqlite3ColumnsFromExprList produces -- accepting only the namings that do
// not fall back to source text: an alias, a bare column, and "*".
//
// declared is the item's "(col, ...)" list, which resolveFromTermToCte and the
// view expander pass instead of the body's pEList, hiding the body's names.
// A compound body takes names from its leftmost arm, which this does not
// chase (keeping recursive CTEs out too).
func r36dDerivedNames(p *ReadOnlyPager, sel *SelectStmt, declared []string,
	cteDefs map[string]*CTEDef, depth int) (map[string]bool, bool) {
	if declared != nil {
		out := make(map[string]bool, len(declared))
		for _, n := range declared {
			out[r33sFoldIdent(n)] = true
		}
		return out, true
	}
	if sel == nil || len(sel.Compound) > 0 {
		return nil, false
	}
	if len(sel.CTEs) > 0 {
		cteDefs = r36dWithCTEDefs(cteDefs, sel.CTEs)
	}
	inner, ok := r36dShadowOf(p, sel, cteDefs, depth)
	if !ok {
		return nil, false
	}
	out := map[string]bool{}
	for _, sc := range sel.Columns {
		switch {
		case sc.Star:
			// A QUALIFIED "x.*" expands only that item's columns; inner.cols is
			// the union over the whole clause, so it is exact only when the
			// clause has one item.
			if sc.StarQualifier != "" && len(sel.From) != 1 {
				return nil, false
			}
			for n := range inner.cols {
				out[n] = true
			}
		case sc.HasAlias:
			out[r33sFoldIdent(sc.Alias)] = true
		default:
			ce, isCol := sc.Expr.(ColumnExpr)
			if !isCol {
				return nil, false
			}
			out[r33sFoldIdent(ce.Name)] = true
		}
	}
	return out, true
}

// r36dWithCTEDefs extends the in-scope CTE map with one WITH clause's
// definitions. A CTE name shadows a same-named real table, so a nested FROM
// item naming one is resolved through the definition here or not at all.
func r36dWithCTEDefs(have map[string]*CTEDef, defs []CTEDef) map[string]*CTEDef {
	out := make(map[string]*CTEDef, len(have)+len(defs))
	for k, v := range have {
		out[k] = v
	}
	for i := range defs {
		out[r33sFoldIdent(defs[i].Name)] = &defs[i]
	}
	return out
}

// r36dSubColUsed walks nested SELECT sel and marks the enclosing statement's
// colUsed bits for every name resolving outward. shadow is the stack of FROM
// clauses between sel and the enclosing statement, innermost first; mark is
// wherePlanColUsed's visitor (false when a name cannot be resolved exactly);
// cteDefs is the WITH definitions in scope. false declines the statement.
func r36dSubColUsed(p *ReadOnlyPager, sel *SelectStmt, shadow []r36dShadow, cteDefs map[string]*CTEDef, depth int, mark func(ColumnExpr) bool) bool {
	if sel == nil || depth > 8 {
		return false
	}
	if len(sel.Compound) > 0 {
		return r36dCompoundColUsed(p, sel, shadow, cteDefs, depth, mark)
	}
	if len(sel.CTEs) > 0 {
		cteDefs = r36dWithCTEDefs(cteDefs, sel.CTEs)
	}
	sh, ok := r36dShadowOf(p, sel, cteDefs, depth)
	if !ok {
		return false
	}
	stack := append([]r36dShadow{sh}, shadow...)

	good := true
	visit := func(ce ColumnExpr) bool {
		if ce.Schema != "" || ce.UsingRepr {
			good = false
			return false
		}
		lname := r33sFoldIdent(ce.Name)
		rowidName := isRowidName(lname)
		if ce.Qualifier != "" {
			q := r33sFoldIdent(ce.Qualifier)
			for _, s := range stack {
				stop := false
				for _, it := range s.items {
					if it.name != q {
						continue
					}
					// A matching item that does not supply the column leaves
					// cnt at 0, and the loop climbs -- see r36dShadow.
					if it.cols[lname] || (rowidName && it.rowid) {
						stop = true
						break
					}
				}
				if stop {
					// Resolves at a nested level: the bit lands on THAT
					// SrcItem, which the enclosing plan never reads.
					return true
				}
			}
			if !mark(ce) {
				good = false
				return false
			}
			return true
		}
		for _, s := range stack {
			if s.cols[lname] {
				return true
			}
			// lookupName's ROWID fallback is applied PER NameContext -- "if(
			// cnt==0 && cntTab>=1 && pMatch && ... sqlite3IsRowid(zCol) )"
			// (resolve.c:623), inside the loop that climbs -- so a level with a
			// visible rowid stops the climb even though it supplies no column of
			// that name. It sets fg.rowidUsed rather than a colUsed bit
			// (resolve.c:831), so it contributes nothing either way.
			if rowidName && s.rowid {
				return true
			}
			if s.aliases[lname] {
				return true
			}
		}
		// Not supplied, nor aliased, by any nested level: an enclosing column
		// or an error. mark answers the first exactly and declines the second.
		if !mark(ce) {
			good = false
			return false
		}
		return true
	}
	sub := func(inner *SelectStmt) bool {
		if !r36dSubColUsed(p, inner, stack, cteDefs, depth, mark) {
			good = false
			return false
		}
		return true
	}
	walk := func(e Expr) {
		if good && e != nil {
			wherePlanWalkColumnsSub(e, visit, sub)
		}
	}

	// A DERIVED table's body is resolved against the ENCLOSING levels but NOT
	// against its own siblings: SQLite has no LATERAL, so a FROM-clause subquery
	// cannot see the other items of the clause it sits in. It CAN see an
	// enclosing query's items, which is why its body is walked at all -- "SELECT
	// a, (SELECT count(*) FROM (SELECT s.* FROM t AS s WHERE s.a = t.a)) FROM t"
	// is accepted by the oracle and its "t.a" credits the outer t.
	for i := range sel.From {
		it := &sel.From[i]
		if it.Subquery != nil {
			if !r36dSubColUsed(p, it.Subquery, shadow, cteDefs, depth+1, mark) {
				return false
			}
			continue
		}
		// A CTE's body is resolved in the scope its WITH clause was DECLARED
		// in, which is not necessarily this one -- resolveFromTermToCte attaches it as a
		// subquery of the reference and resolve.c then climbs from THERE, so a
		// correlated CTE body ("WITH c AS (SELECT t.a) SELECT count(*) FROM c"
		// inside a subquery over t) really does credit an enclosing item. This
		// walk cannot say WHICH levels that body sees, so it accepts only a
		// CLOSED one: every name in it resolves within its own FROM clause.
		// The rejecting mark is what enforces that -- any name that would climb
		// out declines the statement, exactly as before this file existed.
		if it.Table != "" && it.Schema == "" {
			if def, isCTE := cteDefs[r33sFoldIdent(it.Table)]; isCTE {
				if def == nil || !r36dSubColUsed(p, def.Select, nil, cteDefs, depth+1,
					func(ColumnExpr) bool { return false }) {
					return false
				}
			}
		}
	}

	for _, sc := range sel.Columns {
		if sc.Star {
			// "*" expands against this level's own SrcList only; a qualified
			// "x.*" naming something else is "no such table" in resolve.c.
			if sc.StarQualifier != "" {
				q := r33sFoldIdent(sc.StarQualifier)
				known := false
				for _, it := range sh.items {
					if it.name == q {
						known = true
					}
				}
				if !known {
					return false
				}
			}
			continue
		}
		walk(sc.Expr)
	}
	walk(sel.Where)
	walk(sel.Having)
	for _, e := range sel.GroupBy {
		walk(e)
	}
	for _, ot := range sel.OrderBy {
		walk(ot.Expr)
	}
	for i := range sel.From {
		walk(sel.From[i].On)
	}
	return good
}

// r36dCompoundColUsed is r36dSubColUsed for a compound subquery: each arm
// resolves against its own FROM and the same outer chain -- resolveSelectStep's
// loop
//
//	while( p ){ ... sNC.pSrcList = p->pSrc; sNC.pNext = pOuterNC; ... p = p->pPrior; }
//
// (resolve.c:1895, 1967-1968, 2104) -- so any arm's outward name credits the
// enclosing item (rowvalue.test 30.3).
//
// The compound's ORDER BY is not walked per arm: resolveCompoundOrderBy
// (resolve.c:1607) resolves terms against the leftmost arm's result set and
// never climbs. Only integer ordinals are admitted, since they name no column;
// alias and expression terms would need resolveOrderByTermToExprList's
// compare. An arm with its own ORDER BY or nested compound declines.
func r36dCompoundColUsed(p *ReadOnlyPager, sel *SelectStmt, shadow []r36dShadow, cteDefs map[string]*CTEDef, depth int, mark func(ColumnExpr) bool) bool {
	for _, ot := range sel.OrderBy {
		lit, isLit := ot.Expr.(LiteralExpr)
		if !isLit || lit.Val.Typ != Int {
			return false
		}
	}
	// A leading WITH is in scope for EVERY arm (sqlite3WithPush runs once for
	// the whole compound, select.c:6000), so it is pushed here rather than
	// inside the leftmost arm's own walk below.
	if len(sel.CTEs) > 0 {
		cteDefs = r36dWithCTEDefs(cteDefs, sel.CTEs)
	}
	lead := *sel
	lead.CTEs = nil
	lead.Compound = nil
	lead.OrderBy = nil
	lead.Limit, lead.Offset = nil, nil
	if !r36dSubColUsed(p, &lead, shadow, cteDefs, depth, mark) {
		return false
	}
	for i := range sel.Compound {
		arm := sel.Compound[i].Stmt
		if arm == nil || len(arm.Compound) > 0 || len(arm.OrderBy) > 0 {
			return false
		}
		if !r36dSubColUsed(p, arm, shadow, cteDefs, depth, mark) {
			return false
		}
	}
	return true
}
