// LIMIT/OFFSET clause name scope: cannot refer to FROM columns or correlated references, but trigger NEW/OLD are allowed.
package compat

import "testing"

func limitScopeBase() []string {
	return []string{
		`CREATE TABLE t(a INTEGER, c INTEGER)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`CREATE TABLE u(b INTEGER)`,
		`INSERT INTO u VALUES(100),(200),(300)`,
		`CREATE TABLE dst(v INTEGER)`,
	}
}

// TestLimitOffsetRejectsCorrelatedNames: LIMIT/OFFSET rejects correlated names with live outer context.
func TestLimitOffsetRejectsCorrelatedNames(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		{"update-set-qualified", []string{
			`UPDATE t SET c = (SELECT b FROM u LIMIT t.a)`,
			`SELECT a,c FROM t ORDER BY a`,
		}},
		{"update-set-unqualified", []string{
			`UPDATE t SET c = (SELECT b FROM u LIMIT a)`,
			`SELECT a,c FROM t ORDER BY a`,
		}},
		{"update-where-exists", []string{
			`UPDATE t SET c = 0 WHERE EXISTS(SELECT 1 FROM u LIMIT t.a)`,
			`SELECT a,c FROM t ORDER BY a`,
		}},
		{"delete-where-exists", []string{
			`DELETE FROM t WHERE EXISTS(SELECT 1 FROM u LIMIT t.a)`,
			`SELECT a,c FROM t ORDER BY a`,
		}},
		{"having-exists", []string{
			`SELECT a, count(*) FROM t GROUP BY a HAVING EXISTS(SELECT 1 FROM u LIMIT t.a)`,
		}},
		{"update-set-offset-qualified", []string{
			`UPDATE t SET c = (SELECT b FROM u LIMIT 1 OFFSET t.a)`,
			`SELECT a,c FROM t ORDER BY a`,
		}},
	} {
		differ(t, "limit-scope-"+c.name, append(limitScopeBase(), c.tail...))
	}
}

// TestLimitOffsetAcceptsTriggerPseudoRow: trigger NEW/OLD inside LIMIT must still answer.
func TestLimitOffsetAcceptsTriggerPseudoRow(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		{"after-insert-limit-new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a; END`,
			`INSERT INTO t VALUES(2,0)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		{"after-insert-offset-new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT 1 OFFSET new.a; END`,
			`INSERT INTO t VALUES(1,0)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		{"after-delete-limit-old", []string{
			`CREATE TRIGGER tg AFTER DELETE ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT old.a; END`,
			`DELETE FROM t WHERE a = 2`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		{"after-update-limit-old-offset-new", []string{
			`CREATE TRIGGER tg AFTER UPDATE ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT old.a OFFSET new.c; END`,
			`UPDATE t SET c = 1 WHERE a = 2`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		// The substituted value still takes the clause's ordinary INTEGER
		// coercion: a whole-string-numeric TEXT folds, a NULL is C SQLite's
		// "datatype mismatch" (OP_MustBeInt, select.c:2549).
		{"pseudorow-text-numeric", []string{
			`CREATE TABLE ts(a TEXT, c INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON ts BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a; END`,
			`INSERT INTO ts VALUES('2',0)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		{"pseudorow-null", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.c; END`,
			`INSERT INTO t VALUES(1,NULL)`,
			`SELECT v FROM dst ORDER BY v`,
			`SELECT count(*) FROM t`,
		}},
		{"pseudorow-arith", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a + 1; END`,
			`INSERT INTO t VALUES(1,0)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
	} {
		differ(t, "limit-pseudorow-"+c.name, append(limitScopeBase(), c.tail...))
	}
}

// TestLimitOffsetPseudoRowRowid: the rowid pseudo-column is read from a different slot than real columns.
func TestLimitOffsetPseudoRowRowid(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		// new.rowid is 2 and new.a is 3, so "LIMIT new.rowid" must copy TWO
		// rows of u; reading the column instead copies three.
		{"after-insert-limit-new-rowid", []string{
			`CREATE TABLE r(a INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON r BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.rowid; END`,
			`INSERT INTO r(rowid,a) VALUES(2,3)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		// The same split on the OFFSET side, where the wrong read skips past
		// the end of u and lands zero rows instead of one.
		{"after-insert-offset-new-rowid", []string{
			`CREATE TABLE r(a INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON r BEGIN INSERT INTO dst SELECT b FROM u LIMIT 1 OFFSET new.rowid; END`,
			`INSERT INTO r(rowid,a) VALUES(1,7)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		// old.rowid on a DELETE: rowid 2, column 3.
		{"after-delete-limit-old-rowid", []string{
			`CREATE TABLE r(a INTEGER)`,
			`INSERT INTO r(rowid,a) VALUES(2,3),(5,1)`,
			`CREATE TRIGGER tg AFTER DELETE ON r BEGIN INSERT INTO dst SELECT b FROM u LIMIT old.rowid; END`,
			`DELETE FROM r WHERE a=3`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		// The _rowid_ spelling is the same pseudo-column.
		{"after-insert-limit-new-underscore-rowid", []string{
			`CREATE TABLE r(a INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON r BEGIN INSERT INTO dst SELECT b FROM u LIMIT new._rowid_; END`,
			`INSERT INTO r(rowid,a) VALUES(2,3)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		// SHADOWING: a REAL column named "rowid" wins for that name on that
		// table, so this must read 3, not the row's actual rowid of 1.
		{"after-insert-limit-new-shadowed-rowid", []string{
			`CREATE TABLE r(rowid INTEGER, a INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON r BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.rowid; END`,
			`INSERT INTO r VALUES(3,99)`,
			`SELECT v FROM dst ORDER BY v`,
		}},
		// An UPDATE fires with both rows live: old.rowid and new.a must come
		// from the two different slots of the same firing row.
		{"after-update-limit-old-rowid-offset-new", []string{
			`CREATE TABLE r(a INTEGER, c INTEGER)`,
			`INSERT INTO r(rowid,a,c) VALUES(3,9,0)`,
			`CREATE TRIGGER tg AFTER UPDATE ON r BEGIN INSERT INTO dst SELECT b FROM u LIMIT old.rowid OFFSET new.c; END`,
			`UPDATE r SET c = 1 WHERE a = 9`,
			`SELECT v FROM dst ORDER BY v`,
		}},
	} {
		differ(t, "limit-pseudorowid-"+c.name, append(limitScopeBase(), c.tail...))
	}
}

// TestLimitOffsetExpressionsStillAnswer: expressions in LIMIT must still evaluate.
func TestLimitOffsetExpressionsStillAnswer(t *testing.T) {
	for _, q := range []string{
		`SELECT a FROM t ORDER BY a LIMIT 1+1`,
		`SELECT a FROM t ORDER BY a LIMIT abs(-2)`,
		`SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u)`,
		`SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u) OFFSET (SELECT min(b)/100 FROM u)`,
		`SELECT a FROM t ORDER BY a LIMIT '2'`,
		`SELECT a FROM t ORDER BY a LIMIT 2.0`,
		`SELECT a FROM t ORDER BY a LIMIT CAST('2' AS INTEGER)`,
		`SELECT a FROM t ORDER BY a LIMIT NULL`,
		`SELECT a FROM t ORDER BY a LIMIT -1 OFFSET 2`,
		`WITH cte(n) AS (SELECT 2) SELECT a FROM t ORDER BY a LIMIT (SELECT n FROM cte)`,
		`WITH cte(n) AS (SELECT 2) SELECT a FROM t ORDER BY a LIMIT 2 OFFSET (SELECT n FROM cte)`,
		`SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 1+2`,
		`SELECT x FROM (SELECT a AS x FROM t) ORDER BY x LIMIT 1+1`,
		`SELECT a, count(*) FROM t GROUP BY a ORDER BY a LIMIT 1+1`,
		`SELECT 1 LIMIT 1+0`,
	} {
		differ(t, "limit-expr-"+q, append(limitScopeBase(), q))
	}
}
