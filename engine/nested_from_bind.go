package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Name resolution through SF_NestedFrom groups, as C does it.
//
// C wraps a parenthesized join that is aliased or not leading into a
// subquery "SELECT * FROM <group>" (nested_from.go). Its columns are what
// selectExpander builds for that subquery: per member, the next member's
// USING columns as "..x" entries, the member's own columns named
// "db.tab.col", then its rowid alias; and sqlite3ColumnsFromExprList renames
// repeats with ":N". A name reaches into the group through those entries
// (lookupName's isNestedFrom arm), or through the group's alias and the
// renamed columns. The engine executes a group as a flat join or a
// materialized row, which is not what decides names.
//
// nfBind ports that resolution for a SELECT statement tree holding such
// groups, before anything else reads the tree, and rewrites it so the rest of
// the engine has nothing left to decide:
//
//   - every FROM item under a SELECT holding a group gets a synthetic alias,
//     unique in the statement;
//   - every column reference C binds to such an item becomes a reference to
//     that item by its synthetic alias, by column name or a rowid spelling;
//   - each "*" in such a SELECT becomes the columns C expands it into, named
//     as C names them (they are AS names in C too, ENAME_NAME);
//   - an unaliased result column naming a column keeps the name C gives it
//     (SelectColumn.cName).
//
// Where C reports an error, the first one in C's order is returned. Anything
// the port does not cover leaves the whole statement as it was: the rewrite is
// all or nothing.
//
// Not covered, and left as written: a column C resolves to the coalesce() of
// a FULL JOIN's USING column, a USING join C codes with such a coalesce, a
// compound SELECT holding a group, full_column_names, and a FROM item whose
// columns cannot be learned.

// errNFAbstain marks a shape the port does not cover.
var errNFAbstain = errors.New("nested-from: not covered")

// The JT_* bits of C's SrcItem.fg.jointype.
const (
	nfJTInner   = 0x01
	nfJTCross   = 0x02
	nfJTNatural = 0x04
	nfJTLeft    = 0x08
	nfJTRight   = 0x10
	nfJTOuter   = 0x20
	nfJTLtorj   = 0x40
)

// nfCol is one column of an item's table (pSTab->aCol).
type nfCol struct {
	name     string
	hidden   bool
	noExpand bool // COLFLAG_NOEXPAND
}

// nfEntry is one entry of an SF_NestedFrom subquery's result list.
type nfEntry struct {
	ename     string // zEName, "db.tab.col"
	rowid     bool   // ENAME_ROWID
	usingTerm bool   // bUsingTerm
	noExpand  bool   // bNoExpand
	// The entry's expression: a bare name (a USING term), or "db.tab.col"
	// with db possibly empty. self, when set, is the item the qualifier names
	// (an item with no name a reference could spell).
	refDB, refTab, refCol string
	self                  *nfItem
	bind                  *nfBinding
}

// nfItem is one SrcItem.
type nfItem struct {
	sel *nfSel
	idx int

	// A leaf: the FromItem in the working copy, by cID.
	cID  int
	leaf *FromItem
	kind nfLeafKind
	// A nested group.
	nested *nfSel

	alias   string // zAlias; "" for none
	tabName string // pSTab->zName, or a unique token for an unnamed item
	db      string // the schema's name, "" for an item with no schema
	cols    []nfCol
	colls   []string // each leaf column's declared collating sequence
	visRow  bool     // VisibleRowid
	ipk     int      // iPKey, -1 for none
	jt      int
	using   []string // after NATURAL is synthesized
	isUsing bool
	synthU  bool // isSynthUsing
	node    *cSrcNode

	synth string // the synthetic alias, for a leaf of a managed SELECT
}

type nfLeafKind int

const (
	nfLeafTable nfLeafKind = iota
	nfLeafView
	nfLeafSub
	nfLeafCTE
	nfLeafVtab
)

// nfSel is one SELECT C resolves: a statement's SELECT or an SF_NestedFrom
// group.
type nfSel struct {
	items  []*nfItem
	nested bool
	elist  []*nfEntry // nested only
	// stmt is the working copy of a statement SELECT; owner is the statement
	// SELECT whose FROM holds this one (itself for a statement SELECT).
	stmt  *SelectStmt
	owner *SelectStmt
	// managed: the SELECT holds a group, so its leaves are renamed and every
	// reference to them rewritten.
	managed bool
	// outer is the name context outside this SELECT that its expressions see
	// (for a nested group, its parent's outer, as for any FROM subquery).
	outer *nfNC
}

// nfBinding is what a reference resolves to: a leaf's column (col >= 0) or
// rowid (col == -1), through any number of groups.
type nfBinding struct {
	item *nfItem
	col  int
	// rest, when set, makes the binding C's coalesce() of a FULL JOIN's
	// USING column: this column, then each of rest.
	rest []*nfBinding
}

// nfCoalesce is the binding of coalesce(parts...), flattened.
func nfCoalesce(parts []*nfBinding) *nfBinding {
	var flat []*nfBinding
	for _, p := range parts {
		flat = append(flat, &nfBinding{item: p.item, col: p.col})
		flat = append(flat, p.rest...)
	}
	out := *flat[0]
	if len(flat) > 1 {
		out.rest = flat[1:]
	}
	return &out
}

// nfNC is a NameContext.
type nfNC struct {
	sel *nfSel
	// aliases is the result list's AS names, for NC_UEList (nil where C does
	// not set it).
	aliases map[string]bool
	next    *nfNC
	// unknown marks the end of what the port knows: a reference that reaches
	// it is left alone, neither bound nor reported.
	unknown bool
}

// nfBinder carries one statement's resolution.
type nfBinder struct {
	p        *ReadOnlyPager
	synthSeq int
	err      error // the first C error, in C's order
	abstain  bool
	why      string
	unnamed  int
}

// nfMarker is what a synthetic alias starts with; a SQL name containing a NUL
// cannot be written.
const nfMarker = "\x00nf"

func isNFSynthAlias(s string) bool { return strings.HasPrefix(s, nfMarker) }

// nfBindSelect rewrites stmt, a statement's top-level SELECT, as described
// above. It returns stmt itself when no SELECT in it holds a group, or when
// the port does not cover it.
func (p *ReadOnlyPager) nfBindSelect(stmt *SelectStmt, topLevel bool) (*SelectStmt, error) {
	if stmt == nil || stmt.nfBound || !selectTreeHasNested(stmt) {
		return stmt, nil
	}
	if p == nil || p.colNameMode().full {
		return stmt, nil
	}
	b := &nfBinder{p: p}
	cp := nfCopySelect(stmt)
	var outer *nfNC
	if !topLevel {
		outer = &nfNC{unknown: true}
	}
	role := nfRoleOutput
	if !topLevel {
		role = nfRoleSubquery
	}
	b.selectStmt(cp, outer, role)
	if b.abstain {
		return stmt, nil
	}
	if b.err != nil {
		return nil, b.err
	}
	// Every SELECT in the tree is final: compiling one on its own must not
	// resolve it again.
	nfWalkSelects(cp, func(x *SelectStmt) { x.nfBound = true })
	return cp, nil
}

// selectTreeHasNested reports whether any SELECT in the tree holds a group.
func selectTreeHasNested(s *SelectStmt) bool {
	found := false
	nfWalkSelects(s, func(x *SelectStmt) {
		if cFromHasNested(x.cFrom) {
			found = true
		}
	})
	return found
}

