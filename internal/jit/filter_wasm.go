//go:build wasm

package jit

// The filter kernels as WebAssembly. Each is a module that reads Args at the
// pointer f receives and walks the column blocks in Go's memory. The engine
// (V8, SpiderMonkey) compiles it to native code; this decides only what that
// code computes.

// scalarCmp is the i64 comparison opcode for "x cond bound", leaving an i32
// 0 or 1.
func scalarCmp(cond Cond) byte {
	switch cond {
	case CondE:
		return 0x51
	case CondNE:
		return 0x52
	case CondL:
		return 0x53
	case CondG:
		return 0x55
	case CondLE:
		return 0x57
	default: // CondGE
		return 0x59
	}
}

func laneCmp(cond Cond) uint32 {
	switch cond {
	case CondE:
		return simdI64x2Eq
	case CondNE:
		return simdI64x2Ne
	case CondL:
		return simdI64x2LtS
	case CondG:
		return simdI64x2GtS
	case CondLE:
		return simdI64x2LeS
	default:
		return simdI64x2GeS
	}
}

// filterKernel holds the locals every kernel shares.
type filterKernel struct {
	w            Wasm
	a, c, v, end uint32 // i32 cursors and A's end address
	xa, xc       uint32 // i64 bounds
	acc, cnt     uint32 // i64 accumulators
	two, sum     bool
	condA, condC Cond
}

func newFilterKernel(condA Cond, two bool, condC Cond, sum bool) *filterKernel {
	k := &filterKernel{two: two, sum: sum, condA: condA, condC: condC}
	w := &k.w
	k.a, k.c, k.v, k.end = w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	k.xa, k.xc, k.acc, k.cnt = w.Local(wI64), w.Local(wI64), w.Local(wI64), w.Local(wI64)

	w.LoadPtr(OffA)
	w.Set(k.a)
	w.Get(0)
	w.LoadI64(OffXA)
	w.Set(k.xa)
	if two {
		w.LoadPtr(OffC)
		w.Set(k.c)
		w.Get(0)
		w.LoadI64(OffXC)
		w.Set(k.xc)
	}
	if sum {
		w.LoadPtr(OffV)
		w.Set(k.v)
	}
	// end = a + n*8
	w.Get(k.a)
	w.Get(0)
	w.LoadI64(OffN)
	w.op(opI32WrapI64)
	w.I32(3)
	w.op(opI32Shl)
	w.op(opI32Add)
	w.Set(k.end)
	return k
}

// bump advances local l by d bytes.
func (k *filterKernel) bump(l uint32, d int32) {
	k.w.Get(l)
	k.w.I32(d)
	k.w.op(opI32Add)
	k.w.Set(l)
}

// scalarLoop handles every row left between a and end, one at a time.
func (k *filterKernel) scalarLoop() {
	w := &k.w
	w.op(opBlock, opVoid, opLoop, opVoid)
	w.Get(k.a)
	w.Get(k.end)
	w.op(opI32GeU)
	w.BrIf(1)

	// match (i32 0/1) = a[i] condA xa [&& c[i] condC xc]
	w.Get(k.a)
	w.LoadI64(0)
	w.Get(k.xa)
	w.op(scalarCmp(k.condA))
	if k.two {
		w.Get(k.c)
		w.LoadI64(0)
		w.Get(k.xc)
		w.op(scalarCmp(k.condC))
		w.op(opI32And)
	}
	m := w.Local(wI32)
	w.Set(m)
	if k.sum {
		// acc += match ? v[i] : 0
		w.Get(k.acc)
		w.Get(k.v)
		w.LoadI64(0)
		w.I64(0)
		w.Get(m)
		w.op(opSelect)
		w.op(opI64Add)
		w.Set(k.acc)
		k.bump(k.v, 8)
	}
	// cnt += match
	w.Get(k.cnt)
	w.Get(m)
	w.op(opI64ExtendI32U)
	w.op(opI64Add)
	w.Set(k.cnt)

	k.bump(k.a, 8)
	if k.two {
		k.bump(k.c, 8)
	}
	w.Br(0)
	w.op(opEnd, opEnd)
}

