package compat

import (
	"fmt"
	"testing"
)

// Comparisons of INTEGER columns holding NULLs in a loop native code runs
// (issue #169): a NULL operand gives NULL in the stored form and jumps or
// falls through in the jump form as C's comparison does, and IS / IS NOT
// still compare NULLs equal.
func TestNullCompareInNativeLoops(t *testing.T) {
	pre := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, a INT, b INT)",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 2000) " +
			"INSERT INTO t SELECT i, CASE WHEN i % 7 = 0 THEN NULL ELSE i % 19 END, CASE WHEN i % 5 = 0 THEN NULL ELSE i % 23 END FROM g",
	}
	for i, q := range []string{
		"SELECT count(*), sum(a < b), sum(a >= b), count(a < b), sum((a < b) IS NULL) FROM t",
		"SELECT count(*), sum(id) FROM t WHERE a < b",
		"SELECT count(*), sum(id) FROM t WHERE NOT (a < b)",
		"SELECT count(*), sum(id) FROM t WHERE a <> b OR a IS NULL",
		"SELECT sum(CASE WHEN a < b THEN 1 WHEN a >= b THEN 2 ELSE 3 END) FROM t",
		"SELECT count(*), sum(a IS b), sum(a IS NOT b) FROM t",
		"SELECT id, a < b, a = b FROM t WHERE id % 97 = 0 ORDER BY id",
		"SELECT count(*) FROM t x JOIN t y ON y.id = x.id + 1 WHERE x.a < y.b",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
		differ(t, fmt.Sprintf("q-%d-vacuumed", i), append(append([]string(nil), pre...), "VACUUM", q))
	}
}
