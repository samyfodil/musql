// This tests aggregate-containing subqueries correlated to sqlite_master,
// a previously declined gap now supported.
package compat

import "testing"

// TestDistinctMinedAggSubquerySchemaCatalog tests aggregate subqueries
// correlated to sqlite_master as the schema changes.
func TestDistinctMinedAggSubquerySchemaCatalog(t *testing.T) {
	differ(t, "distinct.test-6.1-6.2", []string{
		"CREATE TABLE jjj(x)",
		"SELECT (SELECT 'mmm' UNION SELECT DISTINCT max(name) ORDER BY 1) FROM sqlite_master",
		"CREATE TABLE nnn(x)",
		"SELECT (SELECT 'mmm' UNION SELECT DISTINCT max(name) ORDER BY 1) FROM sqlite_master",
	})
}

// schemaCatalogAggSubqueryClosed tests various aggregate subquery forms over schemas.
var schemaCatalogAggSubqueryClosed = []struct {
	name  string
	stmts []string
}{
	{
		"different-aggregate-function",
		[]string{
			"CREATE TABLE jjj(x)", "CREATE TABLE nnn(x)",
			"SELECT (SELECT 'mmm' UNION SELECT DISTINCT min(name) ORDER BY 1) FROM sqlite_master",
		},
	},
	{
		"without-union",
		[]string{
			"CREATE TABLE jjj(x)", "CREATE TABLE nnn(x)",
			"SELECT (SELECT max(name)) FROM sqlite_master",
		},
	},
	{
		"extra-compound-arm",
		[]string{
			"CREATE TABLE jjj(x)", "CREATE TABLE nnn(x)",
			"SELECT (SELECT 'mmm' UNION SELECT DISTINCT max(name) UNION SELECT 'zzz' ORDER BY 1) FROM sqlite_master",
		},
	},
	{
		"exists-instead-of-scalar",
		[]string{
			"CREATE TABLE jjj(x)", "CREATE TABLE nnn(x)",
			"SELECT EXISTS(SELECT 'mmm' UNION SELECT DISTINCT max(name) ORDER BY 1) FROM sqlite_master",
		},
	},
	{
		"correlated-to-temp-master",
		[]string{
			"CREATE TEMP TABLE ttt(x)",
			"SELECT (SELECT 'mmm' UNION SELECT DISTINCT max(name) ORDER BY 1) FROM sqlite_temp_master",
		},
	},
	{
		"three-objects-mixed-types",
		// Stresses the same hoist mechanism over a schema with more than two
		// catalog rows, mixing table/index/view so max(name) has a real tie
		// to break against sqlite_master's actual rowid order.
		[]string{
			"CREATE TABLE aaa(x)", "CREATE TABLE zzz(x)",
			"CREATE INDEX zzz_idx ON zzz(x)",
			"CREATE VIEW mmm_view AS SELECT x FROM aaa",
			"SELECT (SELECT 'nnn' UNION SELECT DISTINCT max(name) ORDER BY 1) FROM sqlite_master",
		},
	},
}

func TestSchemaCatalogAggSubqueryVariantsAnswerCorrectly(t *testing.T) {
	for _, c := range schemaCatalogAggSubqueryClosed {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
