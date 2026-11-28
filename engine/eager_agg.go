package engine

import (
	"fmt"
	"math"
	"math/bits"
	"strings"
)

// eagerAggregate pre-aggregates the fact sides of a fanning-out join (#400):
//
//	SELECT d.g, count(*), max(b.x) FROM a JOIN d USING (k) JOIN b USING (k) GROUP BY d.g
//
// becomes
//
//	SELECT d.g, sum(a.n * b.n), max(b.m)
//	FROM (SELECT k, count(*) AS n FROM a GROUP BY k) a
//	JOIN d USING (k)
//	JOIN (SELECT k, count(*) AS n, max(x) AS m FROM b GROUP BY k) b USING (k)
//	GROUP BY d.g
//
// Every row of the rewritten join stands for the n_a * n_b rows the original
// join makes from the same d row and key, so count(*) is the sum of those
// products, count(x) the sum of x's own per-key count times the other sides'
// counts, and min/max the min/max of the per-key min/max. C walks the nested
// loop instead, whose work is the size of the join.
//
// Only aggregates that regrouping cannot change are rebuilt: count(*),
// count(col), min(col), max(col). sum, avg and total are not: C adds REALs in
// arrival order with Kahan-Babuska-Neumaier compensation (func.c sumStep), so a
// different grouping changes the last digits, and an integer sum's "integer
// overflow" depends on the order of the running total.
//
// A pre-aggregated table ("fact") is an ordinary table reached only through
// one key column, compared by bare-column equality, and otherwise read only
// inside those aggregates and in WHERE/ON terms of its own, which move into its
// subquery. GROUP BY must name the kept tables' columns, and every other
// column the result, HAVING or ORDER BY reads must be one of them, so the
// rewritten join produces the same groups from the same kept rows.
//
// The per-key rows stand for their group's rows only if every row of a group
// compares to the other side as the group's first row does: the key's
// collation must be BINARY (one group holds only equal values), and a key with
// no affinity must not meet a TEXT one, which would turn 1 and 1.0, one group,
// into '1' and '1.0'. A derived table's column keeps its column's affinity, so
// every other comparison converts the group's values alike.
//
// The rewrite fires only where it pays (choose): at least two facts each with
// at least eagerAggMinFanout rows per key, and a join as written larger than
// the walk the rewritten one makes. A filter on a kept table with an index the
// facts could be seeked through is a selective star join, whose nested loop
// reads only what matches, and is left alone.
//
// The rewritten join reaches the aggregates in an order of its own, so a
// min/max that meets two compare-equal values with different bytes (texts
// under NOCASE, 1 and 1.0) declines, as one does over a join whose order is not
// provably C's (orderNotC).
//
// Like countCrossJoin, only for a statement with no enclosing query (a
// top-level one, or a subquery body compileSubProgram reaches without an outer
// scope), and the statement as written is compiled first so a prepare-time
// error stays its own.
func (p *ReadOnlyPager) eagerAggregate(stmt *SelectStmt) *SelectStmt {
	if p == nil || stmt == nil || p.eagerAggProbing || len(stmt.From) < 3 || len(stmt.GroupBy) == 0 {
		return nil
	}
	if stmt.Distinct || len(stmt.Compound) != 0 || stmt.LimitParam != nil || stmt.OffsetParam != nil ||
		len(stmt.Windows) != 0 || len(stmt.CTEs) != 0 || stmt.ValuesArms != 0 ||
		stmt.FromNestedParenJoin || stmt.FromParenthesized || p.fullColumnNames || p.shortColumnNamesOff {
		return nil
	}
	ea := &eaState{p: p}
	for i, it := range stmt.From {
		t, ok := p.eaSource(it, i)
		if !ok {
			return nil
		}
		for _, prev := range ea.tabs {
			if strings.EqualFold(prev.name, t.name) {
				return nil
			}
		}
		ea.tabs = append(ea.tabs, t)
	}
	if !ea.classifyTerms(stmt) {
		return nil
	}
	group := map[[2]int]bool{}
	for _, g := range stmt.GroupBy {
		c, ok := g.(ColumnExpr)
		if !ok {
			return nil
		}
		t, col, ok := ea.resolve(c)
		if !ok {
			return nil
		}
		ea.tabs[t].pinned = true
		group[[2]int{t, col}] = true
	}
	ea.group = group
	for _, c := range stmt.Columns {
		if c.Star {
			return nil
		}
		if _, ok := ea.mapOuter(c.Expr); !ok {
			return nil
		}
	}
	if stmt.Having != nil {
		ea.having = stmt
		_, ok := ea.mapOuter(stmt.Having)
		ea.having = nil
		if !ok {
			return nil
		}
	}
	for _, ot := range stmt.OrderBy {
		if ea.namesAlias(stmt, ot.Expr) {
			continue
		}
		if _, ok := ea.mapOuter(ot.Expr); !ok {
			return nil
		}
	}
	if !ea.choose() {
		return nil
	}
	p.eagerAggProbing = true
	_, err := compileSubProgram(p, stmt, nil)
	p.eagerAggProbing = false
	if err != nil {
		return nil
	}
	out := ea.rewrite(stmt)
	if out == nil {
		return nil
	}
	if _, err := compileSubProgram(p, out, nil); err != nil {
		return nil
	}
	return out
}

