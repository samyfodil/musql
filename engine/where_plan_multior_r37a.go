// WHERE_MULTI_OR, the multi-index OR access method, ported from
// exprAnalyzeOrTerm's case 3 (whereexpr.c), whereLoopAddOr (where.c) and
// sqlite3WhereCodeOneLoopStart's case-5 arm (wherecode.c).
//
// An OR whose disjuncts all constrain one table may run one sub-scan per
// disjunct, de-duplicated by rowid through a RowSet. The row set equals a full
// scan's, but the order is the concatenation of sub-scans, and a plain SELECT
// has no decline channel -- so without this, shapes like "WHERE a<2 OR a>2"
// and "WHERE a=1 OR b='x'" came back in the wrong order.
//
// The order: wherecode.c:2404 runs disjuncts in clause order, and each
// sub-scan's OP_RowSetTest emits a row only the first time its rowid is seen.
// So a row comes out in the pass of the first disjunct it satisfies, in that
// sub-scan's index order. One stable sort by
//
//	(index of the first disjunct the row satisfies, that sub-scan's index key)
//
// reproduces it, as OpAutoIndexOrder does for an automatic index. Rows
// satisfying no disjunct sort last and are filtered by the WHERE as before.
//
// Scope: a one-table statement (wherePlanSingleTableIndexOrder) whose every
// disjunct analyses to a single indexable term. An AND-connected disjunct gets
// WO_AND and its own WhereClause in C, and declines here.
package engine

// whereOrInfo is WhereTerm.u.pOrInfo (whereInt.h): the broken-out sub-clause of
// an OR term plus the set of tables every disjunct constrains.
type whereOrInfo struct {
	// indexable is WhereOrInfo.indexable: the AND over disjuncts of the bitmask
	// of the table that disjunct constrains. Zero means case 3 does not apply and
	// no WHERE_MULTI_OR loop exists.
	indexable whereMask
	// subs is pOrInfo->wc.a[]: one term per disjunct in clause order, then
	// the virtual terms derived from them (a commuted column comparison, the
	// "x>NULL" child of "x IS NOT NULL") in reverse disjunct order, since
	// sqlite3WhereExprAnalyze walks backwards (whereexpr.c:1890). Both
	// whereLoopAddOr and case 5 walk the whole list, so a commuted copy is a
	// sub-scan of its own (rowid.test rowid-15.1 shows "INDEX 1" and "INDEX
	// 3").
	//
	// exprs is parallel and holds each entry's originating disjunct (a copy
	// has the same truth value): the codegen re-analyses it (C re-runs
	// sqlite3WhereBegin per sub-scan) and uses it to assign rows to
	// sub-scans.
	subs  []whereIdxTerm
	exprs []Expr
	// and is parallel to subs: for a WO_AND disjunct -- one that is an AND, or
	// that has no single operator of its own -- its own analysed sub-clause
	// (pOrTerm->u.pAndInfo->wc), nil for a single-operator one.
	and []*whereAndInfo

	// jts/scopes are the resolved FROM clause the disjuncts were analysed
	// against, kept so the sub-plan can be built where the winning loop is known
	// rather than threaded through wherePlanInput. scopes is also the runtime
	// evaluation scope -- see multiOrOrder (vdbe_op.go).
	jts    []joinedTable
	scopes []tableScope

	// others is pAndExpr (wherecode.c:2374): the OTHER top-level WHERE conjuncts
	// that carry an eOperator, which the C ANDs onto every disjunct before
	// handing it to the sub-sqlite3WhereBegin so those terms can drive the
	// sub-scan's index too. Filled by wherePlanTermsFrom once every term is
	// analysed. otherIdx is parallel: each one's index in the analysed term
	// list, which the join half reads for the term's join markings and for
	// whether an outer loop has already coded it (TERM_CODED).
	others   []Expr
	otherIdx []int

	// outerON/onItem are the OR term's own join markings (see whereConj): an OR
	// lifted out of an ON clause, whose every disjunct carries them too.
	outerON bool
	onItem  int
}

// woSingle is WO_SINGLE (whereInt.h:631): the operators of a term that
// constrains ONE column. exprAnalyzeOrTerm treats a disjunct without one of
// these as a WO_AND sub-clause of its own (whereexpr.c:733).
const woSingle = woIn | woEq | woLT | woLE | woGT | woGE | woIsNull | woIs

// whereOrCost / whereOrSet port WhereOrCost / WhereOrSet (whereInt.h): the
// (prereq, rRun, nOut) triples whereLoopAddOr accumulates instead of whole
// WhereLoops while it is pricing a disjunct.
type whereOrCost struct {
	prereq     whereMask
	rRun, nOut logEst
}

