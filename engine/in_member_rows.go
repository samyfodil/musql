package engine

import (
	"encoding/binary"
	"math"
	"sync/atomic"
)

// memberRowsOffForTest turns the leaving out off, and memberRowsLeftOut and
// memberRowsLeftOutNative count the rows left out, all and of native code's,
// for tests.
var (
	memberRowsOffForTest    bool
	memberRowsLeftOut       atomic.Int64
	memberRowsLeftOutNative atomic.Int64
)

// memberSet is the rows an IN subquery's program has returned, by identity:
// the same type, value and bytes. Two identical values compare the same
// under any affinity and collation, so leaving out a row identical to one
// returned before changes no membership; IN reads nothing else of the rows.
type memberSet struct {
	ints   map[int64]struct{}
	keys   map[string]struct{}
	buf    []byte
	dups   int
	native int
}

// seen reports whether row is identical to one before it, recording it when
// it is not.
func (s *memberSet) seen(row []Value) bool {
	if len(row) == 1 && row[0].Typ == Int && row[0].Subtype == 0 && !row[0].intReal {
		if s.ints == nil {
			s.ints = map[int64]struct{}{}
		}
		if _, ok := s.ints[row[0].I]; ok {
			s.dups++
			return true
		}
		s.ints[row[0].I] = struct{}{}
		return false
	}
	b := s.buf[:0]
	for _, v := range row {
		b = append(b, byte(v.Typ), v.Subtype)
		if v.intReal {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
		b = binary.LittleEndian.AppendUint64(b, uint64(v.I))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v.F))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(v.S)))
		b = append(b, v.S...)
	}
	s.buf = b
	if s.keys == nil {
		s.keys = map[string]struct{}{}
	}
	if _, ok := s.keys[string(b)]; ok {
		s.dups++
		return true
	}
	s.keys[string(b)] = struct{}{}
	return false
}

// keepNew drops from rows[from:] each row identical to one before it.
func (s *memberSet) keepNew(rows [][]Value, from int) [][]Value {
	out := rows[:from]
	for _, r := range rows[from:] {
		if !s.seen(r) {
			out = append(out, r)
		}
	}
	s.native += len(rows) - len(out)
	clear(rows[len(out):])
	return out
}
