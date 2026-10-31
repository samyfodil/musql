package driver_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// BenchmarkReadRows reads 1000 rows of an INTEGER, a TEXT and a BLOB through
// database/sql, into typed destinations and into any.
func BenchmarkReadRows(b *testing.B) {
	db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "r.musq"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	var sb strings.Builder
	sb.WriteString("BEGIN; CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT, b BLOB);")
	for i := range 1000 {
		fmt.Fprintf(&sb, "INSERT INTO t VALUES(%d, 'row number %08d', x'%032x');", i, i, i)
	}
	sb.WriteString("COMMIT;")
	if _, err := db.Exec(sb.String()); err != nil {
		b.Fatal(err)
	}
	read := func(b *testing.B, dest func() []any) {
		b.ReportAllocs()
		for b.Loop() {
			rows, err := db.Query("SELECT id, s, b FROM t")
			if err != nil {
				b.Fatal(err)
			}
			d := dest()
			for rows.Next() {
				if err := rows.Scan(d...); err != nil {
					b.Fatal(err)
				}
			}
			if err := rows.Err(); err != nil {
				b.Fatal(err)
			}
			rows.Close()
		}
	}
	b.Run("typed", func(b *testing.B) {
		read(b, func() []any {
			var id int64
			var s string
			var blob []byte
			return []any{&id, &s, &blob}
		})
	})
	b.Run("any", func(b *testing.B) {
		read(b, func() []any {
			var id, s, blob any
			return []any{&id, &s, &blob}
		})
	})
}
