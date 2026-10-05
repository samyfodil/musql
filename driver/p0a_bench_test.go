package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// Benchmark P0a workloads measure batch insert and ordered scan performance.
const benchScanRows = 100_000

func benchOpen(b *testing.B, name string) *sql.DB {
	b.Helper()
	db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), name))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

func benchSchema(b *testing.B, db *sql.DB) {
	b.Helper()
	for _, s := range []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)",
		"CREATE INDEX t_sec ON t(sec)",
	} {
		if _, err := db.Exec(s); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkP0aBatchInsert measures batch INSERT per-row cost.
func BenchmarkP0aBatchInsert(b *testing.B) {
	db := benchOpen(b, "b7a.db")
	benchSchema(b, db)
	b.ReportAllocs()
	b.ResetTimer()
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	st, err := tx.Prepare("INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= b.N; i++ {
		if _, err := st.Exec(i, i%1000, i%10, i, 1, "w"); err != nil {
			b.Fatal(err)
		}
	}
	st.Close()
	b.StopTimer()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkP0aOrderByLimit measures ordered scan performance over 100k rows.
func BenchmarkP0aOrderByLimit(b *testing.B) {
	db := benchOpen(b, "b6.db")
	benchSchema(b, db)
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	st, err := tx.Prepare("INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= benchScanRows; i++ {
		if _, err := st.Exec(i, i%1000, i%10, (i*7919)%benchScanRows, 1, "w"); err != nil {
			b.Fatal(err)
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	q := "SELECT id, v FROM t ORDER BY v DESC LIMIT 20"
	if _, err := db.Exec(q); err != nil { // warm the schema and page cache
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.Query(q)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var id, v int64
			if err := rows.Scan(&id, &v); err != nil {
				b.Fatal(err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
		rows.Close()
		if n != 20 {
			b.Fatalf("got %d rows, want 20", n)
		}
	}
}
