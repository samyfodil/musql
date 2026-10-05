package compat

import (
	"fmt"
	"strings"
	"testing"
)

// TestIsBoolKeywordOperandMatchesC verifies IS TRUE/FALSE correctly treats
// the TRUE/FALSE keywords, not columns with those names.
func TestIsBoolKeywordOperandMatchesC(t *testing.T) {
	var exprs []string
	for _, x := range []string{"2", "0", "0.5", "'x'", "'1'", "NULL", "-1"} {
		for _, rhs := range []string{
			"TRUE", "(TRUE)", "((TRUE))", "(true)", "FALSE", "(FALSE)", "(((false)))",
			"(TRUE COLLATE nocase)", "(FALSE) COLLATE nocase", "TRUE COLLATE bogus",
			"likely(TRUE)", "+TRUE", "(+FALSE)",
		} {
			exprs = append(exprs, x+" IS "+rhs, x+" IS NOT "+rhs)
		}
	}
	for i := 0; i < len(exprs); i += 13 {
		end := min(i+13, len(exprs))
		sel := "SELECT "
		for j, e := range exprs[i:end] {
			if j > 0 {
				sel += ", "
			}
			sel += e
		}
		differ(t, fmt.Sprintf("truth table %d", i), []string{sel})
	}
	differ(t, "precedence and nesting", []string{
		"SELECT 5 IS (((false)))=0, (2 IS (TRUE)) + 1, NOT 2 IS NOT (TRUE), 2 IS (TRUE) AND 0 IS (FALSE), typeof(2 IS (TRUE))",
	})

	// WHERE, CHECK, a generated column, an expression index and a DEFAULT
	// (whose IS never reaches resolveExprStep, so it compares with 1 or 0 --
	// defaultClauseIsBool) all with the parenthesized keyword.
	base := []string{
		"CREATE TABLE t(a, b INT CHECK (b IS NOT (FALSE)), g AS (a IS (TRUE)), d DEFAULT (5 IS (TRUE)), e DEFAULT ('00:00:00' IS NOT (FALSE)))",
		"INSERT INTO t(a, b) VALUES(2, 1), (0, 2), (NULL, 3), ('x', 4), (0.0, 5), (7, 6)",
		"CREATE INDEX ti ON t(a IS (TRUE))",
	}
	for i, q := range []string{
		"SELECT b, g, d, e FROM t ORDER BY b",
		"SELECT b FROM t WHERE a IS (TRUE) ORDER BY b",
		"SELECT b FROM t WHERE a IS NOT (TRUE) ORDER BY b",
		"SELECT b FROM t WHERE a IS (FALSE) ORDER BY b",
		"SELECT b FROM t WHERE a IS NOT (FALSE) ORDER BY b",
		"SELECT b FROM t WHERE (a IS (TRUE)) = g ORDER BY b",
		"SELECT a IS (TRUE), count(*) FROM t GROUP BY a IS (TRUE) ORDER BY 1",
		"SELECT sum(a IS (TRUE)), max(a) IS (FALSE) FROM t",
		"INSERT INTO t(a, b) VALUES(1, 0)",
	} {
		differ(t, fmt.Sprintf("table %d", i), append(append([]string{}, base...), q))
	}

	// A column named "true": the keyword is that column, for the bare
	// spelling and the parenthesized one alike, in every clause.
	named := []string{
		`CREATE TABLE c(a, "true", "false")`,
		"INSERT INTO c VALUES(2, 1, 0), (0, 0, 2), (NULL, 5, NULL), (1, 1, 1), (3, 3, 0)",
		"CREATE TABLE o(a)",
		"INSERT INTO o VALUES(2), (0), (1)",
		`CREATE TABLE n(b, "true")`,
		"INSERT INTO n VALUES(10, 2)",
	}
	for i, q := range []string{
		"SELECT a, a IS TRUE, a IS (TRUE), a IS NOT true, a IS NOT (true), a IS FALSE, a IS (false), a IS NOT (FALSE) FROM c ORDER BY rowid",
		"SELECT a FROM c WHERE a IS (true) ORDER BY rowid",
		"SELECT a FROM c WHERE a IS NOT (false) ORDER BY rowid",
		`SELECT "true" IS (TRUE), "true" IS (FALSE) FROM c ORDER BY rowid`,
		"SELECT a FROM o, n WHERE a IS true ORDER BY a",
		"SELECT a FROM n, o WHERE a IS (true) ORDER BY a",
		"SELECT a, (SELECT a IS (true) FROM n) FROM o ORDER BY a",
		"SELECT max(a) IS (true), max(a) IS false FROM c",
		"SELECT a IS (true), count(*) FROM c GROUP BY a IS (true) ORDER BY 1",
		"SELECT a, 0 AS true FROM o WHERE a IS (true) ORDER BY a",
		"SELECT a, 0 AS true FROM o WHERE a IS NOT true ORDER BY a",
		"SELECT a FROM o WHERE a IS (true) ORDER BY a",
	} {
		differ(t, fmt.Sprintf("column named true %d", i), append(append([]string{}, named...), q))
	}

	// Partial indexes whose condition is a truth test, over rows on both sides
	// of it: the planner's implication test (whereUsablePartialIndex,
	// where_plan_partial.go) must read "a IS (TRUE)" as the same TK_TRUTH it
	// reads "a IS TRUE" as, and a column named "true" as no truth test at all.
	// Over p the scan ORDER is not read out: the port compares a TK_TRUEFALSE
	// as unknown (its spelling is not kept), so an order-sensitive readout
	// there declines, at the base commit too, rather than guess C's plan.
	part := []string{
		"CREATE TABLE p(k INTEGER PRIMARY KEY, a, s)",
		"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<30) INSERT INTO p SELECT i, i % 3, 31 - i FROM r",
		"CREATE INDEX pt ON p(s) WHERE a IS TRUE",
		"CREATE INDEX pf ON p(s) WHERE a IS (FALSE)",
		`CREATE TABLE q(k INTEGER PRIMARY KEY, a, s, "true")`,
		`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<30) INSERT INTO q SELECT i, i % 3, 31 - i, i % 2 FROM r`,
		"CREATE INDEX qt ON q(s) WHERE a IS TRUE",
	}
	for i, w := range []string{
		"a IS (TRUE)", "a IS TRUE", "(a IS (TRUE))", "a IS (TRUE) AND s > 5", "a IS (FALSE)", "a IS FALSE AND s < 20",
		"a IS NOT (FALSE)", "a", "a IS ((true))",
	} {
		for j, out := range []string{"SELECT k FROM p WHERE %s ORDER BY k", "SELECT count(*), sum(s) FROM p WHERE %s",
			"SELECT group_concat(k) FROM q WHERE %s", "SELECT k FROM q WHERE %s LIMIT 4"} {
			differ(t, fmt.Sprintf("partial %d/%d", i, j), append(append([]string{}, part...), fmt.Sprintf(out, w)))
		}
	}
}

