package compat

import "testing"

// A bare TRUE/FALSE keyword is treated as a literal for comparison-affinity.
// This tests affinity rules are applied correctly to columns in comparisons.

func TestTrueFalseKeywordComparisonAffinity(t *testing.T) {
	setup := []string{
		`CREATE TABLE tt(x TEXT)`,
		`INSERT INTO tt VALUES(1),('1'),(true),(0),('0'),(false),('abc')`,
		`CREATE TABLE ti(y INTEGER)`,
		`INSERT INTO ti VALUES(1),('1'),(true),(0)`,
	}
	qs := []string{
		`SELECT quote(x), typeof(x) FROM tt ORDER BY rowid`,
		`SELECT count(*) FROM tt WHERE x = true`,
		`SELECT count(*) FROM tt WHERE x = 1`,
		`SELECT count(*) FROM tt WHERE x = '1'`,
		`SELECT count(*) FROM tt WHERE x = false`,
		`SELECT count(*) FROM tt WHERE x < true`,
		`SELECT count(*) FROM tt WHERE x > false`,
		`SELECT count(*) FROM tt WHERE x IN (true, false)`,
		`SELECT count(*) FROM tt WHERE x BETWEEN false AND true`,
		`SELECT count(*) FROM ti WHERE y = true`,
		`SELECT quote(true), typeof(true), quote(false), typeof(false)`,
		`SELECT quote(CAST(true AS TEXT)), quote('x'||true)`,
		`SELECT count(*) FROM tt WHERE x = TRUE`,
		`SELECT count(*) FROM tt WHERE true = x`,
		`SELECT count(*) FROM tt WHERE x IS true`,
		`SELECT count(*) FROM tt WHERE x = (true)`,
		`SELECT count(*) FROM tt WHERE x = +true`,
		`SELECT count(*) FROM tt WHERE x = 1=1`,
	}
	for _, q := range qs {
		q := q
		t.Run(q, func(t *testing.T) { differ(t, "tf/"+q, append(append([]string{}, setup...), q)) })
	}
}
