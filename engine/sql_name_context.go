// This file holds position restrictions on what expressions can name or call.
// They need their own home because this engine resolves such a position by
// COMPILING the expression as a synthetic one-column FROM-less SELECT and
// running it once (foldLimitOffsetExpr, vdbe_codegen.go). That seam is right
// about EVALUATION -- AGENTS.md Rule 1: the expression is COMPILED and RUN,
// never evaluated off the AST -- but it is a strictly
// MORE PERMISSIVE RESOLVER than the position is, and the difference is a
// silent WRONG ANSWER rather than a gap: "SELECT row_number() OVER ()" is a
// perfectly legal FROM-less window query yielding 1, so compiling a LIMIT
// clause that way ANSWERS where 3.53.3 raises "misuse of window function
// row_number()".
//
// The general shape of the mistake, worth naming because the fold seam is
// spreading: a synthetic "SELECT <e>" is resolved by C SQLite with a SELECT's
// own NameContext -- NC_AllowAgg for the select list, plus NC_AllowWin (resolve.c:1966,
// "sNC.ncFlags = NC_AllowAgg|NC_AllowWin;") -- while the positions this engine folds
// are resolved with a FRESHLY ZEROED one. Every ncFlag the memset clears is a
// restriction the synthetic SELECT silently drops. So a fold at a restricted
// position must re-impose them explicitly; that is what lives here.
package engine

// This is the ONE implementation of that rule. Two were written independently
// -- one for LIMIT/OFFSET, one for VACUUM INTO and ATTACH -- and merged into
// this one, because two implementations of a single semantics is precisely what
// compileIndexExprs' doc comment warns about and RULE #1 bans: they drift, and
// the drift is a wrong answer at whichever position was not updated. The
// LIMIT/OFFSET version covered only the middle arm below; this one covers all
// three, so unifying on it is strictly more faithful at that position too.
//
// requireZeroedNameContext reproduces the resolve-time rejections C SQLite
// raises for an expression written at a position it resolves under a FRESHLY
// ZEROED NameContext -- one whose ncFlags carry neither NC_AllowAgg nor
// NC_AllowWin, so no aggregate and no window-function call may appear in it.
//
// RULE ZERO. The two positions in this package, each with the memset that
// makes its NameContext zeroed:
//
//	vacuum.c:128  "    if( pInto && sqlite3ResolveSelfReference(pParse,0,0,pInto,0)==0 ){"
//	              VACUUM INTO's TARGET. sqlite3ResolveSelfReference is passed
//	              pTab==0 and type==0, and its body is resolve.c:2316,
//	              "  memset(&sNC, 0, sizeof(sNC));", then resolve.c:2333,
//	              "  sNC.ncFlags = type | NC_IsDDL;" -- so with type 0 the two
//	              Allow bits are clear. Its own doc comment lists this as case
//	              "(4)   Expression arguments to VACUUM INTO." with the "type"
//	              flag column reading "0" (resolve.c:2286).
//
//	attach.c:373  "  memset(&sName, 0, sizeof(NameContext));"
//	              ATTACH's PATH. codeAttach zeroes ONE NameContext and runs all
//	              three of its arguments through resolveAttachExpr against it
//	              (attach.c:377-379).
//
// The SAME memset governs LIMIT/OFFSET -- resolve.c:1903,
// "    memset(&sNC, 0, sizeof(sNC));", under the comment "These are not
// allowed to refer to any names, so pass an empty NameContext." -- which is
// why an UPSERT's "excluded." cannot resolve there either (the excluded arm at
// resolve.c:547 tests "(pNC->ncFlags & NC_UUpsert)!=0"). So this is the shared
// restriction those positions need, not a VACUUM/ATTACH special case; only the
// call sites differ.
//
// Note the ASYMMETRY, because it is easy to get backwards: a TRIGGER's new./
// old. still resolve under a zeroed NameContext, since resolve.c:525 gates
// that arm on "pParse->pTriggerTab!=0" -- a PARSE field, which no memset of a
// NameContext can clear.
func requireZeroedNameContext(e Expr) error {
	var bad error
	// PRE-ORDER, and NOT into a subquery. sqlite3WalkExpr calls its expression
	// callback on a node before walking that node's children (walker.c), so
	// the OUTERMOST offending call is the one SQLite reports -- "VACUUM INTO
	// ltrim(min('x.db'))" is "misuse of aggregate function min()" only because
	// ltrim is not itself an offender. And a subquery carries its OWN
	// NameContext, so an aggregate inside one is legal at these positions:
	// verified against the 3.53.3 oracle, which WRITES the copy for
	// "VACUUM INTO (SELECT min('x.db'))" and refuses the statement for the
	// bare "VACUUM INTO min('x.db')". walkExprShallow is exactly that walk --
	// pre-order over FuncExpr nodes, descending into ordinary arguments and
	// stopping at a subquery (its default arm).
	walkExprShallow(e, func(fc FuncExpr) bool {
		bad = misusedAggOrWindowCall(fc)
		return bad == nil
	})
	return bad
}

