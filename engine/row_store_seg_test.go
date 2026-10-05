package engine

import (
	"fmt"
	"maps"
	"math/rand"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestScanDoesNotLoadTheTable: a session reading a table must not
// hold it. The rows are in the mapped segment file; a scan streams them, and the
// heap a scan leaves behind is the cursor's, not the table's. C SQLite holds
// pages in a bounded cache the same way.
func TestScanDoesNotLoadTheTable(t *testing.T) {
	skipUnlessMapped(t)
	if testing.Short() {
		t.Skip("builds a ~40MB table")
	}
	path := filepath.Join(t.TempDir(), "big.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, p TEXT)`); err != nil {
		t.Fatal(err)
	}
	const rows = 200_000
	pad := strings.Repeat("x", 180)
	for i := 1; i <= rows; i++ {
		if _, _, err := n.ExecArgs(`INSERT INTO t VALUES(?, ?, ?)`, []Value{{Typ: Int, I: int64(i)}, {Typ: Int, I: int64(i * 7)}, {Typ: Text, S: []byte(pad)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// VACUUM folds the delta into segments, so the rows are column blocks.
	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	heap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	before := heap()
	_, out, err := n.Query(`SELECT sum(length(p)), count(*), max(v) FROM t WHERE v % 3 <> 1`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprint(int64(len(pad)) * 133_333); fmt.Sprint(out[0][0].I) != want {
		t.Fatalf("scan answered %v, want sum %s", out[0], want)
	}
	_, one, err := n.Query(`SELECT v FROM t WHERE id = 123456`, nil)
	if err != nil || len(one) != 1 || one[0][0].I != 123456*7 {
		t.Fatalf("point read: %v %v", one, err)
	}
	after := heap()
	data := uint64(rows * (len(pad) + 16))
	if after > before && after-before > data/10 {
		t.Fatalf("reading the table grew the heap by %d bytes, %d%% of its %d bytes of rows: it was loaded",
			after-before, 100*(after-before)/data, data)
	}
	t.Logf("heap %d -> %d over %d bytes of rows", before, after, data)
}

// TestSegRowStoreAgreesWithMapStore: the segment-backed store (committed rows read from
// segments + delta, writes in an overlay) against the map store over the SAME
// committed rows, under one random sequence of puts, drops, clears, savepoints
// and rollbacks. Every read -- get, len, both orders, max rowid -- must agree
// after every step: a wrong merge here is a wrong answer for every table.
func TestSegRowStoreAgreesWithMapStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 300) INSERT INTO t SELECT i*3, i FROM c`,
	} {
		if err := n.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// A delta on top: puts that supersede segment rows, new rows, and kills.
	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE t SET v = -v WHERE id % 7 = 0`,
		`DELETE FROM t WHERE id % 11 = 0`,
		`INSERT INTO t VALUES(-5, 5), (1000, 1), (2, 2)`,
	} {
		if err := n.Exec(q); err != nil {
			t.Fatal(err)
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
	db := n.DB
	tbl := db.findTableMetaIn(scopeMain, "t")
	if err := db.ensureTableLoaded(tbl); err != nil {
		t.Fatal(err)
	}
	seg := tbl.rows
	if seg.seg == nil {
		t.Fatal("the table did not get the segment-backed store")
	}
	rows, sorted, err := readTableRowsFromPager(db.segments, tbl)
	if err != nil {
		t.Fatal(err)
	}
	ref := newRowStore(rows, sorted)

	check := func(step int, what string) {
		t.Helper()
		if a, b := seg.len(), ref.len(); a != b {
			t.Fatalf("step %d (%s): len %d, want %d", step, what, a, b)
		}
		if a, b := seg.sortedRowids(), ref.sortedRowids(); fmt.Sprint(a) != fmt.Sprint(b) {
			t.Fatalf("step %d (%s): sorted rowids\n got %v\nwant %v", step, what, a, b)
		}
		var ea, eb []string
		seg.eachSorted(func(r uint64, v []Value) { ea = append(ea, fmt.Sprint(int64(r), v)) })
		ref.eachSorted(func(r uint64, v []Value) { eb = append(eb, fmt.Sprint(int64(r), v)) })
		if fmt.Sprint(ea) != fmt.Sprint(eb) {
			t.Fatalf("step %d (%s): eachSorted differs", step, what)
		}
		got := map[uint64]string{}
		for r, v := range seg.all() {
			if _, dup := got[r]; dup {
				t.Fatalf("step %d (%s): all() yielded rowid %d twice", step, what, int64(r))
			}
			got[r] = fmt.Sprint(v)
		}
		for r, v := range ref.all() {
			if got[r] != fmt.Sprint(v) {
				t.Fatalf("step %d (%s): all() row %d = %q, want %q", step, what, int64(r), got[r], fmt.Sprint(v))
			}
		}
		ma, oka := seg.maxRowid()
		mb, okb := ref.maxRowid()
		if ma != mb || oka != okb {
			t.Fatalf("step %d (%s): maxRowid %d,%v want %d,%v", step, what, int64(ma), oka, int64(mb), okb)
		}
		for _, r := range []int64{-5, 2, 3, 21, 33, 900, 1000, 4242} {
			va, ha := seg.get(uint64(r))
			vb, hb := ref.get(uint64(r))
			if ha != hb || fmt.Sprint(va) != fmt.Sprint(vb) {
				t.Fatalf("step %d (%s): get(%d) = %v,%v want %v,%v", step, what, r, va, ha, vb, hb)
			}
		}
	}
	// The count's FIRST computation with an overlay already there: writes before
	// anything asked for len.
	fresh, freshRef := newSegRowStore(db.segments, tbl), newRowStore(maps.Clone(rows), nil)
	for _, r := range []int64{2, 3, 7, -5, 5000} {
		fresh.put(uint64(r), []Value{{Typ: Null}, {Typ: Int, I: 1}})
		freshRef.put(uint64(r), []Value{{Typ: Null}, {Typ: Int, I: 1}})
	}
	fresh.drop(6)
	freshRef.drop(6)
	if a, b := fresh.len(), freshRef.len(); a != b {
		t.Fatalf("len computed over a written overlay: %d, want %d", a, b)
	}
	check(0, "loaded")
	rng := rand.New(rand.NewSource(7))
	var marks [][2]int
	seg.startUndo()
	ref.startUndo()
	for step := 1; step <= 3000; step++ {
		rid := uint64(int64(rng.Intn(1100) - 20))
		var what string
		switch k := rng.Intn(20); {
		case k < 8:
			v := []Value{{Typ: Null}, {Typ: Int, I: int64(step)}}
			seg.put(rid, v)
			ref.put(rid, v)
			what = fmt.Sprint("put ", int64(rid))
		case k < 14:
			seg.drop(rid)
			ref.drop(rid)
			what = fmt.Sprint("drop ", int64(rid))
		case k < 16:
			marks = append(marks, [2]int{seg.startUndo(), ref.startUndo()})
			what = "savepoint"
		case k < 19 && len(marks) > 0:
			m := marks[len(marks)-1]
			marks = marks[:len(marks)-1]
			seg.undoTo(m[0])
			ref.undoTo(m[1])
			what = "rollback to"
		case k == 19 && rng.Intn(10) == 0:
			seg.clear()
			ref.clear()
			what = "clear"
		default:
			what = "read"
		}
		check(step, what)
		if step%500 == 0 {
			c := seg.clone(tbl)
			if fmt.Sprint(c.sortedRowids()) != fmt.Sprint(ref.sortedRowids()) {
				t.Fatalf("step %d: clone differs", step)
			}
		}
	}
}
