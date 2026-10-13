package engine

// A vec1 table read: vec1FilterMethod and what it calls -- a full scan, a
// rowid lookup, or a nearest-neighbour search (vec1SetupKANNQuery,
// vec1DoKANNQuery, vec1DoKANNBucket, vec1ScanBlocked, vec1StreamingNext) over
// the shadow tables, or, for a table with no index, the SQL statement vec1
// runs instead (vec1QueryToSql). The rows are materialized whole, as every
// virtual table's are here; a streaming query's are produced until vec1 has
// none left, which the statement then reads a prefix of.

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/samyfodil/musql/internal/mmapfile"
)

// vec1RowSource is a vec1 table's columns in this engine's layout (the rowid
// slot first): one row of the cursor.
type vec1Out struct {
	rows   [][]Value
	rowids []int64
}

// materializeVec1 is the read path of a vec1 FROM item.
func (p *ReadOnlyPager) materializeVec1(it FromItem, cols []columnInfo, st *vec1Store, params []Value, outer *evalCtx, trig *trigCompileCtx, from *vdbe, corr []vtabCorrValue) ([]columnInfo, [][]Value, []int64, error) {
	name := it.Table
	scope := it.Alias
	if scope == "" {
		scope = it.Table
	}
	if len(it.TableFuncArgs) > 3 {
		return nil, nil, nil, fmt.Errorf("engine: too many arguments on %s() - max 3", name)
	}
	if len(it.TableFuncArgs) == 3 {
		return nil, nil, nil, fmt.Errorf("%w: vec1: a third table-valued-function argument (a constraint on distance)", errVDBEUnsupported)
	}
	ps := vec1BestIndex(it, cols, scope)
	if ps.declineError != nil {
		return nil, nil, nil, ps.declineError
	}
	// The conjuncts the compiler left out of the WHERE must be the ones this
	// plan consumes, or a filter would be dropped or applied twice.
	if !slices.Equal(slices.Sorted(slices.Values(ps.omit)), slices.Sorted(slices.Values(it.tvfOmitSet))) {
		return nil, nil, nil, fmt.Errorf("%w: vec1: the constraints consumed differ from those removed at compile time", errVDBEUnsupported)
	}
	hidden := hiddenColIndices(cols)
	fold := func(c vec1Cons) (Value, error) {
		if c.conj < 0 {
			if v, ok := vtabCorrArgFor(corr, c.arg, hidden[c.arg]); ok {
				return v, nil
			}
		}
		v, err := p.foldVtabInputValue(c.rhs, params, outer, trig, from)
		if err != nil {
			return Value{}, fmt.Errorf("%w: vec1: a constraint value could not be computed where the table is read: %w", errVDBEUnsupported, err)
		}
		return v, nil
	}

	c := p.vec1Conn()
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tab("main", name)
	sh := p.vec1Shadow(name)
	q := &vec1Read{p: p, t: t, sh: sh, st: st, name: name, conn: c, stmt: it.vtabSelect, scope: scope}

	switch {
	case ps.knn:
		cmd := ps.cons[ps.cmd]
		if cmd.in != nil || (ps.arg >= 0 && ps.cons[ps.arg].in != nil) {
			return nil, nil, nil, fmt.Errorf("%w: vec1: an IN list as the query vector or parameters", errVDBEUnsupported)
		}
		qv, err := fold(cmd)
		if err != nil {
			return nil, nil, nil, err
		}
		in := &vec1KnnInput{query: qv, hasParams: ps.arg >= 0}
		if ps.arg >= 0 {
			if in.params, err = fold(ps.cons[ps.arg]); err != nil {
				return nil, nil, nil, err
			}
		}
		for k, i := range ps.meta {
			con := ps.cons[i]
			f := vec1Filter{op: ps.metaOp[k], meta: con.col - 4}
			switch f.op {
			case vec1OpIN:
				vals := make([]Value, len(con.in))
				for j, e := range con.in {
					v, err := p.foldVtabInputValue(e, params, outer, trig, from)
					if err != nil {
						return nil, nil, nil, fmt.Errorf("%w: vec1: an IN value could not be computed: %w", errVDBEUnsupported, err)
					}
					vals[j] = v
				}
				in.filters = append(in.filters, vec1InFilter{f: f, vals: vec1InValues(vals)})
			case vec1OpNOTNULL, vec1OpISNULL:
				in.filters = append(in.filters, vec1InFilter{f: f})
			default:
				v, err := fold(con)
				if err != nil {
					return nil, nil, nil, err
				}
				in.filters = append(in.filters, vec1InFilter{f: f, val: v})
			}
		}
		if ps.limit >= 0 {
			lv, err := vec1LimitValue(it.vtabSelect, params)
			if err != nil {
				return nil, nil, nil, err
			}
			in.limit, in.hasLimit = lv, true
		}
		out, err := q.knn(in)
		if err != nil {
			return nil, nil, nil, err
		}
		return cols, out.rows, out.rowids, nil
	case ps.rowid >= 0:
		v, err := fold(ps.cons[ps.rowid])
		if err != nil {
			return nil, nil, nil, err
		}
		out, err := q.lookup(v)
		if err != nil {
			return nil, nil, nil, err
		}
		return cols, out.rows, out.rowids, nil
	}
	out, err := q.scan()
	if err != nil {
		return nil, nil, nil, err
	}
	return cols, out.rows, out.rowids, nil
}

