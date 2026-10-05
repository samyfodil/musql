// This file implements fts5's table-valued call form: the MATCH query as an
// argument.
//
//	SELECT * FROM t1('one AND two')   ==   SELECT * FROM t1 WHERE t1 MATCH 'one AND two'
//
// It rewrites the first spelling into the second before compiling, so it
// inherits every MATCH rule (including the join-placement rule, ticket
// [7c0e06b16]) instead of reimplementing them. Over t1(a,b):
//
//	SELECT * FROM t1('two')              the TEXT columns only, exactly as a
//	                                     plain "SELECT * FROM t1" gives
//	SELECT rowid,a,b FROM t1('two')      rowids 1,2 -- the MATCH rows
//	SELECT rowid FROM t1('one AND two')  1
//	SELECT rowid FROM t1('t*')           1,2
//	SELECT count(*) FROM t1('nosuch')    0
//	SELECT rowid FROM t1('two') AS x     works, and "x.rowid" resolves through
//	                                     the alias -- so the MATCH this file
//	                                     builds must name the ALIAS, not the
//	                                     table, whenever there is one
//	SELECT rowid FROM t1()               1,2,3 -- NO argument is a plain scan,
//	                                     not an empty query
//
// Declined, falling through to "no such table-valued function":
//
//   - the two-argument form, whose second argument is a rank function ("parse
//     error in rank function: extra" in C);
//   - most non-literal arguments: the fts5 query is resolved at prepare time,
//     so a column or general expression cannot become one. fts5_locale(<lit>,
//     <lit>) is the exception; see fts5TVFPatternArg.
package engine

import "strings"

// fts5RewriteTableFunc returns stmt with every "FROM <fts5 table>(<query>)"
// turned into a plain scan plus a MATCH conjunct on that SELECT's WHERE, or
// stmt unchanged. It recurses through derived tables, CTE bodies and compound
// arms, since the VDBE compiles the whole tree in one pass. A subquery inside
// an expression (IN, EXISTS, scalar) is not rewritten and declines. The
// statement is copied, not mutated: parsed statements are shared.
func (p *ReadOnlyPager) fts5RewriteTableFunc(stmt *SelectStmt) *SelectStmt {
	if stmt == nil || p == nil {
		return stmt
	}
	// Recurse first, so a rewritten child forces this statement to be copied
	// even when its own FROM list needs nothing.
	newFromSub, subChanged := p.fts5RewriteSubqueries(stmt.From)
	newCTEs, cteChanged := p.fts5RewriteCTEs(stmt.CTEs)
	newCompound, compChanged := p.fts5RewriteCompound(stmt.Compound)
	if subChanged || cteChanged || compChanged {
		cp := *stmt
		cp.From = newFromSub
		cp.CTEs = newCTEs
		cp.Compound = newCompound
		stmt = &cp
	}
	// Fast path: almost no statement has a table-valued call at all, and
	// isFts5Table would otherwise read the schema for every SELECT.
	found := false
	for _, it := range stmt.From {
		if it.TableFunc && it.Subquery == nil && it.Table != "" {
			found = true
			break
		}
	}
	if !found {
		return stmt
	}

	newFrom := make([]FromItem, len(stmt.From))
	copy(newFrom, stmt.From)
	var match Expr
	changed := false
	for i, it := range newFrom {
		if !it.TableFunc || it.Subquery != nil || it.Table == "" {
			continue
		}
		if !p.isFts5Table(it.Table) {
			continue
		}
		switch len(it.TableFuncArgs) {
		case 0:
			// "FROM t1()" is a plain scan (verified above).
		case 1:
			// A literal of ANY type, or an fts5_locale(<locale-lit>,
			// <text-lit>) call, PROVABLY non-NULL -- see
			// fts5TVFPatternArg's own doc comment for exactly what qualifies
			// and why. A NULL argument is an ERROR through this call syntax
			// ("FROM t1(NULL)" is `fts5: syntax error near ""`, verified),
			// where a rewritten "MATCH NULL" would quietly select no rows
			// here -- the exact shape of wrong answer this project exists to
			// prevent -- so anything that is not PROVABLY non-NULL falls
			// through to the existing decline, and the two engines go on
			// agreeing by both rejecting it.
			pat, ok := fts5TVFPatternArg(it.TableFuncArgs[0])
			if !ok {
				continue
			}
			// The hidden column this binds to is named for the TABLE, never
			// for an alias (sqlite3Fts5ConfigDeclareVtab, fts5_config.c:765),
			// so an aliased item takes the QUALIFIED spelling "<alias>.<table>"
			// -- "FROM t1('two') AS x" is "FROM t1 AS x WHERE x.t1 MATCH 'two'".
			// Naming the ALIAS instead resolved only because fts5MatchTarget
			// used to key the whole-table form on the scope name, which also
			// answered rows for the "x MATCH 'two'" C SQLite rejects.
			x := ColumnExpr{Name: it.Table}
			if it.Alias != "" {
				x.Qualifier = it.Alias
			}
			m := Expr(MatchExpr{X: x, Pattern: pat})
			if match == nil {
				match = m
			} else {
				match = BinaryExpr{Op: "AND", L: match, R: m}
			}
		default:
			// The rank-function form: left to decline.
			continue
		}
		newFrom[i].TableFunc = false
		newFrom[i].TableFuncArgs = nil
		changed = true
	}
	if !changed {
		return stmt
	}
	cp := *stmt
	cp.From = newFrom
	if match != nil {
		if cp.Where == nil {
			cp.Where = match
		} else {
			// The rewritten MATCH goes on the RIGHT, so the user's own WHERE
			// keeps its position as the left operand -- splitTopLevelAnd sees
			// both as top-level conjuncts either way, which is what the MATCH
			// placement rule needs.
			cp.Where = BinaryExpr{Op: "AND", L: cp.Where, R: match}
		}
	}
	return &cp
}

