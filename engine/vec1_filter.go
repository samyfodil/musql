package engine

// vec1's metadata filters: a WHERE constraint on a metadata column that a
// nearest-neighbour query applies inside the index, before the K nearest are
// chosen (vec1MetaValueFilter, vec1FilterToIntOp, vec1MetaFilterIntTail,
// vec1DoMetaFilters). Their comparisons are vec1's own -- an INTEGER against a
// REAL as two doubles, TEXT and BLOB by memcmp, a REAL bound narrowed to an
// integer for the integer layouts by truncation -- which is not always SQL's,
// and the rows a query returns are the ones these let through.

import (
	"bytes"
	"math"
)

// The filter operators (VEC1_OP_*), as the idxStr letters vec1 uses.
const (
	vec1OpEQ      = 'A'
	vec1OpLT      = 'B'
	vec1OpGT      = 'C'
	vec1OpLE      = 'D'
	vec1OpGE      = 'E'
	vec1OpIS      = 'F'
	vec1OpISNULL  = 'G'
	vec1OpNOTNULL = 'H'
	vec1OpIN      = 'I'
)

// vec1Filter is Vec1Filter: an operator, the metadata column on its left and
// the value on its right. An IN filter's values follow it in the array, as
// EQ filters, iVal of them.
type vec1Filter struct {
	op   byte
	meta int
	typ  ValueType
	i    int64 // INTEGER value; TEXT/BLOB length; IN: how many values follow
	f    float64
	s    []byte
}

// vec1ValueToFilter is vec1ValueToFilter.
func vec1ValueToFilter(v Value, f *vec1Filter) {
	f.typ = v.Typ
	switch v.Typ {
	case Int:
		f.i = v.I
	case Float:
		f.f = v.F
	case Text, Blob:
		f.i = int64(len(v.S))
		f.s = append([]byte(nil), v.S...)
	}
}

func vec1Compare[T int64 | float64](a, b T) int {
	switch {
	case a == b:
		return 0
	case a > b:
		return 1
	}
	return -1
}

// vec1MetaFilter is vec1MetaValueFilter: true when the value is excluded.
// filters[0] is the filter; an IN filter's values follow it.
func vec1MetaFilter(filters []vec1Filter, m *vec1MetaValue) bool {
	f := &filters[0]
	if f.op == vec1OpIN {
		for i := range int(f.i) {
			if !vec1MetaFilter(filters[i+1:], m) {
				return false
			}
		}
		return true
	}
	var cmp int
	switch m.typ {
	case Int:
		switch f.typ {
		case Null:
			return true
		case Int:
			cmp = vec1Compare(f.i, m.i)
		case Float:
			cmp = vec1Compare(f.f, float64(m.i))
		default:
			cmp = 1
		}
	case Float:
		switch f.typ {
		case Null:
			return true
		case Int:
			cmp = vec1Compare(float64(f.i), m.f)
		case Float:
			cmp = vec1Compare(f.f, m.f)
		default:
			cmp = 1
		}
	case Null:
		return !(f.op == vec1OpIS && f.typ == Null)
	case Text:
		switch f.typ {
		case Null:
			return true
		case Int, Float:
			cmp = -1
		case Text:
			n := min(m.i, f.i)
			if n > 0 {
				cmp = bytes.Compare(f.s[:n], m.s[:n])
			}
			if cmp == 0 {
				cmp = vec1Compare(f.i, m.i)
			}
		default:
			cmp = 1
		}
	default:
		switch f.typ {
		case Null:
			return true
		case Int, Float, Text:
			cmp = -1
		default:
			n := min(m.i, f.i)
			if n > 0 {
				cmp = bytes.Compare(f.s[:n], m.s[:n])
			}
			if cmp == 0 {
				cmp = vec1Compare(f.i, m.i)
			}
		}
	}
	switch f.op {
	case vec1OpIS, vec1OpEQ:
		return cmp != 0
	case vec1OpLT:
		return !(cmp > 0)
	case vec1OpLE:
		return !(cmp >= 0)
	case vec1OpGT:
		return !(cmp < 0)
	case vec1OpGE:
		return !(cmp <= 0)
	}
	return false // VEC1_OP_NOTNULL
}

