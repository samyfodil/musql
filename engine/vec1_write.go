package engine

// vec1's xUpdate, its commands and the writer, ported from vec1.c
// (vec1UpdateMethod, vec1SpecialInsert, vec1RebuildIndex, vec1DeleteByRowid,
// vec1WriterVector, vec1ListBuilder*). Each SQL statement vec1 runs against a
// shadow table is one vec1DBShadow call, in vec1's order, so the shadow tables
// end up holding what C would have written, rowids included.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

var errVec1Corrupt = errors.New("database disk image is malformed")

// vec1Writer is Vec1Writer: one list builder per bucket.
type vec1Writer struct {
	rebuild  bool
	bld      []*vec1ListBuilder
	residual []float32
	pq       []byte
}

func newVec1Writer(t *vec1Tab, nElem, nMeta int, rebuild bool) *vec1Writer {
	n := max(t.mod.nBucket(), 1)
	w := &vec1Writer{rebuild: rebuild, bld: make([]*vec1ListBuilder, n),
		residual: make([]float32, t.mod.tmpLen(nElem)), pq: make([]byte, t.mod.encodedSize())}
	for i := range w.bld {
		w.bld[i] = &vec1ListBuilder{}
		if rebuild {
			w.bld[i].setup(t.mod, nElem, nMeta, i)
		}
	}
	return w
}

// vec1W is one write's context: the session, the table's shadow tables and
// the connection's state for it.
type vec1W struct {
	db    *DB
	sh    *vec1DBShadow
	t     *vec1Tab
	st    *vec1Store
	name  string
	nMeta int
}

func (db *DB) vec1Writing(vm *vtabMeta, st *vec1Store) *vec1W {
	c := db.vec1Conn()
	schema := "main"
	if vm.isTemp {
		schema = "temp"
	}
	return &vec1W{db: db, sh: db.vec1Shadow(createScope(vm.isTemp), vm.name), t: c.tab(schema, vm.name), st: st, name: vm.name, nMeta: st.nMeta}
}

// begin is xBegin (vec1BeginMethod): the config loaded once per transaction.
func (w *vec1W) begin() error {
	if w.t.begun {
		return nil
	}
	w.t.begun = true
	return w.t.loadConfig(w.sh)
}

// ---- the list builders -------------------------------------------------------

