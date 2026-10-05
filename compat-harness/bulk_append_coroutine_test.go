// Tests INSERT ... SELECT with a coroutine source, where rows leave memory
// as they arrive. Each script is tested for statement success/failure and row
// values in rowid order against C SQLite. Tests include rowid anomalies,
// partial failures, transactions, and subsequent statements on loaded tables.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

const spillCTE = `WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<%N%) `

func spillScript(n string, stmts ...string) []string {
	cte := strings.ReplaceAll(spillCTE, "%N%", n)
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = strings.ReplaceAll(s, "%CTE%", cte)
	}
	return out
}

func TestBulkAppendCoroutineMatchesCSQLite(t *testing.T) {
	cases := []struct {
		name   string
		script []string
		verify []string
	}{
		{"append", spillScript("200",
			`CREATE TABLE t(a, b)`,
			`%CTE%INSERT INTO t SELECT x, x*2 FROM c`,
			`INSERT INTO t VALUES(1000, 1)`,
			`%CTE%INSERT INTO t SELECT x+1000, zeroblob(x) FROM c`,
		), []string{`SELECT rowid, a, length(b), typeof(b) FROM t ORDER BY rowid`, `SELECT count(*), sum(a) FROM t`}},

		{"rowid below the maximum disarms", spillScript("40",
			`CREATE TABLE u(a)`,
			`INSERT INTO u(rowid, a) VALUES(500, 'pre')`,
			`%CTE%INSERT INTO u(rowid, a) SELECT CASE WHEN x=10 THEN 3 ELSE x+500 END, x FROM c`,
		), []string{`SELECT rowid, a FROM u ORDER BY rowid`}},

		{"duplicate of a spilled rowid", spillScript("30",
			`CREATE TABLE v(a)`,
			`%CTE%INSERT INTO v(rowid, a) SELECT CASE WHEN x=20 THEN 5 ELSE x END, x FROM c`,
			`%CTE%INSERT OR IGNORE INTO v(rowid, a) SELECT CASE WHEN x=20 THEN 5 ELSE x END, x FROM c`,
			`%CTE%INSERT OR REPLACE INTO v(rowid, a) SELECT CASE WHEN x=25 THEN 7 ELSE x+100 END, -x FROM c`,
		), []string{`SELECT rowid, a FROM v ORDER BY rowid`}},

		{"failure part way", spillScript("100",
			`CREATE TABLE w(a NOT NULL)`,
			`%CTE%INSERT OR FAIL INTO w SELECT CASE WHEN x=60 THEN NULL ELSE x END FROM c`,
			`CREATE TABLE w2(a)`,
			`%CTE%INSERT INTO w2 SELECT CASE WHEN x=60 THEN abs(-9223372036854775807-1) ELSE x END FROM c`,
			`INSERT INTO w2 VALUES(1)`,
			`%CTE%INSERT OR IGNORE INTO w SELECT CASE WHEN x%7=0 THEN NULL ELSE x+1000 END FROM c`,
		), []string{`SELECT rowid, a FROM w ORDER BY rowid`, `SELECT rowid, a FROM w2 ORDER BY rowid`}},

		{"transactions", spillScript("80",
			`CREATE TABLE t2(a)`,
			`BEGIN`,
			`%CTE%INSERT INTO t2 SELECT x FROM c`,
			`ROLLBACK`,
			`%CTE%INSERT INTO t2 SELECT x FROM c`,
			`BEGIN`,
			`INSERT INTO t2 VALUES('in txn')`,
			`%CTE%INSERT INTO t2 SELECT -x FROM c`,
			`ROLLBACK`,
			`BEGIN`,
			`%CTE%INSERT INTO t2 SELECT x*10 FROM c`,
			`COMMIT`,
		), []string{`SELECT rowid, a FROM t2 ORDER BY rowid`}},

		{"savepoints", spillScript("50",
			`CREATE TABLE s(a)`,
			`SAVEPOINT sp1`,
			`%CTE%INSERT INTO s SELECT x FROM c`,
			`SAVEPOINT sp2`,
			`%CTE%INSERT INTO s SELECT x+100 FROM c`,
			`ROLLBACK TO sp2`,
			`%CTE%INSERT INTO s SELECT x+200 FROM c`,
			`RELEASE sp2`,
			`ROLLBACK TO sp1`,
			`%CTE%INSERT INTO s SELECT x+300 FROM c`,
			`RELEASE sp1`,
		), []string{`SELECT rowid, a FROM s ORDER BY rowid`}},

		{"conflict rollback in a transaction", spillScript("50",
			`CREATE TABLE r(a)`,
			`INSERT INTO r VALUES('before')`,
			`BEGIN`,
			`%CTE%INSERT INTO r SELECT x FROM c`,
			`INSERT OR ROLLBACK INTO r(rowid, a) VALUES(3, 'clash')`,
			`COMMIT`,
		), []string{`SELECT rowid, a FROM r ORDER BY rowid`}},

		{"later statements over a spilled table", spillScript("120",
			`CREATE TABLE q(a, b)`,
			`%CTE%INSERT INTO q SELECT x, 'v' || x FROM c`,
			`CREATE INDEX qa ON q(a)`,
			`UPDATE q SET b = b || '!' WHERE a % 10 = 0`,
			`DELETE FROM q WHERE a % 3 = 0`,
			`%CTE%INSERT INTO q SELECT x+500, 'w' FROM c`,
			`CREATE UNIQUE INDEX qab ON q(a, b)`,
			`CREATE TABLE q2(a)`,
			`%CTE%INSERT INTO q2 SELECT x FROM c`,
			`CREATE UNIQUE INDEX q2a ON q2(a)`,
			`%CTE%INSERT INTO q2 SELECT x FROM c`,
		), []string{`SELECT rowid, a, b FROM q ORDER BY rowid`, `SELECT a, b FROM q WHERE a = 40`, `SELECT count(*), sum(a) FROM q2`}},

		// A UNIQUE index or an upsert probes every row the table holds, which
		// is why neither arms: a spilled duplicate would go unseen.
		{"unique probes", spillScript("60",
			`CREATE TABLE q3(a)`,
			`CREATE UNIQUE INDEX q3a ON q3(a)`,
			`%CTE%INSERT INTO q3 SELECT x/2 FROM c`,
			`%CTE%INSERT OR IGNORE INTO q3 SELECT x/2 FROM c`,
			`CREATE TABLE up(id INTEGER PRIMARY KEY, v)`,
			`%CTE%INSERT INTO up SELECT x/2, x FROM c WHERE true ON CONFLICT(id) DO UPDATE SET v = v + excluded.v`,
		), []string{`SELECT rowid, a FROM q3 ORDER BY rowid`, `SELECT id, v FROM up ORDER BY id`}},

		{"generated and strict columns", spillScript("60",
			`CREATE TABLE g(a INTEGER, b AS (a*2) STORED, c AS (a||'x'))`,
			`%CTE%INSERT INTO g(a) SELECT x FROM c`,
			`CREATE TABLE st(a INTEGER, b TEXT) STRICT`,
			`%CTE%INSERT INTO st SELECT x, 't' || x FROM c`,
			`%CTE%INSERT INTO st SELECT 'bad', x FROM c`,
		), []string{`SELECT rowid, a, b, c FROM g ORDER BY rowid`, `SELECT rowid, a, b FROM st ORDER BY rowid`}},
	}
	for _, tc := range cases {
		flLockstep(t, tc.name, tc.script, tc.verify...)
	}
}