// eagerAggMinFanout is the rows per key both fact sides need before the
// rewrite fires: below it the join is not much larger than its tables.
const eagerAggMinFanout = 4

type eaState struct {
	p     *ReadOnlyPager
	tabs  []*eaTable
	group map[[2]int]bool
	// where and on hold the conjuncts that stay: WHERE's, and each item's ON.
	where []Expr
	on    [][]Expr
	// build is set for mapOuter's second pass, which rebuilds the aggregates.
	build bool
	// having is the statement while its HAVING is mapped, whose aliases a
	// name there may be.
	having *SelectStmt
}

type eaTable struct {
	it   FromItem
	name string
	rt   *resolvedTable
	// pinned: read outside the aggregates and its own terms, or through more
	// than its key, so kept as it is.
	pinned bool
	key    int
	// partners are the (table, column) pairs the key is compared to.
	partners [][2]int
	terms    []Expr
	filtered bool
	pre      bool
	parts    []SelectColumn
	partOf   map[string]string
}

// eaSource returns the table a FROM item reads, for an ordinary table joined
// by an inner join, and false for any other item.
func (p *ReadOnlyPager) eaSource(it FromItem, i int) (*eaTable, bool) {
	if it.Subquery != nil || it.TableFunc || len(it.TableFuncArgs) != 0 || it.Schema != "" || it.Natural ||
		it.IndexedBy != "" || it.NotIndexed ||
		it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias || it.GroupRebuildID != 0 ||
		it.NestFromWrapDepth != 0 || len(it.NestedGroupSpan) != 0 || it.NestedNonLeading ||
		it.NestedGroupID != 0 || it.UpdateTarget {
		return nil, false
	}
	if i > 0 && it.Join != JoinCross && it.Join != JoinInner {
		return nil, false
	}
	if _, isCTE := p.lookupCTE(it.Table); isCTE {
		return nil, false
	}
	if vt, err := p.isVtabItem(it); err != nil || vt {
		return nil, false
	}
	rt, err := p.resolveTableIn(fromItemScope(it), it.Table)
	if err != nil {
		return nil, false
	}
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	return &eaTable{it: it, name: name, rt: rt, key: -1}, true
}

