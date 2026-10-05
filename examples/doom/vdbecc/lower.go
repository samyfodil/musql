package vdbecc

import (
	"maps"
	"slices"

	"github.com/samyfodil/musql/engine"
)

// Lowering: one IR function to VDBE code. Calling convention: the caller puts
// arguments in args and Gosubs the callee's entry with the return address in
// link; the result comes back in ret.

// r is the register holding v.
func (g *gen) r(v Value) int {
	switch v := v.(type) {
	case Local:
		if r, ok := g.p.reg[v.Name]; ok {
			return r
		}
		g.failf("%%%s is used but never defined", v.Name)
	case IntConst:
		return g.k(v.V)
	case FloatConst:
		return g.kf(v.V)
	case Zero:
		return g.k(0)
	default:
		c, err := g.constant(v)
		if err != nil {
			g.failf("%v", err)
		}
		return g.k(c)
	}
	return g.k(0)
}

func (g *gen) dst(name string) int { return g.p.reg[name] }

func intBits(t Type) int {
	if i, ok := t.(IntType); ok {
		return i.Bits
	}
	return 64
}

// width is a memory access's size in bytes.
func (g *gen) width(t Type) int {
	n := g.env.size(t)
	if !accessWidth(n) {
		g.failf("a %d-byte access (%s)", n, t)
		return 8
	}
	return int(n)
}

func (g *gen) store(addr, width, val int) {
	g.mov(addr, g.w0)
	g.mov(val, g.w1)
	g.fn(g.mem.store[width], g.w0, 2, g.sink)
}

func (g *gen) load(addr, width int, signed bool, d int) {
	s := 0
	if signed {
		s = 1
	}
	g.fn(g.mem.load[width][s], addr, 1, d)
}

func (g *gen) function(p *plan) {
	g.p = p
	f := p.fn
	if len(f.Params) > len(g.args) {
		g.failf("%d parameters, at most %d", len(f.Params), len(g.args))
		return
	}
	// Prologue.
	g.place(p.entry)
	frame := p.frameBytes()
	if frame > 0 {
		g.do(engine.OpSubtract, g.sp, g.k(frame), g.sp)
	}
	for i, r := range p.preserve {
		g.plus(g.sp, int64(i)*8, g.t1)
		g.store(g.t1, 8, r)
	}
	g.mov(g.link, p.retAddr)
	for i, prm := range f.Params {
		g.mov(g.args[i], p.reg[prm.Name])
	}
	locals := int64(len(p.preserve)) * 8
	if p.vaSpill >= 0 {
		for i, a := range g.args {
			g.plus(g.sp, locals+p.vaSpill+int64(i)*8, g.t1)
			g.store(g.t1, 8, a)
		}
	}
	for _, a := range p.allocas {
		g.plus(g.sp, locals+p.slot[a.Dst], p.reg[a.Dst])
	}

	for i, b := range f.Blocks {
		g.place(p.labels[b.Name])
		body := b.Body
		if cmp, br := g.fusable(b); cmp != nil {
			body = body[:len(body)-1]
			for _, in := range body {
				g.instr(b, in)
			}
			g.fusedBranch(b, cmp, br)
			continue
		}
		for _, in := range body {
			g.instr(b, in)
		}
		next := ""
		if i+1 < len(f.Blocks) {
			next = f.Blocks[i+1].Name
		}
		g.term(b, next)
	}
}

// fusable finds a block ending in a compare that only its branch reads: the
// two become one conditional jump.
func (g *gen) fusable(b *Block) (*CmpInstr, *Branch) {
	br, ok := b.End.(*Branch)
	if !ok || len(b.Body) == 0 {
		return nil, nil
	}
	cmp, ok := b.Body[len(b.Body)-1].(*CmpInstr)
	if !ok {
		return nil, nil
	}
	if c, ok := localName(br.Cond); !ok || c != cmp.Dst || g.p.uses[c] != 1 {
		return nil, nil
	}
	return cmp, br
}

