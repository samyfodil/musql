package engine

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestJoinLevelScopesMatchAFreshList: every level of a join compiles its ON
// terms, seek keys and correlated table-valued arguments against the scopes
// of the sources bound before it, sliced from one list per join; each slice
// must be the list built afresh for that level. Joins of up to seven sources,
// LEFT and RIGHT joins, ON subqueries, a parenthesized group, correlated
// seeks and json_each over an earlier table.
func TestJoinLevelScopesMatchAFreshList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path,
		`CREATE TABLE a(id INTEGER PRIMARY KEY, x INTEGER, j TEXT)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, aid INTEGER, z INTEGER)`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, bid INTEGER, w INTEGER)`,
		`CREATE INDEX b_aid ON b(aid)`,
		`CREATE INDEX c_bid ON c(bid)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200) INSERT INTO a SELECT i, i % 17, json_array(i % 3, i % 5) FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 600) INSERT INTO b SELECT i, i % 200 + 1, i % 23 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 900) INSERT INTO c SELECT i, i % 600 + 1, i % 7 FROM n`,
		`ANALYZE`,
	)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	slices, differ := 0, 0
	joinScopesCheckForTest = func(shared, fresh []compileScope) {
		slices++
		if len(shared) != len(fresh) || len(shared) > 0 && !reflect.DeepEqual(shared, fresh) {
			differ++
		}
	}
	defer func() { joinScopesCheckForTest = nil }()
	for _, q := range []string{
		`SELECT count(*) FROM a JOIN b ON b.aid = a.id JOIN c ON c.bid = b.id`,
		`SELECT count(*), sum(c.w) FROM a, b, c, b b2, a a2, c c2, b b3 WHERE b.aid = a.id AND c.bid = b.id AND b2.aid = a.id AND a2.id = b2.aid AND c2.bid = b2.id AND b3.id = c2.bid`,
		`SELECT a.id, b.z FROM a LEFT JOIN b ON b.aid = a.id AND b.z > (SELECT min(w) FROM c WHERE c.bid = a.id) ORDER BY 1, 2 LIMIT 20`,
		`SELECT count(*) FROM a LEFT JOIN (b JOIN c ON c.bid = b.id) ON b.aid = a.id WHERE a.x < 4`,
		`SELECT count(*) FROM b RIGHT JOIN a ON b.aid = a.id JOIN c ON c.bid = b.id`,
		`SELECT a.id, e.value FROM a JOIN b ON b.aid = a.id, json_each(a.j) e WHERE e.value = b.z % 3 ORDER BY 1, 2 LIMIT 20`,
		`SELECT a.x, count(*) FROM a JOIN b ON b.aid = a.id JOIN c ON c.bid = b.id AND c.w = a.x % 7 GROUP BY a.x`,
	} {
		if _, _, err := p.QueryArgs(q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if slices == 0 || differ != 0 {
		t.Fatalf("%d of %d shared scope lists differ from a fresh one", differ, slices)
	}
}
