package engine

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/samyfodil/musql/internal/fir"
	"github.com/samyfodil/musql/internal/jit"
	"github.com/samyfodil/musql/internal/libm"
)

// REAL values in a compiled loop body.
//
// A REAL lives in a program register as its IEEE bits and NULL as a NaN
// (jit.POpI2F and after). Which registers hold REALs is decided once per
// lowering, from what the body's instructions write: a REAL column's block, a
// REAL literal or parameter, arithmetic with a REAL operand, a math function.
// Which columns are REAL is not known when the statement is prepared -- it is
// a property of the segments the table has now -- so a plan lowers its loop
// again for each combination it meets (segKinds), and caches the result.

// segKinds is what a lowering is specialized to: the table columns whose
// blocks hold REALs, and the parameters bound to one. The zero value is
// every column and parameter an INTEGER.
type segKinds struct {
	realCol   map[int]bool
	realParam map[int]bool
}

// key names the combination, for the plan's cache.
func (k *segKinds) key() string {
	if k == nil {
		return ""
	}
	var b strings.Builder
	for _, m := range []map[int]bool{k.realCol, k.realParam} {
		keys := make([]int, 0, len(m))
		for c, on := range m {
			if on {
				keys = append(keys, c)
			}
		}
		slices.Sort(keys)
		for _, c := range keys {
			b.WriteString(strconv.Itoa(c))
			b.WriteByte(',')
		}
		b.WriteByte(';')
	}
	return b.String()
}

func (k *segKinds) col(c int) bool   { return k != nil && k.realCol[c] }
func (k *segKinds) param(p int) bool { return k != nil && k.realParam[p] }

// segRegReals types the registers body writes: true for one holding a REAL.
// A register written as a REAL in one place and an INTEGER in another is
// refused, since a program register has one representation.
func segRegReals(body []Instruction, cursor int, kinds *segKinds, nullOnly map[int]bool) (map[int]bool, bool) {
	real := map[int]bool{}
	written := map[int]bool{}
	ok := true
	set := func(r int, isReal bool) {
		if written[r] && real[r] != isReal {
			ok = false
		}
		written[r] = true
		if isReal {
			real[r] = true
		}
	}
	for _, in := range body {
		switch in.Op {
		case OpColumn:
			set(in.P3, in.P1 == cursor && !nullOnly[in.P3] && kinds.col(in.P2))
		case OpInteger, OpInt64:
			set(in.P2, false)
		case OpReal:
			set(in.P2, true)
		case OpVariable:
			set(in.P2, kinds.param(in.P1))
		case OpSCopy, OpCopy:
			set(in.P2, real[in.P1])
		case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder:
			set(in.P3, real[in.P1] || real[in.P2])
		case OpNegative:
			set(in.P2, real[in.P1])
		case OpNot, OpBitNot:
			set(in.P2, false)
		case OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
			set(in.P3, false)
		case OpGt, OpGe, OpLt, OpLe, OpEq, OpNe:
			if in.P5&p5StoreP2 != 0 {
				set(in.P2, false)
			}
		case OpFunction:
			set(in.P3, segFuncReal(in, real))
		case OpInList, OpLike, OpGlob:
			set(in.P2, false)
		}
	}
	return real, ok
}

// segFuncReal reports whether the call in answers a REAL: a math function,
// or round(); abs(), ceil(), floor() and trunc() keep their argument's type.
// Any other function is taken to answer an INTEGER, which is all a program
// register outside these holds; a block writing back anything else declines.
func segFuncReal(in Instruction, real map[int]bool) bool {
	name, _ := in.P4.(string)
	switch name = strings.ToLower(name); name {
	case "abs", "ceil", "ceiling", "floor", "trunc":
		return in.P2 == 1 && real[in.P1]
	case "round":
		return true
	}
	_, isMath := mathFuncArgs[name]
	return isMath
}

// segIsMathCall reports whether in calls one of the math functions, or
// round() with no digits, which has a kernel too.
func segIsMathCall(in Instruction) bool {
	name, ok := in.P4.(string)
	if !ok || in.Op != OpFunction {
		return false
	}
	name = strings.ToLower(name)
	if name == "round" {
		return in.P2 == 1
	}
	_, isMath := mathFuncArgs[name]
	return isMath
}

