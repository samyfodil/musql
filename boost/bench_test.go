package boost

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// BenchmarkBoost times each way of running the same reads: database/sql,
// boost on its own connections (both scan styles), and boost over a wrapped
// *sql.DB.
func BenchmarkBoost(b *testing.B) {
	path := filepath.Join(b.TempDir(), "b.musq")
	std, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	defer std.Close()
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, name TEXT, v REAL)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 100000)
		 INSERT INTO t SELECT i, (i * 7919) % 10000, 'name-' || i, i * 0.5 FROM c`,
		`VACUUM`,
	} {
		if _, err := std.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	db, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	wrapped, err := Wrap(std)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	type rec struct {
		ID, Sec int64
		Name    string
		V       float64
	}

	for _, w := range []struct {
		name, sql string
		arg       int64
	}{
		{"rowid_lookup", `SELECT id, sec, name, v FROM t WHERE id = ?`, 4242},
		{"index_eq_10_rows", `SELECT id, sec, name, v FROM t WHERE sec = ?`, 777},
		{"range_100_rows", `SELECT id, sec, name, v FROM t WHERE id > ? LIMIT 100`, 5000},
	} {
		var r rec
		dest := []any{&r.ID, &r.Sec, &r.Name, &r.V}
		b.Run(w.name+"/sql_db", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := std.QueryContext(ctx, w.sql, w.arg)
				if err != nil {
					b.Fatal(err)
				}
				for rows.Next() {
					if err := rows.Scan(dest...); err != nil {
						b.Fatal(err)
					}
				}
				rows.Close()
			}
		})
		run := func(b *testing.B, db *DB, scanStyle bool) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := db.Query(ctx, w.sql, w.arg)
				if err != nil {
					b.Fatal(err)
				}
				if scanStyle {
					for rows.Next() {
						if err := rows.Scan(dest...); err != nil {
							b.Fatal(err)
						}
					}
				} else {
					for rows.Next(dest...) {
					}
				}
				if err := rows.Err(); err != nil {
					b.Fatal(err)
				}
				rows.Close()
			}
		}
		b.Run(w.name+"/open", func(b *testing.B) { run(b, db, false) })
		b.Run(w.name+"/open_scan", func(b *testing.B) { run(b, db, true) })
		b.Run(w.name+"/wrap", func(b *testing.B) { run(b, wrapped, false) })
	}
}
