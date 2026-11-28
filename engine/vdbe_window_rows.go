package engine

import (
	"fmt"
	"math"
)

// windowRows is a window query's batch (OpWindowAppend): every scanned row,
// held without Go pointers. A batch is the whole result before any window is
// computed, millions of rows for a big scan, and as [][]Value each of its
// cells carried a slice header the collector traced on every cycle. Here a
// cell is three words of plain data, and TEXT and BLOB bytes live in chunks
// of a byte arena the cells index into, so the collector traces a handful of
// chunk pointers however many rows there are.
//
// A cell keeps a Value exactly: its type, subtype, intReal flag, I, F, and
// whether S is nil. A value read back takes its bytes from the arena, capped
// at their length so an append cannot spill into the next value's. The arena
// is append-only and never reused, so those bytes stay valid for as long as
// anything holds them, as the per-row copies they replace did.
//
// Cells, rows and bytes all grow by whole chunks: a growing slice copies
// everything it holds each time it grows, which for a batch this size cost
// several times the batch in copies alone.
type windowRows struct {
	cells  [][]winCell // a row's cells never straddle two chunks
	rows   [][]winRow
	n      int
	chunks [][]byte
	// esc holds the rare value whose I and F are both nonzero beside its
	// bytes, which a cell's two words cannot hold all three of.
	esc  []Value
	head []Value // headRow's
}

type winCell struct {
	typ   ValueType
	sub   uint8
	flags uint8
	n     uint32
	a, b  uint64
}

const (
	winIntReal  = 1 << iota
	winBytes    // S is non-nil; its arena position is in b
	winBytesInA // ... in a instead, when F rides in b
	winEscaped  // the value is esc[a]
)

// winRow is where a row's cells are: n of them from cells[chunk][off].
type winRow struct {
	chunk, off, n uint32
}

const (
	winChunkMin   = 1 << 10
	winChunkMax   = 1 << 18
	winCellChunk  = 1 << 12
	winRowChunkLg = 13
)

func (r *windowRows) len() int { return r.n }

// appendRow buffers one row: its first len(realCols) values are the scanned
// columns and rowids, the rest its operands.
//
// A MEM_IntReal (which a VIRTUAL generated column of REAL affinity makes, and
// a copy of one carries) is the one value whose reading back C decides by
// where it is read. The outer select reads a column through the window
// sub-select's result column, which has that column's affinity
// (sqlite3WindowRewrite, selectWindowRewriteExprCb), and reading a REAL one
// applies OP_RealAffinity (sqlite3ColumnDefault): such a payload value is
// stored as the REAL it reads as (realCols), any other as the integer a
// record holds (recordValue). What a window function's operand reads depends
// on which nested sub-select its window is coded in and on the operands
// beside it, which this batch does not reproduce, so an IntReal operand
// declines.
func (r *windowRows) appendRow(vals []Value, realCols []bool) error {
	payload := len(realCols)
	for i := payload; i < len(vals); i++ {
		if vals[i].intReal {
			return fmt.Errorf("%w: a window operand holding a REAL generated column's integer value", errVDBEUnsupported)
		}
	}
	k := len(r.cells) - 1
	if k < 0 || cap(r.cells[k])-len(r.cells[k]) < len(vals) {
		r.cells = append(r.cells, make([]winCell, 0, max(winCellChunk, len(vals))))
		k++
	}
	off := len(r.cells[k])
	r.cells[k] = r.cells[k][:off+len(vals)]
	cs := r.cells[k][off:]
	for i := range vals {
		if v := &vals[i]; v.S == nil && !v.intReal {
			cs[i] = winCell{typ: v.Typ, sub: v.Subtype, a: uint64(v.I), b: math.Float64bits(v.F)}
			continue
		}
		v := vals[i]
		if v.intReal && i < payload && realCols[i] {
			v.intReal = false
		}
		cs[i] = r.cell(recordValue(v))
	}
	if r.n&(1<<winRowChunkLg-1) == 0 {
		r.rows = append(r.rows, make([]winRow, 0, 1<<winRowChunkLg))
	}
	last := &r.rows[len(r.rows)-1]
	*last = append(*last, winRow{chunk: uint32(k), off: uint32(off), n: uint32(len(vals))})
	r.n++
	return nil
}

// cellsOf is row ri's cells.
func (r *windowRows) cellsOf(ri int) []winCell {
	w := r.rows[ri>>winRowChunkLg][ri&(1<<winRowChunkLg-1)]
	return r.cells[w.chunk][w.off : w.off+w.n]
}

