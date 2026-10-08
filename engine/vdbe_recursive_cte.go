// This file compiles a RECURSIVE CTE into a bytecode program and runs its queue.
// fixed-point loop instead, compiling the recursive arm AGAIN for every popped
// row (execSelect per row) -- 58% of bigsort.test's 300,000-row INSERT was that
// runtime compile -- and it had three divergences of its own: a negative LIMIT
// returned no rows (a wrong answer, where vdbe.c:7791 makes it unbounded), a
// LIMIT 0 still ran the setup query (select.c:2541 jumps past it), and the
// step after the LIMIT was reached was still expanded (select.c:2812 tests the
// LIMIT first).
//
// Here the setup query and every recursive arm are compiled ONCE, when the
// statement that references the CTE is compiled, into ordinary sub-Programs;
// the queue program sequences them with the OpRecQueue* opcodes. Nothing in the
// runtime state holds an AST node: recQueueSpec carries integers, collation
// names and plain OrderTerm direction flags (whose Expr is always nil).
package engine

import (
	"container/heap"
	"encoding/binary"
	"fmt"
)

// recQueueSpec is OpRecQueueOpen's P4: everything about one recursive CTE's
// queue that is decided at prepare time.
type recQueueSpec struct {
	name     string
	nCol     int
	unionAll bool

	// orderKeys is the CTE's ORDER BY, which selects the queue discipline
	// (select.c:2735-2738 keys the queue ephemeral on it; without one the queue
	// is a FIFO). Resolved to output-column positions and collations at compile
	// time by detectRecursiveShape/recursiveCTECollations, so the run time
	// compares Values and never reads an ORDER BY expression.
	orderKeys []recQueueOrderKey

	// dedupColls is the per-column collation a UNION recursion's iDistinct
	// table compares under (select.c:2743-2766, multiSelectCollSeq per
	// column). nil for UNION ALL.
	dedupColls []string

	// hasLimit/limit/offset are the LIMIT and OFFSET counters
	// computeLimitRegisters (select.c:2703) loads, with the consuming
	// reference's own exact bound (cteRef.outerRowCapPlus1) folded into limit
	// the way the consumer's co-routine would have stopped pulling.
	hasLimit bool
	limit    int64
	offset   int64
}

// recQueueOrderKey is one ORDER BY term of a recursive CTE, resolved: the
// output column it reads, its direction/NULLS placement (term.Expr is always
// nil -- only Desc and Nulls are read, by orderTermLess) and its collation.
type recQueueOrderKey struct {
	col  int
	term OrderTerm
	coll string
}

// unbounded reports whether nothing but the recursion itself stops this
// queue: no LIMIT, or a negative one (vdbe.c:7788-7794's OP_DecrJumpZero never
// reaches zero from below). Only then do the safety caps apply -- see
// cteRecursionRowCap.
func (s *recQueueSpec) unbounded() bool {
	return !s.hasLimit || s.limit < 0
}

// recSelfRef is the COMPILE-TIME binding of a recursive CTE's own name while
// its recursive arms are being compiled (cteBinding.selfRef): the renamed
// output columns the arm's FROM item exposes, and the spec whose current row
// that item reads at run time.
//
// expansionDepth is len(ReadOnlyPager.cteExpansion) at the point the arms are
// compiled. A reference reached from any deeper CTE expansion -- a sibling CTE's
// body naming this one -- is not the recursive step's own FROM item, so
// resolveCTESource refuses the binding there and lets the circularity guard
// answer, as it did before.
type recSelfRef struct {
	cols           []columnInfo
	spec           *recQueueSpec
	expansionDepth int
}

// recCurrentRow is the row a recursive step reads through its self-reference:
// SQLite's iCurrent pseudo-cursor over regCurrent. It travels from the queue
// machine to every machine the step runs on (execOuterTrig, execWithParent),
// exactly as a trigger's NEW/OLD row does, and it names the spec it belongs to
// so a machine can never hand one CTE's row to another CTE's reference.
type recCurrentRow struct {
	// openRecursiveSelf's reused cursor and its row.
	selfCur  *vdbeCursor
	selfBuf  []Value
	selfRows [][]Value
	selfRids []uint64

	spec *recQueueSpec
	row  []Value
}

