package engine

import (
	"math"
	"strings"
)

// An uncorrelated "x IN (SELECT ...)" caches the subquery's rows once, and
// inSubMembership then compared x with every one of them, for every outer row:
// "count(*) FROM t WHERE bid IN (SELECT id FROM b WHERE id < ?)" over 100k
// rows was 857ms against C's 3ms, which probes an ephemeral index instead.
//
// inHashSet is that index for the common shape -- one column, BINARY
// collation -- built once from the cached rows. Equality has to be exactly
// inSubMembership's: each row value goes through the same affinity it applies,
// a key carries its storage class (numbers, text and blobs never compare
// equal), a REAL holding an integral value in int64 range is keyed as that
// integer (5 = 5.0, as compareIntFloat answers), and the NULL rule is
// finalizeIn's. Anything else -- a row value, another collation, a correlated
// subquery whose rows change per outer row -- keeps the loop.

// inHashOffForTest sends every IN to the loop, the reference a test compares
// the set against.
var inHashOffForTest bool

type inHashKey struct {
	class byte // 'i' integer-valued number, 'f' other REAL, 't' text, 'b' blob
	i     int64
	f     float64
	s     string
}

type inHashSet struct {
	aff     affinity
	keys    map[inHashKey]struct{}
	hasNull bool
	empty   bool
	from    *[]Value // rows[0] the set was built from, with n, to detect a re-run
	n       int
}

// inSetFor is the membership set for an uncorrelated single-column IN over
// rows, building it on first use; nil when this IN must use the loop.
func (m *vdbe) inSetFor(plan *inSubPlan, slot int, rows [][]Value) *inHashSet {
	if inHashOffForTest || plan.correlated || len(plan.affs) != 1 || slot < 0 || slot >= len(m.subCache) {
		return nil
	}
	if c := plan.colls[0]; c != "" && !strings.EqualFold(c, "BINARY") {
		return nil
	}
	e := &m.subCache[slot]
	var first *[]Value
	if len(rows) > 0 {
		first = &rows[0]
	}
	if s := e.inSet; s != nil && s.from == first && s.n == len(rows) {
		return s
	}
	s := &inHashSet{aff: plan.affs[0], keys: make(map[inHashKey]struct{}, len(rows)), empty: len(rows) == 0, from: first, n: len(rows)}
	for _, r := range rows {
		if len(r) < 1 {
			return nil
		}
		k, null, ok := inKeyOf(r[0], s.aff)
		if !ok {
			return nil
		}
		if null {
			s.hasNull = true
			continue
		}
		s.keys[k] = struct{}{}
	}
	e.inSet = s
	return s
}

// membership is inSubMembership's answer for x.
func (s *inHashSet) membership(x Value, not bool) Value {
	if s.empty {
		return boolValue(not)
	}
	k, null, ok := inKeyOf(x, s.aff)
	if !ok || null {
		return finalizeIn(false, true, not)
	}
	_, found := s.keys[k]
	return finalizeIn(found, s.hasNull, not)
}

// inKeyOf applies the comparison's affinity as inSubMembership does and keys
// the result; ok is false for a value this set cannot key.
func inKeyOf(v Value, aff affinity) (k inHashKey, null, ok bool) {
	if v.Typ == Null {
		return k, true, true
	}
	switch {
	case isNumericAffinity(aff):
		v = applyAffinityToValue(v, affNumeric)
	case aff == affText:
		v = applyAffinityToValue(v, affText)
	}
	switch v.Typ {
	case Int:
		return inHashKey{class: 'i', i: v.I}, false, true
	case Float:
		f := v.F
		if math.IsNaN(f) {
			return k, false, false
		}
		if f == math.Trunc(f) && f >= -9223372036854775808 && f < 9223372036854775808 {
			return inHashKey{class: 'i', i: int64(f)}, false, true
		}
		return inHashKey{class: 'f', f: f}, false, true
	case Text:
		return inHashKey{class: 't', s: string(v.S)}, false, true
	case Blob:
		return inHashKey{class: 'b', s: string(v.S)}, false, true
	}
	return k, false, false
}