// fts5TVFPatternArg reports whether arg can safely become a MATCH pattern
// without the "zero rows instead of an error" trap a NULL sets. ok only for:
//
//   - a TEXT literal;
//   - a non-negative INTEGER literal: fts5 reads the argument's text
//     (fts5ExtractExprText, fts5_main.c:1411), as evalMatch does for "t MATCH
//     0", and digits lex the same on both sides ("t0(0)", fts5misc.test). A
//     REAL or negative literal stringifies with "." or "-", which this
//     engine's fts5 lexer accepts where C's rejects -- an existing gap,
//     reachable via "t MATCH -5", that this rewrite does not widen;
//   - fts5_locale(<lit>, <lit>) with non-NULL literal arguments
//     (fts5faultI.test): fts5LocaleFunc is pure, so checking its result for
//     NULL here is safe, and the original call is kept as the pattern.
//
// Anything else (a column, a parameter, REAL/negative/BLOB/NULL literals, other
// calls) stays declined.
func fts5TVFPatternArg(arg Expr) (Expr, bool) {
	if lit, isLit := arg.(LiteralExpr); isLit {
		switch lit.Val.Typ {
		case Text:
			return arg, true
		case Int:
			return arg, lit.Val.I >= 0
		default:
			return nil, false
		}
	}
	fn, isFunc := arg.(FuncExpr)
	if !isFunc || fn.Star || fn.Distinct || fn.Over != nil || fn.Filter != nil || len(fn.OrderBy) > 0 {
		return nil, false
	}
	if !fts5LocaleFuncName(fn.Name) || len(fn.Args) != 2 {
		return nil, false
	}
	loc, ok1 := fn.Args[0].(LiteralExpr)
	txt, ok2 := fn.Args[1].(LiteralExpr)
	if !ok1 || !ok2 {
		return nil, false
	}
	res, err := fts5LocaleFunc([]Value{loc.Val, txt.Val})
	if err != nil || res.Typ == Null {
		return nil, false
	}
	return arg, true
}

// fts5RewriteSubqueries rewrites every derived-table FROM item's own SELECT.
func (p *ReadOnlyPager) fts5RewriteSubqueries(from []FromItem) ([]FromItem, bool) {
	var out []FromItem
	for i, it := range from {
		if it.Subquery == nil {
			continue
		}
		ns := p.fts5RewriteTableFunc(it.Subquery)
		if ns == it.Subquery {
			continue
		}
		if out == nil {
			out = make([]FromItem, len(from))
			copy(out, from)
		}
		out[i].Subquery = ns
	}
	if out == nil {
		return from, false
	}
	return out, true
}

// fts5RewriteCTEs rewrites every WITH-clause CTE body.
func (p *ReadOnlyPager) fts5RewriteCTEs(ctes []CTEDef) ([]CTEDef, bool) {
	var out []CTEDef
	for i, c := range ctes {
		ns := p.fts5RewriteTableFunc(c.Select)
		if ns == c.Select {
			continue
		}
		if out == nil {
			out = make([]CTEDef, len(ctes))
			copy(out, ctes)
		}
		out[i].Select = ns
	}
	if out == nil {
		return ctes, false
	}
	return out, true
}

