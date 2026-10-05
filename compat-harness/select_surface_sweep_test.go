package compat

// SELECT surface sweep: cross-product test over clause combinations, join
// kinds, subquery positions and set operations for correctness and decline census.

import (
	"fmt"
	"strings"
	"testing"
)

var selectSweepSchema = []string{
	`CREATE TABLE a(id INTEGER PRIMARY KEY, g INTEGER, t TEXT COLLATE NOCASE, n NUMERIC, x)`,
	`INSERT INTO a VALUES(1,1,'Ann','10',1),(2,1,'bob','2',NULL),(3,2,'ann','3.5','s'),(4,2,NULL,NULL,x'00'),(5,3,'Zoe','-1',2.5)`,
	`CREATE TABLE b(aid INTEGER, tag TEXT, w REAL)`,
	`INSERT INTO b VALUES(1,'x',1.0),(1,'y',2.0),(2,'x',3.0),(4,'z',NULL),(9,'orphan',9.0)`,
	`CREATE TABLE c(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
	`INSERT INTO c VALUES('x',10),('y',20),('q',30)`,
	`CREATE INDEX a_g ON a(g)`,
	`CREATE INDEX b_aid ON b(aid, tag)`,
	`CREATE VIEW av AS SELECT id, g, t FROM a WHERE g > 0`,
}

func TestSelectClauseCrossProduct(t *testing.T) {
	c, m := boundPair(t, selectSweepSchema)
	froms := []string{
		`a`,
		`a, b`,
		`a JOIN b ON b.aid = a.id`,
		`a LEFT JOIN b ON b.aid = a.id`,
		`a LEFT JOIN b ON b.aid = a.id AND b.tag = 'x'`,
		`b RIGHT JOIN a ON b.aid = a.id`,
		`a FULL OUTER JOIN b ON b.aid = a.id`,
		`a CROSS JOIN c`,
		`a JOIN b USING (aid)`,
		`(SELECT id, g FROM a) AS s`,
		`(SELECT id, g FROM a) AS s JOIN b ON b.aid = s.id`,
		`av`,
		`av LEFT JOIN b ON b.aid = av.id`,
		`a, (SELECT max(w) AS mw FROM b)`,
		`a JOIN (a AS a2 JOIN b ON b.aid = a2.id) ON a.id = a2.id`,
	}
	selects := []string{`count(*)`, `count(a.id)`, `max(a.id)`, `sum(a.g)`, `total(a.n)`}
	tails := []string{
		``,
		`WHERE a.g > 1`,
		`WHERE a.t IS NULL`,
		`WHERE a.t = 'ANN'`,
		`GROUP BY a.g`,
		`GROUP BY a.g HAVING count(*) > 1`,
		`GROUP BY a.t`,
		`WHERE a.g > 0 GROUP BY a.g HAVING sum(a.g) > 1`,
	}
	n, total := 0, 0
	for _, f := range froms {
		for _, s := range selects {
			for _, tail := range tails {
				q := fmt.Sprintf(`SELECT %s FROM %s %s`, s, f, tail)
				total++
				cv, mv := renderQuery(c, q), renderQuery(m, q)
				if cv != mv {
					n++
					if n <= 30 {
						t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
					}
				}
			}
		}
	}
	if n > 0 {
		t.Errorf("%d of %d clause combinations diverged", n, total)
	}
}

func TestSelectOrderingCrossProduct(t *testing.T) {
	c, m := boundPair(t, selectSweepSchema)
	projections := []string{
		`id, t`,
		`DISTINCT g`,
		`t AS label, n`,
		`id, t COLLATE BINARY AS tb`,
		`g, count(*) AS cnt`,
	}
	orders := []string{
		`ORDER BY 1`,
		`ORDER BY 1 DESC`,
		`ORDER BY 1 ASC NULLS LAST`,
		`ORDER BY 1 DESC NULLS FIRST`,
		`ORDER BY 2`,
		`ORDER BY 2 NULLS FIRST`,
		`ORDER BY 1, 2 DESC`,
		`ORDER BY t, id`,
		`ORDER BY t COLLATE BINARY, id`,
		`ORDER BY t COLLATE NOCASE DESC, id`,
		`ORDER BY upper(t), id`,
		`ORDER BY n, id`,
		`ORDER BY CAST(n AS REAL), id`,
		`ORDER BY g, id DESC`,
		`ORDER BY -id`,
	}
	n, total := 0, 0
	for _, p := range projections {
		for _, o := range orders {
			group := ``
			if strings.Contains(p, "count(*)") {
				group = ` GROUP BY g`
				o = strings.NewReplacer(", id DESC", "", ", id", "", "ORDER BY t", "ORDER BY g", "ORDER BY n", "ORDER BY g", "ORDER BY upper(t)", "ORDER BY g", "ORDER BY CAST(n AS REAL)", "ORDER BY g", "ORDER BY -id", "ORDER BY -g").Replace(o)
			}
			q := fmt.Sprintf(`SELECT %s FROM a%s %s`, p, group, o)
			total++
			cv, mv := renderQuery(c, q), renderQuery(m, q)
			if cv != mv {
				n++
				if n <= 30 {
					t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
				}
			}
		}
	}
	if n > 0 {
		t.Errorf("%d of %d ordering combinations diverged", n, total)
	}
}

func TestSelectSubqueryPositions(t *testing.T) {
	c, m := boundPair(t, selectSweepSchema)
	n := 0
	for _, q := range []string{
		`SELECT id, (SELECT max(w) FROM b) FROM a ORDER BY id`,
		`SELECT id, (SELECT max(w) FROM b WHERE b.aid = a.id) FROM a ORDER BY id`,
		`SELECT id, EXISTS(SELECT 1 FROM b WHERE b.aid = a.id) FROM a ORDER BY id`,
		`SELECT id, NOT EXISTS(SELECT 1 FROM b WHERE b.aid = a.id) FROM a ORDER BY id`,
		`SELECT id, (SELECT count(*) FROM b WHERE b.aid = a.id AND b.w IS NOT NULL) FROM a ORDER BY id`,
		`SELECT s.g, s.cnt FROM (SELECT g, count(*) AS cnt FROM a GROUP BY g) s ORDER BY s.g`,
		`SELECT * FROM (SELECT * FROM (SELECT id, g FROM a) i WHERE i.g > 1) o ORDER BY o.id`,
		`SELECT * FROM a, (SELECT count(*) AS n FROM b) ORDER BY a.id`,
		`SELECT id FROM a WHERE id IN (SELECT aid FROM b) ORDER BY id`,
		`SELECT id FROM a WHERE id NOT IN (SELECT aid FROM b) ORDER BY id`,
		`SELECT id FROM a WHERE id NOT IN (SELECT aid FROM b WHERE w IS NOT NULL) ORDER BY id`,
		`SELECT id FROM a WHERE g = (SELECT min(g) FROM a) ORDER BY id`,
		`SELECT id FROM a WHERE g > (SELECT avg(g) FROM a) ORDER BY id`,
		`SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.aid = a.id AND b.tag > a.t) ORDER BY id`,
		`SELECT g, count(*) FROM a GROUP BY g HAVING count(*) = (SELECT max(cnt) FROM (SELECT count(*) AS cnt FROM a GROUP BY g)) ORDER BY g`,
		`SELECT g FROM a GROUP BY g HAVING g IN (SELECT aid FROM b) ORDER BY g`,
		`SELECT id FROM a ORDER BY (SELECT count(*) FROM b WHERE b.aid = a.id) DESC, id`,
		`SELECT id FROM a ORDER BY id LIMIT (SELECT count(*) FROM c)`,
		`SELECT a.id, b.tag FROM a JOIN b ON b.aid = a.id AND b.w > (SELECT min(w) FROM b) ORDER BY a.id, b.tag`,
		`SELECT a.id FROM a LEFT JOIN b ON b.aid = a.id AND EXISTS(SELECT 1 FROM c WHERE c.k = b.tag) ORDER BY a.id`,
		`WITH t AS (SELECT aid, count(*) AS n FROM b GROUP BY aid) SELECT a.id, t.n FROM a LEFT JOIN t ON t.aid = a.id ORDER BY a.id`,
		`WITH t AS (SELECT max(w) AS mw FROM b) SELECT id FROM a WHERE id < (SELECT mw FROM t) ORDER BY id`,
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 5) SELECT group_concat(n) FROM r`,
		`WITH RECURSIVE r(n) AS (SELECT min(id) FROM a UNION ALL SELECT n+1 FROM r WHERE n < (SELECT max(id) FROM a)) SELECT count(*) FROM r`,
		`SELECT count((SELECT 1)) FROM a`,
		`SELECT sum((SELECT count(*) FROM b WHERE b.aid = a.id)) FROM a`,
		`SELECT max((SELECT w FROM b WHERE b.aid = a.id ORDER BY w LIMIT 1)) FROM a`,
	} {
		cv, mv := renderQuery(c, q), renderQuery(m, q)
		if cv != mv {
			n++
			t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d subquery positions diverged", n)
	}
}

