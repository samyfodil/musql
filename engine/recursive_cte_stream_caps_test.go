package engine_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecursiveCTEStreamedPastTheRowCap: a recursion streamed into consumers
// that keep few of its rows answers past cteRecursionRowCap, as C does (#70),
// while a runaway still ends in an error: on the rows streamed, on the bytes
// its holder keeps, and, under a consumer that gathers every row, on the old
// cap.
func TestRecursiveCTEStreamedPastTheRowCap(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range recursiveStreamAnswers {
		rows, err := db.Query(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		var got []string
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprint(v))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		rows.Close()
		if s := strings.Join(got, " "); s != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.q, s, c.want)
		}
	}
	for _, c := range []struct{ q, want string }{
		{"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c) SELECT x FROM c WHERE x < 0",
			"rows streamed to its consumer"},
		{"WITH RECURSIVE c(x, y) AS (VALUES(1, zeroblob(100000)) UNION ALL SELECT x+1, y FROM c) SELECT x, y FROM c",
			"bytes of result rows"},
		{"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c) SELECT count(*) FROM c",
			"internal row limit (possible non-terminating recursion relying on an outer LIMIT"},
	} {
		rows, err := db.Query(c.q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", c.q, err, c.want)
		}
	}
}

// recursiveStreamAnswers are recursions of more rows than cteRecursionRowCap
// whose consumers keep few of them; compat-harness checks the same answers
// against C SQLite.
var recursiveStreamAnswers = []struct{ q, want string }{
	{"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x < 400000) SELECT x FROM c WHERE x % 100000 = 0",
		"100000 200000 300000 400000"},
	{"SELECT * FROM (WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x < 350000) SELECT x FROM c WHERE x = 350000)",
		"350000"},
	{"SELECT * FROM (WITH RECURSIVE d(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM d WHERE n < 1), " +
		"x(v, i) AS (SELECT 0, 0 UNION ALL SELECT v*2+n, i+1 FROM x, d WHERE i < 18) " +
		"SELECT v FROM x WHERE i = 18 AND v % 65536 = 0)",
		"0 65536 131072 196608"},
	// The recursion is the second arm of a streamed UNION ALL: the arm that
	// starts once the first is done keeps the chain's holder.
	{"SELECT * FROM (SELECT 0 AS x UNION ALL SELECT x FROM (WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x < 400000) " +
		"SELECT x FROM c WHERE x % 100000 = 0))",
		"0 100000 200000 300000 400000"},
}
