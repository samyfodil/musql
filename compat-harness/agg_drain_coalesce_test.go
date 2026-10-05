package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAggDrainCoalescedColumnAnswers pins answers for RIGHT/FULL JOIN COALESCED
// columns used as aggregate arguments on sorted GROUP BY drains. The unmatched
// rows (with NULL from one side) would expose if the wrong arm was chosen.
func TestAggDrainCoalescedColumnAnswers(t *testing.T) {
	const fx = `CREATE TABLE a(k,x); CREATE TABLE b(k,y);
		INSERT INTO a VALUES(1,10),(2,20),(NULL,90);
		INSERT INTO b VALUES(2,200),(3,300),(NULL,900);`
	// A third table, for a coalesce chain with more than one fallback owner.
	const fx3 = fx + `CREATE TABLE c(k,z); INSERT INTO c VALUES(3,3000),(4,4000);`
	for _, tc := range []struct{ name, sql string }{
		{"full sum", fx + `SELECT k, sum(k) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"right sum", fx + `SELECT k, sum(k) FROM a RIGHT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"full count and max expr", fx + `SELECT k, count(*), max(k+1) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"full group_concat", fx + `SELECT k, group_concat(k) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"full min max together", fx + `SELECT k, min(k), max(k), count(k) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"coalesced inside arithmetic", fx + `SELECT k, sum(k*2+1) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"coalesced inside CASE", fx + `SELECT k, sum(CASE WHEN k IS NULL THEN -1 ELSE k END) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"natural full join", fx + `SELECT k, sum(k) FROM a NATURAL FULL JOIN b GROUP BY k ORDER BY k`},
		{"aggregate FILTER on the coalesced column", fx + `SELECT k, count(*) FILTER (WHERE k > 1) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"HAVING on the coalesced column", fx + `SELECT k, sum(k) FROM a FULL JOIN b USING(k) GROUP BY k HAVING sum(k) > 1 ORDER BY k`},
		{"WHERE on the coalesced column", fx + `SELECT k, sum(k) FROM a FULL JOIN b USING(k) WHERE k IS NOT NULL GROUP BY k ORDER BY k`},
		{"grouped by something else", fx + `SELECT x, sum(k) FROM a FULL JOIN b USING(k) GROUP BY x ORDER BY x`},
		{"three-way chain", fx3 + `SELECT k, sum(k), count(*) FROM a FULL JOIN b USING(k) FULL JOIN c USING(k) GROUP BY k ORDER BY k`},
		{"three-way chain expr", fx3 + `SELECT k, max(k*10) FROM a FULL JOIN b USING(k) FULL JOIN c USING(k) GROUP BY k ORDER BY k`},
		{"coalesced and a plain column together", fx + `SELECT k, sum(k), sum(x), sum(y) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"DISTINCT over the coalesced column", fx + `SELECT k, count(DISTINCT k) FROM a FULL JOIN b USING(k) GROUP BY k ORDER BY k`},
		// CONTROLS: a LEFT JOIN's USING column is NOT coalesced (the left row is
		// always present), and neither is an inner join's. Neither may move.
		{"left join control", fx + `SELECT k, sum(k) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"inner join control", fx + `SELECT k, sum(k) FROM a JOIN b USING(k) GROUP BY k ORDER BY k`},
		// CONTROL: a QUALIFIED spelling names ONE arm, never the coalesce.
		{"qualified names one arm", fx + `SELECT a.k, sum(b.k) FROM a FULL JOIN b USING(k) GROUP BY a.k ORDER BY a.k`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stmts []string
			for _, s := range strings.Split(tc.sql, ";") {
				if s = strings.TrimSpace(s); s != "" {
					stmts = append(stmts, s)
				}
			}
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

// TestAggDrainUsingColumnAnswers pins answers for USING/NATURAL join columns
// (not coalesced) as aggregate arguments on sorted GROUP BY drains. The unmatched
// rows in LEFT JOIN expose if the wrong arm is read.
func TestAggDrainUsingColumnAnswers(t *testing.T) {
	const fx = `CREATE TABLE a(k,x); CREATE TABLE b(k,y);
		INSERT INTO a VALUES(1,10),(2,20),(NULL,90);
		INSERT INTO b VALUES(2,200),(3,300),(NULL,900);`
	for _, tc := range []struct{ name, sql string }{
		{"left sum", fx + `SELECT k, sum(k) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"left count and max", fx + `SELECT k, count(k), max(k), min(k) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"left over both sides", fx + `SELECT k, sum(x), sum(y), sum(x+y) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"left group_concat", fx + `SELECT k, group_concat(k) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"inner sum", fx + `SELECT k, sum(k) FROM a JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"natural inner", fx + `SELECT k, sum(k) FROM a NATURAL JOIN b GROUP BY k ORDER BY k`},
		{"natural left", fx + `SELECT k, sum(k) FROM a NATURAL LEFT JOIN b GROUP BY k ORDER BY k`},
		{"comma join with an explicit predicate", fx + `SELECT x, sum(y) FROM a, b WHERE a.k=b.k GROUP BY x ORDER BY x`},
		{"left grouped by the other side", fx + `SELECT y, sum(k) FROM a LEFT JOIN b USING(k) GROUP BY y ORDER BY y`},
		{"left with HAVING", fx + `SELECT k, sum(k) FROM a LEFT JOIN b USING(k) GROUP BY k HAVING count(*) > 0 ORDER BY k`},
		{"left DISTINCT", fx + `SELECT k, count(DISTINCT k), count(DISTINCT x) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"qualified both arms", fx + `SELECT a.k, sum(a.k), sum(b.k) FROM a LEFT JOIN b USING(k) GROUP BY a.k ORDER BY a.k`},
		{"rowid through the drain", fx + `SELECT k, sum(a.rowid) FROM a LEFT JOIN b USING(k) GROUP BY k ORDER BY k`},
		{"self join aliased", fx + `SELECT p.k, sum(q.x) FROM a p LEFT JOIN a q USING(k) GROUP BY p.k ORDER BY p.k`},
		{"three tables mixed joins", fx + `SELECT k, sum(k), count(*) FROM a LEFT JOIN b USING(k) LEFT JOIN a c2 USING(k) GROUP BY k ORDER BY k`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stmts []string
			for _, s := range strings.Split(tc.sql, ";") {
				if s = strings.TrimSpace(s); s != "" {
					stmts = append(stmts, s)
				}
			}
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