// operands is a compare's operand registers, zero-extended for an unsigned
// predicate on a narrow type.
func (g *gen) cmpOperands(c *CmpInstr) (int, int) {
	x, y := g.r(c.X), g.r(c.Y)
	if bits := intBits(c.Type); !c.Float && c.Pred.unsigned() && bits < 64 {
		g.zeroExtend(x, bits, g.t1)
		g.zeroExtend(y, bits, g.t2)
		return g.t1, g.t2
	}
	return x, y
}

func (g *gen) fusedBranch(b *Block, c *CmpInstr, br *Branch) {
	x, y := g.cmpOperands(c)
	taken := g.label()
	g.when(c.Pred, x, y, taken)
	g.edge(b.Name, br.Els)
	g.place(taken)
	g.edge(b.Name, br.Then)
}

// edge moves control from block from to block to, performing to's phis.
func (g *gen) edge(from, to string) {
	g.phis(from, to)
	g.goTo(g.p.labels[to])
}

// phis performs the phi assignments of the edge from -> to. They are one
// parallel copy: register moves are ordered so none overwrites a source still
// to be read, a cycle is broken through a fresh register, and constants are
// written last since nothing reads them.
func (g *gen) phis(from, to string) {
	target, ok := g.p.blocks[to]
	if !ok {
		g.failf("branch to unknown block %%%s", to)
		return
	}
	moves := map[int]int{} // dst -> src
	var consts [][2]int
	for _, in := range target.Body {
		phi, ok := in.(*PhiInstr)
		if !ok {
			break
		}
		var val Value
		for _, e := range phi.In {
			if e.From == from {
				val = e.Val
			}
		}
		if val == nil {
			g.failf("phi %%%s has no value for %%%s", phi.Dst, from)
			return
		}
		d := g.dst(phi.Dst)
		if _, isLocal := val.(Local); isLocal {
			if s := g.r(val); s != d {
				moves[d] = s
			}
		} else {
			consts = append(consts, [2]int{d, g.r(val)})
		}
	}
	for len(moves) > 0 {
		readers := map[int]int{}
		for _, s := range moves {
			readers[s]++
		}
		progressed := false
		for _, d := range slices.Sorted(maps.Keys(moves)) {
			if readers[d] == 0 {
				g.mov(moves[d], d)
				readers[moves[d]]--
				delete(moves, d)
				progressed = true
			}
		}
		if progressed {
			continue
		}
		d := slices.Sorted(maps.Keys(moves))[0] // every destination is still read: a cycle
		spare := g.reg()
		g.mov(d, spare)
		for k, s := range moves {
			if s == d {
				moves[k] = spare
			}
		}
	}
	for _, c := range consts {
		g.mov(c[1], c[0])
	}
}

func (g *gen) term(b *Block, next string) {
	switch t := b.End.(type) {
	case *Jump:
		g.phis(b.Name, t.To)
		if t.To != next {
			g.goTo(g.p.labels[t.To])
		}
	case *Branch:
		taken := g.label()
		g.ifTrue(g.r(t.Cond), taken)
		g.edge(b.Name, t.Els)
		g.place(taken)
		g.edge(b.Name, t.Then)
	case *Switch:
		x := g.r(t.X)
		arms := make([]label, len(t.Arms))
		for i, a := range t.Arms {
			arms[i] = g.label()
			g.when(PredEQ, x, g.k(a.On), arms[i])
		}
		g.edge(b.Name, t.Default)
		for i, a := range t.Arms {
			g.place(arms[i])
			g.edge(b.Name, a.To)
		}
	case *Return:
		if t.Val != nil {
			g.mov(g.r(t.Val), g.ret)
		}
		g.mov(g.p.retAddr, g.link) // before the restores overwrite retAddr
		for i, r := range g.p.preserve {
			g.plus(g.sp, int64(i)*8, g.t1)
			g.load(g.t1, 8, true, r)
		}
		if frame := g.p.frameBytes(); frame > 0 {
			g.do(engine.OpAdd, g.sp, g.k(frame), g.sp)
		}
		g.op(engine.OpReturn, g.link, 0, 0, nil)
	case *Trap:
		g.fail("vdbecc: unreachable code reached in @" + g.p.fn.Name)
	default:
		g.failf("block %%%s has no terminator", b.Name)
	}
}

