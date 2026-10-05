package compat

import (
	"testing"
)

// TestRecordedDefectsReprobe tests specific defect cases to verify they are fixed.
func TestRecordedDefectsReprobe(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE o(a,c)`,
		`INSERT INTO o VALUES(1,'p'),(3,'q')`,
		`CREATE TABLE u(a,b)`,
		`INSERT INTO u VALUES(1,'z')`,
		`CREATE VIEW v AS SELECT a FROM t`,
		`CREATE INDEX ti ON t(b)`,
	}
	for _, q := range []string{
		// min/max only inside a select-list subquery.
		`SELECT (SELECT min(a) FROM t)`,
		`SELECT (SELECT max(a) FROM t), (SELECT min(b) FROM t)`,
		`SELECT a, (SELECT max(o.a) FROM o WHERE o.a >= t.a) FROM t ORDER BY a`,
		// aggregate / GROUP BY over a parenthesized join group.
		`SELECT count(*) FROM t JOIN (o JOIN u ON u.a = o.a) ON t.a = o.a`,
		`SELECT sum(t.a) FROM (t JOIN o ON t.a = o.a)`,
		`SELECT t.a, count(*) FROM (t JOIN o ON t.a = o.a) GROUP BY t.a ORDER BY t.a`,
		`SELECT count(*) FROM (t JOIN o USING(a)) GROUP BY a ORDER BY a`,
		// ":N" duplicate-column naming inside a parenthesized join group.
		`SELECT * FROM (t JOIN o ON t.a = o.a)`,
		`SELECT * FROM (t JOIN u ON t.a = u.a)`,
		`SELECT * FROM (t JOIN u USING(a))`,
		`ALTER TABLE t RENAME TO t2`,
		`SELECT sql FROM sqlite_master WHERE type='view'`,
	} {
		c, m := boundPair(t, setup)
		if cv, mv := renderQuery(c, q), renderQuery(m, q); cv != mv {
			t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
		}
	}
}