// compileRecursiveCTE compiles reference name/b (a recursive CTE, b.recursive
// != nil) into its queue program. cols is the CTE's output schema
// (cteSchemaCols); outerCapPlus1 is the consuming reference's own exact row
// bound, 0 for none.
//
// The layout is generateWithRecursiveQuery's, instruction for instruction:
//
//	      [Goto BRK]                    LIMIT 0 (select.c:2541-2542)
//	      RecQueueOpen  spec            select.c:2733-2766
//	      RecQueueFill  0 setup         select.c:2791
//	TOP:  RecQueuePop   BRK regCur      select.c:2796-2805
//	      RecQueueOffset CONT           select.c:2809 (codeOffset)
//	      ResultRow     regCur nCol     select.c:2810 (selectInnerLoop)
//	      [RecQueueLimit BRK]           select.c:2812-2815
//	CONT: RecQueueFill  1 arm_i         select.c:2825, one per recursive arm
//	      Goto TOP                      select.c:2828
//	BRK:  Halt
//
// The setup query and each arm are compiled here with no enclosing compiler,
// the scope the old runtime path ran them in (execSelect with a nil outer), and
// through the same flattening pre-passes execSelect applied to them
// (r38cFlattenCompoundArms for a compound setup query, r37cFlattenTransparent
// otherwise) and its per-statement guards, so the plan -- and with it the order
// an arm pushes its rows in, which is the order they come out of a FIFO -- is
// the one those rows were produced in before.
//
// Every recursive arm's FROM names the CTE exactly once (detectRecursiveShape);
// while it compiles, b.selfRef makes that FROM item a one-row source over the
// current row (resolveRecursiveSelfSource). SQLite compiles it the same way:
// the recursive table is bound to iCurrent (select.c:2821-2825).
func (p *ReadOnlyPager) compileRecursiveCTE(name string, b *cteBinding, cols []columnInfo, outerCapPlus1 int64) (*Program, error) {
	rs := b.recursive
	if b.multiSelfRef {
		// select.c:5786-5792 -- see cteBinding.multiSelfRef. Checked before the
		// generic circularity guard below, which would otherwise report
		// select.c:5802's separate "circular reference" for it.
		return nil, semanticf("engine: multiple references to recursive table: %s", name)
	}
	for _, seen := range p.cteExpansion {
		if seen.b == b {
			return nil, fmt.Errorf("%w: circular reference: %s", errVDBESemantic, name)
		}
	}
	p.cteExpansion = append(p.cteExpansion, cteExpansionFrame{name: name, b: b})
	defer func() { p.cteExpansion = p.cteExpansion[:len(p.cteExpansion)-1] }()
	// The body resolves in the scope where the CTE was DEFINED -- the scope
	// cteSchemaCols compiled the setup query in, and the one resolveCTERows ran
	// it in; see enterCTEBodyScope.
	defer p.enterCTEBodyScope(name, b)()

	nCol := len(cols)
	spec := &recQueueSpec{name: b.name, nCol: nCol, unionAll: rs.unionAll}
	// The CTE's per-output-column collation, which both the UNION dedup and the
	// ORDER BY queue compare under -- see recursiveCTECollations.
	var colColls []string
	if !rs.unionAll || len(rs.orderCols) > 0 {
		colColls = p.recursiveCTECollations(b, cols)
	}
	if !rs.unionAll {
		spec.dedupColls = colColls
	}
	for i, col := range rs.orderCols {
		if col >= nCol {
			continue
		}
		coll := ""
		if i < len(rs.orderColl) {
			coll = rs.orderColl[i]
		}
		if coll == "" && col < len(colColls) {
			coll = colColls[col]
		}
		spec.orderKeys = append(spec.orderKeys, recQueueOrderKey{
			col:  col,
			term: OrderTerm{Desc: rs.orderTerms[i].Desc, Nulls: rs.orderTerms[i].Nulls},
			coll: coll,
		})
	}
	if rs.limit != nil {
		spec.hasLimit, spec.limit = true, *rs.limit
	}
	if rs.offset != nil {
		spec.offset = *rs.offset
	}
	// The consuming statement's own exact bound only ever TIGHTENS: a
	// consumer that reads at most cap rows stops pulling there, which is a
	// LIMIT of cap on what the recursion emits.
	if outerCapPlus1 > 0 {
		if lim := outerCapPlus1 - 1; spec.unbounded() || spec.limit > lim {
			spec.hasLimit, spec.limit = true, lim
		}
	}

	seed, err := p.compileRecursiveCTEPart(rs.initial)
	if err != nil {
		return nil, err
	}
	savedSelf := b.selfRef
	b.selfRef = &recSelfRef{cols: cols, spec: spec, expansionDepth: len(p.cteExpansion)}
	arms := make([]*Program, 0, len(rs.recursiveArms))
	for _, arm := range rs.recursiveArms {
		prog, aerr := p.compileRecursiveCTEPart(arm)
		if aerr != nil {
			b.selfRef = savedSelf
			return nil, aerr
		}
		if len(prog.ColNames) != nCol {
			b.selfRef = savedSelf
			return nil, fmt.Errorf("%w: engine: recursive table %s: SELECTs to the left and right of UNION do not have the same number of result columns (%d vs %d)", errVDBESemantic, b.name, nCol, len(prog.ColNames))
		}
		arms = append(arms, prog)
	}
	b.selfRef = savedSelf

	c := &compiler{pager: p}
	regCur := c.allocN(nCol)
	var toBreak []int
	if spec.hasLimit && spec.limit == 0 {
		toBreak = append(toBreak, c.emit(Instruction{Op: OpGoto}))
	}
	c.emit(Instruction{Op: OpRecQueueOpen, P4: spec})
	c.emit(Instruction{Op: OpRecQueueFill, P1: 0, P4: seed})
	top := c.here()
	toBreak = append(toBreak, c.emit(Instruction{Op: OpRecQueuePop, P3: regCur}))
	toCont := c.emit(Instruction{Op: OpRecQueueOffset})
	c.emit(Instruction{Op: OpResultRow, P1: regCur, P2: nCol})
	if spec.hasLimit {
		toBreak = append(toBreak, c.emit(Instruction{Op: OpRecQueueLimit}))
	}
	c.patch(toCont, c.here())
	for _, arm := range arms {
		c.emit(Instruction{Op: OpRecQueueFill, P1: 1, P4: arm})
	}
	c.emit(Instruction{Op: OpGoto, P2: top})
	brk := c.here()
	c.emit(Instruction{Op: OpHalt})
	for _, j := range toBreak {
		c.patch(j, brk)
	}
	names := make([]string, nCol)
	for i := range cols {
		names[i] = cols[i].Name
	}
	prog := &Program{
		Insns:            c.insns,
		NReg:             c.nReg,
		NResultCol:       nCol,
		ColNames:         names,
		CTEScopeSnapshot: p.snapshotCTEScopes(),
	}
	if !recInlineOffForTest {
		recInlinePeephole(prog)
	}
	return prog, nil
}

