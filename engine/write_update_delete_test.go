// Gate for UPDATE/DELETE and writer operations. Checks via pure-Go read side
// (engine.Open/Rows); compat-harness verifies C SQLite accepts the result.
package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"fmt"
	"math"
	"path/filepath"
	"sort"
	"testing"
)

// rowsByID reads tableName's rows via the pure-Go read side and returns them
// keyed by rowid, for order-independent comparison.
func rowsByID(t *testing.T, p *engine.ReadOnlyPager, tableName string) map[uint64][]engine.Value {
	t.Helper()
	rowids, rows, err := p.Rows(tableName)
	if err != nil {
		t.Fatalf("Rows(%s): %v", tableName, err)
	}
	out := make(map[uint64][]engine.Value, len(rowids))
	for i, id := range rowids {
		out[id] = rows[i]
	}
	return out
}

func TestDeletePartialRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "del_partial.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,%s)", i, sqlString(fmt.Sprintf("row-%d", i))))
	}
	n, err := db.Delete("DELETE FROM t WHERE id > 5")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 5 {
		t.Fatalf("Delete: deleted %d rows, want 5", n)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rowids, _, err := p.Rows("t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rowids) != 5 {
		t.Fatalf("got %d rows, want 5", len(rowids))
	}
	sort.Slice(rowids, func(i, j int) bool { return rowids[i] < rowids[j] })
	for i, id := range rowids {
		if id != uint64(i+1) {
			t.Errorf("row %d: rowid = %d, want %d", i, id, i+1)
		}
	}
}

func TestDeleteAllRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "del_all.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,'x')", i))
	}
	n, err := db.Delete("DELETE FROM t")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 10 {
		t.Fatalf("Delete: deleted %d rows, want 10", n)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rowids, _, err := p.Rows("t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rowids) != 0 {
		t.Fatalf("got %d rows, want 0 (empty table is fine)", len(rowids))
	}
}

func TestDeleteNoRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "del_none.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,'x')", i))
	}
	n, err := db.Delete("DELETE FROM t WHERE 1=0")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("Delete: deleted %d rows, want 0", n)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rowids, _, err := p.Rows("t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rowids) != 10 {
		t.Fatalf("got %d rows, want 10", len(rowids))
	}
}

