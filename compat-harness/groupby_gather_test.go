package compat

import (
	"fmt"
	"testing"
)

// TestGroupByGatheredSteps: hashed GROUP BYs whose steps native code gathers
// and the VDBE replays in order. A group's first row decides what its key
// columns show (1 against a later 1.0), a LEFT JOIN steps its null row too,
// groups that arrive in an index's order come out in it, whether the index is
// named or chosen, and a sorted seek into one segment finds tied keys in rowid
// order.
func TestGroupByGatheredSteps(t *testing.T) {
	base := []string{
		`CREATE TABLE c(id INTEGER PRIMARY KEY, k INTEGER, name TEXT)`,
		`CREATE TABLE o(id INTEGER PRIMARY KEY, ck INTEGER, amt REAL, note TEXT)`,
		`CREATE TABLE g(a, b, v INTEGER)`,
		`CREATE INDEX g_a_desc ON g(a DESC)`,
		`CREATE INDEX g_ba ON g(b, a)`,
	}
	for i := 1; i <= 60; i++ {
		base = append(base, fmt.Sprintf(`INSERT INTO c VALUES(%d, %d, 'n%d')`, i, (i*7)%23, i%5))
	}
	for i := 1; i <= 500; i++ {
		note := "'plain'"
		if i%9 == 0 {
			note = "'special requests'"
		}
		base = append(base, fmt.Sprintf(`INSERT INTO o VALUES(%d, %d, %d.5, %s)`, i, (i*13)%67, i%17, note))
	}
	keys := []string{"1", "1.0", "2", "'2'", "2.0", "NULL", "'x'", "3"}
	for i := 0; i < 300; i++ {
		base = append(base, fmt.Sprintf(`INSERT INTO g VALUES(%s, %d, %d)`, keys[(i*5)%len(keys)], i%4, i))
	}
	queries := []string{
		`SELECT a, count(*), sum(v) FROM g GROUP BY a`,
		`SELECT a, b, count(*), max(v) FROM g GROUP BY a, b`,
		`SELECT a, count(*) FROM g WHERE v > 10 GROUP BY a ORDER BY 2 DESC, 1`,
		`SELECT c.id, count(o.id) FROM c LEFT JOIN o ON o.ck = c.id AND o.note NOT LIKE '%special%' GROUP BY c.id`,
		`SELECT n, count(*) FROM (SELECT c.id, count(o.id) AS n FROM c LEFT JOIN o ON o.ck = c.id GROUP BY c.id) GROUP BY n ORDER BY 2 DESC, 1 DESC`,
		`SELECT c.name, count(o.id), total(o.amt) FROM c LEFT JOIN o ON o.ck = c.k GROUP BY c.name`,
		`SELECT c.k, group_concat(o.id) FROM c JOIN o ON o.ck = c.k GROUP BY c.k`,
		`SELECT a, count(*) FROM g INDEXED BY g_a_desc WHERE a IS NOT NULL GROUP BY a`,
		`SELECT a, count(*) FROM g GROUP BY a`,
		`SELECT b, a, count(*) FROM g GROUP BY b, a`,
		`SELECT b, a, count(*) FROM g INDEXED BY g_ba GROUP BY b, a`,
		`SELECT a, count(*) FROM g INDEXED BY g_a_desc WHERE a IS NOT NULL GROUP BY a LIMIT 2`,
		`SELECT a, count(*) FROM g INDEXED BY g_a_desc WHERE a IS NOT NULL GROUP BY a LIMIT 2 OFFSET 1`,
		`SELECT o.ck, count(*) FROM o JOIN c ON c.id = o.ck WHERE o.ck IN (3, 3.0, '3', 4.5) GROUP BY o.ck`,
		`SELECT c.id, sum(o.amt) FROM c LEFT JOIN o ON o.ck = c.id + 0.0 GROUP BY c.id HAVING count(o.id) > 0`,
		`SELECT a, sum(v) FROM g GROUP BY a HAVING sum(v) > 9223372036854775000`,
	}
	for _, q := range queries {
		differ(t, "groupby-gather", append(append([]string{}, base...), q))
	}
	overflow := []string{
		`CREATE TABLE big(k INTEGER, v INTEGER)`,
		`INSERT INTO big VALUES(1, 9223372036854775807), (2, 1), (1, 1), (2, 5)`,
		`SELECT k, sum(v) FROM big GROUP BY k`,
	}
	differ(t, "groupby-gather-overflow", overflow)
}
