package compat

// A WHERE term with no reference to the FROM clause is coded once, before the
// loops open (sqlite3WhereBegin, where.c, "constant" terms with
// sqlite3ExprIfFalse to iBreak). So an error it raises is raised even when the
// table is empty, and a false one skips the scan. These compare the shapes
// against C SQLite.

import (
	"fmt"
	"testing"
)

var constWhereSetup = []string{
	`CREATE TABLE e(k)`,
	`CREATE TABLE t(k)`,
	`INSERT INTO t VALUES(1),(2),(NULL)`,
	`CREATE TABLE u(k, v)`,
	`INSERT INTO u VALUES(1,10),(2,20),(3,30)`,
}

func TestConstWhereIssueShapes(t *testing.T) {
	differ(t, "overflow in a constant WHERE term over an empty table", []string{
		`CREATE TABLE e(k)`,
		`SELECT k FROM e WHERE abs(-9223372036854775807-1)>0`,
		`SELECT EXISTS (SELECT 1 FROM e WHERE abs(-9223372036854775807-1)>0)`,
		`SELECT count(*) FROM e WHERE abs(-9223372036854775807-1)>0`,
		`SELECT k FROM e WHERE 0 AND abs(-9223372036854775807-1)>0`,
		`SELECT k FROM e WHERE abs(-9223372036854775807-1)>0 AND 0`,
	})
}

// constWhereKnownOpen are shapes whose divergence is not the constant term
// itself, left for later work. Both are an EXISTS that existsToJoin turns into
// a join, run here as a subquery per enclosing row:
//
//   - the first has a sub-select in its WHERE, which the planner port cannot
//     follow (existsJoinSource), so the EXISTS table's empty-table exit
//     (addrHalt) that ends C's join after the first row is not known;
//   - in the second C puts the EXISTS table outermost and codes the term in
//     its loop, which this port cannot reach when the enclosing table is empty.
var constWhereKnownOpen = map[string]bool{
	`SELECT k FROM t WHERE EXISTS (SELECT 1 FROM e x WHERE x.k = t.k AND CASE WHEN t.k = 2 THEN (SELECT abs(-9223372036854775807-1)) > 0 ELSE 1 END)`: true,
	`SELECT k FROM e o WHERE EXISTS (SELECT 1 FROM t WHERE (SELECT abs(-9223372036854775807-1)) > 0)`:                                                 true,
}

