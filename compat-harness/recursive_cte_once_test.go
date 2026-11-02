package compat

import "testing"

// TestRecursiveCTEOncePerStatement: an uncorrelated recursive CTE read inside
// a correlated subquery is materialized once per statement, under OP_Once as
// in C, so every outer row sees the same rows; a trigger body, whose frame C
// gives fresh once flags, materializes it again at every firing (#74).
func TestRecursiveCTEOncePerStatement(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(n)",
		"INSERT INTO t VALUES (1), (2), (3), (4), (5)",
		"CREATE TABLE src(v)",
		"CREATE TABLE ev(v)",
		"CREATE TABLE log(n, s)",
		"CREATE TRIGGER tr AFTER INSERT ON ev BEGIN " +
			"INSERT INTO src VALUES (new.v); " +
			"INSERT INTO log SELECT new.v, (SELECT sum(x) FROM (WITH RECURSIVE r(x) AS (SELECT v FROM src UNION ALL SELECT x FROM r WHERE 0) SELECT x FROM r)); " +
			"END",
		"INSERT INTO ev VALUES (1), (2), (3)",
	}
	for _, q := range []string{
		"WITH RECURSIVE r(x) AS (SELECT random() UNION ALL SELECT x FROM r WHERE 0) " +
			"SELECT count(DISTINCT v), count(*) FROM (SELECT (SELECT x FROM r WHERE t.n = t.n) AS v FROM t)",
		"WITH RECURSIVE d(z, lp) AS (VALUES('1', 1) UNION ALL SELECT CAST(lp+1 AS TEXT), lp+1 FROM d WHERE lp < 9) " +
			"SELECT n, (SELECT group_concat(z) FROM d WHERE lp <= t.n), EXISTS (SELECT 1 FROM d WHERE lp = t.n * 2) FROM t",
		"SELECT n, s FROM log ORDER BY n",
		sudoku74,
	} {
		aggOrderCheck(t, aggOrderCase{q, append(append([]string(nil), setup...), q), true})
	}
}

// sudoku74 is the solver from #74: its recursive digits CTE is read by the
// recursive step and by a correlated NOT EXISTS for every candidate.
const sudoku74 = `WITH RECURSIVE
  input(sud) AS (VALUES('53..7....6..195....98....6.8...6...34..8.3..17...2...6.6....28....419..5....8..79')),
  digits(z, lp) AS (VALUES('1', 1) UNION ALL SELECT CAST(lp+1 AS TEXT), lp+1 FROM digits WHERE lp<9),
  x(s, ind) AS (
    SELECT sud, instr(sud, '.') FROM input
    UNION ALL
    SELECT substr(s, 1, ind-1) || z || substr(s, ind+1), instr(substr(s, 1, ind-1) || z || substr(s, ind+1), '.')
      FROM x, digits AS z
     WHERE ind>0 AND NOT EXISTS (
       SELECT 1 FROM digits AS lp
        WHERE z.z = substr(s, ((ind-1)/9)*9 + lp, 1)
           OR z.z = substr(s, ((ind-1)%9) + (lp-1)*9 + 1, 1)
           OR z.z = substr(s, (((ind-1)/3) % 3) * 3 + ((ind-1)/27) * 27 + lp + ((lp-1) / 3) * 6, 1)))
SELECT s FROM x WHERE ind=0`
