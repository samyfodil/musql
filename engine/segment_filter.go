package engine

import "bytes"

// Lowering a filter onto a segment: several predicates, not one.
//
// This layer uses branch-free kernels because their cost does not depend on
// the data, so a planner needs no selectivity estimate to choose them.

// segPredOp is a comparison against a constant.
type segPredOp uint8

const (
	segGT segPredOp = iota
	segGE
	segLT
	segLE
	segEQ
	segNE
)

func (o segPredOp) satisfied(c int) bool {
	switch o {
	case segGT:
		return c > 0
	case segGE:
		return c >= 0
	case segLT:
		return c < 0
	case segLE:
		return c <= 0
	case segEQ:
		return c == 0
	default:
		return c != 0
	}
}

// segPred is one predicate: a column compared against a value.
type segPred struct {
	Col int
	Op  segPredOp
	Val Value
}

// segFilterCount counts the rows of a segment satisfying every predicate.
//
// A row satisfies a predicate only if compareValues says so, which is the same
// comparator every other read path here uses -- so SQLite's ordering across
// storage classes holds, a NULL satisfies nothing, and a stray TEXT in an
// integer column is greater than any integer. The fast path is taken per
// PREDICATE, not per query: one clean int64 column can be filtered through its
// slice while another in the same query goes through the accessor.
// segPredColsFilterable reports whether every column the predicates read can be
// filtered from s's blocks at all -- which is scanKind's safety conditions
// WITHOUT its int64 requirement, because that requirement is about which kernel
// can run, not about whether the answer would be right.
//
// A NULL is the one that matters and it is not obvious. SQLite does not rewrite
// existing records on ALTER TABLE ADD COLUMN: the record stays short and the
// column's DEFAULT is synthesised at READ time. The converter stores that as a
// NULL cell, so a block cannot tell "this row is genuinely NULL" from "this row
// predates the column and is really its default". Filtering over it undercounts,
// silently: TestSegLazyRowsAddColumnDefault has "count(*) WHERE c = 5" answering
// 1 where C SQLite answers 3, for a column added with DEFAULT 5.
//
// scanKind refused that case as a side effect of demanding a clean int64 block.
// This asks for it directly, so a REAL column can take the block path and a
// NULL-bearing column of any type still cannot.
func segPredColsFilterable(s *segment, preds []segPred) bool {
	for _, p := range preds {
		if p.Col < 0 || p.Col >= len(s.cols) {
			return false
		}
		if s.cols[p.Col].nullOff != 0 || s.hasException(p.Col) {
			return false
		}
	}
	return true
}

func segFilterCount(s *segment, preds []segPred) int {
	if len(preds) == 0 {
		return s.nRows
	}
	// Process one batch at a time into a flag array. A batch stays in L1
	// cache while every predicate is applied, avoiding full-segment allocation.
	// The operator is chosen outside the row loop to ensure inlining.
	//
	// For single predicates on fixed-width int64 columns, the fused path counts
	// with no branch and no intermediate pass. The JIT tries first, where
	// available.
	if n := jitFilterCount(s, preds); n >= 0 {
		return n
	}
	if n := segFusedCount(s, preds); n >= 0 {
		return n
	}

	var flags [segFilterBatch]uint8
	count := 0
	for lo := 0; lo < s.nRows; lo += segFilterBatch {
		hi := min(lo+segFilterBatch, s.nRows)
		live := flags[:hi-lo]
		for i := range live {
			live[i] = 1
		}
		for _, p := range preds {
			segApplyPred(s, p, lo, live)
		}
		for _, f := range live {
			count += int(f)
		}
	}
	return count
}

// segFilterBatch is how many rows are filtered at once. 2048 flag bytes stay in
// L1 alongside the column slices a predicate reads.
const segFilterBatch = 2048