// TestConstWhereCrossProduct is the sweep: constant terms (error, true, false,
// NULL, mixed) in every statement shape over an empty and a populated table.
func TestConstWhereCrossProduct(t *testing.T) {
	ovf := `abs(-9223372036854775807-1)`
	consts := []string{
		`1`, `0`, `NULL`, `1=1`, `1=2`,
		ovf + `>0`, `NOT ` + ovf + `>0`,
		`0 AND ` + ovf + `>0`, ovf + `>0 AND 0`, `1 OR ` + ovf + `>0`, ovf + `>0 OR 1`,
		`CASE WHEN 1 THEN ` + ovf + `>0 ELSE 1 END`,
		`coalesce(NULL, 1)`, `abs(5) = 5`,
		`(SELECT ` + ovf + `) > 0`, `EXISTS(SELECT ` + ovf + `)`,
		`'a' LIKE 'a%'`, `1 IN (1,2)`, `NULL IN (1,2)`, `5 BETWEEN 1 AND ` + ovf,
		`CAST('x' AS INTEGER) = 0`, `zeroblob(1) IS NOT NULL`,
		`5 IN (1, ` + ovf + `)`, ovf + ` IS NULL`, `typeof(` + ovf + `) = 'integer'`,
		ovf + ` BETWEEN 0 AND 1`, `likely(` + ovf + `>0)`, `iif(1, ` + ovf + `>0, 0)`,
		`NOT NOT (` + ovf + `>0)`, ovf + `>0 IS TRUE`, `'x' = 'X' COLLATE NOCASE`,
		`TRUE`, `FALSE`, `2147483648`, `0.0`, `'1'`, `-1`,
	}
	shapes := []string{
		`SELECT k FROM %[1]s WHERE %[2]s`,
		`SELECT k FROM %[1]s WHERE k IS NULL AND %[2]s`,
		`SELECT k FROM %[1]s WHERE %[2]s AND k IS NULL`,
		`SELECT k FROM %[1]s WHERE k > 0 OR %[2]s`,
		`SELECT k FROM %[1]s WHERE k > 0 AND (%[2]s) AND k < 9`,
		`SELECT count(*), sum(k) FROM %[1]s WHERE %[2]s`,
		`SELECT k, count(*) FROM %[1]s WHERE %[2]s GROUP BY k`,
		`SELECT count(*) FROM %[1]s WHERE %[2]s HAVING count(*) >= 0`,
		`SELECT DISTINCT k FROM %[1]s WHERE %[2]s`,
		`SELECT k FROM %[1]s WHERE %[2]s ORDER BY k LIMIT 1`,
		`SELECT k, row_number() OVER () FROM %[1]s WHERE %[2]s`,
		`SELECT EXISTS (SELECT 1 FROM %[1]s WHERE %[2]s)`,
		`SELECT (SELECT k FROM %[1]s WHERE %[2]s)`,
		`SELECT k FROM t WHERE k IN (SELECT k FROM %[1]s WHERE %[2]s)`,
		`SELECT k FROM t WHERE EXISTS (SELECT 1 FROM %[1]s x WHERE x.k = t.k AND %[2]s)`,
		`SELECT k FROM %[1]s WHERE %[2]s UNION ALL SELECT k FROM t`,
		`WITH c AS (SELECT k FROM %[1]s WHERE %[2]s) SELECT k FROM c`,
		`SELECT * FROM (SELECT k FROM %[1]s WHERE %[2]s)`,
		`SELECT a.k, b.k FROM %[1]s a, t b WHERE %[2]s`,
		`SELECT a.k, b.k FROM %[1]s a JOIN t b ON a.k = b.k WHERE %[2]s`,
		`SELECT a.k, b.k FROM %[1]s a LEFT JOIN t b ON a.k = b.k WHERE %[2]s`,
		`SELECT a.k, b.k FROM t a LEFT JOIN %[1]s b ON a.k = b.k WHERE %[2]s`,
		`SELECT a.k, b.k FROM %[1]s a LEFT JOIN t b ON a.k = b.k AND %[2]s`,
		`SELECT a.k, b.k FROM %[1]s a JOIN t b ON %[2]s`,
		`SELECT a.k, b.k FROM %[1]s a RIGHT JOIN t b ON a.k = b.k WHERE %[2]s`,
		`SELECT a.k, b.k FROM %[1]s a FULL JOIN t b ON a.k = b.k WHERE %[2]s`,
		`SELECT a.k, b.k FROM t a RIGHT JOIN %[1]s b ON a.k = b.k AND %[2]s`,
		`SELECT a.k, c.v FROM %[1]s a JOIN u c USING(k) WHERE %[2]s`,
		`SELECT k FROM %[1]s NATURAL JOIN u WHERE %[2]s`,
		`SELECT k FROM %[1]s WHERE rowid > 0 AND %[2]s`,
		`UPDATE %[1]s SET k = k WHERE %[2]s`,
		`DELETE FROM %[1]s WHERE %[2]s`,
		`INSERT INTO e SELECT k FROM %[1]s WHERE %[2]s`,
		`UPDATE u SET v = v + 1 WHERE k IN (SELECT k FROM %[1]s WHERE %[2]s)`,
		`UPDATE u SET v = 0 FROM %[1]s x WHERE u.k = x.k AND %[2]s`,
		`SELECT k FROM %[1]s WHERE %[2]s LIMIT 0`,
		`SELECT k FROM %[1]s WHERE %[2]s ORDER BY k LIMIT 0`,
		`SELECT count(*) FROM %[1]s WHERE %[2]s LIMIT 0`,
		`SELECT k, count(*) FROM %[1]s WHERE %[2]s GROUP BY k LIMIT 0`,
		`SELECT DISTINCT k FROM %[1]s WHERE %[2]s LIMIT 0`,
		`SELECT a.k FROM %[1]s a, u WHERE %[2]s LIMIT 0`,
		`SELECT k, row_number() OVER () FROM %[1]s WHERE %[2]s LIMIT 0`,
		`SELECT k FROM %[1]s WHERE %[2]s LIMIT 1 OFFSET 1`,
		`SELECT a.k FROM %[1]s a LEFT JOIN u ON a.k = u.k WHERE %[2]s`,
		`SELECT a.k FROM %[1]s a, u WHERE %[2]s`,
		`SELECT max(k) FROM %[1]s WHERE %[2]s`,
		`SELECT k FROM %[1]s WHERE %[2]s GROUP BY k HAVING 1`,
		`SELECT k FROM t o WHERE EXISTS (SELECT 1 FROM %[1]s WHERE %[2]s)`,
		`SELECT k, (SELECT count(*) FROM %[1]s WHERE %[2]s) FROM t`,
		`CREATE TABLE made AS SELECT k FROM %[1]s WHERE %[2]s`,
		`CREATE VIEW vv AS SELECT k FROM %[1]s WHERE %[2]s`,
		`INSERT INTO e SELECT k FROM %[1]s WHERE %[2]s UNION SELECT k FROM u WHERE %[2]s`,
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3 AND (%[2]s)) SELECT n, (SELECT count(*) FROM %[1]s) FROM r`,
		`DELETE FROM u WHERE %[2]s AND EXISTS (SELECT 1 FROM %[1]s)`,
		// existsToJoin: a sub-select term runs in the enclosing table's loop,
		// an outer-only one at its level, and a constant one before every loop.
		`SELECT k FROM t WHERE EXISTS (SELECT 1 FROM %[1]s x WHERE x.k = t.k AND (SELECT %[2]s))`,
		`SELECT k FROM t WHERE EXISTS (SELECT 1 FROM %[1]s x WHERE x.k = t.k AND CASE WHEN t.k = 2 THEN %[2]s ELSE 1 END)`,
		`SELECT k FROM t WHERE EXISTS (SELECT 1 FROM %[1]s x WHERE (SELECT %[2]s))`,
		`SELECT k FROM e o WHERE EXISTS (SELECT 1 FROM %[1]s WHERE %[2]s)`,
		`SELECT k FROM e o WHERE EXISTS (SELECT 1 FROM %[1]s x WHERE x.k = o.k AND %[2]s)`,
		// UPDATE ... FROM's first pass is a SELECT with the target first.
		`UPDATE u SET v = 0 FROM %[1]s x WHERE (SELECT %[2]s)`,
		`UPDATE u SET v = 0 FROM t y, %[1]s x WHERE u.k = x.k AND (SELECT %[2]s)`,
		`UPDATE u SET v = 0 FROM %[1]s x, t y WHERE u.k = x.k AND (SELECT %[2]s)`,
		// An EXISTS evaluates no result list, aggregate, grouped, windowed or
		// in a UNION ALL arm, and stops at its first row.
		`SELECT EXISTS (SELECT count(*), %[2]s FROM %[1]s)`,
		`SELECT EXISTS (SELECT k, %[2]s FROM %[1]s GROUP BY k)`,
		`SELECT EXISTS (SELECT k, row_number() OVER (), %[2]s FROM %[1]s)`,
		`SELECT EXISTS (SELECT %[2]s FROM %[1]s UNION ALL SELECT 1)`,
		`SELECT EXISTS (SELECT 1 FROM %[1]s UNION ALL SELECT %[2]s)`,
		`SELECT EXISTS (SELECT 1 FROM %[1]s UNION SELECT %[2]s)`,
		`SELECT EXISTS (SELECT k FROM %[1]s UNION ALL SELECT %[2]s ORDER BY 1)`,
		// A compound's LIMIT is counted across its UNION ALL arms, and 0 runs none.
		`SELECT %[2]s FROM %[1]s UNION ALL SELECT 1 LIMIT 0`,
		`SELECT k FROM %[1]s UNION SELECT k FROM t WHERE %[2]s LIMIT 0`,
		`SELECT 1 UNION ALL SELECT %[2]s FROM %[1]s LIMIT 1`,
		`SELECT k FROM %[1]s UNION ALL SELECT %[2]s LIMIT 1`,
		`SELECT * FROM (SELECT DISTINCT CASE WHEN k = 2 THEN %[2]s END FROM %[1]s) LIMIT 1`,
		`SELECT k FROM %[1]s UNION SELECT 7 UNION ALL SELECT %[2]s LIMIT 2`,
		`SELECT * FROM (SELECT k FROM %[1]s UNION ALL SELECT %[2]s LIMIT 1)`,
		`SELECT k FROM %[1]s LIMIT 0 OFFSET (%[2]s)`,
		// A materialization runs when its loop is reached; an empty inner
		// table ends the join.
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3 AND (%[2]s)) SELECT * FROM r, %[1]s`,
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3 AND (%[2]s)) SELECT * FROM %[1]s, r`,
		`SELECT * FROM %[1]s a, (SELECT DISTINCT k FROM t WHERE %[2]s)`,
		`WITH c AS MATERIALIZED (SELECT k FROM t WHERE %[2]s) SELECT * FROM %[1]s a, c`,
		`SELECT * FROM %[1]s a, json_each('[1,2]') WHERE %[2]s`,
		`SELECT * FROM t, %[1]s x WHERE CASE WHEN t.k = 2 THEN %[2]s ELSE 1 END ORDER BY 1, 2`,
		`SELECT * FROM t, u, %[1]s x WHERE CASE WHEN t.k = 2 THEN %[2]s ELSE 1 END ORDER BY 1, 2, 3, 4`,
		`SELECT * FROM t LEFT JOIN u ON 1, %[1]s x WHERE CASE WHEN t.k = 2 THEN %[2]s ELSE 1 END`,
		`SELECT count(*) FROM t, %[1]s x, u WHERE CASE WHEN u.k = 2 THEN %[2]s ELSE 1 END`,
	}
	n, total := 0, 0
	for _, tab := range []string{"e", "t"} {
		for _, x := range consts {
			for _, sh := range shapes {
				q := fmt.Sprintf(sh, tab, x)
				if constWhereKnownOpen[q] {
					continue
				}
				total++
				stmts := append(append([]string{}, constWhereSetup...), q, `SELECT k FROM e ORDER BY k`, `SELECT k FROM t ORDER BY k`, `SELECT k, v FROM u ORDER BY k`)
				if !differ(t, q, stmts) {
					n++
					if n >= 400 {
						t.Fatalf("stopping after %d divergences of %d cases", n, total)
					}
				}
			}
		}
	}
	t.Logf("%d cases, %d diverged", total, n)
}

func TestConstWhereOuterReferences(t *testing.T) {
	ovf := `abs(-9223372036854775807-1 + o.k*0)`
	differ(t, "constant relative to a correlated subquery", append(append([]string{}, constWhereSetup...),
		// The term names only the enclosing query: it is coded once per run of
		// the subquery, before its loop, so it errors against an empty table.
		`SELECT k FROM t o WHERE EXISTS (SELECT 1 FROM e WHERE `+ovf+`>0)`,
		`SELECT k FROM t o WHERE (SELECT count(*) FROM e WHERE `+ovf+`>0) = 0`,
		`SELECT k FROM t o WHERE o.k = 1 AND EXISTS (SELECT 1 FROM e WHERE `+ovf+`>0)`,
		`SELECT k FROM t o WHERE EXISTS (SELECT 1 FROM t i WHERE i.k = o.k AND o.k > 1)`,
		`SELECT k FROM t o WHERE EXISTS (SELECT 1 FROM t i WHERE o.k > 1)`,
		`SELECT k FROM t o WHERE NOT EXISTS (SELECT 1 FROM e WHERE o.k IS NULL)`,
		`SELECT k, (SELECT count(*) FROM u WHERE o.k = 1) FROM t o`,
		`SELECT k, (SELECT max(v) FROM u WHERE u.k = o.k AND o.k > 1) FROM t o`,
		`SELECT k FROM t o WHERE (SELECT k FROM u WHERE o.k IS NULL LIMIT 1) IS NULL`,
	))
}

func TestConstWhereNonDeterministic(t *testing.T) {
	// random() stays per row; a constant deterministic term still goes first.
	differ(t, "non-deterministic terms stay per row", append(append([]string{}, constWhereSetup...),
		`SELECT count(*) FROM t WHERE random() IS NOT NULL`,
		`SELECT count(*) FROM e WHERE random() IS NOT NULL AND `+`abs(-9223372036854775807-1)>0`,
		`SELECT count(*) FROM e WHERE abs(-9223372036854775807-1)>0 AND random() IS NOT NULL`,
		`SELECT count(*) FROM t WHERE (SELECT random()) IS NOT NULL`,
		`SELECT count(*) FROM e WHERE abs(-9223372036854775807-1) > 0 OR random() IS NOT NULL`,
	))
}

func TestConstWhereTriggersAndViews(t *testing.T) {
	differ(t, "constant terms inside triggers and views", append(append([]string{}, constWhereSetup...),
		`CREATE VIEW ve AS SELECT k FROM e WHERE abs(-9223372036854775807-1)>0`,
		`CREATE VIEW vt AS SELECT k FROM t WHERE abs(-9223372036854775807-1)>0`,
		`SELECT * FROM ve`,
		`SELECT * FROM vt`,
		`SELECT count(*) FROM ve`,
		`SELECT * FROM t WHERE k IN (SELECT k FROM ve)`,
		`CREATE TRIGGER tr AFTER INSERT ON u BEGIN
		   UPDATE e SET k = 1 WHERE abs(-9223372036854775807-1)>0;
		 END`,
		`INSERT INTO u VALUES(7,70)`,
		`SELECT count(*) FROM u`,
		`DROP TRIGGER tr`,
		`CREATE TRIGGER tr2 AFTER INSERT ON u BEGIN
		   INSERT INTO e SELECT k FROM t WHERE abs(-9223372036854775807-1)>0;
		 END`,
		`INSERT INTO u VALUES(8,80)`,
		`SELECT count(*) FROM u`,
		`SELECT count(*) FROM e`,
		`DROP TRIGGER tr2`,
		`CREATE TRIGGER tr3 AFTER INSERT ON u BEGIN
		   DELETE FROM e WHERE new.k = 9 AND abs(-9223372036854775807-1)>0;
		   DELETE FROM e WHERE new.k > 0 AND 0;
		 END`,
		`INSERT INTO u VALUES(9,90)`,
		`SELECT count(*) FROM u`,
	))
}

func TestConstWhereParameters(t *testing.T) {
	c, m := boundPair(t, constWhereSetup)
	for _, q := range []string{
		`SELECT k FROM e WHERE ?`,
		`SELECT k FROM t WHERE ?`,
		`SELECT k FROM e WHERE abs(?) > 0`,
		`SELECT k FROM t WHERE abs(?) > 0`,
	} {
		for _, arg := range []any{int64(-9223372036854775807 - 1), int64(0), int64(1), nil, "x"} {
			cv, mv := boundRows(c, q, arg), boundRows(m, q, arg)
			if cv != mv {
				t.Errorf("%s with %v\n  cgo: %s\n  mus: %s", q, arg, cv, mv)
			}
		}
	}
}

// An EXISTS only asks whether a row arrives, so selectInnerLoop never evaluates
// its result list (select.c, the eDest!=SRT_Exists arm) and sqlite3Select drops
// its DISTINCT and ORDER BY first. Names in the list are still resolved. The
// same holds for an aggregate or windowed list, whose aggregates still step,
// and for each UNION ALL arm multiSelect codes in turn under the EXISTS's
// LIMIT 1; a merged arm (UNION, INTERSECT, EXCEPT) is a co-routine and is
// evaluated.
func TestConstWhereExistsResultList(t *testing.T) {
	ovf := `abs(-9223372036854775807-1)`
	differ(t, "EXISTS does not evaluate its result list", append(append([]string{}, constWhereSetup...),
		`SELECT EXISTS(SELECT `+ovf+`)`,
		`SELECT EXISTS(SELECT `+ovf+` FROM t)`,
		`SELECT EXISTS(SELECT DISTINCT `+ovf+` FROM t)`,
		`SELECT EXISTS(SELECT `+ovf+` FROM t ORDER BY 1)`,
		`SELECT EXISTS(SELECT `+ovf+` FROM t LIMIT 1)`,
		`SELECT NOT EXISTS(SELECT `+ovf+` FROM t)`,
		`SELECT k FROM t WHERE EXISTS(SELECT `+ovf+`)`,
		`SELECT k FROM t WHERE NOT EXISTS(SELECT `+ovf+` FROM e)`,
		`SELECT EXISTS(SELECT nosuchcol FROM t)`,
		`SELECT EXISTS(SELECT 1 FROM t ORDER BY nosuchcol)`,
		`SELECT EXISTS(SELECT * FROM t, u)`,
		`SELECT EXISTS(SELECT (SELECT `+ovf+`) FROM t)`,
		`SELECT EXISTS(SELECT count(*), `+ovf+` FROM t)`,
		`SELECT EXISTS(SELECT sum(`+ovf+`) FROM t)`,
		`SELECT EXISTS(SELECT k, `+ovf+` FROM t GROUP BY k)`,
		`SELECT EXISTS(SELECT k FROM t GROUP BY k HAVING `+ovf+` > 0)`,
		`SELECT EXISTS(SELECT k, row_number() OVER (), `+ovf+` FROM t)`,
		`SELECT k FROM t WHERE EXISTS(SELECT count(*), `+ovf+` FROM u WHERE u.k = t.k)`,
		`SELECT EXISTS(SELECT `+ovf+` FROM t UNION ALL SELECT 1)`,
		`SELECT EXISTS(SELECT 1 UNION ALL SELECT `+ovf+` FROM t)`,
		`SELECT EXISTS(SELECT 1 UNION SELECT `+ovf+` FROM t)`,
		`SELECT EXISTS(SELECT `+ovf+` FROM t UNION ALL SELECT 1 ORDER BY 1)`,
		`SELECT EXISTS(SELECT 1 FROM e UNION ALL SELECT `+ovf+` FROM t LIMIT 5)`,
	))
}

// A literal LIMIT 0 leaves the select before any WHERE term, aggregate or
// sorter runs (computeLimitRegisters).
func TestConstWhereLimitZero(t *testing.T) {
	ovf := `abs(-9223372036854775807-1)`
	differ(t, "LIMIT 0 skips the select", append(append([]string{}, constWhereSetup...),
		`SELECT k FROM t WHERE `+ovf+`>0 LIMIT 0`,
		`SELECT k FROM t WHERE `+ovf+`>0 ORDER BY k LIMIT 0`,
		`SELECT count(*) FROM t WHERE `+ovf+`>0 LIMIT 0`,
		`SELECT sum(`+ovf+`) FROM t LIMIT 0`,
		`SELECT `+ovf+` FROM t LIMIT 0`,
		`SELECT `+ovf+` LIMIT 0`,
		`SELECT k, count(*) FROM t GROUP BY k HAVING `+ovf+`>0 LIMIT 0`,
		`SELECT k, row_number() OVER (ORDER BY `+ovf+`) FROM t LIMIT 0`,
		`SELECT k FROM t WHERE `+ovf+`>0 LIMIT 0 OFFSET 1`,
		`SELECT k FROM t WHERE `+ovf+`>0 LIMIT 1 OFFSET 5`,
		`SELECT * FROM (SELECT k FROM t WHERE `+ovf+`>0 LIMIT 0)`,
		`SELECT k FROM t WHERE nosuchcol > 0 LIMIT 0`,
		// A compound: no arm runs (multiSelect), and a UNION ALL arm is not
		// started once the arms before it used the LIMIT up.
		`SELECT `+ovf+` UNION ALL SELECT 1 LIMIT 0`,
		`SELECT 1 UNION SELECT `+ovf+` LIMIT 0`,
		`SELECT k FROM t INTERSECT SELECT `+ovf+` FROM t LIMIT 0`,
		`SELECT k FROM t EXCEPT SELECT `+ovf+` FROM t ORDER BY 1 LIMIT 0`,
		`SELECT * FROM (SELECT `+ovf+` UNION ALL SELECT 1 LIMIT 0)`,
		`SELECT 1 IN (SELECT `+ovf+` UNION ALL SELECT 1 LIMIT 0)`,
		`SELECT 1 UNION ALL SELECT `+ovf+` LIMIT 1`,
		`SELECT CASE WHEN k = 2 THEN `+ovf+` END FROM t UNION ALL SELECT 1 LIMIT 1`,
		`SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT `+ovf+` LIMIT 1 OFFSET 1`,
		`SELECT 1 UNION SELECT 2 UNION ALL SELECT `+ovf+` LIMIT 2`,
		// Nor is the OFFSET, coded after the LIMIT's jump.
		`SELECT k FROM t LIMIT 0 OFFSET `+ovf,
		`SELECT k FROM t LIMIT 0 OFFSET 'x'`,
		`SELECT (SELECT k FROM t LIMIT 0 OFFSET `+ovf+`)`,
		`SELECT 1 UNION ALL SELECT 2 LIMIT 0 OFFSET `+ovf,
	))
}