// segRealOp reports whether in is lowered as REAL code: it reads or writes a
// REAL register, loads a REAL literal, or calls a math function.
func segRealOp(in Instruction, real map[int]bool) bool {
	switch in.Op {
	case OpReal:
		return true
	case OpFunction:
		if segIsMathCall(in) {
			return true
		}
	}
	for _, r := range segLowerRegOperands(in) {
		if r >= 0 && real[r] {
			return true
		}
	}
	return false
}

// segIntSources finds the registers written once, by an INTEGER literal
// (lits) or a parameter (params: its index).
func segIntSources(body []Instruction) (lits map[int]bool, params map[int]int) {
	writes := map[int]int{}
	for _, in := range body {
		_, w, _, _ := segInsnRegs(in)
		for _, r := range w {
			writes[r]++
		}
	}
	lits, params = map[int]bool{}, map[int]int{}
	for _, in := range body {
		switch {
		case in.Op == OpInteger && writes[in.P2] == 1:
			lits[in.P2] = true
		case in.Op == OpVariable && writes[in.P2] == 1:
			params[in.P2] = in.P1
		}
	}
	return lits, params
}

// segRealNative reports whether an instruction lowered as REAL code has a
// native form; the others run in service blocks.
func segRealNative(in Instruction, cursor int, real, lits map[int]bool, params map[int]int) bool {
	switch in.Op {
	case OpColumn:
		return in.P1 == cursor
	case OpReal, OpSCopy, OpVariable:
		return true
	case OpAdd, OpSubtract, OpMultiply, OpDivide:
		return real[in.P1] || real[in.P2]
	case OpNegative:
		return real[in.P1]
	case OpIsNull, OpNotNull:
		return true
	case OpGt, OpGe, OpLt, OpLe, OpEq, OpNe:
		_, ok := segRealCompareLowerable(in, real, lits, params)
		return ok
	case OpFunction:
		_, ok := segRealCall(in, real)
		return ok
	}
	return false
}

// segRealCompareLowerable reports whether a comparison with a REAL operand
// compiles, and whether it stores its result. Affinity cannot matter: neither
// operand is TEXT. IS and IS NOT are refused, as for integers. An INTEGER
// operand is compared as a double, which is exact only for an INTEGER a
// double holds: a literal, or a parameter checked when it is bound.
func segRealCompareLowerable(in Instruction, real, lits map[int]bool, params map[int]int) (store, ok bool) {
	if in.P5&p5NullEq != 0 {
		return false, false
	}
	switch affinity(in.P5 & p5AffMask) {
	case affNone, affNumeric, affInteger, affReal:
	default:
		return false, false
	}
	if !real[in.P1] && !real[in.P3] {
		return false, false
	}
	for _, r := range []int{in.P1, in.P3} {
		if _, isParam := params[r]; !real[r] && !lits[r] && !isParam {
			return false, false
		}
	}
	return in.P5&p5StoreP2 != 0, true
}

