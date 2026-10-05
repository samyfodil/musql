// Differential tests of ADD COLUMN with REFERENCES and non-NULL default.
// The operation refuses only when the table has rows.
package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

// differAlter compares ALTER results, ignoring C SQLite's generated column names on zero-row results.
func differAlter(t *testing.T, name string, stmts []string) {
	t.Helper()
	strip := func(res []map[string]any) []byte {
		for i, r := range res {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmts[i])), "ALTER ") {
				delete(r, "cols")
			}
		}
		b, _ := json.Marshal(res)
		return b
	}
	oracle := strip(run(t, "cgo", stmts))
	got := strip(run(t, "musql", stmts))
	if string(got) != string(oracle) {
		t.Errorf("[%s] musql DIVERGES from C SQLite\n  sql:     %v\n  cgo:     %s\n  musql:  %s",
			name, stmts, oracle, got)
	}
}

func TestAddColumnReferencesDefaultEmptyTable(t *testing.T) {
	differAlter(t, "add column references default, empty table", []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE e(x)`,
		`ALTER TABLE e ADD COLUMN f REFERENCES p DEFAULT 'text'`,
		`SELECT sql FROM sqlite_master WHERE name='e'`,
		`PRAGMA table_info(e)`,
		`INSERT INTO e(x) VALUES(1)`,
		`SELECT * FROM e`,
		`INSERT INTO p VALUES('text')`,
		`INSERT INTO e(x) VALUES(2)`,
		`SELECT * FROM e ORDER BY x`,
	})
	differAlter(t, "add column references default, emptied by delete", []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE d(x)`,
		`INSERT INTO d VALUES(1)`,
		`DELETE FROM d`,
		`ALTER TABLE d ADD COLUMN f REFERENCES p DEFAULT 'text'`,
		`SELECT sql FROM sqlite_master WHERE name='d'`,
	})
}

func TestAddColumnReferencesDefaultNonEmptyTable(t *testing.T) {
	differAlter(t, "add column references default, non-empty table", []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE n(x)`,
		`INSERT INTO n VALUES(1)`,
		`ALTER TABLE n ADD COLUMN f REFERENCES p DEFAULT 'text'`,
		`SELECT sql FROM sqlite_master WHERE name='n'`,
		`SELECT * FROM n`,
		`ALTER TABLE n ADD COLUMN g REFERENCES p DEFAULT NULL`,
		`SELECT sql FROM sqlite_master WHERE name='n'`,
		`ALTER TABLE n ADD COLUMN h REFERENCES p`,
		`SELECT sql FROM sqlite_master WHERE name='n'`,
		`SELECT * FROM n`,
	})
	differAlter(t, "add column references default, foreign keys off", []string{
		`PRAGMA foreign_keys = OFF`,
		`CREATE TABLE p(a PRIMARY KEY)`,
		`CREATE TABLE n(x)`,
		`INSERT INTO n VALUES(1)`,
		`ALTER TABLE n ADD COLUMN f REFERENCES p DEFAULT 'text'`,
		`SELECT sql FROM sqlite_master WHERE name='n'`,
		`SELECT * FROM n`,
	})
}
