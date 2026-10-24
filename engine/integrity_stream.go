package engine

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/samyfodil/musql/internal/extsort"
)

// PRAGMA integrity_check's per-row half in bounded memory.
//
// integrityRowFindingsInMemory (integrity_rowcheck.go) holds a table's rows and
// every index's entries at once. This pass reads the rows once, in walk order,
// and holds only what a capped answer can show: the first `limit` findings in
// pragma.c's order, and how many there are in all. The two sides that need
// more than one row at a time go to disk:
//
//   - an index's entries, derived from the rows, are sorted externally
//     (internal/extsort) wherever the check reads them in index order: a
//     UNIQUE index, whose finding compares an entry with the next, and every
//     index the caller holds a stored copy of;
//   - a stored copy (IntegrityCheckOptions.StoredIndexEntries) is written to a
//     scratch file in its own order, and each row's entry is found in it as
//     sort.Search found it in the slice: a merge when the copy is in order, a
//     replay of the binary search otherwise (integritySearch).
//
// The CHECK constraints are two queries in place of one row per row: how many
// rows violate one, and the first `limit` of those rowids.
//
// What it cannot take exactly it hands back to the in-memory form, which reads
// at most integrityInMemoryBudgetVar bytes of rows and declines past that. Two
// of those are files this engine did not write: rows out of rowid order, and a
// REAL NaN in a key (the comparison is then no order, and the in-memory answer
// turns on the sort it ran). The third is legal and rare: a table with CHECK
// constraints whose columns take all three names of its rowid.

// integrityStreamBudgetVar is about how much memory the derived entries being
// sorted take, all indexes together; a variable so a test can make every sort
// spill.
var integrityStreamBudgetVar = 32 << 20

// integrityInMemoryBudgetVar bounds the rows integrityRowFindingsInMemory
// reads; a variable so a test can reach the decline.
var integrityInMemoryBudgetVar = 32 << 20

// errIntegrityInMemory is the streaming pass handing a table to the in-memory
// form.
var errIntegrityInMemory = errors.New("engine: integrity_check: the in-memory form answers this table")

// rowFinding is one per-row message and its place in pragma.c's order: the
// row's walk position, then slot -- a column's index, then the CHECK, then
// each index's two (integrityIndexSlot).
type rowFinding struct {
	rowid int64
	slot  int
	msg   string
}

func rowFindingLess(a, b rowFinding) bool {
	if a.rowid != b.rowid {
		return a.rowid < b.rowid
	}
	return a.slot < b.slot
}

// findingHeap keeps the `limit` first findings it is shown, in any order of
// showing, and counts them all.
type findingHeap struct {
	limit int
	h     []rowFinding // a max-heap: h[0] is the last one kept
	n     int
}

func (fh *findingHeap) add(f rowFinding) {
	fh.n++
	if fh.limit <= 0 {
		return
	}
	if len(fh.h) < fh.limit {
		fh.h = append(fh.h, f)
		for i := len(fh.h) - 1; i > 0; {
			p := (i - 1) / 2
			if !rowFindingLess(fh.h[p], fh.h[i]) {
				break
			}
			fh.h[p], fh.h[i] = fh.h[i], fh.h[p]
			i = p
		}
		return
	}
	if !rowFindingLess(f, fh.h[0]) {
		return
	}
	fh.h[0] = f
	for i := 0; ; {
		l := 2*i + 1
		if l >= len(fh.h) {
			break
		}
		m := l
		if r := l + 1; r < len(fh.h) && rowFindingLess(fh.h[l], fh.h[r]) {
			m = r
		}
		if !rowFindingLess(fh.h[i], fh.h[m]) {
			break
		}
		fh.h[i], fh.h[m] = fh.h[m], fh.h[i]
		i = m
	}
}

// messages is what was kept, in order.
func (fh *findingHeap) messages() []string {
	slices.SortFunc(fh.h, func(a, b rowFinding) int {
		if rowFindingLess(a, b) {
			return -1
		}
		if rowFindingLess(b, a) {
			return 1
		}
		return 0
	})
	out := make([]string, len(fh.h))
	for i, f := range fh.h {
		out[i] = f.msg
	}
	return out
}

// integrityIndexSlot is index k's slot for its row-against-index finding
// ("missing", or "values differ") when sub is 0, its UNIQUE finding when 1.
func integrityIndexSlot(nCols, k, sub int) int { return nCols + 1 + 2*k + sub }