// fts5RewriteCompound rewrites every UNION/INTERSECT/EXCEPT arm.
func (p *ReadOnlyPager) fts5RewriteCompound(arms []CompoundArm) ([]CompoundArm, bool) {
	var out []CompoundArm
	for i, a := range arms {
		ns := p.fts5RewriteTableFunc(a.Stmt)
		if ns == a.Stmt {
			continue
		}
		if out == nil {
			out = make([]CompoundArm, len(arms))
			copy(out, arms)
		}
		out[i].Stmt = ns
	}
	if out == nil {
		return arms, false
	}
	return out, true
}

// fts5RewriteEqMatch turns "<fts5 table> = <expr>" into "<fts5 table> MATCH
// <expr>", as fts5's xBestIndex treats it (fts5_main.c:653-655):
//
//	if( p->op==SQLITE_INDEX_CONSTRAINT_MATCH
//	 || (p->op==SQLITE_INDEX_CONSTRAINT_EQ && iCol>=nCol)
//	){
//	  /* A MATCH operator or equivalent */
//
// iCol>=nCol are the hidden columns fts5 declares (fts5_config.c:765): one
// named for the table, and "rank". So "SELECT rowid FROM t1 WHERE rowid=2 AND
// t1 = 'hello'" (fts5misc.test) is a match. Only the table-named column is
// rewritten, only for an fts5 table in this FROM (fts5 forbids a column with
// its table's name). "rank = <expr>" selects a ranking function and is left
// alone.
func (p *ReadOnlyPager) fts5RewriteEqMatch(stmt *SelectStmt) *SelectStmt {
	if stmt == nil || p == nil || stmt.Where == nil {
		return stmt
	}
	// Only a bare (unaliased) fts5 FROM item can be named this way; an aliased
	// one takes the qualified "<alias>.<table>" spelling, which resolves as an
	// ordinary qualified column reference already (see fts5MatchTarget).
	names := map[string]bool{}
	for _, it := range stmt.From {
		if it.Subquery != nil || it.Table == "" || it.Alias != "" {
			continue
		}
		if p.isFts5Table(it.Table) {
			names[r33sFoldIdent(it.Table)] = true
		}
	}
	if len(names) == 0 {
		return stmt
	}
	// A bare name another source exposes as a real column is ambiguous in
	// C ("SELECT count(*) FROM t2 JOIN ft ON ft = t2.ft" -> "ambiguous
	// column name: ft"), so it is dropped and ordinary resolution raises
	// the error.
	for _, it := range stmt.From {
		if it.Subquery != nil || it.Table == "" || p.isFts5Table(it.Table) {
			continue
		}
		rt, rerr := p.resolveTable(it.Table)
		if rerr != nil || rt == nil {
			continue
		}
		for _, c := range rt.cols {
			delete(names, r33sFoldIdent(c.Name))
		}
	}
	if len(names) == 0 {
		return stmt
	}
	rewritten, changed := fts5EqMatchInConjuncts(stmt.Where, names)
	if !changed {
		return stmt
	}
	cp := *stmt
	cp.Where = rewritten
	return &cp
}

// fts5EqMatchInConjuncts rewrites every top-level AND conjunct of e that is an
// "=" against one of names. It deliberately does not descend into OR, NOT or a
// nested expression: fts5 only reaches the equivalence through a constraint
// xBestIndex is offered, and those are the top-level AND terms.
func fts5EqMatchInConjuncts(e Expr, names map[string]bool) (Expr, bool) {
	b, ok := e.(BinaryExpr)
	if !ok {
		return e, false
	}
	if strings.EqualFold(b.Op, "AND") {
		l, lc := fts5EqMatchInConjuncts(b.L, names)
		r, rc := fts5EqMatchInConjuncts(b.R, names)
		if !lc && !rc {
			return e, false
		}
		return BinaryExpr{Op: b.Op, L: l, R: r}, true
	}
	if b.Op != "=" && b.Op != "==" {
		return e, false
	}
	// The column may be written on either side of the operator.
	if col, pat, ok := fts5EqMatchOperands(b.L, b.R, names); ok {
		return MatchExpr{X: col, Pattern: pat}, true
	}
	if col, pat, ok := fts5EqMatchOperands(b.R, b.L, names); ok {
		return MatchExpr{X: col, Pattern: pat}, true
	}
	return e, false
}

