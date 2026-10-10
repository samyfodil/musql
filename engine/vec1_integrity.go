package engine

// vec1's xIntegrity (vec1IntegrityMethod): PRAGMA integrity_check over a vec1
// table with an index. Every %_idx entry is checked against %_base -- its row
// exists, its metadata matches, it is in the bucket its vector belongs in, and
// its code is the vector's code (or one as good, for a PQ code whose
// arithmetic may differ, vec1IntegrityCheckEncoded) -- and %_base must hold
// as many rows as the lists. The first problem found is the table's message.

import (
	"bytes"
	"fmt"
	"math"
)

// vec1Integrity is one table's check: "" when it is sound.
func (p *ReadOnlyPager) vec1Integrity(name string, st *vec1Store) (string, error) {
	c := p.vec1Conn()
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tab("main", name)
	sh := p.vec1Shadow(name)
	if err := t.loadConfig(sh); err != nil {
		return "", err
	}
	mod := t.mod
	if !mod.indexed() {
		return "", nil
	}
	flat := mod.isFlat()
	nCodebook := mod.nCodebook()
	nElem := t.cfg.nElem
	var lists []vec1IdxRow
	if err := sh.scan("_idx", func(id int64, row []Value) (bool, error) {
		lists = append(lists, vec1IdxRowOf(id, row))
		return true, nil
	}); err != nil {
		return "", err
	}
	var total int64
	tmp := make([]float32, max(t.tmpLen(), nElem))
	residual := make([]float32, max(t.tmpLen(), nElem))
	for _, r := range lists {
		blob := r.val
		if !vec1ListSizeOK(mod, nElem, blob) {
			return fmt.Sprintf("%s: %%_idx entry id=%d is corrupt", name, r.id), nil
		}
		flags := be.Uint32(blob)
		nEntry := int(be.Uint32(blob[4:]))
		nTomb := int(be.Uint32(blob[8:]))
		sz := 4
		if flags&vec1List64 != 0 {
			sz = 8
		}
		metas := make([][]byte, st.nMeta)
		offs := make([]int, st.nMeta)
		for m := range st.nMeta {
			row, ok, err := sh.get("_meta", r.id<<8+int64(m))
			if err != nil {
				return "", err
			}
			v := vec1Col(row, 1)
			if !ok || len(v.S) == 0 {
				return fmt.Sprintf("%s: error reading meta-list id=%d,meta=%d", name, r.id, m), nil
			}
			metas[m], offs[m] = v.S, vec1MetaHdr
		}
		live := 0
		for e := range nEntry {
			var rowid int64
			tomb := false
			if sz == 4 {
				rowid = int64(be.Uint32(blob[vec1ListHdr+4*e:]))
				tomb = rowid == vec1Tomb32
			} else {
				rowid = int64(be.Uint64(blob[vec1ListHdr+8*e:]))
				tomb = rowid == vec1Tomb64
			}
			if tomb {
				for m := range st.nMeta {
					offs[m] = vec1IntegrityMeta(metas[m], offs[m], nil)
				}
				continue
			}
			live++
			base, exists, err := sh.get("_base", rowid)
			if err != nil {
				return "", err
			}
			if !exists {
				return fmt.Sprintf("%s: %%_idx row %d missing from %%_base", name, rowid), nil
			}
			for m := range st.nMeta {
				v := vec1Col(base, 2+m)
				next := vec1IntegrityMeta(metas[m], offs[m], &v)
				if next < 0 {
					return fmt.Sprintf("%s: meta-value value mismatch for row %d", name, rowid), nil
				}
				offs[m] = next
			}
			if flat {
				if int(int32(valueToInt64Trunc(vec1Col(base, 1)))) != int(int32(r.bucket)) {
					return fmt.Sprintf("%s: bucket value in %%_base row %d is incorrect", name, rowid), nil
				}
				continue
			}
			raw := vec1BlobOf(vec1Col(base, 1))
			if len(raw) != 4*nElem {
				return fmt.Sprintf("%s: vector in %%_base row %d is wrong size", name, rowid), nil
			}
			clear(tmp)
			a := mod.transform(tmp, vec1Floats(raw))
			calc := 0
			if mod.nBucket() > 0 {
				calc = vec1BestMatch(mod.cent, mod.nBucket(), a[:nElem], nil)
			}
			if calc != int(int32(r.bucket)) {
				return fmt.Sprintf("%s: vector from row %d is in wrong bucket", name, rowid), nil
			}
			enc := a
			if mod.residual() {
				clear(residual)
				vec1Sub(residual[:nElem], a[:nElem], mod.centroid(calc))
				enc = residual
			}
			if !vec1EncodedMatches(mod, enc, e, blob, nEntry, sz, nCodebook) {
				kind := "PQ"
				if mod.bitQuant() {
					kind = "BQ"
				}
				return fmt.Sprintf("%s: %%_idx %s does not match calculated %s for row %d", name, kind, kind, rowid), nil
			}
		}
		if live+nTomb != nEntry {
			return fmt.Sprintf("%s: bad tombstone count", name), nil
		}
		total += int64(live)
	}
	var got int64
	if err := sh.scan("_base", func(int64, []Value) (bool, error) {
		got++
		return true, nil
	}); err != nil {
		return "", err
	}
	if got != total {
		return fmt.Sprintf("%s: wrong number of entries in %%_base - have %d, expect %d", name, got, total), nil
	}
	return "", nil
}

