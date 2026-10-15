package engine

import "fmt"

// A RETURNING subquery resolves against the statement's leading WITH clause:
// the parser pushes it onto pParse->pWith for the whole statement
// (sqlite3WithPush, parse.y), and codeReturningTrigger prepares the RETURNING
// list in that same Parse (trigger.c:1059), so
//
//	WITH c(x) AS (SELECT 42) UPDATE t SET v = 1 RETURNING id, (SELECT x FROM c)
//
// answers 42 per row, and a CTE shadows a table of the same name there as
// anywhere else.
//
// What the CTE's rows are is decided by how C codes that reference:
//
//   - flattened into the subquery (flattenSubquery), its body is read wherever
//     and whenever the subquery runs, which is what lowering the subquery live
//     does here;
//   - materialized, it is filled once per statement (OP_Once, select.c:8115),
//     and a reference coded after another materialization of the same CTE
//     reuses that one (addrM9e, select.c:8075). An UPDATE ... FROM's pass one
//     is coded before the row loop, so a CTE it materializes is the one every
//     RETURNING row reads: the statement-start rows, whatever a trigger writes
//     in between (update-from.sqltest's "reuses-cte-snapshot" case, 6 rather
//     than 104).
//
// returningWith carries the clause into emitReturning and picks one of those
// for each RETURNING subquery naming a CTE; a shape whose answer it cannot pin
// declines.
type returningWith struct {
	ctes []CTEDef
	// declineNames: the statement's path does not offer the clause to its
	// RETURNING subqueries (an INSERT, whose WITH this parser merges into its
	// SELECT, or a view's INSTEAD OF write), so a subquery naming one of its
	// CTEs declines rather than resolving the name elsewhere.
	declineNames bool
	// upd and updTbl are the UPDATE this RETURNING belongs to, for its FROM
	// clause; nil for a DELETE.
	upd    *updateStmt
	updTbl *tableMeta
	// mentions walks every FROM item the statement names outside its RETURNING
	// list (SET, WHERE, FROM and their subqueries).
	mentions func(match func(FromItem) bool)
	// returning is the RETURNING list, whose other subqueries count as other
	// references too.
	returning []SelectColumn
	// frozenFilter is, by folded CTE name, the WHERE terms UPDATE ... FROM's
	// pass one pushed into the materialization the RETURNING subqueries share
	// (fromMaterializes).
	frozenFilter map[string][]Expr
	// where is a DELETE's or an UPDATE's WHERE clause. A RETURNING clause is a
	// trigger, which keeps the statement off the one-pass path: the WHERE is
	// scanned whole before the first row changes (delete.c, update.c), so a
	// CTE it materializes holds the statement-start rows (whereMaterializes).
	where Expr
	// insertSel is an INSERT's SELECT, every reference outside RETURNING
	// (insertMaterializes).
	insertSel *SelectStmt
}

// returningWithRoute is what returningWith.route decided for one subquery.
type returningWithRoute int

const (
	// retWithLive: lower it live with the WITH clause in scope.
	retWithLive returningWithRoute = iota
	// retWithLiveOnce: as retWithLive, but only exact when the subquery runs
	// once for the statement; a per-row one declines.
	retWithLiveOnce
	// retWithFrozen: lower it against the statement-start image, which is the
	// rows of the materialization UPDATE ... FROM's pass one made.
	retWithFrozen
	// retWithFirstImage: lower it live, every run against the image its first
	// run saw -- when the CTE it reads was materialized.
	retWithFirstImage
)

// mentionsCTE is a FROM-item matcher for the clause's CTE names.
func (w *returningWith) mentionsCTE(it FromItem) bool {
	if it.Table == "" || it.Schema != "" || it.TableFunc {
		return false
	}
	for _, d := range w.ctes {
		if equalFoldName(d.Name, it.Table) {
			return true
		}
	}
	return false
}

// countMentions counts the FROM items match accepts in sel's whole tree.
func countMentions(sel *SelectStmt, match func(FromItem) bool) int {
	n := 0
	selectTreeMentionsTable(sel, func(it FromItem) bool {
		if match(it) {
			n++
		}
		return false
	})
	return n
}

