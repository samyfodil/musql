package compat

import "testing"

// TestVtabHiddenColumnAffinity: a table-valued function's hidden columns are
// NUMERIC, as C declares them ("json HIDDEN", "arg HIDDEN"): the type is the
// word HIDDEN, which sqlite3AffinityType reads as NUMERIC before vtab.c strips
// it (#138).
func TestVtabHiddenColumnAffinity(t *testing.T) {
	base := []string{`CREATE TABLE t5(a)`, `CREATE TABLE "5"(b)`}
	for _, q := range []string{
		`SELECT count(*) FROM json_each('5') WHERE json = 5`,
		`SELECT json = 5, json = '5', typeof(json) FROM json_each('5')`,
		`SELECT count(*) FROM json_each('[1]', '$') WHERE root = '$'`,
		`SELECT count(*) FROM json_tree('7') WHERE json = 7.0`,
		`SELECT count(*) FROM pragma_table_info('t5') WHERE arg = 't5'`,
		`SELECT count(*) FROM pragma_table_info('5') WHERE arg = 5`,
		`SELECT arg = 5, typeof(arg) FROM pragma_table_info('5')`,
		`SELECT count(*) FROM pragma_table_info('t5') WHERE schema = 'main'`,
	} {
		differ(t, "vtab-hidden-affinity", append(append([]string{}, base...), q))
	}
}