// recInlineOffForTest keeps every recursive step a sub-program, so a test's
// reference is the path recInlinePeephole replaces.
var recInlineOffForTest bool

// compileRecursiveCTEPart compiles the setup query or one recursive arm of a
// recursive CTE. It applies what execSelect applied to the same SELECT when the
// recursion ran it at run time -- the per-statement guards and the flattening
// pre-pass -- and nothing else, so the compiled plan is that path's plan.
func (p *ReadOnlyPager) compileRecursiveCTEPart(stmt *SelectStmt) (*Program, error) {
	if err := p.validateSelectSchemaQualifiers(stmt); err != nil {
		return nil, fmt.Errorf("%w: %v", errVDBESemantic, err)
	}
	if err := p.schemaCatalogQueryGuard(stmt); err != nil {
		return nil, declineOrSemantic(err)
	}
	if err := p.fts5SegmentShadowQueryGuard(stmt); err != nil {
		return nil, declineOrSemantic(err)
	}
	switch {
	case len(stmt.Compound) > 0:
		stmt = p.r38cFlattenCompoundArms(stmt)
	case len(stmt.From) > 0:
		stmt = p.r37cFlattenTransparent(stmt)
	}
	return compileSubProgram(p, stmt, nil)
}

// recQueue is one execution's queue state for a recursive CTE: select.c's
// iQueue ephemeral (a FIFO, or a priority queue keyed on the ORDER BY), its
// iDistinct table, the LIMIT/OFFSET registers, and the current row.
type recQueue struct {
	spec *recQueueSpec
	enc  TextEncoding

	// fifo/head is the queue when there is no ORDER BY: rows are appended at
	// the tail and taken from head, and a taken slot is cleared so the row it
	// held can be collected as soon as the step that expands it is done.
	fifo [][]Value
	head int

	// pq is the queue when there is an ORDER BY. seq is select.c:1489's
	// OP_Sequence column, which makes every key unique and breaks a tie in
	// insertion order.
	pq  recPriorityQueue
	seq int64

	seen *rowKeySet // UNION's iDistinct; nil for UNION ALL

	// disk is the FIFO's tail once it has outgrown recQueueSpillBytes, as C
	// keeps its queue in an ephemeral table that spills (select.c:2740).
	disk *recQueueDisk

	limit, offset int64
	emitted       int64
	producedBytes int64
	queuedBytes   int64 // footprint of the rows now in the queue

	cur recCurrentRow
}

