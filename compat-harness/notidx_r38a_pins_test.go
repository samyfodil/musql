package compat

// Tests NOT INDEXED aggregate shapes, pinned against the oracle.

import "testing"

const r38aPinSetup = `CREATE TABLE t1( x INTEGER, y VARCHAR(8) );
INSERT INTO t1 VALUES(1,'true');
INSERT INTO t1 VALUES(0,'false');
INSERT INTO t1 VALUES(NULL,'NULL');
CREATE INDEX t1i1 ON t1(x)`

func r38aPinStmts(q string) []string {
	return []string{
		"CREATE TABLE t1( x INTEGER, y VARCHAR(8) )",
		"INSERT INTO t1 VALUES(1,'true')",
		"INSERT INTO t1 VALUES(0,'false')",
		"INSERT INTO t1 VALUES(NULL,'NULL')",
		"CREATE INDEX t1i1 ON t1(x)",
		q,
	}
}

// TestR38ANotIndexedPins asserts the oracle's answer for NOT INDEXED aggregates.
func TestR38ANotIndexedPins(t *testing.T) {
	for _, q := range []string{
		`SELECT group_concat(DISTINCT x) FROM t1 NOT INDEXED`,
		`SELECT group_concat(x) FROM t1 NOT INDEXED`,
		`SELECT group_concat(x,':') FROM t1 NOT INDEXED`,
		`SELECT group_concat(DISTINCT y) FROM t1 NOT INDEXED`,
		`SELECT sum(DISTINCT x) FROM t1 NOT INDEXED`,
		`SELECT count(DISTINCT x) FROM t1 NOT INDEXED`,
		`SELECT avg(DISTINCT x) FROM t1 NOT INDEXED`,
		`SELECT total(DISTINCT x) FROM t1 NOT INDEXED`,
		`SELECT min(DISTINCT x), max(DISTINCT x) FROM t1 NOT INDEXED`,
		`SELECT json_group_array(DISTINCT y) FROM t1 NOT INDEXED`,
		`SELECT y, group_concat(DISTINCT x) FROM t1 NOT INDEXED GROUP BY y`,
		`SELECT group_concat(DISTINCT x) FROM t1 NOT INDEXED WHERE x IS NOT NULL`,
	} {
		t.Run(q, func(t *testing.T) {
			if !differ(t, "r38a-pin", r38aPinStmts(q)) {
				t.Errorf("R38A PIN: %s\n"+
					"  If musql ERRORED, a guard is over-declining a NOT INDEXED source again:\n"+
					"  whereLoopAddBtree never prices t1i1 for it, so the rowid scan is the only\n"+
					"  plan and wherePlanIndexesInert (engine/where_plan_gate.go) must say so.\n"+
					"  Setup: %s", q, r38aPinSetup)
			}
		})
	}
}
