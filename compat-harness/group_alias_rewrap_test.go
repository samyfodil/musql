// Differential gate for parenthesized join groups re-wrapped in parentheses:
// "((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) AS y" now answers
// correctly by collapsing nested spans before alias/rebuild logic runs.
package compat

import "testing"

// TestGroupAliasRewrapMined tests the mined statement with empty and populated tables.
func TestGroupAliasRewrapMined(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1 (a INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2 (a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2x (b INTEGER PRIMARY KEY)`,
	}
	differ(t, "mined statement, empty tables", append(append([]string{}, setup...),
		`SELECT t1.a FROM ((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) as y`,
	))
	populated := append(append([]string{}, setup...),
		`INSERT INTO t1 VALUES (1),(2)`,
		`INSERT INTO t2 VALUES (1,10),(2,20)`,
		`INSERT INTO t2x VALUES (10),(20),(99)`,
	)
	differ(t, "mined statement, populated tables", append(append([]string{}, populated...),
		`SELECT t1.a FROM ((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) as y`,
	))
}

// TestGroupAliasRewrapScoping tests scoping boundaries: outer alias reaches all
// members, inner alias does not leak past the outer re-wrap, and bare "*" detects duplicates.
func TestGroupAliasRewrapScoping(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1 (a INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2 (a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2x (b INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES (1),(2)`,
		`INSERT INTO t2 VALUES (1,10),(2,20)`,
		`INSERT INTO t2x VALUES (10),(20),(99)`,
	}
	const from = `((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) as y`
	for _, tc := range []struct {
		name string
		q    string
	}{
		{"t1's bare name reaches through both nesting levels", `SELECT t1.a FROM ` + from},
		{"y's own alias reaches through to the inner group's members", `SELECT y.a FROM ` + from},
		{"x's own alias does not leak past the outer re-wrap", `SELECT x.a FROM ` + from},
		{"a bare star collides on t1.a/t2.a in the outer rebuild", `SELECT * FROM ` + from},
	} {
		differ(t, tc.name, append(append([]string{}, setup...), tc.q))
	}
}

// TestGroupAliasRewrapNonConnectorStillWorks confirms the fix did not disturb
// nested groups at non-connector positions that were already working.
func TestGroupAliasRewrapNonConnectorStillWorks(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1 (a INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2 (a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2x (b INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES (1),(2)`,
		`INSERT INTO t2 VALUES (1,10),(2,20)`,
		`INSERT INTO t2x VALUES (10),(20),(99)`,
	}
	differ(t, "nested group at a non-connector position, no further wrap", append(append([]string{}, setup...),
		`SELECT t2x.b FROM t2x JOIN (t1 JOIN t2 ON t1.a=t2.a) AS x ON t2x.b=x.b`,
	))
}

// TestGroupAliasRewrapRecursive tests that re-wraps collapse recursively at each nesting level.
func TestGroupAliasRewrapRecursive(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1 (a INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2 (a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2x (b INTEGER PRIMARY KEY)`,
		`CREATE TABLE t3x (c INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES (1),(2)`,
		`INSERT INTO t2 VALUES (1,10),(2,20)`,
		`INSERT INTO t2x VALUES (10),(20),(99)`,
		`INSERT INTO t3x VALUES (10),(30)`,
	}
	differ(t, "triple rewrap: z wraps y wraps x", append(append([]string{}, setup...),
		`SELECT t1.a FROM (((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) AS y JOIN t3x ON y.b=t3x.c) AS z`,
	))
	differ(t, "triple rewrap: z's own alias reaches the innermost member", append(append([]string{}, setup...),
		`SELECT z.a FROM (((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) AS y JOIN t3x ON y.b=t3x.c) AS z`,
	))
	differ(t, "triple rewrap: the middle alias y does not leak past the outer re-wrap either", append(append([]string{}, setup...),
		`SELECT y.a FROM (((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) AS y JOIN t3x ON y.b=t3x.c) AS z`,
	))
}

// TestGroupAliasRewrapLoneNoFurtherAlias tests the single-element case: re-wrap
// with no outer alias, where the inner alias must stay reachable.
func TestGroupAliasRewrapLoneNoFurtherAlias(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1 (a INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2 (a INTEGER PRIMARY KEY, b INTEGER)`,
		`INSERT INTO t1 VALUES (1),(2)`,
		`INSERT INTO t2 VALUES (1,10),(2,20)`,
	}
	differ(t, "lone re-wrap, no outer alias -- x's own alias still reaches", append(append([]string{}, setup...),
		`SELECT x.a FROM ((t1 JOIN t2 ON t1.a=t2.a) AS x)`,
	))
	differ(t, "lone re-wrap, no outer alias -- a bare name still reaches too", append(append([]string{}, setup...),
		`SELECT t1.a FROM ((t1 JOIN t2 ON t1.a=t2.a) AS x)`,
	))
}

// TestGroupAliasRewrapNonConnectorAliasStillDeclined tests the one deliberate gap:
// an aliased group at a non-connector position re-wrapped by an enclosing paren.
func TestGroupAliasRewrapNonConnectorAliasStillDeclined(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1 (a INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2 (a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2x (b INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES (1),(2)`,
		`INSERT INTO t2 VALUES (1,10),(2,20)`,
		`INSERT INTO t2x VALUES (10),(20),(99)`,
	}
	differAllowingDeclines(t, "non-connector-position aliased group, re-wrapped by an enclosing paren (declared gap)", append(append([]string{}, setup...),
		`SELECT t2x.b FROM (t2x JOIN (t1 JOIN t2 ON t1.a=t2.a) AS x ON t2x.b=x.b)`,
	))
}

// TestGroupAliasRewrapUnaliasedOuterGroupDeclines tests unaliased outer groups
// where an inner alias should not leak past the outer wrap.
func TestGroupAliasRewrapUnaliasedOuterGroupDeclines(t *testing.T) {
	setup := []string{
		`CREATE TABLE t0 (c INTEGER PRIMARY KEY)`,
		`CREATE TABLE t1 (a INTEGER PRIMARY KEY)`,
		`CREATE TABLE t2 (a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2x (b INTEGER PRIMARY KEY)`,
		`INSERT INTO t0 VALUES (10),(20),(99)`,
		`INSERT INTO t1 VALUES (1),(2)`,
		`INSERT INTO t2 VALUES (1,10),(2,20)`,
		`INSERT INTO t2x VALUES (10),(20)`,
	}
	differ(t, "unaliased outer group, connector's own alias referenced from the group's outward ON", append(append([]string{}, setup...),
		`SELECT t0.c FROM t0 JOIN ((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) ON t0.c=x.b`,
	))
	differ(t, "same shape, reached via a comma join's own trailing ON instead of JOIN", append(append([]string{}, setup...),
		`SELECT t0.c FROM t0, ((t1 JOIN t2 ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) ON t0.c=x.b`,
	))
	differ(t, "unaliased outer group forced atomic by an internal LEFT JOIN (no outward ON at all), referenced from the SELECT list", append(append([]string{}, setup...),
		`SELECT x.b FROM t0 JOIN ((t1 JOIN t2 ON t1.a=t2.a) AS x LEFT JOIN t2x ON x.b=t2x.b) ON t0.c IS NOT NULL`,
	))
}
