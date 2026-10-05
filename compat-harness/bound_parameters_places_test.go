package compat

// Tests bound parameters in every syntactic position and prepared-statement reuse.

import (
	"database/sql"
	"fmt"
	"testing"
)

func boundPair(t *testing.T, setup []string) (cgo, mus *sql.DB) {
	t.Helper()
	c, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	m, err := sql.Open("sqlite", t.TempDir()+"/p.db")
	if err != nil {
		t.Fatal(err)
	}
	c.SetMaxOpenConns(1)
	m.SetMaxOpenConns(1)
	t.Cleanup(func() { c.Close(); m.Close() })
	for _, s := range setup {
		if _, err := c.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
		if _, err := m.Exec(s); err != nil {
			t.Fatalf("musql %s: %v", s, err)
		}
	}
	return c, m
}

// TestBoundParameterPlaces puts "?" in every syntactic position that matters.
func TestBoundParameterPlaces(t *testing.T) {
	c, m := boundPair(t, []string{
		`CREATE TABLE u(a INTEGER PRIMARY KEY, b TEXT, g INTEGER)`,
		`INSERT INTO u VALUES(1,'x',1),(2,'y',1),(3,'z',2),(4,'X',2)`,
		`CREATE TABLE k(a INTEGER PRIMARY KEY, b TEXT)`,
		`INSERT INTO k VALUES(1,'one')`,
		`CREATE UNIQUE INDEX ku ON k(b)`,
	})
	n := 0
	for _, st := range []struct {
		q    string
		args []any
	}{
		// LIMIT / OFFSET.
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{2}},
		{`SELECT a FROM u ORDER BY a LIMIT ? OFFSET ?`, []any{2, 2}},
		{`SELECT a FROM u ORDER BY a LIMIT ?, ?`, []any{1, 2}},
		{`SELECT a FROM u ORDER BY a LIMIT -1 OFFSET ?`, []any{1}},
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{"2"}},
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{2.0}},
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{2.5}},
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{"abc"}},
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{nil}},
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{[]byte("2")}},
		// GROUP BY / HAVING / FILTER.
		{`SELECT g, count(*) FROM u GROUP BY g HAVING count(*) > ? ORDER BY g`, []any{1}},
		{`SELECT g, count(*) FROM u GROUP BY g HAVING g = ? ORDER BY g`, []any{2}},
		{`SELECT g, count(*) FROM u GROUP BY ? ORDER BY 1`, []any{1}},
		{`SELECT count(*) FILTER (WHERE a > ?) FROM u`, []any{2}},
		{`SELECT g, count(*) FILTER (WHERE b = ?) FROM u GROUP BY g ORDER BY g`, []any{"x"}},
		// Window frames: the offsets, the ORDER BY, and the PARTITION BY.
		{`SELECT a, sum(a) OVER (ORDER BY a ROWS BETWEEN ? PRECEDING AND ? FOLLOWING) FROM u ORDER BY a`, []any{1, 1}},
		{`SELECT a, sum(a) OVER (ORDER BY a ROWS ? PRECEDING) FROM u ORDER BY a`, []any{2}},
		{`SELECT a, sum(a) OVER (ORDER BY a ROWS BETWEEN ? PRECEDING AND CURRENT ROW) FROM u ORDER BY a`, []any{-1}},
		{`SELECT a, sum(a) OVER (ORDER BY a ROWS BETWEEN ? PRECEDING AND CURRENT ROW) FROM u ORDER BY a`, []any{"x"}},
		{`SELECT a, sum(a) OVER (ORDER BY a RANGE BETWEEN ? PRECEDING AND ? FOLLOWING) FROM u ORDER BY a`, []any{1, 1}},
		{`SELECT a, count(*) OVER (PARTITION BY ? ORDER BY a) FROM u ORDER BY a`, []any{1}},
		// ORDER BY: a parameter there is a constant, not a column index.
		{`SELECT a FROM u ORDER BY ?, a`, []any{2}},
		{`SELECT a FROM u ORDER BY ? DESC, a`, []any{1}},
		// COLLATE, CASE, IN (SELECT), a correlated subquery, a CTE seed.
		{`SELECT a FROM u WHERE b = ? COLLATE NOCASE ORDER BY a`, []any{"X"}},
		{`SELECT a FROM u WHERE b COLLATE NOCASE = ? ORDER BY a`, []any{"X"}},
		{`SELECT CASE WHEN a > ? THEN ? ELSE ? END FROM u ORDER BY a`, []any{2, "hi", "lo"}},
		{`SELECT a FROM u WHERE a IN (SELECT a FROM u WHERE g = ?) ORDER BY a`, []any{2}},
		{`SELECT a, (SELECT count(*) FROM u v WHERE v.g = u.g AND v.a > ?) FROM u ORDER BY a`, []any{1}},
		{`WITH RECURSIVE r(v) AS (SELECT ? UNION ALL SELECT v+1 FROM r WHERE v < ?) SELECT v FROM r`, []any{1, 4}},
		{`SELECT a FROM u WHERE a = ? UNION SELECT a FROM u WHERE a = ? ORDER BY 1`, []any{1, 3}},
		{`SELECT a FROM u EXCEPT SELECT ? ORDER BY 1`, []any{2}},
		{`SELECT ? UNION ALL SELECT ? ORDER BY 1`, []any{"b", "a"}},
		// VALUES, multi-row, and a JOIN's ON.
		{`SELECT * FROM (VALUES(?,?),(?,?)) ORDER BY 1`, []any{1, "a", 2, "b"}},
		{`SELECT u.a, k.b FROM u JOIN k ON k.a = u.a AND u.b <> ? ORDER BY u.a`, []any{"zzz"}},
		{`SELECT u.a FROM u LEFT JOIN k ON k.a = u.a AND k.b = ? ORDER BY u.a`, []any{"one"}},
		// ON CONFLICT: the target's WHERE, DO UPDATE's SET and WHERE, excluded.
		{`INSERT INTO k VALUES(?,?) ON CONFLICT(a) DO UPDATE SET b = ? RETURNING a, b`, []any{1, "dup", "upd"}},
		{`INSERT INTO k VALUES(?,?) ON CONFLICT(a) DO UPDATE SET b = excluded.b || ? RETURNING a, b`, []any{1, "e", "!"}},
		{`INSERT INTO k VALUES(?,?) ON CONFLICT(a) DO UPDATE SET b = ? WHERE k.a > ? RETURNING a, b`, []any{1, "z", "w", 99}},
		{`INSERT INTO k VALUES(?,?) ON CONFLICT(b) DO NOTHING RETURNING a`, []any{7, "e!"}},
		{`SELECT a, b FROM k ORDER BY a`, nil},
		// The conflict-clause forms, whose bind has to survive an abort.
		{`INSERT OR IGNORE INTO k VALUES(?,?)`, []any{1, "nope"}},
		{`INSERT OR REPLACE INTO k VALUES(?,?) RETURNING a, b`, []any{1, "rep"}},
		{`UPDATE OR IGNORE k SET a = ? WHERE a = ?`, []any{1, 1}},
		{`SELECT a, b FROM k ORDER BY a`, nil},
		// UPDATE ... FROM, and a DELETE's own subquery.
		{`UPDATE u SET b = ? FROM k WHERE k.a = u.a AND k.b = ? RETURNING u.a, u.b`, []any{"from", "rep"}},
		{`DELETE FROM u WHERE a IN (SELECT ? ) RETURNING a`, []any{4}},
		{`SELECT a, b FROM u ORDER BY a`, nil},
	} {
		cv := boundRows(c, st.q, st.args...)
		mv := boundRows(m, st.q, st.args...)
		if cv != mv {
			n++
			t.Errorf("%s with %v\n  cgo:    %s\n  musql: %s", st.q, st.args, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d parameter positions diverged", n)
	}
}

// TestBoundParametersFireTrigger tests binds in trigger bodies.
func TestBoundParametersFireTrigger(t *testing.T) {
	c, m := boundPair(t, []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TABLE log(what TEXT, old TEXT, new TEXT)`,
		`CREATE TRIGGER ai AFTER INSERT ON t BEGIN
			INSERT INTO log VALUES('i', NULL, NEW.b || ':' || typeof(NEW.b));
		END`,
		`CREATE TRIGGER au AFTER UPDATE ON t WHEN NEW.b <> OLD.b BEGIN
			INSERT INTO log VALUES('u', OLD.b, NEW.b);
		END`,
		`CREATE TRIGGER ad BEFORE DELETE ON t BEGIN
			INSERT INTO log VALUES('d', OLD.b, NULL);
		END`,
		`CREATE VIEW v AS SELECT a, b FROM t`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN
			INSERT INTO t VALUES(NEW.a, NEW.b || '/v');
		END`,
	})
	n := 0
	for _, st := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO t VALUES(?,?)`, []any{1, "one"}},
		{`INSERT INTO t VALUES(?,?)`, []any{2, 22}},
		{`INSERT INTO t VALUES(?,?)`, []any{3, nil}},
		{`INSERT INTO t VALUES(?,?)`, []any{4, []byte{0xff}}},
		{`UPDATE t SET b = ? WHERE a = ?`, []any{"ONE", 1}},
		{`UPDATE t SET b = ? WHERE a = ?`, []any{"ONE", 1}},
		{`DELETE FROM t WHERE a = ?`, []any{4}},
		{`INSERT INTO v VALUES(?,?)`, []any{9, "vv"}},
		{`SELECT a, quote(b) FROM t ORDER BY a`, nil},
		{`SELECT what, quote(old), quote(new) FROM log`, nil},
		// A RAISE in the body, with the parameter deciding whether it fires.
		{`CREATE TRIGGER g BEFORE INSERT ON t WHEN NEW.a > 100 BEGIN SELECT RAISE(ABORT,'too big'); END`, nil},
		{`INSERT INTO t VALUES(?,?)`, []any{101, "no"}},
		{`INSERT INTO t VALUES(?,?)`, []any{5, "yes"}},
		{`SELECT count(*) FROM t`, nil},
	} {
		cv := boundRows(c, st.q, st.args...)
		mv := boundRows(m, st.q, st.args...)
		if cv != mv {
			n++
			t.Errorf("%s with %v\n  cgo:    %s\n  musql: %s", st.q, st.args, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d trigger-firing statements diverged", n)
	}
}

// TestPreparedStatementReuse prepares once and reuses with changing arg types.
func TestPreparedStatementReuse(t *testing.T) {
	c, m := boundPair(t, []string{
		`CREATE TABLE t(k, v)`,
		`INSERT INTO t VALUES(1,'a'),(2,'b'),('1','c'),(1.0,'d'),(NULL,'e'),(x'31','f')`,
	})
	for si, spec := range []struct {
		q    string
		args [][]any
	}{
		{`SELECT quote(k), quote(v), typeof(k) FROM t WHERE k = ? ORDER BY v`, [][]any{
			{1}, {"1"}, {1.0}, {nil}, {[]byte("1")}, {int64(1) << 62}, {true}, {1},
		}},
		{`SELECT typeof(?), quote(?), ? IS NULL, ? = ?`, [][]any{
			{1, 1, 1, 1, 1},
			{"s", "s", "s", "s", "s"},
			{nil, nil, nil, nil, nil},
			{2.5, 2.5, 2.5, 2.5, 2.5},
			{[]byte{0}, []byte{0}, []byte{0}, []byte{0}, []byte{0}},
			{1, "1", nil, 1, "1"},
		}},
		{`SELECT v FROM t ORDER BY v LIMIT ?`, [][]any{{1}, {6}, {0}, {-1}, {"2"}, {3}}},
		{`SELECT count(*) FROM t WHERE k IN (?,?)`, [][]any{
			{1, 2}, {"1", "2"}, {nil, nil}, {1, nil}, {1, 2},
		}},
		{`INSERT INTO t VALUES(?,?) RETURNING quote(k), typeof(k)`, [][]any{
			{10, "x"}, {"10", "y"}, {10.5, "z"}, {nil, nil}, {[]byte{9}, "w"},
		}},
	} {
		si, spec := si, spec
		t.Run(fmt.Sprintf("%02d", si), func(t *testing.T) {
			cs, err := c.Prepare(spec.q)
			if err != nil {
				t.Fatalf("cgo prepare: %v", err)
			}
			defer cs.Close()
			ms, err := m.Prepare(spec.q)
			if err != nil {
				t.Fatalf("musql prepare: %v", err)
			}
			defer ms.Close()
			for ai, args := range spec.args {
				cv := preparedRows(cs, args...)
				mv := preparedRows(ms, args...)
				if cv != mv {
					t.Errorf("exec %d of %s with %v\n  cgo:    %s\n  musql: %s", ai, spec.q, args, cv, mv)
				}
			}
		})
	}
}

// preparedRows runs a prepared statement; handle is NOT re-prepared.
func preparedRows(st *sql.Stmt, args ...any) string {
	rows, err := st.Query(args...)
	if err != nil {
		return "ERR:" + boundErrShort(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := fmt.Sprint(cols) + "::"
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
			out += fmt.Sprintf("%v,", v)
		}
		out += ";"
	}
	if rows.Err() != nil {
		return "ERR:" + boundErrShort(rows.Err())
	}
	return out
}

// TestBoundLimitOffsetEveryBody puts LIMIT/OFFSET through every SELECT body.
func TestBoundLimitOffsetEveryBody(t *testing.T) {
	c, m := boundPair(t, []string{
		`CREATE TABLE u(a INTEGER PRIMARY KEY, b TEXT, g INTEGER)`,
		`INSERT INTO u VALUES(1,'x',1),(2,'y',1),(3,'z',2),(4,'X',2)`,
	})
	n := 0
	for _, st := range []struct {
		q    string
		args []any
	}{
		{`SELECT 1 LIMIT ?`, []any{1}},
		{`SELECT 1 LIMIT ? OFFSET ?`, []any{1, 1}},
		{`SELECT a FROM u LIMIT ?`, []any{1}},
		{`SELECT a FROM u LIMIT ? OFFSET ?`, []any{2, 1}},
		{`SELECT a FROM u ORDER BY a LIMIT ?`, []any{1}},
		{`SELECT a FROM u ORDER BY a DESC LIMIT ? OFFSET ?`, []any{2, 1}},
		{`SELECT a FROM u ORDER BY a LIMIT ? OFFSET 1`, []any{2}},
		{`SELECT a FROM u ORDER BY a LIMIT 2 OFFSET ?`, []any{1}},
		{`SELECT DISTINCT g FROM u LIMIT ?`, []any{1}},
		{`SELECT DISTINCT g FROM u ORDER BY g LIMIT ?`, []any{1}},
		{`SELECT count(*) FROM u LIMIT ?`, []any{1}},
		{`SELECT count(*) FROM u LIMIT ?`, []any{0}},
		{`SELECT g, count(*) FROM u GROUP BY g LIMIT ?`, []any{1}},
		{`SELECT g, count(*) FROM u GROUP BY g ORDER BY g DESC LIMIT ?`, []any{1}},
		{`SELECT g, count(*) FROM u GROUP BY g HAVING g>0 LIMIT ? OFFSET ?`, []any{1, 1}},
		{`SELECT a, sum(a) OVER (ORDER BY a) FROM u LIMIT ?`, []any{2}},
		{`SELECT a, sum(a) OVER (ORDER BY a) FROM u ORDER BY a DESC LIMIT ?`, []any{2}},
		{`SELECT a FROM u UNION ALL SELECT 9 LIMIT ?`, []any{2}},
		{`SELECT a FROM u UNION SELECT 9 ORDER BY 1 DESC LIMIT ?`, []any{2}},
		{`SELECT * FROM (SELECT a FROM u LIMIT ?)`, []any{2}},
		{`SELECT * FROM (SELECT a FROM u ORDER BY a DESC LIMIT ?)`, []any{2}},
		{`SELECT (SELECT a FROM u ORDER BY a DESC LIMIT ?)`, []any{1}},
		{`SELECT a FROM u WHERE a IN (SELECT a FROM u ORDER BY a DESC LIMIT ?) ORDER BY a`, []any{2}},
		{`WITH x AS (SELECT a FROM u ORDER BY a DESC LIMIT ?) SELECT * FROM x ORDER BY a`, []any{2}},
		{`SELECT group_concat(a) FROM (SELECT a FROM u ORDER BY a DESC LIMIT ?)`, []any{2}},
		{`DELETE FROM u WHERE a IN (SELECT a FROM u ORDER BY a DESC LIMIT ?) RETURNING a`, []any{1}},
		{`SELECT a FROM u ORDER BY a`, nil},
	} {
		cv := boundRows(c, st.q, st.args...)
		mv := boundRows(m, st.q, st.args...)
		if cv != mv {
			n++
			t.Errorf("%s with %v\n  cgo:    %s\n  musql: %s", st.q, st.args, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d LIMIT/OFFSET bodies diverged", n)
	}
}
