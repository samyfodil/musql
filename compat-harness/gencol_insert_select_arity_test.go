// TestGencolInsertSelectArity verifies INSERT...SELECT arity excludes
// GENERATED and HIDDEN columns, matching VALUES form behavior.
package compat

import "testing"

func TestGencolInsertSelectArity(t *testing.T) {
	differ(t, "gencol_insert_select_arity", []string{
		`CREATE TABLE csv_import_table ("debit" TEXT, "credit" TEXT)`,
		`INSERT INTO csv_import_table VALUES ('', '250.00')`,
		`CREATE TABLE transactions (debit REAL, credit REAL, amount REAL GENERATED ALWAYS AS (ifnull(credit, 0.0) - ifnull(debit, 0.0))) STRICT`,
		`INSERT INTO transactions SELECT nullif(debit, '') AS debit, nullif(credit, '') AS credit FROM csv_import_table`,
		`SELECT * FROM transactions`,
	})
}
