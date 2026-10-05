// This file holds the seam between a RESOLVED column reference and the row it
// names: resolveColumnEx (column_scope.go) answers WHERE a ColumnExpr binds in
// the live scope chain, and the read below answers WHAT is in that slot.
//
// It is a value primitive, not an evaluator. C SQLite has the same split and
// also keeps it out of its evaluator: resolution decides WHICH slot
// (sqlite3ColumnIndex in lookupName, resolve.c:562-567, where a rowid alias
// becomes "iCol = -1"), and the read is coded from that slot alone --
// sqlite3ExprCodeGetColumn (expr.c:4483) -> sqlite3ExprCodeGetColumnOfTable
// (expr.c:4426), whose whole branch is the one below's:
//
//	expr.c:4437   if( iCol<0 || iCol==pTab->iPKey ){
//	expr.c:4438     sqlite3VdbeAddOp2(v, OP_Rowid, iTabCur, regOut);
//
// with everything else falling through to OP_Column (vdbe.c:2975). Nothing on
// that path is valueFromExpr, and this engine's compiled path already carries
// the same half (resolveRowCtxValue, vdbe_codegen.go). The rest of the
// scope-resolution family -- tableScope, evalCtx, resolveColumn/
// resolveColumnEx, resolveCoalesceChain, buildColIndex, isRowidAliasName --
// lives next door in column_scope.go.
package engine

import "fmt"

// columnRowValue reads the value a ColumnExpr resolves to out of ctx's live
// row -- the ONE node kind whose value comes from the row rather than from
// opcodes: resolveColumnEx answers WHERE the reference binds, and this answers
// WHAT is there.
//
// Its single caller is the substitution pass that needs exactly this:
// rewriteExprOuterRefs' ColumnExpr case (outer_ref.go), materializing an outer
// row's column as a literal, reached from the COMPILED read path through
// query.go's limitOffsetPseudoRowRefs -> rewriteSelectOuterRefs.
//
// NOT folded into resolveRowCtxValue (vdbe_codegen.go), which resolves the same
// reference for the compiler and looks interchangeable but is not, in two
// corners this call site would silently change:
//
//   - ORDER. This checks the fts5 "rank" pseudo-column BEFORE resolveColumnEx;
//     resolveRowCtxValue checks it after, so it answers bm25() for a REAL
//     column named "rank" on an fts5-aux query and never for the pseudo-column,
//     which resolveColumnEx cannot resolve at all.
//   - ColumnExpr.FallbackLiteral. This serves it on an unqualified miss;
//     resolveRowCtxValue reports "not resolvable" instead.
//
// Reconciling the two is still open, and those two corners are what any
// reconciliation has to answer for.
func columnRowValue(ctx *evalCtx, x ColumnExpr) (Value, error) {
	// The fts5 "rank" hidden column: when this query carries an fts5
	// auxiliary context (see fts5AuxState), a bare "rank" (or "t.rank")
	// evaluates to the row's default bm25() score, exactly as C fts5's
	// rank column does. Intercepted before ordinary column resolution
	// because fts5 reserves the name (there is no real "rank" column to
	// shadow it).
	if ctx.fts5Aux != nil && x.Schema == "" && equalFoldName(x.Name, "rank") && ctx.fts5Aux.matchesRankQualifier(x.Qualifier) {
		return ctx.fts5Aux.bm25(ctx, nil), nil
	}
	// A three-part "schema.table.column" reference's database qualifier
	// (ColumnExpr.Schema) is validated at PLAN time by validateColumnRefs
	// (query.go), which holds the pager to check it against this database's
	// own schema; an invalid qualifier declines the whole statement there,
	// before any row is evaluated. By the time execution reaches this
	// per-row read the qualifier is known-good, so it is simply ignored and
	// the reference resolved as the plain two-part "Qualifier.Name" -- which
	// is exactly what a locally-resolving "main.t.c" means ("t.c"). Ignoring
	// it here (rather than re-checking) is what lets the single-table WHERE
	// push-down filter -- whose mini evalCtx deliberately carries no pager
	// (join.go) -- evaluate an already-validated three-part reference.
	foundCtx, idx, _, rowidTableIdx, err := resolveColumnEx(ctx, x)
	if err != nil {
		if x.FallbackLiteral != nil && unqualifiedColumnNotFoundErr(err, x.Name) {
			return *x.FallbackLiteral, nil
		}
		return Value{}, err
	}
	if rowidTableIdx >= 0 {
		if rowidTableIdx >= len(foundCtx.rowids) {
			// Same "no row context" guard as the ordinary-column case
			// below, for a pseudo-column reference reached through a
			// scope with no live row source (e.g. a schema-only probe
			// ctx -- see rowids' doc comment).
			return Value{}, fmt.Errorf("engine: no such column: %s (no row context available)", x.Name)
		}
		return foundCtx.rowids[rowidTableIdx], nil
	}
	if idx >= len(foundCtx.vals) {
		// A correlated reference into a collapsed GROUP BY group resolves
		// to the group's (uniform) key value when -- and only when -- the
		// referenced column is one of that query's GROUP BY key columns:
		// its value is well-defined for the whole group even though the
		// group carries no live per-row context. See groupKeyVals' doc
		// comment (evalCtx, column_scope.go).
		if v, ok := foundCtx.groupKeyVals[idx]; ok {
			return v, nil
		}
		// The resolved scope has no row values: a bare column reference there is
		// unsupported, not a crash -- planAggItem rejects this for the
		// current scope's own columns, and a scope reached via the chain
		// needs the same guard here.
		return Value{}, fmt.Errorf("engine: no such column: %s (no row context available)", x.Name)
	}
	return foundCtx.vals[idx], nil
}
