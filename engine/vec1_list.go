package engine

// The bytes vec1 keeps in its shadow tables: an %_idx row's list of vectors
// or codes, and the %_meta row holding one metadata column for the same
// entries. Both are written here byte for byte as vec1.c writes them, so a
// table this engine builds reads the same in C SQLite and back.
//
// An %_idx list (vec1.c's "%_IDX TABLE LIST FORMAT"): three big-endian u32s
// (flags, entries, tombstones), the rowids (u32, or i64 with VEC1_LIST_64BIT;
// a deleted entry's rowid becomes the tombstone, 0xFFFFFFFF or 0), then the
// data. A full-vector list stores each vector whole; a coded list stores codes
// in blocks of 16 entries, each block holding the first code byte of all 16,
// then the second, and so on -- a binary code's float32 norms first.
//
// A %_meta list (vec1.c's "%_META TABLE LIST FORMAT"): flags and entries as
// u32s, then one value per entry in the narrowest of four layouts.

import (
	"bytes"
	"encoding/binary"
	"math"
)

const (
	vec1ListSorted = 0x02
	vec1List64     = 0x01
	vec1ListHdr    = 12
	vec1Tomb32     = 0xFFFFFFFF
	vec1Tomb64     = 0

	vec1MetaGeneric  = 0x01
	vec1Meta1Byte    = 0x02
	vec1Meta4Byte    = 0x04
	vec1MetaReal     = 0x08
	vec1MetaHasNull  = 0x10
	vec1MetaTypeMask = vec1MetaGeneric | vec1Meta1Byte | vec1Meta4Byte | vec1MetaReal
	vec1MetaHdr      = 8
	vec1Meta1Null    = 255
	vec1Meta4Min     = -2147483648
	vec1Meta4Max     = 2147483646
	vec1Meta4Null    = 2147483647
	vec1MetaRealNull = uint64(0x7FF8000000000001)

	vec1BlockedNone = 0
	vec1BlockedPQ   = 1
	vec1BlockedBin  = 2

	vec1LargestInt64  = math.MaxInt64
	vec1SmallestInt64 = math.MinInt64
)

var be = binary.BigEndian

// vec1PutVarint is vec1PutVarint: a big-endian base-128 varint, unlike
// SQLite's own, a byte's high bit marking that more follow.
func vec1PutVarint(b []byte, v uint64) []byte {
	if v <= 0x7f {
		return append(b, byte(v))
	}
	if v <= 0x3fff {
		return append(b, byte(v>>7)|0x80, byte(v&0x7f))
	}
	var a [10]byte
	n := 0
	for ; v != 0; n++ {
		a[n] = byte(v & 0x7f)
		v >>= 7
	}
	for i := range n {
		c := a[n-1-i]
		if i != n-1 {
			c |= 0x80
		}
		b = append(b, c)
	}
	return b
}

// vec1GetVarint is vec1GetVarint. A varint running off the buffer is cut
// short instead of read past it (C would read on).
func vec1GetVarint(b []byte) (uint64, int) {
	var out uint64
	n := 0
	for n < len(b) {
		c := b[n]
		out = out<<7 + uint64(c&0x7f)
		n++
		if c&0x80 == 0 {
			break
		}
	}
	return out, n
}

// vec1AppendMeta is vec1AppendMetaValue: one value in the generic layout --
// a type byte (0 NULL, 1 a u8, 2 an i32, 3 an i64, 4 a REAL) and its bytes,
// or a varint 2n+5 (TEXT) or 2n+6 (BLOB) and n bytes.
func vec1AppendMeta(b []byte, v Value) []byte {
	switch v.Typ {
	case Null:
		return append(b, 0)
	case Int:
		switch {
		case v.I >= 0 && v.I <= 254:
			return append(b, 1, byte(v.I))
		case v.I >= vec1Meta4Min && v.I <= vec1Meta4Max:
			return be.AppendUint32(append(b, 2), uint32(int32(v.I)))
		}
		return be.AppendUint64(append(b, 3), uint64(v.I))
	case Float:
		return be.AppendUint64(append(b, 4), math.Float64bits(v.F))
	case Text:
		b = vec1PutVarint(b, uint64(len(v.S))*2+5)
		return append(b, v.S...)
	}
	b = vec1PutVarint(b, uint64(len(v.S))*2+6)
	return append(b, v.S...)
}