// route decides how sel, an immediate subquery of the RETURNING list, is
// lowered. pager holds the clause in scope.
func (w *returningWith) route(pager *ReadOnlyPager, sel *SelectStmt) (returningWithRoute, error) {
	if countMentions(sel, w.mentionsCTE) == 0 {
		return retWithLive, nil
	}
	decline := func(why string) (returningWithRoute, error) {
		return 0, fmt.Errorf("%w: RETURNING subquery over the statement's WITH clause: %s", errVDBEUnsupported, why)
	}
	if w.declineNames {
		return decline("this write path does not scope the clause to RETURNING")
	}
	// The references must be sel's own top-level FROM items: one nested deeper
	// is coded in a subquery of its own, whose flattening is not followed here.
	if len(sel.CTEs) != 0 || len(sel.Compound) != 0 {
		return decline("a WITH or compound in the subquery")
	}
	var top []int
	for i, it := range sel.From {
		if w.mentionsCTE(it) {
			top = append(top, i)
		}
	}
	if countMentions(sel, w.mentionsCTE) != len(top) {
		return decline("a reference below the subquery's own FROM")
	}
	if pager.cVolatile(sel) {
		return decline("a body that is not deterministic")
	}
	// References outside sel: C's nUse counts every one (select.c:5767), and
	// a materialization coded before sel's is the one sel reads
	// (select.c:8075).
	// others counts the references outside sel that can fill the
	// materialization at another moment than sel's first run. Another
	// subquery that is a whole RETURNING column and names the CTE only in its
	// own FROM runs, unconditionally, beside sel on the first row: whichever
	// fills it, the other reads rows of that same moment, and it is followed by
	// its own route. nUse counts them all.
	others := map[string]int{}
	nUse := map[string]int{}
	count := func(it FromItem) bool {
		if w.mentionsCTE(it) {
			others[r33sFoldIdent(it.Table)]++
			nUse[r33sFoldIdent(it.Table)]++
		}
		return false
	}
	sibling := func(it FromItem) bool {
		if w.mentionsCTE(it) {
			nUse[r33sFoldIdent(it.Table)]++
		}
		return false
	}
	if w.mentions != nil {
		w.mentions(count)
	}
	for _, rc := range w.returning {
		if sq, ok := rc.Expr.(SubqueryExpr); ok && sq.Stmt != sel && retWithTopLevelOnly(w, sq.Stmt) {
			exprTreeMentionsTable(rc.Expr, sibling)
			continue
		}
		exprTreeMentionsTable(rc.Expr, count)
	}
	for _, i := range top {
		key := r33sFoldIdent(sel.From[i].Table)
		others[key]--
	}
	materialized := map[string]bool{}
	for _, i := range top {
		key := r33sFoldIdent(sel.From[i].Table)
		b, ok := pager.lookupCTE(sel.From[i].Table)
		if !ok || b.core == nil || b.recursive != nil || b.multiSelfRef || b.recRefErr != "" {
			return decline("a recursive or unresolved CTE")
		}
		// A CTE whose body names another of the clause's CTEs is materialized
		// or flattened in turn; not followed.
		if countMentions(b.core, w.mentionsCTE) != 0 {
			return decline("a CTE reading another CTE of the clause")
		}
		flat, certain := pager.cFlattens(sel, sel.From, i)
		coroutine := retWithCoroutine(pager, sel, i, b, nUse[key])
		if !certain && !coroutine {
			// Flattened or materialized, which this cannot tell; a
			// co-routine reads where the subquery runs either way.
			return decline("whether C flattens the CTE is not decided")
		}
		if flat || coroutine {
			// Read where and whenever the subquery runs, as the live
			// lowering reads it.
			continue
		}
		if !selectTreeMentionsTable(b.core, func(FromItem) bool { return true }) {
			// A body reading no table has one answer whenever it runs.
			continue
		}
		materialized[key] = true
	}
	if len(materialized) == 0 {
		return retWithLive, nil
	}
	shared := 0
	for name := range materialized {
		n := others[name]
		if n == 1 {
			// The one other reference is pass one's: shared if C
			// materializes it there, else read by sel's own first run.
			if mat, ok := w.fromMaterializes(pager, name); ok {
				if mat {
					shared++
				}
				continue
			}
		}
		if n >= 1 {
			if mat, ok := w.whereMaterializes(pager, name, n); ok {
				if mat {
					shared++
				}
				continue
			}
			if mat, ok := w.insertMaterializes(pager, name, n); ok {
				if mat {
					shared++
				}
				continue
			}
		}
		switch {
		case n == 0:
		default:
			return decline("a materialized CTE referenced elsewhere in the statement")
		}
	}
	// Whether sel reads nothing but materialized CTEs: then one image of the
	// database serves all of it, where otherwise its other sources are read
	// live in C.
	onlyMaterialized := countMentions(sel, func(it FromItem) bool { return !w.mentionsCTE(it) }) == 0
	for _, i := range top {
		if !materialized[r33sFoldIdent(sel.From[i].Table)] {
			onlyMaterialized = false
		}
	}
	if shared == 0 {
		// Filled by sel's own first run and reused after it.
		if onlyMaterialized {
			return retWithFirstImage, nil
		}
		return retWithLiveOnce, nil
	}
	// The pass-one rows.
	if shared != len(materialized) || !onlyMaterialized {
		return decline("a CTE shared with pass one beside another source")
	}
	return retWithFrozen, nil
}