// flushList is vec1ListBuilderFlush.
func (w *vec1W) flushList(b *vec1ListBuilder) error {
	if len(b.rowids) > 0 {
		nEntry := b.nEntry()
		blob := b.listBytes()
		id := b.id
		if id == 0 {
			var err error
			if id, err = w.sh.newRowid("_idx"); err != nil {
				return err
			}
		}
		row := []Value{{Typ: Null}, {Typ: Int, I: int64(b.bucket)}, {Typ: Int, I: b.first}, {Typ: Int, I: b.last}, {Typ: Blob, S: blob}}
		if err := w.sh.put("_idx", id, row); err != nil {
			return err
		}
		b.rowids, b.data, b.id = b.rowids[:0], b.data[:0], 0
		for i := range b.meta {
			m := &b.meta[i]
			mb := append([]byte(nil), m.finish(nEntry)...)
			if err := w.sh.put("_meta", id<<8+int64(i), []Value{{Typ: Null}, {Typ: Blob, S: mb}}); err != nil {
				return err
			}
			m.buf = m.buf[:vec1MetaHdr]
		}
		b.sorted = true
		return nil
	}
	if b.id > 0 {
		if err := w.sh.drop("_idx", b.id); err != nil {
			return err
		}
		if w.nMeta > 0 {
			for i := range int64(256) {
				if err := w.sh.drop("_meta", b.id<<8+i); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// listAdd is vec1ListBuilderAdd.
func (w *vec1W) listAdd(b *vec1ListBuilder, rowid int64, store []byte) error {
	if b.needsFlush(rowid, w.t.cfg.blocksize) {
		if err := w.flushList(b); err != nil {
			return err
		}
		b.szRowid = 4
		if rowid < 0 || rowid >= vec1Tomb32 {
			b.szRowid = 8
		}
		b.sorted, b.id, b.nTombstone = true, 0, 0
		b.initMeta()
	}
	b.add(rowid, store)
	return nil
}

// readMeta is vec1ReadMeta: a %_meta list, which must exist and hold bytes.
func (w *vec1W) readMeta(id int64, iMeta int) ([]byte, error) {
	row, ok, err := w.sh.get("_meta", id<<8+int64(iMeta))
	if err != nil {
		return nil, err
	}
	v := vec1Col(row, 1)
	if !ok || len(v.S) == 0 || (v.Typ != Blob && v.Typ != Text) {
		return nil, errVec1Corrupt
	}
	return append([]byte(nil), v.S...), nil
}

// loadList is vec1ListBuilderLoad.
func (w *vec1W) loadList(b *vec1ListBuilder, r vec1IdxRow, first, last int64) error {
	if !vec1ListSizeOK(w.t.mod, w.t.cfg.nElem, r.val) {
		return errVec1Corrupt
	}
	b.load(w.t.mod, r.id, first, last, r.val)
	for i := range b.meta {
		buf, err := w.readMeta(r.id, i)
		if err != nil {
			return err
		}
		if len(buf) < vec1MetaHdr {
			return errVec1Corrupt
		}
		f := be.Uint32(buf)
		b.meta[i] = vec1MetaBuilder{buf: buf, flags: f, format: f & vec1MetaTypeMask}
	}
	return nil
}

// writerVector is vec1WriterVector: the vector's entry appended to its
// bucket's builder, set up first by loading the bucket's first list still
// smaller than blocksize-min, if there is one.
func (w *vec1W) writerVector(wr *vec1Writer, rowid int64, vec []byte, bucket int) (int, error) {
	if bucket < 0 {
		bucket = w.t.mod.quantize(wr.residual, vec1Floats(vec), wr.pq)
	}
	store := vec
	if w.t.mod.nCodebook() > 0 {
		store = wr.pq
	}
	if bucket >= len(wr.bld) {
		// A writer begun under an earlier model in this transaction has
		// fewer buckets than this one: vec1 writes past its array here.
		return bucket, fmt.Errorf("%w: vec1: a write after a rebuild changed the bucket count in the same transaction", errVDBEUnsupported)
	}
	b := wr.bld[bucket]
	if !b.init {
		b.setup(w.t.mod, w.t.cfg.nElem, w.nMeta, bucket)
		lists, err := w.sh.bucketLists(int64(bucket))
		if err != nil {
			return bucket, err
		}
		for _, r := range lists {
			if len(r.val) < w.t.cfg.blocksizeMin {
				if err := w.loadList(b, r, r.first, r.last); err != nil {
					return bucket, err
				}
				break
			}
		}
	}
	return bucket, w.listAdd(b, rowid, store)
}

// finishWriter is vec1FinishWriter: the pending lists written, or dropped.
func (w *vec1W) finishWriter(discard bool) error {
	wr := w.t.writer
	w.t.writer = nil
	if wr == nil || discard {
		return nil
	}
	for _, b := range wr.bld {
		if b.init {
			if err := w.flushList(b); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- xUpdate -----------------------------------------------------------------

// vec1VectorTypeError is vec1VectorTypeError.
func vec1VectorTypeError(v Value) error {
	names := map[ValueType]string{Int: "INTEGER", Float: "FLOAT", Text: "TEXT", Null: "NULL"}
	return fmt.Errorf("vec1: unexpected vector type %s, expected BLOB", names[v.Typ])
}

// fixVectorSize is vec1FixVectorSize: the table's vector size set by its
// first vector, or by its first model.
func (w *vec1W) fixVectorSize(nBytes int) error {
	if nBytes%4 != 0 || nBytes < vec1VecSizeMin*4 || nBytes > vec1VecSizeMax*4 {
		return fmt.Errorf("invalid vector size: %d bytes", nBytes)
	}
	nElem := nBytes / 4
	if err := w.sh.put("_config", vec1CfgNElem, []Value{{Typ: Null}, {Typ: Int, I: int64(nElem)}}); err != nil {
		return err
	}
	w.t.cfg.nElem = nElem
	return nil
}

// vec1Arg is one xUpdate argument as vec1 reads it.
func vec1BlobOf(v Value) []byte {
	switch v.Typ {
	case Blob, Text:
		return v.S
	case Int, Float:
		return []byte(valueToText(v))
	}
	return nil
}

// xUpdate is vec1UpdateMethod. old is argv[0], NULL for an INSERT; newRowid
// argv[1]; vals argv[2..]: cmd, arg, distance, the vector and the metadata.
// deleteOnly is argc==1. It returns the rowid an INSERT stored.
func (w *vec1W) xUpdate(deleteOnly bool, old Value, newRowid Value, vals []Value) (int64, error) {
	if err := w.begin(); err != nil {
		return 0, err
	}
	if err := w.t.loadIfRequired(w.sh); err != nil {
		return 0, err
	}
	deleteRow := deleteOnly
	var rowid int64
	if !deleteOnly {
		cmd, arg, vecV := vals[0], vals[1], vals[3]
		if cmd.Typ != Null {
			if arg.Typ == Null {
				arg = vecV
			}
			return 0, w.special(valueToText(cmd), arg)
		}
		vec := vecV.S
		var err error
		switch {
		case vecV.Typ != Blob:
			err = vec1VectorTypeError(vecV)
		case w.t.cfg.nElem > 0:
			if len(vec) != 4*w.t.cfg.nElem {
				err = fmt.Errorf("vec1: unexpected vector blob size %d bytes, expected %d", len(vec), 4*w.t.cfg.nElem)
			}
		default:
			err = w.fixVectorSize(len(vec))
		}
		if err == nil && vec1NonFinite(vec[:min(len(vec), 4*w.t.cfg.nElem)]) {
			err = fmt.Errorf("vec1: non-finite value not allowed in vector")
		}
		if err == nil && old.Typ == Int {
			if old.I == valueToInt64Trunc(newRowid) {
				err = w.deleteByRowid(old.I)
			} else {
				deleteRow = true
			}
		}
		if err != nil {
			return 0, err
		}
		bucket := 0
		if w.t.mod.indexed() && w.t.writer == nil {
			w.t.writer = newVec1Writer(w.t, w.t.cfg.nElem, w.nMeta, false)
		}
		if wr := w.t.writer; wr != nil {
			bucket = w.t.mod.quantize(wr.residual, vec1Floats(vec), wr.pq)
		}
		if rowid, err = w.insertBase(newRowid, vec, bucket, vals[4:]); err != nil {
			return 0, err
		}
		if w.t.mod.indexed() {
			if bucket, err = w.writerVector(w.t.writer, rowid, vec, bucket); err != nil {
				return 0, err
			}
			b := w.t.writer.bld[bucket]
			for i := range b.meta {
				b.meta[i].add(vals[4+i])
			}
		}
	}
	if deleteRow {
		if err := w.deleteByRowid(old.I); err != nil {
			return 0, err
		}
	}
	return rowid, nil
}

// insertBase is "INSERT INTO %_base VALUES(?, ?, ...)": the rowid given or
// the next one, a flat model's bucket in place of the vector.
func (w *vec1W) insertBase(rowidV Value, vec []byte, bucket int, meta []Value) (int64, error) {
	var rowid int64
	if rowidV.Typ == Null {
		r, err := w.sh.newRowid("_base")
		if err != nil {
			return 0, err
		}
		rowid = r
	} else {
		rowid = valueToInt64Trunc(rowidV)
		if _, exists, err := w.sh.get("_base", rowid); err != nil {
			return 0, err
		} else if exists {
			// vec1 leaves no message of its own: the statement reports the bare
			// SQLITE_CONSTRAINT.
			return 0, vtabConstraint(fmt.Errorf("constraint failed"))
		}
	}
	row := make([]Value, 2+w.nMeta)
	row[0] = Value{Typ: Null}
	if w.t.mod.isFlat() {
		row[1] = Value{Typ: Int, I: int64(bucket)}
	} else {
		row[1] = Value{Typ: Blob, S: append([]byte(nil), vec...)}
	}
	for i := range w.nMeta {
		row[2+i] = vec1OwnedValue(meta[i])
	}
	return rowid, w.sh.put("_base", rowid, row)
}

// vec1OwnedValue is v holding bytes of its own.
func vec1OwnedValue(v Value) Value {
	if v.Typ == Text || v.Typ == Blob {
		v.S = append([]byte(nil), v.S...)
	}
	v.Subtype = 0
	return v
}

// ---- deleting a row ------------------------------------------------------------

// vec1Loc is Vec1RowidLocation: where an entry is, in a pending list or in an
// %_idx row.
type vec1Loc struct {
	entry, nEntry, nTombstone, szRowid int
	list                               *vec1ListBuilder
	row                                *vec1IdxRow
}

// findByRowid is vec1FindByRowid: the writer's pending list for the bucket
// first, then the bucket's lists holding the rowid in range, in index order.
func (w *vec1W) findByRowid(bucket int, rowid int64) (vec1Loc, error) {
	if wr := w.t.writer; wr != nil && bucket < len(wr.bld) {
		b := wr.bld[bucket]
		if b.init {
			for i := range b.nEntry() {
				if b.rowidAt(i) == rowid {
					return vec1Loc{entry: i, nEntry: b.nEntry(), nTombstone: b.nTombstone, szRowid: b.szRowid, list: b}, nil
				}
			}
		}
	}
	lists, err := w.sh.bucketLists(int64(bucket))
	if err != nil {
		return vec1Loc{}, err
	}
	for i := range lists {
		r := &lists[i]
		if rowid < r.first || rowid > r.last {
			continue
		}
		val := r.val
		if len(val) < vec1ListHdr {
			return vec1Loc{}, errVec1Corrupt
		}
		flags := be.Uint32(val)
		nEntry := int(be.Uint32(val[4:]))
		nTomb := int(be.Uint32(val[8:]))
		sz := 4
		if flags&vec1List64 != 0 {
			sz = 8
		}
		if (sz == 4 && rowid == vec1Tomb32) || (sz == 8 && rowid == vec1Tomb64) {
			continue
		}
		if vec1ListHdr+nEntry*sz > len(val) {
			return vec1Loc{}, errVec1Corrupt
		}
		for e := range nEntry {
			var got int64
			if sz == 4 {
				got = int64(be.Uint32(val[vec1ListHdr+4*e:]))
			} else {
				got = int64(be.Uint64(val[vec1ListHdr+8*e:]))
			}
			if got == rowid {
				return vec1Loc{entry: e, nEntry: nEntry, nTombstone: nTomb, szRowid: sz, row: r}, nil
			}
		}
	}
	return vec1Loc{}, nil
}

// deleteByRowid is vec1DeleteByRowid: the %_base row deleted, its entry made
// a tombstone, or its list compacted once tombstones pass a fifth of it.
func (w *vec1W) deleteByRowid(rowid int64) error {
	row, exists, err := w.sh.get("_base", rowid)
	if err != nil {
		return err
	}
	bucket := 0
	if exists {
		vecV := vec1Col(row, 1)
		row = append([]Value(nil), row...)
		if err := w.sh.drop("_base", rowid); err != nil {
			return err
		}
		if w.t.mod.nBucket() > 0 {
			if w.t.mod.nCodebook() == 0 {
				bucket = int(int32(valueToInt64Trunc(vecV)))
			} else {
				vec := vec1BlobOf(vecV)
				if len(vec) != 4*w.t.cfg.nElem {
					return errVec1Corrupt
				}
				tmp := make([]float32, w.t.tmpLen())
				a := w.t.mod.transform(tmp, vec1Floats(vec))
				bucket = w.t.mod.bestBucket(a[:w.t.cfg.nElem], false)
			}
		}
	}
	if !w.t.mod.indexed() {
		return nil
	}
	loc, err := w.findByRowid(bucket, rowid)
	if err != nil {
		return err
	}
	switch {
	case loc.list == nil && loc.row == nil:
		return errVec1Corrupt
	case loc.nTombstone+1 > loc.nEntry*2/10:
		if loc.list != nil {
			loc.list.compress(rowid)
			return nil
		}
		b := &vec1ListBuilder{}
		b.setup(w.t.mod, w.t.cfg.nElem, w.nMeta, bucket)
		if err := w.loadList(b, *loc.row, 0, 0); err != nil {
			return err
		}
		b.compress(rowid)
		return w.flushList(b)
	}
	if loc.list != nil {
		off := loc.entry * loc.szRowid
		if loc.szRowid == 4 {
			be.PutUint32(loc.list.rowids[off:], vec1Tomb32)
		} else {
			be.PutUint64(loc.list.rowids[off:], vec1Tomb64)
		}
		loc.list.nTombstone++
		return nil
	}
	val := append([]byte(nil), loc.row.val...)
	off := vec1ListHdr + loc.entry*loc.szRowid
	if loc.szRowid == 4 {
		be.PutUint32(val[off:], vec1Tomb32)
	} else {
		be.PutUint64(val[off:], vec1Tomb64)
	}
	be.PutUint32(val[8:], uint32(loc.nTombstone+1))
	r := loc.row
	return w.sh.put("_idx", r.id, []Value{{Typ: Null}, {Typ: Int, I: r.bucket}, {Typ: Int, I: r.first}, {Typ: Int, I: r.last}, {Typ: Blob, S: val}})
}

// ---- the commands --------------------------------------------------------------

// special is vec1SpecialInsert: "INSERT INTO t(cmd, arg) VALUES(name, arg)".
func (w *vec1W) special(cmd string, arg Value) error {
	var which int
	switch strings.ToLower(cmd) {
	case "blocksize":
		which = 0
	case "rebuild":
		which = 1
	case "blocksize-min":
		which = 2
	default:
		return fmt.Errorf("vec1: unknown command: %s", cmd)
	}
	if which != 1 {
		name, key := "blocksize", int64(vec1CfgBlocksize)
		if which == 2 {
			name, key = "blocksize-min", vec1CfgBlocksizeMin
		}
		n := vec1NumericType(arg)
		if n.Typ != Int || n.I < vec1BlocksizeMin || n.I > vec1BlocksizeMax {
			return fmt.Errorf("'%s' requires an integer value between %d and %d", name, vec1BlocksizeMin, vec1BlocksizeMax)
		}
		return w.sh.put("_config", key, []Value{{Typ: Null}, {Typ: Int, I: valueToInt64Trunc(arg)}})
	}
	if arg.Typ != Null {
		var blob []byte
		if arg.Typ == Blob {
			blob = arg.S
		} else {
			h := vec1Header{distance: vec1DistL2}
			index := 0
			text := vec1BlobOf(arg)
			err := vec1ParseJSONConfig(text, false, func(o vec1JSONOpt) error {
				switch {
				case strings.EqualFold(o.key, "distance"):
					d, err := vec1ParseDistance(o.text)
					h.distance = uint32(d)
					return err
				case strings.EqualFold(o.key, "index"):
					i, err := vec1ConfigEnum(o.text, []string{"none", "flat"}, "index")
					index = i + 1
					return err
				}
				return fmt.Errorf("unknown parameter: %s", o.key)
			})
			if err == nil && index == 0 {
				err = fmt.Errorf("required parameter missing: index")
			}
			if err != nil {
				return fmt.Errorf("vec1: %s", err)
			}
			if index == 2 {
				h.flags = vec1ModelIndex
			}
			blob = make([]byte, vec1HeaderSize)
			h.write(blob)
		}
		mod, err := vec1DecodeModel(blob)
		if err != nil {
			return err
		}
		if mod.nElem() > 0 {
			if w.t.cfg.nElem == 0 {
				if err := w.fixVectorSize(mod.nElem() * 4); err != nil {
					return err
				}
			} else if w.t.cfg.nElem != mod.nElem() {
				return fmt.Errorf("vec1: model/data vector size mismatch: %d/%d", mod.nElem(), w.t.cfg.nElem)
			}
		}
		if err := w.sh.put("_model", 1, []Value{{Typ: Null}, {Typ: Blob, S: append([]byte(nil), blob...)}}); err != nil {
			return err
		}
		ver := Value{Typ: Int, I: 1}
		if row, ok, err := w.sh.get("_config", vec1CfgModel); err != nil {
			return err
		} else if ok {
			ver = vec1Incremented(vec1Col(row, 1))
		}
		if err := w.sh.put("_config", vec1CfgModel, []Value{{Typ: Null}, ver}); err != nil {
			return err
		}
	}
	if w.t.mod.isFlat() {
		if err := w.flatToNone(); err != nil {
			return err
		}
	}
	if err := w.t.loadConfig(w.sh); err != nil {
		return err
	}
	return w.rebuildIndex()
}

// vec1Incremented is "val+1" over %_config's untyped column.
func vec1Incremented(v Value) Value {
	r, err := evalArith("+", v, Value{Typ: Int, I: 1})
	if err != nil {
		return Value{Typ: Int, I: 1}
	}
	return r
}

// vec1ConfigEnum is vec1ConfigEnum: the option's index, or vec1's message.
func vec1ConfigEnum(val string, opts []string, name string) (int, error) {
	for i, o := range opts {
		if strings.EqualFold(val, o) {
			return i, nil
		}
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "unrecognized %s '%s', should be one of", name, val)
	for i, o := range opts {
		if i+1 < len(opts) {
			fmt.Fprintf(&msg, " %s,", o)
		} else {
			fmt.Fprintf(&msg, " or %s", o)
		}
	}
	return 0, errors.New(msg.String())
}

// vec1ParseDistance is vec1ConfigDistance.
func vec1ParseDistance(val string) (int, error) {
	i, err := vec1ConfigEnum(val, []string{"l2", "cos"}, "distance")
	return i + 1, err
}

// flatToNone is vec1FlatToNone: a flat index's vectors put back in %_base,
// list by list, each list deleted after.
func (w *vec1W) flatToNone() error {
	var lists []vec1IdxRow
	if err := w.sh.scan("_idx", func(id int64, row []Value) (bool, error) {
		r := vec1IdxRowOf(id, row)
		r.val = append([]byte(nil), r.val...)
		lists = append(lists, r)
		return true, nil
	}); err != nil {
		return err
	}
	for _, r := range lists {
		if !vec1ListSizeOK(w.t.mod, w.t.cfg.nElem, r.val) {
			return errVec1Corrupt
		}
		nEntry := int(be.Uint32(r.val[4:]))
		sz := 4
		if be.Uint32(r.val)&vec1List64 != 0 {
			sz = 8
		}
		szVec := 4 * w.t.cfg.nElem
		for e := range nEntry {
			var rowid int64
			if sz == 4 {
				rowid = int64(be.Uint32(r.val[vec1ListHdr+4*e:]))
				if rowid == vec1Tomb32 {
					continue
				}
			} else {
				rowid = int64(be.Uint64(r.val[vec1ListHdr+8*e:]))
				if rowid == vec1Tomb64 {
					continue
				}
			}
			off := vec1ListHdr + nEntry*sz + e*szVec
			if err := w.updateBaseVector(rowid, Value{Typ: Blob, S: append([]byte(nil), r.val[off:off+szVec]...)}); err != nil {
				return err
			}
		}
		if err := w.sh.drop("_idx", r.id); err != nil {
			return err
		}
	}
	return nil
}

// updateBaseVector is "UPDATE %_base SET vector = ? WHERE id=?".
func (w *vec1W) updateBaseVector(rowid int64, v Value) error {
	row, ok, err := w.sh.get("_base", rowid)
	if err != nil || !ok {
		return err
	}
	nrow := append([]Value(nil), row...)
	for len(nrow) < 2 {
		nrow = append(nrow, Value{Typ: Null})
	}
	nrow[1] = v
	return w.sh.put("_base", rowid, nrow)
}

// rebuildIndex is vec1RebuildIndex: %_idx and %_meta emptied and rebuilt from
// %_base, in rowid order, a rebuild's batched quantization in jobs of 1024.
func (w *vec1W) rebuildIndex() error {
	if err := w.sh.clear("_idx"); err != nil {
		return err
	}
	if err := w.sh.clear("_meta"); err != nil {
		return err
	}
	mod := w.t.mod
	if !mod.indexed() {
		return nil
	}
	nElem := w.t.cfg.nElem
	wr := newVec1Writer(w.t, nElem, w.nMeta, true)
	zeroBase := mod.isFlat() && mod.nBucket() == 0
	bucketBase := mod.isFlat()

	// The rows are read first: vec1 rewrites %_base under its own scan.
	// For a quantizing model they go to the workers 1024 at a time as the
	// scan reaches them, and each job is written in order as it finishes --
	// the order vec1's single-threaded rebuild writes them in -- while the
	// scan and later jobs go on.
	type baseRow struct {
		id   int64
		raw  []byte
		meta []Value
	}
	type job struct {
		rows    []baseRow
		buckets []int
		codes   []byte
		done    chan struct{}
	}
	nEnc := mod.encodedSize()
	tl := mod.tmpLen(nElem)
	quantize := func(jb *job) {
		n := len(jb.rows)
		jb.buckets = make([]int, n)
		jb.codes = make([]byte, n*nEnc)
		bulk := n / vec1MultiNVec * vec1MultiNVec
		tmps := make([][]float32, bulk)
		ins := make([][]float32, bulk)
		codes := make([][]byte, bulk)
		tbuf := make([]float32, bulk*tl)
		for i := range bulk {
			tmps[i] = tbuf[i*tl : (i+1)*tl : (i+1)*tl]
			ins[i] = vec1Floats(jb.rows[i].raw)
			codes[i] = jb.codes[i*nEnc : (i+1)*nEnc]
		}
		mod.quantizeMany(tmps, ins, codes, jb.buckets[:bulk], &vec1Buf{})
		tmp := make([]float32, tl)
		for i := bulk; i < n; i++ {
			clear(tmp)
			jb.buckets[i] = mod.quantize(tmp, vec1Floats(jb.rows[i].raw), jb.codes[i*nEnc:(i+1)*nEnc])
		}
	}
	var jobs []*job
	next := make(chan *job, 256)
	var wg sync.WaitGroup
	// vec1's jobs are 1024 rows. Any multiple of four quantizes the same --
	// only the last four-row batch can be short, wherever the jobs split --
	// so one worker takes larger ones, and hands off less often.
	jobRows := vec1QuantizeJob
	if !zeroBase {
		workers := max(1, min(runtime.GOMAXPROCS(0), w.db.vec1Conn().nThreadValue()))
		if workers == 1 {
			jobRows = 16 * vec1QuantizeJob
		}
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for jb := range next {
					quantize(jb)
					close(jb.done)
				}
			}()
		}
	}
	var cur []baseRow
	send := func() {
		if len(cur) == 0 {
			return
		}
		jb := &job{rows: cur, done: make(chan struct{})}
		jobs = append(jobs, jb)
		if !zeroBase {
			next <- jb
		}
		cur = nil
	}
	var verr error
	err := w.sh.scan("_base", func(id int64, row []Value) (bool, error) {
		b := vec1BlobOf(vec1Col(row, 1))
		if len(b) != 4*nElem {
			verr = fmt.Errorf("vec1: unexpected vector size in %s_base: %d", w.name, len(b))
			return false, nil
		}
		r := baseRow{id: id, raw: append([]byte(nil), b...)}
		r.meta = make([]Value, w.nMeta)
		for i := range w.nMeta {
			r.meta[i] = vec1OwnedValue(vec1Col(row, 2+i))
		}
		cur = append(cur, r)
		if len(cur) == jobRows {
			send()
		}
		return true, nil
	})
	send()
	close(next)
	if err == nil {
		err = verr
	}
	if err != nil {
		wg.Wait()
		return err
	}

	if zeroBase {
		for _, jb := range jobs {
			for _, r := range jb.rows {
				bucket, err := w.writerVector(wr, r.id, r.raw, -1)
				if err != nil {
					return err
				}
				for i := range w.nMeta {
					wr.bld[bucket].meta[i].add(r.meta[i])
				}
			}
		}
		for _, jb := range jobs {
			for _, r := range jb.rows {
				if err := w.updateBaseVector(r.id, Value{Typ: Int, I: 0}); err != nil {
					return err
				}
			}
		}
	} else {
		var ferr error
		for _, jb := range jobs {
			<-jb.done
			if ferr != nil {
				continue
			}
			for i, r := range jb.rows {
				bucket := jb.buckets[i]
				store := r.raw
				if nEnc > 0 {
					store = jb.codes[i*nEnc : (i+1)*nEnc]
				}
				if ferr = w.listAdd(wr.bld[bucket], r.id, store); ferr != nil {
					break
				}
				if bucketBase {
					if ferr = w.updateBaseVector(r.id, Value{Typ: Int, I: int64(bucket)}); ferr != nil {
						break
					}
				}
				for m := range w.nMeta {
					wr.bld[bucket].meta[m].add(r.meta[m])
				}
			}
			jb.rows, jb.codes = nil, nil
		}
		wg.Wait()
		if ferr != nil {
			return ferr
		}
	}
	for _, b := range wr.bld {
		if err := w.flushList(b); err != nil {
			return err
		}
	}
	return nil
}

// nThreadValue is vec1_config('nthread').
func (c *Vec1Connection) nThreadValue() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nThread
}

// ---- the session's side: statements and transactions ----------------------------

// vec1Insert is one row of an INSERT into a vec1 table: full is the declared
// row, the rowid slot first.
func (db *DB) vec1Insert(vm *vtabMeta, st *vec1Store, full []Value) (int64, error) {
	w := db.vec1Writing(vm, st)
	return w.xUpdate(false, Value{Typ: Null}, full[vec1ColRowid], full[vec1ColCmd:])
}

// vec1Update is one row of an UPDATE: the old rowid and the new row.
func (db *DB) vec1Update(vm *vtabMeta, st *vec1Store, oldRowid int64, full []Value) (int64, error) {
	w := db.vec1Writing(vm, st)
	newRowid := full[vec1ColRowid]
	if newRowid.Typ == Null {
		// An UPDATE always has a rowid: SET rowid=NULL is "datatype mismatch"
		// before the module is called, so NULL here is the unchanged one.
		newRowid = Value{Typ: Int, I: oldRowid}
	}
	return w.xUpdate(false, Value{Typ: Int, I: oldRowid}, newRowid, full[vec1ColCmd:])
}

// vec1Delete is one row of a DELETE.
func (db *DB) vec1Delete(vm *vtabMeta, st *vec1Store, rowid int64) error {
	w := db.vec1Writing(vm, st)
	_, err := w.xUpdate(true, Value{Typ: Int, I: rowid}, Value{Typ: Null}, nil)
	return err
}

// vec1Sync writes every table's pending lists: xSync at a commit, and
// xSavepoint and xRelease, which vec1 also flushes at.
func (db *DB) vec1Sync() error {
	c := db.vec1Conns
	if c == nil {
		return nil
	}
	for _, vm := range db.vtabs {
		st, ok := vec1ModuleOf(vm)
		if !ok {
			continue
		}
		w := db.vec1Writing(vm, st)
		if w.t.writer == nil {
			continue
		}
		if err := w.t.loadIfRequired(w.sh); err != nil {
			return err
		}
		if err := w.finishWriter(false); err != nil {
			return err
		}
	}
	return nil
}

// vec1TxnEnd is the end of a transaction for every table: the next write
// loads the config again (xBegin).
func (db *DB) vec1TxnEnd() {
	if c := db.vec1Conns; c != nil {
		for _, t := range c.tabs {
			t.begun = false
		}
	}
}

// vec1Rollback is xRollback and xRollbackTo: the pending lists dropped, the
// model (and, for a ROLLBACK TO, the config) unloaded.
func (db *DB) vec1Rollback(to bool) {
	if c := db.vec1Conns; c != nil {
		for _, t := range c.tabs {
			// Only a table the transaction wrote is in it (aVTrans).
			if !t.begun && t.writer == nil {
				continue
			}
			t.writer = nil
			if to {
				t.cfg.nElem = 0
			} else {
				t.unloadModel()
				t.begun = false
			}
		}
	}
}

// vec1FailedStatement is a write statement on vm that failed outside a
// transaction: its transaction rolls back, and vec1 drops what it was
// writing (xRollback).
func (db *DB) vec1FailedStatement(vm *vtabMeta, err error) {
	if _, ok := vec1ModuleOf(vm); ok && err != nil && !db.txActive {
		db.vec1Rollback(false)
	}
}

// vec1AfterStatement ends an autocommit write statement on vm: it is its own
// transaction, which syncs here.
func (db *DB) vec1AfterStatement(vm *vtabMeta) error {
	if _, ok := vec1ModuleOf(vm); !ok || db.txActive {
		return nil
	}
	err := db.vec1Sync()
	db.vec1TxnEnd()
	return err
}