// vec1MetaValue is Vec1MetaValue: a decoded metadata value. A TEXT or BLOB
// value's bytes alias the list it was read from.
type vec1MetaValue struct {
	typ ValueType
	i   int64
	f   float64
	s   []byte
}

// vec1ReadMetaValue is vec1MetaValueRead: the generic value at b[off:], and
// the offset past it. ok is false for a value running off the buffer.
func vec1ReadMetaValue(b []byte, off int) (vec1MetaValue, int, bool) {
	if off >= len(b) {
		return vec1MetaValue{}, off, false
	}
	var v vec1MetaValue
	switch b[off] {
	case 0:
		v.typ = Null
		return v, off + 1, true
	case 1:
		if off+2 > len(b) {
			return v, off, false
		}
		v.typ, v.i = Int, int64(b[off+1])
		return v, off + 2, true
	case 2:
		if off+5 > len(b) {
			return v, off, false
		}
		v.typ, v.i = Int, int64(int32(be.Uint32(b[off+1:])))
		return v, off + 5, true
	case 3:
		if off+9 > len(b) {
			return v, off, false
		}
		v.typ, v.i = Int, int64(be.Uint64(b[off+1:]))
		return v, off + 9, true
	case 4:
		if off+9 > len(b) {
			return v, off, false
		}
		v.typ, v.f = Float, math.Float64frombits(be.Uint64(b[off+1:]))
		return v, off + 9, true
	}
	t, n := vec1GetVarint(b[off:])
	off += n
	sz := int((t - 5) / 2)
	if sz < 0 || off+sz > len(b) {
		return v, off, false
	}
	v.s = b[off : off+sz]
	v.i = int64(sz)
	if t&1 != 0 {
		v.typ = Text
	} else {
		v.typ = Blob
	}
	return v, off + sz, true
}

// vec1MetaValueSize is vec1MetaValueSize: the generic value's length.
func vec1MetaValueSize(b []byte) int {
	switch b[0] {
	case 0:
		return 1
	case 1:
		return 2
	case 2:
		return 5
	case 3, 4:
		return 9
	}
	v, n := vec1GetVarint(b)
	return int((int64(v)-5)/2) + n
}

// vec1MetaUpdateFlags is vec1MetaValueUpdateFlags: which narrow layouts a
// generic list can still take after this value.
func vec1MetaUpdateFlags(b []byte, flags *uint32) {
	switch b[0] {
	case 0:
		*flags |= vec1MetaHasNull
	case 1:
	case 2:
		*flags &^= vec1Meta1Byte
	case 4:
		*flags &^= vec1Meta1Byte | vec1Meta4Byte
	default:
		*flags &^= vec1Meta1Byte | vec1Meta4Byte | vec1MetaReal
	}
}

// vec1MetaBuilder is Vec1MetaBuilder: one metadata column's list being
// built. buf's first vec1MetaHdr bytes are the header, filled when written.
type vec1MetaBuilder struct {
	buf    []byte
	flags  uint32
	format uint32
}

func (m *vec1MetaBuilder) clone() vec1MetaBuilder {
	return vec1MetaBuilder{buf: bytes.Clone(m.buf), flags: m.flags, format: m.format}
}

