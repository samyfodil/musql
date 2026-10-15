package engine

import (
	"fmt"
	"strings"
)

// r41SequentialCompound reports if a compound select is UNION ALL with no ORDER BY (select.c:2989).
func r41SequentialCompound(stmt *SelectStmt) bool {
	if len(stmt.OrderBy) != 0 {
		return false
	}
	for _, a := range stmt.Compound {
		if a.Op != "UNION ALL" {
			return false
		}
	}
	return true
}

// r41FlattenProjection flattens a simple derived table that projects columns from
// a single ordinary table (flattenSubquery, select.c:4290). Moves the table into the
// parent FROM and substitutes references, letting the parent's WHERE/ORDER BY drive
// the plan (select.c:3797). Declines on naming conflicts or subqueries.
func (p *ReadOnlyPager) r41FlattenProjection(stmt *SelectStmt) *SelectStmt {
	return p.r41Flatten(spliceMatchGroups(stmt), true)
}

// spliceMatchGroups splices in flat a parenthesized join group holding an fts
// MATCH in its own ON clause, where C flattens the group. An aliased or
// non-leading group is an SF_NestedFrom subquery "SELECT * FROM <group>" to
// C (parse.y), and flattenSubquery takes it into an inner join like any
// subquery: restriction (3a) bars only an outer join's right operand, and
// (27a) a first member that is a RIGHT JOIN's left operand. Once
// nested_from_bind.go has bound every name in the statement to a member by a
// synthetic alias (nfBound), the group's scope has done its work, and the
// spliced FROM clause is the one C plans; the group's own ON becomes its first
// member's, an inner join's ON, which is a WHERE term to C. Only a group
// holding a MATCH is taken: there the group's own cursor cannot read the fts
// table's rowid, so the MATCH is otherwise refused.
func spliceMatchGroups(stmt *SelectStmt) *SelectStmt {
	if stmt == nil || !stmt.nfBound || len(stmt.Windows) != 0 || selectHasWindow(stmt) {
		return stmt
	}
	out := stmt
	for i := 0; i < len(out.From); i++ {
		gl := out.From[i].GroupLen
		if gl <= 1 {
			continue
		}
		if i+gl > len(out.From) {
			return out
		}
		if !matchGroupSplicable(out.From[i : i+gl]) {
			if s, ok := matchGroupAsSubquery(out, i, gl); ok {
				out = s
				continue
			}
			i += gl - 1
			continue
		}
		cp := *out
		cp.From = append([]FromItem(nil), out.From...)
		for k := i; k < i+gl; k++ {
			m := &cp.From[k]
			m.GroupLen, m.GroupAlias, m.HasGroupAlias, m.GroupRebuildID = 0, "", false, 0
			m.NestFromWrapDepth, m.NestedNonLeading, m.NestedGroupID = 0, false, 0
		}
		out = &cp
		i += gl - 1
	}
	return out
}

// matchGroupAsSubquery is the group at stmt.From[i:i+gl] as C runs it when it
// does not flatten it, the right operand of an outer join (restriction (3a)):
// the subquery "SELECT ... FROM <group>" under a synthetic alias, every
// reference to a member outside the group now one of its result columns. The
// members' synthetic aliases are unique in the statement (nfBound), so a
// reference is rewritten wherever it is, subqueries included. false for any
// other group, or a reference this does not rewrite.
func matchGroupAsSubquery(stmt *SelectStmt, i, gl int) (*SelectStmt, bool) {
	g := stmt.From[i : i+gl]
	if g[0].Join != JoinLeft || len(g[0].Using) != 0 || g[0].Natural {
		return nil, false
	}
	inner := append([]FromItem(nil), g...)
	inner[0].Join, inner[0].On, inner[0].CrossKeyword = JoinInner, nil, false
	if !matchGroupSplicable(inner) {
		return nil, false
	}
	for k := range inner {
		m := &inner[k]
		m.GroupLen, m.GroupAlias, m.HasGroupAlias, m.GroupRebuildID = 0, "", false, 0
		m.NestFromWrapDepth, m.NestedNonLeading, m.NestedGroupID = 0, false, 0
	}
	synth := map[string]bool{}
	for _, m := range g {
		if !isNFSynthAlias(m.Alias) {
			return nil, false
		}
		synth[m.Alias] = true
	}
	alias := ""
	for n := 1; ; n++ {
		alias = fmt.Sprintf("%sgroup%d", nfMarker, n)
		taken := false
		for _, it := range stmt.From {
			if it.Alias == alias {
				taken = true
			}
		}
		if !taken {
			break
		}
	}
	body := &SelectStmt{nfBound: true}
	cols := map[string]string{}
	ref := func(ce ColumnExpr) (Expr, bool) {
		if !synth[ce.Qualifier] {
			return ce, true
		}
		if ce.Schema != "" || ce.coalesceOf != nil || ce.nfSelf != nil {
			return nil, false
		}
		key := ce.Qualifier + "\x00" + strings.ToLower(ce.Name)
		name, ok := cols[key]
		if !ok {
			name = fmt.Sprintf("%sc%d", nfMarker, len(cols)+1)
			cols[key] = name
			body.Columns = append(body.Columns, SelectColumn{Expr: ColumnExpr{Qualifier: ce.Qualifier, Name: ce.Name},
				Alias: name, HasAlias: true, Text: name, RawText: name})
		}
		return ColumnExpr{Qualifier: alias, Name: name}, true
	}
	var rwSel func(s *SelectStmt) (*SelectStmt, bool)
	rw := func(e Expr) (Expr, bool) { return r41RewriteS(e, ref, rwSel) }
	rwSel = func(s *SelectStmt) (*SelectStmt, bool) {
		if s == nil {
			return nil, true
		}
		if len(s.CTEs) != 0 || len(s.Windows) != 0 {
			return nil, false
		}
		out := *s
		ok := true
		one := func(e Expr) Expr {
			if !ok || e == nil {
				return e
			}
			n, eok := rw(e)
			ok = ok && eok
			return n
		}
		out.Columns = make([]SelectColumn, len(s.Columns))
		for k, c := range s.Columns {
			if c.Star {
				if c.StarQualifier != "" && synth[c.StarQualifier] {
					return nil, false
				}
			} else {
				c.Expr = one(c.Expr)
			}
			out.Columns[k] = c
		}
		out.From = append([]FromItem(nil), s.From...)
		for k := range out.From {
			out.From[k].On = one(out.From[k].On)
			if out.From[k].TableFuncArgs != nil {
				args := make([]Expr, len(out.From[k].TableFuncArgs))
				for j, a := range out.From[k].TableFuncArgs {
					args[j] = one(a)
				}
				out.From[k].TableFuncArgs = args
			}
			if out.From[k].Subquery != nil && ok {
				q, qok := rwSel(out.From[k].Subquery)
				ok = ok && qok
				out.From[k].Subquery = q
			}
		}
		out.Where, out.Having = one(s.Where), one(s.Having)
		if s.GroupBy != nil {
			out.GroupBy = make([]Expr, len(s.GroupBy))
			for k, e := range s.GroupBy {
				out.GroupBy[k] = one(e)
			}
		}
		if s.OrderBy != nil {
			out.OrderBy = make([]OrderTerm, len(s.OrderBy))
			for k, ot := range s.OrderBy {
				ot.Expr = one(ot.Expr)
				out.OrderBy[k] = ot
			}
		}
		out.LimitParam, out.OffsetParam = one(s.LimitParam), one(s.OffsetParam)
		if s.Compound != nil {
			out.Compound = make([]CompoundArm, len(s.Compound))
			for k, a := range s.Compound {
				if ok {
					q, qok := rwSel(a.Stmt)
					ok = ok && qok
					a.Stmt = q
				}
				out.Compound[k] = a
			}
		}
		return &out, ok
	}
	// The statement without the group's members, which the subquery keeps.
	outer := *stmt
	outer.From = append(append(append([]FromItem(nil), stmt.From[:i]...),
		FromItem{Alias: alias, Join: g[0].Join, On: g[0].On}), stmt.From[i+gl:]...)
	rewritten, ok := rwSel(&outer)
	if !ok || len(stmt.CTEs) != 0 {
		return nil, false
	}
	if len(body.Columns) == 0 {
		body.Columns = []SelectColumn{{Expr: LiteralExpr{Val: Value{Typ: Int, I: 1}}, Alias: nfMarker + "c0", HasAlias: true}}
	}
	body.From = inner
	rewritten.From[i].Subquery = body
	return rewritten, true
}

// matchGroupSplicable reports a group spliceMatchGroups takes: joined by an
// inner join, its members ordinary items with no group of their own and no
// RIGHT or FULL join, and some member's ON holding a MATCH.
func matchGroupSplicable(g []FromItem) bool {
	if (g[0].Join != JoinInner && g[0].Join != JoinCross) || len(g[0].Using) != 0 || g[0].Natural {
		return false
	}
	match := false
	for k, m := range g {
		if m.NestedGroupSpan != nil || (k > 0 && m.GroupLen != 0) || m.Join == JoinRight || m.Join == JoinFull {
			return false
		}
		if k == 0 || m.On == nil {
			continue
		}
		if !r41Walk(m.On, func(e Expr) {
			if _, ok := e.(MatchExpr); ok {
				match = true
			}
		}) {
			return false
		}
	}
	return match
}

