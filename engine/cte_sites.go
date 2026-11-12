package engine

// cteSingleSite reports that the reference to b being planned is the first of
// b's references C codes: b's statement names it in one place only, in its own
// FROM clauses or in the body of one other CTE that is not AS NOT
// MATERIALIZED. C expands a CTE body once for every reference to it
// (CteUse.nUse, select.c:5767), so that place has as many copies as its
// enclosing CTE has references, all alike; the first one coded materializes
// b, and the others are never coded, as their enclosing CTE is coded once and
// reused. Recompiling a body here counts b's references again (nUse), which
// this does not read.
func cteSingleSite(b *cteBinding) bool {
	if b == nil || b.owner == nil || b.recursive != nil || b.multiSelfRef {
		return false
	}
	main := *b.owner
	main.CTEs = nil
	n, ok := cteRefsIn(&main, b.name)
	if !ok {
		return false
	}
	var encl *cteBinding
	for _, d := range b.owner.CTEs {
		if r33sFoldIdent(d.Name) == r33sFoldIdent(b.name) {
			continue
		}
		k, ok := cteRefsIn(d.Select, b.name)
		if !ok {
			return false
		}
		if k > 0 {
			n += k
			encl = b.frame[r33sFoldIdent(d.Name)]
		}
	}
	if n != 1 {
		return false
	}
	return encl == nil || encl.m10d != cteM10dNo
}

// cteRefsIn counts the FROM items of s and every select inside it that name
// the CTE name; false when a WITH inside s defines that name again.
func cteRefsIn(s *SelectStmt, name string) (int, bool) {
	key := r33sFoldIdent(name)
	n, ok := 0, true
	var from func([]FromItem)
	from = func(items []FromItem) {
		for _, it := range items {
			if it.Subquery == nil && !it.TableFunc && it.Schema == "" && r33sFoldIdent(it.Table) == key {
				n++
			}
			from(it.NestedGroupSpan)
		}
	}
	nfWalkSelects(s, func(sel *SelectStmt) {
		for _, d := range sel.CTEs {
			if r33sFoldIdent(d.Name) == key {
				ok = false
			}
		}
		from(sel.From)
	})
	return n, ok
}
