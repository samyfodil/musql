package driver_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// A prepared Exec reuses its bound-argument slice and the engine reuses the
// statement state between runs. Nothing a statement stored may alias either:
// every row inserted through one reused statement, across plain, trigger,
// upsert, failing and rolled-back runs, reads back exactly what was bound.
func TestReusedBuffersDoNotLeakIntoStoredRows(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, n INTEGER, s TEXT, b BLOB, f REAL)`,
		`CREATE TABLE log(id INTEGER, s TEXT)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.id, new.s); END`,
		`CREATE TABLE u(id INTEGER PRIMARY KEY, s TEXT UNIQUE, c INTEGER)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ins, err := tx.Prepare(`INSERT INTO t(id,n,s,b,f) VALUES(?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	ups, err := tx.Prepare(`INSERT INTO u(s,c) VALUES(?,1) ON CONFLICT(s) DO UPDATE SET c=c+?`)
	if err != nil {
		t.Fatal(err)
	}
	const rows = 300
	for i := 1; i <= rows; i++ {
		if _, err := ins.Exec(i, i*3, fmt.Sprintf("s%d", i), []byte{byte(i), byte(i >> 8)}, float64(i)/4); err != nil {
			t.Fatal(err)
		}
		// A failing run in between: a duplicate key undoes the statement.
		if i%10 == 0 {
			if _, err := ins.Exec(i, -1, "dup", []byte("dup"), -1.0); err == nil {
				t.Fatalf("duplicate id %d accepted", i)
			}
		}
		if _, err := ups.Exec(fmt.Sprintf("k%d", i%7), i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r, err := db.Query(`SELECT id,n,s,b,f FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n := 0
	for r.Next() {
		var id, v int
		var s string
		var b []byte
		var f float64
		if err := r.Scan(&id, &v, &s, &b, &f); err != nil {
			t.Fatal(err)
		}
		n++
		if id != n || v != n*3 || s != fmt.Sprintf("s%d", n) || len(b) != 2 || b[0] != byte(n) || b[1] != byte(n>>8) || f != float64(n)/4 {
			t.Fatalf("row %d read back as %d %d %q %v %v", n, id, v, s, b, f)
		}
	}
	if n != rows {
		t.Fatalf("%d rows, want %d", n, rows)
	}
	var logged, logSum int
	if err := db.QueryRow(`SELECT count(*), sum(id) FROM log`).Scan(&logged, &logSum); err != nil {
		t.Fatal(err)
	}
	if logged != rows || logSum != rows*(rows+1)/2 {
		t.Fatalf("trigger log: %d rows summing %d", logged, logSum)
	}
	var cs int
	if err := db.QueryRow(`SELECT sum(c) FROM u`).Scan(&cs); err != nil {
		t.Fatal(err)
	}
	// 7 first inserts of c=1, every later hit adds i.
	want := 0
	seen := map[int]bool{}
	for i := 1; i <= rows; i++ {
		if seen[i%7] {
			want += i
		} else {
			want++
			seen[i%7] = true
		}
	}
	if cs != want {
		t.Fatalf("upsert sum %d, want %d", cs, want)
	}
}
