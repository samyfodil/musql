package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestInSubqueryLeavesOutRepeats: an IN subquery's program leaves out a row
// identical to one it returned, and every membership answers as it does with
// every row kept: values that are equal but not identical (1, 1.0, '1', 'a'
// and 'A' under NOCASE) are each kept, NULLs keep NOT IN's NULL, and LIMIT
// counts the rows before any is left out. Run by the VDBE and by native code,
// which hands its rows over in batches.
func TestInSubqueryLeavesOutRepeats(t *testing.T) {
	setup := []string{
		`CREATE TABLE s(a INTEGER, b TEXT COLLATE NOCASE, c REAL, d)`,
		`CREATE TABLE p(x, y)`,
	}
	ds := []string{"1", "1.0", "'1'", "NULL", "'a'", "'A'", "x'01'", "2.5", "-7"}
	for i := range 400 {
		setup = append(setup, fmt.Sprintf(`INSERT INTO s VALUES(%d, '%s', %d.5, %s)`, i%13, []string{"a", "A", "b", "1"}[i%4], i%5, ds[i%len(ds)]))
	}
	for i := range 40 {
		setup = append(setup, fmt.Sprintf(`INSERT INTO p VALUES(%s, %s)`, ds[i%len(ds)], ds[(i*5)%len(ds)]))
	}
	setup = append(setup, `INSERT INTO p VALUES(5, 'B'), (12, 'a'), (3.5, 2), (13, NULL)`)
	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	queries := []string{
		`SELECT x, x IN (SELECT a FROM s) FROM p`,
		`SELECT x, x NOT IN (SELECT a FROM s WHERE a > 3) FROM p`,
		`SELECT y, y IN (SELECT b FROM s) FROM p`,
		`SELECT x, x IN (SELECT c FROM s) FROM p`,
		`SELECT x, x IN (SELECT d FROM s), x NOT IN (SELECT d FROM s) FROM p`,
		`SELECT y, y IN (SELECT d FROM s WHERE d IS NOT NULL) FROM p`,
		`SELECT x, (x, y) IN (SELECT d, b FROM s) FROM p`,
		`SELECT x, x IN (SELECT a FROM s ORDER BY a DESC LIMIT 3) FROM p`,
		`SELECT x, x IN (SELECT a FROM s LIMIT 20 OFFSET 7) FROM p`,
		`SELECT x, x IN (SELECT a + 0 FROM s WHERE b = 'A') FROM p`,
		`SELECT count(*) FROM p WHERE x IN (SELECT a FROM s) OR y IN (SELECT b FROM s)`,
		`SELECT x, x IN (SELECT s1.a FROM s s1, s s2 WHERE s1.a = s2.a AND s1.c <> s2.c) FROM p`,
		`SELECT y, y IN (SELECT s1.b FROM s s1, s s2 WHERE s1.a = s2.a AND s1.d IS NOT s2.d) FROM p`,
	}
	var p *ReadOnlyPager
	run := func(q string) string {
		_, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "vdbe", true: "native"}[native], func(t *testing.T) {
			if native {
				if !vmJITEnabled {
					t.Skip("JIT not enabled on this platform")
				}
				lowEntryThreshold(t)
			} else {
				old := vmJITEnabled
				vmJITEnabled = false
				defer func() { vmJITEnabled = old }()
			}
			// Its own connection, so no program compiled by the other half
			// is taken from the plan cache.
			var err error
			if p, err = Open(path); err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			native0 := memberRowsLeftOutNative.Load()
			for _, q := range queries {
				memberRowsOffForTest = true
				want := run(q)
				memberRowsOffForTest = false
				before := memberRowsLeftOut.Load()
				got := run(q)
				if got != want {
					t.Errorf("%s:\n got  %.300s\n want %.300s", q, got, want)
				}
				if memberRowsLeftOut.Load() == before {
					t.Errorf("%s: no row left out", q)
				}
			}
			if native && memberRowsLeftOutNative.Load() == native0 {
				t.Error("native code left no row out")
			}
		})
	}
}