// nfWalkSelects calls fn on s and every SELECT inside it.
func nfWalkSelects(s *SelectStmt, fn func(*SelectStmt)) {
	if s == nil {
		return
	}
	fn(s)
	for _, a := range s.Compound {
		nfWalkSelects(a.Stmt, fn)
	}
	for _, c := range s.CTEs {
		nfWalkSelects(c.Select, fn)
	}
	var walkFrom func(items []FromItem)
	walkFrom = func(items []FromItem) {
		for i := range items {
			nfWalkSelects(items[i].Subquery, fn)
			walkFrom(items[i].NestedGroupSpan)
			for _, a := range items[i].TableFuncArgs {
				nfWalkExprSelects(a, fn)
			}
			nfWalkExprSelects(items[i].On, fn)
		}
	}
	walkFrom(s.From)
	for _, c := range s.Columns {
		nfWalkExprSelects(c.Expr, fn)
	}
	nfWalkExprSelects(s.Where, fn)
	for _, g := range s.GroupBy {
		nfWalkExprSelects(g, fn)
	}
	nfWalkExprSelects(s.Having, fn)
	for _, o := range s.OrderBy {
		nfWalkExprSelects(o.Expr, fn)
	}
	for _, w := range s.Windows {
		nfWalkWindowSelects(w.Spec, fn)
	}
	nfWalkExprSelects(s.LimitParam, fn)
	nfWalkExprSelects(s.OffsetParam, fn)
}

func nfWalkWindowSelects(w *WindowSpec, fn func(*SelectStmt)) {
	if w == nil {
		return
	}
	for _, e := range w.PartitionBy {
		nfWalkExprSelects(e, fn)
	}
	for _, o := range w.OrderBy {
		nfWalkExprSelects(o.Expr, fn)
	}
}

func nfWalkExprSelects(e Expr, fn func(*SelectStmt)) {
	nfMapExpr(e, func(ce ColumnExpr) Expr { return ce }, func(s *SelectStmt) *SelectStmt {
		nfWalkSelects(s, fn)
		return s
	})
}

// nfMapExpr rebuilds e with fn applied to every column reference outside a
// subquery and sub applied to every subquery.
func nfMapExpr(e Expr, fn func(ColumnExpr) Expr, sub func(*SelectStmt) *SelectStmt) Expr {
	if e == nil {
		return nil
	}
	list := func(es []Expr) []Expr {
		if es == nil {
			return nil
		}
		out := make([]Expr, len(es))
		for i, x := range es {
			out[i] = nfMapExpr(x, fn, sub)
		}
		return out
	}
	orders := func(os []OrderTerm) []OrderTerm {
		if os == nil {
			return nil
		}
		out := make([]OrderTerm, len(os))
		for i, o := range os {
			o.Expr = nfMapExpr(o.Expr, fn, sub)
			out[i] = o
		}
		return out
	}
	switch x := e.(type) {
	case ColumnExpr:
		return fn(x)
	case UnaryExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		return x
	case BinaryExpr:
		x.L = nfMapExpr(x.L, fn, sub)
		x.R = nfMapExpr(x.R, fn, sub)
		return x
	case IsNullExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		return x
	case InExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		x.List = list(x.List)
		if x.Sub != nil {
			x.Sub = sub(x.Sub)
		}
		return x
	case BetweenExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		x.Lo = nfMapExpr(x.Lo, fn, sub)
		x.Hi = nfMapExpr(x.Hi, fn, sub)
		return x
	case LikeExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		x.Pattern = nfMapExpr(x.Pattern, fn, sub)
		x.Escape = nfMapExpr(x.Escape, fn, sub)
		return x
	case GlobExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		x.Pattern = nfMapExpr(x.Pattern, fn, sub)
		return x
	case MatchExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		x.Pattern = nfMapExpr(x.Pattern, fn, sub)
		return x
	case CollateExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		return x
	case CastExpr:
		x.X = nfMapExpr(x.X, fn, sub)
		return x
	case FuncExpr:
		x.Args = list(x.Args)
		x.Filter = nfMapExpr(x.Filter, fn, sub)
		x.OrderBy = orders(x.OrderBy)
		if x.Over != nil {
			w := *x.Over
			w.PartitionBy = list(w.PartitionBy)
			w.OrderBy = orders(w.OrderBy)
			x.Over = &w
		}
		return x
	case CaseExpr:
		x.Base = nfMapExpr(x.Base, fn, sub)
		if x.Whens != nil {
			ws := make([]WhenClause, len(x.Whens))
			for i, w := range x.Whens {
				ws[i] = WhenClause{When: nfMapExpr(w.When, fn, sub), Then: nfMapExpr(w.Then, fn, sub)}
			}
			x.Whens = ws
		}
		x.Else = nfMapExpr(x.Else, fn, sub)
		return x
	case SubqueryExpr:
		x.Stmt = sub(x.Stmt)
		return x
	case ExistsExpr:
		x.Stmt = sub(x.Stmt)
		return x
	case RowExpr:
		x.Elems = list(x.Elems)
		return x
	case RaiseExpr:
		x.Msg = nfMapExpr(x.Msg, fn, sub)
		return x
	}
	return e
}

// nfCopySelect deep-copies the parts of a SELECT tree the rewrite changes:
// every SelectStmt, FROM list and select list. Expressions are values and are
// rebuilt by the rewrite itself where it touches them.
func nfCopySelect(s *SelectStmt) *SelectStmt {
	if s == nil {
		return nil
	}
	c := *s
	c.Columns = append([]SelectColumn(nil), s.Columns...)
	c.From = nfCopyFrom(s.From)
	if s.Compound != nil {
		c.Compound = make([]CompoundArm, len(s.Compound))
		for i, a := range s.Compound {
			a.Stmt = nfCopySelect(a.Stmt)
			c.Compound[i] = a
		}
	}
	if s.CTEs != nil {
		c.CTEs = make([]CTEDef, len(s.CTEs))
		for i, d := range s.CTEs {
			d.Select = nfCopySelect(d.Select)
			c.CTEs[i] = d
		}
	}
	copySub := func(x *SelectStmt) *SelectStmt { return nfCopySelect(x) }
	same := func(ce ColumnExpr) Expr { return ce }
	for i := range c.Columns {
		c.Columns[i].Expr = nfMapExpr(c.Columns[i].Expr, same, copySub)
	}
	c.Where = nfMapExpr(s.Where, same, copySub)
	if s.GroupBy != nil {
		c.GroupBy = make([]Expr, len(s.GroupBy))
		for i, g := range s.GroupBy {
			c.GroupBy[i] = nfMapExpr(g, same, copySub)
		}
	}
	c.Having = nfMapExpr(s.Having, same, copySub)
	if s.OrderBy != nil {
		c.OrderBy = make([]OrderTerm, len(s.OrderBy))
		for i, o := range s.OrderBy {
			o.Expr = nfMapExpr(o.Expr, same, copySub)
			c.OrderBy[i] = o
		}
	}
	if s.Windows != nil {
		c.Windows = make([]NamedWindow, len(s.Windows))
		for i, w := range s.Windows {
			if w.Spec != nil {
				sp := *w.Spec
				sp.PartitionBy = append([]Expr(nil), sp.PartitionBy...)
				sp.OrderBy = append([]OrderTerm(nil), sp.OrderBy...)
				for k := range sp.PartitionBy {
					sp.PartitionBy[k] = nfMapExpr(sp.PartitionBy[k], same, copySub)
				}
				for k := range sp.OrderBy {
					sp.OrderBy[k].Expr = nfMapExpr(sp.OrderBy[k].Expr, same, copySub)
				}
				w.Spec = &sp
			}
			c.Windows[i] = w
		}
	}
	return &c
}

