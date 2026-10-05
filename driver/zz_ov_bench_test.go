package driver_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// A PREPARED point lookup through database/sql -- the exact shape the vs-C
// benchmark times. The seek itself is a binary search, so what is left is
// per-statement overhead.
func BenchmarkPreparedPointLookup(b *testing.B) {
	db, dir := ovSeed(b, 100000)
	defer os.RemoveAll(dir)
	defer db.Close()
	q, err := db.Prepare(`SELECT v FROM t WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer q.Close()
	var v int64
	q.QueryRow(1).Scan(&v)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := q.QueryRow(int64(i%100000)+1).Scan(&v); err != nil {
			b.Fatal(err)
		}
	}
}

// A PREPARED INSERT inside one transaction -- the vs-C benchmark's 7a, where
// musql is 3.58x slower than C SQLite.
func BenchmarkPreparedInsert(b *testing.B) {
	db, dir := ovSeed(b, 0)
	defer os.RemoveAll(dir)
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	st, err := tx.Prepare(`INSERT INTO t(id,v,s) VALUES(?,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Exec(int64(i)+1, int64(i), "row"); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	st.Close()
	tx.Rollback()
}

func ovSeed(b *testing.B, n int) (*sql.DB, string) {
	b.Helper()
	dir, err := os.MkdirTemp("", "ov")
	if err != nil {
		b.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "x.musq"))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`); err != nil {
		b.Fatal(err)
	}
	if n == 0 {
		return db, dir
	}
	tx, _ := db.Begin()
	st, _ := tx.Prepare(`INSERT INTO t(id,v,s) VALUES(?,?,?)`)
	for i := 1; i <= n; i++ {
		st.Exec(i, i*7%n, fmt.Sprintf("row-%d", i))
	}
	st.Close()
	tx.Commit()
	return db, dir
}

// The vs-C scoreboard's 7b: one UPDATE touching ~1/10 of the table, in
// autocommit, so the commit is part of what is timed.
func BenchmarkBulkUpdate(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db, dir := ovSeedK(b, 100000)
		b.StartTimer()
		if _, err := db.Exec(`UPDATE t SET v = v + 1 WHERE k = ?`, 3); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		db.Close()
		os.RemoveAll(dir)
	}
}

func ovSeedK(b *testing.B, n int) (*sql.DB, string) {
	b.Helper()
	dir, err := os.MkdirTemp("", "ovk")
	if err != nil {
		b.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "x.musq"))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, payload TEXT)`); err != nil {
		b.Fatal(err)
	}
	tx, _ := db.Begin()
	st, _ := tx.Prepare(`INSERT INTO t(id,k,v,payload) VALUES(?,?,?,?)`)
	for i := 1; i <= n; i++ {
		st.Exec(i, i%10, i, "payload-value")
	}
	st.Close()
	tx.Commit()
	return db, dir
}

// The vs-C scoreboard's 7c: one DELETE removing ~1/10 of the table, in
// autocommit, so the commit is part of what is timed.
func BenchmarkBulkDelete(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db, dir := ovSeedK(b, 100000)
		b.StartTimer()
		if _, err := db.Exec(`DELETE FROM t WHERE k = ?`, 3); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		db.Close()
		os.RemoveAll(dir)
	}
}