// segApplyPred ANDs one predicate into live, for the batch of rows starting at
// lo.
// segApplyPredFloat is segApplyPred's REAL arm: the same batch loop over the same
// 8-byte block, for a PhysFloat64 column against a REAL bound. Reports whether it
// handled the predicate.
//
// FLOAT COLUMN AND FLOAT BOUND ONLY, and that pairing is enforced by construction
// rather than by a check -- Float64Column answers only for a PhysFloat64 block, so
// an INTEGER column with a REAL bound finds nothing here and falls to the general
// path. That is deliberate: mixed INTEGER/REAL must compare EXACTLY the way
// compareIntFloat does for sqlite3IntFloatCompare (value_compare.go), because
// converting either side rounds at |i| >= 2^53 and makes two different int64s
// compare equal to one stored REAL.
//
// NaN needs no special case: Go's comparisons are all false for it, which is what
// SQLite answers, and Float64Column refuses a column carrying NULLs or exceptions.
func segApplyPredFloat(s *segment, p segPred, lo int, live []uint8) bool {
	if p.Val.Typ != Float {
		return false
	}
	full, ok := s.Float64Column(p.Col)
	if !ok {
		return false
	}
	col := full[lo : lo+len(live)]
	bound := p.Val.F
	switch p.Op {
	case segGT:
		for i, x := range col {
			live[i] &= b2flag(x > bound)
		}
	case segGE:
		for i, x := range col {
			live[i] &= b2flag(x >= bound)
		}
	case segLT:
		for i, x := range col {
			live[i] &= b2flag(x < bound)
		}
	case segLE:
		for i, x := range col {
			live[i] &= b2flag(x <= bound)
		}
	case segEQ:
		for i, x := range col {
			live[i] &= b2flag(x == bound)
		}
	case segNE:
		for i, x := range col {
			live[i] &= b2flag(x != bound)
		}
	default:
		return false
	}
	return true
}

// segApplyPredBytes is segApplyPred's TEXT/BLOB arm. It reads the packed cells
// directly and compares heap slices, where the general arm below goes through
// segment.Value -- which builds a 48-byte Value and re-checks the NULL bitmap and
// the exception map for EVERY cell, both of which segPredColsFilterable has
// already proven empty for this column.
//
// BINARY, and only BINARY. bytes.Compare IS SQLite's BINARY collation over UTF-8
// or over a BLOB; segPeepholeCompareOK refuses a NOCASE or RTRIM comparison at
// recognition time, and segFilterCountTable refuses a TEXT bound unless the
// database is UTF-8.
//
// THE CLASSES MUST MATCH. SQLite orders TEXT below BLOB, so a TEXT column against
// a BLOB bound answers the same way for every row -- correct, but it is a
// cross-class question and not this loop's; the general arm has compareValues for
// it.
//
// An out-of-range cell is treated as NULL and excludes the row, which is exactly
// what segment.Value does with one, so the two arms cannot disagree about a
// malformed segment.
func segApplyPredBytes(s *segment, p segPred, lo int, live []uint8) bool {
	if p.Val.Typ != Text && p.Val.Typ != Blob {
		return false
	}
	cells, heap, ok := s.BytesColumn(p.Col)
	if !ok {
		return false
	}
	if (p.Val.Typ == Text) != (s.cols[p.Col].phys == PhysText) {
		return false
	}
	bound := p.Val.S
	cs := cells[lo : lo+len(live)]
	// The prefix decides every row whose first 8 bytes differ from the bound's,
	// without reading the heap (segment_prefix.go); only a tie falls through.
	pre, havePre := s.bytesPrefixes(p.Col)
	bp := bytesPrefix8(bound)
	for i, raw := range cs {
		if live[i] == 0 {
			continue // already excluded by an earlier predicate
		}
		if havePre {
			if x := pre[lo+i]; x != bp {
				c := -1
				if x > bp {
					c = 1
				}
				live[i] = b2flag(p.Op.satisfied(c))
				continue
			}
		}
		o, n := uint32(raw), uint32(raw>>32)
		if int(o)+int(n) > len(heap) {
			live[i] = 0
			continue
		}
		live[i] = b2flag(p.Op.satisfied(bytes.Compare(heap[o:o+n], bound)))
	}
	return true
}

