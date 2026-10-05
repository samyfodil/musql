package engine

import (
	"errors"
	"fmt"
)

// P5 flags for comparison opcodes: store result, jump on NULL, or NULL equality.
const (
	P5StoreP2    = p5StoreP2
	P5JumpIfNull = p5JumpIfNull
	P5NullEq     = p5NullEq
)

// ProgramStmt runs caller-built bytecode on the VDBE, yielding rows via OpResultRow.
// steps, so a program can run as long as its caller keeps stepping and take
// fresh input each time.
//
// Such a program computes: registers, arithmetic, comparisons and jumps,
// subroutines, bound parameters, functions -- built-in by name, or a
// *ScalarFunction the caller holds -- and halts, with or without an error. It
// opens no cursor and touches no table.
type ProgramStmt struct {
	prog *Program
	m    *vdbe
}

// NewProgramStmt checks prog and readies it to run. A program the VDBE could
// trip over -- an opcode outside that set, a register or a jump target outside
// the program, a P4 of the wrong type -- is an error here, never a crash later.
func NewProgramStmt(prog *Program) (*ProgramStmt, error) {
	if prog == nil || prog.NReg < 0 {
		return nil, errors.New("engine: no program")
	}
	for pc := range prog.Insns {
		if err := checkProgramInsn(prog, pc); err != nil {
			return nil, err
		}
	}
	return &ProgramStmt{prog: prog, m: &vdbe{regs: make([]Value, prog.NReg), yield: true, jx: prog.jitCode()}}, nil
}

// Bind sets ?i (1-based) for the steps after it.
func (s *ProgramStmt) Bind(i int, v Value) {
	if i < 1 {
		return
	}
	for len(s.m.params) < i {
		s.m.params = append(s.m.params, Value{Typ: Null})
	}
	s.m.params[i-1] = v
}

// Step runs the program to its next result row and returns it, or nil once
// the program halts or runs off its end.
func (s *ProgramStmt) Step() ([]Value, error) {
	if s.m.done {
		return nil, nil
	}
	rows, err := s.m.run(s.prog.Insns)
	if err != nil {
		s.m.done = true
		return nil, err
	}
	if len(rows) == 0 {
		s.m.done = true
		return nil, nil
	}
	return rows[0], nil
}

// checkProgramInsn validates the instruction at pc of a caller-built program.
func checkProgramInsn(prog *Program, pc int) error {
	in := prog.Insns[pc]
	bad := func(what string) error {
		return fmt.Errorf("engine: program instruction %d (%s): %s", pc, in.Op, what)
	}
	reg := func(r int) bool { return r >= 0 && r < prog.NReg }
	span := func(r, n int) bool { return n >= 0 && r >= 0 && r+n <= prog.NReg }
	jump := func(t int) bool { return t >= 0 && t < len(prog.Insns) }
	ok := true
	switch in.Op {
	case OpInit, OpGoto:
		ok = jump(in.P2)
	case OpHalt:
	case OpHaltError:
		_, isT := in.P4.(string)
		ok = in.P1 == 0 && isT
	case OpResultRow:
		ok = span(in.P1, in.P2)
	case OpIf, OpIfNot, OpIsNull, OpNotNull, OpMustBeInt:
		ok = reg(in.P1) && jump(in.P2)
	case OpInteger, OpNull:
		ok = reg(in.P2)
	case OpInt64:
		_, isT := in.P4.(int64)
		ok = reg(in.P2) && isT
	case OpReal:
		_, isT := in.P4.(float64)
		ok = reg(in.P2) && isT
	case OpString8:
		_, isT := in.P4.(string)
		ok = reg(in.P2) && isT
	case OpBlob:
		_, isT := in.P4.([]byte)
		ok = reg(in.P2) && isT
	case OpVariable:
		ok = in.P1 >= 1 && reg(in.P2)
	case OpCopy, OpSCopy, OpNot, OpNegative, OpBitNot:
		ok = reg(in.P1) && reg(in.P2)
	case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder, OpConcat,
		OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		ok = reg(in.P1) && reg(in.P2) && reg(in.P3)
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		target := jump(in.P2)
		if in.P5&p5StoreP2 != 0 {
			target = reg(in.P2)
		}
		ok = reg(in.P1) && reg(in.P3) && target && in.P4 == nil &&
			in.P5&^(p5AffMask|p5StoreP2|p5JumpIfNull|p5NullEq) == 0
	case OpRealAffinity:
		ok = reg(in.P1)
	case OpCast:
		_, isT := in.P4.(string)
		ok = reg(in.P1) && isT
	case OpFunction:
		switch f := in.P4.(type) {
		case *ScalarFunction:
			ok = f != nil && f.Fn != nil
		case string:
			ok = supportedFuncs[f] && f != "rtreecheck" // rtreecheck reads a database
		default:
			ok = false
		}
		ok = ok && span(in.P1, in.P2) && reg(in.P3)
	case OpGosub:
		ok = reg(in.P1) && jump(in.P2)
	case OpReturn:
		ok = reg(in.P1)
	default:
		return bad("not an opcode a caller-built program may use")
	}
	if !ok {
		return bad("an operand is out of range or of the wrong type")
	}
	return nil
}
