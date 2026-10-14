package compat

// A subquery or view C materializes (not flattened, not a co-routine) and that
// is not correlated is filled once per statement under OP_Once, even inside a
// correlated subquery that runs once per outer row. Inside a parenthesized join
// group holding a RIGHT or FULL join, a FROM-less member is such a body: a
// random() value it produces is the same for every outer row. Members C runs
// as co-routines or flattens are evaluated again each time.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var groupMaterializeOnceSchema = []string{
	"CREATE TABLE o(id INTEGER)",
	"CREATE TABLE anchor(id INTEGER)",
	"CREATE VIEW v AS SELECT hex(randomblob(8)) AS value",
	"CREATE TABLE e(d INTEGER)",
	"INSERT INTO o VALUES(1),(2),(3)",
	"INSERT INTO anchor VALUES(1),(2),(3)",
}

func TestGroupMaterializeOnce(t *testing.T) {
	for _, q := range []string{
		"SELECT count(DISTINCT x) FROM (SELECT (SELECT v.value FROM anchor, v WHERE anchor.id=o.id) AS x FROM o)",
		"SELECT count(DISTINCT x) FROM (SELECT (SELECT v.value FROM anchor RIGHT JOIN v ON true WHERE anchor.id=o.id) AS x FROM o)",
		"SELECT count(DISTINCT x) FROM (SELECT (SELECT v.value FROM anchor RIGHT JOIN (v FULL JOIN e ON false) ON true WHERE anchor.id=o.id LIMIT 1) AS x FROM o)",
		"SELECT count(DISTINCT x) FROM (SELECT (SELECT v.value FROM anchor JOIN (v RIGHT JOIN e ON true) ON true WHERE anchor.id=o.id LIMIT 1) AS x FROM o)",
		"SELECT count(DISTINCT x) FROM (SELECT (SELECT v.value FROM anchor JOIN (v LEFT JOIN e ON false) ON true WHERE anchor.id=o.id LIMIT 1) AS x FROM o)",
		"WITH results AS MATERIALIZED (SELECT o.id AS id, (SELECT v.value FROM anchor RIGHT JOIN (v FULL JOIN e ON false) ON true WHERE anchor.id=o.id LIMIT 1) AS value FROM o) SELECT count(DISTINCT value), min(value)=max(value) FROM results",
	} {
		t.Run(q, func(t *testing.T) {
			differ(t, q, append(append([]string{}, groupMaterializeOnceSchema...), q))
		})
	}
}