func nfCopyFrom(items []FromItem) []FromItem {
	if items == nil {
		return nil
	}
	out := append([]FromItem(nil), items...)
	copySub := func(x *SelectStmt) *SelectStmt { return nfCopySelect(x) }
	same := func(ce ColumnExpr) Expr { return ce }
	for i := range out {
		out[i].Subquery = nfCopySelect(out[i].Subquery)
		out[i].NestedGroupSpan = nfCopyFrom(out[i].NestedGroupSpan)
		out[i].On = nfMapExpr(out[i].On, same, copySub)
		if out[i].TableFuncArgs != nil {
			args := make([]Expr, len(out[i].TableFuncArgs))
			for k, a := range out[i].TableFuncArgs {
				args[k] = nfMapExpr(a, same, copySub)
			}
			out[i].TableFuncArgs = args
		}
		out[i].Using = append([]string(nil), out[i].Using...)
	}
	return out
}

// nfRole is how C names a SELECT's result columns.
type nfRole int

const (
	// nfRoleOutput: the statement's own result (sqlite3GenerateColumnNames,
	// after resolution) and a view's body (sqlite3ResultSetOfSelect).
	nfRoleOutput nfRole = iota
	// nfRoleSubquery: a FROM subquery or CTE body, named by
	// sqlite3ColumnsFromExprList before resolution, from the written token.
	nfRoleSubquery
	// nfRoleExpr: a subquery in an expression, whose names nothing reads.
	nfRoleExpr
)

// giveUp abandons the rewrite, recording the first reason (for tests).
func (b *nfBinder) giveUp(why string) {
	if !b.abstain {
		b.why = why
	}
	b.abstain = true
}

