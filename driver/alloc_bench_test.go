package driver

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// BenchmarkDriverAllocs reports time and allocations per statement through
// database/sql, for the lookups whose cost is mostly per-statement overhead.
func BenchmarkDriverAllocs(b *testing.B) {
	db, err := sql.Open("musql", filepath.Join(b.TempDir(), "a.musq"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 100000)
		 INSERT INTO t SELECT i, (i * 7919) % 100000, i % 10, (i * 104729) % 1000000 FROM c`,
		`VACUUM`,
	} {
		if _, err := db.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	for _, w := range []struct {
		name, sql string
		arg       any
	}{
		{"rowid_lookup", `SELECT sec FROM t WHERE id = ?`, 4242},
		{"index_eq", `SELECT count(*) FROM t WHERE sec = ?`, 777},
		{"count_filter", `SELECT count(*) FROM t WHERE v > ?`, 500000},
	} {
		b.Run(w.name, func(b *testing.B) {
			b.ReportAllocs()
			var n int64
			for b.Loop() {
				if err := db.QueryRow(w.sql, w.arg).Scan(&n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
