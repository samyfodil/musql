package driver_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// BenchmarkUnionAll is the vs-C columnar bench's "UNION ALL compound" at driver
// level: a count over a derived table that is the UNION ALL of two filtered
// scans of a 100,000-row table, laid out as segments by VACUUM.
func BenchmarkUnionAll(b *testing.B) {
	dir, err := os.MkdirTemp("", "ua")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "u.musq"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER)"); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	st, err := tx.Prepare("INSERT INTO t VALUES(?, ?, ?)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 100000; i++ {
		if _, err := st.Exec(i, i%10, i*7%1000); err != nil {
			b.Fatal(err)
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		b.Fatal(err)
	}
	for _, q := range []struct{ name, sql string }{
		{"union-all", "SELECT count(*) FROM (SELECT k FROM t WHERE k < 3 UNION ALL SELECT k FROM t WHERE k > 7)"},
		{"subquery", "SELECT count(*) FROM (SELECT k FROM t WHERE k < 3)"},
	} {
		b.Run(q.name, func(b *testing.B) {
			var n int
			if err := db.QueryRow(q.sql).Scan(&n); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := db.QueryRow(q.sql).Scan(&n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
