// Gates the pragma_function_list table-valued function, including unfiltered
// scan row order matching the oracle.
package compat

import "testing"

func TestPragmaFunctionListMatchesOracle(t *testing.T) {
	for _, q := range []string{
		// Basic filtering and type checks.
		`SELECT DISTINCT name, builtin FROM pragma_function_list WHERE name='upper' AND builtin`,
		`SELECT DISTINCT name, builtin FROM pragma_function_list WHERE name LIKE 'exter%'`,
		// Representative rows: scalars, aggregates, window functions.
		`SELECT name, builtin, type, enc, narg, flags FROM pragma_function_list WHERE name='upper'`,
		`SELECT name, builtin, type, enc, narg, flags FROM pragma_function_list WHERE name='sum' AND builtin=1`,
		`SELECT name, builtin, type, enc, narg, flags FROM pragma_function_list WHERE name='match'`,
		`SELECT name, type FROM pragma_function_list WHERE name='row_number'`,
		`SELECT name FROM pragma_function_list WHERE name='json_extract'`,
		// Double-registered functions with multiple arities.
		`SELECT type, narg FROM pragma_function_list WHERE name='max' ORDER BY narg`,
		// Absent function name.
		`SELECT count(*) FROM pragma_function_list WHERE name='no_such_function_zzz'`,
		// Pattern matching queries.
		`SELECT DISTINCT name FROM pragma_function_list WHERE name LIKE 'json_g%' ORDER BY name`,
		`SELECT DISTINCT name FROM pragma_function_list WHERE name LIKE 'json%' ORDER BY name`,
		// Unfiltered scans: row order tested.
		`SELECT * FROM pragma_function_list`,
		`SELECT rowid, name, narg FROM pragma_function_list`,
		`SELECT count(*), count(DISTINCT name) FROM pragma_function_list`,
		`SELECT name, builtin, flags FROM pragma_function_list WHERE builtin=0`,
	} {
		differ(t, "pragma_function_list", []string{q})
	}
}

// TestPragmaTableInfoOfEponymousVtab gates pragma_table_info on eponymous
// virtual tables accessed without explicit CREATE.
func TestPragmaTableInfoOfEponymousVtab(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM pragma_table_info('pragma_function_list')`,
		`SELECT name, type FROM pragma_table_info('pragma_function_list')`,
		`SELECT * FROM pragma_table_info('pragma_module_list')`,
		`SELECT * FROM pragma_table_info('json_each')`,
		// Non-eponymous module: zero rows.
		`SELECT * FROM pragma_table_info('rtree')`,
	} {
		differ(t, "pragma_table_info_eponymous", []string{q})
	}
}
