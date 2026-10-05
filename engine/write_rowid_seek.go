package engine

// A rowid point lookup for a write.
//
// When a WHERE pins a table's rowid (or INTEGER PRIMARY KEY) to a constant,
// seek that one row instead of walking the table. The conjunct stays in WHERE.
// This mirrors detectRowidSeekKey, reusing its rules: isSeekKeyCandidate and
// resolveInScopes, so "rowid" references resolve the same way throughout compile.
func detectWriteRowidSeekKey(c *compiler, tbl *tableMeta, cursor int, where Expr) (Expr, bool) {
	if where == nil || tbl == nil || tbl.withoutRowid {
		return nil, false
	}
	for _, cj := range splitTopLevelAnd(where) {
		be, ok := cj.(BinaryExpr)
		if !ok || (be.Op != "=" && be.Op != "==") {
			continue
		}
		if writeRowidRef(c, tbl, cursor, be.L) && isSeekKeyCandidate(be.R) {
			return be.R, true
		}
		if writeRowidRef(c, tbl, cursor, be.R) && isSeekKeyCandidate(be.L) {
			return be.L, true
		}
	}
	return nil, false
}

// writeRowidRef is isScopeRowidRef over a *tableMeta: e names cursor's rowid,
// either through the rowid/_rowid_/oid pseudo-column or through the table's
// INTEGER PRIMARY KEY column. A three-part schema-qualified reference is
// conservatively declined, as it is there.
func writeRowidRef(c *compiler, tbl *tableMeta, cursor int, e Expr) bool {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.UsingRepr || ce.Schema != "" {
		return false
	}
	cur, colIdx, isRowid, found, _, hard := resolveInScopes(c.scopes, ce, c.pager)
	if hard != nil || !found || cur != cursor {
		return false
	}
	if isRowid {
		return true
	}
	return tbl.ipkIndex >= 0 && colIdx == tbl.ipkIndex
}

// emitWriteRowidSeekHint emits the hint when where pins cursor's rowid, and
// reports whether it did. The key is a literal or a bound parameter, so it is
// column-free and can be evaluated before the loop opens -- no cursor is
// positioned yet.
//
// A non-integer key is handled at RUN time by OpSeekRowidHint, which leaves the
// cursor in full-scan mode: only an integer can equal a rowid.
func emitWriteRowidSeekHint(c *compiler, tbl *tableMeta, cursor int, where Expr) bool {
	key, ok := detectWriteRowidSeekKey(c, tbl, cursor, where)
	if !ok {
		return false
	}
	reg, err := c.compileExpr(key)
	if err != nil {
		// Declining costs a full scan, which is where this started.
		return false
	}
	c.emit(Instruction{Op: OpSeekRowidHint, P1: cursor, P2: reg})
	return true
}