// r41Flatten repeats the single-item flatten until nothing more merges, which
// is flattenSubquery's own loop: sqlite3Select restarts its FROM walk after
// every success (select.c:7882, "i = -1"). expand is whether a FROM item naming
// a view or a CTE may be expanded into its body; only the outermost call may,
// because only it is in the scope those names resolve in.
func (p *ReadOnlyPager) r41Flatten(stmt *SelectStmt, expand bool) *SelectStmt {
	if p == nil || stmt == nil || len(stmt.From) == 0 || p.fullColumnNames || p.shortColumnNamesOff {
		return stmt
	}
	ok, left := r41ParentOKLeft(stmt)
	if !ok {
		return stmt
	}
	out := stmt
	for range len(stmt.From) {
		merged := false
		for i := range out.From {
			if left && out.From[i].Join != JoinLeft {
				// Beside a LEFT JOIN only that join's own right operand is
				// flattened here, and only the one r41FlattenItem's outer arm
				// takes; see r41ParentOKLeft.
				continue
			}
			if f, ok := p.r41FlattenItem(out, i, expand); ok {
				out, merged = f, true
				break
			}
			if f, ok := p.r41FlattenMultiItem(out, i, expand); ok {
				out, merged = f, true
				break
			}
		}
		if !merged || len(out.Compound) != 0 {
			break
		}
	}
	return out
}

// r41ParentOKLeft is r41ParentOK, except that a LEFT JOIN is let through
// (left reports one): a subquery that is the right operand of a LEFT JOIN is
// flattened by C too (flattenSubquery's isOuterJoin, select.c:4371), and
// r41FlattenItem takes that one shape where its body is not deterministic and
// keeping it unflattened would therefore be a wrong answer.
func r41ParentOKLeft(stmt *SelectStmt) (ok, left bool) {
	cp := *stmt
	cp.From = append([]FromItem(nil), stmt.From...)
	for k := range cp.From {
		if cp.From[k].Join == JoinLeft {
			left = true
			cp.From[k].Join = JoinInner
		}
	}
	return r41ParentOK(&cp), left
}

// r41ParentOK is every restriction on the PARENT statement that does not
// depend on which FROM item is being flattened.
func r41ParentOK(stmt *SelectStmt) bool {
	// FromParenthesized is not one: it only scopes errIfDuplicateOutputNames
	// (query.go) to a parenthesized FROM, and every derived table sets it.
	if len(stmt.Compound) != 0 || len(stmt.Windows) != 0 || stmt.ValuesArms != 0 ||
		stmt.FromNestedParenJoin {
		return false
	}
	for _, it := range stmt.From {
		if it.Join != JoinCross && it.Join != JoinInner {
			return false
		}
		// A table-valued function sits beside the flattened subquery as any
		// table does (its arguments are substituted like an ON clause,
		// substSelect's pFuncArg arm); only one whose columns r41ItemNames
		// reads.
		if (it.TableFunc || len(it.TableFuncArgs) != 0) && r41TVFColumns(it) == nil {
			return false
		}
		if len(it.Using) != 0 || it.Natural ||
			it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias || it.GroupRebuildID != 0 ||
			it.NestFromWrapDepth != 0 || len(it.NestedGroupSpan) != 0 || it.NestedNonLeading ||
			it.NestedGroupID != 0 || it.UpdateTarget {
			return false
		}
	}
	for _, c := range stmt.Columns { // (25)
		if !c.Star && exprHasWindow(c.Expr) {
			return false
		}
	}
	for _, ot := range stmt.OrderBy { // (25)
		if exprHasWindow(ot.Expr) {
			return false
		}
	}
	return !r37cMayReferenceRowid(stmt)
}

// r41DerivedCol is one result column of a projection body: the name the
// derived table exposes it under and the base-table column it reads.
type r41DerivedCol struct {
	name string
	base string
	// expr, when non-nil, is a result column that is not a bare column,
	// rewritten onto the base table's name in the parent: substExpr copies it
	// into every reference (select.c:3797). Only a body calling random() or
	// randomblob() is flattened with one (r41Arm), since only there is the copy
	// observable -- each copy is evaluated on its own.
	expr Expr
	// colRef marks an expr that is a bare column of a body item: its copy
	// keeps the column's own collation and affinity (substExpr adds no
	// COLLATE when pNat==pColl), so it is not an expression column.
	colRef bool
}

// r41Flat is one flatten in progress.
type r41Flat struct {
	dName   string // the derived table's scope name, "" when unaliased
	alias   string // the scope name the base table gets in the parent
	cols    []r41DerivedCol
	byName  map[string]int    // folded derived name -> index into cols
	base    map[string]string // folded base column name -> declared name
	others  map[string]bool   // folded column names another FROM item exposes
	scopes  map[string]bool   // folded scope names of the other FROM items
	aliases map[string]bool   // folded explicit aliases of the parent's result columns

	// aliasExpr is each explicit parent alias's result expression, already
	// substituted (nil for one that holds an aggregate or window function), and
	// aliasOn says the clause being rewritten can see it -- see parentRef.
	aliasExpr map[string]Expr
	aliasOn   bool

	// outerRowid, when not "", flattens the right operand of a LEFT JOIN:
	// an expression column's copy reads NULL on the NULL row (TK_IF_NULL_ROW),
	// tested through this name of the base table's rowid.
	outerRowid string

	// p reads the names a subquery's own FROM clause binds (subqueryUntouched).
	p *ReadOnlyPager
}

func (p *ReadOnlyPager) r41FlattenItem(stmt *SelectStmt, i int, expand bool) (*SelectStmt, bool) {
	it := stmt.From[i]
	if it.Subquery == nil {
		if !expand {
			return nil, false
		}
		ex, ok := p.r38cExpandCTERef(it)
		if !ok {
			ex, ok = p.r37cExpandViewRef(it)
		}
		if !ok {
			return nil, false
		}
		it = ex
	}
	if it.IndexedBy != "" || it.NotIndexed {
		return nil, false
	}
	body := it.Subquery
	// tag-select-0230 (select.c:7811-7848): a body's ORDER BY that cannot
	// accomplish anything -- the parent has an ORDER BY of its own, or the body
	// is one item of a join -- is DELETED before flattening is even tried,
	// unless a LIMIT needs it or the parent aggregates with something other
	// than count()/min()/max() (SF_OrderByReqd, resolve.c:1366-1371 and 1982).
	if len(body.OrderBy) != 0 && (len(stmt.OrderBy) != 0 || len(stmt.From) > 1) &&
		body.Limit == nil && body.LimitParam == nil && !r41OrderAgg(stmt) {
		cp := *body
		cp.OrderBy = nil
		body = &cp
	}
	if it.Join == JoinLeft {
		// isOuterJoin (select.c:4371-4381): a body of one table, a parent
		// that is not DISTINCT. A parent WHERE may first turn the join into an
		// inner one (OUTER JOIN STRENGTH REDUCTION, tag-select-0220), and C
		// then flattens it as an inner join; flattening it as an outer one
		// here gives the same rows: the WHERE that would have reduced it
		// rejects every NULL row, and the compiler reduces the flattened join
		// itself (wherePlanReduceOuterJoins). An expression column's copy
		// reads its value on every row that is not a NULL row either way.
		if !p.cVolatile(body) || len(body.Compound) != 0 || stmt.Distinct ||
			len(it.Using) != 0 || it.Natural {
			return nil, false
		}
	}
	if len(body.Compound) != 0 {
		return p.r41FlattenCompound(stmt, i, it, body)
	}
	if len(body.OrderBy) != 0 {
		// A surviving body ORDER BY moves onto the parent (select.c:4640-4658),
		// which restrictions (11) and (16) guard, and it keeps the body from
		// being flattened at all when the parent's result set is "complex"
		// (select.c:7869-7876: SF_ComplexResult, a function or subquery anywhere
		// in the result list, select.c:6342).
		if len(stmt.OrderBy) != 0 || selectIsAggregateQuery(stmt) || i != 0 || r41ComplexResult(stmt) {
			return nil, false
		}
	}
	a, ok := p.r41Arm(stmt, i, it, body, nil)
	if !ok {
		return nil, false
	}
	if it.Join == JoinLeft {
		if a.baseTbl == nil {
			return nil, false
		}
		// TK_IF_NULL_ROW (substExpr, select.c:3830) is spelled as a CASE on
		// the base table's rowid, which only the NULL row has NULL; under a
		// name no column of the table shadows. A WITHOUT ROWID table's first
		// PRIMARY KEY column serves instead: it is NOT NULL there.
		if a.baseTbl.withoutRowid {
			if len(a.baseTbl.pkColIdx) == 0 {
				return nil, false
			}
			a.f.outerRowid = a.baseTbl.cols[a.baseTbl.pkColIdx[0]].Name
		}
		for _, name := range []string{"rowid", "_rowid_", "oid"} {
			if a.f.outerRowid != "" {
				break
			}
			if _, shadowed := a.f.base[name]; !shadowed {
				a.f.outerRowid = name
				break
			}
		}
		if a.f.outerRowid == "" {
			return nil, false
		}
	}
	return a.parent(stmt, i, it)
}

// r41ArmFlat is one projection body analysed for flattening: its derived
// columns, its base table, and its WHERE and ORDER BY rewritten onto the name
// the base table takes in the parent.
type r41ArmFlat struct {
	f     *r41Flat
	b     FromItem
	where Expr
	order []OrderTerm
	// p and baseTbl are the pager and b's table, for collOf.
	p       *ReadOnlyPager
	baseTbl *resolvedTable
	// multi, when non-nil, is a body of several FROM items
	// (r41FlattenMultiItem), spliced in where the subquery was instead of b.
	multi []FromItem
}

