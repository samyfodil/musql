package engine

import (
	"encoding/binary"
	"math"
	"slices"
)

// Package engine implements a UNIQUE INDEX conflict probe for row store writes.
// A key maps to candidate rowids that may conflict; a comparator re-checks each.
// Supports BINARY, NOCASE, and RTRIM collations. Null keys are excluded.
// The index is maintained by put/drop/clear and rebuilt if schema changes.
type rsUniqIndex struct {
	cols  []int
	colls []uint8 // uqBinary, uqNocase or uqRtrim, per key column
	ipk   int
	keys  map[string][]uint64
	loose []uint64
	buf   []byte
	cand  []Value
}

const (
	uqBinary uint8 = iota
	uqNocase
	uqRtrim
)

// uniqConflictCandidates is the rowids that may hold cand's key in idx, or false
// when this store cannot answer and the caller must scan.
func (s *rowStore) uniqConflictCandidates(tbl *tableMeta, idx *indexMeta, cand []Value, enc TextEncoding) ([]uint64, bool) {
	if s == nil || isUTF16(enc) {
		return nil, false
	}
	colls, ok := uniqCollations(idx)
	if !ok {
		return nil, false
	}
	ix := s.uqIdx[idx]
	if ix == nil || ix.ipk != tbl.ipkIndex || !slices.Equal(ix.cols, idx.colIdx) || !slices.Equal(ix.colls, colls) {
		ix = s.buildUniqIndex(tbl, idx, colls)
	}
	key, keyed := ix.keyOf(cand)
	if !keyed {
		return nil, false
	}
	hits := ix.keys[string(key)]
	if len(ix.loose) == 0 {
		return hits, true
	}
	out := make([]uint64, 0, len(hits)+len(ix.loose))
	out = append(out, hits...)
	return append(out, ix.loose...), true
}

// uniqCollations maps idx's per-column collations onto the three this index can
// normalize for, or false.
func uniqCollations(idx *indexMeta) ([]uint8, bool) {
	colls := make([]uint8, len(idx.colIdx))
	for i := range idx.colIdx {
		switch c := effectiveCollation(atOrEmpty(idx.colCollation, i)); {
		case asciiEqualFold(c, "BINARY"):
			colls[i] = uqBinary
		case asciiEqualFold(c, "NOCASE"):
			colls[i] = uqNocase
		case asciiEqualFold(c, "RTRIM"):
			colls[i] = uqRtrim
		default:
			return nil, false
		}
	}
	return colls, true
}

func (s *rowStore) buildUniqIndex(tbl *tableMeta, idx *indexMeta, colls []uint8) *rsUniqIndex {
	ix := &rsUniqIndex{
		cols:  slices.Clone(idx.colIdx),
		colls: colls,
		ipk:   tbl.ipkIndex,
		keys:  make(map[string][]uint64, len(s.m)),
	}
	for rid, vals := range s.all() {
		ix.add(rid, vals)
	}
	if s.uqIdx == nil {
		s.uqIdx = map[*indexMeta]*rsUniqIndex{}
	}
	s.uqIdx[idx] = ix
	return ix
}

// rowKey is the key of stored row vals under rowid, false when it has a NULL
// component (it conflicts with nothing), and loose when it cannot be keyed.
func (ix *rsUniqIndex) rowKey(rowid uint64, vals []Value) (key []byte, ok, loose bool) {
	cand := ix.cand[:0]
	for _, c := range ix.cols {
		if c < 0 || c >= len(vals) {
			return nil, false, true
		}
		v := vals[c]
		if c == ix.ipk && v.Typ == Null {
			v = Value{Typ: Int, I: int64(rowid)} // indexColumnValue's rule
		}
		if v.Typ == Null {
			return nil, false, false
		}
		cand = append(cand, v)
	}
	ix.cand = cand
	key, keyed := ix.keyOf(cand)
	return key, keyed, !keyed
}

// keyOf encodes a key's values, false when one of them cannot be keyed (a NaN).
// The result aliases ix.buf.
func (ix *rsUniqIndex) keyOf(vals []Value) ([]byte, bool) {
	b := ix.buf[:0]
	for i, v := range vals {
		switch v.Typ {
		case Null:
			b = append(b, 'z') // never matches a stored key, which has no NULL
		case Int:
			b = binary.LittleEndian.AppendUint64(append(b, 'n'), uint64(v.I))
		case Float:
			f := v.F
			if math.IsNaN(f) {
				return nil, false
			}
			if f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63 {
				b = binary.LittleEndian.AppendUint64(append(b, 'n'), uint64(int64(f)))
			} else {
				b = binary.LittleEndian.AppendUint64(append(b, 'f'), math.Float64bits(f))
			}
		case Text:
			t := v.S
			if ix.colls[i] == uqRtrim {
				t = rtrimTrailingSpaces(t)
			}
			b = binary.LittleEndian.AppendUint32(append(b, 't'), uint32(len(t)))
			if ix.colls[i] == uqNocase {
				for _, c := range t {
					b = append(b, asciiLowerByte(c))
				}
			} else {
				b = append(b, t...)
			}
		case Blob:
			b = binary.LittleEndian.AppendUint32(append(b, 'b'), uint32(len(v.S)))
			b = append(b, v.S...)
		default:
			return nil, false
		}
	}
	ix.buf = b
	return b, true
}

func (ix *rsUniqIndex) add(rowid uint64, vals []Value) {
	key, ok, loose := ix.rowKey(rowid, vals)
	switch {
	case loose:
		ix.loose = append(ix.loose, rowid)
	case ok:
		ix.keys[string(key)] = append(ix.keys[string(key)], rowid)
	}
}

func (ix *rsUniqIndex) remove(rowid uint64, vals []Value) {
	key, ok, loose := ix.rowKey(rowid, vals)
	switch {
	case loose:
		ix.loose = slices.DeleteFunc(ix.loose, func(r uint64) bool { return r == rowid })
	case ok:
		k := string(key)
		list := slices.DeleteFunc(ix.keys[k], func(r uint64) bool { return r == rowid })
		if len(list) == 0 {
			delete(ix.keys, k)
		} else {
			ix.keys[k] = list
		}
	}
}

// noteUniqWrite maintains every built unique index across one row write: old is
// the row being replaced (nil for an insert), neu the row now stored (nil for a
// delete).
func (s *rowStore) noteUniqWrite(rowid uint64, old, neu []Value) {
	for _, ix := range s.uqIdx {
		if old != nil {
			ix.remove(rowid, old)
		}
		if neu != nil {
			ix.add(rowid, neu)
		}
	}
}