// TestDerivedTrueFalseColumnIsRenamed: sqlite3ColumnsFromExprList names a
// derived column "true" or "false" "column<N>" (select.c:2287), for a derived
// table, view, CTE and CTAS alike, so "x IS true" over one is the truth test
// and a bare "true" is the keyword. musql kept the name: "a IS true" over a
// derived "true" column compared against it -- and, in the IS spelling,
// returned the column's value.
func TestDerivedTrueFalseColumnIsRenamed(t *testing.T) {
	base := []string{
		"CREATE TABLE o(a)", "INSERT INTO o VALUES(2), (0), (1), (5)",
		`CREATE TABLE n(b, "true")`, "INSERT INTO n VALUES(10, 2), (11, 5)",
	}
	for i, q := range []string{
		`SELECT a IS true, count(*) FROM (SELECT o.a, n."true" FROM o, n) GROUP BY a IS true ORDER BY 1`,
		`SELECT a IS true, count(*) FROM o, (SELECT b, "true" FROM n) GROUP BY a IS true ORDER BY 1`,
		`SELECT a IS true FROM o, (SELECT b, "true" FROM n) ORDER BY 1`,
		`SELECT a IS true, "true" FROM (SELECT 0 AS a, 0 AS "true")`,
		`SELECT a IS NOT false FROM (SELECT 0 AS a, 0 AS "false")`,
		`SELECT * FROM (SELECT 5 AS "true", 6 AS FALSE, 7 AS column1)`,
		`WITH w AS (SELECT b, "true" FROM n) SELECT a IS true FROM o, w ORDER BY 1`,
		`WITH w("true") AS (SELECT 0) SELECT *, true FROM w`,
		`CREATE VIEW v AS SELECT b, "true" FROM n; SELECT a IS true FROM o, v ORDER BY 1`,
		`CREATE VIEW v(a, "true") AS SELECT 5, 6; SELECT *, true FROM v`,
		`CREATE TABLE ct AS SELECT 1 AS "true", 2 AS "False"; SELECT * FROM ct`,
		`CREATE TABLE ct AS SELECT 1 AS "true", 2 AS "False"; SELECT sql FROM sqlite_master WHERE name='ct'`,
	} {
		differ(t, fmt.Sprintf("derived true %d", i), append(append([]string{}, base...), strings.Split(q, "; ")...))
	}
}
