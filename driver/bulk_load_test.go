package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestBulkLoadThroughTheDriverKeepsEveryRow: the driver commits after every
// autocommit statement, which is what lets an INSERT ... SELECT into an empty
// table build its segments directly (engine/insert_bulk_direct.go). Every shape
// around that must still keep exactly the rows it should -- the direct path,
// its out-of-order fallback, the cases it must not take, and a failure -- and
// the answers are checked again after a reopen.
func TestBulkLoadThroughTheDriverKeepsEveryRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bulk.musq")
	open := func() *sql.DB {
		db, err := sql.Open(DriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		return db
	}
	db := open()
	exec := func(q string) error { _, err := db.Exec(q); return err }
	must := func(q string) {
		t.Helper()
		if err := exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const n = 30000
	gen := func(from, to int, order string) string {
		return fmt.Sprintf(`WITH RECURSIVE c(i) AS (SELECT %d UNION ALL SELECT i+1 FROM c WHERE i < %d) SELECT i, i %% 7, 'v' || i FROM c %s`, from, to, order)
	}
	must(`CREATE TABLE direct(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`)
	must(`CREATE INDEX direct_k ON direct(k)`)
	must(`INSERT INTO direct ` + gen(1, n, ""))
	must(`CREATE TABLE fallback(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`)
	must(`INSERT INTO fallback ` + gen(1, n, "ORDER BY i DESC"))
	must(`CREATE TABLE uniq(id INTEGER PRIMARY KEY, k INTEGER, s TEXT UNIQUE)`)
	must(`INSERT INTO uniq ` + gen(1, n, ""))
	must(`CREATE TABLE nonempty(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`)
	must(`INSERT INTO nonempty VALUES (0, 0, 'first')`)
	must(`INSERT INTO nonempty ` + gen(1, n, ""))
	must(`CREATE TABLE intxn(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO intxn ` + gen(1, n, "")); err != nil {
		t.Fatal(err)
	}
	var inTx int
	if err := tx.QueryRow(`SELECT count(*) FROM intxn`).Scan(&inTx); err != nil || inTx != n {
		t.Fatalf("inside the transaction: %d rows, %v", inTx, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	must(`CREATE TABLE failing(id INTEGER PRIMARY KEY, v NOT NULL)`)
	if err := exec(`INSERT INTO failing WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 5000) SELECT i, CASE WHEN i = 4000 THEN NULL ELSE i END FROM c`); err == nil {
		t.Fatal("the NOT NULL violation was accepted")
	}
	must(`CREATE TABLE twice(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`)
	must(`INSERT INTO twice ` + gen(1, n/2, ""))
	must(`INSERT INTO twice ` + gen(n/2+1, n, "")) // no longer empty: the ordinary path

	check := func(stage string) {
		t.Helper()
		want := fmt.Sprint(n, " ", int64(n)*(n+1)/2, " v", n)
		for _, tbl := range []string{"direct", "fallback", "uniq", "intxn", "twice"} {
			var c, sum int64
			var last string
			if err := db.QueryRow(fmt.Sprintf(`SELECT count(*), sum(id), (SELECT s FROM %s ORDER BY id DESC LIMIT 1) FROM %s`, tbl, tbl)).Scan(&c, &sum, &last); err != nil {
				t.Fatalf("%s %s: %v", stage, tbl, err)
			}
			if got := fmt.Sprint(c, " ", sum, " ", last); got != want {
				t.Errorf("%s %s: %s, want %s", stage, tbl, got, want)
			}
		}
		var c int64
		if err := db.QueryRow(`SELECT count(*) FROM nonempty`).Scan(&c); err != nil || c != n+1 {
			t.Errorf("%s nonempty: %d rows, %v", stage, c, err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM failing`).Scan(&c); err != nil || c != 0 {
			t.Errorf("%s failing: %d rows survived the failed statement, %v", stage, c, err)
		}
		var k3, wantK3 int64
		for i := 1; i <= n; i++ {
			if i%7 == 3 {
				wantK3++
			}
		}
		if err := db.QueryRow(`SELECT count(*) FROM direct WHERE k = 3`).Scan(&k3); err != nil || k3 != wantK3 {
			t.Errorf("%s direct k=3 through its index: %d, %v", stage, k3, err)
		}
		var ic string
		if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
			t.Errorf("%s integrity_check: %q %v", stage, ic, err)
		}
	}
	check("live")
	db.Close()
	db = open()
	defer db.Close()
	check("reopened")
}
