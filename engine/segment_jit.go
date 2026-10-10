package engine

import (
	"unsafe"
	"sync"

	"github.com/samyfodil/musql/internal/jit"
)

// The JITted VDBE's filter path: compile predicates to machine code once,
// then run over every segment. Enabled by default on amd64 and arm64;
// disabled elsewhere or with WithoutJIT. Kernels are cached and never unmapped.

var jitEnabled = jit.Available

// JITEnabled reports whether this process will execute generated machine code.
func JITEnabled() bool { return jitEnabled }

type jitFilterKey struct {
	opA, opC segPredOp
	two      bool
	sum      bool // EmitFilterSumSIMD rather than the count kernel
}

var jitFilterCache sync.Map // jitFilterKey -> *jit.Code

// jitCondOf maps a predicate operator to the condition code the emitter uses.
func jitCondOf(op segPredOp) jit.Cond {
	switch op {
	case segGT:
		return jit.CondG
	case segGE:
		return jit.CondGE
	case segLT:
		return jit.CondL
	case segLE:
		return jit.CondLE
	case segEQ:
		return jit.CondE
	default:
		return jit.CondNE
	}
}

// jitFilterKernel returns the compiled kernel for this predicate shape,
// emitting it on first use. nil when the shape is not one the emitter handles.
func jitFilterKernel(preds []segPred) *jit.Code {
	if !jitEnabled || len(preds) == 0 || len(preds) > 2 {
		// Three or more predicates have no emitter yet.
		//
		// One USED to be excluded here, on the grounds that Go's single-compare
		// loop was already 0.75 ns/row and the trampoline call would eat the
		// difference. That was true of the branchy kernel and is not true of
		// this one: a single compare is five instructions with no jump, against
		// a Go loop that still mispredicts.
		return nil
	}
	two := len(preds) == 2
	key := jitFilterKey{opA: preds[0].Op, two: two}
	if two {
		key.opC = preds[1].Op
	}
	if c, ok := jitFilterCache.Load(key); ok {
		if c == nil {
			return nil
		}
		return c.(*jit.Code)
	}
	// Vectors where they exist, the branch-free scalar loop where they do not.
	// Both answer identically -- TestJITFilterMatchesInterpreter checks each
	// against the same reference -- so this decides only speed.
	condA := jitCondOf(preds[0].Op)
	condC := condA
	if two {
		condC = jitCondOf(preds[1].Op)
	}
	var code []byte
	var err error
	if jit.HasVector() {
		code, err = jit.EmitFilterCountSIMD(condA, two, condC)
	} else {
		code, err = jit.EmitFilterCount(condA, two, condC)
	}
	if err != nil {
		jitFilterCache.Store(key, (*jit.Code)(nil))
		return nil
	}
	k, err := jit.Map(code)
	if err != nil {
		jitFilterCache.Store(key, (*jit.Code)(nil))
		return nil
	}
	actual, _ := jitFilterCache.LoadOrStore(key, k)
	if got := actual.(*jit.Code); got != k {
		// Another goroutine won the race; this one's mapping is unreferenced.
		k.Close()
		return got
	}
	return k
}

// jitFilterCount runs the compiled kernel over one segment, or returns -1 when
// this predicate shape or this segment cannot use it.
//
// Both columns must be fixed-width int64 blocks with no NULL and no exception,
// which is the same condition the zero-copy read requires -- the kernel reads
// the block directly and has no way to consult a bitmap or a side list.
func jitFilterCount(s *segment, preds []segPred) int {
	k := jitFilterKernel(preds)
	if k == nil {
		return -1
	}
	cols := make([][]int64, len(preds))
	for i, p := range preds {
		if p.Val.Typ != Int {
			return -1
		}
		if kind, _ := s.scanKind(p.Col); kind != segScanSlice {
			return -1
		}
		col, ok := s.Int64Column(p.Col)
		if !ok {
			return -1
		}
		cols[i] = col
	}
	n := len(cols[0])
	args := jit.Args{A: unsafe.Pointer(&cols[0][0]), XA: preds[0].Val.I}
	if len(preds) == 2 {
		n = min(n, len(cols[1]))
		args.C, args.XC = unsafe.Pointer(&cols[1][0]), preds[1].Val.I
	}
	if n == 0 {
		return 0
	}
	var out int64
	args.N, args.Out = int64(n), unsafe.Pointer(&out)
	k.Call(&args)
	return int(out)
}

// jitFilterSumKernel is jitFilterKernel for the masked sum: vector units only,
// one or two predicates. nil when the shape or the machine has none.
func jitFilterSumKernel(preds []segPred) *jit.Code {
	if !jitEnabled || !jit.HasVector() || len(preds) == 0 || len(preds) > 2 {
		return nil
	}
	two := len(preds) == 2
	key := jitFilterKey{opA: preds[0].Op, two: two, sum: true}
	if two {
		key.opC = preds[1].Op
	}
	if c, ok := jitFilterCache.Load(key); ok {
		if c == nil {
			return nil
		}
		return c.(*jit.Code)
	}
	condC := jitCondOf(preds[0].Op)
	if two {
		condC = jitCondOf(preds[1].Op)
	}
	code, err := jit.EmitFilterSumSIMD(jitCondOf(preds[0].Op), two, condC)
	var k *jit.Code
	if err == nil {
		k, err = jit.Map(code)
	}
	if err != nil {
		jitFilterCache.Store(key, (*jit.Code)(nil))
		return nil
	}
	actual, _ := jitFilterCache.LoadOrStore(key, k)
	if got := actual.(*jit.Code); got != k {
		k.Close()
		return got
	}
	return k
}

// jitFilterSumRange sums vals over rows [lo, hi) satisfying preds with the
// vector kernel, reporting false when there is none for this shape. The
// caller has already proved the sum cannot overflow.
func jitFilterSumRange(k *jit.Code, cols [][]int64, preds []segPred, vals []int64, lo, hi int) (sum int64, n int) {
	if hi <= lo {
		return 0, 0
	}
	var out, cnt int64
	args := jit.Args{A: unsafe.Pointer(&cols[0][lo]), XA: preds[0].Val.I, V: unsafe.Pointer(&vals[lo]), N: int64(hi - lo), Out: unsafe.Pointer(&out), Out2: unsafe.Pointer(&cnt)}
	if len(preds) == 2 {
		args.C, args.XC = unsafe.Pointer(&cols[1][lo]), preds[1].Val.I
	}
	k.Call(&args)
	return out, int(cnt)
}
