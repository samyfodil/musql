package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Benchmarks single-row UPDATE performance scaled by table size. Measures both
// statement execution and commit overhead.
func BenchmarkWriteRowidUpdateScaling(b *testing.B) {
	for _, n := range []int{25000, 50000, 100000, 200000} {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			db, err := OpenWrite(writeScalingFixture(b, n))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			// One transaction, so the commit is not in the measurement.
			if err := db.Exec(`BEGIN`); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := db.Exec(fmt.Sprintf(`UPDATE t SET k = k + 1 WHERE id = %d`, 1+i%n)); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			db.Exec(`COMMIT`)
		})
	}
}

// BenchmarkWriteCommitShare separates per-statement cost from commit overhead.
// implements by opening a throwaway *engine.DB per statement and Closing it
// (== commit). For that cost see BenchmarkWriteOpenCloseCycle; the two differ
// by more than two orders of magnitude and conflating them is how a write
// benchmark tells you the wrong thing.
func BenchmarkWriteCommitShare(b *testing.B) {
	const n = 200000
	for _, batched := range []bool{false, true} {
		name := "autocommit"
		if batched {
			name = "one-transaction"
		}
		b.Run(name, func(b *testing.B) {
			db, err := OpenWrite(writeScalingFixture(b, n))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if batched {
				if err := db.Exec(`BEGIN`); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := db.Exec(fmt.Sprintf(`UPDATE t SET k = k + 1 WHERE id = %d`, 1+i%n)); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if batched {
				db.Exec(`COMMIT`)
			}
		})
	}
}

func writeScalingFixture(b testing.TB, rows int) string {
	path := filepath.Join(b.TempDir(), "w.musq")
	db, err := Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, payload TEXT)`); err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`CREATE INDEX tsec ON t(sec)`); err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`BEGIN`); err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= rows; i++ {
		if err := db.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d,%d,'p%d')`, i, i, i%10, i*7, i)); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Exec(`COMMIT`); err != nil {
		b.Fatal(err)
	}
	db.Close()
	return path
}

// BenchmarkWriteShapes is every write the compat benchmark measures, at two
// table sizes, so a shape that is LINEAR shows up as a doubling rather than as
// one number nobody can interpret.
func BenchmarkWriteShapes(b *testing.B) {
	shapes := []struct{ name, sql string }{
		{"update-by-rowid", `UPDATE t SET k = k + 1 WHERE id = %d`},
		{"update-by-indexed", `UPDATE t SET k = k + 1 WHERE sec = %d`},
		{"delete-by-rowid", `DELETE FROM t WHERE id = %d`},
		{"insert-one", `INSERT INTO t(sec,k,v,payload) VALUES(%d,1,1,'x')`},
	}
	for _, n := range []int{50000, 100000} {
		for _, sh := range shapes {
			b.Run(fmt.Sprintf("%s/rows=%d", sh.name, n), func(b *testing.B) {
				db, err := OpenWrite(writeScalingFixture(b, n))
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				if err := db.Exec(`BEGIN`); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := db.Exec(fmt.Sprintf(sh.sql, 1+i%n)); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				db.Exec(`COMMIT`)
			})
		}
	}
}

// BenchmarkWriteOpenCloseCycle is the driver's AUTOCOMMIT cost with no statement
// in it at all: driver opens a throwaway *engine.DB per autocommit write and
// Closes it (== commit), so whatever this costs is charged to every single-
// statement write before the statement is even compiled.
func BenchmarkWriteOpenCloseCycle(b *testing.B) {
	for _, n := range []int{20000, 40000, 80000} {
		b.Run(fmt.Sprintf("open+close/rows=%d", n), func(b *testing.B) {
			path := writeScalingFixture(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				db, err := OpenWrite(path)
				if err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("open+write+close/rows=%d", n), func(b *testing.B) {
			path := writeScalingFixture(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				db, err := OpenWrite(path)
				if err != nil {
					b.Fatal(err)
				}
				if err := db.Exec(fmt.Sprintf(`UPDATE t SET k = k + 1 WHERE id = %d`, 1+i%n)); err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
