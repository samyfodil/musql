package engine

// A LAZY row source over a table's segments.
//
// The columnar format beats a row format by orders of magnitude where a query
// reads whole COLUMNS -- a filter, a sum, a count. It lost, badly, when it was
// asked for ROWS by materializing a []Value per row (measured, back when the
// alternative was a SQLite b-tree: GROUP BY 18.2ms on the b-tree, 31.1ms from
// segments, where C SQLite is 20.2ms).
//
// That measurement indicted the MATERIALIZATION, not the format. C does not
// build a row either: OP_Column decodes the column the opcode names and
// nothing else (vdbe.c:3185-3217), which is what vdbeCursor's colMask and
// rowRaw exist to reproduce. This source gives segments the same contract --
// the cursor holds (segment, row index) and readColumn fetches ONE column
// through segment.Value, which returns a slice into the mapped buffer without
// copying it.
//
// It is deliberately a separate stream kind beside the row store's cursor
// rather than a mode of it, so the eligibility test below is the only thing
// that decides between them and neither can silently become the other.

// segLazyRows gates the whole path. ON by default; SetSegLazyRowsForTest turns
// it off, because a fast path that cannot be switched off cannot be bisected.
//
// It shipped off, because the measurement it exists to win had gone the other
// way once already. What turned it on, in order:
//
//   - It was FASTER on every shape it takes: 27x to 42x against the b-tree on
//     filtered row scans, 1.6x on an unfiltered one-column scan.
//   - It cannot be slower on the shapes it does not take. segRowSourceWins
//     declines a wide unfiltered projection, which is the measured crossover.
//   - The corpus agrees, through this path, with the cost rule BYPASSED: 67,800
//     statements, wrong=0 panics=0, with the cost rule forced (ForceSegRowSourceForTest). That is the
//     only gate that can observe this flag at all; the -short harness never
//     attaches segments, so it is blind to the change either way.
//
// ForceSegRowSourceForTest additionally skips segRowSourceWins, so EVERY eligible
// scan takes the lazy source whether or not that is the faster choice. That is
// the strongest correctness setting and the one a conformance run wants: with
// the cost rule active, a wide unfiltered projection goes to the row store and
// the lazy path's own code stops being exercised for it -- the coverage narrows
// exactly where the ADD COLUMN width rule lives, which is the one bug on this
// path that produced a wrong VALUE rather than an error.
var segLazyRows = true

// SetSegLazyRowsForTest flips the gate and returns a restore func, so a
// benchmark can measure the same query both ways in one process.
func SetSegLazyRowsForTest(on bool) func() {
	prev := segLazyRows
	segLazyRows = on
	return func() { segLazyRows = prev }
}

// segRowSourceForced skips segRowSourceWins' cost decision.
var segRowSourceForced bool

// ForceSegRowSourceForTest makes every eligible scan take the lazy row source
// regardless of whether it is the faster choice, and returns a restore func.
//
// It exists so a CORRECTNESS test can reach the path on a shape the cost rule
// sends to the b-tree -- the ADD COLUMN width rule, for one, whose whole point
// is a wide projection with no filter. Without it that test would pass by
// testing the b-tree, which is the "gate that does not reach the change" trap.
func ForceSegRowSourceForTest(on bool) func() {
	prev := segRowSourceForced
	segRowSourceForced = on
	return func() { segRowSourceForced = prev }
}

// segRowsServed counts rows handed out by this source, so a test can prove it
// ran rather than quietly declining to the b-tree and passing for that reason.
var segRowsServed int64

// segRowsSelected counts rows the compiled PRE-FILTER chose, so a test can
// tell "the filter ran and skipped rows" from "the filter declined".
var segRowsSelected int64

// SegRowsServedForTest reads and clears the counter.
func SegRowsServedForTest() int64 { n := segRowsServed; segRowsServed = 0; return n }

// SegRowsSelectedForTest reads and clears the pre-filter counter.
func SegRowsSelectedForTest() int64 { n := segRowsSelected; segRowsSelected = 0; return n }

