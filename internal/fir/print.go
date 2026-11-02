package fir

import (
	"fmt"
	"strings"
)

var opNames = [...]string{
	opNone: "?", OpArg: "arg", OpConst: "const", OpCopy: "set",
	OpFAdd: "fadd", OpFSub: "fsub", OpFMul: "fmul", OpFDiv: "fdiv", OpFMod: "fmod", OpFSqrt: "fsqrt", OpFAbs: "fabs", OpFNeg: "fneg",
	OpFMA: "fma", OpMAdd: "madd", OpRoundEven: "roundeven", OpFloor: "floor", OpCeil: "ceil", OpTrunc: "trunc",
	OpFToI: "ftoi", OpIToF: "itof", OpAdd: "add", OpSub: "sub", OpMul: "mul", OpAnd: "and", OpOr: "or", OpXor: "xor",
	OpShl: "shl", OpShr: "shr", OpSar: "sar", OpLoadF: "loadf", OpLoadI: "loadi",
	OpIf: "if", OpExit: "exit", OpReturn: "return",
}

var cmpNames = [...]string{"f==", "f!=", "f<", "f<=", "f>", "f>=", "==", "!=", "<", "<=", ">", ">=", "u<", "u<=", "u>", "u>="}

// String lists f's steps, one per line.
func (f *Func) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s(%d) fused=%v\n", f.Name, f.NArgs, f.Fused)
	f.print(&b, f.Body, 1)
	return b.String()
}

func (f *Func) print(b *strings.Builder, steps []Step, depth int) {
	ind := strings.Repeat("  ", depth)
	for _, st := range steps {
		switch st.Op {
		case OpIf:
			fmt.Fprintf(b, "%sif v%d %s v%d {\n", ind, st.A, cmpNames[st.Cmp], st.B)
			f.print(b, st.Then, depth+1)
			if st.Else != nil {
				fmt.Fprintf(b, "%s} else {\n", ind)
				f.print(b, st.Else, depth+1)
			}
			fmt.Fprintf(b, "%s}\n", ind)
		case OpExit:
			fmt.Fprintf(b, "%sexit\n", ind)
		case OpReturn:
			fmt.Fprintf(b, "%sreturn v%d\n", ind, st.A)
		case OpConst, OpArg, OpShl, OpShr, OpSar:
			fmt.Fprintf(b, "%sv%d = %s v%d %#x\n", ind, st.Dst, opNames[st.Op], st.A, st.Imm)
		default:
			fmt.Fprintf(b, "%sv%d = %s v%d v%d v%d\n", ind, st.Dst, opNames[st.Op], st.A, st.B, st.C)
		}
	}
}
