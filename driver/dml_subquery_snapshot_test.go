package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// rowsAsText renders a query's rows as "[v v] [v v] " for comparison.
func rowsAsText(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := ""
	for rows.Next() {
		vals := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range vals {
			ptr[i] = &vals[i]
		}
		if err := rows.Scan(ptr...); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		out += fmt.Sprintf("%v ", vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// TestDMLSubqueryOverItsOwnTargetIsTwoPass verifies two-pass DML semantics.
// the loop had already deleted -- and the mined corpus caught it in four files
// (delete.test, delete4.test, update.test twice). Every expectation here is C
// SQLite's own answer.
func TestDMLSubqueryOverItsOwnTargetIsTwoPass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
		read  string
		want  string
	}{
		{"DELETE whose EXISTS looks for a row the loop just deleted",
			[]string{`CREATE TABLE t2(x INT)`, `INSERT INTO t2(x) VALUES(1),(2),(3),(4),(5)`,
				`DELETE FROM t2 WHERE EXISTS(SELECT 1 FROM t2 AS v WHERE v.x=t2.x-1)`},
			`SELECT x FROM t2`, "[1] "},
		{"DELETE whose uncorrelated subquery picks by OFFSET",
			[]string{`CREATE TABLE t0(vkey INTEGER, pkey INTEGER,c1 INTEGER)`,
				`INSERT INTO t0 VALUES(2,1,-20),(2,2,NULL),(2,3,0),(8,4,95)`,
				`DELETE FROM t0 WHERE NOT ((t0.vkey <= t0.c1) AND (t0.vkey <> (SELECT vkey FROM t0 ORDER BY vkey LIMIT 1 OFFSET 2)))`},
			`SELECT * FROM t0`, "[8 4 95] "},
		{"UPDATE whose WHERE reads min() of the target",
			[]string{`CREATE TABLE t1 (vkey INTEGER, c5 INTEGER)`, `INSERT INTO t1 VALUES(3,NULL),(6,-54)`,
				`UPDATE t1 SET vkey = 100 WHERE c5 is null OR NOT (-10*(select min(vkey) from t1) >= c5)`},
			`SELECT * FROM t1 ORDER BY vkey, c5`, "[6 -54] [100 <nil>] "},
		{"UPDATE whose SET reads min() of the target",
			[]string{`CREATE TABLE t1(x INT, y INT)`, `INSERT INTO t1(x) VALUES(1),(2),(3),(4),(5)`,
				`UPDATE t1 SET x=x+100, y=x<=(SELECT min(x) FROM t1) WHERE x<3 OR (1 BETWEEN 0 AND x<=(SELECT min(x)+2 FROM t1))`},
			`SELECT x FROM t1 WHERE x<100 ORDER BY x`, "[4] [5] "},
		{"INSERT whose SELECT source is the table it inserts into",
			[]string{`CREATE TABLE t3(a)`, `INSERT INTO t3 VALUES(1),(2)`, `INSERT INTO t3 SELECT a FROM t3`},
			`SELECT a FROM t3 ORDER BY a`, "[1] [1] [2] [2] "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "x.musq"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for _, s := range tc.stmts {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			if got := rowsAsText(t, db, tc.read); got != tc.want {
				t.Errorf("%s\n got %s\nwant %s", tc.read, got, tc.want)
			}
		})
	}
}
