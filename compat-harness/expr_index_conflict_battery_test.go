// This file is the differential battery for UNIQUE EXPRESSION and PARTIAL index
// conflict detection. Expression evaluation must be done through compilation, not
// interpretation, to avoid silently wrong answers.
package compat

import "testing"

func TestExprIndexConflictPartialUnique(t *testing.T) {
	differ(t, "unique partial: rows move in and out via UPDATE", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a) WHERE b > 0`,
		`INSERT INTO t VALUES(1, 1)`,  // in the index
		`INSERT INTO t VALUES(1, -1)`, // excluded: same a, but b <= 0
		`INSERT INTO t VALUES(2, 5)`,  // in
		`SELECT a, b FROM t ORDER BY a, b`,
		// Moving the excluded row INTO the predicate collides with the row
		// already there.
		`UPDATE t SET b = 9 WHERE a = 1 AND b = -1`,
		`SELECT a, b FROM t ORDER BY a, b`,
		// Moving the admitted row OUT frees the key...
		`UPDATE t SET b = -7 WHERE a = 1 AND b = 1`,
		`SELECT a, b FROM t ORDER BY a, b`,
		// ... and now the same insert succeeds.
		`INSERT INTO t VALUES(1, 3)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`PRAGMA integrity_check`,
	})

	differ(t, "unique partial: conflict clauses", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a) WHERE b > 0`,
		`INSERT INTO t VALUES(1, 1)`,
		`INSERT OR IGNORE INTO t VALUES(1, 2)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`INSERT OR REPLACE INTO t VALUES(1, 4)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`INSERT OR IGNORE INTO t VALUES(1, -3)`, // outside the predicate: never conflicts
		`SELECT a, b FROM t ORDER BY a, b`,
		`INSERT OR FAIL INTO t VALUES(1, 6)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`PRAGMA integrity_check`,
	})

	differ(t, "unique partial: upsert", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a) WHERE b > 0`,
		`INSERT INTO t VALUES(1, 1)`,
		`INSERT INTO t(a,b) VALUES(1, 2) ON CONFLICT DO NOTHING`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`INSERT INTO t(a,b) VALUES(1, 2) ON CONFLICT(a) WHERE b > 0 DO UPDATE SET b = b + 100`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`SELECT a FROM t WHERE b > 0 ORDER BY a`,
		`PRAGMA integrity_check`,
	})

	differ(t, "unique partial: DELETE frees the key", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a) WHERE b > 0`,
		`INSERT INTO t VALUES(1, 1)`,
		`INSERT INTO t VALUES(2, 2)`,
		`DELETE FROM t WHERE a = 1`,
		`INSERT INTO t VALUES(1, 8)`,
		`SELECT a, b FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})

	// A predicate over a column the statement does NOT touch, and one whose
	// value is NULL: NULL is not true, so the row is not in the index.
	differ(t, "unique partial: NULL predicate value", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a) WHERE b > 0`,
		`INSERT INTO t VALUES(1, NULL)`,
		`INSERT INTO t VALUES(1, NULL)`,
		`INSERT INTO t VALUES(1, 1)`,
		`INSERT INTO t VALUES(1, 2)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`UPDATE t SET b = NULL WHERE b = 1`,
		`INSERT INTO t VALUES(1, 3)`,
		`SELECT count(*) FROM t`,
		`PRAGMA integrity_check`,
	})
}

func TestExprIndexConflictExpressionKey(t *testing.T) {
	differ(t, "unique expression key: collisions and NULLs", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a + b)`,
		`INSERT INTO t VALUES(1, 2)`, // key 3
		`INSERT INTO t VALUES(2, 1)`, // key 3 as well -- collides
		`SELECT a, b FROM t ORDER BY a`,
		`INSERT INTO t VALUES(4, 5)`,    // key 9
		`INSERT INTO t VALUES(NULL, 1)`, // NULL key: never conflicts
		`INSERT INTO t VALUES(NULL, 2)`, // ... nor with the other NULL key
		`SELECT count(*) FROM t`,
		// An UPDATE that moves a row ONTO an occupied key, and one that moves
		// it off.
		`UPDATE t SET a = 6 WHERE a = 4`,
		`SELECT a, b FROM t ORDER BY a`,
		`UPDATE t SET b = 30 WHERE a = 4`,
		`SELECT a, b FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})

	differ(t, "unique expression key: conflict clauses and upsert", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a * 10 + b)`,
		`INSERT INTO t VALUES(1, 2)`,
		`INSERT OR IGNORE INTO t VALUES(1, 2)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`INSERT OR REPLACE INTO t VALUES(1, 2)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`INSERT INTO t(a,b) VALUES(1, 2) ON CONFLICT DO NOTHING`,
		`SELECT count(*) FROM t`,
		`DELETE FROM t WHERE a = 1`,
		`INSERT INTO t VALUES(1, 2)`,
		`SELECT a, b FROM t ORDER BY a, b`,
		`PRAGMA integrity_check`,
	})

	// Both halves at once: an expression KEY under a partial WHERE.
	differ(t, "unique expression key under a partial WHERE", []string{
		`CREATE TABLE t(a INT, b INT, c TEXT)`,
		`CREATE UNIQUE INDEX ux ON t(a + b) WHERE c IS NOT NULL`,
		`INSERT INTO t VALUES(1, 2, NULL)`,
		`INSERT INTO t VALUES(2, 1, NULL)`, // both excluded: c IS NULL
		`INSERT INTO t VALUES(3, 0, 'x')`,  // in, key 3
		`SELECT a, b, c FROM t ORDER BY a`,
		`INSERT INTO t VALUES(0, 3, 'y')`,  // in, key 3 -- collides
		`UPDATE t SET c = 'z' WHERE a = 1`, // moves an excluded row IN, key 3 -- collides
		`SELECT a, b, c FROM t ORDER BY a`,
		`UPDATE t SET c = NULL WHERE a = 3`, // moves the admitted row OUT
		`INSERT INTO t VALUES(0, 3, 'y')`,   // now free
		`SELECT a, b, c FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})
}

func TestExprIndexConflictNonUnique(t *testing.T) {
	// A non-UNIQUE partial/expression index never reaches the conflict pass,
	// but it does reach the SAME compiled expressions through
	// exprIndexEntriesForTable. Same row movements, so a disagreement between
	// the two callers shows up here.
	differ(t, "non-unique partial index over moving rows", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE INDEX ix ON t(a) WHERE b > 0`,
		`INSERT INTO t VALUES(1, 1)`,
		`INSERT INTO t VALUES(2, -1)`,
		`INSERT INTO t VALUES(3, 5)`,
		`INSERT INTO t VALUES(4, 0)`,
		`UPDATE t SET b = -9 WHERE a = 3`,
		`UPDATE t SET b = 7 WHERE a = 4`,
		`DELETE FROM t WHERE a = 1`,
		`SELECT a, b FROM t ORDER BY a`,
		`SELECT a FROM t WHERE b > 0 ORDER BY a`,
		`PRAGMA integrity_check`,
	})

	differ(t, "non-unique expression index over moving rows", []string{
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE INDEX ix ON t(a + b)`,
		`INSERT INTO t VALUES(1, 2)`,
		`INSERT INTO t VALUES(2, 1)`,
		`INSERT INTO t VALUES(NULL, 4)`,
		`UPDATE t SET a = 10 WHERE b = 1`,
		`DELETE FROM t WHERE b = 2`,
		`SELECT a, b FROM t ORDER BY a`,
		`SELECT a, b FROM t WHERE a + b = 11`,
		`PRAGMA integrity_check`,
	})
}