// integrityIndex is one index's part of the streaming pass.
type integrityIndex struct {
	idx       *indexMeta
	k         int
	stored    iter.Seq[[]Value]
	storedErr func() error
	hasStored bool
	// keys is the rows' entries, sorted, where the check reads them in order.
	keys *extsort.Sorter[[]Value]
}

// integrityRowFindingsStreamed is integrityRowFindingsInMemory's answer for a
// rowid table with a b-tree, or errIntegrityInMemory for a table it hands back
// (see this file's comment) and errIntegrityUnreadable for rows the pager
// cannot read.
func (p *ReadOnlyPager) integrityRowFindingsStreamed(tr *SchemaRow, tm *tableMeta, indexes []*indexMeta, checks []checkConstraint, qualify, isQuick bool, limit int) (counts, out []string, n int, err error) {
	cols := tm.cols
	enc := p.encoding()
	rowidName := ""
	if len(checks) > 0 {
		if rowidName = integrityRowidName(tm); rowidName == "" {
			return nil, nil, 0, errIntegrityInMemory
		}
	}
	ixs := make([]*integrityIndex, len(indexes))
	sorted := 0
	for k, idx := range indexes {
		ix := &integrityIndex{idx: idx, k: k}
		if p.storedIndexEntries != nil {
			ix.stored, ix.storedErr, ix.hasStored = p.storedIndexEntries(idx.name)
		}
		if !isQuick && (ix.hasStored || idx.unique) {
			sorted++
		}
		ixs[k] = ix
	}
	defer func() {
		for _, ix := range ixs {
			if ix.keys != nil {
				ix.keys.Close()
			}
		}
	}()
	for _, ix := range ixs {
		if !isQuick && (ix.hasStored || ix.idx.unique) {
			ix.keys = extsort.New(integrityKeyCodec(ix.idx, enc), integrityStreamBudgetVar/sorted)
		}
	}

	found := &findingHeap{limit: limit}
	_, strict, _ := parseTableTailClauses(tr.SQL)
	nRows := 0
	var prev uint64
	seq, errFn := p.ScanTable(tm.rootPage)
	var rerr error
	for rowid, vals := range seq {
		if nRows > 0 && !rowidLess(prev, rowid) {
			return nil, nil, 0, errIntegrityInMemory
		}
		prev = rowid
		row, ferr := fullStoredRow(tm, rowid, vals)
		if ferr != nil {
			rerr = ferr
			break
		}
		nRows++
		// (1)-(4), per column in declaration order, skipping the rowid alias --
		// pragma.c:1920's "if( j==pTab->iPKey ) continue".
		for j, c := range cols {
			if j == tm.ipkIndex {
				continue
			}
			if msg := integrityColumnFinding(tr.Name, c, strict, row, j); msg != "" {
				found.add(rowFinding{int64(rowid), j, msg})
			}
		}
		if isQuick {
			continue
		}
		for _, ix := range ixs {
			if ix.keys == nil {
				// Read nowhere, but the in-memory form searches it, and a NaN
				// key can make that search miss.
				for _, ci := range ix.idx.colIdx {
					if v := indexColumnValue(tm, rowid, row, ci); v.Typ == Float && math.IsNaN(v.F) {
						return nil, nil, 0, errIntegrityInMemory
					}
				}
				continue
			}
			key := indexEntryForNewRow(tm, ix.idx, rowid, row)
			if valuesHaveNaN(key) {
				return nil, nil, 0, errIntegrityInMemory
			}
			if err := ix.keys.Add(ownedValues(key)); err != nil {
				return nil, nil, 0, err
			}
		}
	}
	if rerr == nil {
		rerr = errFn()
	}
	if rerr != nil {
		return nil, nil, 0, errIntegrityUnreadable
	}

	if len(checks) > 0 {
		if err := p.integrityCheckFindings(tr, checks, qualify, rowidName, len(cols), found); err != nil {
			return nil, nil, 0, err
		}
	}

	for _, ix := range ixs {
		derived := true
		if ix.hasStored {
			sp, ok, err := spillStoredEntries(ix.stored, ix.storedErr, !isQuick, ix.idx, enc)
			if err != nil {
				return nil, nil, 0, err
			}
			if ok {
				derived = false
				if sp.m != nRows {
					counts = append(counts, "wrong # of entries in index "+ix.idx.name)
				}
				if !isQuick {
					if sp.nan {
						sp.close()
						return nil, nil, 0, errIntegrityInMemory
					}
					err := ix.checkStored(sp, cols, enc, found)
					sp.close()
					if err != nil {
						return nil, nil, 0, err
					}
				}
			}
		}
		if derived && !isQuick && ix.idx.unique {
			if err := ix.checkDerivedUnique(cols, enc, found); err != nil {
				return nil, nil, 0, err
			}
		}
	}
	return counts, found.messages(), found.n, nil
}