// fail records the first C error.
func (b *nfBinder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

func (b *nfBinder) stop() bool { return b.abstain || b.err != nil }

// selectStmt resolves one statement SELECT s (a compound's arms too) against
// outer, rewriting it in place.
func (b *nfBinder) selectStmt(s *SelectStmt, outer *nfNC, role nfRole) {
	if s == nil || b.stop() {
		return
	}
	if len(s.Compound) > 0 {
		// C walks a compound from its last arm (the head) back to its first.
		// An arm holding a group is not covered.
		for _, a := range s.Compound {
			if selectTreeHasNestedOwn(a.Stmt) {
				b.giveUp("a compound arm holds a group")
				return
			}
		}
		if cFromHasNested(s.cFrom) {
			b.giveUp("a compound holds a group")
			return
		}
	}
	popCTE := func() {}
	if len(s.CTEs) > 0 {
		popCTE = b.p.pushCTEScope(s.CTEs)
	}
	defer popCTE()
	// A CTE body is resolved where C copies it into a FROM item; it cannot see
	// the statement around it here.
	for i := range s.CTEs {
		b.selectStmt(s.CTEs[i].Select, &nfNC{unknown: true}, nfRoleSubquery)
		if b.stop() {
			return
		}
	}
	managed := cFromHasNested(s.cFrom)
	relevant := managed || outer.hasManaged()
	if !relevant {
		// Nothing here is renamed and nothing outside is known: only the
		// SELECTs inside need visiting.
		b.visitInner(s, &nfNC{unknown: true})
		return
	}
	if s.cFrom == nil && len(s.From) > 0 {
		b.giveUp("no FROM tree")
		return
	}
	if relevant && len(s.Compound) > 0 {
		// A compound's ORDER BY names its result columns, not a FROM
		// item; not covered.
		b.giveUp("a compound inside a group's scope")
		return
	}
	sel := &nfSel{stmt: s, owner: s, managed: managed, outer: outer}
	if !b.buildFrom(sel, s.cFrom, s.From, outer) {
		return
	}
	if managed {
		b.processJoins(sel)
		if b.stop() {
			return
		}
		b.expandStars(sel, role)
		if b.stop() {
			return
		}
	}
	b.resolveSelect(sel, role)
	for _, a := range s.Compound {
		b.selectStmt(a.Stmt, outer, nfRoleExpr)
	}
}

// selectTreeHasNestedOwn is selectTreeHasNested for one arm.
func selectTreeHasNestedOwn(s *SelectStmt) bool {
	return s != nil && selectTreeHasNested(s)
}

func (nc *nfNC) hasManaged() bool {
	for ; nc != nil; nc = nc.next {
		if nc.sel != nil && nc.sel.managed {
			return true
		}
	}
	return false
}

// visitInner processes the SELECTs inside s with no knowledge of s itself.
func (b *nfBinder) visitInner(s *SelectStmt, nc *nfNC) {
	sub := func(x *SelectStmt) *SelectStmt {
		b.selectStmt(x, nc, nfRoleExpr)
		return x
	}
	same := func(ce ColumnExpr) Expr { return ce }
	var from func(items []FromItem)
	from = func(items []FromItem) {
		for i := range items {
			if items[i].Subquery != nil {
				b.selectStmt(items[i].Subquery, nc, nfRoleSubquery)
			}
			from(items[i].NestedGroupSpan)
			items[i].On = nfMapExpr(items[i].On, same, sub)
		}
	}
	from(s.From)
	for i := range s.Columns {
		s.Columns[i].Expr = nfMapExpr(s.Columns[i].Expr, same, sub)
	}
	s.Where = nfMapExpr(s.Where, same, sub)
	for i := range s.GroupBy {
		s.GroupBy[i] = nfMapExpr(s.GroupBy[i], same, sub)
	}
	s.Having = nfMapExpr(s.Having, same, sub)
	for i := range s.OrderBy {
		s.OrderBy[i].Expr = nfMapExpr(s.OrderBy[i].Expr, same, sub)
	}
	for _, a := range s.Compound {
		b.selectStmt(a.Stmt, nc, nfRoleExpr)
	}
}

// ---- building the FROM model ----

// flatLeaves maps every leaf FromItem of a FROM list (stand-in spans
// included) by cID, as pointers into the working copy.
func flatLeaves(items []FromItem, out map[int]*FromItem) {
	for i := range items {
		if items[i].cID != 0 {
			out[items[i].cID] = &items[i]
		}
		flatLeaves(items[i].NestedGroupSpan, out)
	}
}

// buildFrom builds sel's items from its C tree, probing every leaf.
func (b *nfBinder) buildFrom(sel *nfSel, nodes []cSrcNode, flat []FromItem, outer *nfNC) bool {
	leaves := map[int]*FromItem{}
	flatLeaves(flat, leaves)
	var build func(s *nfSel, nodes []cSrcNode) bool
	build = func(s *nfSel, nodes []cSrcNode) bool {
		for i := range nodes {
			n := &nodes[i]
			it := &nfItem{sel: s, idx: len(s.items), node: n, ipk: -1}
			it.jt = nfJoinBits(n)
			if n.leaf != 0 {
				leaf := leaves[n.leaf]
				if leaf == nil || !b.probeLeaf(it, leaf) {
					b.giveUp("a FROM item whose table could not be learned")
					return false
				}
				if sel.managed {
					b.synthSeq++
					it.synth = nfMarker + strconv.Itoa(b.synthSeq)
				}
			} else {
				child := &nfSel{nested: true, owner: sel.owner, managed: sel.managed, outer: outer}
				if !build(child, n.sub) {
					return false
				}
				it.nested = child
				if n.hasAlias {
					it.alias = n.alias
					it.tabName = n.alias
				} else {
					b.unnamed++
					it.tabName = fmt.Sprintf("\x00sub%d", b.unnamed)
				}
			}
			if n.joinSet {
				it.using = append([]string(nil), n.using...)
				it.isUsing = len(n.using) > 0
			}
			s.items = append(s.items, it)
		}
		nfShiftJoinType(s.items)
		return true
	}
	return build(sel, nodes)
}

// nfJoinBits is the JT_* bits C stores for a node's join operator.
func nfJoinBits(n *cSrcNode) int {
	if !n.joinSet {
		return 0
	}
	jt := 0
	switch n.join {
	case JoinCross, JoinInner:
		jt = nfJTInner
		if n.cross {
			jt |= nfJTCross
		}
	case JoinLeft:
		jt = nfJTLeft | nfJTOuter
	case JoinRight:
		jt = nfJTRight | nfJTOuter
	case JoinFull:
		jt = nfJTLeft | nfJTRight | nfJTOuter
	}
	if n.natural {
		jt |= nfJTNatural
	}
	return jt
}

// nfShiftJoinType is the JT_LTORJ half of sqlite3SrcListShiftJoinType: every
// item left of the last RIGHT or FULL join is marked.
func nfShiftJoinType(items []*nfItem) {
	last := -1
	for i, it := range items {
		if it.jt&nfJTRight != 0 {
			last = i
		}
	}
	for i := 0; i < last; i++ {
		items[i].jt |= nfJTLtorj
	}
}

// probeLeaf learns a leaf's table: its columns, rowid and names, as the
// engine resolves the item on its own.
func (b *nfBinder) probeLeaf(it *nfItem, leaf *FromItem) bool {
	p := b.p
	probe := *leaf
	probe.Join, probe.On, probe.Using, probe.Natural, probe.CrossKeyword = JoinCross, nil, nil, false, false
	probe.GroupLen, probe.GroupAlias, probe.HasGroupAlias, probe.GroupRebuildID = 0, "", false, 0
	probe.NestFromWrapDepth, probe.NestedNonLeading, probe.NestedGroupID = 0, false, 0
	probe.GroupRebuildStarred, probe.GroupRebuildQualStarred = false, false
	probe.IndexedBy, probe.NotIndexed = "", false
	if probe.TableFunc {
		// The arguments decide no column; one naming another item cannot
		// be folded on its own.
		args := make([]Expr, len(probe.TableFuncArgs))
		for i, a := range probe.TableFuncArgs {
			if exprHasColumnRef(a) {
				args[i] = LiteralExpr{Val: Value{Typ: Null}}
			} else {
				args[i] = a
			}
		}
		probe.TableFuncArgs = args
	}
	restore := p.nfSnapshotCounts()
	srcs, err := resolveJoinSources(p, &compiler{pager: p}, []FromItem{probe})
	restore()
	if err != nil || len(srcs) != 1 || len(srcs[0].memberScopes) > 0 {
		return false
	}
	ts := srcs[0].scope.tableScope
	it.cID = leaf.cID
	it.leaf = leaf
	it.alias = leaf.Alias
	switch {
	case srcs[0].cteItem != nil:
		it.kind = nfLeafCTE
		it.tabName = leaf.Table
	case leaf.Subquery != nil:
		it.kind = nfLeafSub
		b.unnamed++
		it.tabName = fmt.Sprintf("\x00sub%d", b.unnamed)
	case leaf.TableFunc || srcs[0].vtabItem != nil:
		it.kind = nfLeafVtab
		it.tabName = leaf.Table
		it.db = "main"
	case srcs[0].derived != nil:
		it.kind = nfLeafView
		it.tabName = leaf.Table
		it.db = p.nfDBName(srcs[0].dbIdx)
	default:
		it.kind = nfLeafTable
		it.tabName = leaf.Table
		if srcs[0].tbl != nil && srcs[0].tbl.name != "" {
			it.tabName = srcs[0].tbl.name
		}
		it.db = p.nfDBName(srcs[0].dbIdx)
	}
	if srcs[0].catalogScope != scopeAny {
		return false
	}
	for i, c := range ts.cols {
		it.cols = append(it.cols, nfCol{name: c.Name, hidden: c.Hidden})
		it.colls = append(it.colls, c.Collation)
		if c.IsRowidAlias {
			it.ipk = i
		}
	}
	if _, ok := fts3ScopeInfo(p, &ts); ok && ts.tableName != "" {
		// fts3/fts4 declare a hidden column named for the table after the
		// user columns (fts3DeclareVtab), which a MATCH's left side names.
		it.cols = append(it.cols, nfCol{name: ts.tableName, hidden: true})
		it.colls = append(it.colls, "")
	}
	it.visRow = !ts.noRowid && (it.kind == nfLeafTable || it.kind == nfLeafVtab)
	return true
}

// nfDBName is the schema name C prints for a database index.
func (p *ReadOnlyPager) nfDBName(dbIdx int) string {
	if dbIdx == 0 {
		return "main"
	}
	if dbIdx-1 < len(p.attachedReaders) {
		return p.attachedReaders[dbIdx-1].name
	}
	return ""
}

// nfSnapshotCounts saves what resolving a FROM item counts (view expansions,
// CTE references), for a probe that must leave no trace.
func (p *ReadOnlyPager) nfSnapshotCounts() func() {
	type viewCounts struct {
		pager *ReadOnlyPager
		saved map[string]int
	}
	var views []viewCounts
	save := func(rp *ReadOnlyPager) {
		m := make(map[string]int, len(rp.viewRefCount))
		for k, v := range rp.viewRefCount {
			m[k] = v
		}
		views = append(views, viewCounts{rp, m})
	}
	save(p)
	for _, ar := range p.attachedReaders {
		save(ar.pager)
	}
	type cteCount struct {
		b *cteBinding
		n int
	}
	var ctes []cteCount
	for _, frame := range p.cteScopes {
		for _, cb := range frame {
			ctes = append(ctes, cteCount{cb, cb.nUse})
		}
	}
	return func() {
		for _, v := range views {
			v.pager.viewRefCount = v.saved
		}
		for _, c := range ctes {
			c.b.nUse = c.n
		}
		p.lastThrowawayGroup = nil
	}
}

// ---- sqlite3ProcessJoin and the nested selects' expansion ----

// processJoins runs sqlite3ProcessJoin over sel and, first, over every nested
// group in it (selectExpander expands a FROM subquery before its parent's
// joins), then builds each group's result list.
func (b *nfBinder) processJoins(sel *nfSel) {
	for _, it := range sel.items {
		if it.nested != nil {
			b.processJoins(it.nested)
			if b.stop() {
				return
			}
			b.expandNested(it)
			if b.stop() {
				return
			}
		}
	}
	items := sel.items
	for i := 0; i+1 < len(items); i++ {
		right := items[i+1]
		if right.jt&nfJTNatural != 0 {
			if right.isUsing || right.node.hasOn {
				b.fail(semanticf("engine: a NATURAL join may not have an ON or USING clause"))
				return
			}
			var using []string
			for _, c := range right.cols {
				if c.hidden {
					continue
				}
				if nfTableAndColumn(items, 0, i, c.name, true) >= 0 {
					using = append(using, c.name)
				}
			}
			if len(using) > 0 {
				right.using, right.isUsing, right.synthU = using, true, true
			}
		}
		if right.isUsing {
			for _, name := range right.using {
				if nfColumnIndex(right.cols, name) < 0 || nfTableAndColumn(items, 0, i, name, right.synthU) < 0 {
					b.fail(semanticf("engine: cannot join using column %s - column not present in both tables", name))
					return
				}
				if items[0].jt&nfJTLtorj != 0 {
					// With a RIGHT or FULL join in the list, the left operand is
					// the coalesce() of every left table holding the column, each
					// after the first being there through a USING of its own.
					first := nfTableAndColumn(items, 0, i, name, right.synthU)
					for k := nfTableAndColumn(items, first+1, i, name, right.synthU); k >= 0; k = nfTableAndColumn(items, k+1, i, name, right.synthU) {
						if !items[k].isUsing || !nfInList(items[k].using, name) {
							b.fail(semanticf("engine: ambiguous reference to %s in USING()", name))
							return
						}
					}
				}
			}
		}
	}
}

// nfColumnIndex is sqlite3ColumnIndex: the first column of that name, hidden
// or not.
func nfColumnIndex(cols []nfCol, name string) int {
	for i, c := range cols {
		if equalFoldName(c.name, name) {
			return i
		}
	}
	return -1
}

// nfTableAndColumn is tableAndColumnIndex: the first item in [start, end]
// with the column.
func nfTableAndColumn(items []*nfItem, start, end int, name string, ignoreHidden bool) int {
	for i := start; i <= end && i < len(items); i++ {
		if j := nfColumnIndex(items[i].cols, name); j >= 0 && (!ignoreHidden || !items[i].cols[j].hidden) {
			return i
		}
	}
	return -1
}

// expandNested is selectExpander's "*" over an SF_NestedFrom group's own
// FROM list, then sqlite3ColumnsFromExprList for the group's table.
func (b *nfBinder) expandNested(it *nfItem) {
	n := it.nested
	items := n.items
	for i, from := range items {
		zTabName := from.alias
		if zTabName == "" {
			zTabName = from.tabName
		}
		zSchema := ""
		if from.nested == nil {
			zSchema = from.db
			if zSchema == "" {
				zSchema = "*"
			}
		}
		var pUsing []string
		if i+1 < len(items) && items[i+1].isUsing {
			pUsing = items[i+1].using
			for _, u := range pUsing {
				n.elist = append(n.elist, &nfEntry{ename: ".." + u, usingTerm: true, refCol: u})
			}
		}
		nAdd := len(from.cols)
		if from.visRow {
			nAdd++
		}
		for j := 0; j < nAdd; j++ {
			var zName string
			if j == len(from.cols) {
				zName = nfRowidAlias(from.cols)
				if zName == "" {
					continue
				}
			} else {
				zName = from.cols[j].name
				if from.nested != nil && from.nested.elist[j].rowid {
					continue
				}
				if from.cols[j].hidden {
					continue
				}
			}
			e := &nfEntry{refTab: zTabName, refCol: zName}
			if from.nested == nil {
				e.refDB = zSchema
			}
			if from.tabName != "" && strings.HasPrefix(from.tabName, "\x00") && from.alias == "" {
				e.self = from
			}
			if from.nested != nil {
				e.ename = from.nested.elist[j].ename
			} else {
				e.ename = zSchema + "." + zTabName + "." + zName
			}
			e.rowid = j == len(from.cols)
			if (from.isUsing && nfInList(from.using, zName)) || (pUsing != nil && nfInList(pUsing, zName)) ||
				(j < len(from.cols) && from.cols[j].noExpand) {
				e.noExpand = true
			}
			n.elist = append(n.elist, e)
		}
	}
	// sqlite3ColumnsFromExprList over the unresolved list: each entry's name
	// is its last token.
	seen := map[string]*nfEntry{}
	for _, e := range n.elist {
		name := e.refCol
		col := nfCol{}
		cnt := 0
		for {
			collide, ok := seen[r33sFoldIdent(name)]
			if !ok {
				break
			}
			if collide.usingTerm {
				col.noExpand = true
			}
			base := name
			if k := strings.LastIndexByte(base, ':'); k > 0 {
				digits := true
				for _, ch := range base[k+1:] {
					if ch < '0' || ch > '9' {
						digits = false
						break
					}
				}
				if digits {
					base = base[:k]
				}
			}
			cnt++
			name = base + ":" + strconv.Itoa(cnt)
			if cnt > 3 {
				// C draws a random counter after its fourth try; any large
				// one stands for it.
				cnt += 1000000
			}
		}
		if e.noExpand {
			col.noExpand = true
		}
		col.name = name
		seen[r33sFoldIdent(name)] = e
		it.cols = append(it.cols, col)
	}
	it.visRow = false
	it.ipk = -1
}

// nfRowidAlias is sqlite3RowidAlias.
func nfRowidAlias(cols []nfCol) string {
	for _, opt := range [3]string{"_ROWID_", "ROWID", "OID"} {
		if nfColumnIndex(cols, opt) < 0 {
			return opt
		}
	}
	return ""
}

func nfInList(list []string, name string) bool {
	for _, n := range list {
		if equalFoldName(n, name) {
			return true
		}
	}
	return false
}

// ---- star expansion of a statement SELECT ----

// expandStars is selectExpander's "*" and "T.*" over a statement SELECT with
// a group: each becomes the expressions C generates, AS-named as C names
// them, resolved later like any other reference.
func (b *nfBinder) expandStars(sel *nfSel, role nfRole) {
	s := sel.stmt
	hasStar := false
	for _, c := range s.Columns {
		if c.Star {
			hasStar = true
		}
	}
	if !hasStar {
		return
	}
	items := sel.items
	var out []SelectColumn
	for _, sc := range s.Columns {
		if !sc.Star {
			out = append(out, sc)
			continue
		}
		zTName := sc.StarQualifier
		tableSeen := false
		for i, from := range items {
			zTabName := from.alias
			if zTabName == "" {
				zTabName = from.tabName
			}
			zSchema := ""
			if from.nested == nil {
				if zTName != "" && !equalFoldName(zTName, zTabName) {
					continue
				}
				zSchema = from.db
				if zSchema == "" {
					zSchema = "*"
				}
			}
			nAdd := len(from.cols)
			for j := 0; j < nAdd; j++ {
				zName := from.cols[j].name
				if from.nested != nil && from.nested.elist[j].rowid {
					continue
				}
				if zTName != "" && from.nested != nil && !nfMatchENameTab(from.nested.elist[j], zTName) {
					continue
				}
				if from.cols[j].hidden {
					continue
				}
				if from.cols[j].noExpand && zTName == "" {
					continue
				}
				tableSeen = true
				if i > 0 && zTName == "" && from.isUsing && nfInList(from.using, zName) {
					continue
				}
				var ref ColumnExpr
				if len(items) > 1 && (from.jt&nfJTLtorj == 0 || !nfInAnyUsing(zName, items[i+1:])) {
					ref = ColumnExpr{Qualifier: zTabName, Name: zName}
					if zSchema != "" {
						ref.Schema = zSchema
					}
					if strings.HasPrefix(zTabName, "\x00") {
						ref.nfSelf = from
					}
				} else {
					ref = ColumnExpr{Name: zName}
				}
				out = append(out, SelectColumn{Expr: ref, Alias: zName, HasAlias: true, Text: zName, nfStar: true})
			}
		}
		if !tableSeen {
			if zTName != "" {
				b.fail(semanticf("engine: no such table: %s", zTName))
			} else {
				b.fail(semanticf("engine: no tables specified"))
			}
			return
		}
	}
	s.Columns = out
}

// nfInAnyUsing is inAnyUsingClause: whether a later item's USING names zName.
func nfInAnyUsing(zName string, later []*nfItem) bool {
	for _, it := range later {
		if it.jt&nfJTNatural != 0 {
			return true
		}
		if it.isUsing && nfInList(it.using, zName) {
			return true
		}
	}
	return false
}

// nfMatchENameTab is sqlite3MatchEName(pItem, 0, zTab, 0, 0).
func nfMatchENameTab(e *nfEntry, zTab string) bool {
	_, tab, _, ok := nfSplitEName(e.ename)
	if !ok || e.rowid {
		return false
	}
	return equalFoldName(tab, zTab)
}

// nfSplitEName splits a "db.tab.col" span.
func nfSplitEName(s string) (db, tab, col string, ok bool) {
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return "", "", "", false
	}
	rest := s[i+1:]
	j := strings.IndexByte(rest, '.')
	if j < 0 {
		return "", "", "", false
	}
	return s[:i], rest[:j], rest[j+1:], true
}