// vec1FilterToIntOp is vec1FilterToIntOp: the filter as a comparison with one
// integer of a 1-byte or 4-byte list (whose range is [lo, hi] and NULL null),
// or op 0 when nothing in the list can match. An IN gives its EQ values.
func vec1FilterToIntOp(filters []vec1Filter, lo, hi, null int64) (op byte, rval int64, rvals []int64) {
	f := &filters[0]
	res := -1
	switch {
	case f.op == vec1OpIN:
		for i := range int(f.i) {
			if o, r, _ := vec1FilterToIntOp(filters[i+1:], lo, hi, null); o == vec1OpEQ {
				rvals = append(rvals, r)
			}
		}
		return vec1OpIN, int64(len(rvals)), rvals
	case f.op == vec1OpNOTNULL:
		res = 1
	case f.typ == Null && f.op == vec1OpIS:
		rval, op = null, vec1OpEQ
	case f.typ != Null:
		v := f.i
		switch f.typ {
		case Text, Blob:
			v = hi + 10
		case Float:
			v = vec1TruncInt64(f.f)
			if float64(v) != f.f {
				switch f.op {
				case vec1OpEQ, vec1OpIS:
					v = lo - 10
				case vec1OpLT, vec1OpGE:
					v++
				}
			}
		}
		switch {
		case v < lo:
			if f.op == vec1OpGT || f.op == vec1OpGE {
				res = 1
			} else {
				res = 0
			}
		case v > hi:
			if f.op == vec1OpLT || f.op == vec1OpLE {
				res = 1
			} else {
				res = 0
			}
		default:
			rval = int64(int32(v))
			op = f.op
			if op == vec1OpIS {
				op = vec1OpEQ
			}
		}
	}
	if res > 0 {
		op, rval = vec1OpLT, null
	}
	return op, rval, nil
}

// vec1TruncInt64 is C's "(i64)fVal" as an x86-64 build computes it: the
// integer part, and INT64_MIN for anything outside the range.
func vec1TruncInt64(f float64) int64 {
	if f != f || f >= 9223372036854775808.0 || f < -9223372036854775808.0 {
		return math.MinInt64
	}
	return int64(f)
}

// vec1IntListFilter is vec1MetaFilterIntList and vec1MetaFilterIntTail: each
// entry of a 1-byte (sz 1) or 4-byte list that fails the filter gets its bit
// set in mask.
func vec1IntListFilter(filters []vec1Filter, sz int, nEntry int, meta []byte, mask []byte) bool {
	if len(meta) < vec1MetaHdr+nEntry*sz {
		return false
	}
	lo, hi, null := int64(0), int64(254), int64(vec1Meta1Null)
	if sz == 4 {
		lo, hi, null = vec1Meta4Min, vec1Meta4Max, vec1Meta4Null
	}
	op, rval, rvals := vec1FilterToIntOp(filters, lo, hi, null)
	if op == 0 {
		for i := range mask {
			mask[i] = 0xFF
		}
		return true
	}
	a := meta[vec1MetaHdr:]
	r32 := int32(rval)
	for j := range nEntry {
		var v int32
		if sz == 1 {
			v = int32(a[j])
		} else {
			v = int32(be.Uint32(a[4*j:]))
		}
		var set bool
		switch op {
		case vec1OpEQ:
			set = v == r32
		case vec1OpGT:
			set = v > r32 && int64(v) != null
		case vec1OpLT:
			set = v < r32
		case vec1OpGE:
			set = v >= r32 && int64(v) != null
		case vec1OpLE:
			set = v <= r32
		default:
			for _, x := range rvals {
				if v == int32(x) {
					set = true
					break
				}
			}
		}
		if !set {
			mask[j/8] |= 1 << (j % 8)
		}
	}
	return true
}

// vec1ApplyFilter runs one filter (and an IN's values) over one metadata
// list into mask. ok is false for a list too short for its entries.
func vec1ApplyFilter(filters []vec1Filter, nEntry int, meta []byte, mask []byte) bool {
	if len(meta) < vec1MetaHdr {
		return false
	}
	f := be.Uint32(meta)
	switch {
	case f&vec1Meta1Byte != 0:
		return vec1IntListFilter(filters, 1, nEntry, meta, mask)
	case f&vec1Meta4Byte != 0:
		return vec1IntListFilter(filters, 4, nEntry, meta, mask)
	case f&vec1MetaReal != 0:
		if len(meta) < vec1MetaHdr+8*nEntry {
			return false
		}
		for j := range nEntry {
			bits := be.Uint64(meta[vec1MetaHdr+8*j:])
			m := vec1MetaValue{typ: Float, f: math.Float64frombits(bits)}
			if bits == vec1MetaRealNull {
				m = vec1MetaValue{typ: Null}
			}
			if vec1MetaFilter(filters, &m) {
				mask[j/8] |= 1 << (j % 8)
			}
		}
		return true
	}
	off := vec1MetaHdr
	for j := range nEntry {
		m, next, ok := vec1ReadMetaValue(meta, off)
		if !ok {
			return false
		}
		off = next
		if vec1MetaFilter(filters, &m) {
			mask[j/8] |= 1 << (j % 8)
		}
	}
	return true
}
