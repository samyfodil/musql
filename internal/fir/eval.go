package fir

import (
	"math"
	"unsafe"
)

// Eval runs f in Go: its answer for args, or exited when it leaves the row to
// Go.
//
// It is how a kernel's transcription is checked against the function it was
// transcribed from, on any machine and before a backend is involved. Nothing
// runs it in place of compiled code.
func Eval(f *Func, args ...float64) (res float64, exited bool) {
	e := evaluator{f: f, fused: f.Fused, vals: make([]uint64, f.NVals()), args: args}
	r, exited, ok := e.run(f.Body)
	if !ok && !exited {
		panic("fir: " + f.Name + ": fell off the end")
	}
	return math.Float64frombits(r), exited
}

type evaluator struct {
	f     *Func
	fused bool
	vals  []uint64
	args  []float64
}

func (e *evaluator) fv(v Val) float64 { return math.Float64frombits(e.vals[v]) }

// run executes steps: done reports a Return (with its bits) or an Exit.
func (e *evaluator) run(steps []Step) (res uint64, exited, done bool) {
	for i := range steps {
		st := &steps[i]
		a, b := e.vals[st.A], e.vals[st.B]
		fa, fb, fc := e.fv(st.A), e.fv(st.B), e.fv(st.C)
		var out uint64
		fl := func(x float64) { out = math.Float64bits(x) }
		switch st.Op {
		case OpArg:
			fl(e.args[st.Imm])
		case OpConst:
			out = st.Imm
		case OpCopy:
			out = a
		case OpFAdd:
			fl(fa + fb)
		case OpFSub:
			fl(fa - fb)
		case OpFMul:
			fl(fa * fb)
		case OpFDiv:
			fl(fa / fb)
		case OpFMod:
			fl(math.Mod(fa, fb))
		case OpFSqrt:
			fl(math.Sqrt(fa))
		case OpFAbs:
			out = a &^ (1 << 63)
		case OpFNeg:
			out = a ^ (1 << 63)
		case OpFMA:
			fl(math.FMA(fa, fb, fc))
		case OpMAdd:
			if e.fused {
				fl(math.FMA(fa, fb, fc))
			} else {
				fl(float64(fa*fb) + fc)
			}
		case OpRoundEven:
			fl(math.RoundToEven(fa))
		case OpFloor:
			fl(math.Floor(fa))
		case OpCeil:
			fl(math.Ceil(fa))
		case OpTrunc:
			fl(math.Trunc(fa))
		case OpFToI:
			out = uint64(int64(fa))
		case OpIToF:
			fl(float64(int64(a)))
		case OpAdd:
			out = a + b
		case OpSub:
			out = a - b
		case OpMul:
			out = a * b
		case OpAnd:
			out = a & b
		case OpOr:
			out = a | b
		case OpXor:
			out = a ^ b
		case OpShl:
			out = a << st.Imm
		case OpShr:
			out = a >> st.Imm
		case OpSar:
			out = uint64(int64(a) >> st.Imm)
		case OpLoadF, OpLoadI:
			if a >= uint64(st.TabLen) {
				panic("fir: " + e.f.Name + ": table index out of range")
			}
			out = *(*uint64)(unsafe.Add(st.Tab, a*8))
		case OpIf:
			body := st.Else
			if compare(st.Cmp, a, b) {
				body = st.Then
			}
			if r, x, d := e.run(body); d {
				return r, x, true
			}
			continue
		case OpExit:
			return 0, true, true
		case OpReturn:
			return a, false, true
		}
		e.vals[st.Dst] = out
	}
	return 0, false, false
}

func compare(c Cmp, a, b uint64) bool {
	fa, fb := math.Float64frombits(a), math.Float64frombits(b)
	switch c {
	case FEq:
		return fa == fb
	case FNe:
		return fa != fb
	case FLt:
		return fa < fb
	case FLe:
		return fa <= fb
	case FGt:
		return fa > fb
	case FGe:
		return fa >= fb
	case IEq:
		return a == b
	case INe:
		return a != b
	case ILt:
		return int64(a) < int64(b)
	case ILe:
		return int64(a) <= int64(b)
	case IGt:
		return int64(a) > int64(b)
	case IGe:
		return int64(a) >= int64(b)
	case ULt:
		return a < b
	case ULe:
		return a <= b
	case UGt:
		return a > b
	}
	return a >= b
}
