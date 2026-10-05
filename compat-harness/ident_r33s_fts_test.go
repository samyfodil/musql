// This file tests identifier folding in fts3/fts4/fts5 virtual tables.
// Column names should fold like ordinary identifiers, not by text tokenization rules.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestR33SIdentFoldFtsColumns tests reading identifier pairs from fts3/fts4/fts5 columns.
func TestR33SIdentFoldFtsColumns(t *testing.T) {
	for i, p := range r33sPairs {
		qa, qb := r33sQuote(p.a), r33sQuote(p.b)
		for _, mod := range []string{"fts3", "fts4", "fts5"} {
			differ(t, fmt.Sprintf("r33s %s pair#%d(%s)", mod, i, p.why), []string{
				fmt.Sprintf(`CREATE VIRTUAL TABLE ft USING %s(%s, %s)`, mod, qa, qb),
				`INSERT INTO ft VALUES('alpha','beta')`,
				fmt.Sprintf(`SELECT %s, %s FROM ft`, qa, qb),
				fmt.Sprintf(`SELECT %s FROM ft`, qb),
				`PRAGMA table_info(ft)`,
				`SELECT * FROM ft`,
			})
		}
	}
}