// fts5EqMatchOperands reports whether maybeCol is a BARE reference to one of
// names, in which case it is the table-named hidden column and other is the
// match pattern.
func fts5EqMatchOperands(maybeCol, other Expr, names map[string]bool) (ColumnExpr, Expr, bool) {
	ce, ok := maybeCol.(ColumnExpr)
	if !ok || ce.Schema != "" || ce.Qualifier != "" || !names[r33sFoldIdent(ce.Name)] {
		return ColumnExpr{}, nil, false
	}
	return ce, other, true
}

// fts5RewriteUsingMatch turns "JOIN <fts5 table> USING (<its name>)" into the
// ON-clause MATCH it means: the table-named column is hidden, so the USING
// equality is the MATCH-equivalent EQ (see fts5RewriteEqMatch). With t2(ft)
// holding 'hello' and fts5 ft holding 'hello world', "SELECT t2.ft, ft.rowid
// FROM t2 JOIN ft USING (ft)" returns one row (fts5misc.test).
//
// Rewriting to ON gives identical output: the fts5 side contributes no column
// of that name, so USING's coalescing removes nothing. Narrow: the fts5 item
// must be unaliased, the USING list exactly that name, and exactly one earlier
// item may expose it as a column (more is ambiguous in C).
func (p *ReadOnlyPager) fts5RewriteUsingMatch(stmt *SelectStmt) *SelectStmt {
	if stmt == nil || p == nil || len(stmt.From) < 2 {
		return stmt
	}
	var out []FromItem
	changed := false
	for i, it := range stmt.From {
		if i == 0 || len(it.Using) == 0 || it.Alias != "" || it.Subquery != nil || it.Table == "" {
			continue
		}
		if !p.isFts5Table(it.Table) {
			continue
		}
		self := r33sFoldIdent(it.Table)
		keep := it.Using[:0:0]
		var add Expr
		for _, n := range it.Using {
			if r33sFoldIdent(n) != self {
				keep = append(keep, n)
				continue
			}
			left, ok := p.fts5SoleEarlierColumnOwner(stmt.From[:i], n)
			if !ok {
				keep = append(keep, n) // leave it to decline as before
				continue
			}
			m := Expr(MatchExpr{
				X:       ColumnExpr{Name: it.Table},
				Pattern: ColumnExpr{Qualifier: left, Name: n},
			})
			if add == nil {
				add = m
			} else {
				add = BinaryExpr{Op: "AND", L: add, R: m}
			}
		}
		if add == nil {
			continue
		}
		if !changed {
			out = make([]FromItem, len(stmt.From))
			copy(out, stmt.From)
			changed = true
		}
		ni := out[i]
		// NIL, not an empty slice. A non-nil empty Using is the NATURAL
		// fallback's own shape in desugarJoinItem (join.go), which exposes the
		// columns and returns NO condition at all -- so leaving keep empty
		// here produced an unfiltered cross product: 4 rows where the oracle
		// answers 1.
		if len(keep) == 0 {
			keep = nil
		}
		ni.Using = keep
		if ni.On == nil {
			ni.On = add
		} else {
			ni.On = BinaryExpr{Op: "AND", L: ni.On, R: add}
		}
		out[i] = ni
	}
	if !changed {
		return stmt
	}
	cp := *stmt
	cp.From = out
	return &cp
}

// fts5SoleEarlierColumnOwner returns the scope name of the ONE earlier FROM
// item exposing col as an ordinary column, or ok=false when none or more than
// one does -- the latter being the ambiguity C SQLite reports, which this
// leaves to the ordinary path rather than resolving itself.
func (p *ReadOnlyPager) fts5SoleEarlierColumnOwner(earlier []FromItem, col string) (string, bool) {
	found := ""
	for _, it := range earlier {
		if it.Subquery != nil || it.Table == "" {
			return "", false // a derived table's columns are not known here
		}
		rt, err := p.resolveTable(it.Table)
		if err != nil || rt == nil {
			return "", false
		}
		if colIndexByName(rt.cols, col) < 0 {
			continue
		}
		if found != "" {
			return "", false
		}
		if found = it.Alias; found == "" {
			found = it.Table
		}
	}
	return found, found != ""
}