// errIntegrityUnreadable is the streaming pass finding rows the pager cannot
// read, which the caller answers with the CHECK half alone, as the in-memory
// form does.
var errIntegrityUnreadable = errors.New("engine: integrity_check: rows unreadable")

// integrityRowidName is a name that reads tm's rowid in SQL: one of the three
// spellings no column has taken, else the INTEGER PRIMARY KEY's own, else "".
func integrityRowidName(tm *tableMeta) string {
	for _, name := range []string{"rowid", "_rowid_", "oid"} {
		taken := false
		for _, c := range tm.cols {
			if equalFoldName(c.Name, name) {
				taken = true
				break
			}
		}
		if !taken {
			return name
		}
	}
	if tm.ipkIndex >= 0 {
		return quoteIdent(tm.cols[tm.ipkIndex].Name)
	}
	return ""
}

// integrityCheckFindings adds a CHECK finding for every row that breaks a
// constraint, asking the query engine exactly as the in-memory form does
// (integrityCheckViolatingRowids): how many rows, then which ones, the first
// `limit` in rowid order.
func (p *ReadOnlyPager) integrityCheckFindings(tr *SchemaRow, checks []checkConstraint, qualify bool, rowidName string, nCols int, found *findingHeap) error {
	where := " WHERE " + integrityCheckViolationExpr(checks)
	_, vals, qerr := p.QueryArgs(integrityCheckProbeSQL(tr, checks, qualify, "SELECT count(*) FROM ")+where, nil)
	if qerr != nil {
		return fmt.Errorf("engine: PRAGMA integrity_check: %s: %w", tr.Name, qerr)
	}
	if len(vals) != 1 || len(vals[0]) != 1 || vals[0][0].Typ != Int {
		return fmt.Errorf("engine: PRAGMA integrity_check: %s: unexpected count shape", tr.Name)
	}
	total := vals[0][0].I
	if total <= 0 {
		return nil
	}
	msg := fmt.Sprintf("CHECK constraint failed in %s", tr.Name)
	keep := min(total, int64(max(found.limit, 0)))
	if keep > 0 {
		q := integrityCheckProbeSQL(tr, checks, qualify, "SELECT "+rowidName+" FROM ") + " NOT INDEXED" + where +
			" ORDER BY " + rowidName + " LIMIT " + strconv.FormatInt(keep, 10)
		_, ids, qerr := p.QueryArgs(q, nil)
		if qerr != nil {
			return fmt.Errorf("engine: PRAGMA integrity_check: %s: %w", tr.Name, qerr)
		}
		if int64(len(ids)) != keep {
			return fmt.Errorf("engine: PRAGMA integrity_check: %s: %d of %d violating rows", tr.Name, len(ids), keep)
		}
		for _, r := range ids {
			if len(r) != 1 || r[0].Typ != Int {
				return fmt.Errorf("engine: PRAGMA integrity_check: %s: unexpected rowid shape", tr.Name)
			}
			found.add(rowFinding{r[0].I, nCols, msg})
		}
	}
	found.n += int(total - keep)
	return nil
}

// checkDerivedUnique is the UNIQUE finding over entries derived from the rows:
// each row's entry is its own in the sorted list, so the one test that can
// fire is integrityEntryIsNonUnique's against the entry after it.
func (ix *integrityIndex) checkDerivedUnique(cols []columnInfo, enc TextEncoding, found *findingHeap) error {
	var prev []Value
	err := ix.keys.Each(func(k []Value) error {
		if prev != nil && integrityEntryIsNonUnique(ix.idx, cols, prev, k, true, enc) {
			found.add(rowFinding{prev[len(prev)-1].I, integrityIndexSlot(len(cols), ix.k, 1), "non-unique entry in index " + ix.idx.name})
		}
		prev = k
		return nil
	})
	return err
}

