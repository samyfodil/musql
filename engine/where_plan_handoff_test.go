package engine

import (
	"path/filepath"
	"testing"
)

// TestAggOrderHandOffAgreesWithPlan: an aggregate over a join whose order
// markWherePlanEligibility handed over is answered from that hand-off, and a
// fresh plan of the same sources decides it too: joins of two and three
// tables, a correlated subquery over a join, and the derived tables and CTEs
// markCoroutineReruns hands over, which the planner declines.
func TestAggOrderHandOffAgreesWithPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path,
		`CREATE TABLE a(id INTEGER PRIMARY KEY, x INTEGER, y TEXT)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, aid INTEGER, z INTEGER)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, bid INTEGER, w INTEGER)`,
		`CREATE INDEX b_aid ON b(aid)`,
		`CREATE INDEX c_bid ON c(bid)`,
		`CREATE INDEX a_x ON a(x)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 300) INSERT INTO a SELECT i, i % 17, 'y' || (i % 5) FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 900) INSERT INTO b SELECT i, i % 300 + 1, i % 23 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 1800) INSERT INTO c SELECT i, i % 900 + 1, i % 7 FROM n`,
		`ANALYZE`,
	)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	wherePlanHandOffCheckForTest.on = true
	defer func() { wherePlanHandOffCheckForTest = handOffCheck{} }()
	for _, q := range []string{
		`SELECT a.x, count(*), sum(b.z) FROM a JOIN b ON b.aid = a.id GROUP BY a.x`,
		`SELECT a.id, sum(b.z) FROM a JOIN b ON b.aid = a.id GROUP BY a.id`,
		`SELECT a.id, max(b.z) FROM a, b WHERE b.aid = a.id AND a.x = 3 GROUP BY a.id`,
		`SELECT b.aid, count(*) FROM b JOIN c ON c.bid = b.id GROUP BY b.aid`,
		`SELECT a.y, count(*) FROM a JOIN b ON b.aid = a.id JOIN c ON c.bid = b.id GROUP BY a.y`,
		`SELECT a.x, min(c.w) FROM a JOIN b ON b.aid = a.id JOIN c ON c.bid = b.id WHERE a.x < 5 GROUP BY a.x`,
		`SELECT a.id, count(*) FROM a LEFT JOIN b ON b.aid = a.id GROUP BY a.id`,
		`SELECT a.x, sum(b.z) FROM a JOIN b ON b.aid = a.id WHERE b.z > 10 GROUP BY a.x ORDER BY a.x`,
		`SELECT b.id, count(*) FROM b JOIN c ON c.bid = b.id WHERE b.aid BETWEEN 10 AND 50 GROUP BY b.id`,
		`SELECT a.id FROM a WHERE a.x = (SELECT min(c.w) FROM b, c WHERE b.aid = a.id AND c.bid = b.id) ORDER BY a.id`,
		`SELECT a.id, (SELECT sum(c.w) FROM b, c WHERE b.aid = a.id AND c.bid = b.id AND c.w > 2) FROM a WHERE a.x < 4`,
		`SELECT b.id FROM b, a WHERE a.id = b.aid AND b.z = (SELECT max(c.w) FROM c, b b2 WHERE c.bid = b2.id AND b2.aid = a.id)`,
		`SELECT * FROM (SELECT count(*) FROM b WHERE z > 3) s1, (SELECT count(*) FROM c WHERE w < 2) s2, (SELECT avg(x) FROM a) s3`,
		`WITH t AS (SELECT aid, sum(z) s FROM b GROUP BY aid) SELECT a.x, t.s FROM t, a WHERE a.id = t.aid ORDER BY a.x, t.s`,
		`WITH t AS (SELECT aid, sum(z) s FROM b GROUP BY aid) SELECT t1.aid FROM t t1, t t2 WHERE t1.aid = t2.aid + 1 AND t1.s > t2.s`,
	} {
		if _, _, err := p.QueryArgs(q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if wherePlanHandOffCheckForTest.hits == 0 {
		t.Fatal("no aggregate answered from the hand-off")
	}
	if wherePlanHandOffCheckForTest.differ != 0 {
		t.Fatalf("%d of %d hand-off answers a fresh plan does not decide", wherePlanHandOffCheckForTest.differ, wherePlanHandOffCheckForTest.hits)
	}
}
