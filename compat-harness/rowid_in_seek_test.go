package compat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// C codes "rowid IN (list)" as a loop over the IN's ephemeral index, seeking
// each value: the rows come in ascending rowid order, each once, and a term
// that can raise is tested only on those rows. Every shape here runs over
// rows held in segments and in the log beside them.
func TestRowidInSeek(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, j TEXT)",
		"WITH RECURSIVE c(x) AS (VALUES(-20) UNION ALL SELECT x+1 FROM c WHERE x < 400) " +
			"INSERT INTO t SELECT x, x % 7, CASE WHEN x % 50 = 0 THEN '{bad' ELSE '{\"a\":' || x || '}' END FROM c WHERE x % 3 <> 0",
		"VACUUM",
		"INSERT INTO t VALUES(9007199254740992, 1, '{}'), (-9223372036854775807, 2, '{}')",
		"DELETE FROM t WHERE id BETWEEN 40 AND 44",
	}
	lists := []string{
		"1, 2, 4, 5",
		"5, 4, 2, 1, 5, 4",
		"-20, -19, -18, 0, 3, 400, 401, 9999",
		"'5', 5.0, 7.5, NULL, X'05', 'abc', ' 8 ', '10e0', '11.0'",
		"'9007199254740992.0', 9007199254740992.0, -9223372036854775807",
		"9223372036854775807, -9223372036854775808, '9223372036854775807'",
		"NULL, NULL",
		"41, 43, 47",
	}
	var stmts []string
	stmts = append(stmts, setup...)
	for _, l := range lists {
		stmts = append(stmts,
			fmt.Sprintf("SELECT id, k FROM t WHERE id IN (%s)", l),
			fmt.Sprintf("SELECT id FROM t WHERE rowid IN (%s) AND k > 2", l),
			fmt.Sprintf("SELECT id FROM t WHERE k < 5 AND oid IN (%s) ORDER BY id DESC", l),
			fmt.Sprintf("SELECT count(*), sum(id), min(id), max(id) FROM t WHERE id IN (%s)", l),
			fmt.Sprintf("SELECT k, count(*) FROM t WHERE id IN (%s) GROUP BY k", l),
			fmt.Sprintf("SELECT a.id, b.id FROM t a JOIN t b ON b.id = a.id + 1 WHERE a.id IN (%s)", l),
			fmt.Sprintf("SELECT id FROM t WHERE id IN (%s) LIMIT 2", l),
		)
	}
	stmts = append(stmts,
		// Rows outside the list hold malformed JSON: C never tests them.
		"SELECT id FROM t WHERE json_extract(j, '$.a') > 0 AND id IN (1, 2, 4)",
		"SELECT id FROM t WHERE id IN (1, 2, 4) AND json_extract(j, '$.a') > 0",
		"SELECT id FROM t WHERE json_extract(j, '$.a') > 0 AND id IN (1, 50)",
		// The single-value point seek, read the same way.
		"SELECT id FROM t WHERE id = '9007199254740992.0'",
		"SELECT id FROM t WHERE rowid = 9007199254740992.0",
		"SELECT id FROM t WHERE id = '-9223372036854775807.0'",
	)
	differ(t, "rowid IN seek", stmts)
}

