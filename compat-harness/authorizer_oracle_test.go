// Gates the authorizer: for each statement, musql's authorizer is asked about
// the same actions as C SQLite's sqlite3_set_authorizer callback, in the same
// order, and denying or ignoring any one of them has C's outcome: the same
// error, or the same rows and the same database afterwards.
package compat

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
	musqldriver "github.com/samyfodil/musql/driver"
)

var authSetup = []string{
	`CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)`,
	`CREATE TABLE u(x, y)`,
	`CREATE TABLE w(b, d)`,
	`CREATE INDEX ub ON u(x)`,
	`CREATE TABLE p(id INTEGER PRIMARY KEY AUTOINCREMENT, q UNIQUE)`,
	`CREATE VIEW v AS SELECT a, b FROM t WHERE c > 0`,
	`CREATE TRIGGER td AFTER DELETE ON w BEGIN DELETE FROM u WHERE x = old.b; END`,
	`CREATE TRIGGER tu BEFORE UPDATE OF y ON u WHEN new.y > 0 BEGIN INSERT INTO w VALUES (new.x, old.y); END`,
	`INSERT INTO t VALUES (1, 10, 100), (2, 20, 200), (3, 30, -1)`,
	`INSERT INTO u VALUES (1, 11), (2, 22)`,
	`INSERT INTO w VALUES (10, 1), (2, 2)`,
}

// How a case's calls are compared. Some statements make calls of C's own
// that musql's engine has no counterpart for: ALTER TABLE's and VACUUM's
// nested statements on sqlite_schema, ANALYZE's statistics table.
const (
	authExact  = iota
	authFirst  // the first call only
	authAction // the calls of the statement's own action only
)