// TestDeleteWhereRowid exercises the write path's WHERE support for the
// implicit rowid/oid/_rowid_ pseudo-column (rowEvalCtx, write_update_
// delete.go): "DELETE FROM t WHERE rowid = ..." must work exactly like the
// read side's own rowid support (query.go/sql_eval.go), on both an INTEGER
// PRIMARY KEY table (rowid aliases id) and a plain table with no IPK
// (rowid is the engine's own auto-assigned rowid -- discovered via Rows,
// since this write path's own INSERT has no way to request a specific
// rowid for a non-IPK table).
func TestDeleteWhereRowid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "del_rowid.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE ipk(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO ipk(id,v) VALUES(%d,'x')", i))
	}
	mustExec(t, db, "CREATE TABLE plain(v TEXT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO plain(v) VALUES('x%d')", i))
	}

	if n, err := db.Delete("DELETE FROM ipk WHERE rowid = 3"); err != nil {
		t.Fatalf("Delete: %v", err)
	} else if n != 1 {
		t.Fatalf("Delete (ipk, rowid=3): deleted %d rows, want 1", n)
	}
	if n, err := db.Delete("DELETE FROM ipk WHERE _rowid_ = 5"); err != nil {
		t.Fatalf("Delete: %v", err)
	} else if n != 1 {
		t.Fatalf("Delete (ipk, _rowid_=5): deleted %d rows, want 1", n)
	}

	// plain's rowids aren't user-controlled; find one via the read side
	// before Close, exactly like this file's other tests use p.Rows.
	{
		snap, err := db.SnapshotPager()
		if err != nil {
			t.Fatalf("SnapshotPager: %v", err)
		}
		rowids, _, err := snap.Rows("plain")
		snap.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(rowids) == 0 {
			t.Fatal("plain: no rows to pick a target rowid from")
		}
		target := rowids[0]
		if n, err := db.Delete(fmt.Sprintf("DELETE FROM plain WHERE oid = %d", target)); err != nil {
			t.Fatalf("Delete: %v", err)
		} else if n != 1 {
			t.Fatalf("Delete (plain, oid=%d): deleted %d rows, want 1", target, n)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if rowids, _, err := p.Rows("ipk"); err != nil {
		t.Fatal(err)
	} else if len(rowids) != 3 {
		t.Fatalf("ipk: got %d rows, want 3", len(rowids))
	}
	if rowids, _, err := p.Rows("plain"); err != nil {
		t.Fatal(err)
	} else if len(rowids) != 4 {
		t.Fatalf("plain: got %d rows, want 4", len(rowids))
	}
}

// TestUpdateWhereRowid mirrors TestDeleteWhereRowid for UPDATE: WHERE rowid=
// selects the target row, and the SET right-hand side may reference rowid
// too (rowEvalCtx makes it available to both).
func TestUpdateWhereRowid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upd_rowid.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,%d)", i, i*10))
	}

	if n, err := db.Update("UPDATE t SET v = rowid * 1000 WHERE rowid = 3"); err != nil {
		t.Fatalf("Update: %v", err)
	} else if n != 1 {
		t.Fatalf("Update (rowid=3): updated %d rows, want 1", n)
	}
	if n, err := db.Update("UPDATE t SET v = -1 WHERE _rowid_ >= 4"); err != nil {
		t.Fatalf("Update: %v", err)
	} else if n != 2 {
		t.Fatalf("Update (_rowid_>=4): updated %d rows, want 2", n)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rowids, rows, err := p.Rows("t")
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint64]int64{}
	for i, id := range rowids {
		got[id] = rows[i][1].I
	}
	want := map[uint64]int64{1: 10, 2: 20, 3: 3000, 4: -1, 5: -1}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("rowid %d: v = %d, want %d", id, got[id], v)
		}
	}
}

// TestDeleteFromLargeMultiPageTable forces leaf splits/interior growth at
// INSERT time (at a small page size), then deletes enough rows that the
// resulting table's row count -- and structural validity -- has to survive
// pages having emptied out (this writer's materialize-at-Close model
// sidesteps needing an actual in-place-underflow story: see writer.go).
func TestDeleteFromLargeMultiPageTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "del_large.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE big(id INTEGER PRIMARY KEY, name TEXT)")
	const n = 2000
	for i := 1; i <= n; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO big(id,name) VALUES(%d,%s)", i, sqlString(fmt.Sprintf("row-%d-padding-xxxxxxxxxxxxxxxxxxxx", i))))
	}
	deleted, err := db.Delete("DELETE FROM big WHERE id % 3 != 0") // keep only multiples of 3
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantDeleted := 0
	for i := 1; i <= n; i++ {
		if i%3 != 0 {
			wantDeleted++
		}
	}
	if deleted != wantDeleted {
		t.Fatalf("Delete: deleted %d rows, want %d", deleted, wantDeleted)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rowids, _, err := p.Rows("big")
	if err != nil {
		t.Fatal(err)
	}
	wantKept := n - wantDeleted
	if len(rowids) != wantKept {
		t.Fatalf("got %d rows, want %d", len(rowids), wantKept)
	}
	for _, id := range rowids {
		if id%3 != 0 {
			t.Errorf("surviving rowid %d is not a multiple of 3", id)
		}
	}
}

func TestUpdateNonKeyColumnGrowAndShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upd_grow_shrink.sqlite")
	db, err := engine.Create(path) // small page: a few-KB value forces overflow
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,'short')")
	mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(2,%s)", sqlString(strRepeat("z", 2000))))

	// Grow row 1's payload past this page size (forces overflow on).
	if _, err := db.Update(fmt.Sprintf("UPDATE t SET v=%s WHERE id=1", sqlString(strRepeat("a", 3000)))); err != nil {
		t.Fatalf("Update (grow): %v", err)
	}
	// Shrink row 2's payload back to something tiny (forces overflow off).
	if _, err := db.Update("UPDATE t SET v='tiny' WHERE id=2"); err != nil {
		t.Fatalf("Update (shrink): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got := rowsByID(t, p, "t")
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if s := string(got[1][1].S); s != strRepeat("a", 3000) {
		t.Errorf("row 1: v = %d bytes, want the grown 3000-byte value", len(s))
	}
	if s := string(got[2][1].S); s != "tiny" {
		t.Errorf("row 2: v = %q, want %q", s, "tiny")
	}
}

func TestUpdateIPKToNewValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upd_ipk.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,'a')")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(2,'b')")

	if _, err := db.Update("UPDATE t SET id=100 WHERE id=1"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got := rowsByID(t, p, "t")
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if _, ok := got[1]; ok {
		t.Errorf("old rowid 1 still present")
	}
	row, ok := got[100]
	if !ok {
		t.Fatalf("new rowid 100 not present")
	}
	if row[0].Typ != engine.Null || string(row[1].S) != "a" {
		t.Errorf("row at new rowid 100 = %+v, want id-column NULL (aliased) and v='a'", row)
	}
	if row2, ok := got[2]; !ok || string(row2[1].S) != "b" {
		t.Errorf("unrelated row 2 was disturbed: %+v", row2)
	}
}

func TestUpdateWhereAndCurrentValueReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upd_where_current.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, n INTEGER)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,n) VALUES(%d,%d)", i, i*10))
	}
	n, err := db.Update("UPDATE t SET n=n+1 WHERE id <= 3")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n != 3 {
		t.Fatalf("Update: matched %d rows, want 3", n)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got := rowsByID(t, p, "t")
	want := map[uint64]int64{1: 11, 2: 21, 3: 31, 4: 40, 5: 50}
	for id, wantN := range want {
		row, ok := got[id]
		if !ok {
			t.Fatalf("rowid %d missing", id)
		}
		if row[1].I != wantN {
			t.Errorf("rowid %d: n = %d, want %d", id, row[1].I, wantN)
		}
	}
}

