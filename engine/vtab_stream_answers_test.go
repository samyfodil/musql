package engine_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateSeriesStreamAnswers: generate_series answers the same whether
// its loop reads the live cursor (outermost or correlated) or the materialized
// rows (an inner loop, a RIGHT JOIN), and a scan that stops early stops the
// series with it.
func TestGenerateSeriesStreamAnswers(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{"CREATE TABLE t(n)", "INSERT INTO t VALUES (2), (3), (4)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ q, want string }{
		{"SELECT count(*), sum(value), min(value), max(value) FROM generate_series(1, 1000000)", "1000000 500000500000 1 1000000"},
		{"SELECT value FROM generate_series(10, 1, -3)", "10|7|4|1"},
		{"SELECT value FROM generate_series(1, 10, 0) LIMIT 3", "1|2|3"},
		{"SELECT value, start, stop, step, rowid FROM generate_series(1, 3, 2)", "1 1 3 2 1|3 1 3 2 3"},
		{"SELECT value FROM generate_series(9223372036854775806, 9223372036854775807)", "9223372036854775806|9223372036854775807"},
		{"SELECT count(*) FROM generate_series(1, NULL)", "0"},
		{"SELECT count(*) FROM generate_series(1, 1000) a, generate_series(1, 1000) b WHERE a.value = b.value", "1000"},
		{"SELECT n, value FROM t, generate_series(1, t.n) WHERE n < 4", "2 1|2 2|3 1|3 2|3 3"},
		{"SELECT n, value FROM t LEFT JOIN generate_series(t.n, 3) ON 1", "2 2|2 3|3 3|4 <nil>"},
		{"SELECT count(*), count(n) FROM t RIGHT JOIN generate_series(1, 5) ON value = n", "5 3"},
		{"SELECT n, (SELECT sum(value) FROM generate_series(1, n)) FROM t", "2 3|3 6|4 10"},
		{"SELECT EXISTS (SELECT 1 FROM generate_series(1, 16000000) WHERE value = 3)", "1"},
		{"SELECT value FROM generate_series(1, 16000000) LIMIT 2", "1|2"},
		{"SELECT count(*) FROM json_each('[1,2,3]')", "3"},
	} {
		rows, err := db.Query(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		cols, _ := rows.Columns()
		var got []string
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
				cells[i] = fmt.Sprint(v)
			}
			got = append(got, strings.Join(cells, " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		rows.Close()
		if s := strings.Join(got, "|"); s != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.q, s, c.want)
		}
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM generate_series(1, 20000000)").Scan(&n); err == nil || !strings.Contains(err.Error(), "materialization limit") {
		t.Errorf("a series past the cap: %d, %v", n, err)
	}
}