// residentBytes is what the queue itself holds in memory: its pending rows and
// a UNION's set of every row ever queued.
func (q *recQueue) residentBytes() int64 {
	n := q.queuedBytes
	if q.seen != nil {
		n += q.seen.bytes
	}
	return n
}

// newRecQueue builds OpRecQueueOpen's state.
func newRecQueue(spec *recQueueSpec, enc TextEncoding) *recQueue {
	q := &recQueue{spec: spec, enc: enc, limit: spec.limit, offset: spec.offset}
	q.cur.spec = spec
	q.pq.spec, q.pq.enc = spec, enc
	if !spec.unionAll {
		q.seen = newRowKeySet(spec.dedupColls, enc)
	}
	return q
}

func (q *recQueue) len() int {
	if len(q.spec.orderKeys) > 0 {
		return len(q.pq.items)
	}
	n := len(q.fifo) - q.head
	if q.disk != nil {
		n += q.disk.n
	}
	return n
}

// push queues one row the setup query or a recursive step produced. The row
// reaches the queue as a record in C (OP_MakeRecord, select.c:1316/:1481), and
// a record carries no subtype, so the subtype is cleared here -- where the old
// path cleared it when the rows were opened as a derived cursor.
func (q *recQueue) push(row []Value) {
	if q.seen != nil && !q.seen.add(row) {
		return
	}
	for i := range row {
		row[i].Subtype = 0
	}
	fp := recursiveRowFootprint(row)
	q.producedBytes += fp
	q.queuedBytes += fp
	if len(q.spec.orderKeys) > 0 {
		heap.Push(&q.pq, recPQItem{row: row, seq: q.seq})
		q.seq++
		return
	}
	// Once rows are on disk every later one follows them there, so the queue
	// stays in order: memory is its head, the file its tail.
	if q.disk == nil && q.queuedBytes > recQueueSpillBytes {
		q.disk = newRecQueueDisk() // nil when no file can be made: stays in memory
	}
	// To disk while the file holds rows (they precede this one) or memory is
	// full; back to memory once the file has drained.
	if q.disk != nil && (q.disk.n > 0 || q.queuedBytes > recQueueSpillBytes) {
		q.queuedBytes -= fp
		q.disk.push(row)
		return
	}
	q.fifo = append(q.fifo, row)
}

// pop takes the next row off the queue: the head of the FIFO, or the least
// entry of the priority queue by (ORDER BY keys, sequence).
func (q *recQueue) pop() ([]Value, bool) {
	if len(q.spec.orderKeys) > 0 {
		if len(q.pq.items) == 0 {
			return nil, false
		}
		row := heap.Pop(&q.pq).(recPQItem).row
		q.queuedBytes -= recursiveRowFootprint(row)
		return row, true
	}
	if q.head >= len(q.fifo) {
		q.fifo, q.head = q.fifo[:0], 0
		if q.disk != nil && q.disk.n > 0 {
			return q.disk.pop()
		}
		return nil, false
	}
	row := q.fifo[q.head]
	q.fifo[q.head] = nil
	q.head++
	q.queuedBytes -= recursiveRowFootprint(row)
	// Reclaim the consumed prefix once it is at least half the backing array,
	// so a long-running recursion's queue stays proportional to its frontier.
	if q.head >= 1024 && q.head*2 >= len(q.fifo) {
		n := copy(q.fifo, q.fifo[q.head:])
		clear(q.fifo[n:])
		q.fifo, q.head = q.fifo[:n], 0
	}
	return row, true
}

