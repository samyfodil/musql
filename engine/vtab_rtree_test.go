package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"path/filepath"
	"strings"
	"testing"
)

// newRtreeDB creates a fresh writable engine engine.DB for rtree tests.
func newRtreeDB(t *testing.T) *engine.Session {
	t.Helper()
	db, err := engine.Create(filepath.Join(t.TempDir(), "rtree.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return db
}

// queryRtree runs q against a fresh snapshot of db and returns the rows.
func queryRtree(t *testing.T, db snapshotter, q string) [][]engine.Value {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := p.Query(q)
	if err != nil {
		t.Fatalf("Query %q: %v", q, err)
	}
	return rows
}

// TestRtreeCreateColumnCounts locks in rtree's CREATE-time column-count
// validation, byte-matching SQLite/mattn's own error text.
func TestRtreeCreateColumnCounts(t *testing.T) {
	cases := []struct {
		sql     string
		wantErr string // "" means success
	}{
		{"CREATE VIRTUAL TABLE a1 USING rtree(id)", "Too few columns for an rtree table"},
		{"CREATE VIRTUAL TABLE a2 USING rtree(id, x)", "Too few columns for an rtree table"},
		{"CREATE VIRTUAL TABLE ok3 USING rtree(id, minX, maxX)", ""},
		{"CREATE VIRTUAL TABLE a4 USING rtree(id, x0, x1, y0)", "Wrong number of columns for an rtree table"},
		{"CREATE VIRTUAL TABLE ok5 USING rtree(id, x0, x1, y0, y1)", ""},
		{"CREATE VIRTUAL TABLE ok11 USING rtree(id, a,b,c,d,e,f,g,h,i,j)", ""},
		{"CREATE VIRTUAL TABLE a13 USING rtree(id, a,b,c,d,e,f,g,h,i,j,k,l)", "Too many columns for an rtree table"},
	}
	for _, c := range cases {
		db := newRtreeDB(t)
		err := db.Exec(c.sql)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.sql, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected error %q, got nil", c.sql, c.wantErr)
			continue
		}
		if got := err.Error(); !contains(got, c.wantErr) {
			t.Errorf("%s: error %q does not contain %q", c.sql, got, c.wantErr)
		}
	}
}

// TestRtreeFloat32Rounding locks in the float32 down/up coordinate rounding
// (rtreeValueDown for a min column, rtreeValueUp for a max column), matching
// mattn's real rtree byte-for-byte for the canonical 0.1..0.4 box.
func TestRtreeFloat32Rounding(t *testing.T) {
	db := newRtreeDB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX, minY, maxY)")
	mustExec(t, db, "INSERT INTO t VALUES(1, 0.1, 0.2, 0.3, 0.4)")
	rows := queryRtree(t, db, "SELECT minX, maxX, minY, maxY FROM t WHERE id=1")
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	want := []float64{
		0.09999998658895493, // 0.1 rounded DOWN to float32
		0.20000000298023224, // 0.2 rounded UP
		0.2999999523162842,  // 0.3 rounded DOWN
		0.4000000059604645,  // 0.4 rounded UP
	}
	for i, w := range want {
		if rows[0][i].Typ != engine.Float || rows[0][i].F != w {
			t.Errorf("col %d = %v, want engine.Float %v", i, rows[0][i], w)
		}
	}
}