// whereMaterializes reports, when all n references to CTE name outside the
// RETURNING subquery are the lone FROM item of subqueries of the WHERE clause
// (ok), whether one of them fills the materialization the RETURNING subquery
// then reuses (select.c:8075, addrM9e). The WHERE is coded ahead of the
// RETURNING code (a RETURNING keeps the statement off the one-pass path, so
// the WHERE is scanned whole before the first change), and an IN's right-hand
// side is coded there, in place, materializing the CTE before any row changes,
// correlated or not and whether or not every row reaches it. A scalar subquery
// or an EXISTS is not: uncorrelated, it is a constant factored out to the end
// of the program; correlated, it was coded while the RETURNING reference was
// not yet counted (nUse 1), so its CTE is a co-routine -- unless AS
// MATERIALIZED rules that out, and it is materialized in place. Otherwise
// neither leaves a materialization behind, and the RETURNING subquery fills
// its own on its first run.
func (w *returningWith) whereMaterializes(pager *ReadOnlyPager, name string, n int) (mat, ok bool) {
	if w.where == nil {
		return false, false
	}
	b, found := pager.lookupCTE(name)
	if !found || b.core == nil || b.m10d == cteM10dNo {
		return false, false
	}
	cteCols, cok := cteOutputNames(b)
	if !cok {
		return false, false
	}
	match := func(it FromItem) bool { return w.mentionsCTE(it) && equalFoldName(it.Table, name) }
	refs, nMat := 0, 0
	bad := false
	visit := func(e Expr) {
		var s *SelectStmt
		in := false
		switch x := e.(type) {
		case InExpr:
			s, in = x.Sub, true
		case ExistsExpr:
			s = x.Stmt
		case SubqueryExpr:
			s = x.Stmt
		}
		if s == nil || countMentions(s, match) == 0 {
			return
		}
		if len(s.CTEs) != 0 || len(s.Compound) != 0 || len(s.From) != 1 || !match(s.From[0]) || countMentions(s, match) != 1 {
			bad = true
			return
		}
		refs++
		flat, certain := pager.cFlattens(s, s.From, 0)
		if !certain {
			bad = true
			return
		}
		if flat {
			return
		}
		if in {
			nMat++
			return
		}
		scope := s.From[0].Alias
		if scope == "" {
			scope = s.From[0].Table
		}
		correlated := !r41WalkStmt(s, func(ce ColumnExpr) bool {
			return ce.Schema == "" && (ce.Qualifier == "" || equalFoldName(ce.Qualifier, scope)) && cteCols[r33sFoldIdent(ce.Name)]
		}, func(string) bool { return false })
		switch {
		case !correlated && b.m10d == cteM10dYes:
			bad = true // a constant whose materialization is not followed
		case correlated && b.m10d == cteM10dYes:
			nMat++ // no co-routine for it: materialized, in place
		}
	}
	if !r41WalkQ(w.where, visit, func(*SelectStmt) bool { return true }) || bad || refs != n {
		return false, false
	}
	return nMat > 0, true
}

// retWithTopLevelOnly reports whether sel names the clause's CTEs only as
// items of its own FROM.
func retWithTopLevelOnly(w *returningWith, sel *SelectStmt) bool {
	if sel == nil || len(sel.CTEs) != 0 || len(sel.Compound) != 0 {
		return false
	}
	top := 0
	for _, it := range sel.From {
		if w.mentionsCTE(it) {
			top++
		}
	}
	return countMentions(sel, w.mentionsCTE) == top
}