// recPQItem is one priority-queue entry: select.c:1471-1490's key record of
// the ORDER BY terms plus the OP_Sequence column, carrying the row itself.
type recPQItem struct {
	row []Value
	seq int64
}

// recPriorityQueue is a container/heap over recPQItem, ordered by the ORDER BY
// keys under their collations and then by sequence.
type recPriorityQueue struct {
	items []recPQItem
	spec  *recQueueSpec
	enc   TextEncoding
}

func (h *recPriorityQueue) Len() int { return len(h.items) }

func (h *recPriorityQueue) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	for _, k := range h.spec.orderKeys {
		if k.col >= len(a.row) || k.col >= len(b.row) {
			continue
		}
		less, equal := orderTermLess(k.term, a.row[k.col], b.row[k.col], k.coll, h.enc)
		if !equal {
			return less
		}
	}
	return a.seq < b.seq
}

func (h *recPriorityQueue) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *recPriorityQueue) Push(x any) { h.items = append(h.items, x.(recPQItem)) }

func (h *recPriorityQueue) Pop() any {
	n := len(h.items) - 1
	it := h.items[n]
	h.items[n] = recPQItem{}
	h.items = h.items[:n]
	return it
}

// recQueueFill is OpRecQueueFill: run the setup query (P1 == 0) or one
// recursive step (P1 == 1) and queue what it returns.
//
// The caps are checked after a recursive step and only for an unbounded queue
// -- the placement and the messages the Go loop this replaces had. Which caps
// apply depends on whether the rows end in an armed bulk append
// (streamSink.bounded); see cteRecursionRowCap for what each bounds.
func (m *vdbe) recQueueFill(op *Instruction) error {
	q := m.recq
	prog, ok := op.P4.(*Program)
	if q == nil || !ok || prog == nil {
		return fmt.Errorf("vdbe: recursive CTE queue step with no open queue")
	}
	rows, err := prog.execTrig(m.pager, m.params, m)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if len(r) != q.spec.nCol {
			return fmt.Errorf("engine: internal: recursive CTE %s row has %d values, expected %d", q.spec.name, len(r), q.spec.nCol)
		}
		q.push(r)
	}
	if op.P1 == 0 {
		return nil
	}
	return m.recQueueCaps(q)
}

// recQueueCaps is the check after a recursive step: for an unbounded queue,
// the caps cteRecursionRowCap and friends set, chosen by whether the rows end
// in a bulk append. OpRecQueueFill runs it after a step's sub-program, and
// OpRecQueueCheck after an inlined step (rec_inline.go).
func (m *vdbe) recQueueCaps(q *recQueue) error {
	if !q.spec.unbounded() {
		return nil
	}
	if m.sink != nil && m.sink.bounded {
		// The rows leave memory as the INSERT they feed stores them
		// (streamSink.bounded, row_store_spill.go): memory is bounded by what
		// the queue itself holds, and the content produced only by time and
		// disk. See cteRecursionStreamByteCap.
		if q.residentBytes() > cteRecursionByteCap {
			return fmt.Errorf("engine: unsupported: recursive CTE %s holds more than %d bytes of pending rows (this engine keeps a recursive CTE's queue in memory, where C SQLite spills it to a temp file -- select.c:2740)", q.spec.name, int64(cteRecursionByteCap))
		}
		if q.producedBytes > cteRecursionStreamByteCap {
			return fmt.Errorf("engine: unsupported: recursive CTE %s produced more than %d bytes of row content with nothing bounding it", q.spec.name, int64(cteRecursionStreamByteCap))
		}
		// A row cap of its own: these rows are not held in memory, so it can
		// be far above the in-memory one, but a runaway still has to stop
		// in seconds (cteRecursionStreamRowCap).
		if q.emitted+int64(q.len()) > cteRecursionStreamRowCap {
			return fmt.Errorf("engine: unsupported: recursive CTE %s exceeded this engine's internal row limit of %d rows streamed into an INSERT (possible non-terminating recursion)", q.spec.name, int64(cteRecursionStreamRowCap))
		}
		return nil
	}
	if q.producedBytes > cteRecursionByteCap {
		return fmt.Errorf("engine: unsupported: recursive CTE %s produced more than %d bytes of row content with nothing bounding it (this engine materializes a recursive CTE and everything that consumes it in memory, where C SQLite spills its queue to a temp file -- select.c:2740)", q.spec.name, int64(cteRecursionByteCap))
	}
	if q.emitted+int64(q.len()) > cteRecursionRowCap {
		return fmt.Errorf("engine: unsupported: recursive CTE %s exceeded this engine's internal row limit (possible non-terminating recursion relying on an outer LIMIT, which this engine's eager evaluation cannot short-circuit)", q.spec.name)
	}
	return nil
}