// r41Arm analyses body, the subquery of stmt's FROM item i (it), as a
// projection of one ordinary table. names, when non-nil, are the derived
// column names -- a compound's come from its leftmost arm alone.
func (p *ReadOnlyPager) r41Arm(stmt *SelectStmt, i int, it FromItem, body *SelectStmt, names []string) (*r41ArmFlat, bool) {
	// A body that reads a subquery of its own flattens that one first -- the
	// same end state C reaches by flattening outermost-first and restarting.
	if len(body.From) == 1 && body.From[0].Subquery != nil {
		body = p.r41Flatten(body, false)
	}
	if body.Distinct || len(body.GroupBy) != 0 || body.Having != nil || len(body.Compound) != 0 ||
		body.Limit != nil || body.LimitParam != nil ||
		body.Offset != nil || body.OffsetParam != nil || len(body.Windows) != 0 ||
		len(body.CTEs) != 0 || body.ValuesArms != 0 ||
		body.FromNestedParenJoin || len(body.From) != 1 || r37cMayReferenceRowid(body) {
		return nil, false
	}
	b := body.From[0]
	tbl, ok := p.r37cOrdinaryBase(b)
	if !ok {
		return nil, false
	}
	f := &r41Flat{
		p:       p,
		dName:   it.Alias,
		byName:  map[string]int{},
		base:    map[string]string{},
		others:  map[string]bool{},
		scopes:  map[string]bool{},
		aliases: map[string]bool{},
	}
	for _, c := range tbl.cols {
		if c.Hidden {
			return nil, false
		}
		f.base[r33sFoldIdent(c.Name)] = c.Name
	}
	for j, o := range stmt.From {
		if j == i {
			continue
		}
		name, cols, ok := p.r41ItemNames(o)
		if !ok {
			return nil, false
		}
		if name != "" {
			f.scopes[r33sFoldIdent(name)] = true
		}
		for _, c := range cols {
			f.others[r33sFoldIdent(c)] = true
		}
	}
	if f.dName != "" {
		if f.scopes[r33sFoldIdent(f.dName)] {
			return nil, false
		}
		f.alias = f.dName
	} else {
		for n := 1; ; n++ {
			f.alias = fmt.Sprintf("sqlite_flat_%d", n)
			if !f.scopes[r33sFoldIdent(f.alias)] {
				break
			}
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star && c.HasAlias {
			f.aliases[r33sFoldIdent(c.Alias)] = true
		}
	}

	// The body, rewritten onto the base table's new scope name.
	bName := b.Alias
	if bName == "" {
		bName = b.Table
	}
	inBody := func(ce ColumnExpr) (Expr, bool) {
		if ce.Schema != "" || ce.UsingRepr || ce.UsingPinned {
			return nil, false
		}
		if ce.Qualifier != "" && !equalFoldName(ce.Qualifier, bName) {
			return nil, false // correlated to an enclosing query
		}
		decl, ok := f.base[r33sFoldIdent(ce.Name)]
		if !ok {
			return nil, false
		}
		return ColumnExpr{Qualifier: f.alias, Name: decl}, true
	}
	for _, c := range body.Columns {
		if c.Star {
			if c.StarQualifier != "" && !equalFoldName(c.StarQualifier, bName) {
				return nil, false
			}
			for _, tc := range tbl.cols {
				f.cols = append(f.cols, r41DerivedCol{name: tc.Name, base: tc.Name})
			}
			continue
		}
		ce, isCol := c.Expr.(ColumnExpr)
		if !isCol {
			if names != nil || !selectCallsNondeterministicFunc(body) || containsAggregate(c.Expr) || exprHasWindow(c.Expr) {
				return nil, false
			}
			rw, ok := r41Rewrite(c.Expr, inBody)
			if !ok {
				return nil, false
			}
			dc := r41DerivedCol{name: c.Text, expr: rw}
			if c.HasAlias {
				dc.name = c.Alias
			}
			f.cols = append(f.cols, dc)
			continue
		}
		e, ok := inBody(ce)
		if !ok {
			return nil, false
		}
		dc := r41DerivedCol{base: e.(ColumnExpr).Name}
		dc.name = dc.base
		switch {
		case c.HasAlias:
			dc.name = c.Alias
		case c.cName != "":
			dc.name = c.cName // named before resolution (nameValuesColumnsPreResolve)
		}
		f.cols = append(f.cols, dc)
	}
	if names != nil {
		if len(names) != len(f.cols) {
			return nil, false
		}
		for k := range f.cols {
			f.cols[k].name = names[k]
		}
	}
	// The derived table's names are sqlite3ColumnsFromExprList's (select.c:2287).
	dnames := make([]string, len(f.cols))
	for k := range f.cols {
		dnames[k] = f.cols[k].name
	}
	for k, n := range trueFalseColumnNames(dnames) {
		f.cols[k].name = n
	}
	for k, dc := range f.cols {
		l := r33sFoldIdent(dc.name)
		if _, dup := f.byName[l]; dup {
			return nil, false // named "x:1" on the derived side; no alias spells that
		}
		f.byName[l] = k
	}
	var bodyOrder []OrderTerm
	for _, ot := range body.OrderBy {
		// Resolved in the BODY's scope first, the way resolveOrderGroupBy did it
		// there (resolve.c:1815-1836): an alias of a body result column, then
		// an ordinal, then an expression over the base table.
		e := ot.Expr
		if ce, isCol := e.(ColumnExpr); isCol && ce.Qualifier == "" && ce.Schema == "" {
			if k, aliased := r41BodyAlias(body, ce.Name); aliased {
				if f.cols[k].expr != nil {
					return nil, false
				}
				ot.Expr = ColumnExpr{Qualifier: f.alias, Name: f.cols[k].base}
				bodyOrder = append(bodyOrder, ot)
				continue
			}
		}
		if lit, isLit := e.(LiteralExpr); isLit {
			if lit.Val.Typ != Int || lit.Val.I < 1 || lit.Val.I > int64(len(f.cols)) {
				return nil, false
			}
			if f.cols[lit.Val.I-1].expr != nil {
				return nil, false
			}
			ot.Expr = ColumnExpr{Qualifier: f.alias, Name: f.cols[lit.Val.I-1].base}
			bodyOrder = append(bodyOrder, ot)
			continue
		}
		if _, isColl := e.(CollateExpr); isColl {
			peeled := skipCollateAndLikely(e)
			if _, isLit := peeled.(LiteralExpr); isLit {
				return nil, false
			}
			if ce, isCol := peeled.(ColumnExpr); isCol && ce.Qualifier == "" {
				if _, aliased := r41BodyAlias(body, ce.Name); aliased {
					return nil, false
				}
			}
		}
		if containsAggregate(e) {
			return nil, false
		}
		rw, ok := r41Rewrite(e, inBody)
		if !ok {
			return nil, false
		}
		ot.Expr = rw
		bodyOrder = append(bodyOrder, ot)
	}
	var bodyWhere Expr
	if body.Where != nil {
		if containsAggregate(body.Where) {
			return nil, false
		}
		w, ok := r41Rewrite(body.Where, inBody)
		if !ok {
			return nil, false
		}
		bodyWhere = w
	}
	return &r41ArmFlat{f: f, b: b, where: bodyWhere, order: bodyOrder, p: p, baseTbl: tbl}, true
}

// collOf returns whether an operand of stmt's comparisons collates BINARY
// without saying so: sqlite3ExprCollSeq through CAST and unary plus to a
// column whose declared sequence is BINARY, or no column at all (an explicit
// COLLATE is answered by the operand itself, either way). FROM item i is the
// subquery being flattened; a column of its base table, or of another
// ordinary table of stmt, is looked up, and anything else is not known.
func (a *r41ArmFlat) collOf(stmt *SelectStmt, i int) func(Expr) bool {
	return func(e Expr) bool {
		for {
			switch x := e.(type) {
			case CastExpr:
				e = x.X
				continue
			case UnaryExpr:
				if x.Op == "+" {
					e = x.X
					continue
				}
			}
			break
		}
		ce, ok := e.(ColumnExpr)
		if !ok {
			// No column under CAST/plus: no sequence of its own, or an
			// explicit one (EP_Collate), which wins on either side alike.
			return true
		}
		if ce.Schema != "" || ce.UsingRepr || ce.UsingPinned {
			return false
		}
		binary := func(c columnInfo) bool {
			return c.NoCollation || c.Collation == "" || equalFoldName(c.Collation, "BINARY")
		}
		// A bare derived column: the base table's own column after the flatten.
		if k, ok := a.f.derivedCol(ce); ok && a.f.cols[k].expr == nil {
			if a.baseTbl == nil {
				return false
			}
			for _, c := range a.baseTbl.cols {
				if equalFoldName(c.Name, a.f.cols[k].base) {
					return binary(c)
				}
			}
			return false
		}
		match, found := columnInfo{}, 0
		for j, o := range stmt.From {
			if j == i {
				continue
			}
			b := o
			b.On = nil
			tbl, ok := a.p.r37cOrdinaryBase(b)
			name := o.Alias
			if name == "" {
				name = o.Table
			}
			if ce.Qualifier != "" && !equalFoldName(ce.Qualifier, name) {
				continue
			}
			if !ok {
				return false
			}
			for _, c := range tbl.cols {
				if equalFoldName(c.Name, ce.Name) {
					match = c
					found++
				}
			}
		}
		return found == 1 && binary(match)
	}
}

// parent is stmt with FROM item i (it) replaced by the arm's base table and
// every reference to the derived table substituted -- substSelect over the
// parent (select.c:4672-4682).
func (a *r41ArmFlat) parent(stmt *SelectStmt, i int, it FromItem) (*SelectStmt, bool) {
	f, b, bodyWhere, bodyOrder := a.f, a.b, a.where, a.order
	if f.hasExprCol() {
		binaryColl := a.collOf(stmt, i)
		decline, wrap := f.exprColCompared(stmt, binaryColl)
		if decline {
			return nil, false
		}
		if wrap {
			ws, ok := f.wrapComparedLeft(stmt, binaryColl)
			if !ok {
				return nil, false
			}
			stmt = ws
		}
	}
	f.aliasExpr, f.aliasOn = map[string]Expr{}, false
	sub := func(ce ColumnExpr) (Expr, bool) { return f.parentRef(ce) }
	rewrite := func(e Expr) (Expr, bool) { return r41RewriteS(e, sub, f.substSub) }
	out := *stmt
	out.Columns = nil
	for _, c := range stmt.Columns {
		if c.Star {
			if c.StarQualifier == "" {
				if len(stmt.From) != 1 {
					return nil, false
				}
				out.Columns = append(out.Columns, f.starColumns()...)
				continue
			}
			if f.dName != "" && equalFoldName(c.StarQualifier, f.dName) {
				out.Columns = append(out.Columns, f.starColumns()...)
				continue
			}
			if !f.scopes[r33sFoldIdent(c.StarQualifier)] {
				return nil, false
			}
			out.Columns = append(out.Columns, c)
			continue
		}
		e, ok := rewrite(c.Expr)
		if !ok {
			return nil, false
		}
		if c.HasAlias {
			// lookupName's alias arm takes the FIRST result column so named
			// (resolve.c:664-667, the ENAME_NAME loop over pEList).
			if l := r33sFoldIdent(c.Alias); !r41Has(f.aliasExpr, l) {
				if containsAggregate(e) || exprHasWindow(e) {
					f.aliasExpr[l] = nil
				} else {
					f.aliasExpr[l] = e
				}
			}
		}
		if ce, isCol := c.Expr.(ColumnExpr); isCol && !c.HasAlias {
			if k, ok := f.derivedCol(ce); ok {
				// generateColumnNames reports the derived column's own name.
				c.Alias, c.HasAlias = f.cols[k].name, true
			}
		}
		c.Expr = e
		out.Columns = append(out.Columns, c)
	}
	out.From = append([]FromItem(nil), stmt.From...)
	for j := range out.From {
		if len(out.From[j].TableFuncArgs) != 0 {
			args := make([]Expr, len(out.From[j].TableFuncArgs))
			for k, a := range out.From[j].TableFuncArgs {
				na, ok := rewrite(a)
				if !ok {
					return nil, false
				}
				args[k] = na
			}
			out.From[j].TableFuncArgs = args
		}
		if out.From[j].On == nil {
			continue
		}
		on, ok := rewrite(out.From[j].On)
		if !ok {
			return nil, false
		}
		out.From[j].On = on
	}
	var itOn Expr
	if it.On != nil {
		on, ok := rewrite(it.On)
		if !ok {
			return nil, false
		}
		itOn = on
	}
	if f.outerRowid != "" && bodyWhere != nil {
		// The body's WHERE joins the ON clause of the outer join it now sits
		// in (select.c:4618, sqlite3SetJoinExpr with EP_OuterON).
		if itOn == nil {
			itOn = bodyWhere
		} else {
			itOn = BinaryExpr{Op: "AND", L: bodyWhere, R: itOn}
		}
		bodyWhere = nil
	}
	out.From[i] = FromItem{
		Table:        b.Table,
		Schema:       b.Schema,
		Alias:        f.alias,
		Join:         it.Join,
		CrossKeyword: it.CrossKeyword,
		On:           itOn,
		IndexedBy:    b.IndexedBy,
		NotIndexed:   b.NotIndexed,
		flattened:    true,
	}
	// WHERE, GROUP BY, HAVING and ORDER BY are resolved with the result list
	// in view (NC_UEList, resolve.c:1996-2007), so a name there that no FROM
	// item has may be a result ALIAS.
	f.aliasOn = true
	w := stmt.Where
	if w != nil {
		rw, ok := rewrite(w)
		if !ok {
			return nil, false
		}
		w = rw
	}
	switch {
	case bodyWhere == nil:
		out.Where = w
	case w == nil:
		out.Where = bodyWhere
	default:
		out.Where = BinaryExpr{Op: "AND", L: bodyWhere, R: w}
	}
	if len(stmt.GroupBy) != 0 {
		out.GroupBy = make([]Expr, len(stmt.GroupBy))
		for k, g := range stmt.GroupBy {
			rg, ok := rewrite(g)
			if !ok {
				return nil, false
			}
			out.GroupBy[k] = rg
		}
	}
	if stmt.Having != nil {
		h, ok := rewrite(stmt.Having)
		if !ok {
			return nil, false
		}
		out.Having = h
	}
	if len(stmt.OrderBy) != 0 {
		out.OrderBy = make([]OrderTerm, len(stmt.OrderBy))
		for k, ot := range stmt.OrderBy {
			// resolveOrderGroupBy tries an ORDER BY term as a result-column
			// ALIAS before anything else (resolve.c:1815-1824, resolveAsName on
			// the COLLATE/likely-peeled term), so such a term is not a column
			// reference at all and is left exactly as written.
			if ce, isCol := skipCollateAndLikely(ot.Expr).(ColumnExpr); isCol && ce.Qualifier == "" && ce.Schema == "" &&
				f.aliases[r33sFoldIdent(ce.Name)] {
				out.OrderBy[k] = ot
				continue
			}
			e, ok := rewrite(ot.Expr)
			if !ok {
				return nil, false
			}
			ot.Expr = e
			out.OrderBy[k] = ot
		}
	}
	if bodyOrder != nil {
		out.OrderBy = bodyOrder
	}
	if r41NamesAColumn(stmt.LimitParam) || r41NamesAColumn(stmt.OffsetParam) {
		return nil, false
	}
	if a.multi != nil {
		first := out.From[i]
		spliced := append([]FromItem(nil), a.multi...)
		spliced[0].Join, spliced[0].CrossKeyword = first.Join, first.CrossKeyword
		switch {
		case first.On == nil:
		case spliced[0].On == nil:
			spliced[0].On = first.On
		default:
			spliced[0].On = BinaryExpr{Op: "AND", L: spliced[0].On, R: first.On}
		}
		from := append([]FromItem(nil), out.From[:i]...)
		from = append(from, spliced...)
		out.From = append(from, out.From[i+1:]...)
	}
	return &out, true
}

// r41BodyAlias reports which of body's result columns carries the explicit
// alias name -- resolveAsName's match (resolve.c:1490-1510), which only an AS name
// makes.
func r41BodyAlias(body *SelectStmt, name string) (int, bool) {
	k := 0
	for _, c := range body.Columns {
		if c.Star {
			return 0, false // positions past a "*" are the table's, not counted here
		}
		if c.HasAlias && equalFoldName(c.Alias, name) {
			return k, true
		}
		k++
	}
	return 0, false
}

// r41OrderAgg is SF_OrderByReqd on the parent: it aggregates, and some
// aggregate it calls is not count(), min() or max() -- the three built-ins
// that carry SQLITE_FUNC_ANYORDER (func.c:3314-3370), which resolve.c:1366-1371
// turns into NC_OrderAgg for every OTHER aggregate. Every clause is scanned,
// not just the result set C scans at resolve.c:1982: an aggregate spelled only
// in HAVING or ORDER BY answers true, which keeps the body's ORDER BY and so
// keeps the statement off the flattener rather than guessing.
func r41OrderAgg(stmt *SelectStmt) bool {
	found := false
	visit := func(fc FuncExpr) bool {
		if isAggregateCall(fc) {
			switch r33sFoldIdent(fc.Name) {
			case "count", "min", "max":
			default:
				found = true
			}
		}
		return !found
	}
	for _, c := range stmt.Columns {
		if !c.Star {
			walkExprShallow(c.Expr, visit)
		}
	}
	walkExprShallow(stmt.Having, visit)
	for _, ot := range stmt.OrderBy {
		walkExprShallow(ot.Expr, visit)
	}
	return found
}

// r41ComplexResult is SF_ComplexResult (select.c:6342): some result
// expression carries EP_HasFunc or EP_Subquery. LIKE, GLOB and MATCH are
// function calls to the parser (sqlite3ExprFunction), so they count; so does
// any node r41Walk does not know.
func r41ComplexResult(stmt *SelectStmt) bool {
	for _, c := range stmt.Columns {
		if c.Star {
			continue
		}
		complex := false
		known := r41Walk(c.Expr, func(e Expr) {
			switch e.(type) {
			case FuncExpr, LikeExpr, GlobExpr, MatchExpr:
				complex = true
			}
		})
		if complex || !known {
			return true
		}
	}
	return false
}

// r41Walk visits every node of e, and reports false if e holds a node it does
// not descend into (a subquery, RAISE(), or a kind it does not know).
func r41Walk(e Expr, visit func(Expr)) bool {
	return r41WalkQ(e, visit, nil)
}

// r41WalkQ is r41Walk that steps over, as known, a subquery subOK accepts.
func r41WalkQ(e Expr, visit func(Expr), subOK func(*SelectStmt) bool) bool {
	known := true
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil || !known {
			return
		}
		visit(e)
		switch x := e.(type) {
		case LiteralExpr, ParamExpr, ColumnExpr:
		case UnaryExpr:
			walk(x.X)
		case BinaryExpr:
			walk(x.L)
			walk(x.R)
		case IsNullExpr:
			walk(x.X)
		case InExpr:
			if x.Sub != nil && (subOK == nil || !subOK(x.Sub)) {
				known = false
				return
			}
			walk(x.X)
			for _, a := range x.List {
				walk(a)
			}
		case SubqueryExpr:
			if subOK == nil || !subOK(x.Stmt) {
				known = false
			}
		case ExistsExpr:
			if subOK == nil || !subOK(x.Stmt) {
				known = false
			}
		case BetweenExpr:
			walk(x.X)
			walk(x.Lo)
			walk(x.Hi)
		case LikeExpr:
			walk(x.X)
			walk(x.Pattern)
			walk(x.Escape)
		case GlobExpr:
			walk(x.X)
			walk(x.Pattern)
		case MatchExpr:
			walk(x.X)
			walk(x.Pattern)
		case CollateExpr:
			walk(x.X)
		case CastExpr:
			walk(x.X)
		case FuncExpr:
			for _, a := range x.walkArgs() {
				walk(a)
			}
			walk(x.Filter)
		case CaseExpr:
			walk(x.Base)
			for _, w := range x.Whens {
				walk(w.When)
				walk(w.Then)
			}
			walk(x.Else)
		case RowExpr:
			for _, a := range x.Elems {
				walk(a)
			}
		default:
			known = false
		}
	}
	walk(e)
	return known
}

