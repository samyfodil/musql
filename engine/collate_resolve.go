// Collating-sequence resolution: deciding WHICH collation a comparison runs
// under, before value_compare.go applies it.
//
// SQLite's rule is a left-to-right walk with an explicit COLLATE winning over
// a column's declared collation, and a column's declared collation winning
// over nothing (sqlite3ExprCollSeq, expr.c:250-300; the binary-comparison
// fall-through to the right operand at expr.c:436-439). The subtleties are all
// in what counts as "nothing": a reference carrying no column identity at all
// falls through, while a column merely DECLARED BINARY does not -- see
// columnInfo.NoCollation.
package engine

// exprCollation reports the explicit collating sequence governing e as an
// operand of a comparison, ORDER BY, GROUP BY or DISTINCT, following
// sqlite3ExprCollSeq: a CollateExpr wins; CAST and unary "+" pass through; a
// "+ - * / % ||" BinaryExpr takes its left child's explicit collation, else
// its right's ("'a' || 'b' COLLATE NOCASE = 'AB'" is true). ok=false means no
// explicit collation anywhere in that chain.
//
// A bare column never contributes here, even with a declared collation: C
// picks that up only for a direct operand (possibly through CAST/"+"), never
// nested in an operator, function, CASE, BETWEEN or IN that carries no
// explicit COLLATE -- sqlite3ExprCollSeq descends a "||" only when it is
// EP_Collate. So "a || 'x' = 'Ax'" over a NOCASE column a compares BINARY. The
// direct-operand case is topExprCollation's.
func exprCollation(e Expr) (name string, ok bool) {
	switch x := e.(type) {
	case CollateExpr:
		return x.Name, true
	case collExpr:
		// A precomputed collation standing in for a subquery's result column
		// (subquery_validate.go), ranked here only when it was EXPLICIT there.
		return x.name, x.explicit
	case groupKeyExpr:
		// The GROUP BY key placeholder a rewritten aggregate item holds in
		// place of the key expression (sql_group.go). Same rule as collExpr:
		// ranked as an EXPLICIT collation only when the key expression wrote
		// one; a bare NOCASE column's DECLARED collation reaches a comparison
		// through topExprCollation's own case below, exactly as the bare
		// ColumnExpr this placeholder replaced would have.
		return x.coll, x.collExplicit && x.coll != ""
	case groupAggExpr:
		// The aggregate-call placeholder a rewritten item holds in place of the
		// call (sql_group.go). Only ever an EXPLICIT collation -- see
		// groupAggExpr.coll -- so it ranks here, exactly like the FuncExpr case
		// below that computed it.
		return x.coll, x.coll != ""
	case CastExpr:
		return exprCollation(x.X)
	case UnaryExpr:
		if x.Op == "+" {
			return exprCollation(x.X)
		}
		return "", false
	case BinaryExpr:
		switch x.Op {
		case "+", "-", "*", "/", "%", "||":
			if n, ok := exprCollation(x.L); ok {
				return n, true
			}
			return exprCollation(x.R)
		}
		return "", false
	case FuncExpr:
		// Verified directly against C SQLite (this package's own mined
		// TCL corpus, collate8.test): an explicit COLLATE buried inside a
		// scalar function call's ARGUMENT still governs an enclosing
		// comparison's collating sequence, e.g. "'abc' ==
		// ('ABC'||upper('' COLLATE nocase))" is TRUE -- the NOCASE attached
		// three levels down (inside upper()'s own argument) still wins for
		// the whole comparison. Mirrors SQLite's own sqlite3ExprCollSeq,
		// which does not stop at a function-call boundary; it scans the
		// call's argument list left to right for the first one carrying an
		// explicit collation.
		for _, a := range x.Args {
			if n, ok := exprCollation(a); ok {
				return n, true
			}
		}
		return "", false
	case CaseExpr:
		// Same principle as FuncExpr, and -- verified directly against the
		// same collate8.test -- a STATIC property of the CASE expression's
		// TEXT (scanned in declaration order: Base, then each WHEN/THEN
		// pair, then ELSE), NOT which branch actually executes at runtime:
		// "CASE WHEN 1-1=2 THEN '' COLLATE nocase ELSE '' COLLATE binary
		// END" resolves to NOCASE even though its ELSE branch (COLLATE
		// BINARY) is the one that actually runs (1-1=2 is false).
		if x.Base != nil {
			if n, ok := exprCollation(x.Base); ok {
				return n, true
			}
		}
		for _, w := range x.Whens {
			if n, ok := exprCollation(w.When); ok {
				return n, true
			}
			if n, ok := exprCollation(w.Then); ok {
				return n, true
			}
		}
		if x.Else != nil {
			return exprCollation(x.Else)
		}
		return "", false
	case BetweenExpr:
		if n, ok := exprCollation(x.X); ok {
			return n, true
		}
		if n, ok := exprCollation(x.Lo); ok {
			return n, true
		}
		return exprCollation(x.Hi)
	case InExpr:
		// The subquery form (x.Sub) is not scanned: its own result column's
		// collation isn't reachable as a plain Expr here, and this is
		// already a narrow, declared-scope extension of the base
		// left-operand-wins IN rule (see resolveCompareCollation's IN call
		// sites).
		if n, ok := exprCollation(x.X); ok {
			return n, true
		}
		for _, item := range x.List {
			if n, ok := exprCollation(item); ok {
				return n, true
			}
		}
		return "", false
	default:
		return "", false
	}
}