// misusedAggOrWindowCall is resolve.c's verdict on ONE function call whose
// NameContext allows neither aggregates nor window functions, in resolve.c's
// own arm order:
//
//	resolve.c:1258  "        if( pDef && pDef->xValue==0 && pWin ){"
//	                -> :1260 "              \"%#T() may not be used as a window function\", pExpr"
//	resolve.c:1264  "              (is_agg && (pNC->ncFlags & NC_AllowAgg)==0)"
//	         :1265  "           || (is_agg && (pDef->funcFlags&SQLITE_FUNC_WINDOW) && !pWin)"
//	         :1266  "           || (is_agg && pWin && (pNC->ncFlags & NC_AllowWin)==0)"
//	                -> :1274 "          sqlite3ErrorMsg(pParse, \"misuse of %s function %#T()\",zType,pExpr);"
//	                with zType chosen at :1268-1272, "window" when
//	                "(pDef->funcFlags & SQLITE_FUNC_WINDOW) || pWin" and
//	                "aggregate" otherwise.
//	resolve.c:1297  "        else if( is_agg==0 && ExprHasProperty(pExpr, EP_WinFunc) ){"
//	                -> :1299 "              \"FILTER may not be used with non-aggregate %#T()\","
//
// Every one of the three OR'd conditions at :1264-1266 fires here, because
// both Allow bits are clear -- which is why the whole arm collapses to "is_agg".
//
// pWin is "IsWindowFunc(pExpr)" (resolve.c:1252), and that macro is
// sqliteInt.h:3212, "# define IsWindowFunc(p) ( \" / :3213,
// "    ExprHasProperty((p), EP_WinFunc) && p->y.pWin->eFrmType!=TK_FILTER" --
// so a FILTER written WITHOUT an OVER leaves pWin zero. That is exactly
// FuncExpr.Over vs FuncExpr.Filter here, and the oracle confirms the split:
// "min('x') FILTER (WHERE 1)" is "misuse of AGGREGATE function min()" while
// "min('x') OVER ()" is "misuse of WINDOW function min()".
//
// One ordering difference is accepted rather than papered over: resolve.c
// reaches the FILTER arm only after its "no such function" and "wrong number
// of arguments" arms (:1287, :1292), so "VACUUM INTO nosuchfn('x') FILTER
// (WHERE 1)" is "no such function: nosuchfn" in the oracle and the FILTER
// message here. Both engines REJECT; only the wording differs, and this
// engine's wording differs from SQLite's at every other decline too.
func misusedAggOrWindowCall(fc FuncExpr) error {
	// resolve.c's is_agg is "pDef->xFinalize!=0" -- true for every aggregate
	// AND for every SQLITE_FUNC_WINDOW builtin, and INDEPENDENT of whether the
	// call carries an OVER clause. isAggregateCall answers a deliberately
	// different question (false for any windowed call, so a window call cannot
	// flip a query into aggregate mode), so the OVER is stripped before asking
	// it; everything else about the aggregate/scalar ambiguity -- min/max being
	// the aggregate at exactly one argument, count(*) -- is already settled
	// there and is reused rather than restated. isWindowFunctionName
	// (vdbe_window.go) is SQLITE_FUNC_WINDOW's own set, name for name, from
	// window.c:612-626's WINDOWFUNCX/WINDOWFUNCALL/WINDOWFUNCNOOP table.
	bare := fc
	bare.Over = nil
	isWin := isWindowFunctionName(r33sFoldIdent(fc.Name))
	isAgg := isWin || isAggregateCall(bare)
	switch {
	case !isAgg && fc.Over != nil:
		return semanticf("engine: %s() may not be used as a window function", fc.Name)
	case isAgg:
		zType := "aggregate"
		if isWin || fc.Over != nil {
			zType = "window"
		}
		return semanticf("engine: misuse of %s function %s()", zType, fc.Name)
	case fc.Filter != nil:
		return semanticf("engine: FILTER may not be used with non-aggregate %s()", fc.Name)
	case len(fc.OrderBy) > 0:
		// resolve.c:1306-1307, the arm right below FILTER's.
		return semanticf("engine: ORDER BY may not be used with non-aggregate %s()", fc.Name)
	}
	return nil
}
