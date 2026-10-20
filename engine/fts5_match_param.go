package engine

// fts5FoldMatchParams answers "SELECT ... ORDER BY rank FROM t WHERE t MATCH ?"
// -- the way an application runs a full-text search, and one C SQLite answers.
//
// rank, bm25(), snippet() and highlight() read corpus statistics for the
// MATCH's query, which buildFts5AuxState gathers once, at compile time, from
// the pattern's text (fts5_vdbe_aux.go). A bound parameter has no text until
// the statement runs, so that path declines it. Here, before compiling, a
// "MATCH ?" in a SELECT that uses one of those functions becomes "MATCH
// <the bound value>": for this run the same query. The statement is then not
// plan-cached (the caller clears its cache key), exactly as a folded "LIMIT ?"
// is not, since the next run binds another query.
//
// Only the SELECT cores that use an auxiliary function are touched: a plain
// "MATCH ?" keeps its parameter and its cached plan. The tree is copied where
// it changes, never written: an AST may be compiled again.
func fts5FoldMatchParams(stmt *SelectStmt, params []Value) (*SelectStmt, bool) {
	if stmt == nil {
		return nil, false
	}
	out := *stmt
	changed := false
	if fts5SelectUsesAux(stmt) {
		if w, ok := foldMatchParamConjuncts(stmt.Where, params); ok {
			out.Where, changed = w, true
		}
	}
	if len(stmt.CTEs) > 0 {
		var ctes []CTEDef
		for i, c := range stmt.CTEs {
			if s, ok := fts5FoldMatchParams(c.Select, params); ok {
				if ctes == nil {
					ctes = append([]CTEDef(nil), stmt.CTEs...)
				}
				ctes[i].Select = s
			}
		}
		if ctes != nil {
			out.CTEs, changed = ctes, true
		}
	}
	var from []FromItem
	for i, f := range stmt.From {
		if s, ok := fts5FoldMatchParams(f.Subquery, params); ok {
			if from == nil {
				from = append([]FromItem(nil), stmt.From...)
			}
			from[i].Subquery = s
		}
	}
	if from != nil {
		out.From, changed = from, true
	}
	var arms []CompoundArm
	for i, a := range stmt.Compound {
		if s, ok := fts5FoldMatchParams(a.Stmt, params); ok {
			if arms == nil {
				arms = append([]CompoundArm(nil), stmt.Compound...)
			}
			arms[i].Stmt = s
		}
	}
	if arms != nil {
		out.Compound, changed = arms, true
	}
	if !changed {
		return stmt, false
	}
	return &out, true
}

// fts5SelectUsesAux reports whether a SELECT core's result or ORDER BY uses
// rank, bm25, snippet or highlight, as buildFts5AuxState asks.
func fts5SelectUsesAux(stmt *SelectStmt) bool {
	for _, sc := range stmt.Columns {
		if !sc.Star && fts5ExprUsesAux(sc.Expr) {
			return true
		}
	}
	for _, ot := range stmt.OrderBy {
		if fts5ExprUsesAux(ot.Expr) {
			return true
		}
	}
	return false
}

// foldMatchParamConjuncts replaces the pattern of every top-level AND-ed
// "X MATCH ?" in where with the bound value, the only MATCH buildFts5AuxState
// reads. Returns the new tree and whether anything changed.
func foldMatchParamConjuncts(where Expr, params []Value) (Expr, bool) {
	switch x := where.(type) {
	case MatchExpr:
		if p, ok := x.Pattern.(ParamExpr); ok {
			x.Pattern = LiteralExpr{Val: paramAt(params, p.Index)}
			return x, true
		}
	case BinaryExpr:
		if x.Op != "AND" {
			return where, false
		}
		l, lok := foldMatchParamConjuncts(x.L, params)
		r, rok := foldMatchParamConjuncts(x.R, params)
		if lok || rok {
			x.L, x.R = l, r
			return x, true
		}
	}
	return where, false
}