var intOps = map[Arith]engine.OpCode{
	ArAdd: engine.OpAdd, ArSub: engine.OpSubtract, ArMul: engine.OpMultiply,
	ArSDiv: engine.OpDivide, ArSRem: engine.OpRemainder, ArShl: engine.OpShiftLeft,
	ArAnd: engine.OpBitAnd, ArOr: engine.OpBitOr, ArAShr: engine.OpShiftRight,
}

var floatOps = map[Arith]engine.OpCode{
	ArAdd: engine.OpAdd, ArSub: engine.OpSubtract, ArMul: engine.OpMultiply, ArFDiv: engine.OpDivide,
}

func (g *gen) instr(b *Block, in Instr) {
	if _, ok := g.p.aliases(in); ok {
		return
	}
	switch in := in.(type) {
	case *AllocaInstr, *PhiInstr:
		// The prologue sets allocas; edges set phis.
	case *BinInstr:
		g.binary(in)
	case *NegInstr:
		g.op(engine.OpNegative, g.r(in.X), g.dst(in.Dst), 0, nil)
	case *CmpInstr:
		x, y := g.cmpOperands(in)
		d, done := g.dst(in.Dst), g.label()
		g.set(d, 1)
		g.when(in.Pred, x, y, done)
		g.set(d, 0)
		g.place(done)
	case *ConvInstr:
		g.convert(in)
	case *SelectInstr:
		d, onTrue, done := g.dst(in.Dst), g.label(), g.label()
		g.ifTrue(g.r(in.Cond), onTrue)
		g.mov(g.r(in.F), d)
		g.goTo(done)
		g.place(onTrue)
		g.mov(g.r(in.T), d)
		g.place(done)
	case *LoadInstr:
		addr := g.r(in.Addr)
		switch {
		case g.p.passThrough[in.Dst]:
			g.mov(addr, g.w1) // the store reads from here
		case isFloatType(g.env.resolve(in.Type)):
			f := g.mem.loadF64
			if !g.env.resolve(in.Type).(FloatType).Double {
				f = g.mem.loadF32
			}
			g.fn(f, addr, 1, g.dst(in.Dst))
		default:
			g.load(addr, g.width(in.Type), !g.p.zeroExtLoad[in.Dst], g.dst(in.Dst))
		}
	case *StoreInstr:
		g.storeInstr(in)
	case *GEPInstr:
		g.gep(in)
	case *VaArgInstr:
		// A va_list is a pointer to the next 8-byte argument slot.
		g.mov(g.r(in.List), g.t1)
		g.load(g.t1, 8, true, g.t2)
		d := g.dst(in.Dst)
		g.load(g.t2, 8, true, d)
		g.signExtend(d, intBits(in.Type))
		g.do(engine.OpAdd, g.t2, g.k(8), g.t2)
		g.store(g.t1, 8, g.t2)
	case *CallInstr:
		g.call(in)
	default:
		g.failf("cannot lower %T", in)
	}
}

