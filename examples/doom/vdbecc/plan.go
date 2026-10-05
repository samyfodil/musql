package vdbecc

// Static analysis: everything decided about a function before its code is
// emitted.

// plan is one function's frame. Every SSA value has a register of its own for
// the whole program; a function only needs a stack frame for its allocas, its
// variadic spill area, and, when two of its activations can be live at once,
// the registers it must preserve across its calls.
type plan struct {
	fn      *Func
	entry   label
	labels  map[string]label
	blocks  map[string]*Block
	reg     map[string]int
	owned   []int // registers holding this activation's values
	retAddr int   // the caller's return address, kept here

	allocas  []*AllocaInstr
	slot     map[string]int64 // alloca -> offset in the locals area
	locals   int64            // locals area size, the va spill area included
	vaSpill  int64            // the va spill area's offset, or -1
	preserve []int            // registers saved on the stack across calls

	uses        map[string]int
	zeroExtLoad map[string]bool // loads emitted unsigned, for a zext that then vanishes
	passThrough map[string]bool // loads that a store copies memory-to-memory
}

func (p *plan) frameBytes() int64 { return int64(len(p.preserve))*8 + p.locals }

// countUses counts each SSA name's reads, phi inputs and terminators included.
func countUses(f *Func) map[string]int {
	n := map[string]int{}
	count := func(v *Value) {
		if name, ok := localName(*v); ok {
			n[name]++
		}
	}
	for _, b := range f.Blocks {
		for _, in := range b.Body {
			in.Operands(count)
		}
		b.End.Operands(count)
	}
	return n
}

// aliases reports whether in only renames an existing register: a conversion
// that leaves the 64-bit register contents as they are. Integers are held
// sign-extended and floats as f64, so a sext, a bitcast and an fpext/fptrunc
// change nothing, and a zext of a load emitted unsigned changes nothing.
func (p *plan) aliases(in Instr) (src string, ok bool) {
	c, isConv := in.(*ConvInstr)
	if !isConv {
		return "", false
	}
	name, isLocal := localName(c.X)
	if !isLocal {
		return "", false
	}
	if _, has := p.reg[name]; !has {
		return "", false
	}
	switch c.Kind {
	case ConvSame, ConvSExt, ConvFloatSize:
		return name, true
	case ConvZExt:
		return name, p.zeroExtLoad[name]
	}
	return "", false
}

func (g *gen) newPlan(f *Func) *plan {
	p := &plan{
		fn: f, entry: g.label(), labels: map[string]label{}, blocks: map[string]*Block{},
		reg: map[string]int{}, slot: map[string]int64{}, vaSpill: -1, uses: countUses(f),
	}
	p.zeroExtLoad = findZeroExtLoads(f, p.uses)
	p.passThrough = g.findPassThrough(f, p.uses)
	own := func(name string) {
		r := g.reg()
		p.reg[name] = r
		p.owned = append(p.owned, r)
	}
	for _, prm := range f.Params {
		own(prm.Name)
	}
	p.retAddr = g.reg()
	p.owned = append(p.owned, p.retAddr)
	for _, b := range f.Blocks {
		p.labels[b.Name] = g.label()
		p.blocks[b.Name] = b
		for _, in := range b.Body {
			if src, ok := p.aliases(in); ok {
				p.reg[in.Def()] = p.reg[src]
				continue
			}
			if in.Def() == "" {
				continue
			}
			own(in.Def())
			if a, ok := in.(*AllocaInstr); ok {
				p.allocas = append(p.allocas, a)
				p.slot[a.Dst] = p.locals
				p.locals += roundUp(g.env.size(a.Type)*a.Count, 8)
			}
		}
	}
	if f.Variadic {
		p.vaSpill = p.locals
		p.locals += int64(len(g.args)) * 8
	}
	return p
}

// findZeroExtLoads is the loads whose one reader zero-extends them.
func findZeroExtLoads(f *Func, uses map[string]int) map[string]bool {
	loads := map[string]bool{}
	out := map[string]bool{}
	for _, b := range f.Blocks {
		for _, in := range b.Body {
			switch in := in.(type) {
			case *LoadInstr:
				loads[in.Dst] = true
			case *ConvInstr:
				if x, ok := localName(in.X); ok && in.Kind == ConvZExt && loads[x] && uses[x] == 1 {
					out[x] = true
				}
			}
		}
	}
	return out
}

