package engine

import (
	"fmt"
	"strings"
)

// Exists-to-join conversion in the planner.
//
// existsToJoin (select.c:7326) rewrites a top-level "EXISTS (SELECT ... FROM u
// WHERE w)" of a SELECT's WHERE into a FROM item: u joins the outer FROM with
// fg.fromExists set, the EXISTS becomes the constant 1 IN PLACE, and w is ANDed
// onto the end of the WHERE. where.c then prices u's loops at nOut 0, never
// skip-scans it, keeps it right of every table its terms name (where.c:3580,
// 3631, 4182, 4286, 4992), and codes its loop to break after the first match
// (where.c:7587).
//
// That last part is what lets execution stay as it is. u contributes no output
// column and at most one row per combination of the other loops, so the rows
// come out in the order the OTHER tables' loops produce them, wherever u sits
// among them; evaluating the EXISTS as the filter it always was yields the same
// row set. What the rewrite changes is the PLAN: w's terms price the other
// tables' loops -- "EXISTS (SELECT 1 FROM u WHERE t.c > 3)" hands t an index
// range -- and u is one more item in the join order. So the planner plans the
// rewritten statement and the gate keeps the verdict for the original tables.
//
// Only SELECT runs existsToJoin; an UPDATE or DELETE plans its EXISTS as an
// ordinary term, which is the one-pass planner's own compile (c.onePassPlan).

// existsScopePrefix names the rewritten FROM items. It cannot be spelled in
// SQL, so it collides with no alias the statement wrote.
const existsScopePrefix = "\x00exists"

// wherePlanExistsJoin is the rewritten statement: srcs and stmt carry the
// original items first and one appended item per converted EXISTS.
type wherePlanExistsJoin struct {
	srcs  []joinSource
	stmt  *SelectStmt
	nOrig int
	// base is the WHERE with each converted EXISTS made 1, and subs the
	// subqueries' WHEREs, which wherePlanExistsOrder puts after every ON
	// clause the way C does: sqlite3ProcessJoin folded those in first.
	base Expr
	subs []Expr
}

// wherePlanExistsToJoin reports applies == false when existsToJoin would
// convert nothing here -- the statement plans as it is. ok == false means it
// would, and this port cannot follow: the caller declines.
func wherePlanExistsToJoin(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) (ej *wherePlanExistsJoin, applies, ok bool) {
	if p == nil || c == nil || c.onePassPlan || stmt == nil || stmt.Where == nil ||
		// "(p->pLimit==0 || p->pLimit->pRight==0)": no OFFSET.
		stmt.Offset != nil || stmt.OffsetParam != nil {
		return nil, false, true
	}
	var exists []*SelectStmt
	var walk func(Expr) Expr
	walk = func(e Expr) Expr {
		switch x := e.(type) {
		case BinaryExpr:
			if equalFoldName(x.Op, "AND") {
				x.L = walk(x.L)
				x.R = walk(x.R)
				return x
			}
		case ExistsExpr:
			if !x.Not && existsToJoinCandidate(x.Stmt) {
				exists = append(exists, x.Stmt)
				return LiteralExpr{Val: Value{Typ: Int, I: 1}}
			}
		}
		return e
	}
	where := walk(stmt.Where)
	if len(exists) == 0 {
		return nil, false, true
	}
	if len(srcs)+len(exists) >= 64 { // "p->pSrc->nSrc<BMS"
		return nil, true, false
	}
	stmt2 := *stmt
	stmt2.From = append([]FromItem(nil), stmt.From...)
	ej = &wherePlanExistsJoin{srcs: append([]joinSource(nil), srcs...), stmt: &stmt2, nOrig: len(srcs)}
	for k, sub := range exists {
		name := fmt.Sprintf("%s%d", existsScopePrefix, k)
		src, fi, subWhere, sok := existsJoinSource(p, sub, name)
		if !sok {
			return nil, true, false
		}
		ej.srcs = append(ej.srcs, src)
		stmt2.From = append(stmt2.From, fi)
		if subWhere != nil {
			ej.subs = append(ej.subs, subWhere)
		}
	}
	ej.base = where
	return ej, true, true
}

// existsToJoinCandidate is existsToJoin's test of the subquery itself: one FROM
// item that is not a subquery, no aggregate, no LIMIT, no compound.
func existsToJoinCandidate(sub *SelectStmt) bool {
	return sub != nil && len(sub.From) == 1 && sub.From[0].Subquery == nil && !sub.From[0].TableFunc &&
		len(sub.Compound) == 0 && sub.Limit == nil && sub.LimitParam == nil &&
		!selectIsAggregateQuery(sub) && sub.Having == nil
}

