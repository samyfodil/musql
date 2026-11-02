package engine

// This file reads a virtual table through its live cursor, as C's OP_VFilter
// and OP_VNext do, instead of materializing its rows first (runVtabOnce). Only
// a module that is cheap to filter again opts in (vtabStreamer), and only a
// loop the compiler knows touches the cursor with nothing but rewind, next and
// column reads streams (derivedSource.vtabStream).

// vtabStreamer marks a module whose cursor a scan may read in place,
// filtering again at every rewind.
type vtabStreamer interface {
	streams()
}

// vtabStream is a cursor's live virtual-table scan. start resolves the module
// and filters a cursor over it, or returns nil when the module does not
// stream; fallback is then the materialization the cursor uses instead.
type vtabStream struct {
	start    func() (*vtabScan, error)
	fallback func() ([][]Value, []int64, error)
	scan     *vtabScan
	row      []Value
	fresh    bool // just filtered: the cursor is on its first row
}

// vtabScan is an open, filtered cursor and what filtering it again takes.
type vtabScan struct {
	cur    VtabCursor
	idxNum int
	idxStr string
	argv   []Value
}

// openVtabStreamCursor opens a cursor that streams s, or materializes through
// s.fallback when its module turns out not to stream.
func openVtabStreamCursor(tbl *resolvedTable, s *vtabStream) *vdbeCursor {
	cur := openFillCursor(tbl, s.fallback, false, nil)
	cur.vt = s
	return cur
}

// startVtabStream is the start of a vtabStream over it: the module resolved
// and its cursor opened and filtered as materializeVtab would, or nil for a
// module that is not a vtabStreamer.
func (p *ReadOnlyPager) startVtabStream(it FromItem, params []Value, outer *evalCtx, trig *trigCompileCtx, from *vdbe, corr []vtabCorrValue) (*vtabScan, error) {
	mod, modArgs, err := p.resolveVtabModuleForItem(it)
	if err != nil {
		return nil, err
	}
	vcols, tab, err := vtabConnect(p.fts3Catalog(), mod, modArgs)
	if err != nil {
		return nil, err
	}
	if _, ok := tab.(vtabStreamer); !ok {
		return nil, nil
	}
	cols := make([]columnInfo, len(vcols))
	for i, c := range vcols {
		cols[i] = columnInfo{Name: c.Name, DeclType: c.Type, Aff: vtabColumnAffinity(c), Hidden: c.Hidden, Unindexed: c.Unindexed}
	}
	info, argv, err := p.vtabFilterArgs(it, cols, tab, params, outer, trig, from, corr)
	if err != nil {
		return nil, err
	}
	cur, err := tab.Open()
	if err != nil {
		return nil, err
	}
	if ea, ok := cur.(vtabEncodingAware); ok {
		ea.setTextEncoding(p.encoding())
	}
	sc := &vtabScan{cur: cur, idxNum: info.IdxNum, idxStr: info.IdxStr, argv: argv}
	if err := cur.Filter(sc.idxNum, sc.idxStr, sc.argv); err != nil {
		return nil, err
	}
	return sc, nil
}

// rewindVtab is rewind() for a streaming virtual-table cursor. It filters the
// cursor, again on every rewind after the first, and reports false when the
// module does not stream, leaving the cursor to materialize through its fill.
func (cur *vdbeCursor) rewindVtab() (bool, error) {
	s := cur.vt
	if s.scan == nil {
		sc, err := s.start()
		if err != nil {
			return true, err
		}
		if sc == nil {
			cur.vt = nil
			return false, nil
		}
		s.scan = sc
	} else if err := s.scan.cur.Filter(s.scan.idxNum, s.scan.idxStr, s.scan.argv); err != nil {
		return true, err
	}
	s.fresh = true
	cur.pos = -1
	cur.rowidNull = false
	cur.streamErrPending = nil
	return true, nil
}

// advanceVtab is advance() for a streaming virtual-table cursor: the module's
// next row, read into one reused row as a co-routine's is (the reader copies
// out before it advances), with an error parked in streamErrPending.
func (cur *vdbeCursor) advanceVtab() bool {
	s := cur.vt
	vc := s.scan.cur
	if !s.fresh {
		if err := vc.Next(); err != nil {
			cur.streamErrPending = err
			return false
		}
	}
	s.fresh = false
	if vc.Eof() {
		return false
	}
	if len(s.row) != len(cur.tbl.cols) {
		s.row = make([]Value, len(cur.tbl.cols))
	}
	for i := range s.row {
		v, err := vc.Column(i)
		if err != nil {
			cur.streamErrPending = err
			return false
		}
		s.row[i] = v
	}
	rid, err := vc.Rowid()
	if err != nil {
		cur.streamErrPending = err
		return false
	}
	cur.pos++
	cur.rowid = uint64(rid)
	cur.rowVals = s.row
	cur.rowRaw = nil
	return true
}
