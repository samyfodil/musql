package engine

import (
	"slices"
	"strings"
)

// An IN over a list of literals and bound parameters was a comparison per
// element per row (compileIn's chain). C builds an ephemeral index of a list
// of three or more constants once and probes it per row (sqlite3FindInIndex,
// IN_INDEX_EPH); OpInList is that index as a set built once per run: its
// values sorted in registers when they are all integers, where native code
// searches them too, and otherwise hashed with in_hash.go's keys.
//
// Equality is the chain's: each value takes X's own affinity (exprINAffinity
// of a list) and compares under X's collation, and a NULL on either side with
// no match is NULL (finalizeIn). NOT IN is the OpNot of it.

// inListSetMin is the shortest list made a set: C codes two or fewer terms as
// comparisons (IN_INDEX_NOOP).
const inListSetMin = 3

// inListOffForTest keeps every list a chain of comparisons, the reference a
// test compares the set against.
var inListOffForTest bool

// inListPlan is OpInList's P4. P1 is X's register, P2 the result's, and P3 a
// flag register followed by one register per element: a run's first probe
// writes the sorted integers there and their count in the flag, or -1 in the
// flag when the values are not all integers or one is NULL.
type inListPlan struct {
	lits   []Value // the literal elements
	params []int   // the 1-based indices of the parameter elements
	aff    affinity
	coll   string // "", NOCASE or RTRIM
	// ints is the literals' integers, sorted and distinct, when there are no
	// parameters and every literal is one: the keys the program JIT compiles
	// the probe from.
	ints []int64
	// set is the literals' keys when there are no parameters and ints does
	// not hold them; slot is the subCache slot a run keys a list with
	// parameters in.
	set  *inListSet
	slot int
}

// inListSet is a list's values keyed as the chain compares them.
type inListSet struct {
	keys    map[inHashKey]struct{}
	hasNull bool
}

// newInListPlan is the plan for "X IN (list)" with X's affinity and
// collation, or nil when the list is a chain: too short, an element other
// than a literal, a sign over one or a bound parameter, or a collation whose
// equality is not bytes.
func newInListPlan(list []Expr, aff affinity, coll string) *inListPlan {
	if inListOffForTest || len(list) < inListSetMin {
		return nil
	}
	switch {
	case coll == "" || strings.EqualFold(coll, "BINARY"):
		coll = ""
	case strings.EqualFold(coll, "NOCASE"):
		coll = "NOCASE"
	case strings.EqualFold(coll, "RTRIM"):
		coll = "RTRIM"
	default:
		return nil
	}
	plan := &inListPlan{aff: aff, coll: coll, slot: -1}
	for _, e := range list {
		if p, ok := e.(ParamExpr); ok {
			plan.params = append(plan.params, p.Index)
			continue
		}
		v, ok := inListConst(e)
		if !ok {
			return nil
		}
		plan.lits = append(plan.lits, v)
	}
	if len(plan.params) > 0 {
		return plan
	}
	keys, hasNull, ok := inListKeys(plan.lits, aff, coll)
	if !ok {
		return nil
	}
	if ints, all := inListInts(keys); all && !hasNull {
		plan.ints = ints
		return plan
	}
	plan.set = newInListSet(keys, hasNull)
	return plan
}

// inListConst is e's value when e is a literal, or a sign over one folded as
// OpNegative computes it.
func inListConst(e Expr) (Value, bool) {
	switch x := e.(type) {
	case LiteralExpr:
		return x.Val, x.deferredErr == ""
	case UnaryExpr:
		if x.Op != "-" && x.Op != "+" {
			return Value{}, false
		}
		v, ok := inListConst(x.X)
		if ok && x.Op == "-" {
			v = negateValueUnary(v)
		}
		return v, ok
	}
	return Value{}, false
}

// inListKeys keys vals; ok is false when one cannot be keyed (a NaN), which
// leaves the comparisons to answer.
func inListKeys(vals []Value, aff affinity, coll string) (keys []inHashKey, hasNull, ok bool) {
	keys = make([]inHashKey, 0, len(vals))
	for _, v := range vals {
		k, null, ok := inListKey(v, aff, coll)
		if !ok {
			return nil, false, false
		}
		if null {
			hasNull = true
			continue
		}
		keys = append(keys, k)
	}
	return keys, hasNull, true
}

// inListInts is keys as sorted, distinct integers, when every one is.
func inListInts(keys []inHashKey) ([]int64, bool) {
	ints := make([]int64, 0, len(keys))
	for _, k := range keys {
		if k.class != 'i' {
			return nil, false
		}
		ints = append(ints, k.i)
	}
	slices.Sort(ints)
	return slices.Compact(ints), true
}

func newInListSet(keys []inHashKey, hasNull bool) *inListSet {
	s := &inListSet{keys: make(map[inHashKey]struct{}, len(keys)), hasNull: hasNull}
	for _, k := range keys {
		s.keys[k] = struct{}{}
	}
	return s
}

