// This file tests correlated subqueries with derived tables accessing outer aliases.
// subquery is resolved against pOuterNC -- the outer NameContext of the query
// that contains the FROM clause -- never against that containing query's own
// (not-yet-built) alias list, because the FROM-item loop runs strictly BEFORE
// the containing SELECT's own sNC/NC_UEList is constructed. A derived table
// has no visibility into its own FROM-clause siblings either ("SQLite has no
// LATERAL by default", sql_ast.go's FromItem.Subquery doc comment), so this
// splice is safe to apply to a qualifying FROM item regardless of what else
// sits alongside it. All of it is pure AST-level substitution, run before any
// codegen; compileColumn itself is untouched.
//
// A narrower gap remains open, deliberately: when the query CONTAINING the
// FROM clause (not one of its FROM items) has its OWN WHERE/HAVING/GROUP BY
// referencing the alias while it ALSO owns a FROM clause, that reference
// still declines ("EXISTS (SELECT * FROM (SELECT 1) WHERE c=5)" verified to
// answer in C SQLite, but stays unsupported here) -- deciding that safely
// would require knowing whether one of that level's OWN real FROM columns
// also happens to be named "c", which this pure-AST pass has no schema
// access to prove. See substituteAliasesIntoSub's own doc comment.
package compat

import "testing"

// TestAliasCorrelatedDerivedExists is the exact reported gap and its
// straightforward variations: EXISTS/IN/a scalar comparison wrapping a
// FROM-clause derived table whose own (FROM-less) body references an
// enclosing query's literal select-list alias.
func TestAliasCorrelatedDerivedExists(t *testing.T) {
	differ(t, "EXISTS(SELECT * FROM (bare alias ref))", []string{
		`SELECT 1 AS c WHERE EXISTS (SELECT * FROM (SELECT c))`,
	})
	differ(t, "IN(SELECT * FROM (bare alias ref)) -- outer value is a column ref", []string{
		`SELECT 1 AS c WHERE c IN (SELECT * FROM (SELECT c))`,
	})
	differ(t, "IN(SELECT * FROM (bare alias ref)) -- outer value is a literal", []string{
		`SELECT 1 AS c WHERE 1 IN (SELECT * FROM (SELECT c))`,
	})
	differ(t, "scalar comparison against a derived-table-wrapped alias ref", []string{
		`SELECT 1 AS c, 2 AS c WHERE 1 = (SELECT * FROM (SELECT c))`,
	})
	differ(t, "scalar comparison, second identical alias does NOT win (first-alias-wins)", []string{
		`SELECT 1 AS c, 2 AS c WHERE 2 = (SELECT * FROM (SELECT c))`,
	})
}

// TestAliasCorrelatedDerivedNesting confirms the splice recurses through
// arbitrarily many layers of pure derived-table wrapping, and that it stops
// correctly at a layer whose own body genuinely has a FROM to a real table
// (that layer's own real column must still win, and the splice must not even
// attempt to look past it).
func TestAliasCorrelatedDerivedNesting(t *testing.T) {
	stmts := []string{`CREATE TABLE t3(c)`, `INSERT INTO t3 VALUES(99)`}
	differ(t, "two layers of bare derived-table wrapping", append(stmts,
		`SELECT 5 AS c WHERE 5 = (SELECT * FROM (SELECT * FROM (SELECT c)))`,
	))
	differ(t, "three layers of bare derived-table wrapping", append(stmts,
		`SELECT 5 AS c WHERE 5 = (SELECT * FROM (SELECT * FROM (SELECT * FROM (SELECT c))))`,
	))
	differ(t, "a derived table's OWN FROM to a real table blocks the splice at that layer (EXISTS only checks row existence, not value)", append(stmts,
		`SELECT 1 AS c WHERE EXISTS (SELECT * FROM (SELECT c FROM t3))`,
	))
}

// TestAliasCorrelatedDerivedSiblingIsolation confirms a derived table's body
// is isolated from every OTHER item in the same FROM clause -- verified
// against a REAL table sharing the alias's own name, which must NOT shadow
// the outer alias (SQLite derived tables are never LATERAL).
func TestAliasCorrelatedDerivedSiblingIsolation(t *testing.T) {
	stmts := []string{`CREATE TABLE t3(c)`, `INSERT INTO t3 VALUES(99)`}
	differ(t, "a sibling real-table FROM item sharing the alias's name does not shadow it", append(stmts,
		`SELECT 5 AS c WHERE 5 = (SELECT d.c FROM t3, (SELECT c) AS d)`,
	))
	differ(t, "a sibling derived table's OWN alias of the same name does not leak across siblings", []string{
		`SELECT 5 AS c WHERE 5 = (SELECT y.q FROM (SELECT 7 AS c) x, (SELECT c AS q) AS y)`,
	})
}

// TestAliasCorrelatedDerivedShadowing confirms the existing shadowing/scope
// rules (an inner alias of the same name wins; only a scope-free alias
// expression is ever spliced) still apply identically when reached through a
// FROM-clause derived table, not just the already-shipped FROM-less nested
// case.
func TestAliasCorrelatedDerivedShadowing(t *testing.T) {
	differ(t, "the derived table's own alias of the same name shadows the outer one", []string{
		`SELECT 1 AS c WHERE EXISTS (SELECT * FROM (SELECT 5 AS c WHERE c=1))`,
	})
	differ(t, "an aggregate over the derived table's spliced body", []string{
		`SELECT 5 AS c WHERE 5 = (SELECT sum(x) FROM (SELECT c AS x))`,
	})
}

// TestAliasCorrelatedDerivedOwnClauseStillDeclines pins the one narrower gap
// this fix deliberately leaves open: the query CONTAINING the FROM clause
// referencing the alias directly in ITS OWN WHERE, while also owning a FROM
// clause of its own -- C SQLite answers this, musql still declines it.
// differAllowingDeclines (not differ) is used deliberately: it stays green
// the day this gap closes (a musql answer that now MATCHES the oracle),
// and would only turn red on the thing that actually matters -- a musql
// answer that DIFFERS from the oracle's.
func TestAliasCorrelatedDerivedOwnClauseStillDeclines(t *testing.T) {
	differAllowingDeclines(t, "containing query's own WHERE referencing the alias while it also has a FROM (still declines, a coverage gap not a wrong answer)", []string{
		`SELECT 5 AS c WHERE EXISTS (SELECT * FROM (SELECT * FROM (SELECT 1) WHERE c=5))`,
	})
}
