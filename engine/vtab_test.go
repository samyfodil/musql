package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// vtabRowsInt flattens a single-int-column result to []int64 for comparison.
func vtabRowsInt(t *testing.T, rows [][]Value, col int) []int64 {
	t.Helper()
	out := make([]int64, len(rows))
	for i, r := range rows {
		if col >= len(r) {
			t.Fatalf("row %d has %d columns, want > %d", i, len(r), col)
		}
		if r[col].Typ != Int {
			t.Fatalf("row %d col %d: type %d, want Int", i, col, r[col].Typ)
		}
		out[i] = r[col].I
	}
	return out
}

func eqInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newEmptyPager returns a read-only pager over a fresh empty database.
func newEmptyPager(t *testing.T) *ReadOnlyPager {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "vtab.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	return p
}

func TestGenerateSeriesSemantics(t *testing.T) {
	p := newEmptyPager(t)
	cases := []struct {
		q    string
		want []int64
	}{
		{"SELECT value FROM generate_series(1,5)", []int64{1, 2, 3, 4, 5}},
		{"SELECT value FROM generate_series(0,10,2)", []int64{0, 2, 4, 6, 8, 10}},
		{"SELECT value FROM generate_series(5,1,-1)", []int64{5, 4, 3, 2, 1}},
		{"SELECT value FROM generate_series(5,1,-2)", []int64{5, 3, 1}},
		{"SELECT value FROM generate_series(1,1)", []int64{1}},
		{"SELECT value FROM generate_series(3,1)", nil}, // ascending, start>stop -> empty
		{"SELECT value FROM generate_series(1,NULL)", nil},
		{"SELECT value FROM generate_series(NULL,5)", nil},
		{"SELECT value FROM generate_series(1,10) WHERE value<4", []int64{1, 2, 3}},
		{"SELECT value FROM generate_series WHERE start=2 AND stop=6", []int64{2, 3, 4, 5, 6}},
		{"SELECT value FROM generate_series WHERE start=2 AND stop=8 AND step=3", []int64{2, 5, 8}},
		{"SELECT value FROM generate_series(1,3) ORDER BY value DESC", []int64{3, 2, 1}},
	}
	for _, c := range cases {
		_, rows, err := p.Query(c.q)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.q, err)
			continue
		}
		got := vtabRowsInt(t, rows, 0)
		if !eqInts(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.q, got, c.want)
		}
	}
}

// TestGenerateSeriesStarExcludesHidden verifies "*" hides HIDDEN columns.
func TestGenerateSeriesStarExcludesHidden(t *testing.T) {
	p := newEmptyPager(t)
	cols, rows, err := p.Query("SELECT * FROM generate_series(2,6,2)")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0] != "value" {
		t.Fatalf("SELECT * columns = %v, want [value]", cols)
	}
	if len(rows) != 3 || len(rows[0]) != 1 {
		t.Fatalf("SELECT * shape = %d rows x %d cols, want 3x1", len(rows), len(rows[0]))
	}
	cols2, rows2, err := p.Query("SELECT value, start, stop, step FROM generate_series(2,6,2)")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols2) != 4 {
		t.Fatalf("explicit hidden columns = %v, want 4", cols2)
	}
	// Check hidden columns match inputs.
	for _, r := range rows2 {
		if r[1].I != 2 || r[2].I != 6 || r[3].I != 2 {
			t.Fatalf("hidden echo = %v, want start=2 stop=6 step=2", r)
		}
	}
}

// TestGenerateSeriesRowid verifies rowid equals the series value.
func TestGenerateSeriesRowid(t *testing.T) {
	p := newEmptyPager(t)
	_, rows, err := p.Query("SELECT rowid, value FROM generate_series(10,13)")
	if err != nil {
		t.Fatal(err)
	}
	if got := vtabRowsInt(t, rows, 0); !eqInts(got, []int64{10, 11, 12, 13}) {
		t.Errorf("rowids = %v, want [10 11 12 13]", got)
	}
	if got := vtabRowsInt(t, rows, 1); !eqInts(got, []int64{10, 11, 12, 13}) {
		t.Errorf("values = %v, want [10 11 12 13]", got)
	}
}

// TestGenerateSeriesSpanOverflow verifies overflow handling at span limits.
func TestGenerateSeriesSpanOverflow(t *testing.T) {
	p := newEmptyPager(t)
	if _, _, err := p.Query("SELECT value FROM generate_series(-9223372036854775808, 9223372036854775807, 2) WHERE value BETWEEN 1 AND 5"); err == nil {
		t.Error("the whole-range series answered; want the materialization limit")
	}
	_, rows, err := p.Query("SELECT value FROM generate_series(9223372036854760000,9223372036854775807,10000)")
	if err != nil {
		t.Fatal(err)
	}
	if got := vtabRowsInt(t, rows, 0); !eqInts(got, []int64{9223372036854760000, 9223372036854770000}) {
		t.Errorf("values = %v", got)
	}
}