// lowerReal lowers body[i], an instruction segRealOp picked, whose native
// form segRealNative vouched for. done is false for the instructions the
// INTEGER lowering's form serves for a REAL too: a column load, a copy, a
// parameter.
func (l *segLowered) lowerReal(body []Instruction, i, base int, nextReg *int, scratch int, lits map[int]bool, params map[int]int, note func(int)) (done, ok bool) {
	in := body[i]
	real := l.real
	asReal := func(r int) int {
		if real[r] {
			return r
		}
		t := *nextReg
		*nextReg++
		note(t)
		l.add(jit.ProgInsn{Op: jit.POpI2F, A: t, B: r})
		return t
	}
	switch in.Op {
	case OpReal:
		f, isF := in.P4.(float64)
		if !isF {
			return false, false
		}
		note(in.P2)
		l.add(jit.ProgInsn{Op: jit.POpSetConst, A: in.P2, B: int(math.Float64bits(f))})
		return true, true
	case OpAdd, OpSubtract, OpMultiply, OpDivide:
		// r[P3] = r[P2] op r[P1], an INTEGER operand read as a double as
		// sqlite3VdbeRealValue reads it.
		op := map[OpCode]jit.POp{OpAdd: jit.POpFAdd, OpSubtract: jit.POpFSub, OpMultiply: jit.POpFMul, OpDivide: jit.POpFDiv}[in.Op]
		b, c := asReal(in.P2), asReal(in.P1)
		note(in.P3)
		l.add(jit.ProgInsn{Op: op, A: in.P3, B: b, C: c})
		return true, true
	case OpNegative:
		// The sign flipped, -0.0 for 0.0 included: x * -1.0, exactly.
		t := *nextReg
		*nextReg++
		note(t)
		l.add(jit.ProgInsn{Op: jit.POpSetConst, A: t, B: int(math.Float64bits(-1))})
		note(in.P2)
		l.add(jit.ProgInsn{Op: jit.POpFMul, A: in.P2, B: in.P1, C: t})
		return true, true
	case OpGt, OpGe, OpLt, OpLe, OpEq, OpNe:
		store, okCmp := segRealCompareLowerable(in, real, lits, params)
		cond, okCond := jitCondOfOpcode(in.Op)
		if !okCmp || !okCond {
			return false, false
		}
		for _, r := range []int{in.P1, in.P3} {
			if p, isParam := params[r]; isParam && !real[r] {
				l.exactParams = append(l.exactParams, p)
			}
		}
		x, y := asReal(in.P3), asReal(in.P1)
		if store {
			note(in.P2)
			l.add(jit.ProgInsn{Op: jit.POpFCmp, A: in.P2, B: x, C: y, Cond: cond, Imm: segCmpNullAnswer(body, i, in.P2)})
			return true, true
		}
		// The jump form: with a NULL it jumps only under p5JumpIfNull.
		var null int64
		if in.P5&p5JumpIfNull != 0 {
			null = 1
		}
		note(scratch)
		l.add(jit.ProgInsn{Op: jit.POpFCmp, A: scratch, B: x, C: y, Cond: cond, Imm: null},
			jit.ProgInsn{Op: jit.POpJumpIfNotZero, A: in.P2 - base, B: scratch})
		return true, true
	case OpIsNull, OpNotNull:
		if !real[in.P1] {
			return false, true
		}
		op := jit.POpJumpIfNotZero
		if in.Op == OpNotNull {
			op = jit.POpJumpIfZero
		}
		note(scratch)
		l.add(jit.ProgInsn{Op: jit.POpFIsNull, A: scratch, B: in.P1}, jit.ProgInsn{Op: op, A: in.P2 - base, B: scratch})
		return true, true
	case OpFunction:
		kind, okCall := segRealCall(in, real)
		if !okCall {
			return false, false
		}
		note(in.P3)
		switch kind {
		case segCallAbs:
			l.add(jit.ProgInsn{Op: jit.POpFAbs, A: in.P3, B: in.P1})
		case segCallIdent:
			l.add(jit.ProgInsn{Op: jit.POpLoadReg, A: in.P3, B: in.P1})
		case segCallPi:
			l.add(jit.ProgInsn{Op: jit.POpSetConst, A: in.P3, B: int(math.Float64bits(math.Pi))})
		default:
			name, _ := in.P4.(string)
			k := segMathKernel(strings.ToLower(name), in.P2)
			b := asReal(in.P1)
			c := 0
			if in.P2 == 2 {
				c = asReal(in.P1 + 1)
			}
			// The kernel leaves an input it does not answer to Go: a block
			// of this one call, which loads the arguments native code wrote.
			load := make([]int, in.P2)
			for a := range load {
				load[a] = in.P1 + a
			}
			bi := len(l.blocks)
			l.blocks = append(l.blocks, segSvcBlock{insns: []Instruction{in}, load: load, writeBack: []int{in.P3}, flag: -1, math: true})
			l.blockIR = append(l.blockIR, len(l.insns))
			l.add(jit.ProgInsn{Op: jit.POpMath, A: in.P3, B: b, C: c, Kernel: k, Imm: int64(bi)})
		}
		return true, true
	}
	return false, true
}

