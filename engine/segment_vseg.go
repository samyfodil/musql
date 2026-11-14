package engine

import (
	"encoding/binary"
	"math/bits"
	"slices"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// vmSeg is a segment as native code reads it in place (jit.VSeg): per column,
// its cells and how they read, and mask the columns it reads at all. A TEXT
// column's bounds are checked only once a caller needs it (unchecked), since
// the check reads every row's bound.
type vmSeg struct {
	seg       jit.VSeg
	cols      []jit.VSegCol
	mask      uint64
	unchecked uint64
}

// vmSegFor returns s as native code reads it for a table whose columns read by
// plans, with the columns in need checked, built once and kept with s. A
// column reads natively only where its cells read as segColumnValue reads them
// with no fix-up native code lacks: an int64, double or numeric block, an
// INTEGER PRIMARY KEY stored NULL (the rowid), TEXT between heap bounds. Rows
// narrower than the table, defaults and blobs are the VDBE's.
//
// A view another caller holds is never changed: checking more columns makes a
// new one.
func (s *segment) vmSegFor(plans []segColPlan, need uint64) *vmSeg {
	if v := s.vseg.Load(); v != nil && v.unchecked&need == 0 {
		return v
	}
	s.vsegMu.Lock()
	defer s.vsegMu.Unlock()
	v := s.vseg.Load()
	if v != nil && v.unchecked&need == 0 {
		return v
	}
	if v == nil {
		v = s.buildVMSeg(plans)
	} else {
		w := *v
		w.cols = slices.Clone(v.cols)
		w.seg.Cols = unsafe.Pointer(&w.cols[0])
		v = &w
	}
	for todo := v.unchecked & need; todo != 0; todo &= todo - 1 {
		c := bits.TrailingZeros64(todo)
		v.unchecked &^= 1 << uint(c)
		if !s.textBoundsValid(c) {
			v.cols[c] = jit.VSegCol{}
		} else {
			v.cols[c].Kind = jit.VSegText
			if v.cols[c].Nulls == nil && v.cols[c].Exc == nil {
				v.cols[c].Plain = jit.VSegText
			}
			v.mask |= 1 << uint(c)
		}
	}
	s.vseg.Store(v)
	return v
}

// segAllNull is the NULL bitmap of a column NULL in every row of a segment,
// as long as the longest segment (segmentRows).
var segAllNull = func() *[segmentRows / 8]byte {
	var b [segmentRows / 8]byte
	for i := range b {
		b[i] = 0xff
	}
	return &b
}()

// buildVMSeg is vmSegFor's view with no TEXT column checked.
func (s *segment) buildVMSeg(plans []segColPlan) *vmSeg {
	v := &vmSeg{cols: make([]jit.VSegCol, min(len(s.cols), len(plans), 64))}
	if s.dense {
		v.seg.First, v.seg.Dense = int64(s.firstRowid), 1
	} else {
		v.seg.Rowids = unsafe.Pointer(&s.buf[s.rowidOff])
	}
	for c := range v.cols {
		if !s.fullWidth || s.nRows == 0 {
			break
		}
		col, plan := s.cols[c], &plans[c]
		vc := &v.cols[c]
		hasExc := s.excRows != nil && s.excRows[c] != nil
		text := false
		switch {
		case plan.isIPK:
			if col.phys == PhysNull && !hasExc {
				vc.Kind = jit.VSegRowid
			}
		case col.phys == PhysNull:
			// A column NULL in every row: every bit of its bitmap set, which
			// native code reads as NULL.
			if !hasExc && s.nRows <= 8*len(segAllNull) {
				vc.Kind, vc.Block, vc.Nulls = jit.VSegInt, unsafe.Pointer(&segAllNull[0]), unsafe.Pointer(&segAllNull[0])
				v.mask |= 1 << uint(c)
			}
			continue
		case col.bounds:
			if col.phys == PhysText && len(s.heap) > 0 {
				text = true
				vc.Heap = unsafe.Pointer(unsafe.SliceData(s.heap))
			}
		case col.phys == PhysInt64 && plan.isReal:
			vc.Kind = jit.VSegIntReal
		case col.phys == PhysInt64:
			vc.Kind = jit.VSegInt
		case col.phys == PhysFloat64:
			vc.Kind = jit.VSegFloat
		case col.phys == PhysNumeric && !plan.isReal:
			vc.Kind = jit.VSegNumeric
		}
		if vc.Kind == jit.VSegNone && !text {
			continue
		}
		if vc.Kind != jit.VSegRowid {
			vc.Block = unsafe.Pointer(&s.buf[col.blockOff])
			if col.nullOff != 0 {
				vc.Nulls = unsafe.Pointer(&s.buf[col.nullOff])
			}
			if hasExc {
				vc.Exc = unsafe.Pointer(&s.excRows[c][0])
			}
		}
		if text {
			v.unchecked |= 1 << uint(c)
			continue
		}
		if vc.Nulls == nil && vc.Exc == nil {
			vc.Plain = vc.Kind
		}
		v.mask |= 1 << uint(c)
	}
	if len(v.cols) > 0 {
		v.seg.Cols = unsafe.Pointer(&v.cols[0])
	}
	return v
}

// segColKind is the kind every segment of tbl reads column col as
// (jit.VSegInt and the rest), which native code predicts for that column's
// reads; 0 where they disagree, or one reads it no way native code does.
func (p *ReadOnlyPager) segColKind(tbl *resolvedTable, col int) int {
	if p == nil || p.segs == nil || tbl == nil {
		return 0
	}
	segs := p.segs.byRoot[tbl.root]
	if p.segs.isLive(tbl.root) {
		var ok bool
		if segs, ok = p.segs.liveBaseSegments(tbl.root); !ok {
			return 0
		}
	}
	plans := segColPlansFor(tbl)
	need := uint64(0)
	if col >= 0 && col < 64 {
		need = 1 << uint(col)
	}
	kind := int64(0)
	for _, s := range segs {
		if s == nil || s.nRows == 0 {
			continue
		}
		v := s.vmSegFor(plans, need)
		if col < 0 || col >= len(v.cols) {
			return 0
		}
		k := v.cols[col].Kind
		if k == jit.VSegNone || kind != 0 && k != kind {
			return 0
		}
		kind = k
	}
	return int(kind)
}

// textBoundsValid reports whether TEXT column c's bounds never step back and
// stay within the heap: what boundCell checks per read, once for all rows.
func (s *segment) textBoundsValid(c int) bool {
	b := s.buf[int(s.cols[c].blockOff):]
	prev := uint32(0)
	for i := 0; i <= s.nRows; i++ {
		x := binary.LittleEndian.Uint32(b[4*i:])
		if x < prev || int(x) > len(s.heap) {
			return false
		}
		prev = x
	}
	return true
}