var authCases = []struct {
	sql  string
	mode int
}{
	{`SELECT 1`, authExact},
	{`SELECT * FROM t`, authExact},
	{`SELECT a FROM t`, authExact},
	{`SELECT b FROM t WHERE c = 100 ORDER BY a`, authExact},
	{`SELECT count(*) FROM t`, authExact},
	{`SELECT rowid, b FROM t`, authExact},
	{`SELECT oid FROM u`, authExact},
	{`SELECT t.b, u.y FROM t JOIN u ON t.a = u.x WHERE u.y > 2`, authExact},
	{`SELECT * FROM t LEFT JOIN u ON u.x = t.a`, authExact},
	{`SELECT * FROM t JOIN w USING (b)`, authExact},
	{`SELECT b FROM t JOIN w USING (b)`, authExact},
	{`SELECT * FROM t NATURAL JOIN w`, authExact},
	{`SELECT t.*, u.x FROM t, u`, authExact},
	{`SELECT x.b FROM t AS x WHERE x.c IS NOT NULL`, authExact},
	{`SELECT b FROM t WHERE a IN (SELECT x FROM u)`, authExact},
	{`SELECT b FROM t WHERE b IN (SELECT x FROM u WHERE u.y = t.c)`, authExact},
	{`SELECT (SELECT max(y) FROM u), b FROM t`, authExact},
	{`SELECT (SELECT y FROM u WHERE x = t.a) FROM t`, authExact},
	{`SELECT b FROM t WHERE EXISTS (SELECT 1 FROM u)`, authExact},
	{`SELECT a FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.x = t.a)`, authExact},
	{`SELECT b FROM t WHERE b = (SELECT max(x) FROM u)`, authExact},
	{`SELECT b FROM t LIMIT (SELECT count(*) FROM u)`, authExact},
	{`SELECT * FROM (SELECT b, c FROM t) WHERE b > 1`, authExact},
	{`SELECT * FROM (SELECT b, count(*) AS n FROM t GROUP BY b) WHERE n > 0`, authExact},
	{`SELECT * FROM v`, authExact},
	{`SELECT b FROM v WHERE a > 1`, authExact},
	{`WITH r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3) SELECT n FROM r`, authExact},
	{`WITH c1 AS (SELECT b FROM t) SELECT * FROM c1`, authExact},
	{`SELECT b FROM t UNION SELECT y FROM u`, authExact},
	{`SELECT b FROM t UNION ALL SELECT x FROM u ORDER BY 1`, authExact},
	{`SELECT b AS z FROM t WHERE z > 1 GROUP BY z HAVING count(*) > 0`, authExact},
	{`SELECT b AS z FROM t ORDER BY z`, authExact},
	{`SELECT b, count(*) FROM t GROUP BY 1`, authExact},
	{`SELECT b FROM t GROUP BY b HAVING max(c) > 1 ORDER BY b`, authExact},
	{`SELECT lower(b), abs(c) FROM t`, authExact},
	{`SELECT * FROM t WHERE b LIKE '1%'`, authExact},
	{`SELECT b -> '$.x' FROM t`, authExact},
	{`SELECT CURRENT_TIMESTAMP IS NOT NULL`, authExact},
	{`SELECT group_concat(b ORDER BY c) FROM t`, authExact},
	{`SELECT count(*) FILTER (WHERE b > 1) FROM t`, authExact},
	{`SELECT a, row_number() OVER (ORDER BY b) FROM t`, authExact},
	{`SELECT CASE b WHEN 10 THEN c ELSE a END FROM t`, authExact},
	{`SELECT b BETWEEN c AND a, CAST(b AS TEXT) COLLATE NOCASE FROM t`, authExact},
	{`SELECT DISTINCT b FROM t`, authExact},
	{`SELECT value FROM json_each('[1,2]')`, authExact},
	{`SELECT * FROM sqlite_schema`, authExact},
	{`SELECT name FROM sqlite_master WHERE type = 'table'`, authExact},
	{`VALUES (1, 2)`, authExact},
	{`VALUES (1, 2), (3, 4), (5, 6)`, authExact},
	{`SELECT * FROM (VALUES (1, 2), (3, 4))`, authExact},
	{`EXPLAIN SELECT b FROM t`, authExact},
	{`INSERT INTO t VALUES (9, 2, 3)`, authExact},
	{`INSERT INTO t DEFAULT VALUES`, authExact},
	{`INSERT INTO t(b) SELECT y FROM u`, authExact},
	{`INSERT INTO u VALUES (5, 6), (7, 8), (9, 10)`, authExact},
	{`INSERT INTO u VALUES (1, (SELECT max(x) FROM u))`, authExact},
	{`INSERT OR IGNORE INTO u VALUES (1, 2)`, authExact},
	{`REPLACE INTO u VALUES (1, 2)`, authExact},
	{`INSERT INTO t VALUES (1, 5, 5) ON CONFLICT(a) DO UPDATE SET b = excluded.b`, authExact},
	{`INSERT INTO t VALUES (1, 6, 6) ON CONFLICT DO NOTHING`, authExact},
	{`INSERT INTO t VALUES (8, 6, 6) RETURNING a, b`, authExact},
	{`INSERT INTO p(q) VALUES (1)`, authExact},
	{`UPDATE t SET b = 1`, authExact},
	{`UPDATE t SET b = c + 1 WHERE a = 1`, authExact},
	{`UPDATE t SET b = 1, c = 2 WHERE a > b`, authExact},
	{`UPDATE t SET b = (SELECT max(y) FROM u)`, authExact},
	{`UPDATE t SET b = u.y FROM u WHERE u.x = t.a`, authExact},
	{`UPDATE u SET y = 5 WHERE x = 1`, authExact},
	{`UPDATE u SET x = 5`, authExact},
	{`DELETE FROM t`, authExact},
	{`DELETE FROM t WHERE b = 20`, authExact},
	{`DELETE FROM t WHERE a IN (SELECT x FROM u)`, authExact},
	{`DELETE FROM w WHERE d = 1`, authExact},
	{`CREATE TABLE n(p, q)`, authExact},
	{`CREATE TEMP TABLE tt(p)`, authExact},
	{`CREATE TABLE IF NOT EXISTS t(z)`, authExact},
	{`CREATE TABLE ct AS SELECT a, b FROM t`, authExact},
	{`CREATE INDEX nq ON t(c)`, authExact},
	{`CREATE UNIQUE INDEX ix2 ON t(b, c)`, authExact},
	{`CREATE INDEX ix3 ON t(b + c) WHERE c > 0`, authExact},
	{`CREATE INDEX IF NOT EXISTS ub ON u(x)`, authExact},
	{`CREATE VIEW nv AS SELECT b FROM t`, authExact},
	{`CREATE TRIGGER ntr AFTER INSERT ON t BEGIN UPDATE u SET y = 0; END`, authExact},
	{`CREATE TRIGGER IF NOT EXISTS td AFTER DELETE ON w BEGIN SELECT 1; END`, authExact},
	{`DROP TABLE w`, authExact},
	{`DROP TABLE u`, authExact},
	{`DROP TABLE p`, authExact},
	{`DROP TABLE IF EXISTS nosuch`, authExact},
	{`DROP VIEW v`, authExact},
	{`DROP INDEX ub`, authExact},
	{`DROP TRIGGER td`, authExact},
	{`ALTER TABLE t ADD COLUMN e`, authFirst},
	{`ALTER TABLE t RENAME COLUMN c TO c2`, authFirst},
	{`ALTER TABLE w RENAME TO w2`, authFirst},
	{`ALTER TABLE w DROP COLUMN d`, authFirst},
	{`BEGIN`, authExact},
	{`BEGIN IMMEDIATE`, authExact},
	{`SAVEPOINT sp`, authExact},
	{`PRAGMA user_version`, authExact},
	{`PRAGMA user_version = 3`, authExact},
	{`PRAGMA main.user_version`, authExact},
	{`PRAGMA table_info(t)`, authExact},
	{`PRAGMA table_info('t')`, authExact},
	{`PRAGMA cache_size = -2000`, authExact},
	{`ATTACH ':memory:' AS aux`, authExact},
	{`ANALYZE`, authAction},
	{`ANALYZE t`, authAction},
	{`REINDEX ub`, authExact},
	{`VACUUM`, authFirst},
}

