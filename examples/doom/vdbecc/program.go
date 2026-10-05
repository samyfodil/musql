package vdbecc

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/samyfodil/musql/engine"
)

// image lays out the globals and returns the initial address space. A function
// pointer is the function's index plus one: an integer the dispatcher
// branches on, which also keeps C's pointer equality.
func (g *gen) image() ([]byte, error) {
	g.addr = map[string]int64{}
	sizes := map[string]int64{}
	next := int64(GlobalsBase)
	for _, v := range g.mod.Vars {
		next = roundUp(next, max(v.Align, 1))
		g.addr[v.Name] = next
		sizes[v.Name] = int64(len(v.Init))
		next += int64(len(v.Init))
	}
	if roundUp(next, 8) >= int64(g.opts.Memory) {
		return nil, fmt.Errorf("vdbecc: %d bytes of memory cannot hold %d bytes of globals", g.opts.Memory, next)
	}
	for i, f := range g.mod.Funcs {
		g.ids[f.Name] = int64(i) + 1
	}
	ram := make([]byte, g.opts.Memory)
	for _, v := range g.mod.Vars {
		copy(ram[g.addr[v.Name]:], v.Init)
		for _, fx := range v.Fixups {
			p, err := g.constant(fx.Ref)
			if err != nil {
				return nil, fmt.Errorf("@%s: %w", v.Name, err)
			}
			binary.LittleEndian.PutUint64(ram[g.addr[v.Name]+fx.At:], uint64(p))
		}
	}
	for name, b := range g.opts.Preload {
		at, ok := g.addr[name]
		if !ok {
			return nil, fmt.Errorf("vdbecc: no global @%s to preload", name)
		}
		if int64(len(b)) > sizes[name] {
			return nil, fmt.Errorf("vdbecc: %d bytes do not fit @%s's %d", len(b), name, sizes[name])
		}
		copy(ram[at:], b)
	}
	if g.opts.Frame != "" {
		at, ok := g.addr[g.opts.Frame]
		if !ok {
			return nil, fmt.Errorf("vdbecc: no global @%s to present", g.opts.Frame)
		}
		g.frameAddr, g.frameLen = at, sizes[g.opts.Frame]
	}
	return ram, nil
}

// constant is a link-time value: a global's address, possibly offset, or a
// function's id.
func (g *gen) constant(v Value) (int64, error) {
	switch v := v.(type) {
	case GlobalRef:
		if a, ok := g.addr[v.Name]; ok {
			return a, nil
		}
		if id, ok := g.ids[v.Name]; ok {
			return id, nil
		}
		return 0, fmt.Errorf("vdbecc: @%s is not defined", v.Name)
	case GlobalPlus:
		a, ok := g.addr[v.Name]
		if !ok {
			return 0, fmt.Errorf("vdbecc: @%s is not a variable", v.Name)
		}
		off, err := g.env.constOffset(v.Base, v.Indices)
		return a + off, err
	}
	return 0, fmt.Errorf("vdbecc: %v is not a link-time constant", v)
}

// prelude allocates the fixed registers and opens the program.
func (g *gen) prelude() {
	g.sink = g.reg()
	g.w0, g.w1, g.w2 = g.reg(), g.reg(), g.reg()
	g.sp, g.link, g.ret = g.reg(), g.reg(), g.reg()
	for i := range g.args {
		g.args[i] = g.reg()
	}
	g.callee, g.t1, g.t2, g.t3 = g.reg(), g.reg(), g.reg(), g.reg()
	g.dispatch, g.exit, g.halt = g.label(), g.label(), g.label()
	g.pool, g.poolDone = g.label(), g.label()

	g.set(g.sp, int64(g.opts.Memory))
	for _, r := range append([]int{g.ret, g.link, g.callee}, g.args[:]...) {
		g.set(r, 0)
	}
	g.goTo(g.pool)
	g.place(g.poolDone)
}

// main calls the entry function and returns its result as the last row.
func (g *gen) main() error {
	p, ok := g.plans[g.opts.Entry]
	if !ok {
		return fmt.Errorf("vdbecc: no entry function @%s", g.opts.Entry)
	}
	if len(g.opts.Args) > len(g.args) {
		return fmt.Errorf("vdbecc: %d entry arguments, at most %d", len(g.opts.Args), len(g.args))
	}
	for i, v := range g.opts.Args {
		g.set(g.args[i], v)
	}
	g.out = g.reg()
	g.reg() // out+1: the frame
	g.jump(engine.OpGosub, g.link, 0, p.entry)
	g.place(g.exit)
	g.mov(g.ret, g.out)
	g.emitRow()
	g.goTo(g.halt)
	return nil
}

// emitRow emits a result row: the return register's copy in out, and the
// frame beside it.
func (g *gen) emitRow() {
	if g.opts.Frame == "" {
		g.op(engine.OpResultRow, g.out, 1, 0, nil)
		return
	}
	g.set(g.w0, g.frameAddr)
	g.set(g.w1, g.frameLen)
	g.fn(g.mem.readBlob, g.w0, 2, g.out+1)
	g.op(engine.OpResultRow, g.out, 2, 0, nil)
}

// dispatcher is where an indirect call lands: a binary search over function
// ids that ends in a jump to the callee's entry. The caller's Gosub already
// recorded the return address.
func (g *gen) dispatcher() {
	type target struct {
		id int64
		at label
	}
	var ts []target
	for _, f := range g.mod.Funcs {
		ts = append(ts, target{g.ids[f.Name], g.plans[f.Name].entry})
	}
	slices.SortFunc(ts, func(a, b target) int { return int(a.id - b.id) })
	bad := g.label()
	var search func([]target)
	search = func(ts []target) {
		if len(ts) <= 1 {
			if len(ts) == 1 {
				g.when(PredEQ, g.callee, g.k(ts[0].id), ts[0].at)
			}
			g.goTo(bad)
			return
		}
		mid, upper := len(ts)/2, g.label()
		g.when(PredGE, g.callee, g.k(ts[len(ts)/2].id), upper)
		search(ts[:mid])
		g.place(upper)
		search(ts[mid:])
	}
	g.place(g.dispatch)
	search(ts)
	g.place(bad)
	g.fail("vdbecc: indirect call through an invalid function pointer")
}

// Run runs the program to the end and returns the entry function's result,
// the last row's first column.
func (c *Compiled) Run() (int64, error) {
	st, err := engine.NewProgramStmt(c.Program)
	if err != nil {
		return 0, err
	}
	var last []engine.Value
	for {
		row, err := st.Step()
		if err != nil {
			return 0, err
		}
		if row == nil {
			break
		}
		last = row
	}
	if len(last) == 0 || last[0].Typ != engine.Int {
		return 0, fmt.Errorf("vdbecc: the program returned no integer")
	}
	return last[0].I, nil
}
