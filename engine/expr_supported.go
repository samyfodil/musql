// The prepare-time support gate: whether the compiler can lower an expression
// at all.
//
// checkExprSupported walks a tree once, before code generation, and reports
// errVDBEUnsupported for a shape this engine cannot compile. That report is the
// whole mechanism AGENTS.md Rule 1 asks for: an unlowerable shape is an ERROR,
// never a fallback to a second executor. There is nothing left to fall back to.
//
// It is a walk over the AST, but it produces a VERDICT, not a value.
package engine

import (
	"fmt"
)

// checkLikelihoodLiteralArg reports an error unless arg is a bare numeric
// (INTEGER or REAL) literal strictly between 0.0 and 1.0 -- both conditions
// C SQLite itself enforces at compile time for likelihood()'s second
// argument (see checkExprSupported's FuncExpr case for why this can't live
// in callScalarFuncEnc instead). Verified directly: likelihood(1,0) and
// likelihood(1,1) BOTH error (the valid range is the OPEN interval, exact
// 0.0/1.0 excluded), and a non-literal expression errors regardless of its
// runtime value.
func checkLikelihoodLiteralArg(arg Expr) error {
	return checkLikelihoodLiteralArgNamed("likelihood", arg)
}

// checkLikelihoodLiteralArgNamed is checkLikelihoodLiteralArg with the
// function's name AS WRITTEN, which C SQLite echoes verbatim in the error
// ("LIKELIHOOD()" for an upper-case call, "likelihood()" for a lower-case
// one) -- verified directly.
func checkLikelihoodLiteralArgNamed(name string, arg Expr) error {
	lit, ok := arg.(LiteralExpr)
	if ok {
		// The argument must be a FLOATING-POINT literal -- one written with a
		// decimal point -- whose value lies in the INCLUSIVE range [0.0, 1.0].
		// Both halves verified directly against C SQLite: likelihood(1,
		// 0.0) and likelihood(1, 1.0) are accepted, while the INTEGER
		// literals likelihood(1, 0) and likelihood(1, 1) are the error, as
		// are -0.5, 2 and 'x'. (SQLite's own check is literally
		// "op != TK_FLOAT || r < 0.0 || r > 1.0".)
		if lit.Val.Typ == Float && lit.Val.F >= 0.0 && lit.Val.F <= 1.0 {
			return nil
		}
	}
	// semanticf, not fmt.Errorf: this is a diagnosis C SQLite raises at
	// PREPARE time with exactly this text, so every wrapper that decorates a
	// "the compiler cannot lower this" error has to let it through verbatim
	// (errVDBESemantic's own contract, subquery_validate.go). Checking the
	// argument when the call is CODED rather than parsed put two such
	// decorations in front of it -- "VDBE-only: FROM-less statement not
	// compilable to bytecode: ... (no fallback)" for a bare SELECT, and
	// "CREATE INDEX <name>: " for an index expression -- where C says only the
	// message itself.
	return semanticf("engine: second argument to %s() must be a constant between 0.0 and 1.0", name)
}

