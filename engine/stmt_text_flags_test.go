package engine

import "testing"

// The lock-free last entry must answer only for its own text: alternating
// texts, and a text equal in content but a different string, each get their
// own flags.
func TestStmtTextFlagsLastEntryIsPerText(t *testing.T) {
	exp := "EXPLAIN SELECT 1"
	plain := "DELETE FROM t RETURNING *"
	for i := 0; i < 3; i++ {
		for _, c := range []struct {
			sql          string
			explain, ret bool
		}{
			{exp, true, false},
			{"SELECT 1 FROM tt", false, false}, // the same length as exp
			{plain, false, true},
			{string([]byte(exp)), true, false},
			{"  explain query plan select 2", true, false},
			{"INSERT INTO t VALUES(1)", false, false},
		} {
			if got := IsExplainStatement(c.sql); got != c.explain {
				t.Errorf("IsExplainStatement(%q) = %v", c.sql, got)
			}
			if got := StatementHasReturning(c.sql); got != c.ret {
				t.Errorf("StatementHasReturning(%q) = %v", c.sql, got)
			}
		}
	}
}