// columnDeclaredCollation resolves ce (a ColumnExpr) against ctx and reports
// the underlying column's DECLARED collation (columnInfo.Collation,
// normalized through effectiveCollation), or ok=false if ce doesn't resolve
// to a real column at all (an unresolvable reference -- the caller resolves
// the same reference itself and surfaces that as a proper error -- or the rowid
// pseudo-column, which has no declared collation of its own and is always
// numeric anyway; or columnInfo.NoCollation -- see that field's doc comment).
func columnDeclaredCollation(ctx *evalCtx, ce ColumnExpr) (name string, ok bool) {
	if ctx == nil {
		return "", false
	}
	_, _, col, rowidTableIdx, err := resolveColumnEx(ctx, ce)
	if err != nil || rowidTableIdx >= 0 || col == nil || col.NoCollation {
		return "", false
	}
	return effectiveCollation(col.Collation), true
}

// topExprCollation is resolveCompareCollation's per-operand step: the
// collation e contributes as a direct operand. A CollateExpr wins; CAST and
// unary "+" pass through; a bare column contributes its declared collation
// (columnDeclaredCollation); anything else falls back to exprCollation's
// explicit-only search, as in C.
func topExprCollation(ctx *evalCtx, e Expr) (name string, ok bool) {
	switch x := e.(type) {
	case CollateExpr:
		return x.Name, true
	case CastExpr:
		return topExprCollation(ctx, x.X)
	case UnaryExpr:
		if x.Op == "+" {
			return topExprCollation(ctx, x.X)
		}
		return "", false
	case ColumnExpr:
		return columnDeclaredCollation(ctx, x)
	case LiteralExpr:
		// A materialized correlated reference stands in for exactly such a
		// direct column operand; a literal written in SQL carries "" and so
		// contributes nothing here, as before.
		return x.coll, x.coll != ""
	case groupKeyExpr:
		return x.coll, x.coll != ""
	case groupBareColExpr:
		return x.coll, x.coll != ""
	default:
		return exprCollation(e)
	}
}

// declaredColumnCollation resolves only a bare column's declared collation
// (through CAST/unary "+"), never an explicit CollateExpr. It is for the GROUP
// BY-mode DISTINCT/ORDER BY declines in sql_group.go, which must decline a
// declared NOCASE/RTRIM column without also starting to decline explicit
// COLLATEs they already accepted.
func declaredColumnCollation(ctx *evalCtx, e Expr) (name string, ok bool) {
	switch x := e.(type) {
	case collExpr:
		// The declared-collation half of collExpr's rank; see exprCollation.
		return x.name, !x.explicit
	case groupKeyExpr:
		// The declared-collation half of groupKeyExpr's rank; see exprCollation.
		return x.coll, x.coll != "" && !x.collExplicit
	case groupBareColExpr:
		// A bare column reference's collation is always the DECLARED one.
		return x.coll, x.coll != ""
	case LiteralExpr:
		// Empty for a literal written in SQL; for a materialized correlated
		// reference, the substituted column's DECLARED collation -- which ranks
		// HERE and never in exprCollation, whose tier is EXPLICIT collations
		// only (LiteralExpr's doc comment).
		return x.coll, x.coll != ""
	case CastExpr:
		return declaredColumnCollation(ctx, x.X)
	case UnaryExpr:
		if x.Op == "+" {
			return declaredColumnCollation(ctx, x.X)
		}
		return "", false
	case ColumnExpr:
		return columnDeclaredCollation(ctx, x)
	default:
		return "", false
	}
}