// TestGenerateSeriesUnboundedDeclines verifies unbounded series are declined.
func TestGenerateSeriesUnboundedDeclines(t *testing.T) {
	p := newEmptyPager(t)
	if _, _, err := p.Query("SELECT value FROM generate_series WHERE start=5"); err == nil {
		t.Error("expected an error for an unbounded generate_series, got none")
	}
}

// TestCreateVirtualTablePersistAndDrop exercises the CREATE VIRTUAL TABLE /
// DROP TABLE mechanism end-to-end: a named vtab instance persists across a
// SnapshotPager round-trip, is queryable, and is gone after DROP TABLE.
//
// The fixture is fts4 rather than generate_series because a CREATE of the
// latter is an ERROR in C SQLite -- series.c registers it with a NULL
// xCreate (ext/misc/series.c:906-912), which vtabCallConstructor answers with
// "no such module: generate_series" (vtab.c:789-790, whose test is
// `pMod==0 || xCreate==0 || xDestroy==0`). It is usable in a FROM clause only.
func TestCreateVirtualTablePersistAndDrop(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "cv.musq"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE VIRTUAL TABLE ser USING fts4(x)"); err != nil {
		t.Fatalf("CREATE VIRTUAL TABLE: %v", err)
	}
	if err := db.Exec("INSERT INTO ser(x) VALUES('alpha')"); err != nil {
		t.Fatalf("INSERT into the created vtab: %v", err)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := p.Query("SELECT x FROM ser")
	if err != nil {
		t.Fatalf("query created vtab: %v", err)
	}
	if len(rows) != 1 || string(rows[0][0].S) != "alpha" {
		t.Errorf("created vtab rows = %v, want one 'alpha' row", rows)
	}
	// The vtab is recorded in sqlite_schema as a type="table" row with its
	// verbatim CREATE VIRTUAL TABLE text and rootpage 0.
	_, srows, err := p.Query("SELECT type, name, sql FROM sqlite_schema WHERE name='ser'")
	if err != nil {
		t.Fatalf("schema query: %v", err)
	}
	if len(srows) != 1 || string(srows[0][0].S) != "table" || string(srows[0][1].S) != "ser" {
		t.Fatalf("sqlite_schema row = %v, want one table/ser row", srows)
	}

	if err := db.Exec("DROP TABLE ser"); err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	p2, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p2.Query("SELECT x FROM ser"); err == nil {
		t.Error("expected 'no such table' after DROP TABLE, got none")
	}
}

// TestCreateVirtualTableEponymousOnlyModuleRefused pins what the fixture change
// above rests on: a module C SQLite registers with no xCreate cannot be
// CREATEd, and answers the same "no such module" an unregistered name does.
// Verified against 3.53.3 for pragma_table_list (which that build has) and read
// off series.c:906-912 for generate_series (which it does not).
func TestCreateVirtualTableEponymousOnlyModuleRefused(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "ep.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE ser USING generate_series",
		"CREATE VIRTUAL TABLE pl USING pragma_table_list",
		"CREATE VIRTUAL TABLE temp.ser2 USING generate_series",
	} {
		err := db.Exec(s)
		if err == nil {
			t.Errorf("%s was accepted; C SQLite answers \"no such module\"", s)
		} else if !strings.Contains(err.Error(), "no such module") {
			t.Errorf("%s declined with %q, want \"no such module\"", s, err)
		}
	}
	// ...while the eponymous USE of it still works.
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	if _, rows, qerr := p.Query("SELECT value FROM generate_series WHERE start=1 AND stop=3"); qerr != nil {
		t.Errorf("eponymous generate_series: %v", qerr)
	} else if got := vtabRowsInt(t, rows, 0); !eqInts(got, []int64{1, 2, 3}) {
		t.Errorf("eponymous generate_series rows = %v, want [1 2 3]", got)
	}
}

// TestVirtualTableRecoversAcrossOpenWrite verifies a persisted vtab survives a
// write-session round-trip (OpenWrite recovery) -- a second DDL statement after
// the vtab exists must not choke on its type="table" schema row.
func TestVirtualTableRecoversAcrossOpenWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recover.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE VIRTUAL TABLE ser USING fts4(x)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite (recovery over a vtab schema row): %v", err)
	}
	// A subsequent write must succeed despite the vtab row being present.
	if err := db2.Exec("CREATE TABLE t(a)"); err != nil {
		t.Fatalf("CREATE TABLE after vtab recovery: %v", err)
	}
	p, err := db2.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Query("SELECT x FROM ser"); err != nil {
		t.Fatalf("query recovered vtab: %v", err)
	}
}
