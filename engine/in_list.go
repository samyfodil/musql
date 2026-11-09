package engine

import (
	"slices"
	"strings"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// C probes an ephemeral index for an IN over three or more constants
// (sqlite3FindInIndex, IN_INDEX_EPH); OpInList is that index as a set built
// once per run: sorted integers in registers, which native code also searches,
// or else a hash of in_hash.go's keys. Equality is compileIn's chain's: X's
// affinity and collation, NULL rules by finalizeIn. NOT IN is its OpNot.

// inListSetMin: C codes two or fewer terms as comparisons (IN_INDEX_NOOP).
const inListSetMin = 3

// inListOffForTest keeps every list a chain, the reference for tests.
var inListOffForTest bool

// inListPlan is OpInList's P4. P1 is X's register, P2 the result's, and P3 a
// flag register then one register per element: a run's first probe writes the
// sorted integers there and their count in the flag, or -1 for a hashed set.
type inListPlan struct {
	lits   []Value // the literal elements
	params []int   // the 1-based indices of the parameter elements
	aff    affinity
	coll   string // "", NOCASE or RTRIM
	// ints is the literals' sorted, distinct integers when there are no
	// parameters and every literal is one.
	ints []int64
	// set is the literals' keys otherwise; slot is the subCache slot that
	// holds a run's set when there are parameters.
	set  *inListSet
	slot int
	// texts is the literals as native code matches them (jit.VInText) when
	// every one is a TEXT and they compare as bytes: TEXT affinity, BINARY.
	texts []jit.VTextLit
}

type inListSet struct {
	keys    map[inHashKey]struct{}
	hasNull bool
}

// newInListPlan is the plan for "X IN (list)", or nil when the list stays a
// chain: too short, an element other than a literal, signed literal or
// parameter, or a collation whose equality is not bytes.
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
	if aff == affText && coll == "" && !hasNull {
		texts := make([]jit.VTextLit, 0, len(plan.lits))
		for _, v := range plan.lits {
			if v.Typ != Text {
				texts = nil
				break
			}
			texts = append(texts, jit.VTextLit{Ptr: unsafe.Pointer(unsafe.SliceData(v.S)), Len: int64(len(v.S))})
		}
		plan.texts = texts
	}
	if ints, all := inListInts(keys); all && !hasNull {
		plan.ints = ints
		return plan
	}
	plan.set = newInListSet(keys, hasNull)
	return plan
}

// inListConst is e's value when e is a literal or a sign over one.
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

// inListKeys keys vals; false when one cannot be keyed (a NaN).
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

// nativeKeys is what the program JIT compiles the probe from, for integers
// compared as integers; the caller declines a bound value that is not one.
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

// inListKey is inKeyOf with TEXT keyed under coll: NOCASE folds ASCII case up
// to the first NUL and keeps the length as a tie-break (nocaseCompare), RTRIM
// drops trailing spaces.
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

func (m *vdbe) inList(op *Instruction) Value {
	plan := op.P4.(*inListPlan)
	x := m.regs[op.P1]
	if x.Typ == Null {
		return Value{Typ: Null}
	}
	// UTF-16 TEXT compares in its own encoding, where two invalid UTF-8 byte
	// strings may come out alike.
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

// inListRun is a list keyed for one run with its parameters bound, shared by
// OpInList and the columnar GROUP BY walk (segList).
type inListRun struct {
	aff  affinity
	coll string
	ints []int64
	set  *inListSet
}

// newInListRun keys plan's values for a run; false when the comparisons must
// answer instead: a UTF-16 database or a value that cannot be keyed.
func newInListRun(plan *inListPlan, params []Value, enc TextEncoding) (*inListRun, bool) {
	if isUTF16(enc) {
		return nil, false
	}
	r := &inListRun{aff: plan.aff, coll: plan.coll, ints: plan.ints, set: plan.set}
	if len(plan.params) == 0 {
		return r, true
	}
	vals := slices.Clip(plan.lits)
	for _, p := range plan.params {
		vals = append(vals, paramAt(params, p))
	}
	keys, hasNull, ok := inListKeys(vals, plan.aff, plan.coll)
	if !ok {
		return nil, false
	}
	if ints, all := inListInts(keys); all && !hasNull {
		r.ints, r.set = ints, nil
	} else {
		r.ints, r.set = nil, newInListSet(keys, hasNull)
	}
	return r, true
}

// membership is the IN's answer for x: 1, 0 or NULL; false when x cannot be
// keyed.
func (r *inListRun) membership(x Value) (Value, bool) {
	if x.Typ == Null {
		return Value{Typ: Null}, true
	}
	if x.Typ == Int && r.ints != nil && r.aff != affText {
		_, found := slices.BinarySearch(r.ints, x.I)
		return boolValue(found), true
	}
	k, _, ok := inListKey(x, r.aff, r.coll)
	if !ok {
		return Value{}, false
	}
	if r.ints != nil {
		found := false
		if k.class == 'i' {
			_, found = slices.BinarySearch(r.ints, k.i)
		}
		return boolValue(found), true
	}
	_, found := r.set.keys[k]
	return finalizeIn(found, r.set.hasNull, false), true
}

// inListBuild keys the list for this run: sorted integers after P3 with their
// count in P3, or -1 in P3 and the set in plan.set or the subCache slot.
func (m *vdbe) inListBuild(op *Instruction, plan *inListPlan) {
	m.regs[op.P3] = Value{Typ: Int, I: -1}
	r, ok := newInListRun(plan, m.params, m.encoding())
	if !ok {
		return
	}
	if r.ints == nil {
		if len(plan.params) > 0 && plan.slot >= 0 && plan.slot < len(m.subCache) {
			m.subCache[plan.slot].inList = r.set
		}
		return
	}
	for i, k := range r.ints {
		m.regs[op.P3+1+i] = Value{Typ: Int, I: k}
	}
	m.regs[op.P3] = Value{Typ: Int, I: int64(len(r.ints))}
}

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

// inListScan is compileIn's chain of comparisons, for what the set cannot key.
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
