package engine

import (
	"path/filepath"
	"testing"
)

// BenchmarkAllocs reports allocations per statement for the benchmark table's
// workloads through the direct engine path, over a 100k-row VACUUMed table.
// Run with -benchmem; it exists to keep allocation counts visible.
func BenchmarkAllocs(b *testing.B) {
	path := filepath.Join(b.TempDir(), "a.musq")
	buildDB(b, path,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 100000)
		 INSERT INTO t SELECT i, (i * 7919) % 100000, i % 10, (i * 104729) % 1000000, 1 + (i * 31) % 100000 FROM c`)
	execDB(b, path, `VACUUM`)
	rp, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer rp.Close()
	iv := func(n int64) []Value { return []Value{{Typ: Int, I: n}} }
	for _, w := range []struct {
		name string
		sql  string
		args []Value
	}{
		{"count_filter", `SELECT count(*) FROM t WHERE v > ?`, iv(500000)},
		{"rowid_lookup", `SELECT sec FROM t WHERE id = ?`, iv(4242)},
		{"index_eq", `SELECT count(*) FROM t WHERE sec = ?`, iv(777)},
		{"sum_filter", `SELECT sum(v) FROM t WHERE v > ?`, iv(500000)},
		{"group_by", `SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k`, nil},
		{"top_n", `SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 20`, nil},
		{"or_pred", `SELECT count(*) FROM t WHERE v > ? OR k = 3`, iv(900000)},
	} {
		b.Run(w.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := rp.QueryArgs(w.sql, w.args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
