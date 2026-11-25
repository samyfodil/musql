package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestRowStoreRemembersItHasNoRows: #392. A filtered DELETE, then DELETE of
// every row, leaves the table's segments in place with each row marked gone.
// The next INSERT asks the store for its largest rowid once per row, and an
// empty store's answer was never kept: every row walked every gone row, and
// 25,000 rows took 7 s. The store now keeps "no rows" as it keeps a maximum,
// and drops it on the same writes.
func TestRowStoreRemembersItHasNoRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// One session per statement, as the driver commits each one.
	for _, q := range []string{
		`CREATE TABLE t(a TEXT, b TEXT)`,
		`INSERT INTO t SELECT 'x', value FROM generate_series(1, 3000)`,
		`DELETE FROM t WHERE b > '1230'`,
		`DELETE FROM t`,
	} {
		if err := n.Exec(q); err != nil {
			t.Fatal(q, err)
		}
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
		if n, err = OpenWrite(path); err != nil {
			t.Fatal(err)
		}
	}
	db := n.DB
	tbl := db.findTableMetaIn(scopeMain, "t")
	if err := db.ensureTableLoaded(tbl); err != nil {
		t.Fatal(err)
	}
	s := tbl.rows
	if s.seg == nil || s.baseGone {
		t.Fatalf("want the segment-backed store with its rows marked gone (seg %v, baseGone %v)", s.seg != nil, s.baseGone)
	}
	if m, ok := s.maxRowid(); ok {
		t.Fatalf("maxRowid of an empty store = %d", m)
	}
	if !s.maxKnown {
		t.Fatal("an empty store's maxRowid was not remembered")
	}
	ref := newRowStore(map[uint64][]Value{}, nil)
	s.startUndo()
	ref.startUndo()
	v := []Value{{Typ: Text, S: []byte("y")}, {Typ: Null}}
	for i, op := range []struct {
		put  bool
		rid  uint64
		mark bool
		undo bool
	}{
		{put: true, rid: 7}, {put: true, rid: 3}, {rid: 7}, {rid: 3}, {mark: true},
		{put: true, rid: 9}, {undo: true}, {put: true, rid: 2}, {rid: 2},
	} {
		switch {
		case op.mark:
			s.startUndo()
			ref.startUndo()
		case op.undo:
			s.undoTo(len(s.undo) - 1)
			ref.undoTo(len(ref.undo) - 1)
		case op.put:
			s.put(op.rid, v)
			ref.put(op.rid, v)
		default:
			s.drop(op.rid)
			ref.drop(op.rid)
		}
		ma, oka := s.maxRowid()
		mb, okb := ref.maxRowid()
		if ma != mb || oka != okb {
			t.Fatalf("step %d: maxRowid %d,%v want %d,%v", i, int64(ma), oka, int64(mb), okb)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`INSERT INTO t SELECT 'x', value FROM generate_series(1, 3000)`); err != nil {
		t.Fatal(err)
	}
	_, rows, err := n.Query(`SELECT count(*), min(rowid), max(rowid), count(DISTINCT rowid) FROM t`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, v := range rows[0] {
		got = append(got, v.I)
	}
	if fmt.Sprint(got) != "[3000 1 3000 3000]" {
		t.Fatalf("after the INSERT: count, min(rowid), max(rowid), distinct rowids = %v", got)
	}
}