// retWithCoroutine is fromClauseTermCanBeCoroutine (select.c:7266) for item
// i of sel, a reference to b that C's nUse puts at nUse: a co-routine runs the
// body every time the subquery does. A placement this does not decide is
// reported as not one, which only sends it down the materialized route.
func retWithCoroutine(pager *ReadOnlyPager, sel *SelectStmt, i int, b *cteBinding, nUse int) bool {
	if b.m10d == cteM10dYes || (nUse >= 2 && b.m10d != cteM10dNo) { // (2a), (2b)
		return false
	}
	for _, it := range sel.From {
		if it.Join == JoinRight || it.Join == JoinFull { // (3)
			return false
		}
	}
	if i == 0 { // (1a), (1b)
		return true
	}
	// (1c) and (5), on the FROM list C codes after flattening the others.
	return pager.cteRefPlacementIn(sel, sel.From, i, b) == cteRefCoroutine
}

// fromMaterializes reports, when the UPDATE's FROM clause is exactly one
// item naming CTE name (ok), whether C materializes it in pass one rather
// than flattening it (never a co-routine there, select.c:7287 (1c-iii)).
func (w *returningWith) fromMaterializes(pager *ReadOnlyPager, name string) (mat, ok bool) {
	if w.upd == nil || len(w.upd.from) != 1 {
		return false, false
	}
	it := w.upd.from[0]
	if !w.mentionsCTE(it) || !equalFoldName(it.Table, name) || equalFoldName(name, w.updTbl.name) {
		return false, false
	}
	sel, _, err := buildUpdateFromSelect(w.upd, w.updTbl, nil)
	if err != nil {
		return false, false
	}
	// Pass one pushes a WHERE term over the CTE's columns alone down into its
	// materialization (pushDownWhereTerms, select.c:8016, while nUse is still
	// 1), and RETURNING then reads those filtered rows. Only a WHERE whose
	// every term names the target, which cannot be pushed, is followed --
	// or any WHERE over an AS MATERIALIZED CTE, which nothing is pushed into.
	b, found := pager.lookupCTE(name)
	if !found || b.core == nil {
		return false, false
	}
	if b.m10d == cteM10dYes {
		for i, fi := range sel.From {
			if !fi.UpdateTarget {
				flat, certain := pager.cFlattens(sel, sel.From, i)
				return !flat, certain
			}
		}
		return false, false
	}
	cteCols, cok := cteOutputNames(b)
	if !cok {
		return false, false
	}
	target := writeScopeName(w.upd.alias, w.updTbl)
	scope := it.Alias
	if scope == "" {
		scope = it.Table
	}
	var pushed []Expr
	for _, term := range splitTopLevelAnd(w.upd.where) {
		names := false
		forEachColumnRef(term, func(ce ColumnExpr) {
			switch {
			case ce.Qualifier != "":
				names = names || equalFoldName(ce.Qualifier, target)
			case !cteCols[r33sFoldIdent(ce.Name)]:
				for _, col := range w.updTbl.cols {
					if equalFoldName(col.Name, ce.Name) {
						names = true
					}
				}
			}
		})
		if names {
			continue
		}
		// pushDownWhereTerms copies a term over the CTE's columns alone into
		// the materialization (its HAVING for an aggregate body), and the
		// RETURNING subquery reads the filtered rows: frozenFilter.
		t, ok := retWithPushable(term, scope, cteCols)
		if !ok {
			return false, false
		}
		pushed = append(pushed, t)
	}
	for i, fi := range sel.From {
		if fi.UpdateTarget {
			continue
		}
		flat, certain := pager.cFlattens(sel, sel.From, i)
		if !flat && certain && len(pushed) != 0 {
			if w.frozenFilter == nil {
				w.frozenFilter = map[string][]Expr{}
			}
			w.frozenFilter[r33sFoldIdent(name)] = pushed
		}
		return !flat, certain
	}
	return false, false
}

// retWithPushedAlias is the scope name a pushed-down term's columns are
// qualified with inside frozenFilter's wrapper.
const retWithPushedAlias = "\x00pushed"

