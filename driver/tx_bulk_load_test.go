package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// A transaction loading an empty table through a prepared INSERT builds its
// segments directly (engine/insert_bulk_direct.go, segBulkLoad.inTx). Every
// way the transaction can go on from there must answer as if the rows had been
// stored one by one: reads inside it, savepoints, a statement that fails part
// way, rows out of order, a rollback, a commit and a reopen.
func TestTxBulkLoad(t *testing.T) {
	open := func(t *testing.T) (*sql.DB, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "b.musq")
		db, err := sql.Open(DriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		if _, err := db.Exec("CREATE TABLE kv(k INTEGER PRIMARY KEY, v TEXT NOT NULL CHECK (v <> 'bad'))"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE other(x)"); err != nil {
			t.Fatal(err)
		}
		return db, path
	}
	load := func(t *testing.T, tx *sql.Tx, from, to int) {
		t.Helper()
		st, err := tx.Prepare("INSERT INTO kv(k, v) VALUES (?, ?)")
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		for k := from; k <= to; k++ {
			if _, err := st.Exec(k, fmt.Sprint("v", k)); err != nil {
				t.Fatalf("insert %d: %v", k, err)
			}
		}
	}
	// state is the table as count, sum of keys, and a digest of the values.
	state := func(t *testing.T, q interface {
		QueryRow(string, ...any) *sql.Row
	}) string {
		t.Helper()
		var n, sum int64
		var digest sql.NullString
		if err := q.QueryRow("SELECT count(*), coalesce(sum(k), 0), group_concat(v, ',') FROM (SELECT k, v FROM kv ORDER BY k)").Scan(&n, &sum, &digest); err != nil {
			t.Fatal(err)
		}
		d := digest.String
		if len(d) > 40 {
			d = d[:20] + "..." + d[len(d)-20:]
		}
		return fmt.Sprintf("%d/%d/%s", n, sum, d)
	}
	want := func(keys ...int) string {
		var sum int64
		var vs []string
		for _, k := range keys {
			sum += int64(k)
			vs = append(vs, fmt.Sprint("v", k))
		}
		d := strings.Join(vs, ",")
		if len(d) > 40 {
			d = d[:20] + "..." + d[len(d)-20:]
		}
		return fmt.Sprintf("%d/%d/%s", len(keys), sum, d)
	}
	span := func(from, to int) []int {
		var ks []int
		for k := from; k <= to; k++ {
			ks = append(ks, k)
		}
		return ks
	}
	const big = 70000 // more than one segment's rows

	t.Run("commit and reopen", func(t *testing.T) {
		db, path := open(t)
		engine.TxBulkRowsForTest()
		tx, _ := db.Begin()
		load(t, tx, 1, big)
		if n := engine.TxBulkRowsForTest(); n != big {
			t.Fatalf("the load took %d rows, want %d: the path did not run", n, big)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want(span(1, big)...) {
			t.Fatalf("after commit: %s", got)
		}
		db.Close()
		db2, err := sql.Open(DriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		defer db2.Close()
		if got := state(t, db2); got != want(span(1, big)...) {
			t.Fatalf("after reopen: %s", got)
		}
		var v string
		if err := db2.QueryRow("SELECT v FROM kv WHERE k = ?", 54321).Scan(&v); err != nil || v != "v54321" {
			t.Fatalf("point read: %q %v", v, err)
		}
	})

	t.Run("read inside the transaction, then go on loading", func(t *testing.T) {
		db, _ := open(t)
		tx, _ := db.Begin()
		load(t, tx, 1, 100)
		if got := state(t, tx); got != want(span(1, 100)...) {
			t.Fatalf("read inside: %s", got)
		}
		load(t, tx, 101, 200)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want(span(1, 200)...) {
			t.Fatalf("after commit: %s", got)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		db, _ := open(t)
		tx, _ := db.Begin()
		load(t, tx, 1, big)
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want() {
			t.Fatalf("after rollback: %s", got)
		}
		tx, _ = db.Begin()
		load(t, tx, 5, 9)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want(span(5, 9)...) {
			t.Fatalf("after a second load: %s", got)
		}
	})

	t.Run("savepoint", func(t *testing.T) {
		db, _ := open(t)
		tx, _ := db.Begin()
		load(t, tx, 1, 10)
		if _, err := tx.Exec("SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		load(t, tx, 11, 20)
		if _, err := tx.Exec("ROLLBACK TO s"); err != nil {
			t.Fatal(err)
		}
		load(t, tx, 21, 25)
		if _, err := tx.Exec("RELEASE s"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want(append(span(1, 10), span(21, 25)...)...) {
			t.Fatalf("%s", got)
		}
	})

	t.Run("a failing statement keeps the earlier rows", func(t *testing.T) {
		db, _ := open(t)
		tx, _ := db.Begin()
		load(t, tx, 1, 10)
		// The first row of this statement is stored, the second fails its CHECK.
		if _, err := tx.Exec("INSERT INTO kv VALUES (11, 'v11'), (12, 'bad')"); err == nil {
			t.Fatal("the CHECK did not fail")
		}
		load(t, tx, 11, 12)
		// Across a segment boundary, then failing on the last row.
		if _, err := tx.Exec(fmt.Sprintf(`WITH RECURSIVE c(i) AS (SELECT 13 UNION ALL SELECT i+1 FROM c WHERE i < %d)
			INSERT INTO kv SELECT i, CASE WHEN i = %d THEN 'bad' ELSE 'v' || i END FROM c`, big, big)); err == nil {
			t.Fatal("the CHECK did not fail")
		}
		load(t, tx, 13, 15)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want(span(1, 15)...) {
			t.Fatalf("%s", got)
		}
	})

	t.Run("out of order and duplicates", func(t *testing.T) {
		db, _ := open(t)
		tx, _ := db.Begin()
		load(t, tx, 10, 20)
		load(t, tx, 1, 3) // out of order: the load drains, the rows go on
		if _, err := tx.Exec("INSERT INTO kv VALUES (15, 'dup')"); err == nil {
			t.Fatal("a duplicate key was accepted")
		}
		load(t, tx, 21, 22)
		if _, err := tx.Exec("INSERT INTO kv VALUES (-5, 'v-5')"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want(append(append([]int{-5}, span(1, 3)...), span(10, 22)...)...) {
			t.Fatalf("%s", got)
		}
	})

	t.Run("other statements in between", func(t *testing.T) {
		db, _ := open(t)
		tx, _ := db.Begin()
		load(t, tx, 1, 50)
		if _, err := tx.Exec("INSERT INTO other SELECT count(*) FROM kv"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("UPDATE kv SET v = 'u' || k WHERE k = 7"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("DELETE FROM kv WHERE k = 8"); err != nil {
			t.Fatal(err)
		}
		load(t, tx, 51, 60)
		var auto int64
		res, err := tx.Exec("INSERT INTO kv(v) VALUES ('v61')")
		if err != nil {
			t.Fatal(err)
		}
		auto, _ = res.LastInsertId()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var x int
		if err := db.QueryRow("SELECT x FROM other").Scan(&x); err != nil || x != 50 {
			t.Fatalf("other.x = %d %v", x, err)
		}
		var v7 string
		if err := db.QueryRow("SELECT v FROM kv WHERE k = 7").Scan(&v7); err != nil || v7 != "u7" {
			t.Fatalf("k=7: %q %v", v7, err)
		}
		if auto != 61 {
			t.Fatalf("auto rowid %d, want 61", auto)
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM kv").Scan(&n); err != nil || n != 60 {
			t.Fatalf("count %d %v", n, err)
		}
	})

	t.Run("last_insert_rowid after an autocommit load", func(t *testing.T) {
		// The autocommit direct-path load (INSERT ... SELECT into an empty
		// table) left last_insert_rowid() where it was; C answers the last row.
		db, _ := open(t)
		res, err := db.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 500)
			INSERT INTO kv SELECT i * 3, 'v' || i FROM c`)
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := res.LastInsertId(); id != 1500 {
			t.Fatalf("LastInsertId %d, want 1500", id)
		}
	})

	t.Run("autoincrement rowid within the load", func(t *testing.T) {
		db, _ := open(t)
		tx, _ := db.Begin()
		st, err := tx.Prepare("INSERT INTO kv(v) VALUES (?)")
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 100; i++ {
			res, err := st.Exec(fmt.Sprint("v", i))
			if err != nil {
				t.Fatal(err)
			}
			if id, _ := res.LastInsertId(); id != int64(i) {
				t.Fatalf("row %d got rowid %d", i, id)
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := state(t, db); got != want(span(1, 100)...) {
			t.Fatalf("%s", got)
		}
	})
}
