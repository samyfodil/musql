package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"
)

// segRowSchema is a table whose columns exercise what a lazy column read has to
// get right: an INTEGER PRIMARY KEY alias, a REAL-affinity column holding
// integers, TEXT and BLOB that live outside the int64 blocks, and a nullable
// column so the NULL bitmap is consulted.
var segRowSchema = []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, r REAL, s TEXT, b BLOB, n INTEGER)`}

func segRowInserts(rows int) []string {
	rng := rand.New(rand.NewSource(11))
	var out []string
	for i := 1; i <= rows; i++ {
		n := "NULL"
		if i%3 != 0 {
			n = fmt.Sprint(i * 7)
		}
		out = append(out, fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,%d,'s%d',x'%02x%02x',%s)`,
			i, i%17, rng.Intn(1000000), i, i, i%256, (i*3)%256, n))
	}
	return out
}

// segRowSources is the same rows twice, opened read-only: col holds them as
// column blocks (VACUUMed into segments), which the lazy row source reads, and
// btree -- the reference, named for what it used to be -- holds them as the
// delta's plain records, where no segment column is decoded.
func segRowSources(t *testing.T, schema, rows []string) (btree, col *ReadOnlyPager) {
	t.Helper()
	seg := newSegPair(t, append(slices.Clone(schema), rows...)...)
	ref := newSegPair(t, schema...)
	ref.delta(rows...)
	var err error
	if btree, err = Open(ref.path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { btree.Close() })
	if col, err = Open(seg.path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { col.Close() })
	return btree, col
}

// segRowQueries are queries that must answer identically from either source.
// lazy indicates whether the lazy row path should answer it.
var segRowQueries = []struct {
	sql    string
	lazy   bool
	reason string
}{
	{`SELECT id, k, v FROM t WHERE v > 500000 ORDER BY id`, true, ""},
	{`SELECT * FROM t WHERE id < 20 ORDER BY id`, true, ""},
	{`SELECT s, b FROM t WHERE k = 3 ORDER BY id`, true, ""},
	{`SELECT r FROM t WHERE id < 10 ORDER BY id`, true, "REAL affinity over stored ints"},
	{`SELECT n FROM t WHERE id < 10 ORDER BY id`, true, "the NULL bitmap"},
	{`SELECT id FROM t WHERE n IS NULL AND id < 30 ORDER BY id`, true, ""},
	{`SELECT count(*), sum(v) FROM t WHERE k < 5`, false, "the multi-aggregate program answers it without reading rows"},
	{`SELECT k, count(*) FROM t GROUP BY k ORDER BY k`, false, "the columnar GROUP BY driver answers it"},
	{`SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 10`, false, "the bounded top-N path answers it"},
	{`SELECT id FROM t WHERE s = 's42'`, false, "an equality seeks the column's segment index (automaticSeekCandidates)"},
	{`SELECT typeof(r), typeof(n), typeof(b) FROM t WHERE id = 1`, true, "a rowid seek, positioned rather than built (segPointSeek)"},
	{`SELECT * FROM t WHERE id = 7`, true, "a whole row through a point seek"},
	{`SELECT rowid, id, r FROM t WHERE rowid = 1999`, true, ""},
	{`SELECT * FROM t WHERE id = 999999`, false, "a miss: the seek finds no row to serve"},
	{`SELECT id, v FROM t WHERE v > 900000 ORDER BY id LIMIT 5`, true, ""},
}

// TestSegLazyRowsMatchTheBtree verifies that queries answer identically whether
// rows come from the b-tree or the segments. Silent NULLs from unread columns
// are the failure mode.
func TestSegLazyRowsMatchTheBtree(t *testing.T) {
	btree, col := segRowSources(t, segRowSchema, segRowInserts(2000))
	restore := SetSegLazyRowsForTest(true)
	defer restore()

	for _, tc := range segRowQueries {
		q := tc.sql
		wantCols, wantRows, werr := btree.QueryArgs(q, nil)
		SegRowsServedForTest()
		gotCols, gotRows, gerr := col.QueryArgs(q, nil)
		// A gate that passes because the path declined is no gate.
		if n := SegRowsServedForTest(); (n > 0) != tc.lazy {
			t.Errorf("%s\n  served %d rows lazily, wanted lazy=%v (%s)", q, n, tc.lazy, tc.reason)
		}
		if (werr == nil) != (gerr == nil) {
			t.Errorf("%s\n  btree err=%v segments err=%v", q, werr, gerr)
			continue
		}
		if fmt.Sprint(wantCols) != fmt.Sprint(gotCols) {
			t.Errorf("%s\n  columns btree=%v segments=%v", q, wantCols, gotCols)
		}
		if len(gotRows) != len(wantRows) {
			t.Errorf("%s\n  %d rows from segments, %d from the b-tree", q, len(gotRows), len(wantRows))
			continue
		}
		for i := range wantRows {
			if fmt.Sprint(wantRows[i]) != fmt.Sprint(gotRows[i]) {
				t.Errorf("%s\n  row %d btree=%v segments=%v", q, i, wantRows[i], gotRows[i])
				break
			}
		}
	}
}

