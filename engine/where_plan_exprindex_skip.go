// Narrowing wherePlanIndexList's decline for expression and partial indexes.
// indexProvablyIrrelevant checks if an index's expression is unused in the query.
package engine

// exprEqualIgnoringQualifier is exprEqual with one relaxation: two ColumnExpr
// nodes compare equal by name alone, ignoring the qualifier.
func exprEqualIgnoringQualifier(a, b Expr) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case LiteralExpr:
		y, ok := b.(LiteralExpr)
		return ok && valueExactEqual(x.Val, y.Val)
	case ParamExpr:
		y, ok := b.(ParamExpr)
		return ok && x.Index == y.Index
	case ColumnExpr:
		y, ok := b.(ColumnExpr)
		return ok && equalFoldName(x.Name, y.Name)
	case UnaryExpr:
		y, ok := b.(UnaryExpr)
		return ok && x.Op == y.Op && exprEqualIgnoringQualifier(x.X, y.X)
	case BinaryExpr:
		y, ok := b.(BinaryExpr)
		return ok && x.Op == y.Op && exprEqualIgnoringQualifier(x.L, y.L) && exprEqualIgnoringQualifier(x.R, y.R)
	case IsNullExpr:
		y, ok := b.(IsNullExpr)
		return ok && x.Not == y.Not && exprEqualIgnoringQualifier(x.X, y.X)
	case InExpr:
		y, ok := b.(InExpr)
		if !ok || x.Not != y.Not || len(x.List) != len(y.List) || !exprEqualIgnoringQualifier(x.X, y.X) {
			return false
		}
		for i := range x.List {
			if !exprEqualIgnoringQualifier(x.List[i], y.List[i]) {
				return false
			}
		}
		return true
	case BetweenExpr:
		y, ok := b.(BetweenExpr)
		return ok && x.Not == y.Not && exprEqualIgnoringQualifier(x.X, y.X) &&
			exprEqualIgnoringQualifier(x.Lo, y.Lo) && exprEqualIgnoringQualifier(x.Hi, y.Hi)
	case LikeExpr:
		y, ok := b.(LikeExpr)
		return ok && x.Not == y.Not && exprEqualIgnoringQualifier(x.X, y.X) &&
			exprEqualIgnoringQualifier(x.Pattern, y.Pattern) && exprEqualIgnoringQualifier(x.Escape, y.Escape)
	case GlobExpr:
		y, ok := b.(GlobExpr)
		return ok && x.Not == y.Not && exprEqualIgnoringQualifier(x.X, y.X) && exprEqualIgnoringQualifier(x.Pattern, y.Pattern)
	case CollateExpr:
		y, ok := b.(CollateExpr)
		return ok && equalFoldName(x.Name, y.Name) && exprEqualIgnoringQualifier(x.X, y.X)
	case FuncExpr:
		y, ok := b.(FuncExpr)
		if !ok || !equalFoldName(x.Name, y.Name) || x.Star != y.Star || x.Distinct != y.Distinct || len(x.Args) != len(y.Args) ||
			len(x.OrderBy) > 0 || len(y.OrderBy) > 0 {
			return false
		}
		for i := range x.Args {
			if !exprEqualIgnoringQualifier(x.Args[i], y.Args[i]) {
				return false
			}
		}
		return true
	case CastExpr:
		y, ok := b.(CastExpr)
		return ok && x.Type == y.Type && exprEqualIgnoringQualifier(x.X, y.X)
	default:
		return false
	}
}

