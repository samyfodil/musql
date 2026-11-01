package engine

// wherePlanExplain is one loop's EXPLAIN QUERY PLAN line as
// sqlite3WhereAddExplainText words it; the zero value is a loop not worded.
type wherePlanExplain struct {
	verb  string // "SEARCH" or "SCAN"
	name  string // the item as %S prints it (explainItemName), "" for the table's
	using string // " USING INDEX i (a=? AND b>?)", or nothing
}

// line is the detail text, the table named name unless the item names itself.
func (e wherePlanExplain) line(name string) string {
	if e.name != "" {
		name = e.name
	}
	return e.verb + " " + name + e.using
}

// explainItemName is a FROM item as printf.c's %S writes it: its alias, else
// its name as written, behind the schema it was qualified with.
func explainItemName(f FromItem) string {
	switch {
	case f.Alias != "":
		return f.Alias
	case f.Schema != "":
		return f.Schema + "." + f.Table
	}
	return f.Table
}

// accessOf is the access path of the winning loop w: its equality prefix past
// any skip-scan columns, and its range column.
func (b *wherePlanIdxBuild) accessOf(w *whereIdxLoop) wherePlanAccess {
	if w == nil || w.multiOr {
		return wherePlanAccess{}
	}
	if w.autoIdx {
		return wherePlanAccess{known: true, auto: true}
	}
	idx := w.idx
	if idx == nil || w.nEq > len(w.lTerm) {
		return wherePlanAccess{}
	}
	col := func(j int) int {
		switch {
		case idx.ipk:
			return xnRowid
		case j < len(idx.aiColumn):
			return idx.aiColumn[j]
		}
		return xnExpr
	}
	a := wherePlanAccess{known: true, index: !idx.ipk}
	for j := w.nSkip; j < w.nEq; j++ {
		a.eq = append(a.eq, col(j))
	}
	if len(w.lTerm) > w.nEq {
		a.rangeCol, a.hasRange = col(w.nEq), true
	}
	return a
}

// explainLoop words the winning loop of a one-table plan. Multi-index OR,
// automatic and expression indexes, and vector range bounds are not worded.
func (b *wherePlanIdxBuild) explainLoop(w *whereIdxLoop) wherePlanExplain {
	if w == nil || w.multiOr || w.autoIdx || w.idx == nil || w.nEq > len(w.lTerm) {
		return wherePlanExplain{}
	}
	idx := w.idx
	btm, top := false, false
	for _, ti := range w.lTerm[w.nEq:] {
		if ti < 0 || ti >= len(b.terms) || b.terms[ti].vector {
			return wherePlanExplain{}
		}
		switch op := b.terms[ti].op; {
		case op&(woGT|woGE) != 0:
			btm = true
		case op&(woLT|woLE) != 0:
			top = true
		default:
			return wherePlanExplain{}
		}
	}
	e := wherePlanExplain{verb: "SCAN"}
	if btm || top || w.nEq > 0 || b.sort != nil && b.sort.minMax {
		e.verb = "SEARCH"
	}
	if idx.ipk {
		switch {
		case w.nEq > 0:
			e.using = " USING INTEGER PRIMARY KEY (rowid=?)"
		case btm && top:
			e.using = " USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)"
		case btm:
			e.using = " USING INTEGER PRIMARY KEY (rowid>?)"
		case top:
			e.using = " USING INTEGER PRIMARY KEY (rowid<?)"
		}
		return e
	}
	if idxHasExpr(idx) || idx.primaryKey || w.nEq+1 > len(idx.aiColumn) && (btm || top) {
		return wherePlanExplain{}
	}
	cols := b.items[w.iTab].cols
	name := func(j int) string {
		switch c := idx.aiColumn[j]; {
		case c == xnRowid:
			return "rowid"
		case c >= 0 && c < len(cols):
			return cols[c].Name
		}
		return "<expr>"
	}
	e.using = " USING INDEX " + idx.name
	if w.idxOnly {
		e.using = " USING COVERING INDEX " + idx.name
	}
	if w.nEq == 0 && !btm && !top {
		return e
	}
	// explainIndexRange.
	r := " ("
	for i := 0; i < w.nEq; i++ {
		if i > 0 {
			r += " AND "
		}
		if i >= w.nSkip {
			r += name(i) + "=?"
		} else {
			r += "ANY(" + name(i) + ")"
		}
	}
	and := w.nEq > 0
	if btm {
		if and {
			r += " AND "
		}
		r += name(w.nEq) + ">?"
		and = true
	}
	if top {
		if and {
			r += " AND "
		}
		r += name(w.nEq) + "<?"
	}
	e.using += r + ")"
	return e
}