// checkStored holds every row's entry against a stored copy of the index, as
// pragma.c:2066-2149 holds the row against the index b-tree: the entry is
// found by sort.Search over the copy in its own order.
func (ix *integrityIndex) checkStored(sp *storedSpill, cols []columnInfo, enc TextEncoding, found *findingHeap) error {
	idx := ix.idx
	cmp := func(a, b []Value) int { return compareIndexRec(a, b, idx.colCollation, idx.colDesc, enc) }
	missing := integrityIndexSlot(len(cols), ix.k, 0)
	var seq *storedSeq
	var rnd *storedRandom
	var search *integritySearch
	if sp.sorted {
		var err error
		if seq, err = newStoredSeq(sp); err != nil {
			return err
		}
	} else {
		var err error
		if rnd, err = newStoredRandom(sp); err != nil {
			return err
		}
		defer rnd.close()
		search = &integritySearch{n: sp.m, probe: rnd.at, cmp: cmp}
	}
	return ix.keys.Each(func(key []Value) error {
		rowid := key[len(key)-1].I
		var at int
		var rec, next []Value
		if seq != nil {
			// In order, sort.Search's answer is the first entry not below the
			// key, and keys come in ascending order: a merge.
			for seq.pos < sp.m && cmp(seq.cur, key) < 0 {
				if err := seq.advance(); err != nil {
					return err
				}
			}
			at = seq.pos
			if at < sp.m {
				rec = seq.cur
			}
		} else {
			var err error
			if at, err = search.search(key); err != nil {
				return err
			}
			if at < sp.m {
				if rec, err = rnd.at(at); err != nil {
					return err
				}
			}
		}
		if at == sp.m || cmp(rec, key) != 0 {
			found.add(rowFinding{rowid, missing, fmt.Sprintf("row %d missing from index %s", rowid, idx.name)})
			return nil
		}
		// pragma.c:2111-2124: a key column under a non-BINARY collation must
		// still hold the table's exact value. OP_Ne carries no collating
		// sequence and no SQLITE_NULLEQ, so it compares BINARY and a NULL on
		// either side never counts as different.
		for kk := range idx.colIdx {
			if kk < len(idx.colCollation) && !strings.EqualFold(idx.colCollation[kk], "BINARY") &&
				rec[kk].Typ != Null && key[kk].Typ != Null && compareValues(rec[kk], key[kk]) != 0 {
				found.add(rowFinding{rowid, missing, fmt.Sprintf("row %d values differ from index %s", rowid, idx.name)})
				break
			}
		}
		if !idx.unique {
			return nil
		}
		haveNext := at+1 < sp.m
		if haveNext {
			var err error
			if seq != nil {
				if next, err = seq.peek(); err != nil {
					return err
				}
			} else if next, err = rnd.at(at + 1); err != nil {
				return err
			}
		}
		if integrityEntryIsNonUnique(idx, cols, rec, next, haveNext, enc) {
			found.add(rowFinding{rowid, integrityIndexSlot(len(cols), ix.k, 1), "non-unique entry in index " + idx.name})
		}
		return nil
	})
}

// integritySearch is sort.Search over a sequence read by position, asked for
// keys in ascending order: each answer is the one sort.Search gives, and the
// sequence need not be in order. sort.Search's probes are a path down an
// implicit tree, and the answer moves right as the key grows, so a key's path
// is the last key's up to the first node where it now goes right; only the
// rest is read. No node is entered twice.
type integritySearch struct {
	n     int
	probe func(int) ([]Value, error)
	cmp   func(a, b []Value) int
	path  []searchStep
	final int
	begun bool
}

type searchStep struct {
	j, h int
	s    []Value
	left bool
}

func (b *integritySearch) search(key []Value) (int, error) {
	if !b.begun {
		b.begun = true
		return b.descend(0, b.n, key)
	}
	for d := range b.path {
		st := &b.path[d]
		if st.left && b.cmp(st.s, key) < 0 {
			st.left = false
			b.path = b.path[:d+1]
			return b.descend(st.h+1, st.j, key)
		}
	}
	return b.final, nil
}

func (b *integritySearch) descend(i, j int, key []Value) (int, error) {
	for i < j {
		h := int(uint(i+j) >> 1)
		s, err := b.probe(h)
		if err != nil {
			return 0, err
		}
		if b.cmp(s, key) >= 0 {
			b.path = append(b.path, searchStep{j: j, h: h, s: s, left: true})
			j = h
		} else {
			b.path = append(b.path, searchStep{j: j, h: h, s: s})
			i = h + 1
		}
	}
	b.final = i
	return i, nil
}

// storedSpill is a stored copy of an index, written to a scratch file in its
// own order.
type storedSpill struct {
	f      *extsort.TempFile
	m      int
	sorted bool
	nan    bool
}

func (sp *storedSpill) close() {
	if sp.f != nil {
		sp.f.Close()
	}
}

