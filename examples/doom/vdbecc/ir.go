// Package vdbecc compiles LLVM IR, as clang emits it for C, into musql VDBE
// bytecode that an engine.ProgramStmt runs. The idea of running C on a
// database's bytecode VM comes from Turso's Doom demo
// (github.com/tursodatabase/turso-vdbe-doom-example).
//
// The pipeline is ParseModule (text to IR), LinkModules (several translation
// units to one) and Compile (IR to a program).
package vdbecc

import (
	"fmt"
	"strings"
)

// ---- types ----

// Type is an LLVM type. Layout questions go through a typeEnv, which resolves
// named types.
type Type interface{ String() string }

type (
	VoidType  struct{}
	IntType   struct{ Bits int }
	FloatType struct{ Double bool }
	PtrType   struct{}
	ArrayType struct {
		Len  int64
		Elem Type
	}
	StructType struct {
		Fields []Type
		Packed bool
	}
	NamedType struct{ Name string }
)

func (VoidType) String() string    { return "void" }
func (t IntType) String() string   { return fmt.Sprintf("i%d", t.Bits) }
func (PtrType) String() string     { return "ptr" }
func (t NamedType) String() string { return "%" + t.Name }
func (t ArrayType) String() string { return fmt.Sprintf("[%d x %s]", t.Len, t.Elem) }
func (t FloatType) String() string {
	if t.Double {
		return "double"
	}
	return "float"
}
func (t StructType) String() string {
	parts := make([]string, len(t.Fields))
	for i, f := range t.Fields {
		parts[i] = f.String()
	}
	if t.Packed {
		return "<{" + strings.Join(parts, ", ") + "}>"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func isFloatType(t Type) bool { _, ok := t.(FloatType); return ok }

// ---- values ----

// Value is an instruction operand.
type Value interface{ isValue() }

type (
	// Local is an SSA name: a parameter or an instruction's result.
	Local struct{ Name string }
	// GlobalRef is the address of a global variable, or a function's identity.
	GlobalRef struct{ Name string }
	// IntConst is an integer constant (true is 1).
	IntConst struct{ V int64 }
	// FloatConst is a floating-point constant.
	FloatConst struct{ V float64 }
	// GlobalPlus is a constant getelementptr: a global's address plus the
	// offset Indices select within Base.
	GlobalPlus struct {
		Name    string
		Base    Type
		Indices []int64
	}
	// Zero is null, zeroinitializer, undef or poison: all read as 0 here.
	Zero struct{}
)

func (Local) isValue()      {}
func (GlobalRef) isValue()  {}
func (IntConst) isValue()   {}
func (FloatConst) isValue() {}
func (GlobalPlus) isValue() {}
func (Zero) isValue()       {}

// localName is v's SSA name when v is a Local.
func localName(v Value) (string, bool) {
	l, ok := v.(Local)
	return l.Name, ok
}

// ---- instructions ----

// Pred is a comparison predicate.
type Pred int

const (
	PredEQ Pred = iota
	PredNE
	PredLT // signed, or ordered/unordered for floats
	PredLE
	PredGT
	PredGE
	PredULT
	PredULE
	PredUGT
	PredUGE
)

func (p Pred) unsigned() bool { return p >= PredULT }

// signed is p's signed counterpart: how it compares once operands are
// zero-extended into 64 bits.
func (p Pred) signed() Pred {
	if p.unsigned() {
		return p - PredULT + PredLT
	}
	return p
}

// Arith is an integer or floating-point binary operator.
type Arith int

const (
	ArAdd Arith = iota
	ArSub
	ArMul
	ArSDiv
	ArUDiv
	ArSRem
	ArURem
	ArAnd
	ArOr
	ArXor
	ArShl
	ArAShr
	ArLShr
	ArFDiv
)

// Conv is a conversion.
type Conv int

const (
	ConvSExt Conv = iota
	ConvZExt
	ConvTrunc
	ConvSame      // bitcast, ptrtoint, inttoptr, freeze: the bits do not change
	ConvIntFloat  // sitofp, uitofp
	ConvFloatInt  // fptosi, fptoui: toward zero
	ConvFloatSize // fpext, fptrunc: floats are held as f64 throughout
)

// Instr is a non-terminator instruction.
type Instr interface {
	// Def is the SSA name the instruction defines, or "".
	Def() string
	// Operands visits every value the instruction reads.
	Operands(func(*Value))
}

// TypedValue is a value with its type: a call argument or a GEP index.
type TypedValue struct {
	Type Type
	Val  Value
}

type (
	BinInstr struct {
		Dst  string
		Op   Arith
		Type Type
		X, Y Value
	}
	CmpInstr struct {
		Dst   string
		Pred  Pred
		Type  Type // the operands' type
		X, Y  Value
		Float bool
	}
	ConvInstr struct {
		Dst      string
		Kind     Conv
		From, To Type
		X        Value
	}
	SelectInstr struct {
		Dst        string
		Cond, T, F Value
	}
	PhiInstr struct {
		Dst string
		In  []PhiEdge
	}
	AllocaInstr struct {
		Dst   string
		Type  Type
		Count int64
	}
	LoadInstr struct {
		Dst  string
		Type Type
		Addr Value
	}
	StoreInstr struct {
		Type      Type
		Val, Addr Value
	}
	GEPInstr struct {
		Dst     string
		Base    Type
		Addr    Value
		Indices []TypedValue
	}
	CallInstr struct {
		Dst    string // "" when the result is unused or void
		Callee string // a direct call; "" for an indirect one
		Target Value  // an indirect call's function pointer
		Args   []TypedValue
	}
	VaArgInstr struct {
		Dst  string
		Type Type
		List Value
	}
	NegInstr struct {
		Dst string
		X   Value
	}
)

// PhiEdge is a phi's value when control arrives from From.
type PhiEdge struct {
	Val  Value
	From string
}

func (i *BinInstr) Def() string    { return i.Dst }
func (i *CmpInstr) Def() string    { return i.Dst }
func (i *ConvInstr) Def() string   { return i.Dst }
func (i *SelectInstr) Def() string { return i.Dst }
func (i *PhiInstr) Def() string    { return i.Dst }
func (i *AllocaInstr) Def() string { return i.Dst }
func (i *LoadInstr) Def() string   { return i.Dst }
func (i *StoreInstr) Def() string  { return "" }
func (i *GEPInstr) Def() string    { return i.Dst }
func (i *CallInstr) Def() string   { return i.Dst }
func (i *VaArgInstr) Def() string  { return i.Dst }
func (i *NegInstr) Def() string    { return i.Dst }

func (i *BinInstr) Operands(f func(*Value))    { f(&i.X); f(&i.Y) }
func (i *CmpInstr) Operands(f func(*Value))    { f(&i.X); f(&i.Y) }
func (i *ConvInstr) Operands(f func(*Value))   { f(&i.X) }
func (i *SelectInstr) Operands(f func(*Value)) { f(&i.Cond); f(&i.T); f(&i.F) }
func (i *AllocaInstr) Operands(func(*Value))   {}
func (i *LoadInstr) Operands(f func(*Value))   { f(&i.Addr) }
func (i *StoreInstr) Operands(f func(*Value))  { f(&i.Val); f(&i.Addr) }
func (i *VaArgInstr) Operands(f func(*Value))  { f(&i.List) }
func (i *NegInstr) Operands(f func(*Value))    { f(&i.X) }
func (i *PhiInstr) Operands(f func(*Value)) {
	for k := range i.In {
		f(&i.In[k].Val)
	}
}
func (i *GEPInstr) Operands(f func(*Value)) {
	f(&i.Addr)
	for k := range i.Indices {
		f(&i.Indices[k].Val)
	}
}
func (i *CallInstr) Operands(f func(*Value)) {
	if i.Callee == "" {
		f(&i.Target)
	}
	for k := range i.Args {
		f(&i.Args[k].Val)
	}
}

// ---- terminators ----

// Term ends a block.
type Term interface {
	// Succs visits every successor label.
	Succs(func(string))
	// Operands visits every value the terminator reads.
	Operands(func(*Value))
}

type (
	Jump   struct{ To string }
	Branch struct {
		Cond      Value
		Then, Els string
	}
	Switch struct {
		X       Value
		Default string
		Arms    []SwitchArm
	}
	Return struct {
		Val Value // nil for "ret void"
	}
	Trap struct{} // unreachable
)

// SwitchArm is one case of a switch.
type SwitchArm struct {
	On int64
	To string
}

func (t *Jump) Succs(f func(string))   { f(t.To) }
func (t *Branch) Succs(f func(string)) { f(t.Then); f(t.Els) }
func (t *Switch) Succs(f func(string)) {
	f(t.Default)
	for _, a := range t.Arms {
		f(a.To)
	}
}
func (t *Return) Succs(func(string)) {}
func (t *Trap) Succs(func(string))   {}

func (t *Jump) Operands(func(*Value))     {}
func (t *Branch) Operands(f func(*Value)) { f(&t.Cond) }
func (t *Switch) Operands(f func(*Value)) { f(&t.X) }
func (t *Trap) Operands(func(*Value))     {}
func (t *Return) Operands(f func(*Value)) {
	if t.Val != nil {
		f(&t.Val)
	}
}

// ---- module ----

// Block is a basic block: straight-line instructions and one terminator.
type Block struct {
	Name string
	Body []Instr
	End  Term
}

// Param is a function parameter.
type Param struct {
	Name string
	Type Type
}

// Func is a function definition.
type Func struct {
	Name     string
	Params   []Param
	Result   Type
	Blocks   []*Block
	Local    bool // internal or private linkage
	Variadic bool
}

// Fixup is a pointer slot in a global's initial bytes, filled with an address
// or a function identity once the program is laid out.
type Fixup struct {
	At  int64
	Ref Value // GlobalRef or GlobalPlus
}

// Var is a global variable with its initializer flattened to bytes.
type Var struct {
	Name   string
	Init   []byte
	Align  int64
	Fixups []Fixup
	Local  bool
}

// Module is one translation unit, or several linked into one.
type Module struct {
	Funcs    []*Func
	Vars     []*Var
	Types    map[string]Type
	External []string // declared but not defined here
}

// Func finds a defined function.
func (m *Module) Func(name string) *Func {
	for _, f := range m.Funcs {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// ---- layout ----

// typeEnv answers size and alignment questions, resolving named types.
type typeEnv map[string]Type

func (env typeEnv) resolve(t Type) Type {
	for {
		n, ok := t.(NamedType)
		if !ok {
			return t
		}
		t = env[n.Name]
	}
}

func roundUp(n, a int64) int64 { return (n + a - 1) / a * a }

func (env typeEnv) size(t Type) int64 {
	switch t := env.resolve(t).(type) {
	case IntType:
		return int64(t.Bits+7) / 8
	case FloatType:
		if t.Double {
			return 8
		}
		return 4
	case PtrType:
		return 8
	case ArrayType:
		return t.Len * env.size(t.Elem)
	case StructType:
		end, _ := env.fieldOffsets(t)
		return roundUp(end, env.align(t))
	}
	return 0
}

func (env typeEnv) align(t Type) int64 {
	switch t := env.resolve(t).(type) {
	case IntType:
		return min(int64(t.Bits+7)/8, 8)
	case FloatType:
		return env.size(t)
	case PtrType:
		return 8
	case ArrayType:
		return env.align(t.Elem)
	case StructType:
		if t.Packed {
			return 1
		}
		a := int64(1)
		for _, f := range t.Fields {
			a = max(a, env.align(f))
		}
		return a
	}
	return 1
}

// fieldOffsets is every field's offset in s, and where the last one ends.
func (env typeEnv) fieldOffsets(s StructType) (end int64, offs []int64) {
	for _, f := range s.Fields {
		if !s.Packed {
			end = roundUp(end, env.align(f))
		}
		offs = append(offs, end)
		end += env.size(f)
	}
	return end, offs
}

// step is the type one GEP index selects inside t, and the offset it adds per
// unit of index (for an array) or in total (for a struct field i).
func (env typeEnv) step(t Type, i int64) (elem Type, stride, fixed int64, err error) {
	switch r := env.resolve(t).(type) {
	case ArrayType:
		return r.Elem, env.size(r.Elem), 0, nil
	case StructType:
		if i < 0 || int(i) >= len(r.Fields) {
			return nil, 0, 0, fmt.Errorf("field %d of %s", i, t)
		}
		_, offs := env.fieldOffsets(r)
		return r.Fields[i], 0, offs[i], nil
	}
	return nil, 0, 0, fmt.Errorf("cannot index into %s", t)
}

// constOffset folds an all-constant GEP into bytes.
func (env typeEnv) constOffset(base Type, idx []int64) (int64, error) {
	if len(idx) == 0 {
		return 0, nil
	}
	off := idx[0] * env.size(base)
	cur := base
	for _, i := range idx[1:] {
		elem, stride, fixed, err := env.step(cur, i)
		if err != nil {
			return 0, err
		}
		off += fixed + stride*i
		cur = elem
	}
	return off, nil
}