// vec1InValues is the set an IN constraint's values form: sqlite3_vtab_in
// hands them over from an ephemeral index, which drops duplicates.
func vec1InValues(vals []Value) []Value {
	var out []Value
	for _, v := range vals {
		if v.Typ == Null {
			continue
		}
		dup := false
		for _, o := range out {
			if o.Typ == v.Typ && compareValues(o, v) == 0 {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

// vec1LimitValue is the LIMIT's value as the planner offers it to xFilter.
func vec1LimitValue(stmt *SelectStmt, params []Value) (Value, error) {
	if stmt.Limit != nil {
		return Value{Typ: Int, I: *stmt.Limit}, nil
	}
	if pe, ok := stmt.LimitParam.(ParamExpr); ok {
		n, err := limitOffsetValueToCount(paramAt(params, pe.Index))
		if err != nil {
			return Value{}, err
		}
		return Value{Typ: Int, I: n}, nil
	}
	return Value{}, fmt.Errorf("%w: vec1: a LIMIT this engine cannot read", errVDBEUnsupported)
}

// vec1InFilter is one consumed metadata constraint with its value.
type vec1InFilter struct {
	f    vec1Filter
	val  Value
	vals []Value
}

// vec1KnnInput is xFilter's arguments for a nearest-neighbour query.
type vec1KnnInput struct {
	query     Value
	hasParams bool
	params    Value
	filters   []vec1InFilter
	hasLimit  bool
	limit     Value
}

// vec1Read is one read of a table: the pager's view of its shadow tables and
// the connection's state for it (vec1Tab), which a read loads as vec1's
// xFilter does.
type vec1Read struct {
	p     *ReadOnlyPager
	t     *vec1Tab
	sh    *vec1PagerShadow
	st    *vec1Store
	name  string
	conn  *Vec1Connection
	stmt  *SelectStmt
	scope string
	// sqlOut is a table with no index's answer (vec1QueryToSql).
	sqlOut *vec1Out
	// lists holds the lists this statement has read, by bucket (-1 for all
	// of them): a flat index's output rows read their vectors from the lists
	// its search has just scanned, which nothing can change in between.
	lists map[int64][]vec1IdxRow
}

// bucketLists is the shadow's bucketLists, kept for the statement.
func (q *vec1Read) bucketLists(bucket int64) ([]vec1IdxRow, error) {
	if l, ok := q.lists[bucket]; ok {
		return l, nil
	}
	var l []vec1IdxRow
	if all, ok := q.lists[-1]; ok {
		for _, r := range all {
			if r.intBucket && r.bucket == bucket {
				l = append(l, r)
			}
		}
		vec1IndexOrder(l)
	} else {
		var err error
		if l, err = q.sh.bucketLists(bucket); err != nil {
			return nil, err
		}
	}
	if q.lists == nil {
		q.lists = map[int64][]vec1IdxRow{}
	}
	q.lists[bucket] = l
	return l, nil
}

// instr adds a qprofile counter's elapsed time and count.
func (q *vec1Read) instr(i int, start time.Time) {
	q.t.counts[i]++
	q.t.cycles[i] += int64(time.Since(start))
}

// ---- the nearest-neighbour query ---------------------------------------------

// vec1Query is Vec1Query.
type vec1Query struct {
	k         int64
	nProbe    int
	slack     float64
	streaming bool
	filters   []vec1Filter
	vector    []float32 // the query, padded to the transform length
	transform []float32
	heap      *vec1Heap
	buckets   []vec1Bucket // a streaming query's buckets still to scan
	nOrigRes  int
	cur, nCur int
	// Scratch the scan reuses from bucket to bucket: a distance table (one
	// table is live at a time) and a list's block sums.
	lutBuf, sums []float32
	scratch      *vec1Scratch // where those two came from, and go back to
}

// vec1Scratch is the buffers a search reuses from query to query: it takes a
// distance table of tens of kilobytes and a list's block sums every time.
type vec1Scratch struct{ lut, sums []float32 }

var vec1ScratchPool sync.Pool

// takeScratch gives qy a distance table and sums buffer from earlier queries.
func (qy *vec1Query) takeScratch() {
	s, _ := vec1ScratchPool.Get().(*vec1Scratch)
	if s == nil {
		s = &vec1Scratch{}
	}
	qy.scratch, qy.lutBuf, qy.sums = s, s.lut, s.sums
}

// releaseScratch returns what takeScratch gave, grown as the search needed.
// Nothing the search produced aliases it: the rows it returns are copies.
func (qy *vec1Query) releaseScratch() {
	if s := qy.scratch; s != nil {
		s.lut, s.sums = qy.lutBuf, qy.sums
		qy.scratch, qy.lutBuf, qy.sums = nil, nil, nil
		vec1ScratchPool.Put(s)
	}
}

// interpretNProbe is vec1InterpretNProbe.
func vec1InterpretNProbe(nBucket int, f float64) int {
	var n int
	if f >= 1.0 {
		n = vec1TruncInt(f)
	} else {
		n = vec1TruncInt(f * float64(nBucket))
	}
	n = min(n, nBucket)
	return max(n, 1)
}

// vec1TruncInt is C's "(int)f" on x86-64: INT_MIN outside the range.
func vec1TruncInt(f float64) int {
	if f != f || f >= 2147483648.0 || f <= -2147483649.0 {
		return math.MinInt32
	}
	return int(int32(f))
}

// vec1IsIntegerValue is vecIsIntegerValue.
func vec1IsIntegerValue(v Value) bool {
	n := vec1NumericType(v)
	if n.Typ == Int {
		return true
	}
	if n.Typ == Float {
		_, ok := vec1IntOf(n.F)
		return ok
	}
	return false
}

// setup is vec1SetupKANNQuery.
func (q *vec1Read) setup(in *vec1KnnInput) (*vec1Query, error) {
	t := q.t
	nElem := t.cfg.nElem
	nSpace := max(nElem, t.tmpLen())
	raw := vec1BlobOf(in.query)
	qy := &vec1Query{k: math.MinInt64, nProbe: vec1InterpretNProbe(t.mod.nBucket(), q.conn.nProbe)}
	qy.takeScratch()
	qy.vector = make([]float32, nSpace)
	if nElem != 0 {
		if len(raw) != 4*nElem {
			return nil, fmt.Errorf("vec1: unexpected vector blob size %d bytes, expected %d", len(raw), 4*nElem)
		}
		copy(qy.vector, vec1Floats(raw))
	}
	if vec1NonFinite(raw[:len(raw)/4*4]) {
		return nil, fmt.Errorf("vec1: non-finite value not allowed in vector")
	}
	if in.hasParams {
		pv := in.params
		switch {
		case vec1IsIntegerValue(pv):
			qy.k = valueToInt64Trunc(vec1NumericType(pv))
		case pv.Typ == Text:
			err := vec1ParseJSONConfig(pv.S, false, func(o vec1JSONOpt) error {
				i, err := vec1ConfigEnum(o.key, []string{"nprobe", "K", "streaming", "nprobe_slack"}, "option")
				if err != nil {
					return err
				}
				switch i {
				case 0:
					qy.nProbe = vec1InterpretNProbe(t.mod.nBucket(), o.f)
					if o.f <= 0 {
						return fmt.Errorf("vec1: nprobe must be a numeric value greater than 0.0")
					}
				case 1:
					if o.typ == Int {
						qy.k = o.i
					} else {
						qy.k = -1
					}
				case 2:
					qy.streaming = vec1TruncInt(o.f) != 0
				case 3:
					qy.slack = o.f
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		default:
			qy.k = -1
		}
	}
	for _, f := range in.filters {
		fl := f.f
		switch fl.op {
		case vec1OpIN:
			fl.i = int64(len(f.vals))
			qy.filters = append(qy.filters, fl)
			for _, v := range f.vals {
				e := vec1Filter{op: vec1OpEQ, meta: fl.meta}
				vec1ValueToFilter(v, &e)
				qy.filters = append(qy.filters, e)
			}
			continue
		case vec1OpNOTNULL:
			fl.typ, fl.i = Blob, 0
		case vec1OpISNULL:
			fl.typ, fl.op = Null, vec1OpIS
		default:
			vec1ValueToFilter(f.val, &fl)
		}
		qy.filters = append(qy.filters, fl)
	}
	if in.hasLimit {
		lim := int64(int32(valueToInt64Trunc(in.limit)))
		if lim >= 0 && (qy.k == math.MinInt64 || qy.k > 0) {
			if qy.k < 0 {
				qy.k = lim
			} else {
				qy.k = min(qy.k, lim)
			}
		}
	}
	switch {
	case qy.k == math.MinInt64:
		return nil, fmt.Errorf("vec1: no K value or visible LIMIT clause")
	case qy.k < 0 || qy.k > vec1MaxK:
		return nil, fmt.Errorf("vec1: K must be an integer value between 0 and %d", vec1MaxK)
	}
	if t.mod.transformRequired() {
		qy.transform = make([]float32, nSpace)
	} else {
		qy.transform = qy.vector
	}
	return qy, nil
}

// knn is vec1FilterMethod's nearest-neighbour arm.
func (q *vec1Read) knn(in *vec1KnnInput) (*vec1Out, error) {
	// A search touches lists and vectors scattered over the whole file.
	q.p.segMapping().ReadAhead()
	if err := q.t.loadConfig(q.sh); err != nil {
		return nil, err
	}
	qy, err := q.filterKNN(in)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return q.sqlOut, nil
	}
	defer qy.releaseScratch()
	return q.knnRows(qy)
}

// filterKNN is xFilter's search, timed as vec1's "total" counter times it:
// the query set up and the index searched, or the SQL statement prepared for
// a table with no index (whose rows it leaves in sqlOut).
func (q *vec1Read) filterKNN(in *vec1KnnInput) (*vec1Query, error) {
	defer q.instr(vec1QTotal, time.Now())
	qy, err := q.setup(in)
	if err != nil {
		return nil, err
	}
	if !q.t.mod.indexed() {
		q.sqlOut, err = q.sqlNN(qy)
		return nil, err
	}
	return qy, q.doKANN(qy)
}

// doKANN is vec1DoKANNQuery.
func (q *vec1Read) doKANN(qy *vec1Query) error {
	t := q.t
	mod := t.mod
	start := time.Now()
	if mod.transformRequired() && mod.nElem() > 0 {
		copy(qy.transform, mod.transform(qy.transform, qy.vector))
	}
	qy.heap = newVec1Heap(int(qy.k), qy.streaming)
	q.instr(vec1QInit, start)

	start = time.Now()
	var buckets []vec1Bucket
	if mod.nBucket() > 0 {
		buckets = q.findBuckets(qy.transform, qy.nProbe)
	}
	q.instr(vec1QCoarse, start)

	var lut *vec1LUT
	probe := 0
	for ; probe < qy.nProbe; probe++ {
		bucket := -1
		if mod.nBucket() > 0 {
			bucket = buckets[probe].bucket
			if qy.slack > 0 && float64(buckets[probe].dist) > float64(buckets[0].dist)*(1.0+qy.slack) {
				break
			}
		}
		if mod.residual() || lut == nil {
			lut = &vec1LUT{}
		}
		if err := q.doBucket(qy, bucket, lut); err != nil {
			return err
		}
	}

	start = time.Now()
	nSort := min(len(qy.heap.res), int(qy.k))
	if qy.streaming {
		if n := mod.nBucket() - probe; n > 0 {
			qy.buckets = slices.Clone(buckets[probe:])
		}
		qy.nOrigRes = max(16, len(qy.heap.res))
	}
	vec1SortResults(qy.heap.res[:nSort])
	qy.nCur = nSort
	q.instr(vec1QFinalSort, start)
	return nil
}

// findBuckets is vec1FindBuckets: every centroid's float32 distance, the
// nProbe nearest selected and sorted to the front.
func (q *vec1Read) findBuckets(a []float32, nProbe int) []vec1Bucket {
	mod := q.t.mod
	n := mod.nElem()
	res := make([]vec1Bucket, mod.nBucket())
	dist := make([]float64, 8*vec1T8Blocks(mod.t8().cent, n))
	vec1T8L2D(mod.t8().cent, n, a[:n], dist)
	for i := range res {
		res[i] = vec1Bucket{bucket: i, dist: float32(dist[i])}
	}
	if nProbe < len(res) {
		vec1SelectBuckets(res, nProbe)
		vec1SortBuckets(res[:nProbe])
	}
	return res
}

// vec1LUT is the distance table a bucket's codes are scanned with; built
// once per query, or per bucket for a residual model (vec1DoKANNBucket's
// pDist). norm2 is the binary quantizer's squared residual norm, which vec1
// holds in a local of the call that built the table: a second bucket using
// the same table sees 0.
type vec1LUT struct {
	built bool
	d     []float32
}

// doBucket is vec1DoKANNBucket: every list of a bucket (all of them for -1)
// scanned into the heap.
func (q *vec1Read) doBucket(qy *vec1Query, bucket int, lut *vec1LUT) error {
	t := q.t
	mod := t.mod
	nElem := t.cfg.nElem
	bq := mod.bitQuant()
	pq := mod.nCodebook() > 0 && !bq
	var norm2 float32
	if (pq || bq) && !lut.built {
		start := time.Now()
		enc := qy.transform
		if mod.residual() {
			tmp := make([]float32, max(len(enc), t.tmpLen()))
			vec1Sub(tmp[:nElem], enc[:nElem], mod.centroid(bucket))
			enc = tmp
		}
		if pq {
			lut.d = vec1Grow(&qy.lutBuf, mod.nCodebook()*vec1CodebookSize)
			mod.pqLUT(enc, lut.d)
		} else {
			lut.d = vec1Grow(&qy.lutBuf, vec1BQSize(nElem))
			mod.bqLUT(nElem, enc, lut.d)
			norm2 = vec1Dot(enc[:nElem], enc[:nElem])
		}
		lut.built = true
		q.instr(vec1QLUT, start)
	}

	var lists []vec1IdxRow
	start := time.Now()
	if bucket < 0 {
		if all, ok := q.lists[-1]; ok {
			lists = all
		} else {
			if err := q.sh.scan("_idx", func(id int64, row []Value) (bool, error) {
				lists = append(lists, vec1IdxRowOf(id, row))
				return true, nil
			}); err != nil {
				return err
			}
			if q.lists == nil {
				q.lists = map[int64][]vec1IdxRow{}
			}
			q.lists[-1] = lists
		}
	} else {
		var err error
		if lists, err = q.bucketLists(int64(bucket)); err != nil {
			return err
		}
	}
	t.counts[vec1QIdxRead] += int64(len(lists)) + 1
	t.cycles[vec1QIdxRead] += int64(time.Since(start))

	var mask []byte
	for _, r := range lists {
		if !vec1ListSizeOK(mod, nElem, r.val) {
			return errVec1Corrupt
		}
		blob := r.val
		nEntry := int(be.Uint32(blob[4:]))
		mask = mask[:0]
		if len(qy.filters) > 0 {
			var err error
			if mask, err = q.metaFilters(qy, r.id, nEntry, mask); err != nil {
				return err
			}
		}
		start := time.Now()
		szRowid := 4
		if be.Uint32(blob)&vec1List64 != 0 {
			szRowid = 8
		}
		if pq || bq {
			nNorm := 0
			if bq {
				nNorm = 4
			}
			var m []byte
			if len(qy.filters) > 0 {
				m = mask
			}
			vec1ScanBlocked(qy.heap, m, lut.d, mod.nCodebook(), blob, szRowid, nEntry, nNorm, norm2, &qy.sums)
		} else {
			szVec := 4 * nElem
			vec := make([]float32, nElem)
			first := vec1ListHdr + nEntry*szRowid
			dist := make([]float64, nEntry)
			vec1RowDists(blob[first:first+nEntry*szVec], nEntry, nElem, qy.vector, mod.cos(), dist, vec)
			for i := range nEntry {
				if len(mask) > 0 && mask[i/8]&(1<<(i%8)) != 0 {
					continue
				}
				var rowid int64
				if szRowid == 4 {
					rowid = int64(be.Uint32(blob[vec1ListHdr+4*i:]))
					if rowid == vec1Tomb32 {
						continue
					}
				} else {
					rowid = int64(be.Uint64(blob[vec1ListHdr+8*i:]))
					if rowid == vec1Tomb64 {
						continue
					}
				}
				qy.heap.insert(rowid, dist[i])
			}
		}
		q.instr(vec1QPQScan, start)
	}
	return nil
}

// vec1ReadFloats decodes native-order float32s into dst.
func vec1ReadFloats(dst []float32, b []byte) {
	for i := range dst {
		dst[i] = math.Float32frombits(vec1Native.Uint32(b[4*i:]))
	}
}

// metaFilters is vec1DoMetaFilters: a bit set per entry some filter excludes.
func (q *vec1Read) metaFilters(qy *vec1Query, id int64, nEntry int, mask []byte) ([]byte, error) {
	n := (nEntry + 7) / 8
	mask = append(mask[:0], make([]byte, n)...)
	for i := 0; i < len(qy.filters); i++ {
		f := qy.filters[i]
		start := time.Now()
		row, ok, err := q.sh.get("_meta", id<<8+int64(f.meta))
		q.instr(vec1QMetaRead, start)
		if err != nil {
			return nil, err
		}
		v := vec1Col(row, 1)
		if !ok || len(v.S) == 0 {
			return nil, errVec1Corrupt
		}
		start = time.Now()
		if !vec1ApplyFilter(qy.filters[i:], nEntry, v.S, mask) {
			return nil, errVec1Corrupt
		}
		if f.op == vec1OpIN {
			i += int(f.i)
		}
		q.instr(vec1QMetaScan, start)
	}
	return mask, nil
}

// vec1ScanBlocked is vec1ScanBlocked: each block of 16 codes summed through
// the table, in float32, codebook by codebook; a binary code's sum turned
// into a distance by its norm. A block whose 16 partial sums are all past the
// heap's admittance threshold is left early, which only skips what could not
// be admitted.
//
// The sums come from vec1Scan16 for the whole list at once. Leaving a block
// early cannot change what is admitted when the threshold is a float32 value:
// the early test is only made for a PQ table, whose entries are sums of
// squares, so a sum only grows and a lane past the threshold early is past it
// at the end. A threshold no float32 equals is the exception (a final sum can
// fall between it and its rounding), and such a block is summed as vec1 sums
// it, early test included.
func vec1ScanBlocked(heap *vec1Heap, mask []byte, lut []float32, nCodebook int, blob []byte, szRowid, nEntry, nNorm int, r2 float32, scratch *[]float32) {
	szBlk := vec1PQBlock * (nCodebook + nNorm)
	early := nCodebook
	if nNorm == 0 && !heap.streaming {
		early = nCodebook / 4
	}
	first := vec1ListHdr + nEntry*szRowid
	nBlk := (len(blob) - first) / szBlk
	sums := vec1Grow(scratch, vec1PQBlock*nBlk)
	vec1Scan16(blob[first:], nBlk, nCodebook, nNorm, lut, sums)
	var acc [vec1PQBlock]float32
	for blk := range nBlk {
		b := blob[first+blk*szBlk : first+(blk+1)*szBlk]
		rowids := blob[vec1ListHdr+blk*vec1PQBlock*szRowid:]
		nSlot := min(vec1PQBlock, nEntry-blk*vec1PQBlock)
		if early < nCodebook && float64(float32(heap.fMin)) != heap.fMin {
			if vec1SumBlock(&acc, heap, lut, nCodebook, b, nNorm, early) {
				continue
			}
		} else {
			copy(acc[:], sums[blk*vec1PQBlock:])
		}
		if nNorm != 0 {
			for j := range nSlot {
				norm := math.Float32frombits(vec1Native.Uint32(b[4*j:]))
				a := float64(norm) - float64(2.0*float64(acc[j]))
				acc[j] = float32(float64(r2) + float64(a*float64(norm)))
			}
		}
		for i := range nSlot {
			d := acc[i]
			if float64(d) < heap.fMin && (mask == nil || mask[(blk*vec1PQBlock+i)/8]&(1<<(i%8)) == 0) {
				var rowid int64
				if szRowid == 4 {
					rowid = int64(be.Uint32(rowids[4*i:]))
					if rowid == vec1Tomb32 {
						continue
					}
				} else {
					rowid = int64(be.Uint64(rowids[8*i:]))
					if rowid == vec1Tomb64 {
						continue
					}
				}
				heap.insert(rowid, float64(d))
			}
		}
	}
}

// vec1SumBlock sums one block as vec1ScanBlocked does, codebook by codebook
// with the early test, and reports whether the block was left early.
func vec1SumBlock(acc *[vec1PQBlock]float32, heap *vec1Heap, lut []float32, nCodebook int, b []byte, nNorm, early int) bool {
	*acc = [vec1PQBlock]float32{}
	for book := range nCodebook {
		table := lut[book*vec1CodebookSize : (book+1)*vec1CodebookSize]
		codes := b[(book+nNorm)*vec1PQBlock : (book+nNorm+1)*vec1PQBlock]
		for j := range vec1PQBlock {
			acc[j] += table[codes[j]]
		}
		if book >= early && book&3 == 3 {
			fMin := float32(heap.fMin)
			past := true
			for j := range vec1PQBlock {
				if acc[j] < fMin {
					past = false
					break
				}
			}
			if past {
				return true
			}
		}
	}
	return false
}

// streamingNext is vec1StreamingNext: the rest of the heap reinserted, then
// the nearest buckets still unscanned scanned into it until it has grown by
// half the first batch, and the next batch sorted.
func (q *vec1Read) streamingNext(qy *vec1Query) error {
	h := qy.heap
	nSort := len(h.res) - qy.nCur
	rest := slices.Clone(h.res[qy.nCur:])
	h.res = h.res[:0]
	for _, r := range rest {
		h.insert(r.rowid, r.dist)
	}
	for len(qy.buckets) > 0 && nSort <= qy.nOrigRes/2 {
		best := 0
		for i := 1; i < len(qy.buckets); i++ {
			if qy.buckets[i].dist < qy.buckets[best].dist {
				best = i
			}
		}
		bucket := qy.buckets[best].bucket
		last := len(qy.buckets) - 1
		if best != last {
			qy.buckets[best] = qy.buckets[last]
		}
		qy.buckets = qy.buckets[:last]
		if err := q.doBucket(qy, bucket, &vec1LUT{}); err != nil {
			return err
		}
	}
	nSort = min(h.nMax, len(h.res))
	vec1SortResults(h.res[:nSort])
	qy.cur, qy.nCur = 0, nSort
	return nil
}

// knnRows is the cursor walked to the end (vec1NextMethod, vec1ColumnMethod),
// each result row read from %_base.
func (q *vec1Read) knnRows(qy *vec1Query) (*vec1Out, error) {
	out := &vec1Out{}
	// vec1 reads a result's %_base row only for a column it supplies, the
	// vector or a metadata column (vec1SeekCsr): a row the index still holds
	// but whose base row is gone -- deleted in this transaction, with the
	// index's change still pending -- is reported only then.
	needBase := q.needs("*")
	for i := vec1ColVector; i < len(q.st.cols) && !needBase; i++ {
		needBase = q.needs(q.st.cols[i].Name)
	}
	for {
		for qy.cur < qy.nCur {
			r := qy.heap.res[qy.cur]
			if !needBase {
				row := make([]Value, 5+q.st.nMeta)
				for i := range row {
					row[i] = Value{Typ: Null}
				}
				row[vec1ColRowid] = Value{Typ: Int, I: r.rowid}
				row[vec1ColDistance] = Value{Typ: Float, F: r.dist}
				out.rows = append(out.rows, row)
				out.rowids = append(out.rowids, r.rowid)
				qy.cur++
				continue
			}
			base, ok, err := q.sh.get("_base", r.rowid)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errVec1Corrupt
			}
			row, err := q.row(r.rowid, base, Value{Typ: Float, F: r.dist})
			if err != nil {
				return nil, err
			}
			out.rows = append(out.rows, row)
			out.rowids = append(out.rowids, r.rowid)
			qy.cur++
		}
		if !qy.streaming {
			return out, nil
		}
		if err := q.streamingNext(qy); err != nil {
			return nil, err
		}
		if qy.cur >= qy.nCur {
			return out, nil
		}
	}
}

// row is one output row in this engine's column layout: the rowid slot, cmd,
// arg, distance, the vector, the metadata.
func (q *vec1Read) row(rowid int64, base []Value, dist Value) ([]Value, error) {
	out := make([]Value, 5+q.st.nMeta)
	out[vec1ColRowid] = Value{Typ: Int, I: rowid}
	out[vec1ColCmd], out[vec1ColArg] = Value{Typ: Null}, Value{Typ: Null}
	out[vec1ColDistance] = dist
	vec, err := q.vector(rowid, base)
	if err != nil {
		return nil, err
	}
	out[vec1ColVector] = vec
	for i := range q.st.nMeta {
		out[vec1ColMeta+i] = vec1Col(base, 2+i)
	}
	return out, nil
}

// vector is vec1GetVector: %_base's vector, or for a flat index (whose
// %_base holds the bucket) the list entry vec1FindByRowid finds.
func (q *vec1Read) vector(rowid int64, base []Value) (Value, error) {
	if !q.t.mod.isFlat() {
		return vec1Col(base, 1), nil
	}
	bucket := int(int32(valueToInt64Trunc(vec1Col(base, 1))))
	nElem := q.t.cfg.nElem
	if wr := q.t.writer; wr != nil && bucket >= 0 && bucket < len(wr.bld) && wr.bld[bucket].init {
		b := wr.bld[bucket]
		for i := range b.nEntry() {
			if b.rowidAt(i) == rowid {
				return Value{Typ: Blob, S: append([]byte(nil), b.data[4*nElem*i:4*nElem*(i+1)]...)}, nil
			}
		}
	}
	lists, err := q.bucketLists(int64(bucket))
	if err != nil {
		return Value{}, err
	}
	for _, r := range lists {
		if rowid < r.first || rowid > r.last || len(r.val) < vec1ListHdr {
			continue
		}
		nEntry := int(be.Uint32(r.val[4:]))
		sz := 4
		if be.Uint32(r.val)&vec1List64 != 0 {
			sz = 8
		}
		if (sz == 4 && rowid == vec1Tomb32) || (sz == 8 && rowid == vec1Tomb64) {
			continue
		}
		for e := range nEntry {
			var got int64
			if sz == 4 {
				got = int64(be.Uint32(r.val[vec1ListHdr+4*e:]))
			} else {
				got = int64(be.Uint64(r.val[vec1ListHdr+8*e:]))
			}
			if got == rowid {
				off := vec1ListHdr + nEntry*sz + e*4*nElem
				if off+4*nElem > len(r.val) {
					return Value{}, errVec1Corrupt
				}
				return Value{Typ: Blob, S: r.val[off : off+4*nElem]}, nil
			}
		}
	}
	return Value{Typ: Null}, nil
}

// needs reports whether the statement names a column of this table by name
// anywhere -- the conservative test for whether an expensive column must be
// computed. "*" asks whether it has a star anywhere.
func (q *vec1Read) needs(col string) bool {
	if q.stmt == nil {
		return true
	}
	found := false
	var walk func(Expr)
	var walkStmt func(*SelectStmt)
	walk = func(e Expr) {
		if e == nil || found {
			return
		}
		switch x := e.(type) {
		case ColumnExpr:
			if equalFoldName(x.Name, col) {
				found = true
			}
			return
		case SubqueryExpr:
			walkStmt(x.Stmt)
			return
		case ExistsExpr:
			walkStmt(x.Stmt)
			return
		case InExpr:
			if x.Sub != nil {
				walkStmt(x.Sub)
			}
		}
		walkExprOperands(e, walk)
	}
	walkStmt = func(s *SelectStmt) {
		if s == nil || found {
			return
		}
		for _, c := range s.Columns {
			if c.Star {
				found = found || col == "*"
				continue
			}
			walk(c.Expr)
		}
		walk(s.Where)
		for _, g := range s.GroupBy {
			walk(g)
		}
		walk(s.Having)
		for _, o := range s.OrderBy {
			walk(o.Expr)
		}
		for _, f := range s.From {
			walk(f.On)
			for _, a := range f.TableFuncArgs {
				walk(a)
			}
			walkStmt(f.Subquery)
		}
		for _, arm := range s.Compound {
			walkStmt(arm.Stmt)
		}
	}
	walkStmt(q.stmt)
	return found
}

// ---- a table with no index: vec1QueryToSql -------------------------------------

// sqlNN is vec1QueryToSql: "SELECT *, distance(?, vector) AS d FROM %_base
// WHERE <filters> ORDER BY d LIMIT K", run as vec1 runs it.
func (q *vec1Read) sqlNN(qy *vec1Query) (*vec1Out, error) {
	var where strings.Builder
	var args []Value
	and := ""
	next := 1
	for i := 0; i < len(qy.filters); i++ {
		f := qy.filters[i]
		if f.op == vec1OpIN {
			fmt.Fprintf(&where, "%sc%d IN(?%d", and, f.meta, next)
			next++
			for j := 2; j <= int(f.i); j++ {
				fmt.Fprintf(&where, ", ?%d", next)
				next++
			}
			where.WriteString(")")
			for j := range int(f.i) {
				args = append(args, vec1FilterValue(qy.filters[i+1+j]))
			}
			i += int(f.i)
		} else {
			var op string
			rhs := true
			switch f.op {
			case vec1OpEQ:
				op = "="
			case vec1OpLT:
				op = "<"
			case vec1OpGT:
				op = ">"
			case vec1OpLE:
				op = "<="
			case vec1OpGE:
				op = ">="
			case vec1OpIS:
				op = "IS"
			default:
				op, rhs = "IS NOT NULL", false
			}
			fmt.Fprintf(&where, "%sc%d %s", and, f.meta, op)
			if rhs {
				fmt.Fprintf(&where, " ?%d", next)
				next++
				args = append(args, vec1FilterValue(f))
			}
		}
		and = " AND "
	}
	dist := "vec1_l2_distance"
	if q.t.mod.hdr.distance == vec1DistCos {
		dist = "vec1_cos_distance"
	}
	sql := fmt.Sprintf("SELECT *, %s(?%d, vector) AS d FROM 'main'.'%s_base'", dist, next, strings.ReplaceAll(q.name, "'", "''"))
	if where.Len() > 0 {
		sql += " WHERE " + where.String()
	}
	sql += fmt.Sprintf(" ORDER BY d LIMIT %d", qy.k)
	vec := make([]float32, q.t.cfg.nElem)
	copy(vec, qy.vector)
	args = append(args, Value{Typ: Blob, S: vec1FloatBytes(vec)})
	_, rows, err := q.p.QueryArgs(sql, args)
	if err != nil {
		return nil, fmt.Errorf("%s", trimEnginePrefix(err.Error()))
	}
	out := &vec1Out{}
	for _, r := range rows {
		rowid := valueToInt64Trunc(r[0])
		row := make([]Value, 5+q.st.nMeta)
		row[vec1ColRowid] = Value{Typ: Int, I: rowid}
		row[vec1ColCmd], row[vec1ColArg] = Value{Typ: Null}, Value{Typ: Null}
		row[vec1ColDistance] = r[len(r)-1]
		row[vec1ColVector] = vec1Col(r, 1)
		for i := range q.st.nMeta {
			row[vec1ColMeta+i] = vec1Col(r, 2+i)
		}
		out.rows = append(out.rows, row)
		out.rowids = append(out.rowids, rowid)
	}
	return out, nil
}

// vec1FilterValue is a filter's right-hand side as vec1QueryToSql binds it.
func vec1FilterValue(f vec1Filter) Value {
	switch f.typ {
	case Int:
		return Value{Typ: Int, I: f.i}
	case Float:
		return Value{Typ: Float, F: f.f}
	case Text:
		return Value{Typ: Text, S: f.s}
	case Blob:
		return Value{Typ: Blob, S: f.s}
	}
	return Value{Typ: Null}
}

// ---- a scan, and a rowid lookup -------------------------------------------------

// scan is the full-table plan: %_base in rowid order.
func (q *vec1Read) scan() (*vec1Out, error) {
	if err := q.t.loadConfig(q.sh); err != nil {
		return nil, err
	}
	stats := q.needs("distance")
	out := &vec1Out{}
	err := q.sh.scan("_base", func(rowid int64, base []Value) (bool, error) {
		row, err := q.scanRow(rowid, base, stats)
		if err != nil {
			return false, err
		}
		out.rows = append(out.rows, row)
		out.rowids = append(out.rowids, rowid)
		return true, nil
	})
	return out, err
}

// lookup is the rowid plan: "SELECT * FROM %_base WHERE rowid=?".
func (q *vec1Read) lookup(v Value) (*vec1Out, error) {
	if err := q.t.loadConfig(q.sh); err != nil {
		return nil, err
	}
	out := &vec1Out{}
	key := applyAffinityToValue(v, affInteger)
	if key.Typ != Int {
		return out, nil
	}
	base, ok, err := q.sh.get("_base", key.I)
	if err != nil || !ok {
		return out, err
	}
	row, err := q.scanRow(key.I, base, q.needs("distance"))
	if err != nil {
		return nil, err
	}
	out.rows = append(out.rows, row)
	out.rowids = append(out.rowids, key.I)
	return out, nil
}

func (q *vec1Read) scanRow(rowid int64, base []Value, stats bool) ([]Value, error) {
	dist := Value{Typ: Null}
	row, err := q.row(rowid, base, dist)
	if err != nil {
		return nil, err
	}
	if stats {
		if row[vec1ColDistance], err = q.distanceStats(row[vec1ColVector]); err != nil {
			return nil, err
		}
	}
	return row, nil
}

// distanceStats is vec1DistanceStats: the "distance" of a row read without a
// query vector, a description of how the model encodes it.
func (q *vec1Read) distanceStats(vecV Value) (Value, error) {
	if vecV.Typ == Null {
		return Value{Typ: Null}, nil
	}
	mod := q.t.mod
	nElem := q.t.cfg.nElem
	raw := vec1BlobOf(vecV)
	if len(raw) < 4*nElem {
		return Value{}, errVec1Corrupt
	}
	vec := vec1Floats(raw[:4*nElem])
	tmp := make([]float32, max(q.t.tmpLen(), nElem))
	bucket := 0
	var coarse, recon, norm float64
	var tr []float32
	if mod.nBucket() > 1 || mod.nCodebook() > 0 {
		tr = mod.transform(tmp, vec)
		norm = float64(vec1Dot(tr[:nElem], tr[:nElem]))
	}
	if mod.nBucket() > 1 {
		bucket = vec1BestMatch(mod.cent, mod.nBucket(), vec, &coarse)
		if &tr[0] != &tmp[0] {
			copy(tmp, tr[:nElem])
			tr = tmp
		}
		if mod.residual() {
			vec1Sub(tmp[:nElem], tmp[:nElem], mod.centroid(bucket))
		}
	}
	enc := "null"
	if mod.nCodebook() > 0 {
		code := make([]byte, mod.encodedSize())
		round := make([]float32, max(nElem, len(tr)))
		mod.encode(tr, code)
		mod.decode(code, round)
		recon = vec1L2(tr[:nElem], round[:nElem])
		var b strings.Builder
		b.WriteByte('[')
		for i, c := range code {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%d", c)
		}
		b.WriteByte(']')
		enc = b.String()
	}
	ce, re := 0.0, 0.0
	if norm != 0 {
		ce, re = math.Sqrt(coarse/norm), math.Sqrt(recon/norm)
	}
	s, err := fnPrintf("{bucket:%d, coarse_error:%f, reconstruction_error:%f, code: %s}",
		[]Value{{Typ: Int, I: int64(bucket)}, {Typ: Float, F: ce}, {Typ: Float, F: re}, {Typ: Text, S: []byte(enc)}})
	if err != nil {
		return Value{}, err
	}
	return s, nil
}

// vec1Grow is *buf resized to n, reallocated only when it is too short. Its
// contents are not cleared.
func vec1Grow(buf *[]float32, n int) []float32 {
	if cap(*buf) < n {
		*buf = make([]float32, n)
	}
	*buf = (*buf)[:n]
	return *buf
}

// segMapping is the mapping this pager's committed rows are read from: a
// write session's pager reads them through the session's own segment pager.
func (p *ReadOnlyPager) segMapping() *mmapfile.File {
	for range 3 {
		if p == nil || p.segs == nil {
			return nil
		}
		if f := p.segs.file; f != nil {
			return f.m
		}
		switch {
		case p.segs.frozenSegs != nil:
			p = p.segs.frozenSegs
		case p.segs.session != nil:
			p = p.segs.session.segments
		default:
			return nil
		}
	}
	return nil
}