// TestSegLazyRowsAddColumnDefault verifies that a per-column read correctly
// handles rows narrower than the column list. A row before ALTER TABLE ADD
// COLUMN reads its DEFAULT, not NULL. A segment preserves the original width,
// so the lazy row source must too.
func TestSegLazyRowsAddColumnDefault(t *testing.T) {
	// Rows NARROWER than the column list exist on this format only as an import
	// of a C-written file: every rewrite here stores full rows. So the file is
	// built the way the converter builds one -- stored rows as they are, short
	// ones included -- and read back through the lazy row source.
	src := filepath.Join(t.TempDir(), "a.musq")
	buildDB(t, src, `CREATE TABLE t(a INT, c INT DEFAULT 5, e TEXT DEFAULT 'x')`)
	n, err := OpenWrite(src)
	if err != nil {
		t.Fatal(err)
	}
	tbl := n.findTableMetaIn(scopeMain, "t")
	raw, err := buildSegment(tbl.cols, []uint64{1, 2, 3}, [][]Value{
		{{Typ: Int, I: 1}},
		{{Typ: Int, I: 2}},
		{{Typ: Int, I: 3}, {Typ: Int, I: 5}, {Typ: Text, S: []byte("x")}},
	})
	sql := tbl.sql
	n.Discard()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "narrow.musq")
	if err := WriteSegmentFileWithCatalog(path, []ConvertedTable{{Name: "t", SQL: sql, Cols: []string{"a", "c", "e"}, IPK: -1, Segments: [][]byte{raw}}},
		ConvertedCatalog{Encoding: uint32(UTF8)}, 1, 0); err != nil {
		t.Fatal(err)
	}
	col, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer col.Close()
	restore := SetSegLazyRowsForTest(true)
	defer restore()
	// Force the lazy row source to ensure the width rule works on wide projections.
	unforce := ForceSegRowSourceForTest(true)
	defer unforce()

	// The answers C gives: a short row's missing columns read their DEFAULT.
	for _, q := range []struct{ sql, want string }{
		{`SELECT a, c, e FROM t ORDER BY a`, "[1:1|1:5|3:\"x\"| 1:2|1:5|3:\"x\"| 1:3|1:5|3:\"x\"|]"},
		{`SELECT c FROM t ORDER BY a`, "[1:5| 1:5| 1:5|]"},
		{`SELECT e FROM t ORDER BY a`, "[3:\"x\"| 3:\"x\"| 3:\"x\"|]"},
		{`SELECT count(*) FROM t WHERE c = 5`, "[1:3|]"},
		{`SELECT count(*) FROM t WHERE e = 'x'`, "[1:3|]"},
	} {
		SegRowsServedForTest()
		_, got, gerr := col.QueryArgs(q.sql, nil)
		served := SegRowsServedForTest()
		if gerr != nil {
			t.Errorf("%s: %v", q.sql, gerr)
			continue
		}
		if served == 0 {
			t.Errorf("%s: the lazy source declined -- this case is not being tested", q.sql)
		}
		if g := fmt.Sprint(typedRows(got)); g != q.want {
			t.Errorf("%s\n  got  %s\n  want %s", q.sql, g, q.want)
		}
	}
}

