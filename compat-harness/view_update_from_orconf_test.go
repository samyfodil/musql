package compat

import (
	"encoding/json"
	"testing"
)

// TestViewUpdateFromOrConflictAnswers tests UPDATE with OR clauses on views.
// The OR clause affects trigger body behavior.
func TestViewUpdateFromOrConflictAnswers(t *testing.T) {
	fixture := func(extra ...string) []string {
		return append([]string{
			"CREATE TABLE dst(k INTEGER PRIMARY KEY, v)",
			"INSERT INTO dst VALUES(1,'one'),(2,'two')",
			"CREATE TABLE base(k, v)",
			"INSERT INTO base VALUES(1,'a'),(2,'b'),(3,'c')",
			"CREATE VIEW v1 AS SELECT k, v FROM base",
			"CREATE TABLE m(k, nv)",
			"INSERT INTO m VALUES(1,'X'),(2,'Y'),(3,'Z')",
			"CREATE TRIGGER v1u INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO dst(k,v) VALUES(new.k, new.v); END",
		}, extra...)
	}
	for _, tc := range []struct {
		name   string
		update string
	}{
		{"or ignore", "UPDATE OR IGNORE v1 SET v=m.nv FROM m WHERE m.k=v1.k"},
		{"or replace", "UPDATE OR REPLACE v1 SET v=m.nv FROM m WHERE m.k=v1.k"},
		{"or abort", "UPDATE OR ABORT v1 SET v=m.nv FROM m WHERE m.k=v1.k"},
		{"or fail", "UPDATE OR FAIL v1 SET v=m.nv FROM m WHERE m.k=v1.k"},
		{"or rollback", "UPDATE OR ROLLBACK v1 SET v=m.nv FROM m WHERE m.k=v1.k"},
		// Control case: no OR clause
		{"no or clause", "UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stmts := fixture(tc.update, "SELECT k,v FROM dst ORDER BY k, v", "SELECT count(*) FROM dst")
			m := run(t, "musql", stmts)
			c := run(t, "cgo", stmts)
			for i := range stmts {
				mb, _ := json.Marshal(m[i])
				cb, _ := json.Marshal(c[i])
				if string(mb) != string(cb) {
					t.Errorf("STMT %s\n  cgo:    %s\n  musql: %s", stmts[i], cb, mb)
				}
			}
		})
	}
}