// checkExprSupported returns a non-nil error if e contains a construct the
// executor cannot evaluate — chiefly an unknown or aggregate function. It is
// run at plan time so such a query is reported as unsupported (an error) rather
// than silently "succeeding" when a WHERE clause filters every row before the
// unsupported expression would have been evaluated.
func checkExprSupported(e Expr) error {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return nil
	case windowResultExpr:
		// A rewritten WINDOW-function placeholder (sql_window.go): its own
		// argument/partition/order expressions were validated separately at
		// window-plan time; the placeholder itself is always fine.
		return nil
	case groupKeyExpr, groupAggExpr, groupBareColExpr:
		// Rewritten GROUP BY placeholders (sql_group.go): the underlying
		// group-key/aggregate/bare-column expression was validated when it was
		// rewritten; the placeholder itself is always fine. Reached when a GROUP
		// BY + window query re-validates a window aggregate whose argument is
		// itself a group aggregate.
		return nil
	case FuncExpr:
		if x.Star && len(x.Args) != 0 {
			// A lone "*" is a legal spelling of an EMPTY argument list for any
			// function (SQLite sets EP_Star and passes zero arguments; the
			// arity check below then decides) -- see compileFunc's evidence.
			// Anything else carrying Star is not a shape this package builds.
			return fmt.Errorf("engine: unsupported use of * in %s()", x.Name)
		}
		if fts5AuxFuncName(x.Name) {
			// An fts5 auxiliary call (bm25/snippet/highlight): its first
			// argument is the fts5 table name (not an evaluable expression),
			// and it is served by OpFts5Aux, not the scalar-func path.
			// Whether it is actually usable here is decided when the query's
			// fts5AuxState is built (a clean decline otherwise), not by the
			// generic supportedFuncs gate; only its trailing (marker/weight)
			// arguments need the ordinary shape check.
			if len(x.Args) >= 1 {
				return checkExprsSupported(x.Args[1:]...)
			}
			return nil
		}
		if fts3AuxFuncName(x.Name) || fts3OptimizeFuncName(x.Name) {
			// An fts3/fts4 auxiliary call (offsets()/matchinfo()) or
			// optimize(): like the fts5 ones above, the first argument names
			// the TABLE rather than being an evaluable expression, and whether
			// the call is usable at all is settled by compileFts3Aux /
			// compileFts3Optimize -- both of which decline cleanly when it is
			// not, and fall through to the ordinary "unsupported function"
			// rejection when the argument names no fts3 table.
			return nil
		}
		if isConnStateFunc(r33sFoldIdent(x.Name)) {
			// A connection-state function (conn_state.go): it is served by
			// OpConnState from the connection rather than by callScalarFuncEnc
			// from its arguments, and takes none. C SQLite rejects a call
			// with any at prepare time, in this wording.
			if len(x.Args) != 0 {
				return semanticf("engine: wrong number of arguments to function %s()", x.Name)
			}
			return nil
		}
		if lo, hi, ok := fts5ScalarFuncArity(x.Name); ok {
			// fts5's scalar functions (fts5_locale, fts5_source_id, fts5,
			// fts5_insttoken) are registered by the fts5 extension rather than
			// by the core, so they are a PREDICATE and not supportedFuncs
			// entries -- see fts5_locale.go for why an unconditional entry
			// would be a wrong answer in the default (no-fts5) build.
			if len(x.Args) < lo || len(x.Args) > hi {
				return semanticf("engine: wrong number of arguments to function %s()", x.Name)
			}
			return checkExprsSupported(x.Args...)
		}
		if !supportedFuncs[r33sFoldIdent(x.Name)] {
			return unknownOrUnsupportedFuncErr(x.Name)
		}
		// A wrong ARGUMENT COUNT is a prepare-time rejection C SQLite makes
		// too, so it is errVDBESemantic (semanticf) and not a decline: it is
		// the STATEMENT's error, not a shape this compiler is missing. As an
		// ordinary decline it reached the caller wrapped in "VDBE-only: ...
		// not compilable to bytecode ... (no fallback)", which buried the real
		// diagnosis -- "SELECT abs(1,2)" read as an engine limitation where
		// 3.53.3 says plainly "wrong number of arguments to function abs()".
		//
		// Arg-count is validated HERE, at plan time, rather than left to
		// evalFunc's own (still-present, defense-in-depth) per-call check:
		// evalFunc only ever runs once a row actually reaches it, so a
		// wrong-arity call over a table with zero matching rows (or, for a
		// FROM-less "SELECT coalesce(1)", a call whose OWN evalFunc arity
		// check was simply too lenient) would otherwise silently succeed
		// instead of erroring -- exactly the "zero rows masks a per-row-only
		// check" gap validateColumnRefs (query.go) already closes for column
		// references; funcArity closes the equivalent gap for function
		// arity. Verified directly against C SQLite: "SELECT
		// length(t1,5) FROM tbl1" over an empty tbl1 and "SELECT
		// coalesce(1)" (a single-argument call; SQLite requires 2+) both
		// error in C SQLite and, before this fix, silently succeeded
		// here.
		if min, max, ok := funcArity(r33sFoldIdent(x.Name)); ok {
			n := len(x.Args)
			if n < min || (max >= 0 && n > max) {
				return semanticf("engine: wrong number of arguments to function %s()", x.Name)
			}
		}
		if equalFoldName(x.Name, "likelihood") && len(x.Args) == 2 {
			// C SQLite's own bytecode compiler requires this argument to
			// be a literal constant -- NOT merely a runtime value that
			// happens to fall in (0.0, 1.0) -- rejecting even an
			// arithmetically-constant-foldable expression like "0.5+0.3"
			// (verified directly: likelihood(123, 0.5+0.3) errors in real
			// SQLite even though 0.8 is in range). This has to be checked
			// here, at plan time over the un-evaluated Expr, rather than in
			// callScalarFuncEnc (which only ever sees already-evaluated
			// Values and so cannot tell a literal from a computed
			// expression that landed on the same number).
			if err := checkLikelihoodLiteralArg(x.Args[1]); err != nil {
				return err
			}
		}
		return checkExprsSupported(x.Args...)
	case UnaryExpr:
		return checkExprSupported(x.X)
	case BinaryExpr:
		// A ROW VALUE is a supported comparison operand exactly when the OTHER
		// operand is a multi-column subquery -- "(a,b) = (SELECT x,y FROM t)",
		// compiled by compileRowSubCompare (vdbe_codegen.go). Every other
		// row-value comparison is already a scalar boolean tree by now (the
		// parser's desugarRowCompare and friends), so a RowExpr still standing
		// here in any other position is a genuine misuse and falls to the
		// default arm's decline -- see RowExpr's doc comment (sql_ast.go).
		if probe, sub, _, ok := rowSubOperands(x.Op, x.L, x.R); ok {
			if err := checkExprsSupported(probe.Elems...); err != nil {
				return err
			}
			return checkSubqueryColumnsSupported(sub)
		}
		return checkExprsSupported(x.L, x.R)
	case IsNullExpr:
		return checkExprSupported(x.X)
	case InExpr:
		// x.List is nil when this is the subquery form (x.Sub != nil), so
		// checkExprsSupported is a no-op there; x.Sub's OWN full validation
		// (WHERE, aggregate-mode dispatch, ...) still only happens every time
		// it actually runs, same as SubqueryExpr -- checkSubqueryColumnsSupported
		// just below is an additive, narrower safety net for its select-list.
		//
		// A ROW VALUE is a supported probe HERE and nowhere else: "(a,b) IN
		// (SELECT x,y FROM t)" is compiled by compileInSubquery
		// (vdbe_codegen.go), which spreads the row across a register block. The
		// default arm below still rejects a RowExpr in any other position, which
		// is what keeps SQLite's own "row value misused" shapes declined rather
		// than guessed at -- see RowExpr's doc comment (sql_ast.go). The
		// value-LIST form never reaches here at all (desugarRowIn rewrites it at
		// parse time), so this allowance is specifically the subquery form's.
		if rv, isRow := x.X.(RowExpr); isRow && x.Sub != nil {
			if err := checkExprsSupported(rv.Elems...); err != nil {
				return err
			}
			return checkSubqueryColumnsSupported(x.Sub)
		}
		if err := checkExprSupported(x.X); err != nil {
			return err
		}
		if err := checkExprsSupported(x.List...); err != nil {
			return err
		}
		return checkSubqueryColumnsSupported(x.Sub)
	case BetweenExpr:
		return checkExprsSupported(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return checkExprsSupported(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return checkExprsSupported(x.X, x.Pattern)
	case MatchExpr:
		return checkExprSupported(x.Pattern)
	case CollateExpr:
		return checkExprSupported(x.X)
	case CastExpr:
		return checkExprSupported(x.X)
	case CaseExpr:
		if x.Base != nil {
			if err := checkExprSupported(x.Base); err != nil {
				return err
			}
		}
		for _, w := range x.Whens {
			if err := checkExprSupported(w.When); err != nil {
				return err
			}
			if err := checkExprSupported(w.Then); err != nil {
				return err
			}
		}
		if x.Else != nil {
			return checkExprSupported(x.Else)
		}
		return nil
	case SubqueryExpr:
		// x.Stmt is validated by its own compile;
		// checkSubqueryColumnsSupported is a narrower safety net for its
		// select list, so an enclosing WHERE that never evaluates this
		// expression (zero outer rows) cannot leave it unvalidated -- as
		// InExpr's identical call does.
		return checkSubqueryColumnsSupported(x.Stmt)
	case ExistsExpr:
		// Same reasoning as SubqueryExpr.
		return checkSubqueryColumnsSupported(x.Stmt)
	case RaiseExpr:
		// A trigger-program RAISE(...) is supported (compileExpr's RaiseExpr
		// case, vdbe_codegen.go); its message expression, if any, must itself
		// be supported.
		if x.Ignore {
			return nil
		}
		return checkExprSupported(x.Msg)
	case RowExpr:
		// A ROW VALUE that reaches here has escaped every position SQLite allows
		// one in: the parser desugars "(a,b) = (1,2)" into scalar comparisons and
		// "(a,b) IN (SELECT ...)" into compileRowSubCompare, so what is left is a
		// vector where a scalar belongs. C SQLite says so wherever it notices,
		// and every one of its three sites is this message:
		//
		//	resolve.c:1458-1468  a comparison whose two sides have different
		//	                     vector SIZES ("(a,b) = 1")
		//	resolve.c:684-686    an ALIAS reference whose aliased expression is a
		//	                     vector
		//	expr.c:718-720       codeVectorCompare, the run-time half of the first
		//
		// A bare vector in a select list or an ORDER BY term lands on the same
		// message (sqlite3ExprVectorSize of the term is >1 where 1 is required).
		// semanticf, not a decline: this is a prepare-time rejection C makes too,
		// so it must reach the caller as the statement's error rather than inside
		// "VDBE-only: ... (no fallback)", which is what "SELECT (1,2)" used to
		// read as.
		return semanticf("engine: row value misused")
	default:
		return fmt.Errorf("engine: unsupported expression %T", e)
	}
}

func checkExprsSupported(es ...Expr) error {
	for _, e := range es {
		if err := checkExprSupported(e); err != nil {
			return err
		}
	}
	return nil
}

// checkSubqueryColumnsSupported statically validates a subquery's own
// select-list expressions for a construct checkExprSupported would reject
// (chiefly an unknown/unsupported scalar function -- e.g. sqlite_offset(),
// only ever built into C SQLite under a non-default compile-time option),
// WITHOUT requiring the subquery to ever actually be evaluated. This closes
// the same "zero outer rows never reaches the per-row check" gap
// validateColumnRefs/funcArity close elsewhere: a subquery that is never
// evaluated (every outer row filtered, or short-circuited) must still be
// rejected, as C's prepare-time validation rejects the whole statement
// whether or not rows exist.
//
// Deliberately narrow: only a select-list item that is NOT (and does not
// contain) an aggregate call is checked here -- an aggregate call needs
// GROUP-BY-less-aggregate-query validation (sql_agg.go's own, separate,
// context-aware rules: DISTINCT dedup, correct arity, no aggregate nested in
// another aggregate, ...), not checkExprSupported's plain scalar-function
// allowlist, which does not even recognize count/sum/avg/total/group_concat
// as supported names at all (see supportedFuncs' own doc comment) -- calling
// it directly on an aggregate expression would be a false positive, wrongly
// rejecting a perfectly valid "x IN (SELECT count(*) FROM t)". A compound
// subquery (UNION/INTERSECT/EXCEPT) or one with its own GROUP BY is left
// unchecked here too, for the identical reason: this is a plain, additive
// safety net for the unambiguous, common (row-mode, non-aggregate) select
// list, not a full re-implementation of every dispatch rule execSelect
// itself applies once the subquery actually runs.
func checkSubqueryColumnsSupported(stmt *SelectStmt) error {
	if stmt == nil || len(stmt.Compound) > 0 || len(stmt.GroupBy) > 0 {
		return nil
	}
	for _, c := range stmt.Columns {
		// A WINDOW call is skipped for exactly the reason an aggregate call is:
		// checkExprSupported's scalar-function allowlist does not know sum/
		// row_number/ntile/... and would reject "SELECT (SELECT sum(b) OVER
		// (ORDER BY b) FROM t2) FROM t1" as "unsupported function sum()" -- a
		// query C SQLite runs, and one this engine's OWN window compiler
		// (compileScanWindow, reached when the subquery body is compiled) then
		// handles correctly. The subquery's own compile is what validates it,
		// exactly as it does for the aggregate case above.
		if c.Star || containsAggregate(c.Expr) || exprHasWindow(c.Expr) {
			continue
		}
		if err := checkExprSupported(c.Expr); err != nil {
			return err
		}
	}
	return nil
}
