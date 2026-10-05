package compat

// Bound PARAMETERS, which this package's own differ structurally cannot
// reach: differ() runs a fixed list of SQL strings with no argument channel
// (there is no runArgs), so a defect that only shows with a bound value --
// an affinity applied to the wrong side, a "?" that never reaches the
// program, a named-parameter form the parser mis-numbers -- passes every
// gate here. These two tests open BOTH drivers directly and compare them
// argument for argument instead.

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func boundRows(db *sql.DB, q string, args ...any) string {
	rows, err := db.Query(q, args...)
	if err != nil {
		return "ERR:" + boundErrShort(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var sb strings.Builder
	sb.WriteString(strings.Join(cols, "|") + "::")
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "SCANERR:" + err.Error()
		}
		for _, v := range vals {
			if b, ok := v.([]byte); ok {
				v = "b:" + string(b)
			}
			sb.WriteString(fmt.Sprintf("%v,", v))
		}
		sb.WriteString(";")
	}
	if rows.Err() != nil {
		return "ERR:" + boundErrShort(rows.Err())
	}
	return sb.String()
}

func boundErrShort(err error) string {
	s := err.Error()
	for _, p := range []string{"engine: ", "driver: ", "vdbe: "} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i]
	}
	return s
}

func TestBoundParameterValues(t *testing.T) {
	c, _ := sql.Open("sqlite3", ":memory:")
	defer c.Close()
	m, _ := sql.Open("sqlite", t.TempDir()+"/b.db")
	defer m.Close()
	setup := []string{
		`CREATE TABLE t(i INTEGER, r REAL, s TEXT, b BLOB, n NUMERIC, x)`,
		`INSERT INTO t VALUES(1, 1.0, '1', x'31', 1, 1)`,
		`INSERT INTO t VALUES(2, 2.5, 'two', x'0002', '2.0', 'two')`,
		`INSERT INTO t VALUES(NULL,NULL,NULL,NULL,NULL,NULL)`,
	}
	for _, s := range setup {
		if _, err := c.Exec(s); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	ts := time.Date(2024, 2, 29, 13, 45, 56, 789000000, time.UTC)
	vals := []any{nil, int64(0), int64(1), int64(-1), int64(1) << 62, 2.5, -2.5,
		"", "abc", "1", "1.0", []byte{}, []byte{0x00, 0xff}, true, false, ts, 0.0}
	queries := []string{
		`SELECT typeof(?), quote(?)`,
		`SELECT ? = 1, ? = '1', ? IS NULL`,
		`SELECT i, r, s FROM t WHERE i = ? ORDER BY i`,
		`SELECT s FROM t WHERE s = ? ORDER BY s`,
		`SELECT b FROM t WHERE b = ? ORDER BY b`,
		`SELECT n FROM t WHERE n = ? ORDER BY n`,
		`SELECT count(*) FROM t WHERE x = ?`,
		`SELECT count(*) FROM t WHERE s LIKE ?`,
		`SELECT count(*) FROM t WHERE i IN (?, 2)`,
		`SELECT quote(CAST(? AS TEXT)), quote(CAST(? AS INTEGER)), quote(CAST(? AS REAL)), quote(CAST(? AS BLOB)), quote(CAST(? AS NUMERIC))`,
		`SELECT quote(? + 1), quote(? || 'x'), quote(abs(?)), quote(length(?))`,
		`SELECT quote(max(?, 1)), quote(min(?, 1))`,
	}
	n := 0
	for _, q := range queries {
		nArgs := strings.Count(q, "?")
		for _, v := range vals {
			args := make([]any, nArgs)
			for i := range args {
				args[i] = v
			}
			cv := boundRows(c, q, args...)
			mv := boundRows(m, q, args...)
			if cv != mv {
				n++
				t.Errorf("%s with %#v\n  cgo:    %s\n  musql: %s", q, v, cv, mv)
			}
		}
	}
	// Named / numbered parameter forms.
	for _, tc := range []struct {
		q    string
		args []any
	}{
		{`SELECT :a, :b`, []any{sql.Named("a", 1), sql.Named("b", 2)}},
		{`SELECT @a, @b`, []any{sql.Named("a", 1), sql.Named("b", 2)}},
		{`SELECT $a, $b`, []any{sql.Named("a", 1), sql.Named("b", 2)}},
		{`SELECT ?1, ?2, ?1`, []any{1, 2}},
		{`SELECT ?2, ?1`, []any{1, 2}},
		{`SELECT ?, ?, ?`, []any{1, 2, 3}},
		{`SELECT :a, :a`, []any{sql.Named("a", 7)}},
		{`SELECT ?1 + ?1`, []any{5}},
		{`SELECT ?3`, []any{1, 2, 3}},
	} {
		cv := boundRows(c, tc.q, tc.args...)
		mv := boundRows(m, tc.q, tc.args...)
		if cv != mv {
			n++
			t.Errorf("%s with %v\n  cgo:    %s\n  musql: %s", tc.q, tc.args, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d bound-parameter mismatches", n)
	}
}


func TestBoundParametersInDML(t *testing.T) {
	c, _ := sql.Open("sqlite3", ":memory:")
	defer c.Close()
	m, _ := sql.Open("sqlite", t.TempDir()+"/b2.db")
	defer m.Close()
	setup := []string{
		`CREATE TABLE t(i INTEGER PRIMARY KEY, s TEXT, n NUMERIC)`,
		`CREATE TABLE u(a, b)`,
		`INSERT INTO u VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE INDEX iu ON u(a)`,
	}
	for _, q := range setup {
		if _, err := c.Exec(q); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	type step struct {
		q    string
		args []any
	}
	steps := []step{
		{`INSERT INTO t VALUES(?,?,?)`, []any{1, "a", "1"}},
		{`INSERT INTO t VALUES(?,?,?)`, []any{2, 3, 2.0}},
		{`INSERT INTO t VALUES(?,?,?)`, []any{nil, []byte{1, 2}, nil}},
		{`SELECT i, quote(s), quote(n), typeof(s), typeof(n) FROM t ORDER BY i`, nil},
		{`UPDATE t SET s = ? WHERE i = ?`, []any{"upd", 1}},
		{`SELECT quote(s) FROM t WHERE i = ?`, []any{1}},
		{`DELETE FROM t WHERE i = ?`, []any{2}},
		{`SELECT count(*) FROM t`, nil},
		{`INSERT INTO t VALUES(?,?,?) RETURNING i, quote(s)`, []any{9, "r", 1}},
		{`SELECT a FROM u ORDER BY a LIMIT ? OFFSET ?`, []any{2, 1}},
		{`SELECT a FROM u WHERE a > ? ORDER BY a`, []any{1}},
		{`SELECT a FROM u WHERE a BETWEEN ? AND ? ORDER BY a`, []any{1, 2}},
		{`SELECT group_concat(b) FROM u WHERE a IN (?,?)`, []any{1, 3}},
		{`SELECT (SELECT b FROM u WHERE a = ?)`, []any{2}},
		{`WITH x(v) AS (SELECT ?) SELECT v, typeof(v) FROM x`, []any{"c"}},
		{`SELECT a FROM u UNION ALL SELECT ? ORDER BY 1`, []any{99}},
		{`INSERT INTO u SELECT ?, ? WHERE ?`, []any{4, "w", 1}},
		{`SELECT count(*) FROM u`, nil},
		{`UPDATE u SET b = ? WHERE a = ? RETURNING a, b`, []any{"Z", 4}},
		{`SELECT a, b FROM u ORDER BY a`, nil},
		{`SELECT ?, changes(), total_changes()>0`, []any{1}},
		{`INSERT INTO u VALUES(?,?) ON CONFLICT DO NOTHING`, []any{5, "q"}},
		{`SELECT printf('%s-%d', ?, ?)`, []any{"p", 4}},
		{`SELECT json_extract(?, '$.a')`, []any{`{"a":5}`}},
		{`SELECT a FROM u WHERE b GLOB ? ORDER BY a`, []any{"[xy]"}},
		{`SELECT a FROM u WHERE b LIKE ? ESCAPE ? ORDER BY a`, []any{"x", "\\"}},
	}
	n := 0
	for _, st := range steps {
		cv := boundRows(c, st.q, st.args...)
		mv := boundRows(m, st.q, st.args...)
		if cv != mv {
			n++
			t.Errorf("%s with %v\n  cgo:    %s\n  musql: %s", st.q, st.args, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d of %d parameterised statements diverged", n, len(steps))
	}
}
