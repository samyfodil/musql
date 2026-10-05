package compat

import "testing"

// TestPureFuncContexts tests that non-deterministic date/time functions
// (those without arguments or with 'now') are rejected in CHECK constraints,
// generated columns, and indexes.
func TestPureFuncContexts(t *testing.T) {
	for _, tc := range [][]string{
		{`CREATE TABLE t(a, d AS (julianday('now') > 0))`, `INSERT INTO t VALUES(1)`, `SELECT count(*) FROM t`},
		{`CREATE TABLE t(a, d AS (date()))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (date('now')))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (datetime(a,'localtime')))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (datetime(a,'utc')))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (unixepoch('now')))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (strftime('%s','now')))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (timediff('now', a)))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (current_date))`},
		{`CREATE TABLE t(a, d AS (current_time))`},
		{`CREATE TABLE t(a, d AS (current_timestamp))`},
		{`CREATE TABLE t(a, CHECK(date() > '2000'))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, CHECK(julianday('now') > 0))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, CHECK(datetime(a,'utc') > '2000'))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, CHECK(current_date > '2000'))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(date())`},
		{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(julianday('now'))`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(a) WHERE date() > '2000'`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`, `CREATE INDEX i ON t(julianday('now'))`},
		// The same functions with a COLUMN argument stay legal everywhere.
		{`CREATE TABLE t(a, d AS (julianday(a)))`, `INSERT INTO t VALUES(1)`, `SELECT d FROM t`},
		{`CREATE TABLE t(a, d AS (date(a)))`, `INSERT INTO t VALUES(2451545)`, `SELECT d FROM t`},
		{`CREATE TABLE t(a, CHECK(julianday(a) > 0))`, `INSERT INTO t VALUES(2451545)`, `SELECT a FROM t`},
		{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(date(a))`, `INSERT INTO t VALUES(2451545)`, `SELECT a FROM t`},
		// ...and an ordinary statement, and a DEFAULT, are untouched.
		{`SELECT date('now') IS NOT NULL`},
		{`CREATE TABLE t(a, d DEFAULT CURRENT_TIMESTAMP)`, `INSERT INTO t(a) VALUES(1)`, `SELECT length(d) FROM t`},
		{`CREATE TABLE t(a, d DEFAULT (datetime('now')))`, `INSERT INTO t(a) VALUES(1)`, `SELECT length(d) FROM t`},
	} {
		tc := tc
		t.Run(tc[0]+"|"+tc[len(tc)-1], func(t *testing.T) { differ(t, "notpure/"+tc[0], tc) })
	}
}


// CURRENT_DATE, CURRENT_TIME and CURRENT_TIMESTAMP are the exception, and in
// only one direction. In C they are three REGISTERED FUNCTIONS of their own
// (currentTimeFunc, date.c), and currentTimeFunc does NOT call
// sqlite3NotPureFunc -- so CURRENT_DATE inside a CHECK constraint is legal
// where date() is not. The RESOLVER still refuses all four alike inside a
// generated column or an index, since none of them is SQLITE_FUNC_CONSTANT
// (resolve.c:1228, whose mask is NC_IdxExpr|NC_PartIdx|NC_GenCol and does not
// name NC_IsCheck).
//
// This engine lowers the three keywords to date()/time()/datetime(), so the
// distinction has to be carried explicitly -- FuncExpr.CurrentTimeKw.
func TestCurrentTimeKeywordContexts(t *testing.T) {
	for _, tc := range [][]string{
		{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(current_date)`},
		{`CREATE TABLE t(a)`, `CREATE INDEX i ON t(a) WHERE current_date > '2000'`, `INSERT INTO t VALUES(1)`},
		{`CREATE TABLE t(a, d AS (current_date))`},
		{`CREATE TABLE t(a, CHECK(current_date > '2000'))`, `INSERT INTO t VALUES(1)`, `SELECT a FROM t`},
		{`CREATE TABLE t(a, CHECK(current_timestamp > '2000'))`, `INSERT INTO t VALUES(1)`, `SELECT a FROM t`},
		{`CREATE TABLE t(a DEFAULT CURRENT_DATE, b)`, `INSERT INTO t(b) VALUES(1)`, `SELECT length(a) FROM t`},
		{`SELECT length(current_date), length(current_time), length(current_timestamp)`},
		{`SELECT current_date()`},
		{`SELECT "current_date"`},
		{`CREATE TABLE q("current_date" TEXT)`, `INSERT INTO q VALUES('cd')`,
			`SELECT length(current_date) FROM q`, `SELECT q.current_date FROM q`, `SELECT "current_date" FROM q`},
	} {
		tc := tc
		t.Run(tc[0]+"|"+tc[len(tc)-1], func(t *testing.T) { differ(t, "curkw/"+tc[0], tc) })
	}
}