func (r *windowRows) cell(v Value) winCell {
	c := winCell{typ: v.Typ, sub: v.Subtype}
	if v.intReal {
		c.flags = winIntReal
	}
	fb := math.Float64bits(v.F)
	switch {
	case v.S == nil:
		c.a, c.b = uint64(v.I), fb
	case len(v.S) > math.MaxUint32 || (v.I != 0 && fb != 0):
		c.flags = winEscaped
		c.a = uint64(len(r.esc))
		r.esc = append(r.esc, copyValue(v))
	case fb == 0:
		c.flags |= winBytes
		c.n = uint32(len(v.S))
		c.a, c.b = uint64(v.I), r.store(v.S)
	default:
		c.flags |= winBytes | winBytesInA
		c.n = uint32(len(v.S))
		c.a, c.b = r.store(v.S), fb
	}
	return c
}

// store copies s into the arena and returns its position: the chunk's index
// in the high 32 bits, the offset within it in the low.
func (r *windowRows) store(s []byte) uint64 {
	k := len(r.chunks) - 1
	if k < 0 || cap(r.chunks[k])-len(r.chunks[k]) < len(s) {
		size := winChunkMin
		if k >= 0 {
			size = min(2*cap(r.chunks[k]), winChunkMax)
		}
		size = max(size, len(s))
		r.chunks = append(r.chunks, make([]byte, 0, size))
		k++
	}
	off := len(r.chunks[k])
	r.chunks[k] = append(r.chunks[k], s...)
	return uint64(k)<<32 | uint64(off)
}

func (r *windowRows) bytes(pos uint64, n uint32) []byte {
	ch := r.chunks[pos>>32]
	off := int(uint32(pos))
	return ch[off : off+int(n) : off+int(n)]
}

func (r *windowRows) value(c *winCell) Value {
	if c.flags&winEscaped != 0 {
		return r.esc[c.a]
	}
	v := Value{Typ: c.typ, Subtype: c.sub, intReal: c.flags&winIntReal != 0}
	switch {
	case c.flags&winBytesInA != 0:
		v.S = r.bytes(c.a, c.n)
		v.F = math.Float64frombits(c.b)
	case c.flags&winBytes != 0:
		v.I = int64(c.a)
		v.S = r.bytes(c.b, c.n)
	default:
		v.I = int64(c.a)
		v.F = math.Float64frombits(c.b)
	}
	return v
}

// width is how many values row ri holds.
func (r *windowRows) width(ri int) int {
	return int(r.rows[ri>>winRowChunkLg][ri&(1<<winRowChunkLg-1)].n)
}

// at is row ri's i-th value; i must be below width(ri).
func (r *windowRows) at(ri, i int) Value {
	return r.value(&r.cellsOf(ri)[i])
}

// rangeInto decodes values [from, to) of row ri into buf, reusing its
// storage, and returns them: to-from values, NULL past the row's width.
func (r *windowRows) rangeInto(ri, from, to int, buf *[]Value) []Value {
	out := growValues(buf, max(to-from, 0))
	cs := r.cellsOf(ri)
	for i := range out {
		if j := from + i; j < len(cs) {
			out[i] = r.value(&cs[j])
		}
	}
	return out
}

// pickInto is row ri's n values from base on, of which only those at the
// offsets in idx are decoded, into buf: the cells it never names stay NULL,
// since buf is only ever filled at the same offsets.
func (r *windowRows) pickInto(ri, base, n int, idx []int, buf *[]Value) []Value {
	n = max(n, 0)
	if cap(*buf) < n {
		*buf = make([]Value, n)
	}
	out := (*buf)[:n]
	cs := r.cellsOf(ri)
	for _, i := range idx {
		if i >= n {
			continue
		}
		if j := base + i; j < len(cs) {
			out[i] = r.value(&cs[j])
		} else {
			out[i] = Value{}
		}
	}
	return out
}

// headRow is row 0's first n values, decoded once: rows are append-only, so
// row 0 never changes once it is there.
func (r *windowRows) headRow(n int) []Value {
	if r.head == nil || len(r.head) != n {
		var buf []Value
		r.head = r.rangeInto(0, 0, n, &buf)
	}
	return r.head
}

// identical is windowEntriesIdentical over rows ri and rj.
func (r *windowRows) identical(ri, rj int) bool {
	a, b := r.cellsOf(ri), r.cellsOf(rj)
	if len(a) != len(b) {
		return false
	}
	return r.cellsIdentical(a, b)
}

// identicalRange is windowEntriesIdentical over values [lo, hi) of rows ri
// and rj.
func (r *windowRows) identicalRange(ri, rj, lo, hi int) bool {
	a, b := r.cellsOf(ri), r.cellsOf(rj)
	a, b = a[min(lo, len(a)):min(hi, len(a))], b[min(lo, len(b)):min(hi, len(b))]
	if len(a) != len(b) {
		return false
	}
	return r.cellsIdentical(a, b)
}

func (r *windowRows) cellsIdentical(a, b []winCell) bool {
	for i := range a {
		if !valuesIdentical(r.value(&a[i]), r.value(&b[i])) {
			return false
		}
	}
	return true
}