// expand is vec1ExpandMetaValue: a narrow list rewritten in the generic
// layout.
func (m *vec1MetaBuilder) expand() {
	orig := m.buf
	out := make([]byte, vec1MetaHdr, len(orig)*2)
	hasNull := false
	for in := vec1MetaHdr; in < len(orig); {
		switch m.format {
		case vec1Meta1Byte:
			v := orig[in]
			in++
			if v == vec1Meta1Null {
				out = append(out, 0)
				hasNull = true
			} else {
				out = append(out, 1, v)
			}
		case vec1Meta4Byte:
			v := be.Uint32(orig[in:])
			in += 4
			if v == vec1Meta4Null {
				out = append(out, 0)
				hasNull = true
			} else {
				out = be.AppendUint32(append(out, 2), v)
			}
		default:
			v := be.Uint64(orig[in:])
			in += 8
			if v == vec1MetaRealNull {
				out = append(out, 0)
				hasNull = true
			} else {
				out = be.AppendUint64(append(out, 4), v)
			}
		}
	}
	switch m.format {
	case vec1Meta1Byte:
		m.flags = vec1Meta4Byte | vec1Meta1Byte | vec1MetaReal
	case vec1Meta4Byte:
		m.flags = vec1Meta4Byte | vec1MetaReal
	default:
		m.flags = vec1MetaReal
	}
	m.format = vec1MetaGeneric
	if hasNull {
		m.flags |= vec1MetaHasNull
	}
	m.buf = out
}

// add is one column of vec1WriterMetaFromArray: v appended in the list's
// narrow layout when it fits, otherwise the list goes generic first.
func (m *vec1MetaBuilder) add(v Value) {
	if m.format == vec1Meta1Byte || m.format == vec1Meta4Byte || m.format == vec1MetaReal {
		done := false
		switch {
		case v.Typ == Int:
			if m.format == vec1Meta1Byte {
				if v.I >= 0 && v.I <= 254 {
					m.buf = append(m.buf, byte(v.I))
					done = true
				}
			} else if v.I >= vec1Meta4Min && v.I <= vec1Meta4Max {
				if m.format == vec1Meta4Byte {
					m.buf = be.AppendUint32(m.buf, uint32(int32(v.I)))
				} else {
					m.buf = be.AppendUint64(m.buf, math.Float64bits(float64(v.I)))
				}
				done = true
			}
		case v.Typ == Float && m.format == vec1MetaReal:
			m.buf = be.AppendUint64(m.buf, math.Float64bits(v.F))
			done = true
		case v.Typ == Null:
			switch m.format {
			case vec1Meta1Byte:
				m.buf = append(m.buf, vec1Meta1Null)
			case vec1Meta4Byte:
				m.buf = be.AppendUint32(m.buf, vec1Meta4Null)
			default:
				m.buf = be.AppendUint64(m.buf, vec1MetaRealNull)
			}
			done = true
		}
		if done {
			return
		}
		m.expand()
	}
	off := len(m.buf)
	m.buf = vec1AppendMeta(m.buf, v)
	vec1MetaUpdateFlags(m.buf[off:], &m.flags)
}

// addPacked appends one generic value already encoded (a rebuild's packed
// row, vec1WriterMetaFromPacked) and returns its length.
func (m *vec1MetaBuilder) addPacked(b []byte) int {
	n := vec1MetaValueSize(b)
	vec1MetaUpdateFlags(b, &m.flags)
	m.buf = append(m.buf, b[:n]...)
	return n
}