// recQueuePop is OpRecQueuePop: it reports false (jump to P2) when the queue is
// empty, and otherwise copies the next row into registers P3.. and makes it the
// current row every recursive step reads.
func (m *vdbe) recQueuePop(op *Instruction) (bool, error) {
	q := m.recq
	if q == nil {
		return false, fmt.Errorf("vdbe: recursive CTE queue read with no open queue")
	}
	row, ok := q.pop()
	if q.disk != nil && q.disk.err != nil {
		// A row that could not be written or read back is a row the answer
		// needs: an error, never a shorter result.
		return false, fmt.Errorf("engine: recursive CTE %s queue spill: %w", q.spec.name, q.disk.err)
	}
	if !ok {
		return false, nil
	}
	if op.P3 < 0 || op.P3+len(row) > len(m.regs) {
		return false, fmt.Errorf("vdbe: recursive CTE %s row register %d out of range", q.spec.name, op.P3)
	}
	copy(m.regs[op.P3:], row)
	q.cur.row = row
	m.recCur = &q.cur
	return true, nil
}

// recQueueOffset is OpRecQueueOffset: OP_IfPos on the OFFSET counter
// (select.c:883-887). A counter that is still positive is decremented and the
// row is skipped -- not output -- while its expansion still runs.
func (m *vdbe) recQueueOffset() (bool, error) {
	if m.recq == nil {
		return false, fmt.Errorf("vdbe: recursive CTE offset with no open queue")
	}
	if m.recq.offset > 0 {
		m.recq.offset--
		return true, nil
	}
	m.recq.emitted++
	return false, nil
}

// recQueueLimit is OpRecQueueLimit: OP_DecrJumpZero (vdbe.c:7788-7794), which
// decrements unless the counter is already the smallest int64 and jumps only
// when the result is exactly zero.
func (m *vdbe) recQueueLimit() (bool, error) {
	if m.recq == nil {
		return false, fmt.Errorf("vdbe: recursive CTE limit with no open queue")
	}
	if m.recq.limit > -1<<63 {
		m.recq.limit--
	}
	return m.recq.limit == 0, nil
}

// openRecursiveSelf opens OpOpenDerived's recSelf source: a one-row cursor over
// the current row of the recursive CTE spec names, as SQLite's OP_OpenPseudo
// over regCurrent. A machine that holds no current row for THAT spec is an
// internal error, never another CTE's row.
func (m *vdbe) openRecursiveSelf(ds *derivedSource) (*vdbeCursor, error) {
	cur := m.recCur
	if cur == nil || cur.spec != ds.recSelf || cur.row == nil {
		return nil, fmt.Errorf("vdbe: recursive CTE %s self-reference has no current row", ds.recSelf.name)
	}
	// One cursor, row buffer and backing slices, reused step after step: a
	// recursive select may name its CTE only once, so at most one of these is
	// live at a time, and opening a fresh cursor per step was a quarter of
	// every byte a 100k-row recursive INSERT allocated. The struct is
	// rebuilt whole, so nothing of the previous step survives in it.
	cur.selfBuf = append(cur.selfBuf[:0], cur.row...)
	clearDerivedSubtypes(cur.selfBuf, nil) // openDerivedCursor's rule
	if cur.selfCur == nil {
		cur.selfCur, cur.selfRows, cur.selfRids = new(vdbeCursor), make([][]Value, 1), make([]uint64, 1)
	}
	cur.selfRows[0], cur.selfRids[0] = cur.selfBuf, 0
	*cur.selfCur = vdbeCursor{tbl: ds.tbl, rows: cur.selfRows, rowids: cur.selfRids, materialized: true, colMask: allColumns}
	return cur.selfCur, nil
}