// spillStoredEntries reads a stored copy to its end: how many entries, and,
// when keep is set, the entries themselves, whether they are in index order and
// whether one holds a NaN. ok is false when the copy could not be read, which
// leaves the index derived from the rows, as the in-memory form leaves it.
func spillStoredEntries(entries iter.Seq[[]Value], errFn func() error, keep bool, idx *indexMeta, enc TextEncoding) (sp *storedSpill, ok bool, err error) {
	sp = &storedSpill{sorted: true}
	var w *bufio.Writer
	if keep {
		if sp.f, err = extsort.NewTempFile(); err != nil {
			return nil, false, err
		}
		w = sp.f.Writer()
	}
	var buf []byte
	var prevRec []Value
	var lenBuf [binary.MaxVarintLen64]byte
	for rec := range entries {
		sp.m++
		if !keep {
			continue
		}
		if valuesHaveNaN(rec) {
			sp.nan = true
		}
		if prevRec != nil && sp.sorted && compareIndexRec(prevRec, rec, idx.colCollation, idx.colDesc, enc) > 0 {
			sp.sorted = false
		}
		buf = appendIntegrityValues(buf[:0], rec)
		if _, err = w.Write(lenBuf[:binary.PutUvarint(lenBuf[:], uint64(len(buf)))]); err == nil {
			_, err = w.Write(buf)
		}
		if err != nil {
			sp.close()
			return nil, false, err
		}
		if sp.sorted {
			prevRec = ownedValues(rec)
		}
	}
	if errFn != nil && errFn() != nil {
		sp.close()
		return nil, false, nil
	}
	if keep {
		if err = w.Flush(); err != nil {
			sp.close()
			return nil, false, err
		}
	}
	return sp, true, nil
}

// storedSeq reads a spill front to back: cur is entry pos while pos is short
// of the end, next the one after it once peeked.
type storedSeq struct {
	sp       *storedSpill
	r        *bufio.Reader
	pos      int
	cur      []Value
	next     []Value
	haveNext bool
	buf      []byte
}