func touchesMemory(in Instr) bool {
	switch in.(type) {
	case *LoadInstr, *StoreInstr, *CallInstr, *VaArgInstr:
		return true
	}
	return false
}

// findPassThrough is the integer loads whose one reader is a store of the same
// width with no memory access in between: the store copies from the load's
// address instead. That is every Doom column and span blitter.
func (g *gen) findPassThrough(f *Func, uses map[string]int) map[string]bool {
	out := map[string]bool{}
	for _, b := range f.Blocks {
		lastMem := -1
		loadAt := map[string]int{}
		for i, in := range b.Body {
			if st, ok := in.(*StoreInstr); ok {
				if x, ok := localName(st.Val); ok && uses[x] == 1 {
					if j, ok := loadAt[x]; ok && j >= lastMem && !isFloatType(g.env.resolve(st.Type)) {
						ld := b.Body[j].(*LoadInstr)
						if w := g.env.size(st.Type); w == g.env.size(ld.Type) && accessWidth(w) {
							out[x] = true
						}
					}
				}
			}
			if ld, ok := in.(*LoadInstr); ok && !isFloatType(g.env.resolve(ld.Type)) {
				loadAt[ld.Dst] = i
			}
			if touchesMemory(in) {
				lastMem = i
			}
		}
	}
	return out
}

func accessWidth(n int64) bool { return n == 1 || n == 2 || n == 4 || n == 8 }

// ---- liveness ----

type regSet map[int]struct{}

func (s regSet) add(r int) bool {
	if _, ok := s[r]; ok {
		return false
	}
	s[r] = struct{}{}
	return true
}

// choosePreserved fills p.preserve: the owned registers holding a value at a
// call that is read after it, plus the return address. Only a function that
// can be re-entered needs it -- a nested activation reuses the same registers.
func (p *plan) choosePreserved() {
	defOf := func(in Instr) (int, bool) {
		if _, ok := p.aliases(in); ok || in.Def() == "" {
			return 0, false
		}
		return p.reg[in.Def()], true
	}
	readsOf := func(in Instr, visit func(int)) {
		if _, ok := p.aliases(in); ok {
			return
		}
		if _, ok := in.(*PhiInstr); ok {
			return // a phi reads on its incoming edges
		}
		in.Operands(func(v *Value) {
			if n, ok := localName(*v); ok {
				if r, ok := p.reg[n]; ok {
					visit(r)
				}
			}
		})
	}
	termReads := func(b *Block, visit func(int)) {
		b.End.Operands(func(v *Value) {
			if n, ok := localName(*v); ok {
				if r, ok := p.reg[n]; ok {
					visit(r)
				}
			}
		})
	}

	// Block summaries: upward-exposed reads and definitions; and what each
	// edge's phis read, which is live out of the predecessor only.
	type summary struct {
		reads, defs regSet
		preds       []string
	}
	sum := map[string]*summary{}
	for _, b := range p.fn.Blocks {
		sum[b.Name] = &summary{reads: regSet{}, defs: regSet{}}
	}
	edgeReads := map[[2]string][]int{}
	for _, b := range p.fn.Blocks {
		s := sum[b.Name]
		read := func(r int) {
			if _, def := s.defs[r]; !def {
				s.reads.add(r)
			}
		}
		for _, in := range b.Body {
			readsOf(in, read)
			if d, ok := defOf(in); ok {
				s.defs.add(d)
			}
			if phi, ok := in.(*PhiInstr); ok {
				for _, e := range phi.In {
					if n, ok := localName(e.Val); ok {
						if r, ok := p.reg[n]; ok {
							k := [2]string{e.From, b.Name}
							edgeReads[k] = append(edgeReads[k], r)
						}
					}
				}
			}
		}
		termReads(b, read)
		b.End.Succs(func(to string) {
			if t, ok := sum[to]; ok {
				t.preds = append(t.preds, b.Name)
			}
		})
	}

	// Backward dataflow to a fixed point, driven by a worklist of blocks whose
	// live-in grew.
	liveIn, liveOut := map[string]regSet{}, map[string]regSet{}
	work := make([]string, 0, len(p.fn.Blocks))
	queued := map[string]bool{}
	for _, b := range p.fn.Blocks {
		liveIn[b.Name], liveOut[b.Name] = regSet{}, regSet{}
		work = append(work, b.Name)
		queued[b.Name] = true
	}
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		queued[name] = false
		b, out := p.blocks[name], liveOut[name]
		b.End.Succs(func(to string) {
			for r := range liveIn[to] {
				out.add(r)
			}
			for _, r := range edgeReads[[2]string{name, to}] {
				out.add(r)
			}
		})
		s, in, grew := sum[name], liveIn[name], false
		for r := range s.reads {
			grew = in.add(r) || grew
		}
		for r := range out {
			if _, def := s.defs[r]; !def {
				grew = in.add(r) || grew
			}
		}
		if grew {
			for _, pr := range s.preds {
				if !queued[pr] {
					work = append(work, pr)
					queued[pr] = true
				}
			}
		}
	}

	// Walk each block backwards from its live-out; whatever is live just past
	// a call, other than the call's own result, crosses it.
	owned := regSet{}
	for _, r := range p.owned {
		owned.add(r)
	}
	crosses := regSet{}
	for _, b := range p.fn.Blocks {
		live := regSet{}
		for r := range liveOut[b.Name] {
			live.add(r)
		}
		termReads(b, func(r int) { live.add(r) })
		for i := len(b.Body) - 1; i >= 0; i-- {
			in := b.Body[i]
			d, hasDef := defOf(in)
			if _, isCall := in.(*CallInstr); isCall {
				for r := range live {
					if _, mine := owned[r]; mine && (!hasDef || r != d) {
						crosses.add(r)
					}
				}
			}
			if hasDef {
				delete(live, d)
			}
			readsOf(in, func(r int) { live.add(r) })
		}
	}
	for _, r := range p.owned {
		if _, ok := crosses[r]; ok || r == p.retAddr {
			p.preserve = append(p.preserve, r)
		}
	}
}