// existsJoinSource builds the FROM item for sub's one table under name, and
// sub's WHERE with every reference to that table rebound to name. ok false for
// what this port cannot follow: a view or CTE -- a subquery to C, so C does not
// convert it either, but then it would need planning as an opaque term, which
// wherePlanTermsFrom refuses -- a virtual or foreign table, a schema-qualified
// column, and a nested subquery, whose own names would need the same rebinding.
func existsJoinSource(p *ReadOnlyPager, sub *SelectStmt, name string) (joinSource, FromItem, Expr, bool) {
	it := sub.From[0]
	if len(sub.CTEs) > 0 {
		return joinSource{}, FromItem{}, nil, false
	}
	if it.Schema != "" {
		if local, err := p.qualifierResolvesLocally(it.Schema); err != nil || !local {
			return joinSource{}, FromItem{}, nil, false
		}
	} else if _, isCTE := p.lookupCTE(it.Table); isCTE {
		return joinSource{}, FromItem{}, nil, false
	}
	scope := fromItemScope(it)
	rows, err := p.Schema()
	if err != nil {
		return joinSource{}, FromItem{}, nil, false
	}
	plain := false
	for i := range rows {
		if scope.accepts(rows[i].Temp) && rows[i].Type == "table" && equalFoldName(rows[i].Name, it.Table) {
			// ponytail: a TEMP table would need its own catalog's statistics
			// (joinSource.dbIdx); declined until one shows up.
			plain = !rows[i].Temp && !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(rows[i].SQL)), "CREATE VIRTUAL")
		}
	}
	if !plain {
		return joinSource{}, FromItem{}, nil, false
	}
	tbl, terr := p.resolveTableIn(scope, it.Table)
	if terr != nil || tbl == nil {
		return joinSource{}, FromItem{}, nil, false
	}
	ts := tableScope{name: name, tableName: it.Table, cols: tbl.cols, colIndex: buildColIndex(tbl.cols), fromExists: true}
	own := it.Table
	if it.Alias != "" {
		own = it.Alias
	}
	rebind := func(ce ColumnExpr) (ColumnExpr, bool) {
		if ce.Schema != "" {
			return ce, false
		}
		lname := r33sFoldIdent(ce.Name)
		_, isCol := ts.colIndex[lname]
		switch {
		case ce.Qualifier != "":
			if equalFoldName(ce.Qualifier, own) {
				ce.Qualifier = name
			}
		case isCol || isRowidName(lname) && !tbl.withoutRowid:
			ce.Qualifier = name
		}
		return ce, true
	}
	where, wok := existsRebind(sub.Where, rebind)
	if !wok {
		return joinSource{}, FromItem{}, nil, false
	}
	// colUsed: C resolved the subquery's result list and ORDER BY against u
	// before the rewrite, crediting u's colUsed for them too.
	var used uint64
	mark := func(e Expr) bool {
		re, rok := existsRebind(e, rebind)
		if !rok {
			return false
		}
		return wherePlanWalkColumns(re, func(ce ColumnExpr) bool {
			if !equalFoldName(ce.Qualifier, name) {
				return true // an outer name: credited where the outer walk finds it
			}
			if ci, hit := ts.colIndex[r33sFoldIdent(ce.Name)]; hit {
				used |= colUsedBitsFor(tbl.cols, ci)
			}
			return true
		})
	}
	for _, sc := range sub.Columns {
		if sc.Star {
			for ci := range tbl.cols {
				if !tbl.cols[ci].IsRowidAlias && !tbl.cols[ci].Hidden {
					used |= colUsedBitsFor(tbl.cols, ci)
				}
			}
			continue
		}
		if !mark(sc.Expr) {
			return joinSource{}, FromItem{}, nil, false
		}
	}
	for _, ot := range sub.OrderBy {
		if !mark(ot.Expr) {
			return joinSource{}, FromItem{}, nil, false
		}
	}
	ts.existsColUsed = used
	fi := FromItem{Table: it.Table, Schema: it.Schema, Alias: name, IndexedBy: it.IndexedBy, NotIndexed: it.NotIndexed}
	return joinSource{tbl: tbl, scope: compileScope{tableScope: ts}, notIndexed: it.NotIndexed}, fi, where, true
}