// whereNOrCost is N_OR_COST (whereInt.h:110).
const whereNOrCost = 3

type whereOrSet struct {
	a [whereNOrCost]whereOrCost
	n int
}

// whereOrInsert ports whereOrInsert (where.c:208): keep the N_OR_COST best
// entries seen so far, where "best" is cheaper-and-no-more-prerequisites.
func (s *whereOrSet) insert(prereq whereMask, rRun, nOut logEst) {
	for i := 0; i < s.n; i++ {
		p := &s.a[i]
		if rRun <= p.rRun && prereq&p.prereq == prereq {
			p.prereq = prereq
			p.rRun = rRun
			if p.nOut > nOut {
				p.nOut = nOut
			}
			return
		}
		if p.rRun <= rRun && p.prereq&prereq == p.prereq {
			return
		}
	}
	var p *whereOrCost
	if s.n < whereNOrCost {
		p = &s.a[s.n]
		s.n++
		p.nOut = nOut
	} else {
		p = &s.a[0]
		for i := 1; i < s.n; i++ {
			if p.rRun > s.a[i].rRun {
				p = &s.a[i]
			}
		}
		if p.rRun <= rRun {
			return
		}
	}
	p.prereq = prereq
	p.rRun = rRun
	if p.nOut > nOut {
		p.nOut = nOut
	}
}