// starColumns is a "*" (or "<derived>.*") over the derived table, spelled out:
// each derived column under its own name.
func (f *r41Flat) starColumns() []SelectColumn {
	out := make([]SelectColumn, len(f.cols))
	for k, dc := range f.cols {
		out[k] = SelectColumn{
			Expr:  f.colExpr(k),
			Alias: dc.name, HasAlias: true, Text: dc.name, RawText: dc.name,
		}
	}
	return out
}

// colExpr is what a reference to derived column k becomes in the parent.
func (f *r41Flat) colExpr(k int) Expr {
	if f.cols[k].expr != nil {
		if f.outerRowid != "" {
			return CaseExpr{Whens: []WhenClause{{
				When: IsNullExpr{X: ColumnExpr{Qualifier: f.alias, Name: f.outerRowid}, Not: true},
				Then: f.cols[k].expr,
			}}}
		}
		return f.cols[k].expr
	}
	return ColumnExpr{Qualifier: f.alias, Name: f.cols[k].base}
}

// derivedCol reports which derived column ce names, if it names one.
func (f *r41Flat) derivedCol(ce ColumnExpr) (int, bool) {
	if ce.Qualifier != "" && (f.dName == "" || !equalFoldName(ce.Qualifier, f.dName)) {
		return 0, false
	}
	k, ok := f.byName[r33sFoldIdent(ce.Name)]
	return k, ok
}