// retWithPushable is term, a WHERE term of UPDATE ... FROM naming no column of
// the target, as pushDownWhereTerms copies it into the CTE that FROM item
// scope names (sqlite3ExprIsSingleTableConstraint): every column one of the
// CTE's, requalified onto retWithPushedAlias. Only a term with no subquery and
// no function call is followed. Pushed into the body (its HAVING, for an
// aggregate one) it keeps exactly the rows whose output columns satisfy it,
// which is what filtering the CTE's result by it keeps (filterFrozen):
// substExpr gives each copy its column's affinity and collating sequence.
func retWithPushable(term Expr, scope string, cteCols map[string]bool) (Expr, bool) {
	plain := true
	if !r41Walk(term, func(e Expr) {
		switch e.(type) {
		case FuncExpr, LikeExpr, GlobExpr, MatchExpr:
			plain = false
		}
	}) || !plain {
		return nil, false
	}
	return r41Rewrite(term, func(ce ColumnExpr) (Expr, bool) {
		if ce.Schema != "" || (ce.Qualifier != "" && !equalFoldName(ce.Qualifier, scope)) ||
			!cteCols[r33sFoldIdent(ce.Name)] {
			return nil, false
		}
		return ColumnExpr{Qualifier: retWithPushedAlias, Name: ce.Name}, true
	})
}

// filterFrozen is sel with each FROM item naming a CTE that pass one pushed
// terms into (frozenFilter) reading those filtered rows instead.
func (w *returningWith) filterFrozen(sel *SelectStmt) *SelectStmt {
	if len(w.frozenFilter) == 0 {
		return sel
	}
	out := *sel
	out.From = append([]FromItem(nil), sel.From...)
	for k, it := range out.From {
		terms, ok := w.frozenFilter[r33sFoldIdent(it.Table)]
		if !ok || !w.mentionsCTE(it) {
			continue
		}
		alias := it.Alias
		if alias == "" {
			alias = it.Table
		}
		out.From[k] = FromItem{
			Subquery: &SelectStmt{Columns: []SelectColumn{{Star: true}},
				From:  []FromItem{{Table: it.Table, Alias: retWithPushedAlias}},
				Where: r41AndOnto(nil, terms)},
			Alias: alias, Join: it.Join, On: it.On, CrossKeyword: it.CrossKeyword,
		}
	}
	return &out
}

// returningWithSubSelect lowers sel, an immediate RETURNING subquery, under
// the statement's WITH clause (see returningWith). done is false when the
// clause has nothing to say about sel.
func (c *compiler) returningWithSubSelect(sel *SelectStmt) (prog *Program, done bool, err error) {
	w := c.retWith
	route, rerr := w.route(c.pager, sel)
	if rerr != nil {
		return nil, true, rerr
	}
	switch route {
	case retWithFrozen:
		prog, err = compileSubProgram(c.pager, w.filterFrozen(sel), trigOnlyOuter(c.trig, c.retRow))
		if err != nil {
			return nil, true, err
		}
		if prog.Correlated {
			return nil, true, fmt.Errorf("%w: correlated RETURNING sub-program", errVDBEUnsupported)
		}
		return prog, true, nil
	case retWithFirstImage:
		prog, err = c.liveDB.compileLiveSubProgram(sel, c.trig, c.retRow, c.nQueryLoop, c.nQueryLoopKnown)
		if err != nil {
			return nil, true, err
		}
		prog.LiveFirstImage = true
		return prog, true, nil
	case retWithLiveOnce:
		prog, err = c.liveDB.compileLiveSubProgram(sel, c.trig, c.retRow, c.nQueryLoop, c.nQueryLoopKnown)
		if err != nil {
			return nil, true, err
		}
		if n, _, _ := returningSubqueryLifetimes([]Instruction{{P4: prog}}, c.retRow.tableName); n != 0 {
			return nil, true, fmt.Errorf("%w: RETURNING subquery over the statement's WITH clause: a materialized CTE in a subquery run per row", errVDBEUnsupported)
		}
		return prog, true, nil
	}
	return nil, false, nil
}

// declineReturningWith is the returningWith of a path that does not scope the
// clause to RETURNING: a subquery naming one of its CTEs declines. nil
// without a clause.
func declineReturningWith(ctes []CTEDef) *returningWith {
	if len(ctes) == 0 {
		return nil
	}
	return &returningWith{ctes: ctes, declineNames: true}
}

// insertStmtCTEs is an INSERT's WITH clause: its own for VALUES, else the
// SELECT's (a leading one this parser moved there, or the SELECT's own).
func insertStmtCTEs(stmt *insertStmt) []CTEDef {
	if stmt.selectStmt != nil {
		return stmt.selectStmt.CTEs
	}
	return stmt.ctes
}