// existsRebind rewrites every column reference in e through f, refusing a node
// it does not walk -- a nested subquery above all, whose names would resolve
// against its own FROM first.
func existsRebind(e Expr, f func(ColumnExpr) (ColumnExpr, bool)) (Expr, bool) {
	all := func(es []Expr) ([]Expr, bool) {
		out := make([]Expr, len(es))
		for i, a := range es {
			r, ok := existsRebind(a, f)
			if !ok {
				return nil, false
			}
			out[i] = r
		}
		return out, true
	}
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return e, true
	case ColumnExpr:
		return f(x)
	case UnaryExpr:
		r, ok := existsRebind(x.X, f)
		x.X = r
		return x, ok
	case BinaryExpr:
		l, lok := existsRebind(x.L, f)
		r, rok := existsRebind(x.R, f)
		x.L, x.R = l, r
		return x, lok && rok
	case IsNullExpr:
		r, ok := existsRebind(x.X, f)
		x.X = r
		return x, ok
	case CollateExpr:
		r, ok := existsRebind(x.X, f)
		x.X = r
		return x, ok
	case CastExpr:
		r, ok := existsRebind(x.X, f)
		x.X = r
		return x, ok
	case BetweenExpr:
		v, vok := existsRebind(x.X, f)
		lo, lok := existsRebind(x.Lo, f)
		hi, hok := existsRebind(x.Hi, f)
		x.X, x.Lo, x.Hi = v, lo, hi
		return x, vok && lok && hok
	case LikeExpr:
		v, vok := existsRebind(x.X, f)
		pat, pok := existsRebind(x.Pattern, f)
		esc, eok := existsRebind(x.Escape, f)
		x.X, x.Pattern, x.Escape = v, pat, esc
		return x, vok && pok && eok
	case GlobExpr:
		v, vok := existsRebind(x.X, f)
		pat, pok := existsRebind(x.Pattern, f)
		x.X, x.Pattern = v, pat
		return x, vok && pok
	case InExpr:
		if x.Sub != nil {
			return e, false
		}
		v, vok := existsRebind(x.X, f)
		list, lok := all(x.List)
		x.X, x.List = v, list
		return x, vok && lok
	case CaseExpr:
		base, bok := existsRebind(x.Base, f)
		els, eok := existsRebind(x.Else, f)
		whens := make([]WhenClause, len(x.Whens))
		ok := bok && eok
		for i, w := range x.Whens {
			wh, wok := existsRebind(w.When, f)
			th, tok := existsRebind(w.Then, f)
			whens[i] = WhenClause{When: wh, Then: th}
			ok = ok && wok && tok
		}
		x.Base, x.Else, x.Whens = base, els, whens
		return x, ok
	case FuncExpr:
		if x.Over != nil || x.Filter != nil || x.Star {
			return e, x.Star && x.Over == nil && x.Filter == nil
		}
		args, ok := all(x.Args)
		x.Args = args
		return x, ok
	}
	return e, false
}

// wherePlanExistsOrder plans ej and answers, per ORIGINAL source, the key its
// rows arrive in and its nesting level among the original sources alone -- the
// rewritten items dropped, as the rows they add are not observable (see the
// file comment). It answers what wherePlanMultiTableOrder does for the rest.
func wherePlanExistsOrder(p *ReadOnlyPager, c *compiler, stmt *SelectStmt, ej *wherePlanExistsJoin) (keys []*autoIndexKey, levels []int, nRow logEst, ok bool) {
	// OUTER JOIN STRENGTH REDUCTION (select.c:7740) runs BEFORE existsToJoin
	// (:7912), so it sees the EXISTS, not the terms that replace it: reduce
	// against the original WHERE here, and refuse an outer join left over, which
	// the rewritten WHERE could otherwise reduce.
	orig := ej.srcs[:ej.nOrig]
	jts := make([]joinedTable, len(orig))
	for i := range orig {
		jts[i] = joinedTable{tbl: orig[i].tbl}
	}
	reduced := wherePlanOuterJoinsReduced(jts, tableScopesOf(orig), orig, stmt.Where)
	where := ej.base
	for i := range orig {
		if orig[i].rightOuter || orig[i].left && !reduced[i] {
			return nil, nil, 0, false
		}
		ej.srcs[i].left = false
		// Every ON is now an inner join's: already in C's WHERE, ahead of
		// what existsToJoin appends.
		if ej.srcs[i].on != nil {
			where = whereAndOf(where, ej.srcs[i].on)
			ej.srcs[i].on = nil
		}
	}
	for _, sw := range ej.subs {
		// "p->pWhere = sqlite3PExpr(pParse, TK_AND, p->pWhere, pSubWhere)"
		where = whereAndOf(where, sw)
	}
	ej.stmt.Where = where
	if !wherePlanMultiTableSources(p, c, ej.srcs, ej.stmt) {
		return nil, nil, 0, false
	}
	order, all, nRow, ok := wherePlanMultiTableOrder(p, c, ej.srcs, ej.stmt)
	if !ok {
		return nil, nil, 0, false
	}
	for _, k := range all {
		if k != nil && k.multiOr != nil {
			// A WHERE_MULTI_OR level's passes are compiled against the
			// rewritten FROM clause, which this path collapses back to the
			// original items before the codegen sees it.
			return nil, nil, 0, false
		}
	}
	keys, levels = make([]*autoIndexKey, ej.nOrig), make([]int, ej.nOrig)
	next := 0
	for level, iTab := range order {
		if iTab < ej.nOrig {
			keys[iTab], levels[iTab] = all[level], next
			next++
		}
	}
	return keys, levels, nRow, true
}