// nativeKeys is what the program JIT compiles the probe from, for a list of
// integers compared as integers: the literals' integers, sorted, and the
// parameters, whose caller declines a bound value that is not an INTEGER.
func (plan *inListPlan) nativeKeys() (ints []int64, params []int, ok bool) {
	if plan == nil || plan.aff != affNone && plan.aff != affNumeric {
		return nil, nil, false
	}
	ints = plan.ints
	if len(plan.params) > 0 {
		keys, hasNull, ok := inListKeys(plan.lits, plan.aff, plan.coll)
		if !ok || hasNull {
			return nil, nil, false
		}
		var all bool
		if ints, all = inListInts(keys); !all {
			return nil, nil, false
		}
	} else if ints == nil {
		return nil, nil, false
	}
	for _, k := range ints {
		if int64(int(k)) != k {
			return nil, nil, false // a constant the IR's int cannot hold
		}
	}
	return ints, plan.params, true
}

// inListKey is inKeyOf with a TEXT value keyed under coll: NOCASE compares
// letters folded to lower case and stops at the first NUL of either side,
// breaking a tie by length (nocaseCompare), and RTRIM ignores trailing
// spaces.
func inListKey(v Value, aff affinity, coll string) (inHashKey, bool, bool) {
	k, null, ok := inKeyOf(v, aff)
	if !ok || null || k.class != 't' {
		return k, null, ok
	}
	switch coll {
	case "NOCASE":
		k.i = int64(len(k.s))
		s := k.s
		if i := strings.IndexByte(s, 0); i >= 0 {
			s = s[:i+1]
		}
		b := []byte(s)
		for i, c := range b {
			b[i] = asciiLowerByte(c)
		}
		k.s = string(b)
	case "RTRIM":
		k.s = string(rtrimTrailingSpaces([]byte(k.s)))
	}
	return k, false, true
}

// inList is OpInList's answer for the current row.
func (m *vdbe) inList(op *Instruction) Value {
	plan := op.P4.(*inListPlan)
	x := m.regs[op.P1]
	if x.Typ == Null {
		return Value{Typ: Null}
	}
	// A UTF-16 database compares TEXT in its own encoding, where two byte
	// strings that are not valid UTF-8 may come out alike.
	if isUTF16(m.encoding()) {
		return m.inListScan(x, plan)
	}
	if m.regs[op.P3].Typ != Int {
		m.inListBuild(op, plan)
	}
	if x.Typ == Int && plan.aff != affText {
		if n := m.regs[op.P3].I; n >= 0 {
			return boolValue(inListSearch(m.regs[op.P3+1:op.P3+1+int(n)], x.I))
		}
	}
	k, _, ok := inListKey(x, plan.aff, plan.coll)
	if !ok {
		return m.inListScan(x, plan)
	}
	if n := m.regs[op.P3].I; n >= 0 {
		return boolValue(k.class == 'i' && inListSearch(m.regs[op.P3+1:op.P3+1+int(n)], k.i))
	}
	s := plan.set
	if s == nil && plan.slot >= 0 && plan.slot < len(m.subCache) {
		s = m.subCache[plan.slot].inList
	}
	if s == nil {
		return m.inListScan(x, plan)
	}
	_, found := s.keys[k]
	return finalizeIn(found, s.hasNull, false)
}

// inListBuild keys the list for this run: its integers sorted into the
// registers after P3 with their count in P3, or -1 in P3 and the set in
// plan.set or the subCache slot.
func (m *vdbe) inListBuild(op *Instruction, plan *inListPlan) {
	m.regs[op.P3] = Value{Typ: Int, I: -1}
	ints := plan.ints
	if len(plan.params) > 0 {
		vals := slices.Clip(plan.lits)
		for _, p := range plan.params {
			vals = append(vals, paramAt(m.params, p))
		}
		keys, hasNull, ok := inListKeys(vals, plan.aff, plan.coll)
		if !ok {
			return
		}
		var all bool
		if ints, all = inListInts(keys); !all || hasNull {
			if plan.slot >= 0 && plan.slot < len(m.subCache) {
				m.subCache[plan.slot].inList = newInListSet(keys, hasNull)
			}
			return
		}
	}
	if ints == nil && plan.set != nil {
		return
	}
	for i, k := range ints {
		m.regs[op.P3+1+i] = Value{Typ: Int, I: k}
	}
	m.regs[op.P3] = Value{Typ: Int, I: int64(len(ints))}
}

// inListSearch reports whether x is among keys, INTEGERs in ascending order.
func inListSearch(keys []Value, x int64) bool {
	lo, hi := 0, len(keys)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if keys[mid].I < x {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo < len(keys) && keys[lo].I == x
}

// inListScan is the chain of comparisons compileIn codes for the list,
// answering what the set cannot key.
func (m *vdbe) inListScan(x Value, plan *inListPlan) Value {
	coll := plan.coll
	if coll == "" {
		coll = "BINARY"
	}
	enc := m.encoding()
	sawNull := false
	eq := func(v Value) bool {
		if v.Typ == Null {
			sawNull = true
			return false
		}
		if x.Typ == Int && v.Typ == Int {
			return x.I == v.I
		}
		l, r := x, v
		if plan.aff != affNone {
			if !affinityIsIdentity(l.Typ, plan.aff) {
				l = applyAffinityToValue(l, plan.aff)
			}
			if !affinityIsIdentity(r.Typ, plan.aff) {
				r = applyAffinityToValue(r, plan.aff)
			}
		}
		return compareValuesCollatedEnc(l, r, coll, enc) == 0
	}
	for _, v := range plan.lits {
		if eq(v) {
			return boolValue(true)
		}
	}
	for _, p := range plan.params {
		if eq(paramAt(m.params, p)) {
			return boolValue(true)
		}
	}
	return finalizeIn(false, sawNull, false)
}