// segRowSource walks a table's segments in order, yielding one row at a time.
//
// Order is the whole contract: segment order, then row order within a segment,
// which is the order ScanTable produced them in and therefore the order an
// unsorted result, a LIMIT and an outer join loop all already depend on.
type segRowSource struct {
	segs []*segment
	si   int // segment index
	ri   int // row index within segs[si]

	// filter is the compiled pre-filter, when the scan loop's WHERE lowered
	// (segment_row_filter.go). sel holds the candidate rows of the CURRENT
	// segment, nil when the filter declined it and every row is a candidate.
	filter *segRowFilter
	params []Value
	ipkCol int
	sel    []int64
	selPos int
	selOn  bool // sel is authoritative for this segment (it may legitimately be empty)

	// point is a rowid point lookup (segPointSeek): the one row pointSeg/
	// pointRow, yielded once.
	point     bool
	pointDone bool
	pointSeg  *segment
	pointRow  int

	// cols is the per-column work segColumn would otherwise redo on every row.
	// All four facts are properties of the TABLE, not of the row, and deriving
	// them per read meant two loads out of a columnInfo slice per column per
	// row -- measured as more time in segColumn's own body than in the
	// segment.Value it wraps.
	cols []segColPlan
}

// segColPlan is one column's precomputed read rule.
type segColPlan struct {
	isIPK  bool  // substitute the rowid when the stored slot is NULL
	isReal bool  // affReal: re-read a stored integer as a float
	hasDef bool  // the column has a usable ALTER TABLE ADD COLUMN default
	def    Value // that default, already through the column's affinity
}

// newSegRowSource opens the segments backing rootPage, or reports nil when
// this table has none, when the gate is off, or when any segment's shape does
// not match the table the cursor is reading.
//
// The shape check is conservative on purpose. A segment narrower than the
// column list would need padStoredRow's ADD COLUMN default handling, and a
// generated column is computed FROM the other columns and so cannot be served
// one column at a time. Both decline to the b-tree rather than grow a second
// copy of rules that live in query.go.
func (p *ReadOnlyPager) newSegRowSource(cur *vdbeCursor) *segRowSource {
	if !segLazyRows || p == nil || p.segs == nil || cur == nil || cur.tbl == nil {
		return nil
	}
	// A LIVE ROW STORE WINS OUTRIGHT: its rows are already in memory, so reading
	// them back out of column blocks cannot be cheaper than looking them up, and
	// segRowSourceWins' cost rule does not know that -- it was calibrated when the
	// alternative was a b-tree over SQLite pages, which had to decode a record per
	// row. Measured when the committed blocks first became visible to a held
	// session: "ORDER BY v DESC LIMIT 20" went from 502 allocations per query to
	// 114,626 and from 9.34ms to 41.97ms, a 4.5x LOSS, because this source started
	// serving a scan whose rows the session was already holding.
	if cur.tbl.withoutRowid || cur.hasGeneratedCols() {
		return nil
	}
	if p.segs.isLive(cur.tbl.root) {
		// A write session's store over segments that the session has not
		// written to holds no rows of its own: its rows ARE its base's
		// segments, read on demand, and the alternative to this source builds
		// and normalizes a copy of every one. segRowSourceWins' cost rule was
		// measured against decoding one record per row, which this is not, so
		// it does not apply here.
		segs, ok := p.segs.liveBaseSegments(cur.tbl.root)
		if !ok {
			return nil
		}
		for _, s := range segs {
			if s == nil || len(s.cols) != len(cur.tbl.cols) {
				return nil
			}
		}
		return &segRowSource{segs: segs, cols: segColPlansFor(cur.tbl)}
	}
	if !segRowSourceWins(cur) {
		return nil
	}
	// A row-major delta beside the segments makes a per-COLUMN read wrong: this
	// source reads each column's block directly and so cannot see the log, which
	// is exactly why the aggregate fast paths decline too (segCleanFor). Declining
	// here falls back to the materialized scan, which MERGES.
	//
	// Missing this was the whole of the columnar-mode failure: with
	// with the cost rule forced this source serves every scan, so the merged read
	// returned the segments' 8 rows and silently dropped the delta's 9th. The
	// six aggregate paths were gated and this one was not.
	if !p.segCleanFor(cur.tbl.root) {
		return nil
	}
	segs, ok := p.segs.byRoot[cur.tbl.root]
	if !ok || len(segs) == 0 {
		return nil
	}
	for _, s := range segs {
		if s == nil || len(s.cols) != len(cur.tbl.cols) {
			return nil
		}
	}
	return &segRowSource{segs: segs, cols: segColPlansFor(cur.tbl)}
}

// segColPlansFor is tbl's per-column read rules, built once per table and
// cached on it: a point lookup inside a correlated subquery opens a source
// once per outer row.
func segColPlansFor(tbl *resolvedTable) []segColPlan {
	if p := tbl.segPlans.Load(); p != nil {
		return *p
	}
	plans := make([]segColPlan, len(tbl.cols))
	for i, c := range tbl.cols {
		plans[i] = segColPlan{
			isIPK:  i == tbl.ipkIndex,
			isReal: c.Aff == affReal,
			hasDef: c.HasDefault && c.DefaultKnown,
		}
		if plans[i].hasDef {
			// sqlite3ColumnDefault reads the default through the column's
			// affinity (update.c:70-74). Done once here rather than per read.
			plans[i].def = applyAffinityToValue(c.DefaultValue, c.Aff)
		}
	}
	tbl.segPlans.Store(&plans)
	return plans
}