// TestSegRowFilterNeverDropsARow verifies that the pre-filter never eliminates
// rows that should be selected. A missed row is silently missing. This fuzzes
// predicate shapes and selectivities against the b-tree.
func TestSegRowFilterNeverDropsARow(t *testing.T) {
	btree, col := segRowSources(t, segRowSchema, segRowInserts(3000))
	restore := SetSegLazyRowsForTest(true)
	defer restore()

	var qs []string
	for _, bound := range []int{-1, 0, 1, 500000, 999998, 999999, 1000000} {
		qs = append(qs,
			fmt.Sprintf(`SELECT id FROM t WHERE v > %d ORDER BY id`, bound),
			fmt.Sprintf(`SELECT id FROM t WHERE v <= %d ORDER BY id`, bound),
			fmt.Sprintf(`SELECT id, s FROM t WHERE v > %d AND k < 5 ORDER BY id`, bound),
			fmt.Sprintf(`SELECT id FROM t WHERE v > %d OR k = 1 ORDER BY id`, bound),
			fmt.Sprintf(`SELECT id FROM t WHERE NOT (v > %d) ORDER BY id`, bound),
			fmt.Sprintf(`SELECT id FROM t WHERE v > %d AND k <> 3 AND id < 2000 ORDER BY id`, bound),
		)
	}
	// Nullable column tests: where the NULL bitmap matters, rows are UNCERTAIN
	// and must be added to the selection unconditionally.
	for _, b := range []int{-1, 0, 100, 20000, 34990, 35001} {
		qs = append(qs,
			fmt.Sprintf(`SELECT id FROM t WHERE n > %d ORDER BY id`, b),
			fmt.Sprintf(`SELECT id, n FROM t WHERE n <= %d ORDER BY id`, b),
			fmt.Sprintf(`SELECT id FROM t WHERE n > %d OR v > 999000 ORDER BY id`, b),
			fmt.Sprintf(`SELECT id FROM t WHERE n > %d AND v > 500000 ORDER BY id`, b),
			fmt.Sprintf(`SELECT id FROM t WHERE NOT (n > %d) ORDER BY id`, b),
			fmt.Sprintf(`SELECT id FROM t WHERE n BETWEEN %d AND %d ORDER BY id`, b, b+5000),
		)
	}
	qs = append(qs,
		`SELECT id FROM t WHERE n IS NULL ORDER BY id`,
		`SELECT id FROM t WHERE n IS NOT NULL ORDER BY id`,
	)
	for _, k := range []int{-1, 0, 3, 16, 17} {
		qs = append(qs,
			fmt.Sprintf(`SELECT id FROM t WHERE k = %d ORDER BY id`, k),
			fmt.Sprintf(`SELECT id FROM t WHERE k BETWEEN %d AND %d ORDER BY id`, k, k+2),
			fmt.Sprintf(`SELECT id FROM t WHERE id > %d AND v > 500000 ORDER BY id`, k*100),
			fmt.Sprintf(`SELECT id FROM t WHERE n > %d ORDER BY id`, k), // nullable column
		)
	}
	filtered := 0
	for _, q := range qs {
		_, want, werr := btree.QueryArgs(q, nil)
		SegRowsSelectedForTest()
		_, got, gerr := col.QueryArgs(q, nil)
		if n := SegRowsSelectedForTest(); n > 0 {
			filtered++
		}
		if (werr == nil) != (gerr == nil) {
			t.Errorf("%s: btree err=%v segments err=%v", q, werr, gerr)
			continue
		}
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Errorf("%s\n  btree    = %d rows %v\n  segments = %d rows %v",
				q, len(want), want, len(got), got)
		}
	}
	if filtered == 0 {
		t.Fatal("the pre-filter never fired -- this test proves nothing")
	}
	t.Logf("%d of %d queries went through the compiled pre-filter", filtered, len(qs))
}

// TestSegRowFilterAndJoins verifies that per-segment selection does not break
// on predicates depending on the outer row. A selection computed once per
// segment cannot correctly evaluate outer-dependent predicates. The sel == 0
// assertion guards against routing such predicates to the lazy source.
func TestSegRowFilterAndJoins(t *testing.T) {
	schema := []string{
		`CREATE TABLE a(id INTEGER PRIMARY KEY, k INTEGER)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER)`,
	}
	var rows []string
	for i := 1; i <= 50; i++ {
		rows = append(rows, fmt.Sprintf(`INSERT INTO a VALUES(%d,%d)`, i, i%7))
	}
	for i := 1; i <= 400; i++ {
		rows = append(rows, fmt.Sprintf(`INSERT INTO b VALUES(%d,%d,%d)`, i, i%7, i*3))
	}
	btree, col := segRowSources(t, schema, rows)
	restore := SetSegLazyRowsForTest(true)
	defer restore()

	cases := []struct {
		sql string
		// outerDep: the predicate depends on the outer row, so the pre-filter
		// MUST NOT fire. Anything else may fire or not; only the answer binds.
		outerDep bool
	}{
		{`SELECT a.id, b.id FROM a JOIN b ON a.k = b.k WHERE b.v > 1000 ORDER BY a.id, b.id`, false},
		{`SELECT count(*) FROM a, b WHERE a.k = b.k AND b.v > 500`, false},
		{`SELECT a.id FROM a WHERE a.k IN (SELECT k FROM b WHERE v > 1150) ORDER BY a.id`, false},
		{`SELECT a.id, b.v FROM a LEFT JOIN b ON a.k = b.k AND b.v > 1100 ORDER BY a.id, b.v`, false},
		{`SELECT a.id, b.id FROM a JOIN b ON a.k = b.k WHERE b.v > a.id * 10 ORDER BY a.id, b.id`, true},
		{`SELECT a.id, b.id FROM a JOIN b ON b.v > a.id ORDER BY a.id, b.id LIMIT 40`, true},
		{`SELECT a.id, (SELECT count(*) FROM b WHERE b.v > a.id * 20) FROM a ORDER BY a.id`, true},
		{`SELECT a.id FROM a WHERE (SELECT max(v) FROM b WHERE b.k = a.k) > 1100 ORDER BY a.id`, true},
	}
	for _, tc := range cases {
		_, want, werr := btree.QueryArgs(tc.sql, nil)
		SegRowsSelectedForTest()
		_, got, gerr := col.QueryArgs(tc.sql, nil)
		sel := SegRowsSelectedForTest()
		if (werr == nil) != (gerr == nil) {
			t.Errorf("%s\n  btree err=%v segments err=%v", tc.sql, werr, gerr)
			continue
		}
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Errorf("%s\n  btree    = %d rows %v\n  segments = %d rows %v",
				tc.sql, len(want), want, len(got), got)
		}
		if tc.outerDep && sel != 0 {
			t.Errorf("%s\n  the pre-filter selected %d rows for an OUTER-DEPENDENT predicate; "+
				"a per-segment selection cannot be correct for more than one outer row", tc.sql, sel)
		}
	}
}