// The write loops seek the same rows in the same order: what RETURNING hands
// back, what triggers see, and what is left.
func TestRowidInSeekWrites(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, j TEXT)",
		"WITH RECURSIVE c(x) AS (VALUES(-20) UNION ALL SELECT x+1 FROM c WHERE x < 400) " +
			"INSERT INTO t SELECT x, x % 7, CASE WHEN x % 50 = 0 THEN '{bad' ELSE '{\"a\":' || x || '}' END FROM c WHERE x % 3 <> 0",
		"VACUUM",
		"DELETE FROM t WHERE id BETWEEN 40 AND 44",
		"CREATE TABLE log(op TEXT, id INTEGER)",
	}
	var stmts []string
	stmts = append(stmts, setup...)
	stmts = append(stmts,
		"DELETE FROM t WHERE id IN (5, 1, 2, 41, 7.5, '8', -20) RETURNING id, k",
		"UPDATE t SET k = k + 100 WHERE id IN (10, 11, 13, '14', 16.0, NULL) RETURNING id, k",
		"UPDATE t SET id = id + 1000 WHERE id IN (19, 20, 22) RETURNING id",
		"SELECT id, k FROM t WHERE id < 30 OR id > 1000",
		// A term that can raise, tested on the seeked rows alone.
		"DELETE FROM t WHERE json_extract(j, '$.a') > 0 AND id IN (23, 25) RETURNING id",
		"UPDATE t SET k = -1 WHERE json_extract(j, '$.a') > 0 AND rowid IN (26, 28, 29) RETURNING id",
		"CREATE TRIGGER td BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('del', old.id); INSERT OR IGNORE INTO t VALUES(old.id + 1, 0, '{}'); END",
		"CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('upd', new.id); END",
		"DELETE FROM t WHERE id IN (31, 32, 35, 37)",
		"UPDATE t SET k = 7 WHERE id IN (37, 38, 39, 46, 47)",
		"SELECT op, id FROM log",
		"SELECT id, k FROM t WHERE id BETWEEN 30 AND 50",
	)
	differ(t, "rowid IN seek writes", stmts)
}

// A write that seeks a column's equality visits its rows in rowid order,
// negative rowids first.
func TestWriteSeekNegativeRowidOrder(t *testing.T) {
	differ(t, "write seek negative rowids", []string{
		"CREATE TABLE w(id INTEGER PRIMARY KEY, a, b)",
		"INSERT INTO w VALUES(-5, 1, 'x'), (-1, 1, 'y'), (3, 1, 'z'), (7, 1, 'w'), (9, 2, 'v')",
		"VACUUM",
		"INSERT INTO w VALUES(-9, 1, 'u'), (11, 1, 't')",
		"UPDATE w SET b = b || '!' WHERE a = 1 RETURNING id, b",
		"DELETE FROM w WHERE a = 1 RETURNING id",
		"SELECT id, a, b FROM w",
	})
}

// The list's values bound as parameters, of every class.
func TestRowidInSeekParams(t *testing.T) {
	c, m := boundPair(t, []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER)",
		"WITH RECURSIVE c(x) AS (VALUES(-5) UNION ALL SELECT x+1 FROM c WHERE x < 200) INSERT INTO t SELECT x, x % 7 FROM c",
		"VACUUM",
	})
	sets := [][]any{
		{1, 2, 3},
		{"4", 5.0, 6.5, nil, []byte("7"), "x"},
		{int64(-5), int64(200), int64(201)},
		{"9007199254740992.0", 9e15},
	}
	// Each pool holds one connection, so these are given back before the
	// queries below.
	ctx := context.Background()
	cc, err := c.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := m.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []struct {
		q    string
		args []any
	}{
		{"SELECT k FROM t WHERE id IN (1, 2, 3)", nil},
		{"SELECT k FROM t WHERE k = 2 AND rowid IN (?, ?)", []any{1, 2}},
		{"SELECT count(*) FROM t WHERE id IN (1, 2, 3)", nil},
	} {
		want, got := planDetails(t, cc, st.q, st.args...), planDetails(t, mc, st.q, st.args...)
		if !slices.Equal(want, got) {
			t.Errorf("EXPLAIN QUERY PLAN %s:\n  cgo:   %q\n  musql: %q", st.q, want, got)
		}
	}
	cc.Close()
	mc.Close()
	for _, args := range sets {
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		for _, q := range []string{
			"SELECT id, k FROM t WHERE id IN (" + marks + ")",
			"SELECT count(*) FROM t WHERE rowid IN (" + marks + ") AND k <> 3",
		} {
			want := boundRows(c, q, args...)
			got := boundRows(m, q, args...)
			if want != got {
				t.Errorf("%s %v:\n  cgo:   %s\n  musql: %s", q, args, want, got)
			}
		}
	}
}