func (g *gen) binary(in *BinInstr) {
	x, y, d := g.r(in.X), g.r(in.Y), g.dst(in.Dst)
	if isFloatType(g.env.resolve(in.Type)) {
		op, ok := floatOps[in.Op]
		if !ok {
			g.failf("float operator %d", in.Op)
		}
		g.do(op, x, y, d)
		return
	}
	bits := intBits(in.Type)
	switch in.Op {
	case ArXor:
		// x^y == (x|y) - (x&y), exact on any bit pattern.
		g.do(engine.OpBitOr, x, y, g.t3)
		g.do(engine.OpBitAnd, x, y, d)
		g.do(engine.OpSubtract, g.t3, d, d)
	case ArUDiv, ArURem:
		op := engine.OpDivide
		if in.Op == ArURem {
			op = engine.OpRemainder
		}
		if bits >= 64 {
			g.do(op, x, y, d) // addresses and sizes here stay below 2^63
			return
		}
		g.zeroExtend(x, bits, g.t1)
		g.zeroExtend(y, bits, g.t2)
		g.do(op, g.t1, g.t2, d)
		g.signExtend(d, bits)
	case ArLShr:
		if bits < 64 {
			g.zeroExtend(x, bits, g.t3)
			g.do(engine.OpShiftRight, g.t3, y, d)
			return
		}
		// 64-bit: shift arithmetically, then clear the copied sign bits. Needs
		// the amount at compile time.
		k, ok := in.Y.(IntConst)
		if !ok {
			g.failf("a 64-bit lshr by a variable amount")
			return
		}
		if k.V == 0 {
			g.mov(x, d)
			return
		}
		g.do(engine.OpShiftRight, x, g.k(k.V), d)
		g.do(engine.OpBitAnd, d, g.k(int64(^uint64(0)>>k.V)), d)
	default:
		g.do(intOps[in.Op], x, y, d)
		switch in.Op {
		case ArAnd, ArOr, ArAShr:
			// Bitwise results of canonical operands are canonical.
		default:
			// Wrap like C does even with nsw: real code overflows signed math.
			g.signExtend(d, bits)
		}
	}
}

func (g *gen) convert(c *ConvInstr) {
	x, d := g.r(c.X), g.dst(c.Dst)
	switch c.Kind {
	case ConvSame, ConvSExt, ConvFloatSize:
		g.mov(x, d)
	case ConvZExt:
		if n, ok := localName(c.X); ok && g.p.zeroExtLoad[n] {
			g.mov(x, d) // loaded unsigned already
		} else {
			g.zeroExtend(x, intBits(c.From), d)
		}
	case ConvTrunc:
		g.mov(x, d)
		g.signExtend(d, intBits(c.To))
	case ConvIntFloat:
		g.mov(x, d)
		g.op(engine.OpRealAffinity, d, 0, 0, nil)
	case ConvFloatInt:
		g.mov(x, d)
		g.op(engine.OpCast, d, 0, 0, "INTEGER") // toward zero, as C
		g.signExtend(d, intBits(c.To))
	}
}

func (g *gen) storeInstr(s *StoreInstr) {
	t := g.env.resolve(s.Type)
	if n, ok := localName(s.Val); ok && g.p.passThrough[n] {
		g.mov(g.r(s.Addr), g.w0)
		g.fn(g.mem.copyN[g.width(t)], g.w0, 2, g.sink)
		return
	}
	v, addr := g.r(s.Val), g.r(s.Addr)
	if ft, ok := t.(FloatType); ok {
		f := g.mem.storeF64
		if !ft.Double {
			f = g.mem.storeF32
		}
		g.mov(addr, g.w0)
		g.mov(v, g.w1)
		g.fn(f, g.w0, 2, g.sink)
		return
	}
	g.store(addr, g.width(t), v)
}

// gep adds up an address: constant indices fold into one offset, and each
// variable one adds index*stride.
func (g *gen) gep(in *GEPInstr) {
	base, d := g.r(in.Addr), g.dst(in.Dst)
	var fixed int64
	acc := base // the running address register
	cur := in.Base
	for i, ix := range in.Indices {
		stride := g.env.size(cur)
		if i > 0 {
			k, _ := ix.Val.(IntConst)
			elem, st, off, err := g.env.step(cur, k.V)
			if err != nil {
				g.failf("%v", err)
				return
			}
			if _, isStruct := g.env.resolve(cur).(StructType); isStruct {
				if _, ok := ix.Val.(IntConst); !ok {
					g.failf("a variable struct field index")
					return
				}
				fixed += off
				cur = elem
				continue
			}
			stride, cur = st, elem
		}
		switch v := ix.Val.(type) {
		case IntConst:
			fixed += v.V * stride
		case Zero:
		default:
			term := g.r(v)
			if stride != 1 {
				g.do(engine.OpMultiply, term, g.k(stride), g.t3)
				term = g.t3
			}
			g.do(engine.OpAdd, acc, term, d)
			acc = d
		}
	}
	g.plus(acc, fixed, d)
}
