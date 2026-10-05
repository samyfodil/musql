package compat

import "testing"

// Grouped aggregates must drop JSON function subtypes at the leaf to match SQLite's
// serialized sorter behavior. These tests verify the loss is taken correctly
// via multiple code paths.
func TestAggGroupedSubtypeNarrowing(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(g,x,j)`,
		`INSERT INTO t VALUES(1,'a','[1,2]'),(1,'b','{"k":1}'),(2,'c','[3]')`,
	}
	for _, c := range []struct{ name, sql string }{
		// Expressions that cannot return a subtype.
		{"gc-upper", `SELECT g, group_concat(upper(x)) FROM t GROUP BY g ORDER BY g`},
		{"gc-concat-binop", `SELECT g, group_concat(x || '!') FROM t GROUP BY g ORDER BY g`},
		{"gc-substr-sep", `SELECT g, group_concat(x, substr('---',1,1)) FROM t GROUP BY g ORDER BY g`},
		{"gc-case", `SELECT g, group_concat(CASE WHEN x='a' THEN 'A' ELSE x END) FROM t GROUP BY g ORDER BY g`},
		{"gc-cast", `SELECT g, group_concat(CAST(x AS TEXT)) FROM t GROUP BY g ORDER BY g`},
		{"gc-filter-func", `SELECT g, group_concat(x) FILTER (WHERE upper(x)<>'B') FROM t GROUP BY g ORDER BY g`},
		{"jga-plain", `SELECT g, json_group_array(upper(x)) FROM t GROUP BY g ORDER BY g`},

		// Real subtype generators.
		{"jga-json-array", `SELECT g, json_group_array(json_array(x)) FROM t GROUP BY g ORDER BY g`},
		{"jga-json-extract", `SELECT g, json_group_array(json_extract(j,'$')) FROM t GROUP BY g ORDER BY g`},
		{"gc-json-array", `SELECT g, group_concat(json_array(x)) FROM t GROUP BY g ORDER BY g`},
		{"gc-json-quote", `SELECT g, group_concat(json_quote(x)) FROM t GROUP BY g ORDER BY g`},
		{"jgo-obj", `SELECT g, json_group_object(x, json_array(g)) FROM t GROUP BY g ORDER BY g`},
		{"gc-arrow", `SELECT g, group_concat(j -> '$') FROM t GROUP BY g ORDER BY g`},
		{"gc-arrow2", `SELECT g, group_concat(j ->> '$') FROM t GROUP BY g ORDER BY g`},
		{"subtype-of-func", `SELECT g, group_concat(subtype(json_array(x))) FROM t GROUP BY g ORDER BY g`},

		// Subtype values from row sources, which must lose the subtype in aggregates.
		{"subtype-of-row-hash", `SELECT key, max(subtype(value)) FROM json_each('[[7],[8]]') GROUP BY key`},
		// Drain path tests that would change answer if subtype loss were dropped.
		{"drain-json-quote-arg", `SELECT key, group_concat(json_quote(value)) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`},
		{"drain-json-group-array-col", `SELECT key, json_group_array(value) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`},
		{"drain-json-extract-arg", `SELECT key, group_concat(json_extract(value,'$')) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`},
		{"drain-arrow-arg", `SELECT key, group_concat(value -> '$[0]') FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`},
		{"subtype-of-row-drain", `SELECT key, max(subtype(value)) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`},
		{"subtype-of-row-gc", `SELECT key, group_concat(subtype(value)) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`},
		// Whole-table variant, which keeps the subtype (no grouping record).
		{"subtype-of-row-whole", `SELECT max(subtype(value)) FROM json_each('[[7],[8]]')`},

		// Discriminating cases that verify argument recursion and generator behavior.
		{"jga-json-quote", `SELECT g, json_group_array(json_quote(x)) FROM t GROUP BY g ORDER BY g`},
		{"jga-nested-coalesce", `SELECT g, json_group_array(coalesce(json_array(x),'z')) FROM t GROUP BY g ORDER BY g`},
		{"jga-case-gen", `SELECT g, json_group_array(CASE WHEN 1 THEN json_array(x) ELSE 'z' END) FROM t GROUP BY g ORDER BY g`},
		{"jga-uplus-gen", `SELECT g, json_group_array(+json_array(x)) FROM t GROUP BY g ORDER BY g`},
	} {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, append(append([]string{}, setup...), c.sql)) })
	}
}

// TestAggGroupedSubtypeGeneratedColumn tests that generated columns carrying
// subtypes are handled correctly in grouped aggregates.
func TestAggGroupedSubtypeGeneratedColumn(t *testing.T) {
	setup := []string{
		`CREATE TABLE tg(id INTEGER PRIMARY KEY, k, v, g AS (json_array(v)))`,
		`INSERT INTO tg(id,k,v) VALUES(1,0,5),(2,0,6),(3,1,7)`,
	}
	for _, c := range []struct{ name, sql string }{
		{"gen-ungrouped", `SELECT json_quote(g) FROM tg ORDER BY id`},
		{"gen-grouped-hash", `SELECT k, group_concat(json_quote(g)) FROM tg GROUP BY k`},
		{"gen-grouped-drain", `SELECT k, group_concat(json_quote(g)) FROM tg GROUP BY k ORDER BY k`},
		{"gen-grouped-jga", `SELECT k, json_group_array(g) FROM tg GROUP BY k ORDER BY k`},
		{"gen-grouped-subtype", `SELECT k, max(subtype(g)) FROM tg GROUP BY k ORDER BY k`},
	} {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, append(append([]string{}, setup...), c.sql)) })
	}
}

// TestAggGroupedSubtypeDerivedSource tests that derived tables carrying
// subtypes work correctly in grouped aggregates.
func TestAggGroupedSubtypeDerivedSource(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k,v)`,
		`INSERT INTO t VALUES(0,5),(0,6),(1,7)`,
	}
	for _, c := range []struct{ name, sql string }{
		{"derived-ungrouped", `SELECT json_quote(j) FROM (SELECT k, json_array(v) AS j FROM t) ORDER BY j`},
		{"derived-grouped-hash", `SELECT k, group_concat(json_quote(j)) FROM (SELECT k, json_array(v) AS j FROM t) GROUP BY k`},
		{"derived-grouped-drain", `SELECT k, group_concat(json_quote(j)) FROM (SELECT k, json_array(v) AS j FROM t) GROUP BY k ORDER BY k`},
		{"derived-grouped-jga", `SELECT k, json_group_array(j) FROM (SELECT k, json_array(v) AS j FROM t) GROUP BY k ORDER BY k`},
	} {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, append(append([]string{}, setup...), c.sql)) })
	}
}