// finish is the encoding half of vec1WriteMeta: a generic list goes to the
// narrowest layout its values allow, and the header is filled in.
func (m *vec1MetaBuilder) finish(nEntry int) []byte {
	if m.format&vec1MetaGeneric != 0 && m.flags&(vec1Meta1Byte|vec1Meta4Byte|vec1MetaReal) != 0 {
		var sz int
		var null uint64
		switch {
		case m.flags&vec1Meta1Byte != 0:
			sz, null, m.format = 1, vec1Meta1Null, vec1Meta1Byte
		case m.flags&vec1Meta4Byte != 0:
			sz, null, m.format = 4, vec1Meta4Null, vec1Meta4Byte
		default:
			sz, null, m.format = 8, vec1MetaRealNull, vec1MetaReal
		}
		out := make([]byte, vec1MetaHdr, vec1MetaHdr+nEntry*sz)
		for in := vec1MetaHdr; in < len(m.buf); {
			v, next, ok := vec1ReadMetaValue(m.buf, in)
			if !ok {
				break
			}
			in = next
			bits := uint64(v.i)
			switch {
			case v.typ == Null:
				bits = null
			case m.format == vec1MetaReal:
				f := v.f
				if v.typ == Int {
					f = float64(v.i)
				}
				bits = math.Float64bits(f)
			}
			switch sz {
			case 1:
				out = append(out, byte(bits))
			case 4:
				out = be.AppendUint32(out, uint32(bits))
			default:
				out = be.AppendUint64(out, bits)
			}
		}
		m.buf = out
	}
	be.PutUint32(m.buf[0:], m.format|(m.flags&vec1MetaHasNull))
	be.PutUint32(m.buf[4:], uint32(nEntry))
	return m.buf
}

// vec1ListBuilder is Vec1ListBuilder: one bucket's list being written, kept
// in memory between statements as vec1's writer keeps it.
type vec1ListBuilder struct {
	init       bool // pTab!=0: the builder has been set up
	vecSize    int  // a stored vector's or code's bytes
	bucket     int
	blocked    int
	id         int64 // the %_idx row it replaces, 0 for a new one
	first      int64
	last       int64
	sorted     bool
	szRowid    int
	nTombstone int
	rowids     []byte
	data       []byte
	meta       []vec1MetaBuilder
}

func (b *vec1ListBuilder) clone() *vec1ListBuilder {
	cp := *b
	cp.rowids = bytes.Clone(b.rowids)
	cp.data = bytes.Clone(b.data)
	cp.meta = make([]vec1MetaBuilder, len(b.meta))
	for i := range b.meta {
		cp.meta[i] = b.meta[i].clone()
	}
	return &cp
}

// initMeta is vec1ListBuilderInitMetaArray.
func (b *vec1ListBuilder) initMeta() {
	for i := range b.meta {
		b.meta[i] = vec1MetaBuilder{buf: make([]byte, vec1MetaHdr), flags: vec1Meta1Byte | vec1Meta4Byte | vec1MetaReal, format: vec1MetaGeneric}
	}
}

// setup is vec1ListBuilderInit.
func (b *vec1ListBuilder) setup(mod *vec1Model, nElem, nMeta, bucket int) {
	*b = vec1ListBuilder{init: true, bucket: bucket, szRowid: 4, sorted: true}
	if mod.nCodebook() > 0 {
		b.blocked = vec1BlockedPQ
		if mod.bitQuant() {
			b.blocked = vec1BlockedBin
		}
		b.vecSize = mod.encodedSize()
	} else {
		b.vecSize = 4 * nElem
	}
	b.meta = make([]vec1MetaBuilder, nMeta)
	b.initMeta()
}

func (b *vec1ListBuilder) nEntry() int { return len(b.rowids) / b.szRowid }

// rowidAt is entry i's rowid, the tombstone included.
func (b *vec1ListBuilder) rowidAt(i int) int64 {
	if b.szRowid == 8 {
		return int64(be.Uint64(b.rowids[i*8:]))
	}
	return int64(be.Uint32(b.rowids[i*4:]))
}

