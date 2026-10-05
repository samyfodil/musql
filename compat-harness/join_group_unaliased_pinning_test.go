package compat

// TestJoinGroupUnaliasedPinning tests that unaliased joins properly pin columns
// to their source scope by index rather than by name to avoid spurious ambiguity.
import "testing"

func TestJoinGroupUnaliasedPinning(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"unaliased-derived-in-group", []string{
			"CREATE TABLE t0_a(c0 INT)",
			"CREATE TABLE t0_b(c0 INT)",
			"CREATE TABLE t2(c0 INT)",
			"INSERT INTO t2 VALUES(1)",
			"SELECT * FROM (t0_a RIGHT JOIN (SELECT * FROM t2 LEFT JOIN t0_b))",
		}},
		{"aliased-derived-in-group", []string{
			"CREATE TABLE v0_a(c0 INT)",
			"CREATE TABLE v0_b(c0 INT)",
			"CREATE TABLE v2(c0 INT)",
			"INSERT INTO v2 VALUES(1)",
			"SELECT * FROM (v0_a RIGHT JOIN (SELECT * FROM v2 LEFT JOIN v0_b) AS d)",
		}},
		{"named-scope-still-works-normal-group", []string{
			"CREATE TABLE w1(a,b)",
			"CREATE TABLE w2(a,c)",
			"CREATE TABLE w3(a,d)",
			"INSERT INTO w1 VALUES(1,2)",
			"INSERT INTO w2 VALUES(1,3)",
			"INSERT INTO w3 VALUES(1,4)",
			"SELECT * FROM (w1 JOIN w2 USING(a)) JOIN w3 USING(a)",
		}},
		{"three-way-group-with-derived", []string{
			"CREATE TABLE x1(a INT)",
			"CREATE TABLE x2(a INT)",
			"CREATE TABLE x3(a INT)",
			"INSERT INTO x1 VALUES(1),(2)",
			"INSERT INTO x2 VALUES(2),(3)",
			"INSERT INTO x3 VALUES(3),(4)",
			"SELECT * FROM (x1 LEFT JOIN (SELECT * FROM x2 CROSS JOIN x3))",
		}},
		{"self-join-in-group-genuine-ambiguity", []string{
			"CREATE TABLE y1(a,b)",
			"INSERT INTO y1 VALUES(1,2)",
			"SELECT b FROM (y1 JOIN y1 USING(a))",
		}},
		{"group-with-using-and-unaliased-derived", []string{
			"CREATE TABLE z0_a(c0 INT, z INT)",
			"CREATE TABLE z0_b(c0 INT)",
			"CREATE TABLE z2(c0 INT)",
			"INSERT INTO z0_a VALUES(1,99)",
			"INSERT INTO z2 VALUES(1)",
			"SELECT * FROM (z0_a JOIN (SELECT * FROM z2 LEFT JOIN z0_b) USING(c0))",
		}},
		{"nested-group-with-derived-member", []string{
			"CREATE TABLE n1(a INT)",
			"CREATE TABLE n2(a INT)",
			"CREATE TABLE n3(a INT)",
			"INSERT INTO n1 VALUES(1),(2)",
			"INSERT INTO n2 VALUES(2),(3)",
			"INSERT INTO n3 VALUES(1)",
			"SELECT * FROM ((n1 LEFT JOIN (SELECT * FROM n2)) LEFT JOIN n3)",
		}},
		{"using-coalesce-plus-unaliased-derived-sibling", []string{
			"CREATE TABLE p1(a INT, z INT)",
			"CREATE TABLE p2(a INT)",
			"CREATE TABLE p3(a INT)",
			"INSERT INTO p1 VALUES(1,10)",
			"INSERT INTO p3 VALUES(1)",
			"SELECT * FROM ((p1 RIGHT JOIN p2 USING(a)) LEFT JOIN (SELECT * FROM p3))",
		}},
		{"natural-join-with-unaliased-derived", []string{
			"CREATE TABLE q1(a INT, b INT)",
			"CREATE TABLE q2(a INT)",
			"CREATE TABLE q3(a INT)",
			"INSERT INTO q1 VALUES(1,5)",
			"INSERT INTO q3 VALUES(1)",
			"SELECT * FROM (q1 NATURAL JOIN (SELECT * FROM q2 CROSS JOIN q3))",
		}},
		{"group-with-where-referencing-derived-cols", []string{
			"CREATE TABLE r0_a(c0 INT)",
			"CREATE TABLE r0_b(c0 INT)",
			"CREATE TABLE r2(c0 INT)",
			"INSERT INTO r0_a VALUES(9)",
			"INSERT INTO r2 VALUES(1)",
			"SELECT * FROM (r0_a RIGHT JOIN (SELECT * FROM r2 LEFT JOIN r0_b)) WHERE r0_a.c0 IS NULL OR r0_a.c0=9",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