// TestBulkAppendSurvivesReopen writes the loaded table to the file and reads
// it back from a fresh session, index included.
func TestBulkAppendSurvivesReopen(t *testing.T) {
	epath := filepath.Join(t.TempDir(), "e.sqlite")
	cpath := filepath.Join(t.TempDir(), "c.sqlite")
	edb, err := engine.Create(epath)
	if err != nil {
		t.Fatal(err)
	}
	cdb, err := sql.Open("sqlite3", cpath)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range spillScript("300",
		`CREATE TABLE t(a, b)`,
		`%CTE%INSERT INTO t SELECT x, 'b' || x FROM c`,
		`CREATE INDEX ta ON t(a)`,
	) {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Fatalf("%s: engine %v, cgo %v", s, eerr, cerr)
		}
	}
	if err := edb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, err := engine.Open(epath)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, q := range []string{`SELECT rowid, a, b FROM t ORDER BY rowid`, `SELECT a FROM t WHERE a = 250`, `PRAGMA integrity_check`} {
		ec, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Fatalf("%s: engine %v, cgo %v", q, eerr, cerr)
		}
		if ok, reason := queryResultsMatch(ec, engineRowsToStrings(ev), cc, cr, true); !ok {
			t.Errorf("%s DIVERGES: %s", q, reason)
		}
	}
}