// segPointSeek positions a lazy source on the one row with rowid rid, for a
// rowid point lookup, instead of building, normalizing and wrapping that row
// as a one-row table. served is false when the segments are not the whole
// answer for this table -- a row this session wrote, rows pending in the log,
// a generated column, a segment of the wrong width -- and the caller then does
// what it always did. A nil src with served true is a miss: no such row. The
// source is built in into, which the cursor owns, so a seek allocates nothing.
//
// The eligibility is newSegRowSource's, without its cost rule: there is no
// scan here to weigh, only one row whose columns are read as they are asked
// for, through the same segColumn every lazy read uses.
func (p *ReadOnlyPager) segPointSeek(cur *vdbeCursor, rid int64, into *segRowSource) (src *segRowSource, served bool) {
	if !segLazyRows || p == nil || p.segs == nil || cur == nil || cur.tbl == nil {
		return nil, false
	}
	if cur.tbl.withoutRowid || cur.hasGeneratedCols() {
		return nil, false
	}
	var segs []*segment
	if p.segs.isLive(cur.tbl.root) {
		// A write session's row store, which a held connection reads through.
		// rowStore.segGet's precedence, step for step: this session's own copy
		// wins (declined: it is already in memory), a dropped row is a miss,
		// and otherwise the row is the committed one -- the segments behind
		// the store's base pager, when nothing for this table is pending in
		// that pager's log.
		rows, have, err := p.segs.liveRows(cur.tbl.root)
		if err != nil || !have || rows == nil || rows.seg == nil {
			return nil, false
		}
		if _, over := rows.m[uint64(rid)]; over {
			return nil, false
		}
		if rows.baseGone || rows.gone[uint64(rid)] {
			return nil, true
		}
		base := rows.seg
		if base.rp == nil || base.rp.segs == nil || base.rp.segs.isLive(base.root) || !base.rp.segCleanFor(base.root) {
			return nil, false
		}
		segs = base.rp.segs.byRoot[base.root]
	} else {
		if !p.segCleanFor(cur.tbl.root) {
			return nil, false
		}
		segs = p.segs.byRoot[cur.tbl.root]
	}
	if len(segs) == 0 {
		return nil, false
	}
	for _, s := range segs {
		if s == nil {
			return nil, false
		}
		i, hit := s.findRowid(rid)
		if !hit {
			continue
		}
		if len(s.cols) != len(cur.tbl.cols) {
			return nil, false
		}
		*into = segRowSource{point: true, pointSeg: s, pointRow: i, cols: segColPlansFor(cur.tbl)}
		return into, true
	}
	return nil, true
}

// segRowSourceWins decides whether a per-column read beats one whole-row read
// for THIS scan, which is not always: there is a measured crossover.
//
// Reading one column at a time wins because it skips the columns nothing asked
// for, and because a compiled pre-filter can skip whole ROWS. A scan with
// neither -- every column wanted, nothing filtered -- has nothing to skip, and
// then N per-column reads lose to the single record decode they replaced.
//
// Measured, 200k rows, 30 iterations, when the whole-row read was a SQLite
// b-tree record decode (not re-measured against the row store since):
//
//	SELECT id FROM t                    24.8ms -> 15.4ms   1.61x faster
//	SELECT * FROM t WHERE v > ?         38.6ms -> 31.6ms   1.22x faster
//	SELECT id, k, v, r, s, n FROM t     52.9ms -> 55.8ms   1.05x SLOWER
//	SELECT * FROM t                     50.7ms -> 58.6ms   1.16x SLOWER
//
// So: take the lazy source when a pre-filter will skip rows, or when the scan
// wants at most half the table's columns. The two losing shapes above are
// exactly the ones that fail both tests.
func segRowSourceWins(cur *vdbeCursor) bool {
	if segRowSourceForced {
		return true
	}
	if cur.segFilter != nil {
		return true // rows to skip, which is worth more than any per-column cost
	}
	n := len(cur.tbl.cols)
	if n == 0 || cur.colMask == allColumns {
		return false
	}
	wanted := 0
	for i := 0; i < n; i++ {
		if cur.colMask.has(i) {
			wanted++
		}
	}
	return wanted*2 <= n
}