// resolveCompareCollation picks the collation a comparison of lexpr and rexpr
// uses, following sqlite3BinaryCompareCollSeq: an explicit collation on lexpr,
// then on rexpr ("'ABC' COLLATE BINARY = 'abc' COLLATE NOCASE" is false); only
// if neither has one, lexpr's declared column collation, then rexpr's; else
// BINARY.
//
// Explicit on either side beats declared on either side. "x BETWEEN 'a'
// COLLATE nocase AND 'c' COLLATE nocase" compiles to "x >= 'a' COLLATE
// nocase", and the right operand's NOCASE governs even though x is a
// BINARY column (collate2.test, collate8.test).
//
// ctx may be nil (a schema-only probe), in which case no bare column resolves.
func resolveCompareCollation(ctx *evalCtx, lexpr, rexpr Expr) string {
	if n, ok := exprCollation(lexpr); ok {
		return n
	}
	if n, ok := exprCollation(rexpr); ok {
		return n
	}
	if n, ok := declaredColumnCollation(ctx, lexpr); ok {
		return n
	}
	if n, ok := declaredColumnCollation(ctx, rexpr); ok {
		return n
	}
	return "BINARY"
}

// riskyCrossTableDeclaredCollationOr reports whether e (a WHERE/ON clause) has
// an OR containing a cross-table "col = col" equality where a column's
// declared non-BINARY collation is involved. That is the trigger;
// applyRiskyCollationOrPlan (collation_or_plan.go) decides whether to rewrite
// with an explicit COLLATE, serve as written, or decline.
//
// The divergence is SQLite's OR-to-IN transform, which compares every disjunct
// under the shared column's declared collation (see collation_or_plan.go). e.g.
// with ta(a,b) and tb(c TEXT COLLATE NOCASE) holding 'aaa'/'AAA', "SELECT c FROM
// ta,tb WHERE a=c OR b=c" is one row with "CREATE INDEX itb ON tb(c)" and none
// without: the answer depends on the plan. An automatic index cannot stand in
// (whereLoopAddBtree skips it under an OR set, "!pBuilder->pOrSet").
//
// The check ignores whether an index exists: where2.test's t614/t615/t616
// statements all follow a CREATE INDEX on the column, so narrowing on index
// presence would close nothing and tie correctness to C's cost model.
func riskyCrossTableDeclaredCollationOr(e Expr, ctx *evalCtx) bool {
	b, ok := e.(BinaryExpr)
	if !ok {
		return false
	}
	switch b.Op {
	case "AND":
		return riskyCrossTableDeclaredCollationOr(b.L, ctx) || riskyCrossTableDeclaredCollationOr(b.R, ctx)
	case "OR":
		return subtreeHasCrossTableDeclaredCollationEq(b.L, ctx) ||
			subtreeHasCrossTableDeclaredCollationEq(b.R, ctx) ||
			riskyCrossTableDeclaredCollationOr(b.L, ctx) ||
			riskyCrossTableDeclaredCollationOr(b.R, ctx)
	}
	return false
}

// subtreeHasCrossTableDeclaredCollationEq scans e (one side of an OR) for any
// cross-table "col = col" equality where either column declares a non-BINARY
// collation. "Either" rather than "the resolved collation is non-BINARY":
// with ta(a,b BINARY) and tb(c COLLATE NOCASE, indexed), "a=c" alone compares
// BINARY, but "a=c OR b=c" matches under NOCASE once the OR transform applies,
// so both directions must be caught.
func subtreeHasCrossTableDeclaredCollationEq(e Expr, ctx *evalCtx) bool {
	b, ok := e.(BinaryExpr)
	if !ok {
		return false
	}
	switch b.Op {
	case "AND", "OR":
		return subtreeHasCrossTableDeclaredCollationEq(b.L, ctx) || subtreeHasCrossTableDeclaredCollationEq(b.R, ctx)
	case "=", "==":
		lc, lok := b.L.(ColumnExpr)
		rc, rok := b.R.(ColumnExpr)
		if !lok || !rok {
			return false
		}
		lt, lok2 := resolveColumnScopeName(ctx, lc)
		rt, rok2 := resolveColumnScopeName(ctx, rc)
		if !lok2 || !rok2 || equalFoldName(lt, rt) {
			return false
		}
		// Only the DECLARED case is risky -- an explicit COLLATE on either
		// operand is unambiguous and not the shape C SQLite's index
		// probe would silently override (an index carrying a non-BINARY
		// collation of its own would need to MISmatch the WHERE clause's
		// explicit COLLATE for the same quirk to arise, a narrower
		// scenario this conservative check doesn't need to chase).
		if _, ok := exprCollation(b.L); ok {
			return false
		}
		if _, ok := exprCollation(b.R); ok {
			return false
		}
		if n, ok := declaredColumnCollation(ctx, lc); ok && !equalFoldName(n, "BINARY") {
			return true
		}
		if n, ok := declaredColumnCollation(ctx, rc); ok && !equalFoldName(n, "BINARY") {
			return true
		}
		return false
	}
	return false
}
