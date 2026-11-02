package engine_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateTableAsBuildsSegments: CREATE TABLE AS fills its new table straight
// into segments when autocommit allows and row by row otherwise, with the
// same rows either way, and leaves last_insert_rowid() as it was, as C does.
func TestCreateTableAsBuildsSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.musq")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	query := func(db *sql.DB, q string) string {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			cells := make([]string, len(vals))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				cells[i] = fmt.Sprint(v)
			}
			out = append(out, strings.Join(cells, " "))
		}
		return strings.Join(out, "|")
	}
	exec("CREATE TABLE a(x)")
	exec("INSERT INTO a VALUES (5)")
	src := "SELECT 1 AS i, 'x' AS s, 2.5 AS r, NULL AS n, CAST('7' AS INTEGER) AS c UNION ALL SELECT 2, 'y', 3, x'00', '8'"
	exec("CREATE TABLE direct AS " + src)
	exec("BEGIN")
	exec("CREATE TABLE inTx AS " + src)
	exec("COMMIT")
	exec("CREATE TEMP TABLE tmp AS " + src)
	exec("CREATE TABLE u AS VALUES(CAST(1 AS REAL)),(CAST(1 AS REAL)) UNION ALL SELECT NULL")
	exec("CREATE TABLE big AS SELECT value % 7 AS a, value AS v FROM generate_series(1, 100000)")
	if got := query(db, "SELECT last_insert_rowid()"); got != "1" {
		t.Errorf("last_insert_rowid() after CREATE TABLE AS: %s, want 1", got)
	}
	exec("CREATE TABLE copy(a, v)")
	exec("INSERT INTO copy SELECT a, v FROM big")
	exec("CREATE TABLE half(a, v)")
	if _, err := db.Exec("INSERT INTO half SELECT a, CASE WHEN v = 99999 THEN abs(-9223372036854775808) ELSE v END FROM big"); err == nil {
		t.Fatal("an overflowing INSERT ... SELECT succeeded")
	}
	if got := query(db, "SELECT count(*) FROM half"); got != "0" {
		t.Errorf("a failed load kept %s rows", got)
	}
	want := "1 1 x 2.5 <nil> 7 integer text real null integer|2 2 y 3 \x00 8 integer text integer blob text"
	for _, tbl := range []string{"direct", "inTx", "tmp"} {
		if got := query(db, "SELECT rowid, *, typeof(i), typeof(s), typeof(r), typeof(n), typeof(c) FROM "+tbl); got != want {
			t.Errorf("%s: %q, want %q", tbl, got, want)
		}
	}
	if got := query(db, "SELECT typeof(column1), quote(column1) FROM u"); got != "real 1.0|real 1.0|null NULL" {
		t.Errorf("FLEXNUM: %q", got)
	}
	for _, tbl := range []string{"big", "copy"} {
		if got := query(db, "SELECT count(*), sum(v), max(rowid) FROM "+tbl); got != "100000 5000050000 100000" {
			t.Errorf("%s: %q", tbl, got)
		}
	}

	exec("BEGIN")
	exec("CREATE TABLE gone AS " + src)
	exec("ROLLBACK")
	if _, err := db.Exec("CREATE TABLE bad AS SELECT abs(-9223372036854775808)"); err == nil {
		t.Fatal("an overflowing SELECT made a table")
	}
	if got := query(db, "SELECT group_concat(name) FROM (SELECT name FROM sqlite_schema WHERE name IN ('gone', 'bad'))"); got != "<nil>" {
		t.Errorf("tables left by a rollback or a failure: %s", got)
	}
	db.Close()

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := query(db, "SELECT rowid, *, typeof(i), typeof(s), typeof(r), typeof(n), typeof(c) FROM direct"); got != want {
		t.Errorf("direct after reopening: %q", got)
	}
	if got := query(db, "SELECT a, count(*) FROM big GROUP BY a ORDER BY a LIMIT 2"); got != "0 14285|1 14286" {
		t.Errorf("big after reopening: %q", got)
	}
}