// exprMightReference conservatively reports whether e's tree could possibly
// evaluate the same thing as idxExpr -- an expression from one column of a
// CREATE INDEX this compiler declines to reproduce. It is used ONLY to prove
// such an index IRRELEVANT to a statement (indexProvablyIrrelevant below), so
// the two failure directions are not symmetric: on any shape this function
// does not fully understand, it answers true (assume relevant) rather than
// false (assume irrelevant). Declining an index that could safely have been
// skipped is a missed optimization; skipping one that genuinely mattered
// would let this compiler's solver reach a plan C SQLite's own cost model
// would not have -- a wrong answer, not a decline.
//
// Modelled on exprContainsSubquery's own traversal (sql_ast.go) for which
// node shapes carry sub-expressions at all; SubqueryExpr/ExistsExpr/
// MatchExpr/RaiseExpr and anything else fall to the same conservative
// "true" default exprContainsSubquery gives them, since none is expected to
// appear inside a CREATE INDEX column's own expression as a MATCH target and
// none is worth reasoning through here.
func exprMightReference(e, idxExpr Expr) bool {
	if e == nil {
		return false
	}
	if exprEqualIgnoringQualifier(e, idxExpr) {
		return true
	}
	switch x := e.(type) {
	case LiteralExpr, ParamExpr, ColumnExpr:
		return false // leaves already compared above; nothing further to walk
	case UnaryExpr:
		return exprMightReference(x.X, idxExpr)
	case BinaryExpr:
		return exprMightReference(x.L, idxExpr) || exprMightReference(x.R, idxExpr)
	case IsNullExpr:
		return exprMightReference(x.X, idxExpr)
	case InExpr:
		if x.Sub != nil {
			return true
		}
		if exprMightReference(x.X, idxExpr) {
			return true
		}
		for _, it := range x.List {
			if exprMightReference(it, idxExpr) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return exprMightReference(x.X, idxExpr) || exprMightReference(x.Lo, idxExpr) || exprMightReference(x.Hi, idxExpr)
	case LikeExpr:
		return exprMightReference(x.X, idxExpr) || exprMightReference(x.Pattern, idxExpr) || exprMightReference(x.Escape, idxExpr)
	case GlobExpr:
		return exprMightReference(x.X, idxExpr) || exprMightReference(x.Pattern, idxExpr)
	case CollateExpr:
		return exprMightReference(x.X, idxExpr)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if exprMightReference(a, idxExpr) {
				return true
			}
		}
		return false
	case CastExpr:
		return exprMightReference(x.X, idxExpr)
	case CaseExpr:
		if x.Base != nil && exprMightReference(x.Base, idxExpr) {
			return true
		}
		for _, w := range x.Whens {
			if exprMightReference(w.When, idxExpr) || exprMightReference(w.Then, idxExpr) {
				return true
			}
		}
		return x.Else != nil && exprMightReference(x.Else, idxExpr)
	case RowExpr:
		for _, el := range x.Elems {
			if exprMightReference(el, idxExpr) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// stmtMightReference is exprMightReference widened to every place an
// ordinary SELECT core's own expressions live: WHERE, HAVING, every GROUP BY
// term, every ORDER BY term, and every select-list item's expression (a
// "*"/"t.*" carries none). It does not descend into stmt.Compound (a sibling
// compound arm cannot see this table at all) or stmt.From (a FROM item's own
// ON clause or a derived table/CTE body) -- see this file's SCOPE note for
// why those are out of bounds for now.
func stmtMightReference(stmt *SelectStmt, target Expr) bool {
	if exprMightReference(stmt.Where, target) || exprMightReference(stmt.Having, target) {
		return true
	}
	for _, ge := range stmt.GroupBy {
		if exprMightReference(ge, target) {
			return true
		}
	}
	for _, ot := range stmt.OrderBy {
		if exprMightReference(ot.Expr, target) {
			return true
		}
	}
	for _, sc := range stmt.Columns {
		if !sc.Star && exprMightReference(sc.Expr, target) {
			return true
		}
	}
	return false
}

// indexProvablyIrrelevant reports whether idx -- an expression and/or partial
// CREATE INDEX entry wherePlanIndexList would otherwise decline the WHOLE
// TABLE for -- is one no shape of stmt could ever have chosen, so it is safe
// to OMIT from the candidate chain instead of aborting: the solver then
// reasons about the table's remaining ordinary indexes exactly as if this one
// were never there, which is exactly what a real cost model that priced it
// and never picked it would also produce.
//
// Two independent proofs, EVERY complicating piece of idx must clear its own:
//
//   - a PARTIAL index (idx.where != nil) can never answer a query with NO
//     WHERE clause at all: whereUsablePartialIndex (where.c) requires the
//     query's WHERE to IMPLY the partial condition, and there is nothing to
//     imply it from when there is none. A query WITH a WHERE clause is left
//     alone entirely -- implication is not attempted, so it stays declined
//     exactly as before.
//   - an EXPRESSION-KEYED column (a non-nil entry of idx.exprs) whose
//     expression does not appear -- per stmtMightReference, qualifier-blind
//     and defaulting to "might" on anything it does not fully model -- can
//     never be the column whereScanInitIndexExpr (where.c:461) matches a term
//     against, so it can never anchor an eq/range term, satisfy an ORDER BY,
//     or serve a min()/max() seek either.
func indexProvablyIrrelevant(idx *parsedCreateIndex, stmt *SelectStmt) bool {
	if idx.where != nil && stmt.Where != nil {
		return false
	}
	for _, e := range idx.exprs {
		if e != nil && stmtMightReference(stmt, e) {
			return false
		}
	}
	return true
}