// nfMatchEName is sqlite3MatchEName.
func nfMatchEName(e *nfEntry, zCol, zTab, zDb string) (match, rowid bool) {
	db, tab, col, ok := nfSplitEName(e.ename)
	if !ok {
		return false, false
	}
	if zDb != "" && !equalFoldName(db, zDb) {
		return false, false
	}
	if zTab != "" && !equalFoldName(tab, zTab) {
		return false, false
	}
	if zCol != "" {
		if !e.rowid && !equalFoldName(col, zCol) {
			return false, false
		}
		if e.rowid && !isRowidAliasName(zCol) {
			return false, false
		}
	}
	return true, e.rowid
}

// ---- lookupName ----

// nfLookup is what lookupName decided for one reference.
type nfLookup struct {
	bind *nfBinding
	// The item and column the reference matched at its own level, for the
	// name C gives a result column (generateColumnNames): a group's renamed
	// column, a leaf's column, -1 for a leaf's rowid.
	matchItem *nfItem
	matchCol  int
	alias     bool // resolved to a result-list alias
	unknown   bool // reached the end of what is known
	coalesced bool // C's coalesce() of a FULL JOIN's USING column
	fallback  bool // no column: a TRUE/FALSE or double-quoted literal
}

// lookup ports lookupName for a reference written (zDb, zTab, zCol) in nc.
func (b *nfBinder) lookup(nc *nfNC, ce ColumnExpr) (nfLookup, error) {
	zDb, zTab, zCol := ce.Schema, ce.Qualifier, ce.Name
	for ; nc != nil; nc = nc.next {
		if nc.unknown {
			return nfLookup{unknown: true}, nil
		}
		cnt, cntTab := 0, 0
		var pMatch *nfItem
		iCol := 0
		var fj []nfFJ
		if nc.sel != nil {
			for _, it := range nc.sel.items {
				if it.nested != nil {
					hit := false
					for j, e := range it.nested.elist {
						if ce.nfSelf != nil {
							break
						}
						ok, bRowid := nfMatchEName(e, zCol, zTab, zDb)
						if !ok {
							continue
						}
						if !bRowid {
							if cnt > 0 {
								switch {
								case !it.isUsing || !nfInList(it.using, zCol) || pMatch == it:
									fj = nil
								case it.jt&nfJTRight == 0:
									continue
								case it.jt&nfJTLeft == 0:
									cnt = 0
									fj = nil
								default:
									fj = append(fj, nfFJ{pMatch, iCol})
								}
							}
							cnt++
							hit = true
						} else if cnt > 0 {
							continue
						}
						cntTab++
						pMatch = it
						iCol = j
						if e.usingTerm {
							break
						}
					}
					if hit || zTab == "" {
						continue
					}
				}
				if zTab != "" {
					if ce.nfSelf != nil {
						if ce.nfSelf != it {
							continue
						}
					} else {
						if zDb != "" {
							if zDb == "*" {
								if it.db != "" {
									continue
								}
							} else if it.nested != nil || !equalFoldName(it.db, zDb) {
								continue
							}
						}
						if it.alias != "" {
							if !equalFoldName(zTab, it.alias) {
								continue
							}
						} else if strings.HasPrefix(it.tabName, "\x00") || !equalFoldName(zTab, it.tabName) {
							continue
						}
					}
				}
				if j := nfColumnIndex(it.cols, zCol); j >= 0 {
					if cnt > 0 {
						switch {
						case !it.isUsing || !nfInList(it.using, zCol):
							fj = nil
						case it.jt&nfJTRight == 0:
							continue
						case it.jt&nfJTLeft == 0:
							cnt = 0
							fj = nil
						default:
							fj = append(fj, nfFJ{pMatch, iCol})
						}
					}
					cnt++
					pMatch = it
					if j == it.ipk {
						iCol = -1
					} else {
						iCol = j
					}
				}
				if cnt == 0 && it.visRow {
					cntTab++
					pMatch = it
				}
			}
		}
		if cnt == 0 && cntTab >= 1 && pMatch != nil && isRowidAliasName(zCol) && (pMatch.visRow || pMatch.nested != nil) {
			cnt = cntTab
			if pMatch.nested == nil {
				iCol = -1
			}
		}
		if cnt == 0 && zTab == "" && nc.aliases != nil && nc.aliases[r33sFoldIdent(zCol)] {
			return nfLookup{alias: true}, nil
		}
		if cnt == 0 {
			continue
		}
		if cnt != 1 {
			if fj != nil && len(fj) == cnt-1 {
				// A FULL JOIN's USING column: coalesce() over every match, in
				// the order they were met.
				fj = append(fj, nfFJ{pMatch, iCol})
				parts := make([]*nfBinding, len(fj))
				for k, m := range fj {
					pb, err := b.bindingOf(m.item, m.col)
					if err != nil {
						return nfLookup{}, err
					}
					parts[k] = pb
				}
				return nfLookup{bind: nfCoalesce(parts), coalesced: true}, nil
			}
			return nfLookup{}, nfNameErr("ambiguous column name", zDb, zTab, zCol)
		}
		bind, err := b.bindingOf(pMatch, iCol)
		if err != nil {
			return nfLookup{}, err
		}
		return nfLookup{bind: bind, matchItem: pMatch, matchCol: iCol}, nil
	}
	if zTab == "" && ce.FallbackLiteral != nil {
		return nfLookup{fallback: true}, nil
	}
	return nfLookup{}, nfNameErr("no such column", zDb, zTab, zCol)
}

