package compat

// TestR29IndexOrderRules checks single-table index order decisions from SQLite's planner.
// Each case verifies the row order when a specific index rule applies.

import (
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestR29IndexOrderRules(t *testing.T) {
	// Mixed collations, DESC-leading indexes, automatic indexes on UNIQUE columns.
	setup := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b INTEGER, c TEXT COLLATE NOCASE,
			d TEXT COLLATE RTRIM, e TEXT COLLATE NOCASE, u TEXT UNIQUE, f INTEGER, g INTEGER)`,
		`CREATE INDEX ia ON t(a)`,
		`CREATE INDEX ib ON t(b)`,
		`CREATE INDEX ic ON t(c)`,
		`CREATE INDEX idd ON t(d)`,
		`CREATE INDEX ie ON t(e COLLATE BINARY)`,
		`CREATE INDEX ifdesc ON t(f DESC)`,
		`CREATE INDEX igmix ON t(g, f DESC)`,
		`INSERT INTO t VALUES(1,'apple',14,'foo','x','foo','u1',100,5)`,
		`INSERT INTO t VALUES(2,'apple',14,'FOO','x ','FOO','u2',200,5)`,
		`INSERT INTO t VALUES(3,'banana',7,'Foo','x  ','Foo','u3',100,8)`,
		`INSERT INTO t VALUES(4,'cherry',99,'bar','y','bar','u4',300,8)`,
		`INSERT INTO t VALUES(5,NULL,14,'baz','z ','baz','u5',200,5)`,
		`INSERT INTO t VALUES(6,'date',7,'qux','y ','qux',NULL,100,9)`,
	}
	cases := []struct{ name, sql string }{
		// Collation mismatch: covering scan uses index despite collation incompatibility.
		{"covering-scan-despite-collation-mismatch", "SELECT id FROM t WHERE e = 'FOO'"},
		// Non-leading DESC column in composite index affects row order within key groups.
		{"composite-nonleading-desc", "SELECT id FROM t WHERE g = 5"},
		{"composite-nonleading-desc-2", "SELECT id FROM t WHERE g = 8"},
		// DESC leading column reverses entire scan.
		{"desc-leading", "SELECT group_concat(id) FROM t WHERE f >= 100"},
		// Equality on leading column returns rows in rowid order.
		{"leading-eq-is-rowid-order", "SELECT id FROM t WHERE a = 'apple'"},
		{"leading-eq-numeric", "SELECT id FROM t WHERE b = 14"},
		{"leading-eq-nocase", "SELECT id FROM t WHERE c = 'foo'"},
		{"leading-eq-rtrim", "SELECT id FROM t WHERE d = 'x'"},
		// Range scan uses index order, not rowid order.
		{"range-binary", "SELECT group_concat(a) FROM t WHERE a > 'a'"},
		{"range-numeric", "SELECT group_concat(id) FROM t WHERE b > 7"},
		{"range-rtrim", "SELECT group_concat(id) FROM t WHERE d > 'x'"},
		// Automatic index on UNIQUE column uses index order.
		{"automatic-index-unique", "SELECT group_concat(id) FROM t WHERE u > 'u2'"},
		{"automatic-index-eq", "SELECT id FROM t WHERE u = 'u3'"},
		// Unindexable term forces table scan in rowid order.
		{"unindexable-term", "SELECT group_concat(id) FROM t WHERE id > 2"},
		// Non-covering index forces table lookups.
		{"noncovering-unconstrained", "SELECT group_concat(a||coalesce(c,'')) FROM t"},
		// IS NULL is a real index constraint.
		{"is-null", "SELECT group_concat(id) FROM t WHERE a IS NULL"},
		// IS NOT NULL does not drive index.
		{"is-not-null", "SELECT group_concat(id) FROM t WHERE a IS NOT NULL"},
		// NOT INDEXED hint forces table scan.
		{"not-indexed", "SELECT group_concat(a) FROM t NOT INDEXED WHERE a > 'a'"},
		// INDEXED BY hint uses named index.
		{"indexed-by", "SELECT group_concat(id) FROM t INDEXED BY ib WHERE b > 7"},
		// Two constraints on same indexed column create bounded range scan.
		{"two-sided-range", "SELECT group_concat(id) FROM t WHERE b > 7 AND b < 99"},
		// Unindexable constraint filters index scan results.
		{"index-plus-filter", "SELECT group_concat(id) FROM t WHERE b >= 7 AND id <> 3"},
	}

	stmts := append([]string{}, setup...)
	at := make([]int, 0, len(cases))
	for _, c := range cases {
		at = append(at, len(stmts))
		stmts = append(stmts, c.sql)
	}
	cgo := run(t, "cgo", stmts)
	mush := run(t, "musql", stmts)
	for i, c := range cases {
		cb, _ := json.Marshal(cgo[at[i]])
		mb, _ := json.Marshal(mush[at[i]])
		if string(cb) != string(mb) {
			t.Errorf("single-table index order rule %q DIVERGES\n  %s\n  cgo:    %s\n  musql: %s",
				c.name, c.sql, cb, mb)
		}
	}
}

// TestR29TieNowServed checks index selection when multiple indexes have identical cost.
// The planner should choose the last-created index in the chain.
func TestR29TieNowServed(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// Equal-width indexes covering statements with no column references.
		// Different index orders yield different row orders.
		{"equal-width-covering-tie", []string{
			"CREATE TABLE w1(id INTEGER PRIMARY KEY, b INTEGER, f INTEGER)",
			"CREATE INDEX w1b ON w1(b)", "CREATE INDEX w1f ON w1(f)",
			"INSERT INTO w1 VALUES(1,3,10),(2,1,30),(3,2,20)",
			"SELECT group_concat(id) FROM w1",
		}},
		{"equal-width-covering-tie-rev", []string{
			"CREATE TABLE w2(id INTEGER PRIMARY KEY, b INTEGER, f INTEGER)",
			"CREATE INDEX w2f ON w2(f)", "CREATE INDEX w2b ON w2(b)",
			"INSERT INTO w2 VALUES(1,3,10),(2,1,30),(3,2,20)",
			"SELECT group_concat(id) FROM w2",
		}},
	}
	for _, c := range cases {
		cgo := run(t, "cgo", c.stmts)
		mush := run(t, "musql", c.stmts)
		cb, _ := json.Marshal(cgo[len(c.stmts)-1])
		mb, _ := json.Marshal(mush[len(c.stmts)-1])
		if string(cb) != string(mb) {
			t.Errorf("cost TIE (%s): the index chain's direction is not reproduced\n  cgo:    %s\n  musql: %s",
				c.name, cb, mb)
		}
	}
}