// ---- reentrancy ----

// reentrant is the functions that may have two activations at once: those on
// a cycle of the call graph. An indirect call may reach any function whose
// address is taken. The cycles are found with Kosaraju's algorithm: a DFS
// finishing order on the graph, then components on its transpose.
func reentrant(m *Module) map[string]bool {
	defined := map[string]bool{}
	for _, f := range m.Funcs {
		defined[f.Name] = true
	}
	taken := map[string]bool{}
	noteRef := func(v *Value) {
		if g, ok := (*v).(GlobalRef); ok && defined[g.Name] {
			taken[g.Name] = true
		}
	}
	for _, v := range m.Vars {
		for i := range v.Fixups {
			noteRef(&v.Fixups[i].Ref)
		}
	}
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for _, in := range b.Body {
				in.Operands(noteRef)
			}
			b.End.Operands(noteRef)
		}
	}
	calls := map[string][]string{}
	for _, f := range m.Funcs {
		seen := map[string]bool{}
		edge := func(to string) {
			if defined[to] && !seen[to] {
				seen[to] = true
				calls[f.Name] = append(calls[f.Name], to)
			}
		}
		for _, b := range f.Blocks {
			for _, in := range b.Body {
				c, ok := in.(*CallInstr)
				switch {
				case !ok:
				case c.Callee != "":
					edge(c.Callee)
				default:
					for _, g := range m.Funcs {
						if taken[g.Name] {
							edge(g.Name)
						}
					}
				}
			}
		}
	}
	callers := map[string][]string{}
	for from, tos := range calls {
		for _, to := range tos {
			callers[to] = append(callers[to], from)
		}
	}

	var order []string
	done := map[string]bool{}
	var finish func(string)
	finish = func(n string) {
		done[n] = true
		for _, to := range calls[n] {
			if !done[to] {
				finish(to)
			}
		}
		order = append(order, n)
	}
	for _, f := range m.Funcs {
		if !done[f.Name] {
			finish(f.Name)
		}
	}
	comp := map[string]int{}
	size := map[int]int{}
	var collect func(string, int)
	collect = func(n string, c int) {
		comp[n] = c
		size[c]++
		for _, from := range callers[n] {
			if _, ok := comp[from]; !ok {
				collect(from, c)
			}
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		if _, ok := comp[order[i]]; !ok {
			collect(order[i], i)
		}
	}
	out := map[string]bool{}
	for _, f := range m.Funcs {
		if size[comp[f.Name]] > 1 {
			out[f.Name] = true
		}
		for _, to := range calls[f.Name] {
			if to == f.Name {
				out[f.Name] = true
			}
		}
	}
	return out
}