// vectorLoop handles two rows per iteration while at least two remain, the
// way the NEON kernel does: a compare leaves each lane all-ones or zero, so
// subtracting the mask counts hits and ANDing it with the values sums them.
func (k *filterKernel) vectorLoop() {
	w := &k.w
	vxa, vxc := w.Local(wV128), w.Local(wV128)
	vcnt, vacc := w.Local(wV128), w.Local(wV128)
	mask := w.Local(wV128)
	w.Get(k.xa)
	w.Simd(simdI64x2Splat)
	w.Set(vxa)
	if k.two {
		w.Get(k.xc)
		w.Simd(simdI64x2Splat)
		w.Set(vxc)
	}
	// v128 locals start zeroed, so vcnt and vacc need no init.

	w.op(opBlock, opVoid, opLoop, opVoid)
	// exit when a+16 > end
	w.Get(k.a)
	w.I32(16)
	w.op(opI32Add)
	w.Get(k.end)
	w.op(opI32GtU)
	w.BrIf(1)

	w.Get(k.a)
	w.LoadV128()
	w.Get(vxa)
	w.Simd(laneCmp(k.condA))
	if k.two {
		w.Get(k.c)
		w.LoadV128()
		w.Get(vxc)
		w.Simd(laneCmp(k.condC))
		w.Simd(simdV128And)
	}
	w.Set(mask)
	if k.sum {
		w.Get(vacc)
		w.Get(k.v)
		w.LoadV128()
		w.Get(mask)
		w.Simd(simdV128And)
		w.Simd(simdI64x2Add)
		w.Set(vacc)
		k.bump(k.v, 16)
	}
	w.Get(vcnt)
	w.Get(mask)
	w.Simd(simdI64x2Sub)
	w.Set(vcnt)

	k.bump(k.a, 16)
	if k.two {
		k.bump(k.c, 16)
	}
	w.Br(0)
	w.op(opEnd, opEnd)

	// Fold the lanes into the scalar accumulators the tail continues.
	fold := func(v, into uint32) {
		w.Get(v)
		w.ExtractLane(0)
		w.Get(v)
		w.ExtractLane(1)
		w.op(opI64Add)
		w.Set(into)
	}
	fold(vcnt, k.cnt)
	if k.sum {
		fold(vacc, k.acc)
	}
}

// finish stores the answer: the count to Out for a count kernel, or the sum
// to Out and the count to Out2 for a sum kernel.
func (k *filterKernel) finish() []byte {
	w := &k.w
	store := func(off, l uint32) {
		w.LoadPtr(off)
		w.Get(l)
		w.StoreI64(0)
	}
	if k.sum {
		store(OffOut, k.acc)
		store(OffOut2, k.cnt)
	} else {
		store(OffOut, k.cnt)
	}
	return w.Module()
}

// EmitFilterCount emits a scalar wasm kernel counting rows satisfying one or
// two integer comparisons.
func EmitFilterCount(condA Cond, two bool, condC Cond) ([]byte, error) {
	k := newFilterKernel(condA, two, condC, false)
	k.scalarLoop()
	return k.finish(), nil
}

// EmitFilterCountSIMD is EmitFilterCount two lanes at a time with SIMD128.
func EmitFilterCountSIMD(condA Cond, two bool, condC Cond) ([]byte, error) {
	k := newFilterKernel(condA, two, condC, false)
	k.vectorLoop()
	k.scalarLoop()
	return k.finish(), nil
}

// EmitFilterSumSIMD sums V over matching rows with SIMD128: Out gets the sum,
// Out2 the count. Like the native twins it does not detect overflow; the
// caller has already proved the sum cannot.
func EmitFilterSumSIMD(condA Cond, two bool, condC Cond) ([]byte, error) {
	k := newFilterKernel(condA, two, condC, true)
	k.vectorLoop()
	k.scalarLoop()
	return k.finish(), nil
}