func segApplyPred(s *segment, p segPred, lo int, live []uint8) {
	if segApplyPredFloat(s, p, lo, live) {
		return
	}
	if segApplyPredBytes(s, p, lo, live) {
		return
	}
	if kind, _ := s.scanKind(p.Col); kind == segScanSlice && p.Val.Typ == Int {
		// Both sides are integers and the column has no NULL and no exception,
		// so the comparison is an integer comparison and nothing else can
		// intervene.
		if full, ok := s.Int64Column(p.Col); ok {
			col := full[lo : lo+len(live)]
			bound := p.Val.I
			switch p.Op {
			case segGT:
				for i, x := range col {
					live[i] &= b2flag(x > bound)
				}
			case segGE:
				for i, x := range col {
					live[i] &= b2flag(x >= bound)
				}
			case segLT:
				for i, x := range col {
					live[i] &= b2flag(x < bound)
				}
			case segLE:
				for i, x := range col {
					live[i] &= b2flag(x <= bound)
				}
			case segEQ:
				for i, x := range col {
					live[i] &= b2flag(x == bound)
				}
			default:
				for i, x := range col {
					live[i] &= b2flag(x != bound)
				}
			}
			return
		}
	}
	for i := range live {
		if live[i] == 0 {
			continue // already excluded; the general path is expensive enough to skip
		}
		v := s.Value(p.Col, lo+i)
		// A NULL on either side makes the comparison NULL, and NULL is not
		// TRUE, so the row is excluded whatever the operator is. compareValues
		// alone will NOT do this: it ORDERS a NULL below every value, so
		// "NULL < 5" and "NULL <> 5" both come back satisfied from it. That is
		// the right answer for a sort and the wrong one for a WHERE.
		if v.Typ == Null || p.Val.Typ == Null {
			live[i] = 0
			continue
		}
		live[i] = b2flag(p.Op.satisfied(compareValues(v, p.Val)))
	}
}

func b2flag(t bool) uint8 {
	if t {
		return 1
	}
	return 0
}

// segFusedCount answers the whole filter in one branchless pass when it is a
// single integer comparison against a fixed-width int64 column. Returns -1
// when it does not apply, signaling the caller to use the kernels.
// The operator is chosen outside the row loop to ensure inlining.
//
// segFusedCountFloat is its REAL arm: the single-pass count over a PhysFloat64
// block. Returns -1 when it does not apply.
func segFusedCountFloat(s *segment, preds []segPred) int {
	if len(preds) != 1 || preds[0].Val.Typ != Float {
		return -1
	}
	col, ok := s.Float64Column(preds[0].Col)
	if !ok {
		return -1
	}
	x, count := preds[0].Val.F, 0
	switch preds[0].Op {
	case segGT:
		for _, v := range col {
			if v > x {
				count++
			}
		}
	case segGE:
		for _, v := range col {
			if v >= x {
				count++
			}
		}
	case segLT:
		for _, v := range col {
			if v < x {
				count++
			}
		}
	case segLE:
		for _, v := range col {
			if v <= x {
				count++
			}
		}
	case segEQ:
		for _, v := range col {
			if v == x {
				count++
			}
		}
	case segNE:
		for _, v := range col {
			if v != x {
				count++
			}
		}
	default:
		return -1
	}
	return count
}

func segFusedCount(s *segment, preds []segPred) int {
	if n := segFusedCountFloat(s, preds); n >= 0 {
		return n
	}
	if len(preds) != 1 {
		return -1
	}
	cols := make([][]int64, 0, 2)
	for _, q := range preds {
		if q.Val.Typ != Int {
			return -1
		}
		if kind, _ := s.scanKind(q.Col); kind != segScanSlice {
			return -1
		}
		col, ok := s.Int64Column(q.Col)
		if !ok {
			return -1
		}
		cols = append(cols, col)
	}
	p := preds[0]
	col := cols[0]
	x, count := p.Val.I, 0
	switch p.Op {
	case segGT:
		for _, v := range col {
			if v > x {
				count++
			}
		}
	case segGE:
		for _, v := range col {
			if v >= x {
				count++
			}
		}
	case segLT:
		for _, v := range col {
			if v < x {
				count++
			}
		}
	case segLE:
		for _, v := range col {
			if v <= x {
				count++
			}
		}
	case segEQ:
		for _, v := range col {
			if v == x {
				count++
			}
		}
	default:
		for _, v := range col {
			if v != x {
				count++
			}
		}
	}
	return count
}
