package engine

import (
	"math"
	"sort"
	"sync"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// A COMPILED PRE-FILTER over a row scan.
//
// The lazy row source (segment_cursor.go) makes a segment scan cheaper per row
// than a b-tree walk. This makes it visit fewer rows: the scan loop's WHERE is
// compiled to machine code that answers WHICH rows can match, and the source
// hands back only those.
//
// IT IS A HINT AND NOTHING ELSE, which is the whole reason it is allowed to
// exist. The statement's real predicate stays exactly where the compiler put
// it and runs again on every row this selects, so a row selected that should
// not have been costs time and cannot change an answer. Only the other
// direction -- a row NOT selected that should have been -- could be wrong, and
// that is why the lowering is the same total one the aggregate path uses and
// why any segment it cannot lower scans in full instead.
//
// This is the same bargain OpSeekRowidHint already makes for a rowid-pinned
// WHERE (vdbe_cursor.go's seekConfigured): restrict the candidate set, keep
// the conjunct.

// segRowFilter is what the recogniser attaches to an OpRewind.
type segRowFilter struct {
	low *segLowered
	// The predicate as the VDBE has it, which a segment with REAL columns
	// lowers again for them (forKinds), and those lowerings by kinds.
	body              []Instruction
	base, cursor, end int
	cols              []int
	params            []int
	variants          sync.Map // segKinds.key() -> *segLowered, nil when it does not lower
}

// forKinds is the filter lowered for kinds, on first use.
func (f *segRowFilter) forKinds(kinds *segKinds) *segLowered {
	if kinds == nil {
		return f.low
	}
	key := kinds.key()
	if v, ok := f.variants.Load(key); ok {
		return v.(*segLowered)
	}
	low, ok := segLowerBody(f.body, f.base, f.cursor, f.end, kinds)
	if ok {
		low = segSelecting(low)
	} else {
		low = nil
	}
	v, _ := f.variants.LoadOrStore(key, low)
	return v.(*segLowered)
}

// segSelecting makes low a selection: its POpMath kernels, instead of
// stopping for their calls, select a row they cannot judge, and the
// selection's POpEmitRow is appended.
func segSelecting(low *segLowered) *segLowered {
	blocks := low.blocks[:0:0]
	for _, b := range low.blocks {
		if !b.math {
			blocks = append(blocks, b)
		}
	}
	low.blocks = blocks
	for i := range low.insns {
		if low.insns[i].Op == jit.POpMath {
			low.insns[i].Imm = jit.ProgMathSelects
		}
	}
	low.add(jit.ProgInsn{Op: jit.POpEmitRow})
	return low
}

// kindsFor is which of the columns the filter reads are REAL blocks in s,
// and which of its parameters are bound to REALs: nil when none.
func (f *segRowFilter) kindsFor(s *segment, ipkCol int, params []Value) *segKinds {
	var kinds *segKinds
	mark := func(m func(*segKinds) map[int]bool, k int) {
		if kinds == nil {
			kinds = &segKinds{realCol: map[int]bool{}, realParam: map[int]bool{}}
		}
		m(kinds)[k] = true
	}
	for _, c := range f.cols {
		if c != ipkCol && c >= 0 && c < len(s.cols) && s.cols[c].phys == PhysFloat64 {
			mark(func(k *segKinds) map[int]bool { return k.realCol }, c)
		}
	}
	for _, p := range f.params {
		if p >= 1 && p <= len(params) && params[p-1].Typ == Float {
			mark(func(k *segKinds) map[int]bool { return k.realParam }, p)
		}
	}
	return kinds
}

// segRowFilterPeephole finds a row-returning scan loop whose WHERE lowers, and
// attaches the compiled predicate to the loop's own OpRewind.
//
// IT ADDS NO INSTRUCTION AND MOVES NONE. OpRewind's P4 is unused, so the hint
// rides on an instruction that is already there and every address in the
// program stays exactly what it was. That is deliberate: the aggregate
// recogniser splices a guard in and had to be rewritten once already when a
// shifted jump target corrupted the arm that runs when the guard declines.
// There is no such arm here and no such shift.
func segRowFilterPeephole(prog *Program) bool {
	if !jitEnabled || !jit.Available || prog == nil {
		return false
	}
	in := prog.Insns
	found := false
	for at := range in {
		if in[at].Op != OpRewind || in[at].P4 != nil {
			continue
		}
		cursor := in[at].P1
		// ONLY A CURSOR OPENED BY OpOpenRead. Segments back TABLES, so a cursor
		// that is not reading one can never have any -- a derived table, a
		// recursive CTE's queue, a sorter drain. Attaching to one is not merely
		// useless, it is OBSERVABLE: the recursive CTE body of
		//
		//   WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3)
		//   INSERT OR FAIL INTO u SELECT ... FROM c
		//
		// is a 15-instruction scan over the queue cursor whose "x<3" lowers
		// perfectly, and hanging a filter on its Rewind changed how many rows
		// the INSERT got through before the overflow aborted it -- engine 1,
		// SQLite 2. The differential caught it; neither corpus sweep did,
		// because no corpus statement builds segments AND drives a recursive
		// CTE into an aborting INSERT.
		if !segCursorReadsATable(in, at, cursor) {
			continue
		}
		if segAttachRowFilter(in, at, cursor) {
			found = true
		}
	}
	return found
}

// segAttachRowFilter compiles the leading WHERE of the scan loop OpRewind at
// starts and hangs it on that Rewind's P4, reporting whether it did.
func segAttachRowFilter(in []Instruction, at, cursor int) bool {
	// The loop's Next is the instruction that jumps BACK to the body's
	// first address; its own address ends the body.
	nextAt := -1
	for j := at + 1; j < len(in); j++ {
		if in[j].Op == OpNext && in[j].P1 == cursor && in[j].P2 == at+1 {
			nextAt = j
			break
		}
	}
	if nextAt < 0 {
		return false
	}
	// The predicate is the leading run of "evaluate, and leave for the Next
	// if it failed" groups. "a > ? AND b < ?" is TWO such groups, so
	// stopping at the first jump would compile only the first conjunct --
	// still correct, because a superset of the matching rows is exactly
	// what a hint is allowed to be, but it selected every row of a 20,000
	// row table where 5,884 matched and bought nothing.
	//
	// So take the LONGEST prefix that lowers, by trying each group
	// boundary in turn. Projection follows the last one and does not
	// lower, which is what ends the search; the cost is a few compile-time
	// attempts on a program that is compiled once.
	//
	// A body is judged lowerable when it lowers for INTEGER columns or for
	// the table's REAL-affinity columns held as REALs; each segment then
	// gets the lowering for the kinds it actually holds (forKinds).
	guess := segRealAffinityKinds(in, at, cursor)
	predEnd := -1
	// The INTEGER lowering of the longest prefix, which the filter keeps: the
	// last attempt's, when it lowered without the guess.
	var plain *segLowered
	for j := at + 1; j < nextAt; j++ {
		if (in[j].Op != OpIfNot && in[j].Op != OpIf) || in[j].P2 != nextAt {
			continue
		}
		low, lowers := segBodyLowers(in[at+1:j+1], at+1, cursor, j+1, guess)
		if !lowers {
			break
		}
		predEnd, plain = j+1, low
	}
	if predEnd < 0 {
		return false // no WHERE this models: every row is a candidate anyway
	}
	f := &segRowFilter{body: in[at+1 : predEnd], base: at + 1, cursor: cursor, end: predEnd}
	if plain != nil {
		f.low = segSelecting(plain)
	}
	seen := map[int]bool{}
	for _, ins := range f.body {
		switch {
		case ins.Op == OpColumn && ins.P1 == cursor && !seen[ins.P2]:
			seen[ins.P2] = true
			f.cols = append(f.cols, ins.P2)
		case ins.Op == OpVariable:
			f.params = append(f.params, ins.P1)
		}
	}
	in[at].P4 = f
	return true
}

// segBodyLowers reports whether body lowers to a program that reads a
// column, for INTEGER columns or for guess, and returns the INTEGER lowering
// when that is the one that did (nil when only guess's did).
func segBodyLowers(body []Instruction, base, cursor, end int, guess *segKinds) (*segLowered, bool) {
	low, ok := segLowerBody(body, base, cursor, end, nil)
	if ok && len(low.cols) > 0 {
		return low, true
	}
	if guess == nil {
		return nil, false
	}
	low, ok = segLowerBody(body, base, cursor, end, guess)
	return nil, ok && len(low.cols) > 0
}

// segRealAffinityKinds is the kinds of the cursor's table with its
// REAL-affinity columns held as REALs, or nil when it has none.
func segRealAffinityKinds(in []Instruction, at, cursor int) *segKinds {
	var t *resolvedTable
	for i := 0; i < at; i++ {
		if (in[i].Op == OpOpenRead || in[i].Op == OpOpenWrite) && in[i].P1 == cursor {
			t, _ = in[i].P4.(*resolvedTable)
		}
	}
	if t == nil {
		return nil
	}
	var kinds *segKinds
	for c, col := range t.cols {
		if col.Aff == affReal {
			if kinds == nil {
				kinds = &segKinds{realCol: map[int]bool{}, realParam: map[int]bool{}}
			}
			kinds.realCol[c] = true
		}
	}
	return kinds
}

// segWriteFilterPeephole is segRowFilterPeephole for a write program's scan --
// "UPDATE t SET ... WHERE k = ?", "DELETE FROM t WHERE k = ?" -- whose cursor
// reads the table's ROW STORE: materializeRowStore then decodes only the base
// rows the filter selects, plus every row this session or the log wrote,
// instead of every row of the table (segRows.walkFiltered).
//
// The cursor then holds a SUBSET of the table, which is only safe when nothing
// in the program asks it for a row the WHERE did not select -- a rowid seek, a
// trigger's re-read, a REPLACE victim found through it. So it applies only when
// EVERY instruction is on a short list that cannot: open, rewind, column and
// rowid reads, constants, comparisons and arithmetic, the record and the
// UpdateRow/Delete it feeds, and the loop's Next. A trigger, a foreign key, a
// RETURNING, an index seek or anything else this list does not name leaves the
// program exactly as compiled.
func segWriteFilterPeephole(prog *Program) bool {
	if !jitEnabled || !jit.Available || prog == nil {
		return false
	}
	in := prog.Insns
	for i := range in {
		if !segWriteFilterSafeOps[in[i].Op] {
			return false
		}
	}
	found := false
	for at := range in {
		if in[at].Op != OpRewind || in[at].P4 != nil {
			continue
		}
		opened := false
		for i := 0; i < at; i++ {
			if in[i].Op == OpOpenWrite && in[i].P1 == in[at].P1 {
				opened = true
			}
		}
		if opened && segAttachRowFilter(in, at, in[at].P1) {
			found = true
		}
	}
	return found
}

var segWriteFilterSafeOps = map[OpCode]bool{
	OpInit: true, OpOpenWrite: true, OpRewind: true, OpNext: true, OpHalt: true, OpGoto: true,
	OpColumn: true, OpRowid: true, OpVariable: true, OpInteger: true, OpReal: true, OpString8: true, OpNull: true,
	OpEq: true, OpNe: true, OpLt: true, OpLe: true, OpGt: true, OpGe: true, OpIf: true, OpIfNot: true, OpNot: true,
	OpAdd: true, OpSubtract: true, OpMultiply: true, OpRemainder: true, OpCopy: true, OpSCopy: true,
	OpAffinity: true, OpMakeRecord: true, OpUpdateRow: true, OpDelete: true,
}

// segFilterColumns is segOrderColumns' counterpart for a PRE-FILTER, and the
// difference is the whole reason a nullable column can be filtered at all.
//
// segOrderColumns goes through segCleanInt64Column, which REFUSES a column
// carrying a NULL bitmap or an exception-list entry -- rightly, because a path
// that must answer the statement cannot read a block whose values are
// meaningless for some rows. A pre-filter is not answering anything. It only
// has to return a SUPERSET, so it can read the raw block and then declare the
// rows it could not have judged truthfully.
//
// For every row NOT in uncertain, the column's true value IS the int64 in the
// block, so the kernel evaluated the real comparison on it -- and that holds
// for an arbitrary predicate over several columns, because uncertain is the
// union over all of them. For every row IN uncertain, nothing is claimed: it is
// added to the selection unconditionally and the VDBE's own WHERE decides it.
//
// So a column with NULLs costs the scan its NULL rows, not its fast path. That
// matters because "the column has no NULLs anywhere" is a property synthetic
// data has and real tables mostly do not.
func segFilterColumns(s *segment, want []int, asNull, real []bool, ipkCol int) (blocks [][]int64, isRowid []bool, uncertain []int, ok bool) {
	blocks = make([][]int64, len(want))
	isRowid = make([]bool, len(want))
	seen := make(map[int]bool)
	for i, c := range want {
		if i < len(asNull) && asNull[i] {
			// A NULL INDICATOR is exact for every row -- it IS the bitmap -- so
			// it makes nothing uncertain. The rowid alias's is all zeros: it is
			// stored as NULL but reads as the rowid, which is never NULL.
			if c == ipkCol && ipkCol >= 0 {
				blocks[i] = make([]int64, s.nRows)
				continue
			}
			idx, okIdx := s.nullIndicator(c)
			if !okIdx {
				return nil, nil, nil, false
			}
			blocks[i] = idx
			continue
		}
		if c == ipkCol && ipkCol >= 0 {
			isRowid[i] = true // the rowid block has neither NULLs nor exceptions
			continue
		}
		phys := PhysInt64
		if i < len(real) && real[i] {
			phys = PhysFloat64
		}
		if c < 0 || c >= len(s.cols) || s.cols[c].phys != phys {
			return nil, nil, nil, false
		}
		col, okCol := s.rawWordColumn(c)
		if !okCol {
			return nil, nil, nil, false // unaligned, or not little-endian
		}
		blocks[i] = col
		if off := int(s.cols[c].nullOff); off != 0 {
			for r := 0; r < s.nRows; r++ {
				if s.buf[off+r/8]&(1<<uint(r%8)) != 0 {
					seen[r] = true
				}
			}
		}
	}
	if len(s.exc) != 0 {
		nCols := uint64(len(s.cols))
		for k := range s.exc {
			c := int(k % nCols)
			for i, w := range want {
				// An indicator slot reads the bitmap, which an exception does
				// not disturb: a value that missed its physical type is still
				// NOT NULL, and the bitmap says so.
				if w == c && !(i < len(asNull) && asNull[i]) {
					seen[int(k/nCols)] = true
					break
				}
			}
		}
	}
	if len(seen) > 0 {
		uncertain = make([]int, 0, len(seen))
		for r := range seen {
			uncertain = append(uncertain, r)
		}
		sort.Ints(uncertain)
	}
	return blocks, isRowid, uncertain, true
}

// segCursorReadsATable reports whether cursor was opened by an OpOpenRead
// before address at -- the only kind of cursor a segment can back.
//
// It scans for the LAST open before at, so a cursor number reused across two
// loops is judged by the open that actually applies to this one.
func segCursorReadsATable(in []Instruction, at, cursor int) bool {
	reads := false
	for i := 0; i < at; i++ {
		switch in[i].Op {
		case OpOpenRead:
			if in[i].P1 == cursor {
				reads = true
			}
		case OpOpenWrite, OpOpenDerived:
			if in[i].P1 == cursor {
				reads = false
			}
		}
	}
	return reads
}

// segSelectRows runs the compiled predicate over one segment and returns the
// row indices that can match, or false to decline -- in which case the caller
// scans every row of that segment, which is what it did before.
func segSelectRows(s *segment, f *segRowFilter, ipkCol int, params []Value, buf []int64) ([]int64, bool) {
	sel, ok := segSelectPrep(s, f, ipkCol, params)
	if !ok {
		return nil, false
	}
	return sel.run(0, s.nRows, buf)
}

// segSelection is the compiled predicate made ready for one segment, run over
// any window of its rows. A scan that stops early (a LIMIT) judges only the
// windows it reaches instead of the whole segment.
type segSelection struct {
	s         *segment
	kern      *jit.Code
	blocks    [][]int64
	isRowid   []bool
	uncertain []int
	regs      []int64
	consts    []int64 // regs as the bound parameters set them, restored per run
	rowids    [][]int64
}

func segSelectPrep(s *segment, f *segRowFilter, ipkCol int, params []Value) (*segSelection, bool) {
	if s == nil || f == nil || s.nRows == 0 {
		return nil, false
	}
	low := f.forKinds(f.kindsFor(s, ipkCol, params))
	if low == nil {
		return nil, false
	}
	if len(low.blocks) > 0 {
		// A selection kernel with services needs the driver loop and a
		// machine to run them on, which this hint path does not have yet.
		return nil, false
	}
	kern := segProgKernel(low)
	if kern == nil {
		return nil, false
	}
	blocks, isRowid, uncertain, ok := segFilterColumns(s, low.cols, low.nullCol, low.realCol, ipkCol)
	if !ok {
		return nil, false
	}
	if len(uncertain) > s.nRows/2 {
		// More than half the rows unjudgeable makes the selection no smaller
		// than the scan it was meant to shrink, and it still costs a kernel
		// call and a merge. Decline and let the scan run.
		return nil, false
	}
	if len(blocks) > len(jit.ProgArgs{}.Col) {
		return nil, false
	}
	for i, b := range blocks {
		if !isRowid[i] && len(b) < s.nRows {
			return nil, false
		}
	}
	regs := make([]int64, low.nRegs+2)
	for i, reg := range low.consts {
		idx := low.cvIndex[i]
		if idx < 1 || idx > len(params) || reg >= len(regs) {
			return nil, false
		}
		// A bound of any other class than the lowering's declines the whole
		// selection: coercing here is exactly where SQLite's cross-class
		// ordering would be got wrong. Declining costs a full scan, which is
		// the answer this started from.
		switch v := params[idx-1]; {
		case low.real[reg] && v.Typ == Float && v.F == v.F:
			regs[reg] = int64(math.Float64bits(v.F))
		case !low.real[reg] && v.Typ == Int:
			regs[reg] = v.I
		default:
			return nil, false
		}
	}
	for _, idx := range low.exactParams {
		if idx < 1 || idx > len(params) {
			return nil, false
		}
		if v := params[idx-1]; v.Typ != Int || v.I > 1<<53 || v.I < -(1<<53) {
			return nil, false
		}
	}
	if !low.seedSorted(regs, params) {
		return nil, false
	}
	return &segSelection{
		s: s, kern: kern, blocks: blocks, isRowid: isRowid, uncertain: uncertain,
		regs: regs, consts: append([]int64(nil), regs...), rowids: make([][]int64, len(blocks)),
	}, true
}

// run returns the rows in [lo, hi) that can match, in ascending order, or
// false to decline that window.
func (p *segSelection) run(lo, hi int, buf []int64) ([]int64, bool) {
	n := hi - lo
	if n <= 0 {
		return buf[:0], true
	}
	copy(p.regs, p.consts)
	if cap(buf) < n {
		buf = make([]int64, n)
	}
	buf = buf[:n]
	var out, ovf int64
	args := &jit.ProgArgs{N: int64(n), Regs: &p.regs[0], Out: &out, Overflow: &ovf, Sel: &buf[0]}
	for i, b := range p.blocks {
		if p.isRowid[i] {
			// A rowid-alias column has no block of its own; materialize the
			// window's, exactly as segRunProgramAll does.
			if cap(p.rowids[i]) < n {
				p.rowids[i] = make([]int64, n)
			}
			b = p.rowids[i][:n]
			for r := range b {
				b[r] = int64(p.s.Rowid(lo + r))
			}
			args.Col[i] = &b[0]
			continue
		}
		args.Col[i] = &b[lo]
	}
	p.kern.Call2(args)
	if ovf != 0 {
		return nil, false
	}
	// The emitted indices count up from -n, so shift them back in place.
	buf = buf[:out]
	for i := range buf {
		buf[i] += int64(n)
		if buf[i] < 0 || buf[i] >= int64(n) {
			return nil, false // a selection outside the window is a bug, not a hint
		}
		buf[i] += int64(lo)
	}
	a := sort.SearchInts(p.uncertain, lo)
	b := sort.SearchInts(p.uncertain, hi)
	if a == b {
		return buf, true
	}
	return segMergeUncertain(buf, p.uncertain[a:b]), true
}

// segMergeUncertain unions the rows the kernel could not judge into its
// selection, keeping SCAN ORDER -- which the source's whole contract rests on,
// since an unsorted result, a LIMIT and an outer join loop all depend on it.
//
// Both inputs are ascending and disjointness is not assumed: a row can be both
// selected by the kernel (on garbage) and uncertain, and must appear once.
func segMergeUncertain(sel []int64, uncertain []int) []int64 {
	out := make([]int64, 0, len(sel)+len(uncertain))
	i, j := 0, 0
	for i < len(sel) && j < len(uncertain) {
		switch a, b := sel[i], int64(uncertain[j]); {
		case a < b:
			out = append(out, a)
			i++
		case a > b:
			out = append(out, b)
			j++
		default:
			out = append(out, a)
			i++
			j++
		}
	}
	out = append(out, sel[i:]...)
	for ; j < len(uncertain); j++ {
		out = append(out, int64(uncertain[j]))
	}
	return out
}

// rawWordColumn is column c's fixed-width block as words, NULLs and exceptions
// included: an INTEGER block's values, a REAL block's bits. What a row's word
// means where it is NULL or an exception is the caller's business.
func (s *segment) rawWordColumn(c int) ([]int64, bool) {
	if c < 0 || c >= len(s.cols) || !segLittleEndian {
		return nil, false
	}
	if p := s.cols[c].phys; p != PhysInt64 && p != PhysFloat64 {
		return nil, false
	}
	off := int(s.cols[c].blockOff)
	if off == 0 || uintptr(unsafe.Pointer(&s.buf[off]))%8 != 0 {
		return nil, false
	}
	return unsafe.Slice((*int64)(unsafe.Pointer(&s.buf[off])), s.nRows), true
}