func (t *eaTable) col(name string) int {
	for i, c := range t.rt.cols {
		if !c.Hidden && strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// resolve finds the table and column a reference names: a qualified one in
// the table of that name, an unqualified one in the one table declaring it.
func (ea *eaState) resolve(c ColumnExpr) (t, col int, ok bool) {
	if c.Schema != "" {
		return 0, 0, false
	}
	t = -1
	for i, tab := range ea.tabs {
		if c.Qualifier != "" && !strings.EqualFold(c.Qualifier, tab.name) {
			continue
		}
		j := tab.col(c.Name)
		if j < 0 {
			continue
		}
		if t >= 0 {
			return 0, 0, false
		}
		t, col = i, j
	}
	return t, col, t >= 0
}

// classifyTerms sorts the WHERE and ON conjuncts and the USING equalities into
// key equalities and terms reading one table. Any other term pins the tables
// it reads. Every term must be one that cannot fail, since a fact's own terms
// run over all of its rows rather than the ones the join reaches.
func (ea *eaState) classifyTerms(stmt *SelectStmt) bool {
	ea.on = make([][]Expr, len(stmt.From))
	keep := func(cj Expr, item int) bool {
		var refs [][2]int
		ok := countCrossJoinSafe(cj, func(c ColumnExpr) bool {
			t, col, ok := ea.resolve(c)
			if ok {
				refs = append(refs, [2]int{t, col})
			}
			return ok
		})
		if !ok {
			return false
		}
		tables := map[int]bool{}
		for _, r := range refs {
			tables[r[0]] = true
		}
		switch {
		case len(tables) == 1:
			t := ea.tabs[refs[0][0]]
			t.terms = append(t.terms, cj)
		case len(tables) == 2 && ea.keyEquality(cj, refs):
		default:
			for t := range tables {
				ea.tabs[t].pinned = true
			}
		}
		// Every term is kept where it was; rewrite takes a fact's own terms
		// out again into its subquery.
		if item < 0 {
			ea.where = append(ea.where, cj)
		} else {
			ea.on[item] = append(ea.on[item], cj)
		}
		return true
	}
	for _, cj := range splitTopLevelAnd(stmt.Where) {
		if !keep(cj, -1) {
			return false
		}
	}
	for i, it := range stmt.From {
		for _, cj := range splitTopLevelAnd(it.On) {
			if !keep(cj, i) {
				return false
			}
		}
		for _, u := range it.Using {
			right := ea.tabs[i].col(u)
			if right < 0 {
				return false
			}
			left := -1
			for j := 0; j < i && left < 0; j++ {
				if c := ea.tabs[j].col(u); c >= 0 {
					left = j
					ea.link([2]int{j, c}, [2]int{i, right})
				}
			}
			if left < 0 {
				return false
			}
		}
	}
	return true
}

// keyEquality records "x.c = y.d" between two tables' bare columns.
func (ea *eaState) keyEquality(cj Expr, refs [][2]int) bool {
	b, ok := cj.(BinaryExpr)
	if !ok || (b.Op != "=" && b.Op != "==") || len(refs) != 2 {
		return false
	}
	if _, ok := b.L.(ColumnExpr); !ok {
		return false
	}
	if _, ok := b.R.(ColumnExpr); !ok {
		return false
	}
	ea.link(refs[0], refs[1])
	return true
}

func (ea *eaState) link(x, y [2]int) {
	for _, s := range [][2][2]int{{x, y}, {y, x}} {
		t := ea.tabs[s[0][0]]
		if t.key >= 0 && t.key != s[0][1] {
			t.pinned = true
		}
		t.key = s[0][1]
		t.partners = append(t.partners, s[1])
	}
}

// namesAlias reports a bare name that is a result column's alias. C resolves
// an ORDER BY term to it before any table's column (resolveAsName), and a
// HAVING name to it when no table has the column (lookupName).
func (ea *eaState) namesAlias(stmt *SelectStmt, e Expr) bool {
	c, ok := e.(ColumnExpr)
	if !ok || c.Qualifier != "" || c.Schema != "" {
		return false
	}
	for _, sc := range stmt.Columns {
		if sc.HasAlias && strings.EqualFold(sc.Alias, c.Name) {
			return true
		}
	}
	return false
}

// mapOuter checks (and with build set, rewrites) an expression of the result,
// HAVING or ORDER BY: aggregates are count/min/max of a bare column, every
// other column one GROUP BY names.
func (ea *eaState) mapOuter(e Expr) (Expr, bool) {
	switch x := e.(type) {
	case LiteralExpr, ParamExpr:
		return e, true
	case ColumnExpr:
		t, col, ok := ea.resolve(x)
		if !ok && ea.having != nil {
			return e, ea.namesAlias(ea.having, x)
		}
		return e, ok && ea.group[[2]int{t, col}]
	case UnaryExpr:
		v, ok := ea.mapOuter(x.X)
		x.X = v
		return x, ok
	case IsNullExpr:
		v, ok := ea.mapOuter(x.X)
		x.X = v
		return x, ok
	case CastExpr:
		v, ok := ea.mapOuter(x.X)
		x.X = v
		return x, ok
	case CollateExpr:
		v, ok := ea.mapOuter(x.X)
		x.X = v
		return x, ok
	case BinaryExpr:
		l, okL := ea.mapOuter(x.L)
		r, okR := ea.mapOuter(x.R)
		x.L, x.R = l, r
		return x, okL && okR
	case FuncExpr:
		if x.Over != nil || x.Filter != nil || len(x.OrderBy) != 0 || x.Distinct {
			return e, false
		}
		if isAggregateCall(x) {
			return ea.agg(x)
		}
		if x.Star {
			return e, false
		}
		args := make([]Expr, len(x.Args))
		for i, a := range x.Args {
			v, ok := ea.mapOuter(a)
			if !ok {
				return e, false
			}
			args[i] = v
		}
		x.Args = args
		return x, true
	}
	return e, false
}

// agg checks one aggregate call, and with build set returns its rebuilt form.
func (ea *eaState) agg(fc FuncExpr) (Expr, bool) {
	name := r33sFoldIdent(fc.Name)
	if name == "count" && fc.Star && len(fc.Args) == 0 {
		if !ea.build {
			return fc, true
		}
		return ea.sum(ea.product(-1)), true
	}
	if (name != "count" && name != "min" && name != "max") || fc.Star || len(fc.Args) != 1 {
		return fc, false
	}
	c, ok := fc.Args[0].(ColumnExpr)
	if !ok {
		return fc, false
	}
	ti, col, ok := ea.resolve(c)
	if !ok {
		return fc, false
	}
	if !ea.build {
		return fc, true
	}
	t := ea.tabs[ti]
	if !t.pre {
		if name == "count" {
			return ea.sum(eaTimes(IsNullExpr{X: c, Not: true}, ea.product(-1))), true
		}
		return fc, true
	}
	part := ColumnExpr{Qualifier: t.name, Name: t.part(name, col)}
	if name == "count" {
		return ea.sum(eaTimes(part, ea.product(ti))), true
	}
	return FuncExpr{Name: name, Args: []Expr{part}}, true
}

// part returns the name of t's per-key partial fn(col), adding it on first
// use; col -1 is count(*).
func (t *eaTable) part(fn string, col int) string {
	k := fmt.Sprintf("%s:%d", fn, col)
	if n, ok := t.partOf[k]; ok {
		return n
	}
	n := fmt.Sprintf("musql_ea_%d", len(t.parts))
	t.partOf[k] = n
	var call Expr = FuncExpr{Name: fn, Star: true}
	if col >= 0 {
		call = FuncExpr{Name: fn, Args: []Expr{ColumnExpr{Qualifier: t.name, Name: t.rt.cols[col].Name}}}
		if coll := t.rt.cols[col].Collation; fn != "count" && !eaBinary(t.rt.cols[col]) {
			// The outer min/max must compare under the argument column's
			// collation, and a derived column takes its expression's. An
			// explicit COLLATE on the outer call's argument would instead give
			// the call's result that collation, where the original's has none.
			call = CollateExpr{X: call, Name: coll}
		}
	}
	t.parts = append(t.parts, SelectColumn{
		Expr:     call,
		Alias:    n,
		HasAlias: true,
		Text:     n,
	})
	return n
}

// product multiplies every fact's per-key row count except skip's.
func (ea *eaState) product(skip int) Expr {
	var e Expr
	for i, t := range ea.tabs {
		if t.pre && i != skip {
			e = eaTimes(e, ColumnExpr{Qualifier: t.name, Name: t.part("count", -1)})
		}
	}
	return e
}

func eaTimes(l, r Expr) Expr {
	if l == nil {
		return r
	}
	if r == nil {
		return l
	}
	return BinaryExpr{Op: "*", L: l, R: r}
}

func (ea *eaState) sum(e Expr) Expr { return FuncExpr{Name: "sum", Args: []Expr{e}} }

// choose picks the facts and reports whether the rewrite pays. A fact fans
// out when it has at least eagerAggMinFanout rows per key: what ANALYZE
// measured, or else its rows over its key's values (keyBound). The per-key
// subqueries are joined to each other without an index, so the rewritten join
// walks up to the product of their key counts, and it pays only when the join
// as written, estimated as the smallest kept table's rows times every fact's
// rows per key, is at least that large.
func (ea *eaState) choose() bool {
	kept := int64(-1)
	rows := make([]int64, len(ea.tabs))
	for i, t := range ea.tabs {
		n, ok := ea.p.segFilterCountTable(t.rt.root, nil, t.rt.cols, t.rt.ipkIndex)
		if !ok {
			return false
		}
		rows[i] = int64(n)
		if !ea.candidate(t) && (kept < 0 || rows[i] < kept) {
			kept = rows[i]
		}
	}
	if kept <= 0 {
		return false
	}
	facts := 0
	join, walk := float64(kept), 1.0
	for i, t := range ea.tabs {
		if !ea.candidate(t) || rows[i] == 0 {
			continue
		}
		keys := float64(ea.keyBound(t, rows[i]))
		if m, ok := ea.statFanout(t); ok && m > 0 {
			keys = max(1, float64(rows[i])/float64(m))
		}
		if fanout := float64(rows[i]) / keys; fanout >= eagerAggMinFanout {
			t.pre = true
			facts++
			join *= fanout
			walk *= keys
		}
	}
	if facts < 2 || join < walk {
		return false
	}
	filtered := false
	for _, t := range ea.tabs {
		filtered = filtered || (!t.pre && len(t.terms) != 0)
	}
	if !filtered {
		return true
	}
	for _, t := range ea.tabs {
		if !t.pre {
			continue
		}
		if t.rt.ipkIndex == t.key {
			return false
		}
		cands, err := ea.p.secondaryIndexSeekCandidates(t.it.Table, t.rt.root, t.rt.cols)
		if err != nil {
			return false
		}
		for _, c := range cands {
			if c.leadingCol == t.key {
				return false
			}
		}
	}
	return true
}

// keyBound bounds how many distinct values t's key holds: its integer cells
// counted in a bitmap over their span, plus a value for every row the delta log
// adds; or else its rows, where the key is not a block of integers or the
// bitmap would be larger than the column. A NULL cell counts as whatever it
// holds, which can only add one.
func (ea *eaState) keyBound(t *eaTable, rows int64) int64 {
	if t.key == t.rt.ipkIndex || ea.p.segs == nil {
		return rows
	}
	segs := ea.p.segs.byRoot[t.rt.root]
	cols := make([][]int64, 0, len(segs))
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for _, s := range segs {
		if s.nRows == 0 {
			continue
		}
		vals, ok := s.Int64Column(t.key)
		if !ok || s.hasException(t.key) {
			return rows
		}
		for _, v := range vals {
			lo, hi = min(lo, v), max(hi, v)
		}
		cols = append(cols, vals)
	}
	_, add, ok := ea.p.segs.segDeltaCorrection(t.rt.root, segs, nil)
	if !ok {
		return rows
	}
	if lo > hi {
		return min(rows, int64(add))
	}
	span := uint64(hi) - uint64(lo) + 1
	if span == 0 || span > 64*uint64(rows) {
		return rows
	}
	seen := make([]uint64, (span+63)/64)
	for _, vals := range cols {
		for _, v := range vals {
			i := uint64(v) - uint64(lo)
			seen[i/64] |= 1 << (i % 64)
		}
	}
	n := int64(add)
	for _, w := range seen {
		n += int64(bits.OnesCount64(w))
	}
	return min(rows, n)
}

// statFanout returns the rows per key ANALYZE measured for t's key, from an
// index that leads with it.
func (ea *eaState) statFanout(t *eaTable) (uint64, bool) {
	st := wherePlanStat1For(ea.p, t.rt)
	if st == nil {
		return 0, false
	}
	cands, err := ea.p.secondaryIndexSeekCandidates(t.it.Table, t.rt.root, t.rt.cols)
	if err != nil {
		return 0, false
	}
	for _, c := range cands {
		is := st.indexes[r33sFoldIdent(c.name)]
		if c.leadingCol != t.key || is == nil || !is.hasStat1 || len(is.decodes) == 0 {
			continue
		}
		var a [2]logEst
		for _, z := range is.decodes {
			decodeStat1(z, 2, a[:])
		}
		return logEstToInt(a[1]), true
	}
	return 0, false
}

// candidate reports a table that could be pre-aggregated: reached through one
// key whose groups compare to the other side as one row does.
func (ea *eaState) candidate(t *eaTable) bool {
	if t.pinned || t.key < 0 {
		return false
	}
	k := t.rt.cols[t.key]
	if !eaBinary(k) {
		return false
	}
	for _, pr := range t.partners {
		o := ea.tabs[pr[0]].rt.cols[pr[1]]
		if !eaBinary(o) || (k.Aff == affNone && o.Aff == affText) {
			return false
		}
	}
	return true
}

func eaBinary(c columnInfo) bool {
	return c.Collation == "" || strings.EqualFold(c.Collation, "BINARY")
}

// rewrite builds the statement over the per-key subqueries.
func (ea *eaState) rewrite(stmt *SelectStmt) *SelectStmt {
	for _, t := range ea.tabs {
		t.partOf = map[string]string{}
		if t.pre {
			// The row count is every fact's first partial.
			t.part("count", -1)
		}
	}
	ea.build = true
	out := *stmt
	out.orderNotC = true
	out.Columns = make([]SelectColumn, len(stmt.Columns))
	for i, c := range stmt.Columns {
		v, ok := ea.mapOuter(c.Expr)
		if !ok {
			return nil
		}
		c.Expr = v
		out.Columns[i] = c
	}
	if stmt.Having != nil {
		ea.having = stmt
		v, ok := ea.mapOuter(stmt.Having)
		ea.having = nil
		if !ok {
			return nil
		}
		out.Having = v
	}
	if len(stmt.OrderBy) != 0 {
		out.OrderBy = make([]OrderTerm, len(stmt.OrderBy))
		for i, ot := range stmt.OrderBy {
			if !ea.namesAlias(stmt, ot.Expr) {
				v, ok := ea.mapOuter(ot.Expr)
				if !ok {
					return nil
				}
				ot.Expr = v
			}
			out.OrderBy[i] = ot
		}
	}
	stays := func(cjs []Expr) []Expr {
		var kept []Expr
		for _, cj := range cjs {
			if t := ea.ownTable(cj); t == nil || !t.pre {
				kept = append(kept, cj)
			}
		}
		return kept
	}
	out.Where = andConjuncts(stays(ea.where))
	out.From = make([]FromItem, len(stmt.From))
	for i, it := range stmt.From {
		it.On = andConjuncts(stays(ea.on[i]))
		t := ea.tabs[i]
		if t.pre {
			k := t.rt.cols[t.key].Name
			for _, pc := range t.parts {
				if strings.EqualFold(pc.Alias, k) {
					return nil
				}
			}
			base := t.it
			base.Join, base.On, base.Using = JoinCross, nil, nil
			cols := append([]SelectColumn{{Expr: ColumnExpr{Qualifier: t.name, Name: k}, Text: k}}, t.parts...)
			it = FromItem{
				Subquery: &SelectStmt{
					Columns:   cols,
					From:      []FromItem{base},
					Where:     andConjuncts(t.terms),
					GroupBy:   []Expr{ColumnExpr{Qualifier: t.name, Name: k}},
					orderNotC: true,
				},
				Alias: t.name,
				Join:  it.Join,
				On:    it.On,
				Using: it.Using,
			}
		}
		out.From[i] = it
	}
	return &out
}

// ownTable returns the one table a conjunct reads, or nil.
func (ea *eaState) ownTable(cj Expr) *eaTable {
	var own *eaTable
	one := countCrossJoinSafe(cj, func(c ColumnExpr) bool {
		t, _, ok := ea.resolve(c)
		if !ok || (own != nil && own != ea.tabs[t]) {
			return false
		}
		own = ea.tabs[t]
		return true
	})
	if !one {
		return nil
	}
	return own
}