// segRealCall is what a call with a REAL argument or answer lowers to.
type segRealCallKind uint8

const (
	segCallAbs   segRealCallKind = iota + 1 // POpFAbs
	segCallIdent                            // ceil/floor/trunc of an INTEGER: itself
	segCallPi                               // pi(): a constant
	segCallMath                             // a kernel
)

func segRealCall(in Instruction, real map[int]bool) (segRealCallKind, bool) {
	name, isName := in.P4.(string)
	if !isName {
		return 0, false
	}
	name = strings.ToLower(name)
	switch {
	case name == "abs" && in.P2 == 1:
		if real[in.P1] {
			return segCallAbs, true
		}
		return 0, false
	case (name == "ceil" || name == "ceiling" || name == "floor" || name == "trunc") && in.P2 == 1 && !real[in.P1]:
		return segCallIdent, true
	case name == "pi" && in.P2 == 0:
		return segCallPi, true
	}
	if segMathKernel(name, in.P2) == nil {
		return 0, false
	}
	return segCallMath, true
}

// segMathKernels caches the SQL kernels by name, argument count and glibc
// build: a kernel is compiled into every program that calls the function.
var segMathKernels sync.Map // segMathKey -> *fir.Func (nil: none)

type segMathKey struct {
	name  string
	nargs int
	fused bool
}

// segMathKernel is the kernel computing the SQL function name(args) over
// REALs: func.c's semantics around the libm function, NULL (a NaN) in and
// out. nil when the function has none, or this CPU cannot run it.
func segMathKernel(name string, nargs int) *fir.Func {
	key := segMathKey{name, nargs, libm.Fused()}
	if f, ok := segMathKernels.Load(key); ok {
		return f.(*fir.Func)
	}
	f := buildSegMathKernel(name, nargs)
	if f != nil && !jit.KernelOK(f) {
		f = nil
	}
	segMathKernels.Store(key, f)
	return f
}

func buildSegMathKernel(name string, nargs int) *fir.Func {
	if name == "round" && nargs == 1 {
		return buildSegRoundKernel()
	}
	if lo, hi, ok := mathFuncArity(name); !ok || nargs < lo || nargs > hi || nargs == 0 {
		return nil
	}
	k := libm.NewKernel("sql:"+name, nargs)
	null := func(v fir.Val) { k.If(fir.FNe, v, v, func() { k.Return(v) }, nil) }
	positive := func(v fir.Val) {
		k.If(fir.FLe, v, k.K(0), func() { k.Return(k.K(math.NaN())) }, nil)
	}
	x := k.Arg(0)
	// NULL in, NULL out.
	null(x)
	if nargs == 2 {
		y := k.Arg(1)
		switch name {
		case "pow", "power":
			null(y)
			k.Return(k.Pow(x, y))
		case "atan2":
			null(y)
			k.Return(k.Atan2(x, y))
		case "mod":
			null(y)
			k.Return(k.FMod(x, y))
		case "log":
			// mathLog: log(B,X) is ln(X)/ln(B), NULL unless B, ln(B) and
			// X are all positive.
			positive(x)
			b := k.Log(x)
			positive(b)
			null(y)
			positive(y)
			k.Return(k.FDiv(k.Log(y), b))
		default:
			return nil
		}
		f, err := k.Func()
		if err != nil {
			return nil
		}
		return f
	}
	switch name {
	case "sqrt":
		k.Return(k.FSqrt(x))
	case "floor":
		k.Return(k.Floor(x))
	case "ceil", "ceiling":
		k.Return(k.Ceil(x))
	case "trunc":
		k.Return(k.Trunc(x))
	case "radians":
		k.Return(k.FMul(x, k.K(math.Pi/180)))
	case "degrees":
		k.Return(k.FMul(x, k.K(180/math.Pi)))
	case "exp":
		k.Return(k.Exp(x))
	case "ln":
		positive(x)
		k.Return(k.Log(x))
	case "log", "log10":
		positive(x)
		k.Return(k.Log10(x))
	case "log2":
		positive(x)
		k.Return(k.Log2(x))
	case "sin":
		k.Return(k.Sin(x))
	case "cos":
		k.Return(k.Cos(x))
	case "sinh":
		k.Return(k.Sinh(x))
	case "cosh":
		k.Return(k.Cosh(x))
	case "tanh":
		k.Return(k.Tanh(x))
	case "asinh":
		k.Return(k.Asinh(x))
	case "acosh":
		k.Return(k.Acosh(x))
	case "atanh":
		k.Return(k.Atanh(x))
	case "tan":
		k.Return(k.Tan(x))
	case "atan":
		k.Return(k.Atan(x))
	case "asin":
		k.Return(k.Asin(x))
	case "acos":
		k.Return(k.Acos(x))
	default:
		return nil
	}
	f, err := k.Func()
	if err != nil {
		return nil
	}
	return f
}

