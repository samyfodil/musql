package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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
	// Rowids the engine assigns: the direct path keeps its rows out of the
	// row store NewRowid reads, and handing out rowid 1 again failed every
	// such INSERT with "UNIQUE constraint failed: <t>.rowid".
	must(`CREATE TABLE auto(k, s)`)
	must(`INSERT INTO auto(k, s) WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 5000) SELECT i % 7, 'v' || i FROM c`)
	must(`CREATE TABLE ainc(id INTEGER PRIMARY KEY AUTOINCREMENT, s)`)
	must(`INSERT INTO ainc(s) WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 300) SELECT 'v' || i FROM c`)
	must(`CREATE TABLE mixed(x)`)
	must(`INSERT INTO mixed(rowid, x) SELECT 5, 1 UNION ALL SELECT NULL, 2 UNION ALL SELECT 100, 3 UNION ALL SELECT NULL, 4`)
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
		for q, want := range map[string]string{
			`SELECT count(*), min(rowid), max(rowid), sum(s = 'v' || rowid) FROM auto`:                  "5000 1 5000 5000",
			`SELECT count(*), max(id), (SELECT seq FROM sqlite_sequence WHERE name = 'ainc') FROM ainc`: "300 300 300",
			`SELECT group_concat(rowid) || ' ' || group_concat(x) FROM mixed`:                           "5,6,100,101 1,2,3,4",
		} {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatalf("%s %s: %v", stage, q, err)
			}
			cols, _ := rows.Columns()
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Next()
			rows.Scan(ptrs...)
			rows.Close()
			var got []string
			for _, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				got = append(got, fmt.Sprint(v))
			}
			if g := strings.Join(got, " "); g != want {
				t.Errorf("%s %s: %s, want %s", stage, q, g, want)
			}
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

// TestBulkLoadReadsBackWithAVirtualTable: with a virtual table in the database
// the rewrite does not rebase the session onto the file it wrote, so a
// direct-path table's row store, which never held its rows, was what the
// session read afterwards: the rows were durable, and the same connection saw
// none of them until a reopen.
func TestBulkLoadReadsBackWithAVirtualTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vt.musq")
	db, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE VIRTUAL TABLE r1 USING rtree(id, x1, x2)`,
		`INSERT INTO r1 VALUES(1, 5, 5)`,
		`CREATE TABLE big(k)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 20000) INSERT INTO big SELECT i FROM c`,
		`CREATE TABLE one(x)`,
		`INSERT INTO one SELECT (SELECT count(*) FROM r1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n, sum, one int64
	if err := db.QueryRow(`SELECT count(*), sum(k), (SELECT count(*) FROM one) FROM big`).Scan(&n, &sum, &one); err != nil {
		t.Fatal(err)
	}
	if n != 20000 || sum != 200010000 || one != 1 {
		t.Fatalf("read back %d rows summing %d, and %d in one; want 20000, 200010000, 1", n, sum, one)
	}
}