// insertReturningWith is the returningWith of an INSERT: the statement's own
// leading WITH clause (insertStmt.ctes, or leadingWith), with every other
// place the statement names its CTEs -- the VALUES tuples or the SELECT --
// counted as another reference. A SELECT's own WITH is not RETURNING's, and an
// upsert is not followed: those decline.
func insertReturningWith(stmt *insertStmt) *returningWith {
	ctes := insertStmtCTEs(stmt)
	if len(ctes) == 0 || stmt.returning == nil {
		return nil
	}
	if stmt.upsert != nil || (stmt.selectStmt != nil && !stmt.leadingWith) {
		return declineReturningWith(ctes)
	}
	rest := stmt.selectStmt
	if rest == nil {
		rest = &SelectStmt{}
		for _, row := range stmt.rows {
			for _, e := range row {
				rest.Columns = append(rest.Columns, SelectColumn{Expr: e})
			}
		}
	}
	mentions := func(match func(FromItem) bool) { selectTreeMentionsTable(rest, match) }
	if len(stmt.ownCTEs) != 0 {
		// The SELECT's own clause shadows a leading CTE of the same name
		// everywhere in the SELECT (wrapLeadingWith): those references are
		// not the leading CTE's.
		own := map[string]bool{}
		for _, d := range stmt.ownCTEs {
			own[r33sFoldIdent(d.Name)] = true
		}
		mentions = func(match func(FromItem) bool) {
			for _, d := range rest.CTEs {
				selectTreeMentionsTable(d.Select, match)
			}
			for _, it := range rest.From {
				selectTreeMentionsTable(it.Subquery, func(it FromItem) bool {
					return !own[r33sFoldIdent(it.Table)] && match(it)
				})
			}
		}
	}
	return &returningWith{ctes: ctes, returning: stmt.returning, mentions: mentions, insertSel: stmt.selectStmt}
}

// cteOutputNames is the set of b's result column names (folded); ok is false
// for a "*" or an unnamed expression, whose name is not modelled.
func cteOutputNames(b *cteBinding) (map[string]bool, bool) {
	out := map[string]bool{}
	if b.colNames != nil {
		for _, n := range b.colNames {
			out[r33sFoldIdent(n)] = true
		}
		return out, true
	}
	for _, c := range b.core.Columns {
		switch {
		case c.Star:
			return nil, false
		case c.HasAlias:
			out[r33sFoldIdent(c.Alias)] = true
		default:
			ce, ok := c.Expr.(ColumnExpr)
			if !ok {
				return nil, false
			}
			out[r33sFoldIdent(ce.Name)] = true
		}
	}
	return out, true
}

// insertMaterializes is whereMaterializes for an INSERT's SELECT, which holds
// all n references to CTE name outside the RETURNING subquery and is coded
// ahead of it: its rows are read before the first one is inserted. Referenced
// there once, from a FROM clause of that one item, the CTE is a co-routine
// (fromClauseTermCanBeCoroutine (1a); the RETURNING reference is counted only
// later) and leaves no materialization behind. Referenced more than once, or
// AS MATERIALIZED, a body no flattening can take is materialized there, before
// any row is inserted, and the RETURNING subquery reuses it.
func (w *returningWith) insertMaterializes(pager *ReadOnlyPager, name string, n int) (mat, ok bool) {
	if w.insertSel == nil {
		return false, false
	}
	b, found := pager.lookupCTE(name)
	if !found || b.core == nil || b.m10d == cteM10dNo {
		return false, false
	}
	match := func(it FromItem) bool { return w.mentionsCTE(it) && equalFoldName(it.Table, name) }
	refs, bad := 0, false
	var at *SelectStmt
	atItem := 0
	nfWalkSelects(w.insertSel, func(s *SelectStmt) {
		for _, d := range s.CTEs {
			if s != w.insertSel && equalFoldName(d.Name, name) {
				bad = true
			}
		}
		for i, it := range s.From {
			if match(it) {
				refs++
				at, atItem = s, i
			}
		}
	})
	if bad || refs != n {
		return false, false
	}
	if n == 1 && b.m10d != cteM10dYes {
		if retWithCoroutine(pager, at, atItem, b, 1) {
			return false, true
		}
		flat, certain := pager.cFlattens(at, at.From, atItem)
		if !certain {
			return false, false
		}
		if flat {
			return false, true
		}
		return true, true
	}
	core := b.core
	if len(core.Compound) == 0 && (cSelectIsAggregate(core) || core.Distinct || core.Limit != nil || core.LimitParam != nil) {
		return true, true
	}
	return false, false
}
