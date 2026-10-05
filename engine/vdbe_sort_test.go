package engine_test

// ORDER BY sorter tests.

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// buildOrderByTestDB creates a test table with varied column types and values.
func buildOrderByTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sort.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	exec(`CREATE TABLE t1 (
		id INTEGER PRIMARY KEY,
		a INTEGER,
		b INTEGER,
		c TEXT,
		d REAL,
		grp INTEGER,
		mixed
	)`)

	type row struct {
		id    int64
		a     any
		b     any
		c     any
		d     any
		grp   int64
		mixed any
	}
	rows := []row{
		{1, 3, 10, "banana", 1.5, 1, int64(7)},
		{2, 1, 20, "Apple", -2.5, 1, "text"},
		{3, nil, 30, "cherry", nil, 2, 3.25},
		{4, 2, nil, "apple", 0.0, 2, nil},
		{5, -5, 10, "Banana", 100.0, 1, []byte{0x01, 0x02}},
		{6, 2, 20, nil, 2.5, 3, int64(-3)},
		{7, 3, 10, "date", -100.0, 2, "abc"},
		{8, 1, 30, "Cherry", 2.5, 3, nil},
		{9, nil, nil, "elderberry", 5.0, 1, int64(0)},
		{10, -5, 20, "fig", 1.5, 2, 3.25},
		{11, 2, 10, "Date", nil, 3, "zzz"},
		{12, 3, 30, "elderberry", 2.5, 1, int64(100)},
	}
	st, err := db.Prepare(`INSERT INTO t1 (id,a,b,c,d,grp,mixed) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := st.Exec(r.id, r.a, r.b, r.c, r.d, r.grp, r.mixed); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	// "bulk": enough rows to span several b-tree pages, with a low-
	// cardinality "val" column (heavy tie volume) so the sort actually
	// exercises real merge/compare volume, not just a handful of registers.
	// Inserted inside one transaction -- without it, 2000 individual
	// autocommit inserts each pay a full fsync-equivalent, which is what
	// made an earlier version of this test take tens of seconds.
	exec(`CREATE TABLE bulk (id INTEGER PRIMARY KEY, val INTEGER, tag TEXT)`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	bst, err := tx.Prepare(`INSERT INTO bulk (id,val,tag) VALUES (?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	const n = 2000
	for i := 0; i < n; i++ {
		if _, err := bst.Exec(int64(i+1), int64(i%37), fmt.Sprintf("tag%04d", i%11)); err != nil {
			t.Fatal(err)
		}
	}
	bst.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	return path, db
}

// mustMatchOrderBy runs a query and asserts it matches the reference database.
func mustMatchOrderBy(t *testing.T, p *engine.ReadOnlyPager, db *sql.DB, sqlText string) {
	t.Helper()
	vCols, vRows, vErr := p.QueryArgs(sqlText, nil)
	if vErr != nil {
		t.Fatalf("VDBE %q: unexpected error: %v", sqlText, vErr)
	}

	wantCols, wantRows := refRowsFor(t, db, sqlText)
	if len(vCols) != len(wantCols) {
		t.Fatalf("%q: got %d columns %v, want %d columns %v", sqlText, len(vCols), vCols, len(wantCols), wantCols)
	}
	for i := range vCols {
		if vCols[i] != wantCols[i] {
			t.Errorf("%q: column %d name = %q, want %q", sqlText, i, vCols[i], wantCols[i])
		}
	}
	if len(vRows) != len(wantRows) {
		t.Fatalf("%q: got %d rows, want %d rows\n  got:  %v\n  want: %v", sqlText, len(vRows), len(wantRows), vRows, wantRows)
	}
	for r := range vRows {
		if len(vRows[r]) != len(wantRows[r]) {
			t.Fatalf("%q: row %d: got %d values, want %d", sqlText, r, len(vRows[r]), len(wantRows[r]))
		}
		for c := range vRows[r] {
			if !valuesEqualAllowingRealStorageOptimization(vRows[r][c], wantRows[r][c]) {
				t.Errorf("%q: row %d col %d (%s): got %+v, want %+v",
					sqlText, r, c, vCols[c], vRows[r][c], wantRows[r][c])
			}
		}
	}
}

// TestVDBEOrderBy is the ORDER BY sorter's gate: every case must agree,
// loosely, with a real reference SQLite connection. wrong=0 is the pass
// condition.
func TestVDBEOrderBy(t *testing.T) {
	path, db := buildOrderByTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	cases := []string{
		// Single key, ASC and DESC.
		"SELECT id, a FROM t1 ORDER BY a",
		"SELECT id, a FROM t1 ORDER BY a DESC",
		"SELECT id, c FROM t1 ORDER BY c",
		"SELECT id, c FROM t1 ORDER BY c DESC",

		// Multiple keys, mixed ASC/DESC.
		"SELECT id, grp, a FROM t1 ORDER BY grp ASC, a DESC",
		"SELECT id, grp, a, b FROM t1 ORDER BY grp DESC, a ASC, b DESC",

		// ORDER BY an expression.
		"SELECT id, a, b FROM t1 ORDER BY a+b",
		"SELECT id, c FROM t1 ORDER BY upper(c)",
		"SELECT id, a FROM t1 ORDER BY -a",

		// ORDER BY ordinal.
		"SELECT id, a, b FROM t1 ORDER BY 2",
		"SELECT id, a, b FROM t1 ORDER BY 3 DESC, 1",
		"SELECT id, a, b FROM t1 ORDER BY +2",

		// ORDER BY output alias (precedence over an unrelated same-named
		// table column would be a different case; here the alias simply
		// renames an output column).
		"SELECT id, a AS x FROM t1 ORDER BY x",
		"SELECT id, a AS x, b AS y FROM t1 ORDER BY y, x",

		// ORDER BY a column not in the select list at all.
		"SELECT id FROM t1 ORDER BY b DESC",
		"SELECT c FROM t1 ORDER BY a, id",

		// NULLs present in the sort key (single and multiple key columns).
		"SELECT id, a FROM t1 ORDER BY a",       // a has NULLs (rows 3, 9)
		"SELECT id, a FROM t1 ORDER BY a DESC",  // NULLs sort first even DESC (classOrder, not value, flips)
		"SELECT id, c FROM t1 ORDER BY c",       // c has a NULL (row 6)
		"SELECT id, b, d FROM t1 ORDER BY b, d", // b and d both have NULLs

		// ORDER BY with WHERE.
		"SELECT id, a FROM t1 WHERE grp=1 ORDER BY a",
		"SELECT id, a FROM t1 WHERE a IS NOT NULL ORDER BY a DESC",
		"SELECT id, b FROM t1 WHERE id>3 ORDER BY b DESC, id",

		// ORDER BY + LIMIT, and + LIMIT + OFFSET.
		"SELECT id, a FROM t1 ORDER BY a LIMIT 3",
		"SELECT id, a FROM t1 ORDER BY a DESC LIMIT 3",
		"SELECT id, a FROM t1 ORDER BY a LIMIT 3 OFFSET 2",
		"SELECT id, a FROM t1 ORDER BY a LIMIT 100 OFFSET 2",
		"SELECT id, a FROM t1 ORDER BY a LIMIT 0",
		"SELECT id, grp, a FROM t1 ORDER BY grp, a LIMIT 2 OFFSET 1",

		// All storage classes / affinity in the key (the "mixed" NONE-
		// affinity column: NULL, INTEGER, REAL, TEXT, BLOB all present).
		"SELECT id, mixed FROM t1 ORDER BY mixed",
		"SELECT id, mixed FROM t1 ORDER BY mixed DESC",

		// A multi-page table: real sort volume, heavy tie duplication (val
		// repeats every 37 rows over 2000 rows), and a secondary key to
		// pin down a total order for the reference-engine.DB comparison.
		"SELECT id, val FROM bulk ORDER BY val, id",
		"SELECT id, val FROM bulk ORDER BY val DESC, id DESC",
		"SELECT id, val, tag FROM bulk ORDER BY tag, val, id LIMIT 25 OFFSET 50",
	}
	for _, sql := range cases {
		mustMatchOrderBy(t, p, db, sql)
	}
}

// TestVDBEOrderByDisassemble is a structural smoke check (not a semantic
// gate): disassembly of a representative ORDER BY program mentions every new
// sorter opcode, in a shape recognizable as the two-phase scan/drain
// skeleton this file's package doc comment describes.
func TestVDBEOrderByDisassemble(t *testing.T) {
	path, _ := buildOrderByTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	d, err := engine.DisassembleScan(p, "SELECT id, a FROM t1 ORDER BY a DESC, id LIMIT 5 OFFSET 1")
	if err != nil {
		t.Fatalf("DisassembleScan: %v", err)
	}
	for _, want := range []string{
		"SorterOpen", "MakeRecord", "SorterInsert", "SorterSort", "SorterData", "RecordColumn", "SorterNext",
		"keyinfo(nKey=2 DESC,ASC)",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("disassembly missing %q:\n%s", want, d)
		}
	}
}