// vec1IntegrityMeta is vec1IntegrityMetaValue: the value at off of a meta
// list compared with want (nil to skip it), and the next offset, -1 when it
// does not match.
func vec1IntegrityMeta(buf []byte, off int, want *Value) int {
	if len(buf) < vec1MetaHdr {
		return -1
	}
	f := be.Uint32(buf)
	var m vec1MetaValue
	switch {
	case f&vec1Meta1Byte != 0:
		if off >= len(buf) {
			return -1
		}
		m.i = int64(buf[off])
		off++
		m.typ = Int
		if m.i == vec1Meta1Null {
			m.typ = Null
		}
	case f&vec1Meta4Byte != 0:
		if off+4 > len(buf) {
			return -1
		}
		u := be.Uint32(buf[off:])
		m.i = int64(int32(u))
		off += 4
		m.typ = Int
		if u == vec1Meta4Null {
			m.typ = Null
		}
	case f&vec1MetaReal != 0:
		if off+8 > len(buf) {
			return -1
		}
		u := be.Uint64(buf[off:])
		off += 8
		m.typ, m.f = Float, math.Float64frombits(u)
		if u == vec1MetaRealNull {
			m.typ = Null
		}
	default:
		v, next, ok := vec1ReadMetaValue(buf, off)
		if !ok {
			return -1
		}
		m, off = v, next
	}
	if want == nil {
		return off
	}
	match := false
	switch want.Typ {
	case Null:
		match = m.typ == Null
	case Int:
		match = (m.typ == Int && m.i == want.I) || (m.typ == Float && m.f == float64(want.I))
	case Float:
		match = m.typ == Float && m.f == want.F
	case Text:
		match = m.typ == Text && bytes.Equal(m.s, want.S)
	default:
		match = m.typ == Blob && bytes.Equal(m.s, want.S)
	}
	if !match {
		return -1
	}
	return off
}

// vec1EncodedMatches is vec1IntegrityCheckEncoded: entry e's stored code
// against a's. A binary code must match bit for bit, its norm to a relative
// 1e-6; a PQ code that differs must reconstruct the vector as well as a's.
func vec1EncodedMatches(mod *vec1Model, a []float32, e int, blob []byte, nEntry, sz, nCodebook int) bool {
	nEnc := mod.encodedSize()
	enc := make([]byte, nEnc)
	mod.encode(a, enc)
	idx := make([]byte, nEnc)
	off := vec1ListHdr + nEntry*sz
	out := 0
	if mod.bitQuant() {
		copy(idx, blob[off+vec1NormOffset(e, nCodebook):][:4])
		off += vec1CodeOffset(e, nCodebook)
		out = 4
	} else {
		off += vec1BlockedOffset(e, nCodebook)
	}
	for range nCodebook {
		idx[out] = blob[off]
		out++
		off += vec1PQBlock
	}
	if bytes.Equal(enc, idx) {
		return true
	}
	const tol = float32(0.000001)
	if mod.bitQuant() {
		n1 := math.Float32frombits(vec1Native.Uint32(enc))
		n2 := math.Float32frombits(vec1Native.Uint32(idx))
		if !vec1WithinTolerance(n1, n2, tol) {
			return false
		}
		return bytes.Equal(enc[4:], idx[4:])
	}
	n := mod.nElem()
	v := make([]float32, max(n, mod.nCodeElem*nCodebook))
	mod.decode(enc, v)
	e1 := float32(vec1L2(v[:n], a[:n]))
	mod.decode(idx, v)
	e2 := float32(vec1L2(v[:n], a[:n]))
	return vec1WithinTolerance(e1, e2, tol)
}

// vec1WithinTolerance is vec1FloatWithinTolerance.
func vec1WithinTolerance(f1, f2, tol float32) bool {
	if f1 == 0 || f2 == 0 {
		return false
	}
	if f2 > f1 {
		return (f2-f1)/f2 <= tol
	}
	return (f1-f2)/f1 <= tol
}