// next advances to the next row, or reports false at the end of the last
// segment.
func (src *segRowSource) next() (s *segment, row int, rowid uint64, ok bool) {
	if src.point {
		if src.pointDone {
			return nil, 0, 0, false
		}
		src.pointDone = true
		segRowsServed++
		return src.pointSeg, src.pointRow, src.pointSeg.Rowid(src.pointRow), true
	}
	for src.si < len(src.segs) {
		s = src.segs[src.si]
		if src.filter != nil && !src.selOn {
			// First visit to this segment: ask the compiled predicate which
			// rows can match. A decline leaves sel nil, which scans it whole.
			src.sel, _ = segSelectRows(s, src.filter, src.ipkCol, src.params, src.sel[:0])
			src.selOn = true
			src.selPos = 0
		}
		if src.selOn && src.sel != nil {
			if src.selPos < len(src.sel) {
				row = int(src.sel[src.selPos])
				src.selPos++
				segRowsServed++
				segRowsSelected++
				return s, row, s.Rowid(row), true
			}
		} else if src.ri < s.nRows {
			row = src.ri
			src.ri++
			segRowsServed++
			return s, row, s.Rowid(row), true
		}
		src.si++
		src.ri = 0
		src.selOn = false
		src.selPos = 0
	}
	return nil, 0, 0, false
}

// segColumn is readColumn's segment arm: column c of the cursor's current row,
// with the two fix-ups normalizeRowInto (query.go) applies to every
// scanned row, done one column at a time rather than over a whole slice.
//
// Both are per-column by construction, which is why this path is possible at
// all: the INTEGER PRIMARY KEY alias substitutes the rowid into its own column
// when the stored slot is NULL, and a REAL-affinity column re-reads a stored
// integer as a float. The third fix-up -- generated columns -- is not
// per-column, and newSegRowSource refuses a table that has any.
func (cur *vdbeCursor) segColumn(c int) Value {
	if c < 0 || c >= len(cur.segSrcCols) {
		return Value{Typ: Null}
	}
	plan := &cur.segSrcCols[c]
	var v Value
	if c < cur.segWidth {
		v = cur.segCur.Value(c, cur.segRow)
	} else {
		// PAST THE ROW'S STORED WIDTH: this row was written before an ALTER
		// TABLE ADD COLUMN, so the column does not exist in it and reads its
		// DEFAULT rather than NULL. padStoredRow (query.go) is the whole
		// rule, one column at a time, and sqlite3ColumnDefault reads the
		// default through the column's affinity (update.c:70-74).
		//
		// A segment PRESERVES the original width (s.Width) precisely so this
		// stays distinguishable; reading every column out of the block instead
		// would answer NULL for a column whose default is 5, which is a wrong
		// value rather than a missing row. It cost three rows and a wrong
		// answer in TestSelfRowExprCheckOnAddColumnBackfill before this arm
		// existed.
		if plan.hasDef {
			v = plan.def
		}
	}
	if plan.isIPK && v.Typ == Null {
		return Value{Typ: Int, I: int64(cur.rowid)}
	}
	if plan.isReal && v.Typ == Int {
		return Value{Typ: Float, F: float64(v.I)}
	}
	return v
}

// segFullRow builds the whole row, for the readers that consume one at once
// (gatherCursorRow and friends). It is the path this design exists to AVOID,
// so it allocates once per cursor and refills rather than per row.
func (cur *vdbeCursor) segFullRow() []Value {
	if cap(cur.rowVals) < len(cur.tbl.cols) {
		cur.rowVals = make([]Value, len(cur.tbl.cols))
	}
	cur.rowVals = cur.rowVals[:len(cur.tbl.cols)]
	for c := range cur.rowVals {
		cur.rowVals[c] = cur.segColumn(c)
	}
	return cur.rowVals
}

// liveBaseSegments is the segments behind a write session's row store for
// rootPage when they are the whole of it: the store holds no row of its own
// and has dropped none, and its base pager has nothing for the table pending in
// its log. ok is false otherwise.
func (src *segSource) liveBaseSegments(rootPage uint32) (segs []*segment, ok bool) {
	rows, have, err := src.liveRows(rootPage)
	if err != nil || !have || rows == nil || rows.seg == nil {
		return nil, false
	}
	if len(rows.m) != 0 || len(rows.gone) != 0 || rows.baseGone {
		return nil, false
	}
	base := rows.seg
	if base.rp == nil || base.rp.segs == nil || base.rp.segs.isLive(base.root) || !base.rp.segCleanFor(base.root) {
		return nil, false
	}
	segs = base.rp.segs.byRoot[base.root]
	return segs, len(segs) > 0
}
