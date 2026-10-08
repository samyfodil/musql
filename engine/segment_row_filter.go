package engine

import (
	"sort"

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
	var low *segLowered
	predEnd := -1
	for j := at + 1; j < nextAt; j++ {
		if (in[j].Op != OpIfNot && in[j].Op != OpIf) || in[j].P2 != nextAt {
			continue
		}
		cand, ok := segLowerBody(in[at+1:j+1], at+1, cursor, j+1)
		if !ok || len(cand.cols) == 0 {
			break
		}
		low, predEnd = cand, j+1
	}
	if low == nil || predEnd < 0 {
		return false // no WHERE this models: every row is a candidate anyway
	}
	low.insns = append(low.insns, jit.ProgInsn{Op: jit.POpEmitRow})
	in[at].P4 = &segRowFilter{low: low}
	return true
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
func segFilterColumns(s *segment, want []int, asNull []bool, ipkCol int) (blocks [][]int64, isRowid []bool, uncertain []int, ok bool) {
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
		if c < 0 || c >= len(s.cols) || s.cols[c].phys != PhysInt64 {
			return nil, nil, nil, false
		}
		col, okCol := s.Int64Column(c)
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
	if s == nil || f == nil || f.low == nil || s.nRows == 0 {
		return nil, false
	}
	low := f.low
	kern := segProgKernel(low)
	if kern == nil {
		return nil, false
	}
	blocks, isRowid, uncertain, ok := segFilterColumns(s, low.cols, low.nullCol, ipkCol)
	if !ok {
		return nil, false
	}
	if len(uncertain) > s.nRows/2 {
		// More than half the rows unjudgeable makes the selection no smaller
		// than the scan it was meant to shrink, and it still costs a kernel
		// call and a merge. Decline and let the scan run.
		return nil, false
	}
	regs := make([]int64, low.nRegs+2)
	for i, reg := range low.consts {
		idx := low.cvIndex[i]
		if idx < 1 || idx > len(params) || reg >= len(regs) {
			return nil, false
		}
		// A non-integer bound declines the whole selection: every value in a
		// compiled program is an int64, and coercing here is exactly where
		// SQLite's cross-class ordering would be got wrong. Declining costs a
		// full scan, which is the answer this started from.
		if v := params[idx-1]; v.Typ == Int {
			regs[reg] = v.I
		} else {
			return nil, false
		}
	}
	// A rowid-alias column has no block of its own; materialize one, exactly
	// as segRunProgramAll does.
	for i := range blocks {
		if !isRowid[i] {
			continue
		}
		tmp := make([]int64, s.nRows)
		for r := 0; r < s.nRows; r++ {
			tmp[r] = int64(s.Rowid(r))
		}
		blocks[i] = tmp
	}
	if cap(buf) < s.nRows {
		buf = make([]int64, s.nRows)
	}
	buf = buf[:s.nRows]
	var out, ovf int64
	args := &jit.ProgArgs{N: int64(s.nRows), Regs: &regs[0], Out: &out, Overflow: &ovf, Sel: &buf[0]}
	for i, b := range blocks {
		if len(b) < s.nRows {
			return nil, false
		}
		args.Col[i] = &b[0]
	}
	kern.Call2(args)
	if ovf != 0 {
		return nil, false
	}
	// The emitted indices count up from -n, so shift them back in place.
	buf = buf[:out]
	for i := range buf {
		buf[i] += int64(s.nRows)
		if buf[i] < 0 || buf[i] >= int64(s.nRows) {
			return nil, false // a selection outside the segment is a bug, not a hint
		}
	}
	if len(uncertain) == 0 {
		return buf, true
	}
	return segMergeUncertain(buf, uncertain), true
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
