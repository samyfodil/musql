package compat

import "testing"

// TestGroupedWindowCollateAnswers verifies collation carries through window
// aggregate hoisting to DISTINCT, ORDER BY, and PARTITION BY.
func TestGroupedWindowCollateAnswers(t *testing.T) {
	schema := []string{
		"CREATE TABLE c1(a, b)",
		"INSERT INTO c1 VALUES(1,'abcd'),(2,'BCDE'),(3,'cdef'),(4,'DEFG')",
	}
	for _, q := range []string{
		"SELECT count() OVER (), rowid, max(b COLLATE nocase)||'' FROM c1 GROUP BY rowid ORDER BY max(b COLLATE nocase)||''",
		"SELECT count() OVER (), rowid, max(b COLLATE nocase) FROM c1 GROUP BY rowid ORDER BY max(b COLLATE nocase)",
		// DISTINCT with collation.
		"SELECT DISTINCT max(b COLLATE nocase), count(*) OVER () FROM c1 GROUP BY rowid ORDER BY 1",
		// Window ORDER BY with collation.
		"SELECT a, b, sum(a) OVER (ORDER BY b COLLATE nocase) FROM c1 GROUP BY a ORDER BY b COLLATE nocase",
	} {
		differ(t, q, append(append([]string(nil), schema...), q))
	}
}
