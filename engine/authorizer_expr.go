package engine

import "strings"

// expr resolves the names in e in sqlite3WalkExpr's order: a node before its
// operands, a subquery before the operand it is compared with.
func (a *authPass) expr(e Expr, sc *authScope, w *authWith) {
	a.exprNamed(e, sc, w, "")
}

// exprNamed is expr for a result column, whose name an IGNOREd read keeps.
func (a *authPass) exprNamed(e Expr, sc *authScope, w *authWith, name string) {
	if e == nil || a.stopped() {
		return
	}
	switch x := e.(type) {
	case ColumnExpr:
		a.column(x, sc, name)
	case FuncExpr:
		a.function(x, sc, w, name)
	case UnaryExpr:
		a.expr(x.X, sc, w)
	case BinaryExpr:
		a.expr(x.L, sc, w)
		a.expr(x.R, sc, w)
	case IsNullExpr:
		a.expr(x.X, sc, w)
	case InExpr:
		if x.Sub != nil {
			a.subquery(x.Sub, sc, w)
		}
		a.expr(x.X, sc, w)
		for _, l := range x.List {
			a.expr(l, sc, w)
		}
	case BetweenExpr:
		a.expr(x.X, sc, w)
		a.expr(x.Lo, sc, w)
		a.expr(x.Hi, sc, w)
	case LikeExpr:
		a.patternCall("like", x.Pattern, x.X, x.Escape, sc, w)
	case GlobExpr:
		a.patternCall("glob", x.Pattern, x.X, nil, sc, w)
	case MatchExpr:
		a.patternCall("match", x.Pattern, x.X, nil, sc, w)
	case CollateExpr:
		a.expr(x.X, sc, w)
	case CastExpr:
		a.expr(x.X, sc, w)
	case CaseExpr:
		if x.iif != "" {
			switch a.check(AuthFunction, "", strings.ToLower(x.iif), "") {
			case AuthDeny:
				a.err = &authError{"engine: not authorized to use function: " + x.iif}
				return
			case AuthIgnore:
				a.ignore(x.authRef, name)
				return
			}
		}
		a.expr(x.Base, sc, w)
		for _, wc := range x.Whens {
			a.expr(wc.When, sc, w)
			a.expr(wc.Then, sc, w)
		}
		a.expr(x.Else, sc, w)
	case SubqueryExpr:
		a.subquery(x.Stmt, sc, w)
	case ExistsExpr:
		a.subquery(x.Stmt, sc, w)
		if sc.sel != nil && len(sc.sel.exprSubs) > 0 {
			sc.sel.exprSubs[len(sc.sel.exprSubs)-1].exists = true
		}
	case RowExpr:
		for _, el := range x.Elems {
			a.expr(el, sc, w)
		}
	case RaiseExpr:
		a.expr(x.Msg, sc, w)
	}
}

// patternCall is LIKE, GLOB or MATCH, which C parses as a call with the
// pattern first.
func (a *authPass) patternCall(name string, pattern, x, esc Expr, sc *authScope, w *authWith) {
	switch a.check(AuthFunction, "", name, "") {
	case AuthDeny:
		a.err = &authError{"engine: not authorized to use function: " + strings.ToUpper(name)}
		return
	case AuthIgnore:
		a.err = errAuthIgnoreUnsupported
		return
	}
	a.expr(pattern, sc, w)
	a.expr(x, sc, w)
	a.expr(esc, sc, w)
}

// funcAuthName is the name C registered a function under, which the FUNCTION
// action passes.
func funcAuthName(f FuncExpr) string {
	if f.CurrentTimeKw {
		switch f.Name {
		case "date":
			return "current_date"
		case "time":
			return "current_time"
		}
		return "current_timestamp"
	}
	return strings.ToLower(f.Name)
}

// function is resolveExprStep's TK_FUNCTION: the FUNCTION action, then its
// arguments, its ORDER BY, FILTER and window.
func (a *authPass) function(f FuncExpr, sc *authScope, w *authWith, name string) {
	switch a.check(AuthFunction, "", funcAuthName(f), "") {
	case AuthDeny:
		written := f.NameAsWritten
		if f.CurrentTimeKw && f.authRef > 0 && int(f.authRef) <= len(a.spans) {
			sp := a.spans[f.authRef-1]
			written = a.src[sp.start:sp.end]
		}
		if written == "" {
			written = f.Name
		}
		a.err = &authError{"engine: not authorized to use function: " + written}
		return
	case AuthIgnore:
		a.ignore(f.authRef, name)
		return
	}
	for _, arg := range f.Args {
		a.expr(arg, sc, w)
	}
	for _, o := range f.OrderBy {
		a.expr(o.Expr, sc, w)
	}
	a.expr(f.Filter, sc, w)
	if spec := f.Over; spec != nil {
		if sc.sel != nil {
			sc.sel.window = true
		}
		if spec.Ref != "" && sc.sel != nil {
			for _, nw := range sc.sel.s.Windows {
				if equalFoldName(nw.Name, spec.Ref) && nw.Spec != nil {
					a.windowSpec(nw.Spec, sc, w)
				}
			}
		}
		a.windowSpec(spec, sc, w)
	}
}

