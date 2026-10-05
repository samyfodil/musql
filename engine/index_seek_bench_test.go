package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// buildIndexSeekBenchDB builds an on-disk database with two secondary indexes
// on unique integer and text columns, populated with n rows.
func buildIndexSeekBenchDB(tb testing.TB, n int) *ReadOnlyPager {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "idxseekbench.sqlite")
	db, err := Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`); err != nil {
		tb.Fatal(err)
	}
	if err := db.Exec(`CREATE INDEX tv ON t(v)`); err != nil {
		tb.Fatal(err)
	}
	if err := db.Exec(`CREATE INDEX ts ON t(s)`); err != nil {
		tb.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, _, err := db.ExecArgs(`INSERT INTO t(id,v,s) VALUES(?,?,?)`,
			[]Value{{Typ: Int, I: int64(i)}, {Typ: Int, I: int64(i) * 7}, {Typ: Text, S: []byte(fmt.Sprintf("row-%08d", i))}}); err != nil {
			tb.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		tb.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { p.Close() })
	return p
}

// benchIndexPointLookup runs parameterized integer equality lookups on an
// indexed column, verifying results.
func benchIndexPointLookup(b *testing.B) {
	const n = 50000
	p := buildIndexSeekBenchDB(b, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%n) + 1
		_, rows, err := p.QueryArgs("SELECT id FROM t WHERE v = ?", []Value{{Typ: Int, I: id * 7}})
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 1 || rows[0][0].I != id {
			b.Fatalf("v=%d: unexpected result %v", id*7, rows)
		}
	}
}

// BenchmarkIndexPointLookup measures secondary-index equality lookups.
func BenchmarkIndexPointLookup(b *testing.B) { benchIndexPointLookup(b) }

// benchIndexTextLookup runs parameterized text equality lookups on an indexed
// column.
func benchIndexTextLookup(b *testing.B) {
	const n = 50000
	p := buildIndexSeekBenchDB(b, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%n) + 1
		key := fmt.Sprintf("row-%08d", id)
		_, rows, err := p.QueryArgs("SELECT id FROM t WHERE s = ?", []Value{{Typ: Text, S: []byte(key)}})
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 1 || rows[0][0].I != id {
			b.Fatalf("s=%q: unexpected result %v", key, rows)
		}
	}
}

// BenchmarkIndexTextLookup measures text-index equality lookups.
func BenchmarkIndexTextLookup(b *testing.B) { benchIndexTextLookup(b) }