// TestRtreeConstraints locks in the min<=max constraint and duplicate-id
// UNIQUE constraint, matching mattn's error text.
func TestRtreeConstraints(t *testing.T) {
	db := newRtreeDB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX, minY, maxY)")
	mustExec(t, db, "INSERT INTO t VALUES(1, 0.0, 1.0, 0.0, 1.0)")

	if err := db.Exec("INSERT INTO t VALUES(2, 9.0, 1.0, 0.0, 0.0)"); err == nil ||
		!contains(err.Error(), "rtree constraint failed: t.(minX<=maxX)") {
		t.Errorf("min>max: got %v, want rtree constraint failed: t.(minX<=maxX)", err)
	}
	// A violation on the SECOND dimension names that dimension's columns.
	if err := db.Exec("INSERT INTO t VALUES(2, 0.0, 1.0, 5.0, 1.0)"); err == nil ||
		!contains(err.Error(), "rtree constraint failed: t.(minY<=maxY)") {
		t.Errorf("min>max dim2: got %v, want rtree constraint failed: t.(minY<=maxY)", err)
	}
	if err := db.Exec("INSERT INTO t VALUES(1, 0.0, 0.0, 0.0, 0.0)"); err == nil ||
		!contains(err.Error(), "UNIQUE constraint failed: t.id") {
		t.Errorf("dup id: got %v, want UNIQUE constraint failed: t.id", err)
	}
	// The failed inserts left exactly the one original row.
	if rows := queryRtree(t, db, "SELECT count(*) FROM t"); rows[0][0].I != 1 {
		t.Errorf("count after failed inserts = %v, want 1", rows[0][0])
	}
}

// TestRtreeAutoRowidAndColumns exercises NULL-id auto-assignment, an explicit
// id acting as the rowid, and the rowid==id equality.
func TestRtreeAutoRowidAndColumns(t *testing.T) {
	db := newRtreeDB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)")
	mustExec(t, db, "INSERT INTO t VALUES(5, 1.0, 2.0)")
	mustExec(t, db, "INSERT INTO t VALUES(NULL, 3.0, 4.0)")       // -> rowid 6
	mustExec(t, db, "INSERT INTO t(minX, maxX) VALUES(5.0, 6.0)") // -> rowid 7, id omitted
	rows := queryRtree(t, db, "SELECT rowid, id FROM t ORDER BY id")
	got := make([][2]int64, len(rows))
	for i, r := range rows {
		got[i] = [2]int64{r[0].I, r[1].I}
	}
	want := [][2]int64{{5, 5}, {6, 6}, {7, 7}}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestRtreeUpdateDelete exercises UPDATE (id reassignment, coordinate change,
// and a constraint-violating update that leaves the row unchanged) and DELETE.
func TestRtreeUpdateDelete(t *testing.T) {
	db := newRtreeDB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)")
	mustExec(t, db, "INSERT INTO t VALUES(1, 1.0, 2.0)")
	mustExec(t, db, "INSERT INTO t VALUES(2, 3.0, 4.0)")

	mustExec(t, db, "UPDATE t SET id=9 WHERE id=1")
	if rows := queryRtree(t, db, "SELECT id, minX FROM t WHERE id=9"); len(rows) != 1 || rows[0][1].F != 1.0 {
		t.Errorf("after id update: %v", rows)
	}
	mustExec(t, db, "UPDATE t SET maxX=100.0 WHERE id=2")
	if rows := queryRtree(t, db, "SELECT maxX FROM t WHERE id=2"); rows[0][0].F != 100.0 {
		t.Errorf("coord update failed: %v", rows)
	}
	// A constraint-violating UPDATE errors and changes nothing.
	if err := db.Exec("UPDATE t SET maxX=0.0 WHERE id=2"); err == nil ||
		!contains(err.Error(), "rtree constraint failed: t.(minX<=maxX)") {
		t.Errorf("bad update: got %v", err)
	}
	if rows := queryRtree(t, db, "SELECT maxX FROM t WHERE id=2"); rows[0][0].F != 100.0 {
		t.Errorf("row changed after failed update: %v", rows)
	}
	mustExec(t, db, "DELETE FROM t WHERE id=9")
	if rows := queryRtree(t, db, "SELECT id FROM t ORDER BY id"); len(rows) != 1 || rows[0][0].I != 2 {
		t.Errorf("after delete: %v", rows)
	}
}

