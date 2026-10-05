package vdbecc

import (
	"fmt"

	"github.com/samyfodil/musql/engine"
)

// GlobalsBase is the address of the first global. Address 0 stays unmapped
// from C's point of view, so a null dereference reads zeros rather than data.
const GlobalsBase = 0x1000

// Options configures Compile.
type Options struct {
	Memory int     // bytes of address space; the stack starts at the top
	Entry  string  // the function the program runs
	Args   []int64 // its arguments
	// Frame, when set, names a global the program returns alongside every
	// result row, as a BLOB: vdbe_present() emits one such row per call.
	Frame string
	// Preload overwrites the start of globals' initial contents.
	Preload map[string][]byte
}

// Compiled is a program ready for engine.NewProgramStmt.
type Compiled struct {
	Program *engine.Program
}

// gen is the code generator's state across a whole module.
type gen struct {
	*asm
	mod  *Module
	env  typeEnv
	mem  *memFuncs
	opts Options

	addr  map[string]int64 // global variable -> address
	ids   map[string]int64 // function -> its pointer value, a small integer
	plans map[string]*plan

	// Fixed registers. w0..w2 are adjacent: the argument window of a memory
	// function.
	sink, w0, w1, w2 int
	sp, link, ret    int
	args             [16]int
	callee           int // an indirect call's target id
	t1, t2, t3       int // scratch, never live across an instruction

	dispatch, exit, halt label
	pool, poolDone       label
	frameAddr            int64
	frameLen             int64
	out                  int // the result row: out, out+1

	p   *plan // the function being emitted
	err error
}

func (g *gen) failf(format string, a ...any) {
	if g.err == nil {
		g.err = fmt.Errorf("vdbecc: "+format, a...)
	}
}

// Compile turns a linked module into a program.
func Compile(m *Module, opts Options) (*Compiled, error) {
	g := &gen{asm: newAsm(), mod: m, env: typeEnv(m.Types), opts: opts, ids: map[string]int64{}, plans: map[string]*plan{}}
	ram, err := g.image()
	if err != nil {
		return nil, err
	}
	g.mem = &newMemory(ram).fns
	g.prelude()

	reent := reentrant(m)
	firstOwned := g.nreg
	for _, f := range m.Funcs {
		p := g.newPlan(f)
		if reent[f.Name] {
			p.choosePreserved()
		}
		g.plans[f.Name] = p
	}
	for r := firstOwned; r < g.nreg; r++ {
		g.set(r, 0) // the stack saves read them before some are written
	}
	if err := g.main(); err != nil {
		return nil, err
	}
	for _, f := range m.Funcs {
		g.function(g.plans[f.Name])
		if g.err != nil {
			return nil, fmt.Errorf("@%s: %w", f.Name, g.err)
		}
	}
	g.dispatcher()

	// The constant pool is complete only now. Its loads run once, first: the
	// program opens with a jump here and comes back.
	g.place(g.pool)
	g.loadPool()
	g.goTo(g.poolDone)

	g.place(g.halt)
	g.op(engine.OpHalt, 0, 0, 0, nil)
	if err := g.resolve(); err != nil {
		return nil, err
	}
	cols := 1
	if opts.Frame != "" {
		cols = 2
	}
	return &Compiled{Program: &engine.Program{Insns: g.code, NReg: g.nreg, NResultCol: cols}}, nil
}
