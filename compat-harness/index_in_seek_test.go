package compat

import (
	"fmt"
	"strings"
	"testing"
)

// C codes "col IN (list)" on an indexed column as a loop over the IN's values
// seeking the index for each, and tests a term that can raise on those rows
// alone; the order a query without ORDER BY gets is the index's. Reads and
// writes, over rows in segments and in the log.
func TestIndexInSeek(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, g INTEGER, a INTEGER, s TEXT COLLATE NOCASE, j TEXT)",
		"CREATE INDEX ta ON t(a)",
		"CREATE INDEX tgs ON t(g, s)",
		"WITH RECURSIVE c(x) AS (VALUES(-20) UNION ALL SELECT x+1 FROM c WHERE x < 600) " +
			"INSERT INTO t SELECT x, x % 3, x % 37, char(97 + (x + 20) % 4) || (x % 5), " +
			"CASE WHEN x % 50 = 0 THEN '{bad' ELSE '{\"v\":' || x || '}' END FROM c",
		"VACUUM",
		"INSERT INTO t VALUES(5000, 1, 2, 'B2', '{}')",
		"DELETE FROM t WHERE id % 13 = 0",
	}
	lists := []string{
		"1, 2, 3",
		"5, 1, 5, 2, 1",
		"-1, 0, 36, 37, 1000",
		"'1', 2.0, 2.5, NULL, X'01', 'x'",
		"NULL, NULL",
		"'a1', 'B2', 'c3 ', 'zz', 'A0'",
	}
	stmts := append([]string{}, setup...)
	for _, l := range lists {
		stmts = append(stmts,
			fmt.Sprintf("SELECT id, a FROM t WHERE a IN (%s)", l),
			fmt.Sprintf("SELECT id FROM t WHERE g = 1 AND a IN (%s)", l),
			fmt.Sprintf("SELECT id, s FROM t WHERE s IN (%s) AND g = 2", l),
			fmt.Sprintf("SELECT count(*), sum(id) FROM t WHERE a IN (%s)", l),
			fmt.Sprintf("SELECT a, count(*) FROM t WHERE a IN (%s) GROUP BY a", l),
		)
	}
	stmts = append(stmts,
		// Rows outside the list hold malformed JSON; C never tests them.
		"SELECT id FROM t WHERE json_extract(j, '$.v') > 0 AND a IN (1, 2, 3)",
		"SELECT id FROM t WHERE a IN (1, 2, 3) AND json_extract(j, '$.v') > 0",
		"UPDATE t SET a = a + 100 WHERE a IN (6, 7, 8) AND id > 0 RETURNING id, a",
		"UPDATE t SET s = s || '!' WHERE s IN ('a1', 'B2', 'c3 ', 'Z') RETURNING id, s",
		"DELETE FROM t WHERE a IN (3, 1, 2, '4', 5.0, NULL) RETURNING id, a",
		"DELETE FROM t WHERE json_extract(j, '$.v') > 0 AND a IN (9, 10) RETURNING id",
		"SELECT count(*), sum(a) FROM t",
	)
	differ(t, "index IN seek", stmts)
}

// Replication's clock table: a key whose first column holds a few values, and
// "tbl = ? AND pk IN (...)" answered from pk's own values.
func TestIndexInSeekClockShape(t *testing.T) {
	c, m := boundPair(t, []string{
		"CREATE TABLE c(tbl TEXT, pk BLOB, top INTEGER, rec BLOB, PRIMARY KEY(tbl, pk))",
		"WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x < 999) " +
			"INSERT INTO c SELECT char(97 + x % 3), randomblob(0) || unhex(printf('%04x', x)), x, zeroblob(4) FROM n",
		"VACUUM",
		"INSERT INTO c VALUES('a', unhex('ffff'), -1, NULL)",
	})
	for _, n := range []int{3, 16, 128} {
		args := []any{"b"}
		for i := range n {
			args = append(args, []byte{byte(i * 7 >> 8), byte(i * 7)})
		}
		marks := strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
		for _, q := range []string{
			"SELECT hex(pk), top FROM c WHERE tbl = ? AND pk IN (" + marks + ")",
			"SELECT count(*) FROM c WHERE tbl = ? AND pk IN (" + marks + ")",
			"DELETE FROM c WHERE tbl = ? AND pk IN (" + marks + ") RETURNING hex(pk), top",
		} {
			want := boundRows(c, q, args...)
			got := boundRows(m, q, args...)
			if want != got {
				t.Errorf("%s (n=%d):\n  cgo:   %s\n  musql: %s", q, n, want, got)
			}
		}
	}
}
