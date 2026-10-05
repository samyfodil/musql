// Gates pragma_module_list (engine/pragma_module_list.go): the eponymous
// "pragma_module_list" table-valued function. Verified directly against
// mattn/go-sqlite3 3.53.3 (both this project's oracle builds, plain and
// -tags sqlite_fts5), which is what pragmaModuleListNames/
// pragmaModuleListFts5Names hardcode.
package compat

import "testing"

func TestPragmaModuleListMatchesOracle(t *testing.T) {
	for _, q := range []string{
		`SELECT name FROM pragma_module_list ORDER BY name`,
		`SELECT * FROM pragma_module_list WHERE name='fts5'`,
		`SELECT * FROM pragma_module_list WHERE name='rtree'`,
		`SELECT count(*) FROM pragma_module_list`,
	} {
		differ(t, "pragma_module_list", []string{q})
	}
}
