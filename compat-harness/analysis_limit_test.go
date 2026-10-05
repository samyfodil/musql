package compat

import (
	"fmt"
	"testing"
)

// This file tests PRAGMA analysis_limit. The pragma controls what ANALYZE
// writes to sqlite_stat1, and limited scans use a skip-ahead algorithm.
func analysisLimitFixture() []string {
	return []string{
		`CREATE TABLE t(a,b,c)`,
		`WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<5000)` +
			` INSERT INTO t SELECT n%7, n%3, n FROM s`,
		`CREATE INDEX ia ON t(a)`,
		`CREATE INDEX iba ON t(b,a)`,
		`CREATE UNIQUE INDEX ic ON t(c)`,
		`CREATE TABLE small(x)`,
		`INSERT INTO small VALUES(1),(1),(2)`,
		`CREATE INDEX ix ON small(x)`,
		// A second distribution, so the skip-ahead is exercised from both
		// sides: u's leading column is UNIQUE (every row is its own group, so
		// the leading column changes immediately and the scan stops at the
		// limit without ever skipping), and w's is nearly constant (two
		// groups of 400, so a small limit skips repeatedly).
		`CREATE TABLE u(p,q)`,
		`WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<800)` +
			` INSERT INTO u SELECT n, n%5 FROM s`,
		`CREATE INDEX iu ON u(p,q)`,
		`CREATE TABLE w(g,h)`,
		`WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<800)` +
			` INSERT INTO w SELECT n/401, n%11 FROM s`,
		`CREATE INDEX iw ON w(g,h)`,
	}
}

func TestAnalysisLimitedAnalyze(t *testing.T) {
	for _, lim := range []string{
		"0", "1", "2", "3", "5", "10", "17", "50", "99", "100", "101",
		"199", "200", "399", "400", "401", "713", "714", "715", "716",
		"1000", "1428", "1429", "2000", "4999", "5000", "5001", "100000",
	} {
		lim := lim
		t.Run("limit="+lim, func(t *testing.T) {
			stmts := append(analysisLimitFixture(),
				"PRAGMA analysis_limit="+lim,
				"PRAGMA analysis_limit",
				"ANALYZE",
				`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
			)
			differ(t, "analysislimit/"+lim, stmts)
		})
	}
}

// TestAnalysisLimitValueSpellings tests that pragma values are parsed correctly.
func TestAnalysisLimitValueSpellings(t *testing.T) {
	stmts := []string{`PRAGMA analysis_limit`}
	for _, v := range []string{
		"100", "0", "-5", "-0", "bogus", "0x10", "0xFFFFFFFFFFFFFFFFFF",
		"' 12 '", "12abc", "1.5", "99999999999999999999", "+7",
		"2147483648", "4294967296", "on", "''", "007",
	} {
		stmts = append(stmts, fmt.Sprintf("PRAGMA analysis_limit=%s", v), "PRAGMA analysis_limit")
	}
	// A qualifier names no database for this one: it is connection state.
	stmts = append(stmts,
		`PRAGMA main.analysis_limit=33`, `PRAGMA analysis_limit`,
		`PRAGMA temp.analysis_limit`,
	)
	differ(t, "analysislimitvalue", stmts)
}

// The limit is CONNECTION state: it survives a transaction, and it keeps
// applying to every later ANALYZE until it is changed back.
func TestAnalysisLimitIsConnectionState(t *testing.T) {
	stmts := append(analysisLimitFixture(),
		`PRAGMA analysis_limit=10`,
		`BEGIN`,
		`PRAGMA analysis_limit`,
		`ANALYZE`,
		`COMMIT`,
		`PRAGMA analysis_limit`,
		`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
		`ANALYZE`,
		`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
		`PRAGMA analysis_limit=0`,
		`ANALYZE`,
		`SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx`,
	)
	differ(t, "analysislimitconn", stmts)
}
