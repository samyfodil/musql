package engine_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestHashGroupByBareColumnAnswers pins GROUP BY shapes the hash path now
// serves, a bare column under an expression key among them, to C SQLite's
// answers (compat-harness TestHashGroupByBareColumnsAgree).
func TestHashGroupByBareColumnAnswers(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "g.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range hashGroupSetup {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i, q := range hashGroupQueries {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		var got []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for j := range vals {
				ptrs[j] = &vals[j]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			cells := make([]string, len(vals))
			for j, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				cells[j] = fmt.Sprint(v)
			}
			got = append(got, strings.Join(cells, " "))
		}
		rows.Close()
		if s := strings.Join(got, "|"); s != hashGroupWant[i] {
			t.Errorf("%s\n got %s\nwant %s", q, s, hashGroupWant[i])
		}
	}
}

var hashGroupSetup = []string{
	"CREATE TABLE t(a, b, i)",
	"INSERT INTO t WITH RECURSIVE s(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM s WHERE v < 2000) SELECT v%7, v%11, v FROM s",
	"INSERT INTO t VALUES(NULL, 3, 5000), (2.0, NULL, 5001), ('x', 'y', 5002)",
}

var hashGroupQueries = []string{
	"SELECT a, sum(i) FROM t GROUP BY a+1",
	"SELECT b, sum(i), count(*) FROM t GROUP BY a+1",
	"SELECT a+1, sum(i) FROM t GROUP BY a+1",
	"SELECT b, max(i), min(i) FROM t GROUP BY a",
	"SELECT a, i FROM t GROUP BY b HAVING count(*) > 180",
	"SELECT a%2, b, total(i) FROM t GROUP BY a%2 ORDER BY a%2",
}

var hashGroupWant = []string{
	"<nil> 5000|0 290287|1 285571|2 290858|3 286143|4 286429|5 286715|6 285000",
	"3 5000 1|7 290287 286|1 285571 286|2 290858 287|3 286143 286|4 286429 286|5 286715 286|6 285000 285",
	"<nil> 5000|1 290287|2 285571|3 290858|4 286143|5 286429|6 286715|7 285000",
	"3 5000 5000|7 1995 7|1 1996 1|2 5001 2|3 1998 3|4 1999 4|5 2000 5|6 1994 6|y 5002 5002",
	"4 11|1 1|2 2|3 3|4 4|5 5|6 6|0 7|1 8|2 9|3 10",
	"<nil> 3 5000|0 2 1.152574e+06|1 1 858429",
}