func TestSelectSetOperations(t *testing.T) {
	c, m := boundPair(t, selectSweepSchema)
	ops := []string{`UNION`, `UNION ALL`, `INTERSECT`, `EXCEPT`}
	tails := []string{
		``,
		`ORDER BY 1, 2`,
		`ORDER BY 1 DESC, 2`,
		`ORDER BY 1 NULLS FIRST, 2`,
		`ORDER BY 2, 1`,
		`LIMIT 2`,
		`ORDER BY 1, 2 LIMIT 2`,
		`ORDER BY 1, 2 LIMIT 2 OFFSET 1`,
	}
	n, total := 0, 0
	for _, op := range ops {
		for _, tail := range tails {
			for _, pair := range [][2]string{
				{`SELECT id, t FROM a`, `SELECT rowid+100, tag FROM b`},
				{`SELECT id, t FROM a WHERE g > 1`, `SELECT 99, 'x'`},
				{`SELECT DISTINCT id, t FROM a`, `SELECT rowid+100, tag FROM b WHERE w IS NOT NULL`},
				{`SELECT g, count(*) FROM a GROUP BY g`, `SELECT aid, count(*) FROM b GROUP BY aid`},
			} {
				q := fmt.Sprintf(`%s %s %s %s`, pair[0], op, pair[1], tail)
				total++
				cv, mv := renderQuery(c, q), renderQuery(m, q)
				if cv != mv {
					n++
					if n <= 30 {
						t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
					}
				}
			}
		}
	}
	for _, q := range []string{
		`SELECT g FROM a UNION SELECT aid FROM b UNION SELECT v FROM c ORDER BY 1`,
		`SELECT g FROM a UNION ALL SELECT aid FROM b EXCEPT SELECT v FROM c ORDER BY 1`,
		`SELECT g FROM a INTERSECT SELECT aid FROM b UNION SELECT 99 ORDER BY 1`,
		`SELECT count(*) FROM (SELECT g FROM a UNION ALL SELECT aid FROM b) ORDER BY 1`,
		`SELECT * FROM (SELECT g FROM a UNION SELECT aid FROM b) ORDER BY 1`,
		`SELECT count(*) FROM (SELECT g FROM a UNION ALL SELECT aid FROM b)`,
		`SELECT * FROM (SELECT g FROM a EXCEPT SELECT aid FROM b) x ORDER BY 1`,
		`WITH u AS (SELECT g FROM a UNION SELECT aid FROM b) SELECT count(*) FROM u`,
	} {
		total++
		cv, mv := renderQuery(c, q), renderQuery(m, q)
		if cv != mv {
			n++
			t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d of %d set-operation combinations diverged", n, total)
	}
}

// TestSelectGroupConcatJoinDecline tests group_concat/string_agg behavior
// across join shapes, measuring the boundary between served and declined cases.
func TestSelectGroupConcatJoinDecline(t *testing.T) {
	c, m := boundPair(t, selectSweepSchema)
	for _, tc := range []struct {
		q       string
		decline bool
	}{
		{`SELECT group_concat(a.id) FROM a`, false},
		{`SELECT group_concat(a.id) FROM a, b`, false},
		{`SELECT group_concat(a.id) FROM a JOIN b ON b.aid = a.id`, false},
		{`SELECT group_concat(a.id) FROM a LEFT JOIN b ON b.aid = a.id`, false},
		{`SELECT group_concat(a.id ORDER BY a.id) FROM a, b`, false},
		{`SELECT group_concat(a.id) FROM a, (SELECT max(w) AS mw FROM b)`, true},
		{`SELECT group_concat(a.id) FROM a, (SELECT 1 AS one)`, true},
		{`SELECT group_concat(a.id) FROM a JOIN (a AS a2 JOIN b ON b.aid = a2.id) ON a.id = a2.id`, true},
		{`SELECT string_agg(a.id, ',') FROM a, (SELECT max(w) AS mw FROM b)`, true},
		{`SELECT count(*) FROM a, (SELECT max(w) AS mw FROM b)`, false},
		{`SELECT total(a.id) FROM a, (SELECT max(w) AS mw FROM b)`, false},
		{`SELECT max(a.id) FROM a, (SELECT max(w) AS mw FROM b)`, false},
	} {
		tc := tc
		t.Run(strings.NewReplacer(" ", "_", "(", "", ")", "", "'", "", "*", "star", ",", "").Replace(tc.q), func(t *testing.T) {
			cv, mv := renderQuery(c, tc.q), renderQuery(m, tc.q)
			if cv == "ERR" {
				t.Fatalf("the oracle refused %q -- the case no longer measures anything", tc.q)
			}
			if tc.decline {
				if mv != "ERR" {
					t.Errorf("%s: served now (%s) -- if the arrival order is provable for this shape, move it to the served set", tc.q, mv)
				}
				return
			}
			if cv != mv {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", tc.q, cv, mv)
			}
		})
	}
}