// vec1ListSizeOK is the inverse of vec1CheckIdxSize: whether a list's length
// is what its header says, so it can be read without bounds checks.
func vec1ListSizeOK(mod *vec1Model, nElem int, blob []byte) bool {
	if len(blob) < vec1ListHdr {
		return false
	}
	flags := be.Uint32(blob)
	nEntry := int64(be.Uint32(blob[4:]))
	szRowid := int64(4)
	if flags&vec1List64 != 0 {
		szRowid = 8
	}
	want := int64(vec1ListHdr) + nEntry*szRowid
	if mod.nCodebook() > 0 {
		nBlk := (nEntry + vec1PQBlock - 1) / vec1PQBlock
		want += nBlk * int64(vec1PQBlock*mod.encodedSize())
	} else {
		want += nEntry * int64(4*nElem)
	}
	return int64(len(blob)) == want
}

// listBytes is the %_idx value vec1ListBuilderFlush writes.
func (b *vec1ListBuilder) listBytes() []byte {
	out := make([]byte, vec1ListHdr, vec1ListHdr+len(b.rowids)+len(b.data))
	flags := uint32(0)
	if b.sorted {
		flags |= vec1ListSorted
	}
	if b.szRowid == 8 {
		flags |= vec1List64
	}
	be.PutUint32(out, flags)
	be.PutUint32(out[4:], uint32(b.nEntry()))
	be.PutUint32(out[8:], uint32(b.nTombstone))
	out = append(out, b.rowids...)
	return append(out, b.data...)
}

// load is vec1ListBuilderLoad's list half: an %_idx row taken into the
// builder to be appended to or compacted. The caller has checked its size.
func (b *vec1ListBuilder) load(mod *vec1Model, id, first, last int64, val []byte) {
	flags := be.Uint32(val)
	nEntry := int(be.Uint32(val[4:]))
	b.nTombstone = int(be.Uint32(val[8:]))
	if flags&vec1List64 != 0 {
		b.szRowid = 8
	}
	switch {
	case mod.bitQuant():
		b.blocked = vec1BlockedBin
	case mod.nCodebook() > 0:
		b.blocked = vec1BlockedPQ
	default:
		b.blocked = vec1BlockedNone
	}
	b.sorted = flags&vec1ListSorted != 0
	b.first, b.last, b.id = first, last, id
	nRowid := nEntry * b.szRowid
	b.rowids = bytes.Clone(val[vec1ListHdr : vec1ListHdr+nRowid])
	b.data = bytes.Clone(val[vec1ListHdr+nRowid:])
}

// blockedOffset is VEC1_LIST_BLOCKED_OFFSET.
func vec1BlockedOffset(i, vecSize int) int {
	return i/vec1PQBlock*vec1PQBlock*vecSize + i%vec1PQBlock
}

// normOffset and codeOffset are VEC1_LIST_NORM_OFFSET and _CODE_OFFSET.
func vec1NormOffset(i, nCodebook int) int {
	return i/vec1PQBlock*vec1PQBlock*(nCodebook+4) + i%vec1PQBlock*4
}

func vec1CodeOffset(i, nCodebook int) int {
	return i/vec1PQBlock*vec1PQBlock*(nCodebook+4) + vec1PQBlock*4 + i%vec1PQBlock
}

// add is vec1ListBuilderAdd's append, after its flush test: rowid and its
// stored vector or code.
func (b *vec1ListBuilder) add(rowid int64, store []byte) {
	if len(b.rowids) == 0 {
		b.first, b.last = rowid, rowid
	} else if rowid < b.last {
		b.sorted = false
		b.first = min(b.first, rowid)
	} else {
		b.last = rowid
	}
	vs := b.vecSize
	if b.blocked == vec1BlockedNone {
		b.data = append(b.data, store[:vs]...)
	} else {
		entry := b.nEntry()
		if entry%vec1PQBlock == 0 {
			b.data = append(b.data, make([]byte, vec1PQBlock*vs)...)
		}
		blk := entry / vec1PQBlock * vs * vec1PQBlock
		slot := entry % vec1PQBlock
		off := blk + slot
		i := 0
		if b.blocked == vec1BlockedBin {
			copy(b.data[blk+4*slot:], store[:4])
			i = 4
			off += 4*vec1PQBlock - i*vec1PQBlock
		}
		for ; i < vs; i++ {
			b.data[off+i*vec1PQBlock] = store[i]
		}
	}
	if b.szRowid == 8 {
		b.rowids = be.AppendUint64(b.rowids, uint64(rowid))
	} else {
		b.rowids = be.AppendUint32(b.rowids, uint32(rowid))
	}
}

