package compat

import (
	"fmt"
	"testing"
)

// TestAnalyzeAndCatalogOrderMatchC verifies that ANALYZE output and schema
// selection orders match C SQLite's behavior.
func TestAnalyzeAndCatalogOrderMatchC(t *testing.T) {
	many := func(n int) []string {
		var out []string
		for i := 1; i <= n; i++ {
			out = append(out, fmt.Sprintf("CREATE TABLE tab%d(a)", i), fmt.Sprintf("INSERT INTO tab%d VALUES(%d)", i, i))
		}
		return append(out, "ANALYZE", "SELECT rowid, tbl, idx, stat FROM sqlite_stat1")
	}
	differ(t, "analyze table order, no buckets", many(3))
	differ(t, "analyze table order, bucketed", many(12))
	differ(t, "analyze table order, capped", many(40))
	differ(t, "analyze index order", []string{
		"CREATE TABLE t1(a UNIQUE, b UNIQUE)", "CREATE TABLE t2(x)",
		"CREATE INDEX i1 ON t1(b)", "CREATE INDEX i0 ON t1(a,b)", "CREATE UNIQUE INDEX ir ON t1(b)",
		"INSERT INTO t1 VALUES(1,2),(3,4)", "INSERT INTO t2 VALUES(1)",
		"ANALYZE", "SELECT rowid, * FROM sqlite_stat1"})
	differ(t, "analyze index order, REPLACE last", []string{
		"CREATE TABLE t(a PRIMARY KEY, b, c UNIQUE ON CONFLICT REPLACE, d UNIQUE) WITHOUT ROWID",
		"CREATE INDEX tb ON t(b)", "INSERT INTO t VALUES(1,2,3,4),(3,4,5,6)",
		"ANALYZE", "SELECT rowid, * FROM sqlite_stat1"})
	differ(t, "catalog creation order", []string{
		"CREATE TABLE a(x UNIQUE)", "CREATE TABLE b(y)", "CREATE INDEX bi ON b(y)", "CREATE TABLE c(z)",
		"SELECT group_concat(name) FROM sqlite_schema", "SELECT name FROM sqlite_schema LIMIT 2"})
	differ(t, "select star from the catalog", []string{
		"CREATE TABLE t(a PRIMARY KEY, b) WITHOUT ROWID", "CREATE INDEX tb ON t(b)",
		"SELECT * FROM sqlite_schema", "SELECT * FROM sqlite_master WHERE type = 'index'"})
}