// wherePlanOrTermInfo ports exprAnalyzeOrTerm's case 3 (whereexpr.c:692) over
// the disjunct shapes this port can price, returning the WhereOrInfo C
// attaches to a TK_OR term, or ok == false (declining the statement).
//
// outerON/onItem are the OR's join markings, inherited by every disjunct
// (sqlite3SetJoinExpr marks the whole ON). The OR's own clause holds only its
// disjuncts ("sqlite3WhereSplit(pOrWc, pExpr, TK_OR);
// sqlite3WhereExprAnalyze(pSrc, pOrWc)", whereexpr.c:722), so each is analysed
// with the ON clauses stripped.
//
// Declined:
//
//   - the WO_EQUIV-only commuted copy of a same-table "a=b", which becomes a
//     WO_AND sub-clause of its own (whereexpr.c:733);
//   - a two-way OR that whereCombineDisjuncts (whereexpr.c:557) folds into an
//     extra conjunct on the outer clause, which changes every loop's plan.
//
// inert reports that some disjunct has no eOperator (e.g. a LIKE no range
// serves), so cases 1-3 all fail and the OR is an opaque conjunct.
func wherePlanOrTermInfo(jts []joinedTable, scopes []tableScope, e Expr, outerON bool, onItem int) (info *whereOrInfo, isMultiOr, inert bool) {
	be, isBin := e.(BinaryExpr)
	if !isBin || !whereIsPlainOr(be) {
		return nil, false, false
	}
	if wherePlanExprHasCollate(e) {
		// See wherePlanOrTermIsInert: exprAnalyzeOrTerm never runs for this term,
		// so there is no WhereOrInfo to build. The caller admits it as an inert
		// conjunct instead of declining.
		return nil, false, false
	}
	// sqlite3WhereSplit(pOrWc, pExpr, TK_OR) flattens the whole OR tree.
	var subs []Expr
	var split func(Expr)
	split = func(x Expr) {
		if b, ok := x.(BinaryExpr); ok && whereIsPlainOr(b) {
			split(b.L)
			split(b.R)
			return
		}
		subs = append(subs, x)
	}
	split(be)
	if len(subs) < 2 {
		return nil, false, false
	}
	own := make([]joinedTable, len(jts))
	copy(own, jts)
	for i := range own {
		own[i].on = nil
	}
	mark := func(x Expr) []whereConj {
		var out []whereConj
		for _, cj := range whereSplitAnd(x) {
			out = append(out, whereConj{e: cj, outerON: outerON, onItem: onItem})
		}
		return out
	}
	info = &whereOrInfo{jts: jts, scopes: scopes, outerON: outerON, onItem: onItem}
	// copied[k] is TERM_COPIED on wc.a[k] (a single-operator disjunct whose
	// commuted copy exists), and parentCur[k] the leftCursor of a VIRTUAL entry's
	// parent: exprAnalyzeOrTerm skips the former and ORs the latter into the
	// entry's own table when it computes indexable (whereexpr.c:765-773).
	var copied []bool
	var parentCur []int
	type child struct {
		t     whereIdxTerm
		disj  int
		pcurs int
		and   *whereAndInfo
	}
	kids := make([][]child, len(subs))
	for di, s := range subs {
		var st []whereIdxTerm
		single := false
		if inner, _, _ := whereSkipCollateAndLikely(s); !isAndExpr(inner) {
			// In the OR's own clause (pOrWc, op TK_OR), where the LIKE
			// optimization and BETWEEN's children do not run.
			var ok bool
			st, ok = wherePlanTermsFromConj(own, scopes, mark(s), false, false)
			if !ok || len(st) == 0 || st[0].virt {
				return nil, false, false
			}
			single = st[0].op&woSingle != 0 && st[0].cursor >= 0
		}
		if !single {
			// exprAnalyzeOrTerm (whereexpr.c:733-764): a disjunct whose own
			// eOperator is not a single comparison -- an AND, or a term with no
			// operator at all -- is split into its own AND clause and analysed
			// there (pAndWC), and the tables its comparison terms constrain are
			// what it contributes to indexable.
			at, ok := wherePlanTermsFromConj(own, scopes, mark(s), true, false)
			if !ok {
				return nil, false, false
			}
			var bits whereMask
			// pWC->nBase is the clause's length as of its last NON-virtual
			// insert (whereClauseInsert, whereexpr.c:79) -- ordinarily its
			// count of base terms, but a row-value equality's slices are
			// appended after virtual terms without being virtual themselves.
			// (The sliced original turns virtual only afterwards, and lies
			// before its slices anyway.)
			nBase := 0
			for i := range at {
				if !at[i].virt {
					nBase = i + 1
				}
				if at[i].cursor >= 0 && at[i].op != 0 {
					bits |= whereMask(1) << uint(at[i].cursor)
				}
			}
			info.subs = append(info.subs, whereIdxTerm{cursor: -1, parent: -1, truthProb: 1})
			info.and = append(info.and, &whereAndInfo{terms: at, nBase: nBase, bits: bits})
		} else {
			info.subs = append(info.subs, st[0])
			info.and = append(info.and, nil)
		}
		info.exprs = append(info.exprs, s)
		copied = append(copied, single && len(st) > 1)
		parentCur = append(parentCur, -1)
		for _, v := range st[min(1, len(st)):] {
			if !v.virt || v.cursor < 0 {
				return nil, false, false
			}
			k := child{t: v, disj: di, pcurs: st[0].cursor}
			if v.op&woSingle == 0 {
				// The commuted copy of an operator-less comparison ("t.a<t.b",
				// both sides one table) is a WO_AND entry of its own, whose
				// pAndWC is the copy split as an AND clause: the same two
				// terms as its parent's, the same bits and the same loops, so
				// the parent's analysis stands for it. A WO_EQUIV copy is not
				// the same -- whereScanNext walks an equivalence from its
				// left column -- and is declined.
				if v.op != 0 || info.and[len(info.and)-1] == nil {
					return nil, false, false
				}
				k.and = info.and[len(info.and)-1]
			}
			kids[di] = append(kids[di], k)
		}
	}
	// The virtual entries, in the order the backwards walk created them.
	for di := len(subs) - 1; di >= 0; di-- {
		for _, k := range kids[di] {
			k.t.parent = -1
			if k.and != nil {
				k.t = whereIdxTerm{cursor: -1, parent: -1, truthProb: 1}
			}
			info.subs = append(info.subs, k.t)
			info.and = append(info.and, k.and)
			info.exprs = append(info.exprs, subs[k.disj])
			copied = append(copied, false)
			parentCur = append(parentCur, k.pcurs)
		}
	}
	// "indexable = ~(Bitmask)0; for(...pOrTerm=pOrWc->a...)" (whereexpr.c:731).
	info.indexable = ^whereMask(0)
	for k := range info.subs {
		switch {
		case info.and[k] != nil:
			info.indexable &= info.and[k].bits
		case copied[k]:
			// "Skip this term for now. We revisit it when we process the
			// corresponding TERM_VIRTUAL term."
		default:
			b := whereMask(1) << uint(info.subs[k].cursor)
			if parentCur[k] >= 0 {
				b |= whereMask(1) << uint(parentCur[k])
			}
			info.indexable &= b
		}
	}
	if info.indexable == 0 {
		// No table every disjunct constrains: case 3 builds nothing, and case
		// 2's two comparisons on one column cannot exist either (case 1, all
		// "==" on one column, was wherePlanOrToIn's, before this ran).
		return nil, false, true
	}
	// "if( indexable && pOrWc->nTerm==2 )" -- nTerm counts the virtual entries,
	// so an OR with a commuted copy never reaches case 2.
	if len(info.subs) == 2 && info.and[0] == nil && info.and[1] == nil &&
		wherePlanOrCombines(&info.subs[0], &info.subs[1], subs[0], subs[1]) {
		return nil, false, false
	}
	if wherePlanOrMayChangeToIn(info, copied, parentCur) {
		return nil, false, false
	}
	return info, true, false
}

