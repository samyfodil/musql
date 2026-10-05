// Tests name context for CHECK constraints and partial index WHERE clauses.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// checkNameContextCases are tested against both engines.
var checkNameContextCases = []struct{ name, sql, wantErr string }{
	// ---- rowid inside a CHECK ----
	{"rowid-check-rowid-table", `CREATE TABLE a(x INT, CHECK(rowid<100))`, ""},
	{"rowid-check-ipk-table", `CREATE TABLE q(x INTEGER PRIMARY KEY, CHECK(rowid<100))`, ""},
	{"oid-check", `CREATE TABLE c(x INT, CHECK(oid<100))`, ""},
	{"underscore-rowid-check", `CREATE TABLE d(x INT, CHECK(_rowid_<100))`, ""},
	{"rowid-check-without-rowid", `CREATE TABLE b2(x INT PRIMARY KEY, CHECK(rowid<100)) WITHOUT ROWID`, "no such column: rowid"},

	// ---- the database qualifier ----
	{"main-qualified-check", `CREATE TABLE h(x INT, CHECK(main.h.x>0))`, ""},
	{"nonexistent-db-qualified-check", `CREATE TABLE i(x INT, CHECK(xyzzy.i.x>0))`, ""},
	{"temp-qualified-check", `CREATE TABLE o(x INT, CHECK(temp.o.x>0))`, ""},
	{"wrong-table-qualified-check", `CREATE TABLE k(x INT, CHECK(xyzzy.zzz.x>0))`, "no such column: zzz.x"},
	{"wrong-column-qualified-check", `CREATE TABLE l(x INT, CHECK(xyzzy.l.nope>0))`, "no such column: l.nope"},
	{"db-qualified-partial-index", `CREATE TABLE t3(a INT, b INT)`, ""},
	{"db-qualified-partial-index-where", `CREATE INDEX t3i ON t3(a) WHERE xyzzy.t3.b BETWEEN 5 AND 10`, ""},

	// ---- and the rows that prove each one is ENFORCED, not merely parsed ----
	{"rowid-check-admits", `INSERT INTO a VALUES(1)`, ""},
	{"rowid-check-rejects", `INSERT INTO a(rowid,x) VALUES(500,3)`, "CHECK constraint failed"},
	{"ipk-check-admits", `INSERT INTO q VALUES(5)`, ""},
	{"ipk-check-rejects", `INSERT INTO q VALUES(500)`, "CHECK constraint failed"},
	{"ipk-check-rejects-on-update", `UPDATE q SET x=900 WHERE x=5`, "CHECK constraint failed"},
	{"rowid-check-not-refired", `UPDATE a SET x=9 WHERE x=1`, ""},
	{"qualified-check-admits", `INSERT INTO i VALUES(1)`, ""},
	{"qualified-check-rejects", `INSERT INTO i VALUES(-1)`, "CHECK constraint failed"},
	{"temp-qualified-check-rejects", `INSERT INTO o VALUES(-1)`, "CHECK constraint failed"},
	{"partial-index-rows", `INSERT INTO t3 VALUES(1,7)`, ""},
	{"partial-index-rows-2", `INSERT INTO t3 VALUES(2,70)`, ""},
	{"partial-index-rows-3", `INSERT INTO t3 VALUES(3,5)`, ""},
}

func TestCheckNameContextVsOracle(t *testing.T) {
	dir := t.TempDir()
	for _, eng := range []struct{ driver, dsn string }{
		{"sqlite3", filepath.Join(dir, "cgo.db")},
		{"sqlite", filepath.Join(dir, "musql.db")},
	} {
		db, err := sql.Open(eng.driver, eng.dsn)
		if err != nil {
			t.Fatalf("%s: open: %v", eng.driver, err)
		}
		for _, c := range checkNameContextCases {
			_, err := db.Exec(c.sql)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("%s/%s: %s: %v -- must be accepted", eng.driver, c.name, c.sql, err)
			case c.wantErr != "" && err == nil:
				t.Errorf("%s/%s: %s: accepted, want an error like %q", eng.driver, c.name, c.sql, c.wantErr)
			case c.wantErr != "" && eng.driver == "sqlite3" && !strings.Contains(err.Error(), c.wantErr):
				// The oracle arm is the specification, so its exact message is
				// held; musql words its errors differently throughout.
				t.Errorf("oracle/%s: %s: %v -- want %q", c.name, c.sql, err, c.wantErr)
			}
		}
		// The partial index must admit exactly the rows its (database-qualified)
		// WHERE selects.
		rows, err := db.Query(`SELECT a FROM t3 WHERE b BETWEEN 5 AND 10 ORDER BY a`)
		if err != nil {
			t.Fatalf("%s: partial-index read: %v", eng.driver, err)
		}
		var got []int64
		for rows.Next() {
			var a int64
			if err := rows.Scan(&a); err != nil {
				t.Fatal(err)
			}
			got = append(got, a)
		}
		rows.Close()
		if len(got) != 2 || got[0] != 1 || got[1] != 3 {
			t.Errorf("%s: got %v, want [1 3]", eng.driver, got)
		}
		db.Close()
	}
}