type nfFJ struct {
	item *nfItem
	col  int
}

func nfNameErr(what, zDb, zTab, zCol string) error {
	switch {
	case zDb != "":
		return semanticf("engine: %s: %s.%s.%s", what, zDb, zTab, zCol)
	case zTab != "":
		return semanticf("engine: %s: %s.%s", what, zTab, zCol)
	}
	return semanticf("engine: %s: %s", what, zCol)
}

// bindingOf follows a match on item column col (-1 for a leaf's rowid) down
// to the leaf.
func (b *nfBinder) bindingOf(it *nfItem, col int) (*nfBinding, error) {
	if it.nested == nil {
		return &nfBinding{item: it, col: col}, nil
	}
	if col < 0 || col >= len(it.nested.elist) {
		return nil, errNFAbstain
	}
	e := it.nested.elist[col]
	if e.bind == nil {
		return nil, errNFAbstain
	}
	return e.bind, nil
}

// ---- resolution of a statement SELECT ----

// resolveSelect resolves sel's statement SELECT in resolveSelectStep's order
// and rewrites it.
func (b *nfBinder) resolveSelect(sel *nfSel, role nfRole) {
	s := sel.stmt
	outer := sel.outer
	// The FROM clause's subqueries first: each group's own list and joins
	// (its WHERE is its ON clauses), then each subquery item.
	for _, it := range sel.items {
		b.resolveItem(it, outer)
		if b.stop() {
			return
		}
	}
	nc := &nfNC{sel: sel, next: outer}
	// The result list.
	for i := range s.Columns {
		col := &s.Columns[i]
		if col.Star {
			// A "*" in a SELECT without a group: the engine expands it.
			continue
		}
		var lk nfLookup
		col.Expr = b.rewriteExpr(col.Expr, nc, &lk)
		if b.stop() {
			return
		}
		if sel.managed && !col.HasAlias && !col.nfStar {
			if ce, ok := s.Columns[i].Expr.(ColumnExpr); ok && lk.bind != nil {
				if lk.coalesced {
					// A TK_FUNCTION: named by its text (generateColumnNames'
					// zEName span).
					col.cName = col.Text
				} else {
					col.cName = b.resultName(ce, lk, role)
				}
			}
		}
	}
	// NC_UEList: the result list's AS names.
	aliases := map[string]bool{}
	for _, c := range s.Columns {
		if c.HasAlias {
			aliases[r33sFoldIdent(c.Alias)] = true
		}
	}
	nc.aliases = aliases
	s.Having = b.rewriteExpr(s.Having, nc, nil)
	if b.stop() {
		return
	}
	s.Where = b.rewriteExpr(s.Where, nc, nil)
	if b.stop() {
		return
	}
	// sqlite3ProcessJoin appended every ON to the WHERE, in FROM order.
	for _, it := range sel.items {
		b.rewriteOn(it, nc)
		if b.stop() {
			return
		}
	}
	// Table-valued function arguments.
	for _, it := range sel.items {
		if it.leaf != nil && it.leaf.TableFunc {
			for k := range it.leaf.TableFuncArgs {
				it.leaf.TableFuncArgs[k] = b.rewriteExpr(it.leaf.TableFuncArgs[k], nc, nil)
			}
		}
	}
	if b.stop() {
		return
	}
	// Named windows, read where their functions use them.
	for i := range s.Windows {
		if sp := s.Windows[i].Spec; sp != nil {
			for k := range sp.PartitionBy {
				sp.PartitionBy[k] = b.rewriteExpr(sp.PartitionBy[k], nc, nil)
			}
			for k := range sp.OrderBy {
				sp.OrderBy[k].Expr = b.rewriteExpr(sp.OrderBy[k].Expr, nc, nil)
			}
		}
	}
	// ORDER BY then GROUP BY: an AS name or an integer is not a reference.
	for i := range s.OrderBy {
		e := s.OrderBy[i].Expr
		if nfOrderTermIsName(e, aliases) || nfIsIntLiteral(e) {
			continue
		}
		s.OrderBy[i].Expr = b.rewriteExpr(e, nc, nil)
		if b.stop() {
			return
		}
	}
	for i := range s.GroupBy {
		if nfIsIntLiteral(s.GroupBy[i]) {
			continue
		}
		s.GroupBy[i] = b.rewriteExpr(s.GroupBy[i], nc, nil)
		if b.stop() {
			return
		}
	}
	if sel.managed {
		b.usingToOn(sel)
		if b.stop() {
			return
		}
		b.renameLeaves(sel)
	}
}