// wherePlanOrMayChangeToIn reports an OR whose case 1 (whereexpr.c:809-900)
// might still rewrite it into an IN that wherePlanOrToIn -- which runs first,
// and serves only literal right-hand sides -- did not build. chngToIN is the
// AND, over every single-operator entry of pOrWc that is not TERM_COPIED, of
// the tables it names, and is 0 once any entry is WO_AND or not WO_EQ
// (whereexpr.c:765-786); the C then converts when some table in it has every
// one of its entries on ONE column. Answered conservatively -- the affinity
// test of ticket #2249 that can still refuse the rewrite is not asked -- so
// the error is only ever a decline: "a=c OR b=c" is the IN "c IN (a,b)" to
// the C (EXPLAIN QUERY PLAN: SEARCH tb USING COVERING INDEX tb_c (c=?)), never
// a MULTI-INDEX OR.
func wherePlanOrMayChangeToIn(info *whereOrInfo, copied []bool, parentCur []int) bool {
	chngToIN := ^whereMask(0)
	for k := range info.subs {
		switch {
		case info.and[k] != nil || info.subs[k].op&woEq == 0:
			return false
		case copied[k]:
		default:
			b := whereMask(1) << uint(info.subs[k].cursor)
			if parentCur[k] >= 0 {
				b |= whereMask(1) << uint(parentCur[k])
			}
			chngToIN &= b
		}
	}
	for k := range info.subs {
		t := info.subs[k].cursor
		if chngToIN&(whereMask(1)<<uint(t)) == 0 {
			continue
		}
		same := true
		for j := range info.subs {
			if info.subs[j].cursor == t && info.subs[j].column != info.subs[k].column {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// whereAndInfo is WhereAndInfo: a WO_AND disjunct's own WhereClause, analysed
// as an AND clause (so the LIKE optimization, BETWEEN and IS NOT NULL give it
// range terms), with nBase its non-virtual terms and bits what it contributes
// to indexable: the tables of its terms that carry an operator
// (whereexpr.c:754-763).
type whereAndInfo struct {
	terms []whereIdxTerm
	nBase int
	bits  whereMask
}

// wherePlanTermsHaveOr reports whether any analysed term is an OR that
// exprAnalyzeOrTerm's case 3 accepted, i.e. one addOr may build a
// WHERE_MULTI_OR loop from.
func wherePlanTermsHaveOr(terms []whereIdxTerm) bool {
	for i := range terms {
		if terms[i].orInfo != nil {
			return true
		}
	}
	return false
}

// wherePlanOrTermIsInert reports an OR exprAnalyze leaves alone entirely (no
// WhereOrInfo, no case-1 IN, no case-2 conjunct, no MULTI_OR loop). C's arm is
// "pExpr->op==TK_OR && !ExprHasProperty(pExpr, EP_Collate)" (whereexpr.c:1319),
// and EP_Collate propagates, so any COLLATE inside the OR suffices: "WHERE a=1
// OR b='x' COLLATE NOCASE" over an index on (a,b) is a plain covering scan.
//
// Every disjunct must still be whereTermAnalyzable: this port's prereqAll
// comes from collectTableRefs, which does not follow subqueries, so a subquery
// would under-report prerequisites.
func wherePlanOrTermIsInert(e Expr) bool {
	be, isBin := e.(BinaryExpr)
	if !isBin || !whereIsPlainOr(be) || !wherePlanExprHasCollate(e) {
		return false
	}
	var ok func(Expr) bool
	ok = func(x Expr) bool {
		if b, isOr := x.(BinaryExpr); isOr && whereIsPlainOr(b) {
			return ok(b.L) && ok(b.R)
		}
		return whereTermAnalyzable(x)
	}
	return ok(be)
}

// wherePlanOrAnalysed reports an OR term exprAnalyzeOrTerm runs on at all --
// "pExpr->op==TK_OR && !ExprHasProperty(pExpr, EP_Collate)" (whereexpr.c:1319)
// -- and so one whose eOperator is WO_OR.
func wherePlanOrAnalysed(e Expr) bool {
	be, ok := e.(BinaryExpr)
	return ok && equalFoldName(be.Op, "OR") && !wherePlanExprHasCollate(e)
}

// wherePlanExprHasCollate reports whether an expression tree holds an explicit
// COLLATE anywhere -- EP_Collate, which sqlite3PExpr propagates up from both
// operands (EP_Propagate, sqliteInt.h).
func wherePlanExprHasCollate(e Expr) bool {
	found := false
	var walk func(Expr)
	walk = func(x Expr) {
		if found || x == nil {
			return
		}
		if _, ok := x.(CollateExpr); ok {
			found = true
			return
		}
		wherePlanWalkChildren(x, walk)
	}
	walk(e)
	return found
}

// wherePlanOrCombines ports whereCombineDisjuncts' own guard (whereexpr.c:557):
// does a two-way OR of these disjuncts insert a further virtual conjunct
// ("x>A OR x=A" adds "x>=A") into the OUTER WHERE clause? If it does, this port
// declines the OR entirely rather than plan without that term.
//
// The operand comparison is sqlite3ExprCompare, which this port does not have;
// wherePlanExprMayCompareEqual answers conservatively (true whenever the C
// might), so the decline is the safe direction.
func wherePlanOrCombines(one, two *whereIdxTerm, ea, eb Expr) bool {
	const cmpOps = woEq | woLT | woLE | woGT | woGE
	if one.vnull || two.vnull {
		return false
	}
	if one.op&cmpOps == 0 || two.op&cmpOps == 0 {
		return false
	}
	eOp := (one.op | two.op) & cmpOps
	if eOp&(woEq|woLT|woLE) != eOp && eOp&(woEq|woGT|woGE) != eOp {
		return false
	}
	ba, aok := ea.(BinaryExpr)
	bb, bok := eb.(BinaryExpr)
	if !aok || !bok {
		return false
	}
	if ba.row != nil || bb.row != nil {
		// A row value's operands are TK_VECTORs, and a vector never compares
		// equal to a scalar (sqlite3ExprCompare, expr.c): only two row values
		// can combine, and those are left to the conservative answer.
		return ba.row != nil && bb.row != nil
	}
	return wherePlanExprMayCompareEqual(ba.L, bb.L) && wherePlanExprMayCompareEqual(ba.R, bb.R)
}

// wherePlanExprMayCompareEqual is a CONSERVATIVE sqlite3ExprCompare: true
// whenever the C might call the two expressions equal. Only the operand shapes
// a disjunct of the admitted subset can hold are distinguished; anything else
// answers true, which only declines an OR that might have been served.
func wherePlanExprMayCompareEqual(a, b Expr) bool {
	switch x := a.(type) {
	case LiteralExpr:
		return whereLiteralsMayCompareEqual(a, b)
	case ColumnExpr:
		y, ok := b.(ColumnExpr)
		if !ok {
			return false
		}
		return equalFoldName(x.Name, y.Name) && equalFoldName(x.Qualifier, y.Qualifier)
	}
	return true
}

// addOr ports whereLoopAddOr (where.c:4811) for iTab: for every OR term whose
// indexable set contains this table, price a sub-scan per disjunct, sum them
// with sqlite3LogEstAdd, and offer the union as a WHERE_MULTI_OR loop.
//
// The "+1" on rRun is the C's own TUNING note: "due to rounding errors, it may
// be that the cost of the OR-scan is equal to its most expensive sub-scan. Add
// the smallest possible penalty ... to ensure that this does not happen."
// The C's own first line -- "if( pItem->fg.jointype & JT_RIGHT ) return" -- has
// no counterpart: markWherePlanEligibility declines a RIGHT or FULL JOIN
// outright, so no item reaching here can carry JT_RIGHT.
func (b *wherePlanIdxBuild) addOr(iTab int, mPrereq whereMask) {
	maskSelf := whereMask(1) << uint(iTab)
	for ti := range b.terms {
		t := &b.terms[ti]
		if t.orInfo == nil || t.orInfo.indexable&maskSelf == 0 {
			continue
		}
		var sSum whereOrSet
		once := true
		for si := range t.orInfo.subs {
			sub := &t.orInfo.subs[si]
			and := t.orInfo.and[si]
			if and == nil && sub.cursor != iTab {
				continue
			}
			var sCur whereOrSet
			// sSubBuild = *pBuilder, with pWC replaced by the one-term tempWC and
			// pOrSet pointing at sCur. pWInfo is SHARED, which is what makes
			// whereLoopAdjustCost still read the main loop list (loops) and the
			// lookup discount still read the main clause (mainTerms); tempWC.pOuter
			// is the outer clause, which whereScanNext walks after the disjunct --
			// hence terms is the disjunct FOLLOWED BY every outer term. See
			// wherePlanIdxBuild.nBase for the third clause in play.
			sb := &wherePlanIdxBuild{
				items: b.items, terms: whereOrSubTerms(*sub, b.terms), loops: b.loops,
				sort: b.sort, noAutoIndex: b.noAutoIndex, nBase: 1, nWC: 1, mainTerms: b.terms,
				orSet: &sCur, planLimit: b.planLimit,
			}
			if and != nil {
				// "sSubBuild.pWC = &pOrTerm->u.pAndInfo->wc", whose pOuter is
				// this clause and whose nBase is its own base terms.
				sb.terms, sb.nBase, sb.nWC = whereAndSubTerms(and.terms, b.terms), and.nBase, len(and.terms)
			}
			sb.addBtree(iTab, mPrereq)
			b.partialUnknown = b.partialUnknown || sb.partialUnknown
			if sCur.n == 0 {
				sSum.n = 0
				break
			}
			if once {
				sSum = sCur
				once = false
				continue
			}
			sPrev := sSum
			sSum.n = 0
			for i := 0; i < sPrev.n; i++ {
				for j := 0; j < sCur.n; j++ {
					sSum.insert(sPrev.a[i].prereq|sCur.a[j].prereq,
						logEstAdd(sPrev.a[i].rRun, sCur.a[j].rRun),
						logEstAdd(sPrev.a[i].nOut, sCur.a[j].nOut))
				}
			}
		}
		for i := 0; i < sSum.n; i++ {
			b.insertLoop(whereIdxLoop{
				iTab: iTab, maskSelf: maskSelf,
				prereq:  sSum.a[i].prereq,
				rRun:    sSum.a[i].rRun + 1,
				nOut:    sSum.a[i].nOut,
				multiOr: true, lTerm: []int{ti},
			})
		}
	}
}

// whereAndSubTerms is a WO_AND disjunct's sub-builder term list: its own
// clause, then the outer one its pOuter reaches, every outer term's iParent
// shifted past the clause.
func whereAndSubTerms(and, outer []whereIdxTerm) []whereIdxTerm {
	out := make([]whereIdxTerm, 0, len(and)+len(outer))
	out = append(out, and...)
	for _, t := range outer {
		if t.parent >= 0 {
			t.parent += len(and)
		}
		out = append(out, t)
	}
	return out
}

func isAndExpr(e Expr) bool {
	b, ok := e.(BinaryExpr)
	return ok && equalFoldName(b.Op, "AND")
}

// whereOrSubTerms is the term list of whereLoopAddOr's sub-builder: the single
// disjunct (tempWC, whose nTerm and nBase are both 1) followed by the outer
// clause tempWC.pOuter points at, which whereScanNext reaches after it.
//
// Every copied outer term's iParent is shifted by one, since it indexed the
// outer list and now indexes this one; -1 stays -1. Nothing in the sub-build
// reads a parent (nBase is 1, so whereLoopOutputAdjust never looks past the
// disjunct), but a stale index that happened to land on 0 would read as "child
// of the disjunct", and that is not a thing to leave to luck.
func whereOrSubTerms(sub whereIdxTerm, outer []whereIdxTerm) []whereIdxTerm {
	out := make([]whereIdxTerm, 0, len(outer)+1)
	sub.parent = -1
	out = append(out, sub)
	for _, t := range outer {
		if t.parent >= 0 {
			t.parent++
		}
		out = append(out, t)
	}
	return out
}

// wherePlanMultiOrOrder builds the runtime order payload for a winning
// WHERE_MULTI_OR loop: the disjuncts in clause order, each with the key of the
// index its sub-scan walks.
//
// The sub-plan is re-derived, as in C: whereLoopAddOr keeps only costs, and
// wherecode.c:2431 runs a fresh sqlite3WhereBegin per disjunct over
// "<disjunct> AND <other WHERE terms>" with no ORDER BY and
// WHERE_OR_SUBCLAUSE, whose observable effect here is no automatic index
// (where.c:4067; noAutoIndex). ok == false declines the statement.
func wherePlanMultiOrOrder(items []whereItem, nEList int, info *whereOrInfo, reverseOrder bool) (*multiOrOrder, bool) {
	mo := &multiOrOrder{scopes: info.scopes}
	for _, d := range info.exprs {
		// The membership test, compiled once (see multiOrOrder for the C). Only
		// a single-table FROM reaches here with a scope to compile against (a
		// multi-table MULTI_OR level goes through wherePlanJoinMultiOrKey, and
		// wherePlanSingleIndexKey requires one loop), which is also all
		// selfRowExpr.runnable admits. The length test is a bounds guard: an
		// empty scopes slice falls back to a column-less scope, against which no
		// column compiles, and multiOrOrderCursor reads the nil program as "did
		// not match".
		var scope tableScope
		if len(info.scopes) == 1 {
			scope = info.scopes[0]
		}
		// Paged, so a disjunct holding a subquery ("d IN (SELECT y FROM u) OR
		// c = 5") compiles. One that still does not is refused: multiOrOrderCursor
		// would read its nil program as "matched no row" and misfile every row it
		// selects into a later group.
		prog := compileSelfRowExprPaged(scope, d, false /* an ordinary WHERE term: the db qualifier is validated, not ignored */, scope.planPager, pureCtxNone)
		if prog.prog == nil {
			return nil, false
		}
		mo.disjuncts = append(mo.disjuncts, prog)
		// pOrExpr = <disjunct> AND o1 AND o2 ... -- built left-deep so that
		// splitTopLevelAnd hands the analyser [disjunct, o1, o2, ...], which is the
		// order sqlite3WhereSplit produces for the C's own pAndExpr tree.
		sub := d
		for _, o := range info.others {
			sub = BinaryExpr{Op: "AND", L: sub, R: o}
		}
		terms, ok := wherePlanTermsFrom(info.jts, info.scopes, sub)
		if !ok {
			return nil, false
		}
		// pOrderBy is 0 for every sub-WhereBegin, so the sub-plan asks no ordering
		// question -- but sqlite3WhereBegin still calls whereReverseScanOrder on it
		// under PRAGMA reverse_unordered_selects, since that guard tests
		// pWInfo->pOrderBy alone (where.c:7126).
		k, _, ok := wherePlanSingleIndexKey(wherePlanInput{
			items: items, terms: terms,
			sort:        &whereSortCtl{nEList: nEList},
			noAutoIndex: true, orSubclause: true,
		}, reverseOrder)
		if !ok {
			return nil, false
		}
		mo.keys = append(mo.keys, k)
	}
	return mo, true
}

// wherePlanJoinMultiOrKey is wherePlanMultiOrOrder for a WHERE_MULTI_OR loop
// winning a level of a multi-table plan: order is the nesting, level the loop's
// position, ti the OR term it drives (aLTerm[0]), and nQueryLoop
// pParse->nQueryLoop when case 5 runs (the statement's plus this plan's
// nRowOut, where.c:7198).
//
// What case 5 (wherecode.c:2229-2560) does per entry into the level:
//
//   - empties the RowSet (wherecode.c:2339), once per outer row;
//   - for each pOrWc entry on this table (or WO_AND), runs a sub-WhereBegin over
//     "<entry> AND <pAndExpr>" in pOrWc order (wherecode.c:2409-2432), whose
//     FROM is this table then every unbound table (2300-2312) with nTabList 1
//     (where.c:6889) -- so outer columns are constants and inner ones unusable;
//   - pAndExpr is every other non-virtual WHERE term not coded further out,
//     with an operator and no subquery (wherecode.c:2378-2398);
//   - emits a row the first time a sub-scan finds it (OP_RowSetTest, 2450).
//
// So a row lands in the first sub-scan it satisfies against the current outer
// row -- hence a list of expressions compiled into the level
// (emitMultiOrGroups) rather than one permutation -- in that sub-plan's index
// order.
//
// Declined (ok == false):
//
//   - WITHOUT ROWID: its RowSet is a PK index (wherecode.c:2343);
//   - an entry naming an inner table or holding a subquery: the sub-scan cannot
//     test it (untestedTerms);
//   - an outer join's right-hand table, unless the OR is that join's own ON
//     term (then a pAndExpr WHERE term is deferred past the outer join,
//     tag-20220513a, wherecode.c:2631-2635, and never coded,
//     1537/2781-2807);
//   - a sub-plan that is itself a MULTI_OR union.
func wherePlanJoinMultiOrKey(in wherePlanInput, order []int, level, ti int, nQueryLoop logEst) (*autoIndexKey, bool) {
	if ti < 0 || ti >= len(in.terms) || in.terms[ti].orInfo == nil {
		return nil, false
	}
	info := in.terms[ti].orInfo
	iCur := order[level]
	it := in.items[iCur]
	if it.noRowid || len(info.jts) != len(in.items) || len(info.scopes) != len(in.items) {
		return nil, false
	}
	var ready whereMask
	for i := 0; i < level; i++ {
		ready |= whereMask(1) << uint(order[i])
	}
	self := whereMask(1) << uint(iCur)
	maskOf := func(e Expr) whereMask {
		set := map[int]bool{}
		collectTableRefs(e, info.scopes, set)
		var m whereMask
		for i := range set {
			m |= whereMask(1) << uint(i)
		}
		return m
	}

	// pOrTab: this table in a[0], then the not-ready ones in nesting order. pos
	// maps a FROM index to its pOrTab slot.
	tabs := append([]int{iCur}, order[level+1:]...)
	pos := make(map[int]int, len(tabs))
	subJts := make([]joinedTable, len(tabs))
	subScopes := make([]tableScope, len(tabs))
	for k, i := range tabs {
		pos[i] = k
		subJts[k] = info.jts[i]
		subJts[k].on = nil
		subScopes[k] = info.scopes[i]
		subScopes[k].planOuter = nil
	}
	// The tables bound further out resolve as the constants they are in the
	// sub-WhereBegin -- through the same channel an enclosing query's row does,
	// and in front of it.
	var readyScopes []tableScope
	for i := 0; i < level; i++ {
		readyScopes = append(readyScopes, info.scopes[order[i]])
	}
	subScopes[0].planOuter = &evalCtx{tables: readyScopes, outer: info.scopes[0].planOuter}
	subScopes[0].planCaseSensitiveLike = info.scopes[0].planCaseSensitiveLike
	subScopes[0].planUTF16LE = info.scopes[0].planUTF16LE
	subScopes[0].planPager = info.scopes[0].planPager
	remap := func(outerON bool, onItem int) (whereConj, bool) {
		c := whereConj{outerON: outerON, onItem: -1}
		if outerON {
			p, ok := pos[onItem]
			if !ok {
				return c, false
			}
			c.onItem = p
		}
		return c, true
	}

	if it.outer && !(info.outerON && info.onItem == iCur) {
		return nil, false
	}
	var pAnd []whereConj
	for k, j := range info.otherIdx {
		o := &in.terms[j]
		if o.virt || o.prereqAll&^ready == 0 || containsSubquery(info.others[k]) {
			continue // TERM_VIRTUAL, TERM_CODED, tag-20220303a
		}
		c, ok := remap(o.outerON, o.onItem)
		if !ok {
			return nil, false
		}
		c.e = info.others[k]
		pAnd = append(pAnd, c)
	}
	orMark, ok := remap(info.outerON, info.onItem)
	if !ok {
		return nil, false
	}

	mo := &multiOrOrder{}
	for k := range info.subs {
		if info.and[k] == nil && info.subs[k].cursor != iCur {
			continue
		}
		d := info.exprs[k]
		if containsSubquery(d) || maskOf(d)&^(ready|self) != 0 {
			return nil, false
		}
		mo.passes = append(mo.passes, d)
		if info.subs[k].vnull {
			// The "x>NULL" child of "x IS NOT NULL": its parent is a WO_AND
			// entry, earlier in pOrWc, whose sub-scan already emitted every row
			// this one can find -- so no row is ever filed under it, and the
			// index it walks does not matter.
			mo.keys = append(mo.keys, nil)
			continue
		}
		var conj []whereConj
		for _, cj := range whereSplitAnd(d) {
			c := orMark
			c.e = cj
			conj = append(conj, c)
		}
		conj = append(conj, pAnd...)
		terms, ok := wherePlanTermsFromConj(subJts, subScopes, conj, true, false)
		if !ok {
			return nil, false
		}
		nEList := 0
		if in.sort != nil {
			nEList = in.sort.nEList
		}
		// pOrderBy is 0 for every sub-WhereBegin, so the pragma reverses each
		// sub-scan whatever the outer statement orders by (where.c:7126).
		key, _, ok := wherePlanSingleIndexKey(wherePlanInput{
			items: []whereItem{it}, terms: terms,
			sort:        &whereSortCtl{nEList: nEList},
			noAutoIndex: true, orSubclause: true, nQueryLoop: nQueryLoop,
		}, in.reverseOrder)
		if !ok || key != nil && key.multiOr != nil {
			return nil, false
		}
		mo.keys = append(mo.keys, key)
	}
	if len(mo.passes) == 0 {
		return nil, false
	}
	return &autoIndexKey{multiOr: mo}, true
}