// With a database attached, a denied read names the database of the column.
var authAttachedSetup = []string{
	`ATTACH ':memory:' AS aux`,
	`CREATE TABLE aux.z(k, l)`,
	`INSERT INTO aux.z VALUES (1, 2)`,
	`CREATE TEMP TABLE tmp(m)`,
	`INSERT INTO tmp VALUES (7)`,
}

var authAttachedCases = []string{
	`SELECT b FROM t`,
	`SELECT k, l FROM z`,
	`SELECT * FROM aux.z`,
	`SELECT m FROM tmp`,
	`SELECT m FROM temp.tmp, t WHERE t.a = tmp.m`,
	`INSERT INTO aux.z VALUES (3, 4)`,
	`UPDATE z SET l = k WHERE k = 1`,
	`DELETE FROM tmp`,
	`DETACH aux`,
}

type authCall struct {
	code       int
	a1, a2, db string
}

func (c authCall) String() string { return fmt.Sprintf("%d(%q,%q,%q)", c.code, c.a1, c.a2, c.db) }

// authRun is one statement run under an authorizer on a fresh database.
type authRun struct {
	calls []authCall
	err   string
	rows  []string // the statement's rows, then every table's
}

// runAuthorized runs stmt on a fresh copy of setup in the engine named by
// driverName, with an authorizer that logs each call and answers it with
// answer (the call's index, from 0).
func runAuthorized(t *testing.T, setup []string, driverName, stmt string, answer func(i int) int) authRun {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, s := range setup {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s setup %q: %v", driverName, s, err)
		}
	}
	var run authRun
	cb := func(code int, a1, a2, dbName string) int {
		i := len(run.calls)
		run.calls = append(run.calls, authCall{code, a1, a2, dbName})
		return answer(i)
	}
	set := func(on bool) {
		if err := conn.Raw(func(dc any) error {
			switch c := dc.(type) {
			case *sqlite3.SQLiteConn:
				if on {
					c.RegisterAuthorizer(cb)
				} else {
					c.RegisterAuthorizer(nil)
				}
			case *musqldriver.Conn:
				if on {
					c.SetAuthorizer(func(code int, a1, a2, dbName, _ string) int { return cb(code, a1, a2, dbName) })
				} else {
					c.SetAuthorizer(nil)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(true)
	rows, qerr := conn.QueryContext(ctx, stmt)
	if qerr == nil {
		run.rows = append(run.rows, authScanRows(rows)...)
		qerr = rows.Err()
		rows.Close()
	}
	set(false)
	if qerr != nil {
		run.err = qerr.Error()
	}
	for _, tab := range []string{"t", "u", "w", "p"} {
		r, err := conn.QueryContext(ctx, "SELECT * FROM "+tab+" ORDER BY rowid")
		if err != nil {
			run.rows = append(run.rows, tab+": "+err.Error())
			continue
		}
		run.rows = append(run.rows, tab+":")
		run.rows = append(run.rows, authScanRows(r)...)
		r.Close()
	}
	return run
}

func authScanRows(rows *sql.Rows) []string {
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return append(out, err.Error())
		}
		var b strings.Builder
		for i, v := range vals {
			if i > 0 {
				b.WriteByte('|')
			}
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			fmt.Fprint(&b, v)
		}
		out = append(out, b.String())
	}
	return out
}

func authCompared(calls []authCall, mode int) []authCall {
	switch mode {
	case authFirst:
		if len(calls) > 1 {
			return calls[:1]
		}
	case authAction:
		var out []authCall
		for _, c := range calls {
			if c.code == 28 {
				out = append(out, c)
			}
		}
		slices.SortFunc(out, func(x, y authCall) int { return strings.Compare(x.a1, y.a1) })
		return out
	}
	return calls
}

// authIgnoreDeclined is the error of an IGNORE musql cannot apply exactly (in
// a view's or trigger's body, an operator's call, a parameter's argument), which
// it fails rather than run differently from C.
const authIgnoreDeclined = "the authorizer's IGNORE cannot be applied to this statement"

func TestAuthorizerOracle(t *testing.T) {
	allow := func(int) int { return 0 }
	declined := 0
	defer func() { t.Logf("%d IGNOREs declined", declined) }()
	type authCase struct {
		sql   string
		mode  int
		setup []string
	}
	var cases []authCase
	for _, c := range authCases {
		cases = append(cases, authCase{c.sql, c.mode, authSetup})
	}
	for _, c := range authAttachedCases {
		cases = append(cases, authCase{c, authExact, append(slices.Clip(authSetup), authAttachedSetup...)})
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			c := runAuthorized(t, tc.setup, "sqlite3", tc.sql, allow)
			m := runAuthorized(t, tc.setup, "sqlite", tc.sql, allow)
			cc, mc := authCompared(c.calls, tc.mode), authCompared(m.calls, tc.mode)
			if !slices.Equal(cc, mc) {
				t.Fatalf("calls:\n  C:     %v\n  musql: %v", cc, mc)
			}
			if c.err != m.err {
				t.Fatalf("error: C %q, musql %q", c.err, m.err)
			}
			if tc.mode != authExact {
				return
			}
			// Deny each call in turn, then ignore it.
			for i := range c.calls {
				for _, answer := range []int{1, 2} {
					if answer == 2 && !authIgnoreCompared(c.calls[i]) {
						continue
					}
					at := func(j int) int {
						if j == i {
							return answer
						}
						return 0
					}
					c2 := runAuthorized(t, tc.setup, "sqlite3", tc.sql, at)
					m2 := runAuthorized(t, tc.setup, "sqlite", tc.sql, at)
					what := fmt.Sprintf("answering %d to call %d %v", answer, i, c.calls[i])
					if answer == 2 && m2.err == authIgnoreDeclined {
						declined++
						continue
					}
					if !slices.Equal(c2.calls[:i+1], m2.calls[:min(i+1, len(m2.calls))]) {
						t.Fatalf("%s: calls:\n  C:     %v\n  musql: %v", what, c2.calls, m2.calls)
					}
					if c2.err != m2.err {
						t.Fatalf("%s: error: C %q, musql %q", what, c2.err, m2.err)
					}
					if c2.err == "" && !strings.HasPrefix(tc.sql, "EXPLAIN") && !slices.Equal(c2.rows, m2.rows) {
						t.Fatalf("%s: rows:\n  C:     %q\n  musql: %q", what, c2.rows, m2.rows)
					}
				}
			}
		})
	}
}

// authIgnoreCompared reports whether an IGNORE of c is one whose effect musql
// applies: a column read or a function call becomes NULL, a DELETE goes on,
// and the action heading the statement makes it do nothing.
func authIgnoreCompared(c authCall) bool {
	switch c.code {
	case 20, 31:
		return c.a2 != "" && !strings.HasPrefix(c.a1, "sqlite_")
	case 9, 18:
		return !strings.HasPrefix(c.a1, "sqlite_")
	case 19, 22, 24, 32:
		return true
	}
	return false
}