func newStoredSeq(sp *storedSpill) (*storedSeq, error) {
	s := &storedSeq{sp: sp, r: bufio.NewReaderSize(io.NewSectionReader(sp.f, 0, sp.f.Size()), 64<<10)}
	if sp.m > 0 {
		var err error
		if s.cur, err = s.read(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *storedSeq) read() ([]Value, error) {
	var err error
	if s.buf, err = extsort.ReadRecord(s.r, s.buf); err != nil {
		return nil, fmt.Errorf("engine: integrity_check: reading a stored index: %w", err)
	}
	return decodeIntegrityValues(s.buf)
}

// advance moves to entry pos+1.
func (s *storedSeq) advance() error {
	s.pos++
	switch {
	case s.haveNext:
		s.cur, s.next, s.haveNext = s.next, nil, false
	case s.pos < s.sp.m:
		var err error
		if s.cur, err = s.read(); err != nil {
			return err
		}
	default:
		s.cur = nil
	}
	return nil
}

// peek is entry pos+1, which the caller knows exists.
func (s *storedSeq) peek() ([]Value, error) {
	if !s.haveNext {
		v, err := s.read()
		if err != nil {
			return nil, err
		}
		s.next, s.haveNext = v, true
	}
	return s.next, nil
}

// storedRandom reads a spill by position, through a second file of offsets.
type storedRandom struct {
	sp   *storedSpill
	offs *extsort.TempFile
	last int
	rec  []Value
	buf  []byte
}

func newStoredRandom(sp *storedSpill) (*storedRandom, error) {
	offs, err := extsort.NewTempFile()
	if err != nil {
		return nil, err
	}
	w := offs.Writer()
	r := bufio.NewReaderSize(io.NewSectionReader(sp.f, 0, sp.f.Size()), 64<<10)
	var off int64
	var buf []byte
	var b [8]byte
	for range sp.m {
		binary.LittleEndian.PutUint64(b[:], uint64(off))
		if _, err := w.Write(b[:]); err != nil {
			offs.Close()
			return nil, err
		}
		n, err := binary.ReadUvarint(r)
		if err == nil {
			buf = slices.Grow(buf[:0], int(n))[:n]
			_, err = io.ReadFull(r, buf)
		}
		if err != nil {
			offs.Close()
			return nil, fmt.Errorf("engine: integrity_check: reading a stored index: %w", err)
		}
		var lb [binary.MaxVarintLen64]byte
		off += int64(binary.PutUvarint(lb[:], n)) + int64(n)
	}
	if err := w.Flush(); err != nil {
		offs.Close()
		return nil, err
	}
	return &storedRandom{sp: sp, offs: offs, last: -1}, nil
}

func (s *storedRandom) close() { s.offs.Close() }

// at is entry i.
func (s *storedRandom) at(i int) ([]Value, error) {
	if i == s.last {
		return s.rec, nil
	}
	var b [8]byte
	if _, err := s.offs.ReadAt(b[:], int64(i)*8); err != nil {
		return nil, fmt.Errorf("engine: integrity_check: reading a stored index: %w", err)
	}
	r := bufio.NewReaderSize(io.NewSectionReader(s.sp.f, int64(binary.LittleEndian.Uint64(b[:])), s.sp.f.Size()), 512)
	var err error
	if s.buf, err = extsort.ReadRecord(r, s.buf); err != nil {
		return nil, fmt.Errorf("engine: integrity_check: reading a stored index: %w", err)
	}
	rec, err := decodeIntegrityValues(s.buf)
	if err != nil {
		return nil, err
	}
	s.last, s.rec = i, rec
	return rec, nil
}

// integrityKeyCodec sorts idx's entries as integrity_check orders them.
func integrityKeyCodec(idx *indexMeta, enc TextEncoding) extsort.Codec[[]Value] {
	return extsort.Codec[[]Value]{
		Compare: func(a, b []Value) int { return compareIndexRec(a, b, idx.colCollation, idx.colDesc, enc) },
		Size: func(v []Value) int {
			n := 24 + 48*len(v)
			for i := range v {
				n += len(v[i].S)
			}
			return n
		},
		Append: appendIntegrityValues,
		Decode: decodeIntegrityValues,
	}
}

// valuesHaveNaN reports a REAL NaN among vs.
func valuesHaveNaN(vs []Value) bool {
	for i := range vs {
		if vs[i].Typ == Float && math.IsNaN(vs[i].F) {
			return true
		}
	}
	return false
}

// ownedValues copies vs and the bytes they hold into memory of its own, so a
// held entry keeps nothing else alive.
func ownedValues(vs []Value) []Value {
	n := 0
	for i := range vs {
		n += len(vs[i].S)
	}
	out := slices.Clone(vs)
	if n == 0 {
		return out
	}
	b := make([]byte, 0, n)
	for i := range out {
		if out[i].S != nil {
			at := len(b)
			b = append(b, out[i].S...)
			out[i].S = b[at:len(b):len(b)]
		}
	}
	return out
}

// appendIntegrityValues encodes vs exactly: every field a Value carries.
func appendIntegrityValues(dst []byte, vs []Value) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(vs)))
	for i := range vs {
		v := &vs[i]
		var flags byte
		if v.intReal {
			flags |= 1
		}
		if v.S == nil {
			flags |= 2
		}
		dst = append(dst, byte(v.Typ), v.Subtype, flags)
		dst = binary.AppendVarint(dst, v.I)
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.F))
		dst = binary.AppendUvarint(dst, uint64(len(v.S)))
		dst = append(dst, v.S...)
	}
	return dst
}

var errIntegrityRecord = errors.New("engine: integrity_check: a scratch record is malformed")

// decodeIntegrityValues reverses appendIntegrityValues into values that own
// their bytes.
func decodeIntegrityValues(b []byte) ([]Value, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return nil, errIntegrityRecord
	}
	b = b[k:]
	vs := make([]Value, n)
	var mem []byte
	for i := range vs {
		if len(b) < 3 {
			return nil, errIntegrityRecord
		}
		v := &vs[i]
		v.Typ, v.Subtype = ValueType(b[0]), b[1]
		flags := b[2]
		v.intReal = flags&1 != 0
		b = b[3:]
		iv, k := binary.Varint(b)
		if k <= 0 || len(b) < k+8 {
			return nil, errIntegrityRecord
		}
		v.I = iv
		v.F = math.Float64frombits(binary.LittleEndian.Uint64(b[k:]))
		b = b[k+8:]
		ln, k := binary.Uvarint(b)
		if k <= 0 || ln > uint64(len(b)-k) {
			return nil, errIntegrityRecord
		}
		b = b[k:]
		if flags&2 == 0 {
			if mem == nil {
				mem = make([]byte, 0, len(b))
			}
			at := len(mem)
			mem = append(mem, b[:ln]...)
			v.S = mem[at:len(mem):len(mem)]
		}
		b = b[ln:]
	}
	return vs, nil
}
