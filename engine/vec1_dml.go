package engine

// DELETE and UPDATE of a vec1 table: the rows they scan, and the points
// inside a transaction where vec1's writer writes what it holds.

import (
	"fmt"
	"slices"
	"strings"
)

// vec1WriteRows is the row source a DELETE or UPDATE of a vec1 table scans
// (vtabWriteRows): %_base in rowid order, in this engine's column layout. Its
// "distance" column is NULL: such a statement naming it declines
// (vec1DeclineDistanceRef), where vec1 would describe the row's encoding.
func (db *DB) vec1WriteRows(vm *vtabMeta, st *vec1Store) ([]int64, [][]Value, error) {
	w := db.vec1Writing(vm, st)
	if err := w.begin(); err != nil {
		return nil, nil, err
	}
	if err := w.t.loadIfRequired(w.sh); err != nil {
		return nil, nil, err
	}
	var flat map[int64][]byte
	if w.t.mod.isFlat() {
		var err error
		if flat, err = w.flatVectors(); err != nil {
			return nil, nil, err
		}
	}
	var rowids []int64
	var rows [][]Value
	err := w.sh.scan("_base", func(rowid int64, base []Value) (bool, error) {
		row := make([]Value, 5+st.nMeta)
		row[vec1ColRowid] = Value{Typ: Int, I: rowid}
		row[vec1ColCmd], row[vec1ColArg], row[vec1ColDistance] = Value{Typ: Null}, Value{Typ: Null}, Value{Typ: Null}
		if flat != nil {
			if v, ok := flat[rowid]; ok {
				row[vec1ColVector] = Value{Typ: Blob, S: v}
			} else {
				row[vec1ColVector] = Value{Typ: Null}
			}
		} else {
			row[vec1ColVector] = vec1OwnedValue(vec1Col(base, 1))
		}
		for i := range st.nMeta {
			row[vec1ColMeta+i] = vec1OwnedValue(vec1Col(base, 2+i))
		}
		rowids = append(rowids, rowid)
		rows = append(rows, row)
		return true, nil
	})
	return rowids, rows, err
}

// flatVectors is every vector of a flat index by rowid, as vec1FindByRowid
// would find each: a pending list first, then the bucket's lists in index
// order.
func (w *vec1W) flatVectors() (map[int64][]byte, error) {
	nElem := w.t.cfg.nElem
	out := map[int64][]byte{}
	take := func(rowid int64, v []byte) {
		if _, ok := out[rowid]; !ok {
			out[rowid] = append([]byte(nil), v...)
		}
	}
	if wr := w.t.writer; wr != nil {
		for _, b := range wr.bld {
			if !b.init {
				continue
			}
			for i := range b.nEntry() {
				take(b.rowidAt(i), b.data[4*nElem*i:4*nElem*(i+1)])
			}
		}
	}
	var lists []vec1IdxRow
	if err := w.sh.scan("_idx", func(id int64, row []Value) (bool, error) {
		lists = append(lists, vec1IdxRowOf(id, row))
		return true, nil
	}); err != nil {
		return nil, err
	}
	vec1IndexOrder(lists)
	slices.SortStableFunc(lists, func(a, b vec1IdxRow) int {
		switch {
		case a.bucket < b.bucket:
			return -1
		case a.bucket > b.bucket:
			return 1
		}
		return 0
	})
	for _, r := range lists {
		if !vec1ListSizeOK(w.t.mod, nElem, r.val) {
			return nil, errVec1Corrupt
		}
		nEntry := int(be.Uint32(r.val[4:]))
		sz := 4
		if be.Uint32(r.val)&vec1List64 != 0 {
			sz = 8
		}
		for e := range nEntry {
			var rowid int64
			if sz == 4 {
				rowid = int64(be.Uint32(r.val[vec1ListHdr+4*e:]))
				if rowid == vec1Tomb32 {
					continue
				}
			} else {
				rowid = int64(be.Uint64(r.val[vec1ListHdr+8*e:]))
				if rowid == vec1Tomb64 {
					continue
				}
			}
			off := vec1ListHdr + nEntry*sz + e*4*nElem
			take(rowid, r.val[off:off+4*nElem])
		}
	}
	return out, nil
}

// deleteVec1 is a DELETE of a vec1 table: xUpdate with argc 1 for each row
// the scan selected, in rowid order.
func (db *DB) deleteVec1(vm *vtabMeta, st *vec1Store, stmt *deleteStmt, pre vtabWriteRowSet) (int, error) {
	if err := vec1DeclineDistanceRef(stmt.where); err != nil {
		return 0, err
	}
	if pre == nil {
		return 0, fmt.Errorf("%w: vec1: DELETE without a compiled row selection", errVDBEUnsupported)
	}
	if err := db.vec1StatementBegin(vm, stmt.where); err != nil {
		return 0, err
	}
	rowids := make([]int64, 0, len(pre))
	for rid := range pre {
		rowids = append(rowids, rid)
	}
	slices.Sort(rowids)
	for _, rid := range rowids {
		if err := db.vec1Delete(vm, st, rid); err != nil {
			return 0, fmt.Errorf("engine: DELETE from %s: %w", stmt.table, err)
		}
	}
	if err := db.vec1StatementEnd(vm, stmt.where); err != nil {
		return 0, err
	}
	return len(rowids), nil
}

// vec1DeclineDistanceRef declines a write whose WHERE or SET reads the
// distance column, which a write's row source does not compute.
func vec1DeclineDistanceRef(e Expr) error {
	found := false
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil || found {
			return
		}
		if ce, ok := e.(ColumnExpr); ok {
			if strings.EqualFold(ce.Name, "distance") {
				found = true
			}
			return
		}
		walkExprOperands(e, walk)
	}
	walk(e)
	if found {
		return fmt.Errorf("%w: vec1: a DELETE or UPDATE reading the distance column", errVDBEUnsupported)
	}
	return nil
}

// vec1StatementJournal reports whether C codes a statement journal for a
// DELETE or UPDATE of a virtual table inside a transaction: when it is not
// one-pass, i.e. unless its WHERE pins the rowid (vec1's xBestIndex marks
// that plan unique). Such a statement opens a savepoint and releases it, and
// vec1 writes its pending lists at each.
func vec1StatementJournal(where Expr) bool {
	for _, c := range splitTopLevelAnd(where) {
		if b, ok := c.(BinaryExpr); ok && (b.Op == "=" || b.Op == "==") {
			for _, side := range []Expr{b.L, b.R} {
				if ce, ok := side.(ColumnExpr); ok && isRowidAliasName(ce.Name) {
					return false
				}
			}
		}
	}
	return true
}

// vec1StatementBegin and vec1StatementEnd are a write statement's savepoint
// inside a transaction, and its own transaction outside one.
func (db *DB) vec1StatementBegin(vm *vtabMeta, where Expr) error {
	if db.txActive && vec1StatementJournal(where) {
		return db.vec1Sync()
	}
	return nil
}

func (db *DB) vec1StatementEnd(vm *vtabMeta, where Expr) error {
	if db.txActive && vec1StatementJournal(where) {
		return db.vec1Sync()
	}
	return db.vec1AfterStatement(vm)
}