// parentRef is substExpr's TK_COLUMN arm for one parent reference, refusing
// every reference whose binding the rewrite could change (see
// r41FlattenProjection).
func (f *r41Flat) parentRef(ce ColumnExpr) (Expr, bool) {
	if ce.UsingRepr || ce.UsingPinned {
		return nil, false
	}
	l := r33sFoldIdent(ce.Name)
	if ce.Qualifier != "" {
		if f.dName == "" && equalFoldName(ce.Qualifier, f.alias) {
			return nil, false // the invented name would newly resolve
		}
		if f.dName == "" || !equalFoldName(ce.Qualifier, f.dName) {
			return ce, true // another FROM item, or an enclosing query
		}
		if ce.Schema != "" {
			return nil, false
		}
		k, ok := f.byName[l]
		if !ok {
			return nil, false
		}
		return f.colExpr(k), true
	}
	if ce.Schema != "" {
		return nil, false
	}
	_, isBase := f.base[l]
	if k, ok := f.byName[l]; ok {
		if f.others[l] {
			return nil, false // ambiguous in C; leave the error to the original
		}
		return f.colExpr(k), true
	}
	if f.others[l] {
		if isBase {
			return nil, false // would turn ambiguous
		}
		return ce, true
	}
	// No FROM item has the name, so lookupName tries the result ALIASES next
	// (resolve.c:658-690, "if( cnt==0 && (pNC->ncFlags & NC_UEList)!=0 && zTab==0 )"),
	// and resolveAlias puts a COPY of the aliased expression in its place.
	// Copied here too, because after the rewrite the base table may have a
	// column of that name which would win instead.
	if f.aliasOn {
		if ae, ok := f.aliasExpr[l]; ok {
			if ae == nil {
				return nil, false // "misuse of aliased aggregate" and kin
			}
			return ae, true
		}
	}
	if isBase {
		return nil, false
	}
	return ce, true
}

// subqueryUntouched reports whether substSub takes s.
func (f *r41Flat) subqueryUntouched(s *SelectStmt) bool {
	_, ok := f.substSub(s)
	return ok
}

// r41Scope is the names one SELECT's FROM clause binds, for substSub.
type r41Scope struct {
	quals, cols map[string]bool
	known       bool // every name the scope binds is in quals/cols
	opaque      bool // a FROM-clause subquery: the scopes outside it are not its own
}

// substSub is substSelect's reach into a subquery of the parent
// (select.c:3914): s with every reference to the derived table replaced by
// its copy, as substExpr replaces one in the parent. false when a name in s
// could bind otherwise than read here -- to a column the flattened base
// table newly exposes, through a scope whose names are not all known, or to
// a result alias of the parent -- or when a copy lands where its implicit
// collating sequence would decide (copyColl), which is spelled only in the
// parent's own clauses.
//
// A name the subquery's own FROM clause binds is its own whatever it spells:
// SQLite resolves the innermost scope first (lookupName, resolve.c). The
// scopes are read only where every item's names are known (r41ItemNames) and
// the subquery defines no CTE of its own; a FROM-clause subquery sees no
// sibling of its own select, so a name it does not bind itself is not
// followed further out.
func (f *r41Flat) substSub(s *SelectStmt) (*SelectStmt, bool) {
	var stack []r41Scope
	// bound is 1 when a scope of s binds ce, 0 when none does, -1 when that
	// is not known.
	bound := func(ce ColumnExpr) int {
		for k := len(stack) - 1; k >= 0; k-- {
			sc := stack[k]
			if !sc.known || ce.Schema != "" {
				return -1
			}
			if ce.Qualifier != "" {
				if sc.quals[r33sFoldIdent(ce.Qualifier)] {
					return 1
				}
			} else if sc.cols[r33sFoldIdent(ce.Name)] {
				return 1
			}
			if sc.opaque {
				return -1
			}
		}
		return 0
	}
	aliasShadowed := func() bool {
		for _, sc := range stack {
			if sc.quals[r33sFoldIdent(f.alias)] {
				return true
			}
		}
		return false
	}
	col := func(ce ColumnExpr) (Expr, bool) {
		if ce.UsingRepr || ce.UsingPinned {
			return nil, false
		}
		l := r33sFoldIdent(ce.Name)
		_, isBase := f.base[l]
		k, isDerived := f.byName[l]
		qualDerived := ce.Qualifier != "" && f.dName != "" && equalFoldName(ce.Qualifier, f.dName)
		qualAlias := ce.Qualifier != "" && equalFoldName(ce.Qualifier, f.alias)
		mentions := qualDerived || qualAlias || (ce.Qualifier == "" && (isBase || isDerived || f.aliases[l]))
		switch bound(ce) {
		case 1:
			return ce, true
		case -1:
			return ce, !mentions
		}
		if !mentions {
			return ce, true
		}
		if qualAlias && !qualDerived {
			return nil, false // the invented name would newly resolve
		}
		if !isDerived || (ce.Qualifier == "" && f.others[l]) {
			return nil, false
		}
		if aliasShadowed() {
			return nil, false // the copy's own references would bind inside
		}
		return f.colExpr(k), true
	}
	// innerBinary is binaryColl for an operand inside s, whose columns are
	// not looked up: only one naming no column is known.
	innerBinary := func(e Expr) bool {
		for {
			switch x := e.(type) {
			case CastExpr:
				e = x.X
				continue
			case UnaryExpr:
				if x.Op == "+" {
					e = x.X
					continue
				}
			}
			break
		}
		_, isCol := e.(ColumnExpr)
		return !isCol
	}
	var walk func(s *SelectStmt, fromItem bool) (*SelectStmt, bool)
	expr := func(e Expr) (Expr, bool) {
		if e == nil {
			return nil, true
		}
		ok := true
		if !r41WalkQ(e, func(n Expr) {
			if w, d := f.copyColl(n, innerBinary); w || d {
				ok = false
			}
		}, func(*SelectStmt) bool { return true }) || !ok {
			return e, false
		}
		return r41RewriteS(e, col, func(q *SelectStmt) (*SelectStmt, bool) { return walk(q, false) })
	}
	walk = func(s *SelectStmt, fromItem bool) (*SelectStmt, bool) {
		if s == nil {
			return nil, true
		}
		if len(s.CTEs) != 0 || len(s.Windows) != 0 {
			return s, false
		}
		sc := r41Scope{quals: map[string]bool{}, cols: map[string]bool{}, known: f.p != nil, opaque: fromItem}
		for _, it := range s.From {
			if !sc.known {
				break
			}
			if it.TableFunc || it.GroupLen != 0 || len(it.NestedGroupSpan) != 0 || it.GroupAlias != "" {
				sc.known = false
				break
			}
			name, cols, ok := f.p.r41ItemNames(it)
			if !ok || name == "" {
				sc.known = false
				break
			}
			sc.quals[r33sFoldIdent(name)] = true
			for _, c := range cols {
				sc.cols[r33sFoldIdent(c)] = true
			}
		}
		stack = append(stack, sc)
		defer func() { stack = stack[:len(stack)-1] }()
		out := *s
		ok := true
		one := func(e Expr) Expr {
			if !ok {
				return e
			}
			n, eok := expr(e)
			if !eok {
				ok = false
			}
			return n
		}
		out.Columns = make([]SelectColumn, len(s.Columns))
		for k, c := range s.Columns {
			if c.Star {
				if c.StarQualifier != "" && !sc.quals[r33sFoldIdent(c.StarQualifier)] &&
					((f.dName != "" && equalFoldName(c.StarQualifier, f.dName)) || equalFoldName(c.StarQualifier, f.alias)) {
					return s, false
				}
			} else {
				c.Expr = one(c.Expr)
			}
			out.Columns[k] = c
		}
		out.From = append([]FromItem(nil), s.From...)
		for k := range out.From {
			out.From[k].On = one(out.From[k].On)
			if len(out.From[k].TableFuncArgs) != 0 {
				args := make([]Expr, len(out.From[k].TableFuncArgs))
				for j, a := range out.From[k].TableFuncArgs {
					args[j] = one(a)
				}
				out.From[k].TableFuncArgs = args
			}
			if out.From[k].Subquery != nil && ok {
				q, qok := walk(out.From[k].Subquery, true)
				if !qok {
					ok = false
				}
				out.From[k].Subquery = q
			}
		}
		out.Where = one(s.Where)
		if s.GroupBy != nil {
			out.GroupBy = make([]Expr, len(s.GroupBy))
			for k, g := range s.GroupBy {
				out.GroupBy[k] = one(g)
			}
		}
		out.Having = one(s.Having)
		if s.OrderBy != nil {
			out.OrderBy = make([]OrderTerm, len(s.OrderBy))
			for k, ot := range s.OrderBy {
				ot.Expr = one(ot.Expr)
				out.OrderBy[k] = ot
			}
		}
		out.LimitParam, out.OffsetParam = one(s.LimitParam), one(s.OffsetParam)
		if s.Compound != nil {
			out.Compound = make([]CompoundArm, len(s.Compound))
			for k, a := range s.Compound {
				if ok {
					// An arm is a select of its own beside this one, not
					// inside it.
					stack = stack[:len(stack)-1]
					q, qok := walk(a.Stmt, fromItem)
					stack = append(stack, sc)
					if !qok {
						ok = false
					}
					a.Stmt = q
				}
				out.Compound[k] = a
			}
		}
		if !ok {
			return s, false
		}
		return &out, true
	}
	return walk(s, false)
}

