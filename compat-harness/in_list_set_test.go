package compat

import (
	"fmt"
	"strings"
	"testing"
)

// C codes an IN over three or more constants as a probe of an ephemeral index
// of the list (sqlite3FindInIndex's IN_INDEX_EPH): each value takes the left
// operand's affinity and compares under its collation, and a NULL with no
// match makes the answer NULL. Every column affinity and collation, over rows
// in segments and in the log.
func TestInListSet(t *testing.T) {
	vals := []string{"1", "2", "3", "-1", "-4", "4.5", "6", "'6'", "'a'", "'A'", "'c'", "'c '", "'d'",
		"'x' || char(0) || 'z'", "'x' || char(0) || 'q'", "X'01'", "X'02'", "NULL", "2.0", "'2'", "' 4'",
		"9223372036854775807", "-9223372036854775808", "0.5", "'b'", "'B '", "'1.0'", "1e0"}
	setup := []string{"CREATE TABLE t(i INTEGER, r REAL, s TEXT, n TEXT COLLATE NOCASE, rt TEXT COLLATE RTRIM, b BLOB, x, num NUMERIC)"}
	for k := range 120 {
		v, w := vals[k%len(vals)], vals[(k*7+3)%len(vals)]
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %s, %s, %s, %s, %s, %s, %s)", k%30-8, v, w, v, w, v, w, v))
	}
	setup = append(setup, "VACUUM", "INSERT INTO t VALUES(5, 'A', 'b ', 'C', 'c   ', X'01', 3.0, '7')", "DELETE FROM t WHERE rowid % 11 = 0")
	lists := []string{
		"1, 2, 3",
		"3, 1, 2, 1, 3",
		"1, '2', 3.0, NULL",
		"-1, -2, +3, - -4, 4.5, '6'",
		"'a', 'B', 'c ', 'd  ', 'e'",
		"'A', 'b', 'C', 'x' || char(0) || 'y', 'Z'",
		"'X' || char(0) || 'zz', 'x' || char(0), 'q', 'r'",
		"X'01', X'02', 'x', 1",
		"NULL, NULL, NULL",
		"'1', '2.0', '3e0', ' 4', '5x'",
		"9223372036854775807, -9223372036854775808, 9.223372036854775807e18, 0.5",
		"7, '7', 7.0, 7.5",
	}
	var long strings.Builder
	for i := range 300 {
		if i > 0 {
			long.WriteString(", ")
		}
		fmt.Fprintf(&long, "%d", i*3-40)
	}
	lists = append(lists, long.String())
	cols := []string{"i", "r", "s", "n", "rt", "b", "x", "num", "i + 1", "s COLLATE NOCASE", "rt COLLATE BINARY", "CAST(x AS TEXT)", "+n"}
	stmts := append([]string{}, setup...)
	for _, c := range cols {
		for _, l := range lists {
			stmts = append(stmts,
				fmt.Sprintf("SELECT rowid FROM t WHERE %s IN (%s)", c, l),
				fmt.Sprintf("SELECT rowid FROM t WHERE %s NOT IN (%s)", c, l),
				fmt.Sprintf("SELECT group_concat(quote(%s IN (%s)), '') FROM t", c, l),
				fmt.Sprintf("SELECT count(*), sum(i) FROM t WHERE %s IN (%s)", c, l),
			)
		}
	}
	differ(t, "IN list set", stmts)
}

// The writes probe it too, and a UTF-16 database compares its TEXT in its own
// encoding.
func TestInListSetWritesAndUTF16(t *testing.T) {
	differ(t, "IN list set writes", []string{
		"CREATE TABLE t(i INTEGER, s TEXT COLLATE NOCASE)",
		"WITH RECURSIVE c(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM c WHERE x < 300) INSERT INTO t SELECT x % 30, char(97 + x % 26) FROM c",
		"VACUUM",
		"UPDATE t SET i = i + 100 WHERE i IN (1, 2, 3, '4', 5.0) RETURNING rowid, i",
		"DELETE FROM t WHERE s IN ('A', 'b', 'C ', 'd') RETURNING rowid, s",
		"DELETE FROM t WHERE i NOT IN (5, 6, 7, NULL) RETURNING rowid",
		"SELECT count(*) FROM t",
	})
	differ(t, "IN list set utf16", []string{
		"PRAGMA encoding = 'UTF-16le'",
		"CREATE TABLE u(s TEXT, n TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM)",
		"INSERT INTO u VALUES('a', 'A', 'x '), ('é', 'É', 'y'), ('z', 'q' || char(0) || 'w', 'z  '), (1, 2, 3)",
		"SELECT rowid FROM u WHERE s IN ('a', 'é', 'b', 1)",
		"SELECT rowid FROM u WHERE n IN ('a', 'é', 'Q' || char(0) || 'x', '2')",
		"SELECT rowid FROM u WHERE r IN ('x', 'y  ', 'z', '3 ')",
		"SELECT group_concat(quote(s IN ('a', NULL, 'b')), '') FROM u",
	})
}

// The list's values bound as parameters, of every class.
func TestInListSetParams(t *testing.T) {
	c, m := boundPair(t, []string{
		"CREATE TABLE t(i INTEGER, s TEXT COLLATE NOCASE, x)",
		"WITH RECURSIVE c(v) AS (VALUES(-5) UNION ALL SELECT v+1 FROM c WHERE v < 200) INSERT INTO t SELECT v % 17, char(97 + abs(v) % 26), CASE v % 3 WHEN 0 THEN v WHEN 1 THEN '' || v ELSE v + 0.5 END FROM c",
		"VACUUM",
	})
	sets := [][]any{
		{1, 2, 3},
		{"4", 5.0, 6.5, nil, []byte("7"), "x"},
		{int64(-5), int64(16), int64(201), 3},
		{"A", "c", "Z", "b "},
		{nil, nil, nil},
	}
	for _, args := range sets {
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		for _, q := range []string{
			"SELECT rowid FROM t WHERE i IN (" + marks + ")",
			"SELECT rowid FROM t WHERE s IN (" + marks + ")",
			"SELECT rowid FROM t WHERE x NOT IN (" + marks + ")",
			"SELECT group_concat(quote(x IN (1, " + marks + ", 'a')), '') FROM t",
			"SELECT count(*) FROM t WHERE i IN (" + marks + ")",
		} {
			want := boundRows(c, q, args...)
			got := boundRows(m, q, args...)
			if want != got {
				t.Errorf("%s %v:\n  cgo:   %s\n  musql: %s", q, args, want, got)
			}
		}
	}
}
