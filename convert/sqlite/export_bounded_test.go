package sqlite

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// boundedExportDB is a database whose export exercises every part of the
// writer: rows with overflow pages, automatic and explicit indexes filled in
// and out of order, a WITHOUT ROWID table, and catalog-only objects.
func boundedExportDB(t *testing.T, autoVacuum string, n int) string {
	path := filepath.Join(t.TempDir(), "x.musq")
	stmts := []string{}
	if autoVacuum != "" {
		stmts = append(stmts, `PRAGMA auto_vacuum = `+autoVacuum)
	}
	stmts = append(stmts,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c REAL, d BLOB, e TEXT COLLATE NOCASE)`,
		fmt.Sprintf(`INSERT INTO t(b, c, d, e) SELECT 'b'||value||substr('xyzzy', 1, value%%5),
			value%%101*0.5, CASE WHEN value%%97=0 THEN randomblob(9000) ELSE zeroblob(value%%40) END,
			CASE value%%3 WHEN 0 THEN 'Mixed'||value%%50 ELSE 'mixed'||value%%70 END
			FROM generate_series(1, %d)`, n),
		`DELETE FROM t WHERE a%7=0`,
		`CREATE INDEX t_c ON t(c DESC, e)`,
		`CREATE INDEX t_e ON t(e)`,
		`CREATE INDEX t_part ON t(length(d)) WHERE c > 10`,
		`CREATE TABLE w(k TEXT, n INT, v, PRIMARY KEY(n DESC, k)) WITHOUT ROWID`,
		fmt.Sprintf(`INSERT INTO w SELECT 'k'||(value*7919%%%d), value%%13, randomblob(value%%300) FROM generate_series(1, %d)`, n, n/2),
		`CREATE INDEX w_v ON w(v)`,
		`CREATE VIEW vw AS SELECT a, e FROM t`,
		`CREATE TRIGGER tr AFTER DELETE ON t BEGIN DELETE FROM w WHERE n = old.a; END`,
	)
	buildDB(t, path, stmts...)
	return path
}

// exportWith exports src under the given cache and sort budgets.
func exportWith(t *testing.T, src string, pageSize, cachePages, sortBytes int) []byte {
	t.Helper()
	oldCache, oldSort := exportCachePages, exportSortBytes
	if cachePages > 0 {
		exportCachePages = func(int) int { return cachePages }
		exportSortBytes = sortBytes
	}
	defer func() { exportCachePages, exportSortBytes = oldCache, oldSort }()
	dst := filepath.Join(t.TempDir(), "out.db")
	if err := Export(src, dst, pageSize); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestExportThroughTheFileIsTheSameFile is a file built with every page
// going out to the file and back, and every index sorted in runs on disk,
// holding the same bytes as one built in memory.
func TestExportThroughTheFileIsTheSameFile(t *testing.T) {
	for _, av := range []string{"", "FULL", "INCREMENTAL"} {
		src := boundedExportDB(t, av, 6000)
		for _, ps := range []int{0, 512, 65536} {
			whole := exportWith(t, src, ps, 1<<30, 1<<30)
			small := exportWith(t, src, ps, 8, 1)
			if !bytes.Equal(whole, small) {
				t.Fatalf("auto_vacuum=%q page size %d: %d bytes in memory, %d through the file, first difference at %d",
					av, ps, len(whole), len(small), firstDiff(whole, small))
			}
			p := openBytes(t, small)
			res, err := p.checkStructuralIntegrity()
			p.Close()
			if err != nil || !res.OK {
				t.Fatalf("auto_vacuum=%q page size %d: %+v %v", av, ps, res, err)
			}
		}
	}
}

// TestExportNaNIndexSortsInMemory is the one index the external sort leaves
// to the in-memory one: a REAL NaN key, which no order holds.
func TestExportNaNIndexSortsInMemory(t *testing.T) {
	src := filepath.Join(t.TempDir(), "nan.musq")
	b, err := engine.NewBuilder(src)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := b.Table("t", `CREATE TABLE t(a INTEGER PRIMARY KEY, x REAL)`, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 5000; i++ {
		x := Value{Typ: Float, F: float64(i*7919%1000) / 3}
		if i%61 == 0 {
			x.F = math.NaN()
		}
		if err := tb.Add(i, []Value{{Typ: Null}, x}); err != nil {
			t.Fatal(err)
		}
	}
	b.Object("index", "t_x", "t", `CREATE INDEX t_x ON t(x)`, 2)
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	if whole, small := exportWith(t, src, 512, 1<<30, 1<<30), exportWith(t, src, 512, 8, 1); !bytes.Equal(whole, small) {
		t.Fatalf("%d bytes in memory, %d through the file, first difference at %d", len(whole), len(small), firstDiff(whole, small))
	}
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func openBytes(t *testing.T, b []byte) *pager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "check.db")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := openPager(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