// needsFlush is vec1ListBuilderAdd's test: whether appending rowid would
// take the list past blocksize, or the rowid does not fit its rowid width.
func (b *vec1ListBuilder) needsFlush(rowid int64, blocksize int) bool {
	final := len(b.rowids) + len(b.data) + b.szRowid + b.vecSize
	return final > blocksize ||
		(b.szRowid == 4 && (rowid < 0 || rowid >= vec1Tomb32)) ||
		(b.szRowid == 8 && rowid == vec1Tomb64)
}

// compress is vec1ListBuilderCompress: every tombstone, and rowid del,
// removed in place -- moved-from bytes left behind in a final partial block,
// as vec1 leaves them.
func (b *vec1ListBuilder) compress(del int64) {
	nMeta := len(b.meta)
	metaIn := make([]int, nMeta)
	metaOut := make([]int, nMeta)
	for i := range nMeta {
		metaIn[i], metaOut[i] = vec1MetaHdr, vec1MetaHdr
	}
	tomb1 := int64(vec1Tomb32)
	if b.szRowid == 8 {
		tomb1 = vec1Tomb64
	}
	b.first, b.last = vec1LargestInt64, vec1SmallestInt64
	nEntry := b.nEntry()
	out := 0
	for i := range nEntry {
		rowid := b.rowidAt(i)
		tomb := rowid == tomb1 || rowid == del
		if !tomb {
			b.first = min(b.first, rowid)
			b.last = max(b.last, rowid)
			if i != out {
				copy(b.rowids[out*b.szRowid:], b.rowids[i*b.szRowid:(i+1)*b.szRowid])
				switch b.blocked {
				case vec1BlockedNone:
					vs := b.vecSize
					copy(b.data[vs*out:], b.data[vs*i:vs*i+vs])
				default:
					var nCodebook, from, to int
					if b.blocked == vec1BlockedBin {
						nCodebook = b.vecSize - 4
						copy(b.data[vec1NormOffset(out, nCodebook):], b.data[vec1NormOffset(i, nCodebook):vec1NormOffset(i, nCodebook)+4])
						from, to = vec1CodeOffset(i, nCodebook), vec1CodeOffset(out, nCodebook)
					} else {
						nCodebook = b.vecSize
						from, to = vec1BlockedOffset(i, nCodebook), vec1BlockedOffset(out, nCodebook)
					}
					for j := range nCodebook {
						b.data[to+j*vec1PQBlock] = b.data[from+j*vec1PQBlock]
					}
				}
			}
			out++
		}
		for m := range nMeta {
			mb := &b.meta[m]
			var n int
			switch mb.format {
			case vec1MetaGeneric:
				n = vec1MetaValueSize(mb.buf[metaIn[m]:])
			case vec1Meta1Byte:
				n = 1
			case vec1Meta4Byte:
				n = 4
			default:
				n = 8
			}
			if !tomb {
				copy(mb.buf[metaOut[m]:], mb.buf[metaIn[m]:metaIn[m]+n])
				metaOut[m] += n
			}
			metaIn[m] += n
		}
	}
	b.nTombstone = 0
	b.rowids = b.rowids[:out*b.szRowid]
	if b.blocked == vec1BlockedNone {
		b.data = b.data[:b.vecSize*out]
	} else {
		b.data = b.data[:b.vecSize*vec1PQBlock*((out+vec1PQBlock-1)/vec1PQBlock)]
	}
	for m := range nMeta {
		b.meta[m].buf = b.meta[m].buf[:metaOut[m]]
	}
}
