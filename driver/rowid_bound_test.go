package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestRowidBoundMatchesTheLongWay: min()/max() of a rowid or INTEGER PRIMARY
// KEY is answered from the table's ends (OpRowidBound) instead of a scan. It
// must agree with the same question asked through ORDER BY ... LIMIT 1 in
// every state a table can be in: empty, negative rowids, committed segments
// with a delta on top, rows deleted at either end, and rows an open
// transaction has written or deleted.
func TestRowidBoundMatchesTheLongWay(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "b.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ex := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	one := func(q string) string {
		t.Helper()
		var v sql.NullInt64
		if err := db.QueryRow(q).Scan(&v); err != nil && err != sql.ErrNoRows {
			t.Fatalf("%s: %v", q, err)
		}
		if !v.Valid {
			return "NULL"
		}
		return fmt.Sprint(v.Int64)
	}
	check := func(stage string) {
		t.Helper()
		for _, c := range [][3]string{
			{"SELECT max(id) FROM a", "SELECT id FROM a ORDER BY id DESC LIMIT 1", "a max(id)"},
			{"SELECT min(id) FROM a", "SELECT id FROM a ORDER BY id LIMIT 1", "a min(id)"},
			{"SELECT max(rowid) FROM a", "SELECT rowid FROM a ORDER BY rowid DESC LIMIT 1", "a max(rowid)"},
			{"SELECT max(rowid) FROM r", "SELECT rowid FROM r ORDER BY rowid DESC LIMIT 1", "r max(rowid)"},
			{"SELECT min(_rowid_) FROM r", "SELECT rowid FROM r ORDER BY rowid LIMIT 1", "r min(_rowid_)"},
			{"SELECT max(oid) FROM e", "SELECT NULL", "e max(oid), empty"},
		} {
			if got, want := one(c[0]), one(c[1]); got != want {
				t.Errorf("%s %s: %s, want %s", stage, c[2], got, want)
			}
		}
	}
	ex(`CREATE TABLE a(id INTEGER PRIMARY KEY, v)`)
	ex(`WITH RECURSIVE c(i) AS (SELECT -50 UNION ALL SELECT i + 1 FROM c WHERE i < 5000) INSERT INTO a SELECT i, i FROM c`)
	ex(`CREATE TABLE r(v)`)
	ex(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 3000) INSERT INTO r SELECT i FROM c`)
	ex(`CREATE TABLE e(v)`)
	check("loaded")
	ex(`VACUUM`)
	check("vacuumed")
	ex(`DELETE FROM a WHERE id >= 4990`)
	ex(`DELETE FROM a WHERE id < -40`)
	ex(`DELETE FROM r WHERE rowid IN (1, 2, 3000, 2999)`)
	check("ends deleted, in the delta")
	ex(`INSERT INTO a VALUES (-9000, 0), (9000, 0)`)
	check("new ends")
	ex(`BEGIN`)
	ex(`DELETE FROM a WHERE id IN (-9000, 9000)`)
	ex(`INSERT INTO r(rowid, v) VALUES (-3, 0)`)
	check("inside a transaction")
	ex(`ROLLBACK`)
	check("rolled back")
	ex(`DELETE FROM a`)
	check("emptied")

	// The DELETE that seeks with a scalar subquery's value removes exactly the
	// row a scan would: the largest, once per statement.
	ex(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 100) INSERT INTO a SELECT i, i FROM c`)
	for i := 0; i < 5; i++ {
		ex(`DELETE FROM a WHERE id = (SELECT max(id) FROM a)`)
	}
	if got := one(`SELECT count(*) * 1000 + max(id) FROM a`); got != "95095" {
		t.Errorf("after five max deletes: count*1000+max = %s, want 95095", got)
	}
	// A write with a subquery is compiled once and cached; each run must read
	// the database as it is then, not as it was at the compile.
	ex(`CREATE TABLE src(x)`)
	ex(`CREATE TABLE dst(id INTEGER PRIMARY KEY, n)`)
	ex(`INSERT INTO dst VALUES (1, 0)`)
	for i := 1; i <= 3; i++ {
		ex(`INSERT INTO src VALUES (1)`)
		ex(`UPDATE dst SET n = (SELECT count(*) FROM src) WHERE id = 1`)
		ex(`INSERT INTO dst(n) SELECT count(*) * 100 FROM src`)
		ex(`DELETE FROM dst WHERE id = (SELECT max(id) FROM dst) AND n < (SELECT count(*) * 1000 FROM src)`)
		if got, want := one(`SELECT n FROM dst WHERE id = 1`), fmt.Sprint(i); got != want {
			t.Errorf("run %d: the cached UPDATE read %s, want %s", i, got, want)
		}
		if got := one(`SELECT count(*) FROM dst`); got != "1" {
			t.Errorf("run %d: %s rows in dst, want 1 (the cached INSERT/DELETE pair)", i, got)
		}
	}

	// A correlated subquery is not a seek key: it reads the row being tested,
	// so it cannot run before the loop. This one matches every row.
	ex(`CREATE TABLE corr(id INTEGER PRIMARY KEY, v)`)
	ex(`INSERT INTO corr VALUES (1, 0), (2, 0), (3, 0)`)
	ex(`DELETE FROM corr WHERE id = (SELECT max(x.id) FROM corr x WHERE x.id = corr.id)`)
	if got := one(`SELECT count(*) FROM corr`); got != "0" {
		t.Errorf("correlated rowid key deleted all but %s rows", got)
	}
	ex(`DELETE FROM a WHERE id = (SELECT min(id) FROM a WHERE v > 50)`)
	if got := one(`SELECT count(*) * 1000 + min(id) FROM a WHERE id > 50`); got != "44052" {
		t.Errorf("after the filtered min delete: %s, want 44052", got)
	}
}