func r41Has(m map[string]Expr, k string) bool {
	_, ok := m[k]
	return ok
}

// r41NamesAColumn reports whether e names any column at all.
func r41NamesAColumn(e Expr) bool {
	if e == nil {
		return false
	}
	found := false
	_, ok := r41Rewrite(e, func(ce ColumnExpr) (Expr, bool) {
		found = true
		return ce, true
	})
	return found || !ok
}

// r41Rewrite returns e with every column reference replaced by fn's answer, or
// false when fn refuses one or e holds a node the rewrite does not reach into:
// a subquery of any kind, a window function and RAISE().
func r41Rewrite(e Expr, fn func(ColumnExpr) (Expr, bool)) (Expr, bool) {
	return r41RewriteQ(e, fn, nil)
}

// r41RewriteQ is r41Rewrite that lets a subquery through, unchanged, whenever
// subOK accepts it.
func r41RewriteQ(e Expr, fn func(ColumnExpr) (Expr, bool), subOK func(*SelectStmt) bool) (Expr, bool) {
	var subRW func(*SelectStmt) (*SelectStmt, bool)
	if subOK != nil {
		subRW = func(s *SelectStmt) (*SelectStmt, bool) { return s, subOK(s) }
	}
	return r41RewriteS(e, fn, subRW)
}

// r41RewriteS is r41Rewrite that puts subRW's answer in place of every
// subquery, or fails when subRW is nil or refuses one.
func r41RewriteS(e Expr, fn func(ColumnExpr) (Expr, bool), subRW func(*SelectStmt) (*SelectStmt, bool)) (Expr, bool) {
	var ok = true
	sub := func(s *SelectStmt) *SelectStmt {
		if subRW == nil {
			ok = false
			return s
		}
		n, sok := subRW(s)
		if !sok {
			ok = false
			return s
		}
		return n
	}
	var rw func(Expr) Expr
	list := func(es []Expr) []Expr {
		if es == nil {
			return nil
		}
		out := make([]Expr, len(es))
		for k, x := range es {
			out[k] = rw(x)
		}
		return out
	}
	rw = func(e Expr) Expr {
		if !ok || e == nil {
			return e
		}
		switch x := e.(type) {
		case LiteralExpr, ParamExpr:
			return e
		case ColumnExpr:
			n, cok := fn(x)
			if !cok {
				ok = false
				return e
			}
			return n
		case UnaryExpr:
			x.X = rw(x.X)
			return x
		case BinaryExpr:
			x.L, x.R = rw(x.L), rw(x.R)
			return x
		case IsNullExpr:
			x.X = rw(x.X)
			return x
		case InExpr:
			if x.Sub != nil {
				x.Sub = sub(x.Sub)
			}
			x.X, x.List = rw(x.X), list(x.List)
			return x
		case SubqueryExpr:
			x.Stmt = sub(x.Stmt)
			return x
		case ExistsExpr:
			x.Stmt = sub(x.Stmt)
			return x
		case BetweenExpr:
			x.X, x.Lo, x.Hi = rw(x.X), rw(x.Lo), rw(x.Hi)
			return x
		case LikeExpr:
			x.X, x.Pattern, x.Escape = rw(x.X), rw(x.Pattern), rw(x.Escape)
			return x
		case GlobExpr:
			x.X, x.Pattern = rw(x.X), rw(x.Pattern)
			return x
		case MatchExpr:
			x.X, x.Pattern = rw(x.X), rw(x.Pattern)
			return x
		case CollateExpr:
			x.X = rw(x.X)
			return x
		case CastExpr:
			x.X = rw(x.X)
			return x
		case FuncExpr:
			if x.Over != nil {
				ok = false
				return e
			}
			x.Args, x.Filter = list(x.Args), rw(x.Filter)
			if len(x.OrderBy) != 0 {
				ob := make([]OrderTerm, len(x.OrderBy))
				for k, t := range x.OrderBy {
					t.Expr = rw(t.Expr)
					ob[k] = t
				}
				x.OrderBy = ob
			}
			return x
		case CaseExpr:
			x.Base, x.Else = rw(x.Base), rw(x.Else)
			if len(x.Whens) != 0 {
				ws := make([]WhenClause, len(x.Whens))
				for k, wc := range x.Whens {
					ws[k] = WhenClause{When: rw(wc.When), Then: rw(wc.Then)}
				}
				x.Whens = ws
			}
			return x
		case RowExpr:
			x.Elems = list(x.Elems)
			return x
		default:
			ok = false
			return e
		}
	}
	out := rw(e)
	return out, ok
}

// r41FlattenCompound is the "compound-subquery flattening" (select.c:4470-4538):
// a UNION ALL body turns the parent itself into a UNION ALL of one copy per arm,
// each copy flattening its own arm and carrying the arm's WHERE, with the
// parent's LIMIT/OFFSET left on the whole compound. The arms come out in the
// body's own order.
//
// Restriction (17) is checked in full -- (17a) UNION ALL only, (17b) no
// DISTINCT or aggregate arm, (17c) every arm has a FROM clause, (17d) the parent
// is neither aggregate nor DISTINCT, (17e) no window function, (17f) not an
// outer join's right operand, (17g) no RIGHT/FULL join in an arm, (17h) the
// arms agree on every column's affinity -- and so are (20), (23) and (15).
// (18) is narrowed to "the parent has no ORDER BY at all": with one, SQLite
// runs the compound through multiSelectByMerge, which sorts each arm
// separately and merges, and this package's compound codegen concatenates and
// sorts instead -- a different tie order (see r38cFlattenCompoundArms).
func (p *ReadOnlyPager) r41FlattenCompound(stmt *SelectStmt, i int, it FromItem, body *SelectStmt) (*SelectStmt, bool) {
	if selectIsAggregateQuery(stmt) || stmt.Distinct || stmt.Having != nil || len(stmt.OrderBy) != 0 { // (17d), (18)
		return nil, false
	}
	if len(body.OrderBy) != 0 || body.Limit != nil || body.LimitParam != nil || // (20), (15)
		body.Offset != nil || body.OffsetParam != nil || body.ValuesArms != 0 ||
		len(body.CTEs) != 0 || len(body.Windows) != 0 {
		return nil, false
	}
	if len(p.cteExpansion) != 0 { // (23), as r37cCompoundFlattenable reads it
		return nil, false
	}
	first := *body
	first.Compound = nil
	arms := []*SelectStmt{&first}
	for _, c := range body.Compound {
		if c.Op != "UNION ALL" || c.Stmt == nil { // (17a)
			return nil, false
		}
		arms = append(arms, c.Stmt)
	}
	// (17h), compoundHasDifferentAffinities (select.c:4092).
	aff0, ok := p.r37cArmAffinities(arms[0])
	if !ok {
		return nil, false
	}
	for _, arm := range arms[1:] {
		affs, aok := p.r37cArmAffinities(arm)
		if !aok || len(affs) != len(aff0) {
			return nil, false
		}
		for k := range affs {
			if affs[k] != aff0[k] {
				return nil, false
			}
		}
	}
	// substExpr gives an arm's copy whose collation is not the LEFTMOST arm's
	// an implicit COLLATE of the leftmost's (select.c:3857-3870, pCList is
	// findLeftmostExprlist) -- which this AST cannot spell, since its COLLATE
	// is always the explicit kind. Measured on collate5.test: over t1(a, b
	// COLLATE nocase) UNION ALL t2(c, d) with no collation, "WHERE b='BbB'"
	// matched t2's 'bbb' in C, and flattening without the wrapper lost it.
	colls0, ok := p.r41ArmCollations(arms[0])
	if !ok {
		return nil, false
	}
	for _, arm := range arms[1:] {
		colls, cok := p.r41ArmCollations(arm)
		if !cok || len(colls) != len(colls0) {
			return nil, false
		}
		for k := range colls {
			if colls[k] != colls0[k] {
				return nil, false
			}
		}
	}
	a0, ok := p.r41Arm(stmt, i, it, arms[0], nil)
	if !ok {
		return nil, false
	}
	names := make([]string, len(a0.f.cols))
	for k, dc := range a0.f.cols {
		names[k] = dc.name
	}
	out, ok := a0.parent(stmt, i, it)
	if !ok {
		return nil, false
	}
	for _, arm := range arms[1:] {
		ak, ok := p.r41Arm(stmt, i, it, arm, names)
		if !ok {
			return nil, false
		}
		ck, ok := ak.parent(stmt, i, it)
		if !ok {
			return nil, false
		}
		ck.CTEs, ck.OrderBy = nil, nil
		ck.Limit, ck.LimitParam, ck.Offset, ck.OffsetParam = nil, nil, nil, nil
		out.Compound = append(out.Compound, CompoundArm{Op: "UNION ALL", Stmt: ck})
	}
	return out, true
}