// recQueueSpilled counts rows written to a queue file, for tests.
var recQueueSpilled int64

// recQueueSpillBytes is how much of a FIFO queue stays in memory before its
// tail goes to disk. A variable so a test can force the spill.
var recQueueSpillBytes int64 = 64 << 20

// recQueueDisk is a FIFO queue's tail in a temp file: records, each after its
// length as a uvarint, written and read through 1MB buffers in order.
type recQueueDisk struct {
	f    *spillFile
	wbuf []byte // written next, at f.at
	rbuf []byte // read from file offset rat, consumed up to rpos
	rat  int64
	rpos int
	n    int // rows on disk or in wbuf, not yet popped
	err  error
	rec  []byte // one record being encoded, reused
}

func newRecQueueDisk() *recQueueDisk {
	f, err := newSpillFile()
	if err != nil {
		return nil
	}
	return &recQueueDisk{f: f}
}

func (d *recQueueDisk) push(row []Value) {
	if d.err != nil {
		return
	}
	d.rec = appendRecord(d.rec[:0], row)
	rec := d.rec
	recQueueSpilled++
	var lenBuf [binary.MaxVarintLen64]byte
	d.wbuf = append(d.wbuf, lenBuf[:binary.PutUvarint(lenBuf[:], uint64(len(rec)))]...)
	d.wbuf = append(d.wbuf, rec...)
	d.n++
	if len(d.wbuf) >= 1<<20 {
		d.flush()
	}
}

func (d *recQueueDisk) flush() {
	if len(d.wbuf) == 0 || d.err != nil {
		return
	}
	if _, err := d.f.f.WriteAt(d.wbuf, d.f.at); err != nil {
		d.err = err
		return
	}
	d.f.at += int64(len(d.wbuf))
	d.wbuf = d.wbuf[:0]
}

// pop reads the next record back. Rows still in wbuf are flushed first, so
// the file holds every row in order.
func (d *recQueueDisk) pop() ([]Value, bool) {
	if d.err != nil || d.n == 0 {
		return nil, false
	}
	for {
		if l, k := binary.Uvarint(d.rbuf[d.rpos:]); k > 0 && d.rpos+k+int(l) <= len(d.rbuf) {
			// decodeRecord's TEXT and BLOB values point into the bytes it is
			// given, and rbuf is overwritten by the next refill: the row gets
			// its own copy, one allocation that owns all of its payload.
			rec := append([]byte(nil), d.rbuf[d.rpos+k:d.rpos+k+int(l)]...)
			row, err := decodeRecord(rec)
			if err != nil {
				d.err = err
				return nil, false
			}
			d.rpos += k + int(l)
			d.n--
			if d.n == 0 {
				// Drained: start the file over rather than let it grow.
				d.f.at, d.rat, d.rbuf, d.rpos = 0, 0, d.rbuf[:0], 0
			}
			return row, true
		}
		// Not a whole record buffered: refill from the file.
		d.flush()
		if d.err != nil {
			return nil, false
		}
		d.rat += int64(d.rpos)
		rest := copy(d.rbuf, d.rbuf[d.rpos:])
		d.rbuf, d.rpos = d.rbuf[:rest], 0
		// 1MB, grown only when one record does not fit in what is left.
		if cap(d.rbuf) < 1<<20 {
			d.rbuf = append(make([]byte, 0, 1<<20), d.rbuf...)
		} else if len(d.rbuf) == cap(d.rbuf) {
			d.rbuf = append(make([]byte, 0, 2*cap(d.rbuf)), d.rbuf...)
		}
		from := d.rat + int64(len(d.rbuf))
		if from >= d.f.at {
			d.err = fmt.Errorf("queue file ends with %d rows unread", d.n)
			return nil, false
		}
		k, err := d.f.f.ReadAt(d.rbuf[len(d.rbuf):cap(d.rbuf)], from)
		if k == 0 && err != nil {
			d.err = err
			return nil, false
		}
		d.rbuf = d.rbuf[:len(d.rbuf)+k]
	}
}