// usingToOn codes each USING or NATURAL join of a managed SELECT, and of the
// groups in it, as the equalities sqlite3ProcessJoin builds for it: the left
// operand is the first item to the left holding the column
// (tableAndColumnIndex), the right one the joined item's, both bound. The
// engine is left no USING of its own to resolve.
func (b *nfBinder) usingToOn(sel *nfSel) {
	for _, it := range sel.items {
		if it.nested != nil {
			b.usingToOn(it.nested)
			if b.stop() {
				return
			}
		}
	}
	items := sel.items
	for i := 0; i+1 < len(items); i++ {
		right := items[i+1]
		if right.jt&nfJTNatural == 0 && !right.isUsing {
			continue
		}
		holder := b.onHolder(right)
		if holder == nil || holder.On != nil {
			b.giveUp("a USING join whose holder already has an ON")
			return
		}
		var on Expr
		for _, name := range right.using {
			li := nfTableAndColumn(items, 0, i, name, right.synthU)
			rc := nfColumnIndex(right.cols, name)
			if li < 0 || rc < 0 {
				b.giveUp("a USING column missing at bind time")
				return
			}
			lb, lerr := b.bindingOf(items[li], nfColumnIndex(items[li].cols, name))
			if lerr == nil && items[0].jt&nfJTLtorj != 0 {
				parts := []*nfBinding{lb}
				for k := nfTableAndColumn(items, li+1, i, name, right.synthU); k >= 0 && lerr == nil; k = nfTableAndColumn(items, k+1, i, name, right.synthU) {
					var pb *nfBinding
					pb, lerr = b.bindingOf(items[k], nfColumnIndex(items[k].cols, name))
					parts = append(parts, pb)
				}
				if lerr == nil {
					lb = nfCoalesce(parts)
				}
			}
			rb, rerr := b.bindingOf(right, rc)
			if lerr != nil || rerr != nil {
				b.giveUp("a USING operand that does not bind")
				return
			}
			eq := BinaryExpr{Op: "=", L: b.refFor(ColumnExpr{Name: name}, lb), R: b.refFor(ColumnExpr{Name: name}, rb)}
			if on == nil {
				on = eq
			} else {
				on = BinaryExpr{Op: "AND", L: on, R: eq}
			}
		}
		holder.On, holder.Using, holder.Natural = on, nil, false
	}
}

func nfOrderTermIsName(e Expr, aliases map[string]bool) bool {
	ce, ok := e.(ColumnExpr)
	return ok && ce.Qualifier == "" && ce.Schema == "" && aliases[r33sFoldIdent(ce.Name)]
}

func nfIsIntLiteral(e Expr) bool {
	lit, ok := e.(LiteralExpr)
	return ok && lit.Val.Typ == Int
}

// resolveItem resolves one FROM item's subquery: a group's result list and
// ON clauses, or a subquery item's statement.
func (b *nfBinder) resolveItem(it *nfItem, outer *nfNC) {
	if it.nested != nil {
		n := it.nested
		for _, child := range n.items {
			b.resolveItem(child, outer)
			if b.stop() {
				return
			}
		}
		nc := &nfNC{sel: n, next: outer}
		for _, e := range n.elist {
			ref := ColumnExpr{Schema: e.refDB, Qualifier: e.refTab, Name: e.refCol, nfSelf: e.self}
			if e.usingTerm {
				ref = ColumnExpr{Name: e.refCol}
			}
			if e.refDB == "*" {
				ref.Schema = "*"
			}
			lk, err := b.lookup(nc, ref)
			if err != nil {
				b.failOrAbstain(err)
				return
			}
			if lk.bind == nil {
				b.giveUp("a group entry that binds nowhere known")
				return
			}
			e.bind = lk.bind
		}
		for _, child := range n.items {
			b.rewriteOn(child, nc)
			if b.stop() {
				return
			}
		}
		return
	}
	if it.leaf != nil && it.leaf.Subquery != nil {
		b.selectStmt(it.leaf.Subquery, outer, nfRoleSubquery)
	}
}

