// This file decides whether a single-table pure-expression index is a covering
// index for a specific query.
// WHY THIS MATTERS, not just for cost: without it, wherePlanIndexList's own
// column-building loop already represents the index (aiColumn/exprs), which
// makes the cost-model machinery in where_plan_index.go's addBtree consider
// it as a candidate loop -- but a NON-covering index scan is never cheaper
// than a plain table scan when there is no WHERE term to seek on (it pays
// for the index AND the table), so addBtree's own
// "mightSort || indexedBy || (m==0 && szIdxRow<szTabRow)" gate (see that
// file) never even builds the loop, and the solver silently falls back to
// the ordinary rowid scan -- a DIFFERENT order than C SQLite's actual
// covering-index choice. That is not a decline: it is wherePlanIndexOrderDecided
// confidently reporting the WRONG order. Verified directly against the
// oracle: a 3-row table where insertion (rowid) order and the index's own
// key order disagree on which row sorts first reports the WRONG anchor row
// for a bare-column aggregate until this file's isCovering computation is
// wired in (compat-harness/exprindex_covering_r40_test.go's
// indexexpr1-covering-order-disagrees case).
//
// SCOPE: only wherePlanIndexList's own PURE-expression-index branch calls
// this, and only when stmt != nil AND len(stmt.From) == 1 -- a multi-table
// stmt is deliberately excluded, even though stmt itself is available there
// too (round 41 ported the machinery for a general stmt, not just a
// single-table one). The reason is THIS file, not that one: colIndexed below
// matches a bare ColumnExpr by NAME ALONE against the one table currently
// being considered (exprCoveredForIndexScan never looks at x.Qualifier at
// all), which is exactly right when stmt has one FROM item -- every
// unqualified name can only mean that table's own column -- but is a WRONG
// isCovering=true waiting to happen with a second FROM item present: an
// unqualified (or differently-qualified) reference to the OTHER table's
// same-named column would be misread as covered by THIS table's index. Real
// SQLite never has this hazard: whereIsCoveringIndex (where.c:3831) tests
// pSrc->colUsed, which sqlite3ExprColUsed sets during NAME RESOLUTION bound
// to one specific cursor, so two tables sharing a column name can never be
// confused. Not attempted here, matching the C's own "this routine is an
// optimization, always safe to return false/0" posture, and each staying a
// MISSED optimization (the index just does not get marked covering, so it
// competes on cost as if it paid for a table lookup -- normally then losing
// to the plain scan, which is this port's existing, already-verified
// behavior) rather than a wrong answer:
//
//   - a "*" or "table.*" select-list item (needs every column, which this
//     does not attempt to prove one-by-one against colNotIdxed the way the
//     C's whereIsCoveringIndex short-circuits via colUsed's low 62 bits for
//     the NON-expression case -- see that function's own early-return);
//   - a subquery anywhere (InExpr.Sub, or any node type this file's
//     exprCoveredForIndexScan does not otherwise name) -- conservatively
//     "not covered", the same direction exprMightReference
//     (where_plan_exprindex_skip.go) defaults the opposite question to;
//   - WHERE_EXPRIDX's own "likely covering, but keep the table open anyway"
//     middle ground (where.c:3821-3825, for an expression whose covering-ness
//     could not be fully proven structurally) -- this file only ever answers
//     the two ends, matching this port's existing all-or-nothing isCovering
//     field.
package engine

// exprCoveredByIndexExprs is exprIsCoveredByIndex (where.c:3735): does e
// exactly match one of idx's own expression key columns (by structure,
// qualifier-blind -- exprEqualIgnoringQualifier, where_plan_exprindex_skip.go
// -- for the same reason that file needs the qualifier-blind comparison: a
// CREATE INDEX column's own expression is always unqualified).
func exprCoveredByIndexExprs(e Expr, idxExprs []Expr) bool {
	for _, ie := range idxExprs {
		if ie != nil && exprEqualIgnoringQualifier(e, ie) {
			return true
		}
	}
	return false
}