// r41ItemNames is a FROM item's scope name and the column names it exposes to
// an unqualified reference: an ordinary table's columns, or the result names of
// a subquery, view or CTE -- read off its leftmost arm the way
// sqlite3ColumnsFromExprList names them (alias, else the column's name, else
// the expression's text). false when the item is anything else, or its names
// cannot be read without resolving it.
func (p *ReadOnlyPager) r41ItemNames(o FromItem) (string, []string, bool) {
	name := o.Alias
	if o.Subquery == nil {
		if name == "" {
			name = o.Table
		}
		if o.TableFunc || len(o.TableFuncArgs) != 0 {
			cols := r41TVFColumns(o)
			return name, cols, cols != nil
		}
		b := o
		b.On = nil
		if t, ok := p.r37cOrdinaryBase(b); ok {
			cols := make([]string, len(t.cols))
			for k, c := range t.cols {
				cols[k] = c.Name
			}
			return name, cols, true
		}
		ex, ok := p.r38cExpandCTERef(o)
		if !ok {
			ex, ok = p.r37cExpandViewRef(o)
		}
		if !ok {
			return "", nil, false
		}
		o = ex
	}
	body := o.Subquery
	var cols []string
	for _, c := range body.Columns {
		switch {
		case c.Star:
			if c.StarQualifier != "" || len(body.From) != 1 {
				return "", nil, false
			}
			_, inner, ok := p.r41ItemNames(body.From[0])
			if !ok {
				return "", nil, false
			}
			cols = append(cols, inner...)
		case c.HasAlias:
			cols = append(cols, c.Alias)
		default:
			if ce, isCol := c.Expr.(ColumnExpr); isCol {
				cols = append(cols, ce.Name)
			} else {
				cols = append(cols, c.Text)
			}
		}
	}
	return name, cols, true
}

// r41TVFColumns is every column name, hidden ones included, of a
// table-valued function item whose module r41 reads (json_each and its kin),
// or nil.
func r41TVFColumns(o FromItem) []string {
	if o.Schema != "" || o.Subquery != nil {
		return nil
	}
	mod, ok := lookupVtabModule(o.Table)
	if !ok {
		return nil
	}
	if _, isJSON := mod.(jsonEachModule); !isJSON {
		return nil
	}
	cols, _, err := mod.Connect(nil)
	if err != nil {
		return nil
	}
	out := make([]string, len(cols))
	for k, c := range cols {
		out[k] = c.Name
	}
	return out
}

// r41WalkStmt visits every column reference and every qualified "*" anywhere in
// s, subqueries and CTE bodies included, and reports false when a visit does
// or when s holds a node it does not know.
func r41WalkStmt(s *SelectStmt, col func(ColumnExpr) bool, star func(string) bool) bool {
	return r41WalkStmtE(s, col, star, nil, false)
}

// r41WalkStmtE is r41WalkStmt that calls enter, when set, on every SELECT it
// walks before anything in it (fromItem: it is a FROM-clause subquery), and
// the function enter returns once that SELECT is done.
func r41WalkStmtE(s *SelectStmt, col func(ColumnExpr) bool, star func(string) bool,
	enter func(s *SelectStmt, fromItem bool) func(), fromItem bool) bool {
	if s == nil {
		return true
	}
	if enter != nil {
		defer enter(s, fromItem)()
	}
	ok := true
	var expr func(Expr)
	stmt := func(s *SelectStmt) {
		if ok && !r41WalkStmtE(s, col, star, enter, false) {
			ok = false
		}
	}
	fromStmt := func(s *SelectStmt) {
		if ok && !r41WalkStmtE(s, col, star, enter, true) {
			ok = false
		}
	}
	spec := func(w *WindowSpec) {
		if w == nil {
			return
		}
		for _, e := range w.PartitionBy {
			expr(e)
		}
		for _, t := range w.OrderBy {
			expr(t.Expr)
		}
		if w.Frame != nil {
			expr(w.Frame.Start.Offset)
			expr(w.Frame.End.Offset)
		}
	}
	expr = func(e Expr) {
		if !ok || e == nil {
			return
		}
		switch x := e.(type) {
		case LiteralExpr, ParamExpr:
		case ColumnExpr:
			if !col(x) {
				ok = false
			}
		case SubqueryExpr:
			stmt(x.Stmt)
		case ExistsExpr:
			stmt(x.Stmt)
		case InExpr:
			stmt(x.Sub)
			expr(x.X)
			for _, a := range x.List {
				expr(a)
			}
		case MatchExpr:
			expr(x.X)
			expr(x.Pattern)
		case RaiseExpr:
			expr(x.Msg)
		case FuncExpr:
			for _, a := range x.walkArgs() {
				expr(a)
			}
			expr(x.Filter)
			spec(x.Over)
		case UnaryExpr, BinaryExpr, IsNullExpr, BetweenExpr, LikeExpr, GlobExpr,
			CollateExpr, CastExpr, CaseExpr, RowExpr:
			walkExprOperands(e, expr)
		default:
			ok = false
		}
	}
	for _, c := range s.Columns {
		if c.Star {
			if c.StarQualifier != "" && !star(c.StarQualifier) {
				return false
			}
			continue
		}
		expr(c.Expr)
	}
	for _, it := range s.From {
		expr(it.On)
		for _, a := range it.TableFuncArgs {
			expr(a)
		}
		fromStmt(it.Subquery)
	}
	expr(s.Where)
	for _, g := range s.GroupBy {
		expr(g)
	}
	expr(s.Having)
	for _, t := range s.OrderBy {
		expr(t.Expr)
	}
	expr(s.LimitParam)
	expr(s.OffsetParam)
	for _, a := range s.Compound {
		stmt(a.Stmt)
	}
	for _, c := range s.CTEs {
		stmt(c.Select)
	}
	for _, w := range s.Windows {
		spec(w.Spec)
	}
	return ok
}

func (f *r41Flat) hasExprCol() bool {
	for _, dc := range f.cols {
		if dc.expr != nil {
			return true
		}
	}
	return false
}