// TestAggGroupedSubtypeNarrowingKeepsFilterOrder tests that FILTER clauses are
// evaluated before their arguments to prevent invalid evaluations on rejected rows.
func TestAggGroupedSubtypeNarrowingKeepsFilterOrder(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k,v,s)`,
		`INSERT INTO t VALUES(1,-9223372036854775808,'a'),(1,5,'b'),(2,7,'c')`,
	}
	for _, c := range []struct{ name, sql string }{
		{"filter-excludes-overflow", `SELECT k, sum(abs(v)) FILTER (WHERE v > -9223372036854775808) FROM t GROUP BY k ORDER BY k`},
		{"filter-excludes-overflow-nogroup", `SELECT sum(abs(v)) FILTER (WHERE v > -9223372036854775808) FROM t`},
		{"sep-with-overflow", `SELECT k, group_concat(NULL, abs(v)) FROM t WHERE v = -9223372036854775808 GROUP BY k`},
		{"jgo-sep-overflow", `SELECT k, json_group_object(NULL, abs(v)) FROM t WHERE v = -9223372036854775808 GROUP BY k`},
		{"sep-generator", `SELECT k, group_concat(NULL, json_quote(abs(v))) FROM t WHERE v = -9223372036854775808 GROUP BY k`},
		{"arg-overflow-unfiltered", `SELECT k, sum(abs(v)) FROM t GROUP BY k ORDER BY k`},
		{"wider-arg", `SELECT k, sum(v * 2 + length(s)) FROM t GROUP BY k ORDER BY k`},
		{"wider-sep", `SELECT k, group_concat(s, s || 'x') FROM t GROUP BY k ORDER BY k`},
		{"filter-both-lower", `SELECT k, sum(v), max(v) FILTER (WHERE v > 0) FROM t GROUP BY k ORDER BY k`},
	} {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, append(append([]string{}, setup...), c.sql)) })
	}
}

// TestAggGroupedLeafSubtypeLoss tests that subtype loss in grouped aggregates
// is applied at the leaf level, matching SQLite's serialized sorter behavior.
func TestAggGroupedLeafSubtypeLoss(t *testing.T) {
	gen := []string{
		`CREATE TABLE tg(id INTEGER PRIMARY KEY, k, v, g AS (json_array(v)))`,
		`INSERT INTO tg(id,k,v) VALUES(1,0,5),(2,0,6),(3,1,7)`,
	}
	for _, c := range []struct {
		name  string
		setup []string
		sql   string
	}{
		// Subtype generators with subtyped arguments.
		{"min-json-quote-hash", nil,
			`SELECT min(json_quote(value)) FROM json_each('[[7]]') GROUP BY key`},
		{"gc-json-quote-generated-hash", gen,
			`SELECT k, group_concat(json_quote(g)) FROM tg GROUP BY k`},
		{"jga-json-quote-hash", nil,
			`SELECT key, json_group_array(json_quote(value)) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`},

		// Arguments that generate subtypes themselves, which must survive the strip.
		{"jga-json-array-of-leaf", nil,
			`SELECT key, json_group_array(json_array(value)) FROM json_each('[[7]]') GROUP BY key`},
		{"jga-json-array-generated", gen,
			`SELECT k, json_group_array(json_array(g)) FROM tg GROUP BY k ORDER BY k`},
		{"gc-json-quote-of-generator", nil,
			`SELECT key, group_concat(json_quote(json_array(value))) FROM json_each('[[7]]') GROUP BY key`},

		// WHERE conditions see subtypes before the sorter strips them.
		{"where-keeps-subtype", nil,
			`SELECT key, count(*) FROM json_each('[[7]]') WHERE json_quote(value) = '[7]' GROUP BY key`},
		{"where-keeps-subtype-generated", gen,
			`SELECT k, count(*) FROM tg WHERE json_quote(g) = '[5]' GROUP BY k`},
		// GROUP BY conditions also see subtypes before the sorter.
		{"group-by-a-generator", nil,
			`SELECT json_quote(value), count(*) FROM json_each('[[7],[7]]') GROUP BY json_quote(value)`},

		// Correlated subqueries must see the stripped leaf value.
		{"correlated-subquery-arg", nil,
			`SELECT key, max((SELECT json_quote(je.value))) FROM json_each('[[7]]') je GROUP BY key`},
		{"correlated-subquery-arg-generated", gen,
			`SELECT k, max((SELECT json_quote(tg.g))) FROM tg GROUP BY k`},

		// DISTINCT and FILTER with stripped leaves.
		{"distinct-over-stripped-leaf", nil,
			`SELECT key, count(DISTINCT json_quote(value)) FROM json_each('[[7],[7]]') GROUP BY key`},
		{"filter-over-stripped-leaf", nil,
			`SELECT key, group_concat(json_quote(value)) FILTER (WHERE json_quote(value) <> 'x') FROM json_each('[[7]]') GROUP BY key`},

		// Whole-table variants that must keep the subtype (no grouping record).
		{"whole-table-keeps", nil,
			`SELECT json_quote(max(value)) FROM json_each('[[7],[8]]')`},
		{"whole-table-keeps-arg", nil,
			`SELECT min(json_quote(value)) FROM json_each('[[7]]')`},
		{"whole-table-keeps-generated", gen,
			`SELECT group_concat(json_quote(g)) FROM tg`},
	} {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, append(append([]string{}, c.setup...), c.sql))
		})
	}
}