// exprCoveredForIndexScan is whereIsCoveringIndexWalkCallback (where.c:3779)
// for one expression tree: TRUE when every table-column reference inside e
// is either a column colIndexed reports as carried by the index, or sits
// inside a subtree that exactly matches one of idxExprs (WRC_Prune -- not
// descended into further, exactly like the C). FALSE (not covered) as soon
// as a bare column reference is found that is neither, OR the tree reaches a
// node shape this function does not model (conservative default, matching
// this file's SCOPE note).
//
// colIndexed answers for ONE table (this file's single-table scope): true
// for an ordinary column the index carries, and ALWAYS true for a rowid
// pseudo-reference (rowid/oid/_rowid_, unless shadowed by a real column of
// that name) -- every index implicitly carries the rowid
// (wherePlanIndexList's own "append the table key to the end" step).
func exprCoveredForIndexScan(e Expr, colIndexed func(name string) bool, idxExprs []Expr) bool {
	if e == nil {
		return true
	}
	if _, isCol := e.(ColumnExpr); !isCol && exprCoveredByIndexExprs(e, idxExprs) {
		return true
	}
	switch x := e.(type) {
	case ColumnExpr:
		return colIndexed(x.Name)
	case LiteralExpr, ParamExpr:
		return true
	case UnaryExpr:
		return exprCoveredForIndexScan(x.X, colIndexed, idxExprs)
	case BinaryExpr:
		return exprCoveredForIndexScan(x.L, colIndexed, idxExprs) &&
			exprCoveredForIndexScan(x.R, colIndexed, idxExprs)
	case IsNullExpr:
		return exprCoveredForIndexScan(x.X, colIndexed, idxExprs)
	case InExpr:
		if x.Sub != nil {
			return false
		}
		if !exprCoveredForIndexScan(x.X, colIndexed, idxExprs) {
			return false
		}
		for _, it := range x.List {
			if !exprCoveredForIndexScan(it, colIndexed, idxExprs) {
				return false
			}
		}
		return true
	case BetweenExpr:
		return exprCoveredForIndexScan(x.X, colIndexed, idxExprs) &&
			exprCoveredForIndexScan(x.Lo, colIndexed, idxExprs) &&
			exprCoveredForIndexScan(x.Hi, colIndexed, idxExprs)
	case LikeExpr:
		return exprCoveredForIndexScan(x.X, colIndexed, idxExprs) &&
			exprCoveredForIndexScan(x.Pattern, colIndexed, idxExprs) &&
			exprCoveredForIndexScan(x.Escape, colIndexed, idxExprs)
	case GlobExpr:
		return exprCoveredForIndexScan(x.X, colIndexed, idxExprs) &&
			exprCoveredForIndexScan(x.Pattern, colIndexed, idxExprs)
	case CollateExpr:
		return exprCoveredForIndexScan(x.X, colIndexed, idxExprs)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if !exprCoveredForIndexScan(a, colIndexed, idxExprs) {
				return false
			}
		}
		return true
	case CastExpr:
		return exprCoveredForIndexScan(x.X, colIndexed, idxExprs)
	case CaseExpr:
		if x.Base != nil && !exprCoveredForIndexScan(x.Base, colIndexed, idxExprs) {
			return false
		}
		for _, w := range x.Whens {
			if !exprCoveredForIndexScan(w.When, colIndexed, idxExprs) ||
				!exprCoveredForIndexScan(w.Then, colIndexed, idxExprs) {
				return false
			}
		}
		if x.Else != nil {
			return exprCoveredForIndexScan(x.Else, colIndexed, idxExprs)
		}
		return true
	case RowExpr:
		for _, el := range x.Elems {
			if !exprCoveredForIndexScan(el, colIndexed, idxExprs) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// stmtCoveredByExprIndex is whereIsCoveringIndex (where.c:3831) restricted to
// this port's already-modeled statement surface: WHERE, HAVING, every GROUP
// BY term, every ORDER BY term, and every non-star select-list item -- the
// same surface stmtMightReference (where_plan_exprindex_skip.go) walks,
// which is deliberate: anything that file cannot prove IRRELEVANT to (a
// GROUP BY/ORDER BY/subquery in stmt.From, or a sibling compound arm) this
// one also does not attempt to prove COVERED, for the identical reason
// (neither is wired to look there yet).
func stmtCoveredByExprIndex(stmt *SelectStmt, colIndexed func(name string) bool, idxExprs []Expr) bool {
	if len(stmt.Compound) > 0 {
		return false
	}
	if !exprCoveredForIndexScan(stmt.Where, colIndexed, idxExprs) {
		return false
	}
	if !exprCoveredForIndexScan(stmt.Having, colIndexed, idxExprs) {
		return false
	}
	for _, ge := range stmt.GroupBy {
		if !exprCoveredForIndexScan(ge, colIndexed, idxExprs) {
			return false
		}
	}
	for _, ot := range stmt.OrderBy {
		if !exprCoveredForIndexScan(ot.Expr, colIndexed, idxExprs) {
			return false
		}
	}
	for _, sc := range stmt.Columns {
		if sc.Star {
			return false
		}
		if !exprCoveredForIndexScan(sc.Expr, colIndexed, idxExprs) {
			return false
		}
	}
	return true
}