func (a *authPass) windowSpec(spec *WindowSpec, sc *authScope, w *authWith) {
	for _, p := range spec.PartitionBy {
		a.expr(p, sc, w)
	}
	for _, o := range spec.OrderBy {
		a.expr(o.Expr, sc, w)
	}
	if fr := spec.Frame; fr != nil {
		a.expr(fr.Start.Offset, sc, w)
		a.expr(fr.End.Offset, sc, w)
	}
}

// subquery resolves an expression subquery in sc, and leaves it for codegen.
func (a *authPass) subquery(s *SelectStmt, sc *authScope, w *authWith) {
	sub := a.resolveSelect(s, sc, w, "", false)
	if sc.sel != nil {
		sc.sel.exprSubs = append(sc.sel.exprSubs, sub)
	}
}

// column is lookupName: the scopes from the innermost out, each one's FROM
// items, then the result columns it may name, then a trigger's NEW and OLD.
func (a *authPass) column(c ColumnExpr, sc *authScope, name string) {
	for s := sc; s != nil; s = s.outer {
		if s.trig != nil && c.Qualifier != "" && c.Schema == "" &&
			(equalFoldName(c.Qualifier, "new") || equalFoldName(c.Qualifier, "old")) && !s.hasItem(c.Qualifier) {
			t := s.trig.tab
			col := c.Name
			if i := t.colIndex(col); i >= 0 && i != t.ipk {
				col = t.cols[i]
			} else if i >= 0 || isRowidAliasName(col) {
				col = t.rowidName()
			} else {
				a.halt = true
				return
			}
			if a.readCol(t.name, col, t.db) == AuthIgnore {
				a.ignore(c.authRef, name)
			}
			return
		}
		if s.excl && c.Qualifier != "" && c.Schema == "" && equalFoldName(c.Qualifier, "excluded") && !s.hasItem(c.Qualifier) {
			return
		}
		if it, col, found, bad := s.find(c); bad {
			a.halt = true
			return
		} else if found {
			if it != nil {
				a.readItemCol(it, col, c.authRef, name)
			}
			return
		}
		if c.Qualifier == "" && s.aliases != nil {
			for _, rc := range s.aliases {
				if rc.HasAlias && equalFoldName(rc.Alias, c.Name) {
					a.aliasRead(rc.Expr, s, c.authRef, name)
					return
				}
			}
		}
	}
	if c.Qualifier == "" && c.FallbackLiteral != nil {
		return
	}
	a.halt = true // C fails with "no such column"
}

// aliasRead is a reference to a result column by its alias: resolveAlias
// copies the result's expression, and a copy of a column read is read again.
func (a *authPass) aliasRead(e Expr, s *authScope, ref int32, name string) {
	c, ok := e.(ColumnExpr)
	if !ok {
		return
	}
	saved := s.aliases
	s.aliases = nil
	if it, col, found, _ := s.find(c); found && it != nil {
		if it.tab != nil {
			idx := it.tab.colIndex(col)
			if idx < 0 || idx == it.tab.ipk {
				col = it.tab.rowidName()
			} else {
				col = it.tab.cols[idx]
			}
			if a.readCol(it.tab.name, col, it.tab.db) == AuthIgnore {
				a.ignore(ref, name)
			}
		}
	}
	s.aliases = saved
}

func (s *authScope) hasItem(name string) bool {
	for _, it := range s.items {
		if equalFoldName(it.name, name) {
			return true
		}
	}
	return false
}

// find looks c up among s's FROM items. bad is a reference C rejects, an
// ambiguous one.
func (s *authScope) find(c ColumnExpr) (it *authItem, col string, found, bad bool) {
	var match *authItem
	n := 0
	for _, cand := range s.items {
		if c.Qualifier != "" {
			if !equalFoldName(cand.name, c.Qualifier) {
				continue
			}
			if c.Schema != "" && (cand.tab == nil || !equalFoldName(cand.tab.db, c.Schema) && !(equalFoldName(c.Schema, "main") && cand.tab.db == "main")) {
				continue
			}
		}
		if !cand.hasCol(c.Name) {
			continue
		}
		if match != nil && cand.merged[strings.ToLower(c.Name)] && c.Qualifier == "" {
			continue
		}
		if match == nil {
			match = cand
		}
		n++
	}
	if n > 1 {
		return nil, "", false, true
	}
	if match != nil {
		return match, c.Name, true, false
	}
	if isRowidAliasName(c.Name) {
		var only *authItem
		cnt := 0
		for _, cand := range s.items {
			if c.Qualifier != "" && !equalFoldName(cand.name, c.Qualifier) {
				continue
			}
			only = cand
			cnt++
		}
		if cnt == 1 && only.tab != nil && only.tab.view == nil && only.tab.rowid {
			return only, c.Name, true, false
		}
	}
	return nil, "", false, false
}
