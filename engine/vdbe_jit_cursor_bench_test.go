package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkVMJITCursorLoops times nested loops over materialized cursors with
// the whole-program JIT off, on without the cursor instructions (before they
// existed), and on with them.
func BenchmarkVMJITCursorLoops(b *testing.B) {
	queries := []struct{ name, sql string }{
		{"int-compare", "SELECT count(*) FROM (SELECT v, k FROM t WHERE id <= 400) a, (SELECT v, k FROM t WHERE id <= 400) b WHERE a.v > b.v AND a.k = b.k"},
		{"int-equijoin", "SELECT count(*) FROM (SELECT v, k FROM t WHERE id <= 600) a, (SELECT v, k FROM t WHERE id <= 600) b WHERE a.v = b.k"},
		{"text-compare", "SELECT count(*) FROM (SELECT s, k FROM t WHERE id <= 400) a, (SELECT s, k FROM t WHERE id <= 400) b WHERE a.s = b.s AND a.k < b.k"},
		{"scan-sum", "SELECT sum(x.v + x.k) FROM (SELECT v, k FROM t) x"},
	}
	for _, mode := range []struct {
		name         string
		jit, cursors bool
	}{{"off", false, false}, {"before", true, false}, {"after", true, true}} {
		b.Run(mode.name, func(b *testing.B) {
			oldJ, oldC, oldMin := vmJITEnabled, vmJITCursorOps, vmJITMinWork
			vmJITEnabled, vmJITCursorOps, vmJITMinWork = mode.jit, mode.cursors, 3
			defer func() { vmJITEnabled, vmJITCursorOps, vmJITMinWork = oldJ, oldC, oldMin }()
			db, err := Create(filepath.Join(b.TempDir(), "c.musq"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k, v, s)"); err != nil {
				b.Fatal(err)
			}
			for i := 1; i <= 2000; i++ {
				if err := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d, %d, %d, 'row-%d')", i, i%10, (i*7919)%1000, i%50)); err != nil {
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
		})
	}
}