// segCmpNullAnswer is what a REAL comparison storing into dst at body index i
// may store for a NULL: the value its one reader, an If or IfNot, treats as it
// treats NULL. With any other reader the NULL cannot be represented, and a
// NULL declines the statement.
func segCmpNullAnswer(body []Instruction, i, dst int) int64 {
	reader := -1
	for j, in := range body {
		if j == i {
			continue
		}
		for _, r := range segLowerRegOperands(in) {
			if r != dst {
				continue
			}
			if reader >= 0 && reader != j {
				return jit.ProgNullDeclines
			}
			reader = j
		}
	}
	if reader < 0 {
		return jit.ProgNullDeclines
	}
	in := body[reader]
	if (in.Op != OpIf && in.Op != OpIfNot) || in.P1 != dst {
		return jit.ProgNullDeclines
	}
	// IfNot jumps on false, and on NULL when P3 is set; If jumps on true,
	// and on NULL when P3 is set.
	if (in.Op == OpIfNot) == (in.P3 != 0) {
		return 0
	}
	return 1
}

// buildSegRoundKernel is round(x) with no digits, as roundFunc computes it:
// a double past +-2^52 has no fraction to round, and any other is
// (double)(i64)(x +- 0.5), whose zero is +0.0.
func buildSegRoundKernel() *fir.Func {
	k := libm.NewKernel("sql:round", 1)
	x := k.Arg(0)
	k.If(fir.FNe, x, x, func() { k.Return(x) }, nil)
	k.If(fir.FGt, k.FAbs(x), k.K(4503599627370496), func() { k.Return(x) }, nil)
	h := k.Var(fir.F)
	k.Set(h, k.K(0.5))
	k.If(fir.FLt, x, k.K(0), func() { k.Set(h, k.K(-0.5)) }, nil)
	k.Return(k.FAdd(k.Trunc(k.FAdd(x, h)), k.K(0)))
	f, err := k.Func()
	if err != nil {
		return nil
	}
	return f
}

// segFloatAnswer finalizes a REAL aggregate a program accumulated in FAcc,
// over rows non-NULL values, with the finalizers the VDBE runs.
func segFloatAnswer(agg aggKind, rows int64, acc [2]float64) Value {
	it := aggItem{kind: agg, cnt: rows, rSum: acc[0], rErr: acc[1], sumApprox: rows > 0}
	switch agg {
	case aggSum:
		v, _ := finalizeSum(&it)
		return v
	case aggTotal:
		return resultDouble(it.sumFloat())
	case aggAvg:
		if rows == 0 {
			return Value{Typ: Null}
		}
		return resultDouble(it.sumFloat() / float64(rows))
	case aggMin, aggMax:
		if rows == 0 {
			return Value{Typ: Null}
		}
		return Value{Typ: Float, F: acc[0]}
	}
	return Value{Typ: Null}
}

// segRealBits is v as a REAL register holds it: its bits, a NaN for NULL.
// false for a value a REAL register cannot hold.
func segRealBits(v Value) (int64, bool) {
	switch v.Typ {
	case Float:
		return int64(math.Float64bits(v.F)), true
	case Null:
		return int64(math.Float64bits(math.NaN())), true
	}
	return 0, false
}

// segRealValue is a REAL register's bits as a Value.
func segRealValue(bits int64) Value {
	f := math.Float64frombits(uint64(bits))
	if f != f {
		return Value{Typ: Null}
	}
	return Value{Typ: Float, F: f}
}
