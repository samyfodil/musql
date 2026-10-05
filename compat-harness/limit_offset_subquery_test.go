// Tests for LIMIT/OFFSET clauses whose values are expressions (subqueries or
// EXISTS) in nested subqueries.
package compat

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestLimitOffsetSubqueryBasic verifies basic LIMIT/OFFSET expressions in
// scalar subqueries.
func TestLimitOffsetSubqueryBasic(t *testing.T) {
	differ(t, "LIMIT (subquery)", []string{
		`SELECT (SELECT 1 LIMIT (SELECT 2))`,
	})
	differ(t, "LIMIT (subquery) evaluating to 0 -- zero rows, NULL", []string{
		`SELECT (SELECT 1 LIMIT (SELECT 0))`,
	})
	differ(t, "OFFSET (subquery)", []string{
		`SELECT (SELECT 1 LIMIT (SELECT 5) OFFSET (SELECT 1))`,
	})
	differ(t, "OFFSET EXISTS(...) -- fuzz-1.18's own OFFSET shape", []string{
		`SELECT (SELECT 1 LIMIT (SELECT 5) OFFSET EXISTS (SELECT 1))`,
	})
}

// TestLimitOffsetSubqueryDerivedAndCorrelated verifies LIMIT/OFFSET
// expressions in derived tables, IN subqueries, and correlated subqueries.
func TestLimitOffsetSubqueryDerivedAndCorrelated(t *testing.T) {
	ddl := []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1),(2),(3),(4),(5)`,
	}
	differ(t, "derived table: FROM (SELECT ... LIMIT (subquery))", append(append([]string{}, ddl...),
		`SELECT * FROM (SELECT a FROM t1 LIMIT (SELECT 2))`,
	))
	differ(t, "IN (SELECT ... LIMIT (subquery))", append(append([]string{}, ddl...),
		`SELECT a FROM t1 WHERE a IN (SELECT a FROM t1 LIMIT (SELECT 2))`,
	))
	differ(t, "correlated outer scalar subquery, uncorrelated LIMIT subquery", append(append([]string{}, ddl...),
		`SELECT a, (SELECT x.a FROM t1 x WHERE x.a >= t1.a LIMIT (SELECT count(*) FROM t1)) FROM t1`,
	))
}

// TestLimitOffsetSubqueryParamStillDeclines verifies that bound parameters
// in LIMIT/OFFSET expressions decline rather than silently treating them as NULL.
func TestLimitOffsetSubqueryParamStillDeclines(t *testing.T) {
	q := `SELECT (SELECT a FROM t1 ORDER BY a LIMIT ?)`

	cdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE t1(a)`, `INSERT INTO t1 VALUES(1),(2),(3)`} {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	var want any
	if err := cdb.QueryRow(q, 2).Scan(&want); err != nil {
		t.Fatalf("oracle: %v", err)
	}

	mdb, err := sql.Open("sqlite", t.TempDir()+"/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	for _, s := range []string{`CREATE TABLE t1(a)`, `INSERT INTO t1 VALUES(1),(2),(3)`} {
		if _, err := mdb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	var got any
	err = mdb.QueryRow(q, 2).Scan(&got)
	if err != nil {
		// A clean decline is an acceptable outcome here: never-wrong beats
		// coverage for a shape compile time cannot correctly serve.
		return
	}
	if got != want {
		t.Fatalf("musql answered %v (WRONG), oracle says %v -- a bound-parameter LIMIT inside a nested subquery must decline, not guess", got, want)
	}
}

// TestLimitOffsetSubqueryFuzzTest118 verifies a complex mined statement that
// declines for a separate reason (ORDER BY ordinal range).
func TestLimitOffsetSubqueryFuzzTest118(t *testing.T) {
	differAllowingDeclines(t, "fuzz.test fuzz-1.18 verbatim -- LIMIT/OFFSET gap closed, a separate ORDER-BY-ordinal gap remains", []string{
		`SELECT -2147483649 << upper('fault' NOT IN (
        SELECT ALL (
           SELECT ALL -1
           ORDER BY -2147483649
           LIMIT (
              SELECT ALL (
                 SELECT 0 EXCEPT SELECT DISTINCT 'experiments' ORDER BY 1 ASC
              )
           )
           OFFSET EXISTS (
              SELECT ALL
                  (SELECT ALL -2147483648) NOT IN (
                     SELECT ALL 123456789.1234567899
                  ) IN (SELECT 2147483649)
              FROM sqlite_master
           ) NOT IN (SELECT ALL 'The')
        )
     ))`,
	})
}

// TestLimitOffsetSubqueryAggnested54 verifies a mined statement that uses
// LIMIT expressions with aggregates.
func TestLimitOffsetSubqueryAggnested54(t *testing.T) {
	differ(t, "aggnested.test 5.4 verbatim", []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(b)`,
		`SELECT(
    SELECT max(b) LIMIT (
      SELECT total( (SELECT a FROM t1) )
    )
  )
  FROM t2`,
	})
}

// TestLimitOffsetSubqueryAggnested54NonEmptyLimit verifies the same shape
// with a non-zero LIMIT to ensure aggregates actually execute.
func TestLimitOffsetSubqueryAggnested54NonEmptyLimit(t *testing.T) {
	differ(t, "aggnested.test 5.4 shape, non-zero LIMIT so the hoisted max(b) actually runs", []string{
		`CREATE TABLE t2(b)`,
		`INSERT INTO t2 VALUES(1),(2),(3)`,
		`SELECT (SELECT max(b) LIMIT (SELECT 5)) FROM t2`,
	})
}

// TestLimitOffsetSubqueryDoublyNestedUnresolvedStillDeclines verifies that
// doubly-nested unresolved LIMIT/OFFSET subqueries decline.
func TestLimitOffsetSubqueryDoublyNestedUnresolvedStillDeclines(t *testing.T) {
	differAllowingDeclines(t, "two levels of LIMIT-holds-a-still-unresolved-subquery, no parameter anywhere", []string{
		`SELECT (SELECT 1 LIMIT (SELECT 2 LIMIT (SELECT 3)))`,
	})
}