// TestRtreeSpatialQuery verifies that a spatial-overlap query returns exactly
// the rows whose coordinates satisfy the WHERE predicate -- the whole point of
// the full-scan + engine-WHERE design.
func TestRtreeSpatialQuery(t *testing.T) {
	db := newRtreeDB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX, minY, maxY)")
	mustExec(t, db, "INSERT INTO t VALUES(1, 0.0, 1.0, 0.0, 1.0)")
	mustExec(t, db, "INSERT INTO t VALUES(2, 5.0, 6.0, 5.0, 6.0)")
	mustExec(t, db, "INSERT INTO t VALUES(3, 2.0, 3.0, 2.0, 3.0)")
	mustExec(t, db, "INSERT INTO t VALUES(4, -1.0, 0.5, -1.0, 0.5)")

	rows := queryRtree(t, db, "SELECT id FROM t WHERE minX>=0.0 AND maxX<=3.5 ORDER BY id")
	var ids []int64
	for _, r := range rows {
		ids = append(ids, r[0].I)
	}
	want := []int64{1, 3}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids = %v, want %v", ids, want)
			break
		}
	}
}

// TestRtreeI32 locks in the integer variant: coordinates stored as int32
// (wrapping on overflow), returned as INTEGER, with the same min<=max check.
func TestRtreeI32(t *testing.T) {
	db := newRtreeDB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE ti USING rtree_i32(id, x0, x1)")
	mustExec(t, db, "INSERT INTO ti VALUES(1, 3, 7)")
	mustExec(t, db, "INSERT INTO ti VALUES(2, 3000000000, 3000000001)") // > int32 -> wraps
	if err := db.Exec("INSERT INTO ti VALUES(3, 5, 3)"); err == nil ||
		!contains(err.Error(), "rtree constraint failed: ti.(x0<=x1)") {
		t.Errorf("i32 min>max: got %v", err)
	}
	rows := queryRtree(t, db, "SELECT id, x0, x1 FROM ti ORDER BY id")
	want := [][3]int64{{1, 3, 7}, {2, -1294967296, -1294967295}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %v", rows, want)
	}
	for i, w := range want {
		if rows[i][0].Typ != engine.Int || rows[i][1].Typ != engine.Int || rows[i][2].Typ != engine.Int {
			t.Fatalf("row %d not all INT: %v", i, rows[i])
		}
		got := [3]int64{rows[i][0].I, rows[i][1].I, rows[i][2].I}
		if got != w {
			t.Errorf("row %d = %v, want %v", i, got, w)
		}
	}
}

// TestRtreeTextCoercion locks in numeric-text coordinate/id coercion.
func TestRtreeTextCoercion(t *testing.T) {
	db := newRtreeDB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING rtree(id, lo, hi)")
	mustExec(t, db, "INSERT INTO t VALUES('7', '3.5', '4.5')")
	rows := queryRtree(t, db, "SELECT id, lo, hi FROM t WHERE id=7")
	if len(rows) != 1 || rows[0][0].I != 7 || rows[0][1].F != 3.5 || rows[0][2].F != 4.5 {
		t.Errorf("text coercion: %v", rows)
	}
}

// TestRtreePersistence verifies a writable rtree's rows survive Close/engine.OpenWrite
// (persisted in a real b-tree under the vtab's rootpage), which is what makes
// the vtab usable across the driver's per-statement engine sessions.
func TestRtreePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)")
	mustExec(t, db, "INSERT INTO t VALUES(1, 1.0, 2.0)")
	mustExec(t, db, "INSERT INTO t VALUES(2, 3.0, 4.0)")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	rows := queryRtree(t, db2, "SELECT id, minX, maxX FROM t ORDER BY id")
	if len(rows) != 2 || rows[0][0].I != 1 || rows[1][0].I != 2 || rows[0][1].F != 1.0 || rows[1][2].F != 4.0 {
		t.Fatalf("recovered rows = %v", rows)
	}
	// A further write after recovery still works and persists.
	mustExec(t, db2, "INSERT INTO t VALUES(3, 5.0, 6.0)")
	mustExec(t, db2, "DELETE FROM t WHERE id=1")
	if rows := queryRtree(t, db2, "SELECT id FROM t ORDER BY id"); len(rows) != 2 || rows[0][0].I != 2 || rows[1][0].I != 3 {
		t.Errorf("after post-recovery write: %v", rows)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
