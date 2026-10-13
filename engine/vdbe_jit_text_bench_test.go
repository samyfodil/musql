package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkVMJITTextRows times the shapes the JIT's pointer stores serve --
// joins on text, projections of text and blob columns, scans returning every
// row, and UPDATEs whose WHERE walks a write cursor -- with the whole-program
// JIT off and on.
func BenchmarkVMJITTextRows(b *testing.B) {
	queries := []struct{ name, sql string }{
		{"text-join", "SELECT a.s, b.s, a.k FROM (SELECT s, k FROM t WHERE id <= 300) a, (SELECT s, k FROM t WHERE id <= 300) b WHERE a.s = b.s"},
		{"text-project", "SELECT x.s, x.b, x.s, x.k FROM (SELECT s, b, k FROM t) x"},
		{"scan-rows", "SELECT * FROM t"},
		{"text-filter", "SELECT s, b FROM t WHERE k = 3"},
	}
	for _, mode := range []struct {
		name string
		jit  bool
	}{{"off", false}, {"on", true}} {
		b.Run(mode.name, func(b *testing.B) {
			oldJ := vmJITEnabled
			vmJITEnabled = mode.jit
			defer func() { vmJITEnabled = oldJ }()
			db, err := Create(filepath.Join(b.TempDir(), "t.musq"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k, v, s TEXT, b BLOB)"); err != nil {
				b.Fatal(err)
			}
			for i := 1; i <= 2000; i++ {
				if err := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d, %d, %d, 'row-%d', x'%04x')", i, i%10, (i*7919)%1000, i%50, i)); err != nil {
					b.Fatal(err)
				}
			}
			p, err := db.SnapshotPager()
			if err != nil {
				b.Fatal(err)
			}
			defer p.Close()
			for _, q := range queries {
				b.Run(q.name, func(b *testing.B) {
					if _, _, err := p.QueryArgs(q.sql, nil); err != nil {
						b.Fatal(err)
					}
					b.ResetTimer()
					for range b.N {
						if _, _, err := p.QueryArgs(q.sql, nil); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
			b.Run("update-int-where", func(b *testing.B) {
				for i := range b.N {
					if err := db.Exec(fmt.Sprintf("UPDATE t SET v = v + 1 WHERE k = %d AND v > 5000", i%10)); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("update-text-where", func(b *testing.B) {
				for i := range b.N {
					if err := db.Exec(fmt.Sprintf("UPDATE t SET v = v + 1 WHERE s = 'row-%d' AND b IS NOT NULL", i%50)); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
