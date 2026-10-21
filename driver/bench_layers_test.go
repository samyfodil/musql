package driver

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"io"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// BenchmarkLayers times the same queries at each layer an application can
// call, so the cost of the driver is the difference between two lines:
//
//	engine    ReadOnlyPager.QueryArgs, no driver at all
//	conn      this package's Conn and Stmt, without database/sql
//	sql_stmt  database/sql with a prepared *sql.Stmt
//	sql_db    database/sql's db.Query, as most code calls it
//	fast      FastStmt over a *sql.Conn
//
// Every layer reads every column of every row.
func BenchmarkLayers(b *testing.B) {
	path := filepath.Join(b.TempDir(), "l.musq")
	db, err := sql.Open("musql", path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, name TEXT, v REAL)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 100000)
		 INSERT INTO t SELECT i, (i * 7919) % 10000, 'name-' || i, i * 0.5 FROM c`,
		`VACUUM`,
	} {
		if _, err := db.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	rp, err := engine.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer rp.Close()
	conn, err := (&Driver{}).Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()

	for _, w := range []struct {
		name, sql string
		arg       int64
	}{
		{"rowid_lookup", `SELECT id, sec, name, v FROM t WHERE id = ?`, 4242},
		{"index_eq_10_rows", `SELECT id, sec, name, v FROM t WHERE sec = ?`, 777},
		{"range_100_rows", `SELECT id, sec, name, v FROM t WHERE id > ? LIMIT 100`, 5000},
		{"count_filter", `SELECT count(*) FROM t WHERE v > ?`, 25000},
	} {
		b.Run(w.name+"/engine", func(b *testing.B) {
			b.ReportAllocs()
			args := []engine.Value{{Typ: engine.Int, I: w.arg}}
			for b.Loop() {
				if _, _, err := rp.QueryArgs(w.sql, args); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(w.name+"/conn", func(b *testing.B) {
			b.ReportAllocs()
			st, err := conn.(sqldriver.ConnPrepareContext).PrepareContext(ctx, w.sql)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			args := []sqldriver.NamedValue{{Ordinal: 1, Value: w.arg}}
			var dest []sqldriver.Value
			for b.Loop() {
				rows, err := st.(sqldriver.StmtQueryContext).QueryContext(ctx, args)
				if err != nil {
					b.Fatal(err)
				}
				if dest == nil {
					dest = make([]sqldriver.Value, len(rows.Columns()))
				}
				for {
					if err := rows.Next(dest); err == io.EOF {
						break
					} else if err != nil {
						b.Fatal(err)
					}
				}
				rows.Close()
			}
		})
		scan := func(b *testing.B, rows *sql.Rows) {
			cols, err := rows.Columns()
			if err != nil {
				b.Fatal(err)
			}
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			for rows.Next() {
				if err := rows.Scan(ptrs...); err != nil {
					b.Fatal(err)
				}
			}
			if err := rows.Err(); err != nil {
				b.Fatal(err)
			}
			rows.Close()
		}
		b.Run(w.name+"/sql_stmt", func(b *testing.B) {
			b.ReportAllocs()
			st, err := db.Prepare(w.sql)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			for b.Loop() {
				rows, err := st.Query(w.arg)
				if err != nil {
					b.Fatal(err)
				}
				scan(b, rows)
			}
		})
		b.Run(w.name+"/fast", func(b *testing.B) {
			b.ReportAllocs()
			sc, err := db.Conn(ctx)
			if err != nil {
				b.Fatal(err)
			}
			defer sc.Close()
			st := PrepareFast(sc, w.sql)
			args := []any{w.arg}
			var (
				id, sec int64
				name    string
				v       float64
			)
			dest := []any{&id, &sec, &name, &v}
			if w.name == "count_filter" {
				dest = dest[:1]
			}
			scanRow := func(scan func(dest ...any) error) error { return scan(dest...) }
			for b.Loop() {
				if err := st.Query(ctx, args, scanRow); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(w.name+"/sql_db", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := db.Query(w.sql, w.arg)
				if err != nil {
					b.Fatal(err)
				}
				scan(b, rows)
			}
		})
	}
}