// rewriteOn rewrites the ON clause a node carries, held on the FromItem that
// starts it.
func (b *nfBinder) rewriteOn(it *nfItem, nc *nfNC) {
	if !it.node.hasOn {
		return
	}
	holder := b.onHolder(it)
	if holder == nil || holder.On == nil {
		b.giveUp("an ON clause whose holder was not found")
		return
	}
	holder.On = b.rewriteExpr(holder.On, nc, nil)
}

// onHolder is the FromItem whose On is the node's: the item where the node
// starts, which for a group is its first leaf, or the stand-in holding it.
func (b *nfBinder) onHolder(it *nfItem) *FromItem {
	first := it
	for first.nested != nil {
		if len(first.nested.items) == 0 {
			return nil
		}
		first = first.nested.items[0]
	}
	if first.leaf == nil {
		return nil
	}
	// The stand-in of a re-wrapped group carries the outward join; its span
	// starts with the leaf. Otherwise the leaf itself does.
	stmt := b.stmtOf(it)
	if stmt == nil {
		return nil
	}
	var find func(items []FromItem) *FromItem
	find = func(items []FromItem) *FromItem {
		for i := range items {
			if items[i].NestedGroupSpan != nil && nfSpanStartsWith(items[i].NestedGroupSpan, first.cID) {
				return &items[i]
			}
			if items[i].cID == first.cID {
				return &items[i]
			}
			if h := find(items[i].NestedGroupSpan); h != nil {
				return h
			}
		}
		return nil
	}
	return find(stmt.From)
}

func nfSpanStartsWith(span []FromItem, cID int) bool {
	for len(span) > 0 {
		if span[0].cID == cID {
			return true
		}
		if span[0].NestedGroupSpan == nil {
			return false
		}
		span = span[0].NestedGroupSpan
	}
	return false
}

// stmtOf is the statement SELECT an item's FROM list belongs to.
func (b *nfBinder) stmtOf(it *nfItem) *SelectStmt { return it.sel.owner }

func (b *nfBinder) failOrAbstain(err error) {
	if errors.Is(err, errNFAbstain) {
		b.giveUp(err.Error())
		return
	}
	b.fail(err)
}

// rewriteExpr resolves every reference in e against nc and rewrites the ones
// bound to a renamed leaf; lk, when non-nil, receives a bare reference's
// lookup.
func (b *nfBinder) rewriteExpr(e Expr, nc *nfNC, lk *nfLookup) Expr {
	if e == nil || b.stop() {
		return e
	}
	fn := func(ce ColumnExpr) Expr {
		if b.stop() {
			return ce
		}
		if isNFSynthAlias(ce.Qualifier) {
			return ce
		}
		r, err := b.lookup(nc, ce)
		if err != nil {
			b.failOrAbstain(err)
			return ce
		}
		if lk != nil {
			*lk = r
		}
		if r.bind == nil {
			return ce
		}
		return b.refFor(ce, r.bind)
	}
	sub := func(s *SelectStmt) *SelectStmt {
		b.selectStmt(s, nc, nfRoleExpr)
		return s
	}
	return nfMapExpr(e, fn, sub)
}

// refFor is the reference a binding becomes: by synthetic alias for a leaf
// of a managed SELECT, unchanged otherwise.
func (b *nfBinder) refFor(ce ColumnExpr, bind *nfBinding) Expr {
	if bind.rest != nil {
		first, ok := b.refFor(ce, &nfBinding{item: bind.item, col: bind.col}).(ColumnExpr)
		if !ok || first.Qualifier == "" || !isNFSynthAlias(first.Qualifier) {
			b.giveUp("a coalesce whose first operand is not renamed")
			return ce
		}
		others := make([]ColumnExpr, len(bind.rest))
		for k, r := range bind.rest {
			o, ok := b.refFor(ce, r).(ColumnExpr)
			if !ok || !isNFSynthAlias(o.Qualifier) || r.col < 0 {
				b.giveUp("a coalesce operand that is not renamed")
				return ce
			}
			others[k] = o
		}
		if bind.col < 0 || b.collated(bind) {
			// A coalesce() compares under BINARY whatever its columns
			// declare; not reproduced, so not covered.
			b.giveUp("a coalesce over a rowid or a collated column")
			return ce
		}
		first.coalesceOf = &others
		return first
	}
	it := bind.item
	if it.synth == "" {
		ce.nfSelf = nil
		return ce
	}
	out := ColumnExpr{Qualifier: it.synth}
	if bind.col < 0 && it.ipk >= 0 {
		// The rowid of a table with an INTEGER PRIMARY KEY is that column.
		bind = &nfBinding{item: it, col: it.ipk}
	}
	if bind.col < 0 {
		name := ""
		for _, opt := range [3]string{"rowid", "_rowid_", "oid"} {
			if nfColumnIndex(it.cols, opt) < 0 {
				name = opt
				break
			}
		}
		if name == "" {
			b.giveUp("a rowid whose every spelling is a column")
			return ce
		}
		out.Name = name
		return out
	}
	out.Name = it.cols[bind.col].name
	// The column must be the first of its name for the engine to find it
	// by name.
	if nfColumnIndex(it.cols, out.Name) != bind.col {
		b.giveUp("a column that is not the first of its name")
		return ce
	}
	return out
}

// collated reports whether any column of a coalesce binding declares a
// collating sequence other than BINARY.
func (b *nfBinder) collated(bind *nfBinding) bool {
	parts := append([]*nfBinding{{item: bind.item, col: bind.col}}, bind.rest...)
	for _, p := range parts {
		if p.col >= 0 && p.col < len(p.item.colls) && p.item.colls[p.col] != "" && !equalFoldName(p.item.colls[p.col], "BINARY") {
			return true
		}
	}
	return false
}

// resultName is the name C gives an unaliased result column that is a
// column reference.
func (b *nfBinder) resultName(written ColumnExpr, lk nfLookup, role nfRole) string {
	if role == nfRoleSubquery {
		return written.Name
	}
	it := lk.matchItem
	if it == nil {
		return written.Name
	}
	col := lk.matchCol
	if col < 0 {
		col = it.ipk
	}
	if col < 0 {
		return "rowid"
	}
	if col >= len(it.cols) {
		return written.Name
	}
	return it.cols[col].name
}

// renameLeaves gives every leaf of a managed SELECT its synthetic alias, and
// drops what the "*" left behind.
func (b *nfBinder) renameLeaves(sel *nfSel) {
	var walk func(n *nfSel)
	walk = func(n *nfSel) {
		for _, it := range n.items {
			if it.leaf != nil && it.synth != "" {
				it.leaf.Alias = it.synth
			}
			if it.nested != nil {
				walk(it.nested)
			}
		}
	}
	walk(sel)
	var clear func(items []FromItem)
	clear = func(items []FromItem) {
		for i := range items {
			items[i].GroupRebuildStarred, items[i].GroupRebuildQualStarred = false, false
			clear(items[i].NestedGroupSpan)
		}
	}
	clear(sel.stmt.From)
}
