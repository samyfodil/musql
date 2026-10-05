package compat

import "testing"

// This file gates anchor plan order checks for window functions in subqueries.
// It verifies a correlated outer reference doesn't incorrectly trigger index-inert decline.

func r43AnchorSchema() []string {
	return []string{
		"CREATE TABLE a(c UNIQUE)",
		"INSERT INTO a VALUES(4),(0),(9),(-9)",
	}
}

// TestR43AnchorWindowMoverDeclines verifies window functions in correlated subqueries work.
func TestR43AnchorWindowMoverDeclines(t *testing.T) {
	for _, q := range []string{
		// Mined statement with join wrapper.
		`SELECT a.c
		   FROM a
		   JOIN a AS b ON a.c=4
		   JOIN a AS e ON a.c=e.c
		  WHERE a.c=(SELECT (SELECT coalesce(lead(2) OVER(),0) + sum(d.c))
		               FROM a AS d
		              WHERE a.c)`,
		// Same anchor without join.
		`SELECT a.c FROM a
		  WHERE a.c=(SELECT (SELECT coalesce(lead(2) OVER(),0) + sum(d.c))
		               FROM a AS d
		              WHERE a.c)
		  ORDER BY a.c`,
		// Window call with local argument.
		`SELECT a.c FROM a
		  WHERE a.c=(SELECT (SELECT coalesce(lead(d.c) OVER(),0) + sum(d.c))
		               FROM a AS d
		              WHERE a.c)
		  ORDER BY a.c`,
	} {
		stmts := append(append([]string(nil), r43AnchorSchema()...), q)
		if res := run(t, "musql", stmts); res[len(res)-1]["kind"] == "error" {
			t.Errorf("anchorPlanOrderProvable should now prove this table's index inert: %s\n  got: %v", q, res[len(res)-1])
			continue
		}
		if !differ(t, "r43anchorwindowmover", stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
