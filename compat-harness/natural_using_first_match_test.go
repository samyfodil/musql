package compat

// TestNaturalUsingFirstMatch verifies that NATURAL/USING column resolution
// picks the first matching table when multiple tables share the column name,
// and only checks for ambiguity in RIGHT/FULL JOIN coalesce contexts.
import "testing"

func TestNaturalUsingFirstMatch(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"5way-self-join-natural", []string{
			"CREATE TABLE t102(i0 INTEGER)",
			"INSERT INTO t102 VALUES(1),(2),(500)",
			"CREATE TABLE t2x AS SELECT DISTINCT * FROM t102 AS t0 JOIN t102 AS t4 ON (t0.i0 IN (SELECT i0 FROM t102)) NATURAL JOIN t102 AS t3 JOIN t102 AS t1 ON (t0.i0 IN (SELECT i0 FROM t102)) JOIN t102 AS t2 ON (t2.i0=+t0.i0 OR (t0.i0<>500 AND t2.i0=t1.i0))",
		}},
		{"simple-3way-inner-natural-picks-first", []string{
			"CREATE TABLE a1(k,v1)",
			"CREATE TABLE a2(k,v2)",
			"CREATE TABLE a3(k,v3)",
			"INSERT INTO a1 VALUES(1,'x')",
			"INSERT INTO a2 VALUES(1,'y')",
			"INSERT INTO a3 VALUES(1,'z')",
			"SELECT * FROM a1 JOIN a2 NATURAL JOIN a3",
		}},
		{"using-with-two-earlier-matches-picks-first", []string{
			"CREATE TABLE b1(k,v)",
			"CREATE TABLE b2(k,v)",
			"CREATE TABLE b3(k)",
			"INSERT INTO b1 VALUES(1,10)",
			"INSERT INTO b2 VALUES(1,20)",
			"INSERT INTO b3 VALUES(1)",
			"SELECT * FROM b1 JOIN b2 JOIN b3 USING(k)",
		}},
		{"right-join-genuine-ambiguity-still-declined", []string{
			"CREATE TABLE c1(k,v1)",
			"CREATE TABLE c2(k,v2)",
			"CREATE TABLE c3(k,v3)",
			"INSERT INTO c1 VALUES(1,'x')",
			"INSERT INTO c2 VALUES(1,'y')",
			"INSERT INTO c3 VALUES(1,'z')",
			"SELECT * FROM c1 JOIN c2 RIGHT JOIN c3 USING(k)",
		}},
		{"full-join-genuine-ambiguity-still-declined", []string{
			"CREATE TABLE d1(k,v1)",
			"CREATE TABLE d2(k,v2)",
			"CREATE TABLE d3(k,v3)",
			"INSERT INTO d1 VALUES(1,'x')",
			"INSERT INTO d2 VALUES(1,'y')",
			"INSERT INTO d3 VALUES(1,'z')",
			"SELECT * FROM d1 JOIN d2 FULL JOIN d3 USING(k)",
		}},
		{"right-join-with-priors-usingprefixed-not-ambiguous", []string{
			"CREATE TABLE e1(k,v1)",
			"CREATE TABLE e2(k,v2)",
			"CREATE TABLE e3(k,v3)",
			"INSERT INTO e1 VALUES(1,'x')",
			"INSERT INTO e2 VALUES(1,'y')",
			"INSERT INTO e3 VALUES(1,'z')",
			"SELECT * FROM e1 JOIN e2 USING(k) RIGHT JOIN e3 USING(k)",
		}},
		{"self-join-genuine-two-value-ambiguity", []string{
			"CREATE TABLE f1(a,b)",
			"INSERT INTO f1 VALUES(1,2)",
			"SELECT b FROM (f1 JOIN f1 USING(a))",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
