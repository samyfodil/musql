package engine

// Access to a vec1 table's shadow tables, which is all a vec1 table is: from a
// write session (vec1DBShadow), where vec1's own statements run, and from a
// read pager (vec1PagerShadow), where a query runs. Each statement vec1.c
// prepares against them is one method here, so the order rows come back in --
// rowid order for a scan, the order of the %_idx_idx index (bucket, first,
// last, rowid) for a bucket's lists -- is the order vec1 sees them in.

import (
	"cmp"
	"fmt"
	"slices"
)

// vec1IdxRow is one %_idx row.
type vec1IdxRow struct {
	id, bucket, first, last int64
	val                     []byte
	intBucket               bool // the bucket column holds an INTEGER
}

func vec1IdxRowOf(id int64, row []Value) vec1IdxRow {
	r := vec1IdxRow{id: id, bucket: valueToInt64Trunc(vec1Col(row, 1)), first: valueToInt64Trunc(vec1Col(row, 2)), last: valueToInt64Trunc(vec1Col(row, 3))}
	r.intBucket = vec1Col(row, 1).Typ == Int
	if v := vec1Col(row, 4); v.Typ == Blob || v.Typ == Text {
		r.val = v.S
	}
	return r
}

// vec1IndexOrder sorts lists as the %_idx_idx index holds them. A NULL first
// or last sorts before every integer in the index; vec1 writes neither.
func vec1IndexOrder(rows []vec1IdxRow) {
	slices.SortStableFunc(rows, func(a, b vec1IdxRow) int {
		if c := cmp.Compare(a.first, b.first); c != 0 {
			return c
		}
		if c := cmp.Compare(a.last, b.last); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
}

// ---- from a write session ----------------------------------------------------

// vec1DBShadow is a table's shadow tables in a write session.
type vec1DBShadow struct {
	db    *DB
	scope schemaScope
	name  string
}

func (db *DB) vec1Shadow(scope schemaScope, name string) *vec1DBShadow {
	return &vec1DBShadow{db: db, scope: scope, name: name}
}

func (s *vec1DBShadow) table(suffix string) (*tableMeta, error) {
	tbl := s.db.findTableMetaIn(s.scope, s.name+suffix)
	if tbl == nil {
		return nil, fmt.Errorf("no such table: %s.%s%s", schemaScopeName(s.scope), s.name, suffix)
	}
	if err := s.db.ensureTableLoaded(tbl); err != nil {
		return nil, err
	}
	return tbl, nil
}

func (s *vec1DBShadow) get(suffix string, rowid int64) ([]Value, bool, error) {
	tbl, err := s.table(suffix)
	if err != nil {
		return nil, false, err
	}
	v, ok := tbl.rows.get(uint64(rowid))
	return v, ok, nil
}

func (s *vec1DBShadow) scan(suffix string, fn func(int64, []Value) (bool, error)) error {
	tbl, err := s.table(suffix)
	if err != nil {
		return err
	}
	var ferr error
	tbl.rows.eachSortedUntil(func(rid uint64, vals []Value) bool {
		more, err := fn(int64(rid), vals)
		if err != nil {
			ferr = err
			return false
		}
		return more
	})
	return ferr
}

// put stores a row as an ordinary write does, the change logged for the
// commit's delta (and for row-change capture).
func (s *vec1DBShadow) put(suffix string, rowid int64, vals []Value) error {
	tbl, err := s.table(suffix)
	if err != nil {
		return err
	}
	old, had := tbl.rows.get(uint64(rowid))
	if had {
		old = append([]Value(nil), old...)
	}
	tbl.putRow(uint64(rowid), vals)
	if had {
		s.db.noteRowUpdate(tbl, uint64(rowid), uint64(rowid), old, vals)
	} else {
		s.db.noteRowChange(tbl, RowInsert, uint64(rowid), nil, vals)
	}
	// The statement's journal, so a statement that fails takes its shadow
	// writes with it, as C's statement rollback does.
	if wc := s.db.vec1Wctx; wc != nil {
		if had {
			wc.undoPut(tbl, uint64(rowid), old)
		} else {
			wc.undoDrop(tbl, uint64(rowid))
		}
	}
	return nil
}

// drop deletes a row, if there is one.
func (s *vec1DBShadow) drop(suffix string, rowid int64) error {
	tbl, err := s.table(suffix)
	if err != nil {
		return err
	}
	old, had := tbl.rows.get(uint64(rowid))
	if !had {
		return nil
	}
	s.db.noteRowChange(tbl, RowDelete, uint64(rowid), old, nil)
	tbl.dropRow(uint64(rowid))
	if wc := s.db.vec1Wctx; wc != nil {
		wc.undoPut(tbl, uint64(rowid), old)
	}
	return nil
}

// newRowid is the rowid an INSERT naming none gets.
func (s *vec1DBShadow) newRowid(suffix string) (int64, error) {
	tbl, err := s.table(suffix)
	if err != nil {
		return 0, err
	}
	rid, err := s.db.autoRowid(tbl)
	return int64(rid), err
}

// clear is "DELETE FROM %Q.'%q<suffix>'".
func (s *vec1DBShadow) clear(suffix string) error {
	tbl, err := s.table(suffix)
	if err != nil {
		return err
	}
	for _, rid := range tbl.rows.sortedRowids() {
		if err := s.drop(suffix, int64(rid)); err != nil {
			return err
		}
	}
	return nil
}

// bucketLists is every %_idx row of a bucket, in index order: what vec1's
// "... FROM %_idx WHERE bucket=?" statements step through.
func (s *vec1DBShadow) bucketLists(bucket int64) ([]vec1IdxRow, error) {
	tbl, err := s.table("_idx")
	if err != nil {
		return nil, err
	}
	var out []vec1IdxRow
	if rids, ok := tbl.rows.eqRowids(1, bucket); ok {
		for _, rid := range rids {
			if row, ok := tbl.rows.get(rid); ok {
				if r := vec1IdxRowOf(int64(rid), row); r.bucket == bucket && vec1Col(row, 1).Typ == Int {
					out = append(out, r)
				}
			}
		}
	} else {
		tbl.rows.eachSortedUntil(func(rid uint64, row []Value) bool {
			if b := vec1Col(row, 1); b.Typ == Int && b.I == bucket {
				out = append(out, vec1IdxRowOf(int64(rid), row))
			}
			return true
		})
	}
	vec1IndexOrder(out)
	return out, nil
}

func schemaScopeName(scope schemaScope) string {
	if scope == scopeTemp {
		return "temp"
	}
	return "main"
}

// ---- from a read pager -------------------------------------------------------

// vec1PagerShadow is a table's shadow tables as a read pager sees them.
type vec1PagerShadow struct {
	p     *ReadOnlyPager
	name  string
	roots map[string]uint32
}

func (p *ReadOnlyPager) vec1Shadow(name string) *vec1PagerShadow {
	return &vec1PagerShadow{p: p, name: name, roots: map[string]uint32{}}
}

func (s *vec1PagerShadow) root(suffix string) (uint32, error) {
	if r, ok := s.roots[suffix]; ok {
		return r, nil
	}
	r, err := s.p.TableRoot(s.name + suffix)
	if err != nil {
		return 0, err
	}
	s.roots[suffix] = r
	return r, nil
}

func (s *vec1PagerShadow) get(suffix string, rowid int64) ([]Value, bool, error) {
	root, err := s.root(suffix)
	if err != nil {
		return nil, false, err
	}
	if v, found, served := s.p.SeekRowidSegments(root, rowid); served {
		return v, found, nil
	}
	var out []Value
	var ok bool
	seq, errFn := s.p.ScanTable(root)
	for rid, vals := range seq {
		if int64(rid) == rowid {
			out, ok = vals, true
			break
		}
	}
	return out, ok, errFn()
}

func (s *vec1PagerShadow) scan(suffix string, fn func(int64, []Value) (bool, error)) error {
	root, err := s.root(suffix)
	if err != nil {
		return err
	}
	seq, errFn := s.p.ScanTable(root)
	var ferr error
	for rid, vals := range seq {
		more, err := fn(int64(rid), vals)
		if err != nil {
			ferr = err
			break
		}
		if !more {
			break
		}
	}
	if ferr != nil {
		return ferr
	}
	return errFn()
}

func (s *vec1PagerShadow) bucketLists(bucket int64) ([]vec1IdxRow, error) {
	root, err := s.root("_idx")
	if err != nil {
		return nil, err
	}
	var out []vec1IdxRow
	if rids, ok := s.p.SeekIndexRowidsSegments(root, 1, Value{Typ: Int, I: bucket}, "BINARY"); ok {
		for _, rid := range rids {
			row, found, err := s.get("_idx", rid)
			if err != nil {
				return nil, err
			}
			if found {
				if b := vec1Col(row, 1); b.Typ == Int && b.I == bucket {
					out = append(out, vec1IdxRowOf(rid, row))
				}
			}
		}
	} else if err := s.scan("_idx", func(rid int64, row []Value) (bool, error) {
		if b := vec1Col(row, 1); b.Typ == Int && b.I == bucket {
			out = append(out, vec1IdxRowOf(rid, row))
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	vec1IndexOrder(out)
	return out, nil
}