// exprColCompared reports a reference to an expression column of the derived
// table that is an operand of a comparison. substExpr gives the copy the
// column's own implicit collating sequence (an un-flagged COLLATE, BINARY for
// an expression that has none), which a comparison then prefers over the other
// operand's -- a node this AST has no spelling for. Everywhere else the copy
// and the bare expression collate alike, BINARY being the default.
//
// wrap reports a comparison where the copy's sequence decides and the bare
// expression would take another operand's sequence instead:
// wrapComparedLeft spells the copy's BINARY out there. decline reports one
// this does not follow at all.
func (f *r41Flat) exprColCompared(stmt *SelectStmt, binaryColl func(Expr) bool) (decline, wrap bool) {
	visit := func(e Expr) {
		w, d := f.copyColl(e, binaryColl)
		wrap = wrap || w
		decline = decline || d
	}
	walk := func(e Expr) {
		// A subquery that names nothing of the derived table holds no such
		// reference; the flatten leaves it as it is (subqueryUntouched).
		if e != nil && !r41WalkQ(e, visit, f.subqueryUntouched) {
			// A node the walk does not enter could hold one.
			decline = true
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star {
			walk(c.Expr)
		}
	}
	walk(stmt.Where)
	walk(stmt.Having)
	for _, it := range stmt.From {
		walk(it.On)
		for _, a := range it.TableFuncArgs {
			walk(a)
		}
	}
	for _, g := range stmt.GroupBy {
		walk(g)
	}
	for _, ot := range stmt.OrderBy {
		walk(ot.Expr)
	}
	return decline, wrap
}

// copyColl is exprColCompared for one node: wrap when the operand whose
// sequence the node's comparisons take first is a copy, the bare expression
// would defer to another operand's sequence there, and that sequence is not
// BINARY; decline for a shape that is not followed.
//
// The operand that decides is the comparison's left one (sqlite3BinaryCompareCollSeq,
// expr.c:430: an explicit COLLATE on either side first, then the left
// operand's sequence, then the right's), BETWEEN's X (coded as two comparisons
// with X on the left), a CASE's base (compared with each WHEN on the right),
// and IN's X: a list takes X's sequence alone (sqlite3CodeRhsOfIN), a
// subquery compares X with its result column as a comparison does. The copy's
// sequence carries no EP_Collate, so an explicit COLLATE on another operand
// still beats it; binaryColl answers true for one.
func (f *r41Flat) copyColl(e Expr, binaryColl func(Expr) bool) (wrap, decline bool) {
	isRef := f.isExprColRef
	var x Expr
	var others []Expr
	switch n := e.(type) {
	case BinaryExpr:
		switch strings.ToUpper(n.Op) {
		case "=", "==", "<", "<=", ">", ">=", "!=", "<>", "IS", "IS NOT":
		default:
			return false, false
		}
		x, others = n.L, []Expr{n.R}
	case BetweenExpr:
		x, others = n.X, []Expr{n.Lo, n.Hi}
	case CaseExpr:
		if n.Base == nil {
			return false, false
		}
		x = n.Base
		for _, w := range n.Whens {
			others = append(others, w.When)
		}
	case InExpr:
		x, others = n.X, n.List
	default:
		return false, false
	}
	operands := append([]Expr{x}, others...)
	for _, o := range operands {
		// A row value compares element by element, each with its own
		// sequence; not followed.
		if r, ok := o.(RowExpr); ok {
			for _, el := range r.Elems {
				if isRef(el) {
					return false, true
				}
			}
		}
		// The NULL row's copy is TK_IF_NULL_ROW, which sqlite3ExprAffinity
		// looks through to the expression's own affinity; the CASE spelling
		// here has none, so it stands in only for an expression that has
		// none either.
		if f.outerRowid != "" && isRef(o) && f.refMayHaveAffinity(o) {
			return false, true
		}
	}
	if !isRef(x) {
		return false, false
	}
	if in, ok := e.(InExpr); ok {
		if in.Sub == nil {
			return true, false
		}
		// A result column with an explicit COLLATE beats the copy's
		// sequence and would lose to the spelled one.
		for _, arm := range append([]*SelectStmt{in.Sub}, compoundArmStmts(in.Sub)...) {
			for _, c := range arm.Columns {
				if c.Star || exprHasCollate(c.Expr) {
					return false, true
				}
			}
		}
		return true, false
	}
	need, explicit := false, false
	for _, o := range others {
		if isRef(o) {
			continue
		}
		if !binaryColl(o) {
			need = true
		}
		if exprHasCollate(o) {
			explicit = true
		}
	}
	if need && explicit {
		// One operand's explicit COLLATE beats the copy and another's
		// declared sequence loses to it: one spelling cannot say both.
		return false, true
	}
	return need, false
}

// exprHasCollate reports a COLLATE anywhere in e (EP_Collate propagates up
// from it), or a node r41Walk does not know.
func exprHasCollate(e Expr) bool {
	found := false
	if !r41Walk(e, func(n Expr) {
		if _, ok := n.(CollateExpr); ok {
			found = true
		}
	}) {
		return true
	}
	return found
}

// refMayHaveAffinity reports whether the expression column o refers to
// (through unary plus) may carry an affinity of its own: a column, a CAST, a
// scalar subquery, under any COLLATE. A CAST over the reference brings its
// own and is answered false.
func (f *r41Flat) refMayHaveAffinity(o Expr) bool {
	for {
		if u, ok := o.(UnaryExpr); ok && u.Op == "+" {
			o = u.X
			continue
		}
		break
	}
	ce, ok := o.(ColumnExpr)
	if !ok {
		return false
	}
	k, ok := f.byName[r33sFoldIdent(ce.Name)]
	if !ok {
		return true
	}
	x := f.cols[k].expr
	for {
		if c, ok := x.(CollateExpr); ok {
			x = c.X
			continue
		}
		break
	}
	switch x.(type) {
	case ColumnExpr, CastExpr, SubqueryExpr:
		return true
	}
	return false
}

// isExprColRef reports a parent reference to an expression column of the
// derived table, also under CAST or unary plus, which sqlite3ExprCollSeq looks
// through to the copy's sequence.
func (f *r41Flat) isExprColRef(e Expr) bool {
	for {
		switch x := e.(type) {
		case CastExpr:
			e = x.X
			continue
		case UnaryExpr:
			if x.Op == "+" {
				e = x.X
				continue
			}
		}
		break
	}
	ce, ok := e.(ColumnExpr)
	if !ok {
		return false
	}
	if ce.Qualifier != "" && (f.dName == "" || !equalFoldName(ce.Qualifier, f.dName)) {
		return false
	}
	k, ok := f.byName[r33sFoldIdent(ce.Name)]
	return ok && f.cols[k].expr != nil && !f.cols[k].colRef
}

// wrapComparedLeft returns stmt with every operand copyColl flagged for wrap
// given an explicit COLLATE BINARY. That is the copy's implicit BINARY
// (substExpr, select.c:3858-3866), and spelled explicitly it decides the
// comparison the same way, since no other operand carries an explicit COLLATE
// there (copyColl declines that). false for a clause holding such a node
// inside one this does not rebuild.
func (f *r41Flat) wrapComparedLeft(stmt *SelectStmt, binaryColl func(Expr) bool) (*SelectStmt, bool) {
	needs := func(e Expr) bool {
		w, _ := f.copyColl(e, binaryColl)
		return w
	}
	wrapped := func(e Expr) Expr { return CollateExpr{X: e, Name: "BINARY"} }
	ok := true
	var rw func(Expr) Expr
	list := func(es []Expr) []Expr {
		if es == nil {
			return nil
		}
		out := make([]Expr, len(es))
		for k, x := range es {
			out[k] = rw(x)
		}
		return out
	}
	rw = func(e Expr) Expr {
		if !ok || e == nil {
			return e
		}
		wrap := needs(e)
		switch x := e.(type) {
		case LiteralExpr, ParamExpr, ColumnExpr:
			return e
		case BinaryExpr:
			x.L, x.R = rw(x.L), rw(x.R)
			if wrap {
				x.L = wrapped(x.L)
			}
			return x
		case UnaryExpr:
			x.X = rw(x.X)
			return x
		case CastExpr:
			x.X = rw(x.X)
			return x
		case CollateExpr:
			x.X = rw(x.X)
			return x
		case IsNullExpr:
			x.X = rw(x.X)
			return x
		case InExpr:
			if x.Sub != nil && !f.subqueryUntouched(x.Sub) {
				break
			}
			x.X, x.List = rw(x.X), list(x.List)
			if wrap {
				x.X = wrapped(x.X)
			}
			return x
		case BetweenExpr:
			x.X, x.Lo, x.Hi = rw(x.X), rw(x.Lo), rw(x.Hi)
			if wrap {
				x.X = wrapped(x.X)
			}
			return x
		case LikeExpr:
			x.X, x.Pattern, x.Escape = rw(x.X), rw(x.Pattern), rw(x.Escape)
			return x
		case GlobExpr:
			x.X, x.Pattern = rw(x.X), rw(x.Pattern)
			return x
		case RowExpr:
			x.Elems = list(x.Elems)
			return x
		case CaseExpr:
			x.Base, x.Else = rw(x.Base), rw(x.Else)
			whens := make([]WhenClause, len(x.Whens))
			for k, w := range x.Whens {
				whens[k] = WhenClause{When: rw(w.When), Then: rw(w.Then)}
			}
			x.Whens = whens
			if wrap {
				x.Base = wrapped(x.Base)
			}
			return x
		case FuncExpr:
			if len(x.OrderBy) == 0 && x.Over == nil {
				x.Args = list(x.Args)
				x.Filter = rw(x.Filter)
				return x
			}
		}
		// A node not rebuilt here: refused if it holds a node to wrap. A
		// subquery naming nothing of the derived table holds none.
		hit := false
		if !r41WalkQ(e, func(n Expr) {
			if needs(n) {
				hit = true
			}
		}, f.subqueryUntouched) || hit {
			ok = false
		}
		return e
	}
	out := *stmt
	out.Columns = make([]SelectColumn, len(stmt.Columns))
	for k, c := range stmt.Columns {
		if !c.Star {
			c.Expr = rw(c.Expr)
		}
		out.Columns[k] = c
	}
	out.Where, out.Having = rw(stmt.Where), rw(stmt.Having)
	out.From = append([]FromItem(nil), stmt.From...)
	for k := range out.From {
		out.From[k].On = rw(out.From[k].On)
		out.From[k].TableFuncArgs = list(out.From[k].TableFuncArgs)
	}
	out.GroupBy = list(stmt.GroupBy)
	if stmt.OrderBy != nil {
		out.OrderBy = make([]OrderTerm, len(stmt.OrderBy))
		for k, ot := range stmt.OrderBy {
			ot.Expr = rw(ot.Expr)
			out.OrderBy[k] = ot
		}
	}
	return &out, ok
}
