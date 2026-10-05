package engine

// This file is the ONE place a virtual-table module's "omit" decision is made
// and applied: the compile-time removal of a WHERE conjunct that the module
// itself guarantees.
//
// # Why the engine has to do anything at all
//
// C SQLite lets xBestIndex say "I have applied this constraint, do not apply it
// again" -- "pInfo->aConstraintUsage[i].omit = 1" -- and the term is then left
// out of the loop's WHERE code entirely. The chain is two hops:
// whereLoopAddVirtualOne records it, "if( pUsage[i].omit ){ ...
// pNew->u.vtab.omitMask |= 1<<iTerm; }" (where.c:4453-4456), and
// sqlite3WhereCodeOneLoopStart acts on it, "if( j<16 &&
// (pLoop->u.vtab.omitMask>>j)&1 ){ disableTerm(pLevel, pTerm); }"
// (wherecode.c:1616-1617). musql materializes a module's rows
// and then re-applies the STATEMENT's whole WHERE over them (vtab.go's own doc
// comment), so an omit had nowhere to land and VtabConstraintUsage.Omit was
// documented as advisory.
//
// For nearly every module that costs nothing, because the module ECHOES the
// value it was constrained by -- generate_series' Column hands "start" back
// unchanged, so "WHERE start=1" re-tests 1=1 -- and the two models agree.
//
// fts3tokenize does not echo. fts3tokColumnMethod reports column 0 as
// "sqlite3_result_text(pCtx, pCsr->zInput, -1, SQLITE_TRANSIENT)"
// (fts3_tokenize_vtab.c:388), i.e. the TEXT RENDERING of the constrained value,
// strlen-terminated -- while fts3tokBestIndexMethod set omit=1 on the very
// constraint that produced it (fts3_tokenize_vtab.c:248). So "SELECT * FROM t3
// WHERE input = 123" answers one row whose input is the TEXT '123'
// (fts3tok1.test 1.9, verified against 3.53.3), and re-applying "input = 123"
// over that row -- TEXT against INTEGER, a column with no affinity -- drops it.
// Without an omit this engine could only decline the statement, which is what
// it did.
//
// # The shape of the mechanism, and why it fails safe
//
// The decision is made HERE, at compile time, where the residual WHERE is
// built; the module is then TOLD what was done (VtabConstraint.Omitted, read
// back in BestIndex) rather than asked to trust that its request was honoured.
// That inversion is the whole safety property:
//
//   - a module that cannot serve a shape without the omission keeps declining
//     it unless it SEES the flag, so any path that skips this rewrite -- an
//     item nested inside a parenthesized join group, a compile that never
//     reaches compileScanAttempt -- loses an answer instead of dropping a row;
//   - and the conjunct is dropped only when exactly one source claims it, so
//     two same-named modules in one FROM cannot both believe they own it.
//
// The remaining obligation is the module's: OmittedConjunct must name the
// conjunct its own BestIndex will consume at RUN time. fts3TokModule discharges
// it by answering only for a LITERAL right-hand side -- see its own comment.

// vtabOmitConjuncts is compileScanAttempt's hook. It returns stmt unchanged unless
// some virtual-table source in srcs claims a conjunct of stmt.Where
// (joinSource.vtabOmitCandidate, asked of the module by resolveVtabSource), and
// otherwise returns a COPY whose Where has the claimed conjuncts removed,
// stamping each claiming item's FromItem.tvfOmitPlus1 so buildVtabConstraints
// can mark the matching constraint Omitted at run time.
//
// srcs is resolveJoinSources' own result, so an item strictly inside a
// parenthesized join group -- which never gets a top-level srcs entry -- is
// never stamped and never gets its omission. That is the safe direction (see
// this file's doc comment).
func vtabOmitConjuncts(stmt *SelectStmt, srcs []joinSource) *SelectStmt {
	if stmt == nil || stmt.Where == nil {
		return stmt
	}
	// claims[j] counts the sources claiming conjunct j; owner[j] is the one
	// source index that claimed it (meaningful only when the count is 1).
	var claims map[int]int
	var owner map[int]int
	for i := range srcs {
		j := srcs[i].vtabOmitCandidate - 1
		if j < 0 || srcs[i].vtabItem == nil {
			continue
		}
		if claims == nil {
			claims, owner = map[int]int{}, map[int]int{}
		}
		claims[j]++
		owner[j] = i
	}
	if len(claims) == 0 {
		return stmt
	}
	conj := splitTopLevelAnd(stmt.Where)
	drop := map[int]bool{}
	for j, n := range claims {
		if n != 1 || j >= len(conj) {
			// Two sources claiming one conjunct means neither may assume it
			// was dropped for IT; both keep declining. The length test is
			// belt-and-braces: tvfWhere IS this split list (withVtabWhere,
			// vtab.go, called from compileScanAttempt just above), so the indices
			// cannot be out of range -- but an index that were would drop
			// somebody else's filter, and AGENTS.md invariant 2 does not
			// distinguish "cannot happen" from "not checked".
			continue
		}
		drop[j] = true
		srcs[owner[j]].vtabItem.tvfOmitPlus1 = j + 1
	}
	if len(drop) == 0 {
		return stmt
	}
	var kept Expr
	for j := range conj {
		if drop[j] {
			continue
		}
		if kept == nil {
			kept = conj[j]
		} else {
			kept = BinaryExpr{Op: "AND", L: kept, R: conj[j]}
		}
	}
	s2 := *stmt
	s2.Where = kept
	return &s2
}

// vtabOmitLiteralRHS reports whether e is a constraint right-hand side whose
// value is decidable without any row, any cursor and any bound parameter --
// SQLite's own sqlite3ValueFromExpr contract, whose doc comment limits it to
// "very simple expressions that consist of one constant token (i.e. "5", "5.1",
// "'a string'")" (vdbemem.c:1969-1977, over valueFromExpr at :1795-1965), and
// whose body admits a leading unary minus over one of those ("if( op==TK_UMINUS
// ){ ... if( (pLeft->op==TK_INTEGER || pLeft->op==TK_FLOAT) )", vdbemem.c:1843).
//
// It is the gate that makes an omit claim provable rather than probable. A
// module's BestIndex consumes the first USABLE constraint, and usability is
// decided at run time by whether foldVtabInputValue could produce a value
// (vtab.go); a literal always can, so a claim restricted to literals names the
// conjunct BestIndex really will consume. Any other right-hand side -- a
// column of another FROM item, a subquery, a bound parameter -- might or might
// not fold, so a claim on one could name a conjunct BestIndex then skips, and
// the engine would have dropped a filter nothing applied.
//
// deferredErr is excluded for the same reason: a trigger-body literal whose
// magnitude check was held open (LiteralExpr.deferredErr, sql_ast.go) RAISES
// when compiled, so it does not fold and its constraint is not usable.
func vtabOmitLiteralRHS(e Expr) bool {
	switch x := e.(type) {
	case LiteralExpr:
		return x.deferredErr == ""
	case UnaryExpr:
		if x.Op != "-" && x.Op != "+" {
			return false
		}
		lit, ok := x.X.(LiteralExpr)
		return ok && lit.deferredErr == ""
	}
	return false
}
