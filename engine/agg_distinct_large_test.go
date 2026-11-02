package engine_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestAggDistinctAnswers pins DISTINCT aggregates over many distinct values,
// values equal across storage classes, and collations whose equality is not
// byte equality, in each text encoding. The answers are C SQLite's
// (compat-harness TestAggDistinctHashAgrees).
func TestAggDistinctAnswers(t *testing.T) {
	series := "WITH RECURSIVE s(value) AS (VALUES(1) UNION ALL SELECT value+1 FROM s WHERE value<20000) "
	for _, enc := range []string{"UTF-8", "UTF-16le", "UTF-16be"} {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "d.musq"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range append([]string{"PRAGMA encoding='" + enc + "'"}, aggDistinctSchema...) {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		for _, c := range []struct{ q, want string }{
			{series + "SELECT sum(DISTINCT round(value/1.5)), count(DISTINCT value%1000), avg(DISTINCT value%997), total(DISTINCT -value) FROM s",
				"[8.8891111e+07 1000 498 -2.0001e+08]"},
			{series + "SELECT max(DISTINCT value%5000), min(DISTINCT value%5000), json_array_length(json_group_array(DISTINCT value%3000)) FROM s",
				"[4999 0 3000]"},
			{"SELECT k, sum(DISTINCT v), count(DISTINCT v), group_concat(DISTINCT v) FROM m WHERE typeof(v) <> 'blob' GROUP BY k ORDER BY k",
				"[1 4 3 2,2,0.0 2 1e+300 4 9007199254740993,9007199254740992.0,-9223372036854775808,1.0e+300]"},
			{"SELECT count(DISTINCT v), count(DISTINCT typeof(v)) FROM m", "[8 5]"},
			{"SELECT max(DISTINCT v), k FROM m", "[2 1]"},
			{"SELECT min(DISTINCT v), k FROM m WHERE k = 2", "[-9223372036854775808 2]"},
			{"SELECT json_group_array(DISTINCT v) FROM m WHERE typeof(v) <> 'blob'",
				`[[2,"2",null,0.0,9007199254740993,9007199254740992.0,-9223372036854775808,1.0e+300]]`},
			{"SELECT count(DISTINCT n), group_concat(DISTINCT n), count(DISTINCT r), group_concat(DISTINCT r), count(DISTINCT b), group_concat(DISTINCT b) FROM c",
				"[4 a\x00b,ab,a\x00bc,abc 4 x,y, x,Y  4 é,É,e,ü]"},
			{"SELECT count(DISTINCT x), group_concat(DISTINCT x), count(DISTINCT y), group_concat(DISTINCT y), max(DISTINCT y) FROM u",
				"[5 é,ä,Ä,日本,Ω 3 A,B,x x]"},
		} {
			rows, err := db.Query(c.q)
			if err != nil {
				t.Fatalf("%s: %s: %v", enc, c.q, err)
			}
			var got []any
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				for _, v := range vals {
					if b, ok := v.([]byte); ok {
						v = string(b)
					}
					got = append(got, v)
				}
			}
			rows.Close()
			if s := fmt.Sprint(got); s != c.want {
				t.Errorf("%s: %s\n got %s\nwant %s", enc, c.q, s, c.want)
			}
		}
		db.Close()
	}
}

var aggDistinctSchema = []string{
	"CREATE TABLE m(k, v)",
	"INSERT INTO m VALUES(1,2),(1,2.0),(1,'2'),(1,x'32'),(1,NULL),(1,0.0),(1,-0.0),(1,0)," +
		"(2,9007199254740993),(2,9007199254740992.0),(2,9007199254740992),(2,-9223372036854775808),(2,-9223372036854775808.0),(2,1e300),(2,1e300)",
	"CREATE TABLE c(n TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM, b TEXT)",
	"INSERT INTO c VALUES('a'||char(0)||'b','x','é'),('A'||char(0)||'c','x ','É'),('ab','x  ','é'),('AB','y','e'),('a'||char(0)||'bc',' x','ü'),('abc','Y ',NULL)",
	"CREATE TABLE u(x, y TEXT COLLATE NOCASE)",
	"INSERT INTO u VALUES('é','A'),('é','a'),('ä','B'),('Ä','b'),('日本','x'),('日本','X'),('Ω',NULL)",
}