// TestOpenWriteMixedSequence exercises INSERT, UPDATE, DELETE, INSERT across
// two separate writer sessions (engine.Create+Close, then engine.OpenWrite+Close), the
// scenario compat-harness's real-C-SQLite gate also runs.
func TestOpenWriteMixedSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,%s)", i, sqlString(fmt.Sprintf("v%d", i))))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close (1st session): %v", err)
	}

	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	mustExec(t, db2, "INSERT INTO t(id,v) VALUES(6,'v6')")
	if _, err := db2.Update("UPDATE t SET v=v||'-upd' WHERE id IN (2,4)"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := db2.Delete("DELETE FROM t WHERE id=3"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	mustExec(t, db2, "INSERT INTO t(id,v) VALUES(7,'v7')")
	if err := db2.Close(); err != nil {
		t.Fatalf("Close (2nd session): %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got := rowsByID(t, p, "t")
	want := map[uint64]string{1: "v1", 2: "v2-upd", 4: "v4-upd", 5: "v5", 6: "v6", 7: "v7"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for id, wantV := range want {
		row, ok := got[id]
		if !ok {
			t.Fatalf("rowid %d missing", id)
		}
		if string(row[1].S) != wantV {
			t.Errorf("rowid %d: v = %q, want %q", id, row[1].S, wantV)
		}
	}
	if _, ok := got[3]; ok {
		t.Errorf("deleted rowid 3 still present")
	}
}

// TestUpdateDeleteWhereSubqueries verifies the write path now evaluates a
// subquery in an UPDATE/DELETE WHERE (IN (SELECT...), correlated EXISTS)
// against a pre-mutation snapshot, like C SQLite's collect-rowids-then-
// apply model. A SET subquery reading the table being updated remains declined
// (its result would depend on partial-update ordering -- setSubqueryReadsTable).
func TestUpdateDeleteWhereSubqueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subq.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)")
	mustExec(t, db, "CREATE TABLE o(k INTEGER, w INTEGER)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,10),(2,20),(3,30),(4,40)")
	mustExec(t, db, "INSERT INTO o(k,w) VALUES(2,1),(4,1)")

	if n, err := db.Update("UPDATE t SET v = v + 100 WHERE id IN (SELECT k FROM o)"); err != nil || n != 2 {
		t.Fatalf("UPDATE ... IN (SELECT): n=%d err=%v (want n=2)", n, err)
	}
	if n, err := db.Delete("DELETE FROM t WHERE EXISTS (SELECT 1 FROM o WHERE o.k = t.id)"); err != nil || n != 2 {
		t.Fatalf("DELETE ... EXISTS: n=%d err=%v (want n=2)", n, err)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	got := rowsByID(t, p, "t")
	want := map[uint64]int64{1: 10, 3: 30} // rows 2,4 bumped then deleted; 1,3 survive
	if len(got) != len(want) {
		t.Fatalf("after: got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for id, wv := range want {
		if got[id][1].I != wv {
			t.Errorf("rowid %d: v=%d, want %d", id, got[id][1].I, wv)
		}
	}
	// An UNCORRELATED SET subquery over the target table is served: C SQLite
	// evaluates it against the pre-update image, which is exactly what this
	// path's snapshot pager hands it. t currently holds {1:10, 3:30}, so
	// max(v) is 30 and row 1 takes it.
	if err := db.Exec("UPDATE t SET v=(SELECT max(v) FROM t) WHERE id=1"); err != nil {
		t.Fatalf("UPDATE SET (uncorrelated subquery over target table): %v", err)
	}
	p2, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	after := rowsByID(t, p2, "t")
	if after[1][1].I != 30 {
		t.Errorf("rowid 1: v=%d, want 30 (the pre-update max)", after[1][1].I)
	}
	if after[3][1].I != 30 {
		t.Errorf("rowid 3: v=%d, want 30 (untouched)", after[3][1].I)
	}
	// A CORRELATED one reads the live, partially-updated table, one row at a
	// time: row 1 counts {30,30} and takes 32, then row 3 counts only itself
	// under {32,30} and takes 31 (compat-harness TestUpdateSetLiveSelfRead).
	if err := db.Exec("UPDATE t SET v=v+(SELECT count(*) FROM t t2 WHERE t2.v<=t.v)"); err != nil {
		t.Fatalf("UPDATE SET (correlated subquery over target table): %v", err)
	}
	p3, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	live := rowsByID(t, p3, "t")
	if live[1][1].I != 32 || live[3][1].I != 31 {
		t.Errorf("rowids 1,3: v=%d,%d, want 32,31", live[1][1].I, live[3][1].I)
	}
}

// TestUpdateCorrelatedSetSubquerySingleRowVsMultiRow: a CORRELATED SET
// subquery reading the table being updated, over one matched row and then two.
// With one row there is no earlier iteration to observe; with two, the second
// row's read runs against the table as the first row left it (update.c:954
// codes each SET expression inside the per-row loop, and EP_VarSelect,
// resolve.c:1403-1404, withholds its OP_Once). Both are served by lowering the
// subquery again per row (liveRowCtx, vdbe_live_read.go).
//
// Verified directly against SQLite 3.53.3: for a table t(a,b,c) holding
// (1,'x','y'),(2,'p','q'), "UPDATE t SET (b,c)=(SELECT c,b FROM t t2 WHERE
// t2.a=t.a) WHERE a=1" (single row matched) swaps b/c on row 1 using the
// pre-update values, giving (1,'y','x'); (2,'p','q') is untouched. Each row's
// subquery reads only that row, which nothing earlier has written, so the
// WHERE-less form swaps both back and forth: (1,'x','y'),(2,'q','p')
// (compat-harness TestUpdateSetLiveSelfRead's "row value swap").
func TestUpdateCorrelatedSetSubquerySingleRowVsMultiRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corr_selfref.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)")
	mustExec(t, db, "INSERT INTO t(a,b,c) VALUES(1,'x','y'),(2,'p','q')")

	// Single matched row: served, matching C SQLite's pre-update-image read.
	if err := db.Exec("UPDATE t SET (b,c) = (SELECT c, b FROM t t2 WHERE t2.a = t.a) WHERE a=1"); err != nil {
		t.Fatalf("single-row correlated self-referencing SET subquery: expected success, got %v", err)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	got := rowsByID(t, p, "t")
	if s := string(got[1][1].S); s != "y" {
		t.Errorf("rowid 1: b=%q, want %q", s, "y")
	}
	if s := string(got[1][2].S); s != "x" {
		t.Errorf("rowid 1: c=%q, want %q", s, "x")
	}
	if s := string(got[2][1].S); s != "p" {
		t.Errorf("rowid 2: b=%q, want %q (untouched)", s, "p")
	}
	if s := string(got[2][2].S); s != "q" {
		t.Errorf("rowid 2: c=%q, want %q (untouched)", s, "q")
	}

	// Two matched rows.
	if err := db.Exec("UPDATE t SET (b,c) = (SELECT c, b FROM t t2 WHERE t2.a = t.a)"); err != nil {
		t.Fatalf("multi-row correlated self-referencing SET subquery: %v", err)
	}
	p, err = db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	got = rowsByID(t, p, "t")
	for _, w := range []struct {
		id   uint64
		b, c string
	}{{1, "x", "y"}, {2, "q", "p"}} {
		if b, c := string(got[w.id][1].S), string(got[w.id][2].S); b != w.b || c != w.c {
			t.Errorf("rowid %d: (b,c)=(%q,%q), want (%q,%q)", w.id, b, c, w.b, w.c)
		}
	}
}

// TestUpdateTriggerSelfReferencingSetSubqueryFiresOncePerRow reproduces
// SQLite's own rowvalue.test 16.3-16.5 (t16c): an AFTER UPDATE trigger whose
// OWN body is an UPDATE with a SET subquery that reads the trigger's target
// table, filtered by the firing row's new.* value -- a shape a fresh
// compat-harness decline-dump scan mined from that file (the mined statement
// there, "UPDATE t16c SET a=a WHERE a=3", only makes sense in this trigger
// context: it fires t16c1, whose own UPDATE is the shape under test).
//
// Each trigger firing's own UPDATE matches exactly one row, so it falls into
// the single-row carve-out above; recursive_triggers=1 makes 16.5's firing
// cascade recursively (t16c1 fires itself once for each row it touches, each
// firing matching exactly one row in turn). Expected final table content is
// copied verbatim from SQLite 3.53.3's own do_execsql_test 16.5 expectation
// (rowvalue.test) and independently re-verified directly against a built
// 3.53.3 CLI.
func TestUpdateTriggerSelfReferencingSetSubqueryFiresOncePerRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t16c.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t16c(a, b, c, d, e)")
	mustExec(t, db, "INSERT INTO t16c VALUES(1, 'a', 'b', 'c', 'd')")
	mustExec(t, db, `CREATE TRIGGER t16c1 AFTER INSERT ON t16c BEGIN
		UPDATE t16c SET (c, d) = (SELECT 'A', 'B'), (e, b) = (SELECT 'C', 'D')
		  WHERE a = new.a-1;
	END`)
	mustExec(t, db, "INSERT INTO t16c VALUES(2, 'w', 'x', 'y', 'z')") // fires t16c1: row a=1 rewritten
	mustExec(t, db, "DROP TRIGGER t16c1")
	mustExec(t, db, "PRAGMA recursive_triggers = 1")
	mustExec(t, db, "INSERT INTO t16c VALUES(3, 'i', 'ii', 'iii', 'iv')")
	mustExec(t, db, `CREATE TRIGGER t16c1 AFTER UPDATE ON t16c WHEN new.a>1 BEGIN
		UPDATE t16c SET (e, d) = (
		  SELECT b, c FROM t16c WHERE a = new.a-1
		), (c, b) = (
		  SELECT d, e FROM t16c WHERE a = new.a-1
		) WHERE a = new.a-1;
	END`)

	if err := db.Exec("UPDATE t16c SET a=a WHERE a=3"); err != nil {
		t.Fatalf("UPDATE t16c SET a=a WHERE a=3: expected success (fires t16c1's own single-row self-referencing UPDATE), got %v", err)
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := p.QueryArgs("SELECT a, b, c, d, e FROM t16c", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := map[int64][4]string{}
	for _, r := range rows {
		got[r[0].I] = [4]string{string(r[1].S), string(r[2].S), string(r[3].S), string(r[4].S)}
	}
	want := map[int64][4]string{
		1: {"C", "B", "A", "D"},
		2: {"z", "y", "x", "w"},
		3: {"i", "ii", "iii", "iv"},
	}
	for a, wantRow := range want {
		gotRow, ok := got[a]
		if !ok {
			t.Fatalf("row a=%d missing from result", a)
		}
		if gotRow != wantRow {
			t.Errorf("row a=%d: got b,c,d,e=%q want %q", a, gotRow, wantRow)
		}
	}
}

func TestUpdateRejectsUnknownColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unkcol.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,'a')")
	if _, err := db.Update("UPDATE t SET nope=1 WHERE id=1"); err == nil {
		t.Error("Update with unknown column: expected an error, got none")
	}
}

func strRepeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}

// TestUpdateSetMinInt64LiteralStaysInteger is a direct regression test for
// sql_parser.go's parseUnary special-case: "-9223372036854775808" (the
// exact magnitude of math.MinInt64) must fold to the exact INTEGER
// math.MinInt64 in a general expression (UPDATE's SET/WHERE), not just in
// literal INSERT VALUES (insert_write.go's parseValueLiteral already had
// this special case; the general expression parser used here did not,
// until compat-harness's write fuzzer -- write_fuzz_test.go -- caught real
// SQLite doing this folding in a general expression too, e.g.
// "UPDATE t SET x = c1 + -9223372036854775808"). Without the fix, this
// package's own runtime unary-minus overflow rule (negating the positive,
// too-big-for-int64 literal 9223372036854775808 promotes to float) would
// make this a REAL(-9.223372036854776e+18), not the exact same INTEGER
// C SQLite produces -- a value that AFFECTS more than just typeof():
// e.g. it changes whether a later "x = -9223372036854775808" WHERE clause
// matches by exact integer equality or by lossy float comparison.
func TestUpdateSetMinInt64LiteralStaysInteger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minint64.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, x)")
	mustExec(t, db, "INSERT INTO t(id,x) VALUES(1,0)")

	// Standalone assignment.
	if _, err := db.Update("UPDATE t SET x = -9223372036854775808 WHERE id=1"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Embedded in a larger expression (addition) -- the case the fuzzer
	// actually found regressed independently of the standalone form.
	mustExec(t, db, "INSERT INTO t(id,x) VALUES(2,0)")
	if _, err := db.Update("UPDATE t SET x = x + -9223372036854775808 WHERE id=2"); err != nil {
		t.Fatalf("Update (embedded in addition): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rowids, rows, err := p.Rows("t")
	if err != nil {
		t.Fatal(err)
	}
	for i, rowid := range rowids {
		x := rows[i][1]
		if x.Typ != engine.Int || x.I != math.MinInt64 {
			t.Errorf("row id=%d: x = %+v, want exact engine.Int(math.MinInt64)", rowid, x)
		}
	}
}
