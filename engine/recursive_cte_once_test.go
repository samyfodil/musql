package engine_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecursiveCTEOncePerStatement: an uncorrelated recursive CTE read by a
// correlated subquery gives every outer row the same rows, while a trigger
// body materializes it again at each firing (compat-harness
// TestRecursiveCTEOncePerStatement checks both against C SQLite).
func TestRecursiveCTEOncePerStatement(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE t(n)",
		"INSERT INTO t VALUES (1), (2), (3), (4), (5)",
		"CREATE TABLE src(v)",
		"CREATE TABLE ev(v)",
		"CREATE TABLE log(n, s)",
		"CREATE TRIGGER tr AFTER INSERT ON ev BEGIN " +
			"INSERT INTO src VALUES (new.v); " +
			"INSERT INTO log SELECT new.v, (SELECT sum(x) FROM (WITH RECURSIVE r(x) AS (SELECT v FROM src UNION ALL SELECT x FROM r WHERE 0) SELECT x FROM r)); " +
			"END",
		"INSERT INTO ev VALUES (1), (2), (3)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var distinct, n int
	if err := db.QueryRow("WITH RECURSIVE r(x) AS (SELECT random() UNION ALL SELECT x FROM r WHERE 0) "+
		"SELECT count(DISTINCT v), count(*) FROM (SELECT (SELECT x FROM r WHERE t.n = t.n) AS v FROM t)").Scan(&distinct, &n); err != nil || distinct != 1 || n != 5 {
		t.Fatalf("random() through a correlated subquery: %d distinct of %d, %v", distinct, n, err)
	}
	rows, err := db.Query("SELECT n, s FROM log ORDER BY n")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n, s string
		if err := rows.Scan(&n, &s); err != nil {
			t.Fatal(err)
		}
		got = append(got, n+":"+s)
	}
	if s := strings.Join(got, " "); s != "1:1 2:3 3:6" {
		t.Fatalf("trigger firings: %s", s)
	}
}
