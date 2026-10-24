package engine

import "sync/atomic"

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
		if writeRowidRef(c, tbl, cursor, be.L) && isWriteSeekKeyCandidate(be.R) {
			return be.R, true
		}
		if writeRowidRef(c, tbl, cursor, be.R) && isWriteSeekKeyCandidate(be.L) {
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
// A key that is no integer under numeric affinity fetches nothing at RUN time
// (OpSeekRowidHint): only an integer can equal a rowid.
func emitWriteRowidSeekHint(c *compiler, tbl *tableMeta, cursor int, where Expr) bool {
	key, ok := detectWriteRowidSeekKey(c, tbl, cursor, where)
	if !ok {
		return false
	}
	mark := len(c.insns)
	reg, err := c.compileExpr(key)
	if err != nil {
		// Declining costs a full scan, which is where this started.
		c.insns = c.insns[:mark]
		return false
	}
	if !keyRunsBeforeTheLoop(c.insns[mark:], cursor) {
		c.insns = c.insns[:mark]
		return false
	}
	c.emit(Instruction{Op: OpSeekRowidHint, P1: cursor, P2: reg})
	return true
}

// isWriteSeekKeyCandidate is isSeekKeyCandidate plus a scalar subquery: C
// codes an uncorrelated one once, as a constant, and seeks with its value
// ("DELETE FROM t WHERE id = (SELECT max(id) FROM t)" is one seek there, and
// was a scan of every row here). Whether this one is uncorrelated is only
// known once compiled -- keyRunsBeforeTheLoop.
func isWriteSeekKeyCandidate(e Expr) bool {
	if _, ok := e.(SubqueryExpr); ok {
		return true
	}
	return isSeekKeyCandidate(e)
}

// keyRunsBeforeTheLoop reports whether a seek key compiled to insns can run
// before the loop positions cursor: no correlated subquery (one re-runs
// against the enclosing frame) and no read of cursor itself. An uncorrelated
// subquery runs once and caches, so the WHERE that re-reads it on the row the
// seek finds compares the same value.
func keyRunsBeforeTheLoop(insns []Instruction, cursor int) bool {
	for _, in := range insns {
		switch in.Op {
		case OpSubquery, OpExists:
			if in.P5&p5Correlated != 0 {
				return false
			}
		case OpColumn, OpRowid:
			if in.P1 == cursor {
				return false
			}
		}
	}
	return true
}

// emitWriteIndexSeekHint is the column twin of emitWriteRowidSeekHint: for a
// WHERE conjunct "<col> = <column-free key>" on the written table it emits an
// OpSeekIndexHint, which the row-store cursor answers from the store's own
// equality index (row_store_eqindex.go) instead of materializing the table.
// It needs no declared index: the candidates come in rowid order, which is the
// order C visits one key's rows in whether it walks an index or the table, and
// the conjunct still runs on every fetched row, so the hint only narrows.
//
// Every such conjunct is a key, up to maxSeekKeys, as the hint's slots: the
// cursor asks the one whose column holds fewest rows (writeSeekRowids). The
// comparison's collation is the column's own, the key being a bare literal or
// parameter. The INTEGER PRIMARY KEY column is never a key: it is stored NULL,
// its value being the rowid.
//
// A key a conjunct that can raise comes before is kept only where C's own loop
// leaves it exact (seekGuard): from is the target as the statement names it,
// for asking the planner what that loop is.
func (db *DB) emitWriteIndexSeekHint(c *compiler, tbl *tableMeta, cursor int, from FromItem, where Expr, trig *trigCompileCtx) bool {
	if writeIndexSeekOffForTest || where == nil || tbl == nil || tbl.withoutRowid || hasGeneratedCols(tbl.cols) {
		return false
	}
	cjs := splitTopLevelAnd(where)
	guard := seekGuard{raiseAt: -1, tableRaiseAt: -1}
	for pos, cj := range cjs {
		if exprMayRaise(cj) && !containsSubquery(cj) && writeConjunctPerRow(cj) {
			guard.raiseAt, guard.tableRaiseAt = pos, pos
			guard.access = db.writeLoopAccess(tbl, from, where, trig)
			break
		}
	}
	slot := 0
	for pos, cj := range cjs {
		be, ok := cj.(BinaryExpr)
		if !ok || (be.Op != "=" && be.Op != "==") || slot == maxSeekKeys {
			continue
		}
		for _, side := range [2][2]Expr{{be.L, be.R}, {be.R, be.L}} {
			colExpr, key := side[0], side[1]
			col, ok := writeColumnRef(c, cursor, colExpr)
			if !ok || col == tbl.ipkIndex || !isSeekKeyCandidate(key) || !guard.keep(col, pos, db.leadsIndex(tbl, col)) {
				continue
			}
			reg, err := c.compileExpr(key)
			if err != nil {
				return slot > 0
			}
			c.emit(Instruction{Op: OpSeekIndexHint, P1: cursor, P2: reg,
				P4: &indexSeekHint{aff: comparisonAffinity(c.affCtx(), colExpr, key), coll: effectiveCollation(tbl.cols[col].Collation),
					leadingCol: col, slot: slot}})
			slot++
			break
		}
	}
	return slot > 0
}

// writeConsumedFirst is consumedFirst for a write's loop: loopWhere, the
// terms it tests per row, with the equalities C's one-pass loop consumes for
// where moved to the front when a term that can raise is among them.
func (db *DB) writeConsumedFirst(c *compiler, tbl *tableMeta, cursor int, from FromItem, where, loopWhere Expr, trig *trigCompileCtx) Expr {
	terms := splitTopLevelAnd(loopWhere)
	if len(terms) < 2 {
		return loopWhere
	}
	raising := false
	for _, cj := range terms {
		if exprMayRaise(cj) && !containsSubquery(cj) {
			raising = true
			break
		}
	}
	if !raising {
		return loopWhere
	}
	a := db.writeLoopAccess(tbl, from, where, trig)
	if !a.known || a.auto || len(a.eq) == 0 {
		return loopWhere
	}
	taken := make([]bool, len(terms))
	var out []Expr
	for _, col := range a.eq {
		for k, cj := range terms {
			if !taken[k] && writeEqualityOnColumn(c, tbl, cursor, cj, col) {
				taken[k] = true
				out = append(out, cj)
				break
			}
		}
	}
	if len(out) == 0 {
		return loopWhere
	}
	for k, cj := range terms {
		if !taken[k] {
			out = append(out, cj)
		}
	}
	return andConjuncts(out)
}

// writeEqualityOnColumn is equalityOnColumn over a write's target cursor.
func writeEqualityOnColumn(c *compiler, tbl *tableMeta, cursor int, cj Expr, col int) bool {
	be, ok := cj.(BinaryExpr)
	if !ok || be.Op != "=" && be.Op != "==" && be.Op != "IS" {
		return false
	}
	for _, side := range [2][2]Expr{{be.L, be.R}, {be.R, be.L}} {
		if !isSeekKeyCandidate(side[1]) {
			continue
		}
		if col == xnRowid && writeRowidRef(c, tbl, cursor, side[0]) {
			return true
		}
		if got, ok := writeColumnRef(c, cursor, side[0]); ok && got == col {
			return true
		}
	}
	return false
}

// writeConjunctPerRow reports whether a write tests conjunct e on every row
// rather than once before its loop: it names a column.
func writeConjunctPerRow(e Expr) bool {
	named := false
	forEachColumnRef(e, func(ColumnExpr) { named = true })
	return named
}

// leadsIndex reports whether a declared index on tbl leads with column col.
func (db *DB) leadsIndex(tbl *tableMeta, col int) bool {
	for _, idx := range db.indexes {
		if indexBelongsTo(idx, tbl) && len(idx.colIdx) > 0 && idx.colIdx[0] == col {
			return true
		}
	}
	return false
}

// writeColumnRef reports whether e is a plain reference to a stored column of
// cursor's table (not its rowid), returning the column's index.
func writeColumnRef(c *compiler, cursor int, e Expr) (int, bool) {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.UsingRepr || ce.Schema != "" {
		return 0, false
	}
	cur, colIdx, isRowid, found, fb, hard := resolveInScopes(c.scopes, ce, c.pager)
	if hard != nil || !found || isRowid || fb.has || cur != cursor {
		return 0, false
	}
	return colIdx, true
}

// writeIndexSeekOffForTest turns emitWriteIndexSeekHint off, so a test's
// reference is the full walk; writeIndexSeeksServed counts the seeks served.
var (
	writeIndexSeekOffForTest bool
	writeIndexSeeksServed    atomic.Int64
)
