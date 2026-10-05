// Tests for an escaping aggregate reached from HAVING/select-list of a query
// that is already aggregate. The aggregate should resolve to the closest
// enclosing query whose FROM supplies the referenced column.
package compat

import "testing"

// invoiceSetup is aggnested.test's own 7.0 fixture verbatim.
var invoiceSetup = []string{
	`CREATE TABLE invoice (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, amount DOUBLE PRECISION DEFAULT NULL, name VARCHAR(100) DEFAULT NULL)`,
	`INSERT INTO invoice (amount, name) VALUES (4.0,'Michael'),(15.0,'Bara'),(4.0,'Michael'),(6.0,'John')`,
}

// TestAggHoistAlreadyAggregateOwnerMinedStatement is aggnested.test 7.1 itself,
// the exact mined statement, plus 7.2 as the sibling control that already
// worked before this fix and must keep working after it (a select-list
// subquery of a query that only BECOMES aggregate via the hoist, not one that
// already is).
func TestAggHoistAlreadyAggregateOwnerMinedStatement(t *testing.T) {
	differ(t, "aggnested.test 7.1: HAVING escape through a FROM-item derived table", append(append([]string{}, invoiceSetup...), []string{
		`SELECT sum(amount), name from invoice group by name having (select v > 6 from (select sum(amount) v) t)`,
	}...))
	differ(t, "aggnested.test 7.2 control: select-list of a not-yet-aggregate query", append(append([]string{}, invoiceSetup...), []string{
		`SELECT (select 1 from (select sum(amount))) FROM invoice`,
	}...))
	differ(t, "7.1 variant: T reached DIRECTLY, no FROM-item wrapper", append(append([]string{}, invoiceSetup...), []string{
		`SELECT sum(amount), name from invoice group by name having (select sum(amount) > 6)`,
	}...))
}

// TestAggHoistAlreadyAggregateOwnerFurtherControls is aggnested.test 8.0 and 8.1,
// immediately following 7.2 in the same file: nested derived-table wrapping
// of an escaping aggregate reached from a select-list item of a query that
// (like 7.2) only becomes aggregate via the hoist. Both already worked before
// this fix and must not regress.
func TestAggHoistAlreadyAggregateOwnerFurtherControls(t *testing.T) {
	t1Setup := []string{
		`CREATE TABLE t1(x INT)`,
		`INSERT INTO t1 VALUES(100)`,
		`INSERT INTO t1 VALUES(20)`,
		`INSERT INTO t1 VALUES(3)`,
	}
	differ(t, "aggnested.test 8.0", append(append([]string{}, t1Setup...), []string{
		`SELECT (SELECT y FROM (SELECT sum(x) AS y) AS t2 ) FROM t1`,
	}...))
	differ(t, "aggnested.test 8.1", append(append([]string{}, t1Setup...), []string{
		`SELECT ( SELECT y FROM ( SELECT z AS y FROM (SELECT sum(x) AS z) AS t2 ) ) FROM t1`,
	}...))
}

// TestAggHoistAlreadyAggregateOwnerAdjacentShapes exercises the same gap through
// select-list and ORDER BY items (not just HAVING) of an ALREADY GROUP BY
// query, plus deeper nesting and a QUALIFIED column (the latter was already
// working -- the qualified branch has no compound-member gate at all -- and
// is pinned here as a control on the SAME shape).
func TestAggHoistAlreadyAggregateOwnerAdjacentShapes(t *testing.T) {
	gSetup := []string{
		`CREATE TABLE g(grp INTEGER, val INTEGER)`,
		`INSERT INTO g VALUES (1,10),(1,20),(2,30),(2,40)`,
	}
	differ(t, "select-list escape via FROM-item on an already GROUP BY query", append(append([]string{}, gSetup...), []string{
		`SELECT grp, (SELECT v FROM (SELECT sum(val) v)) FROM g GROUP BY grp ORDER BY grp`,
	}...))
	differ(t, "ORDER BY escape via FROM-item on an already GROUP BY query", append(append([]string{}, gSetup...), []string{
		`SELECT grp FROM g GROUP BY grp ORDER BY (SELECT v FROM (SELECT sum(val) v)) DESC`,
	}...))
	differ(t, "deep nesting: two layers of FROM-item wrapping in HAVING", append(append([]string{}, invoiceSetup...), []string{
		`SELECT sum(amount), name from invoice group by name having (select y > 6 from (select z as y from (select sum(amount) z)))`,
	}...))
	differ(t, "qualified column inside HAVING's FROM-item body (already-working control)", append(append([]string{}, invoiceSetup...), []string{
		`SELECT sum(amount), name from invoice group by name having (select v > 6 from (select sum(invoice.amount) v) t)`,
	}...))
	differ(t, "mixing this path with the already-merged compound-arm path in one item", append(append([]string{}, gSetup...), []string{
		`SELECT grp FROM g GROUP BY grp HAVING (SELECT v > 25 FROM (SELECT sum(val) AS v) UNION ALL SELECT avg(val))`,
	}...))
}

// TestAggHoistAlreadyAggregateOwnerNeverWrong pins the shapes this fix must NOT
// touch: a min/max escaping call still declines cleanly (never silently
// computed over the wrong scope -- see isMinMaxAggName's own doc comment for
// why min/max is excluded), a reference belonging to a level FURTHER OUT than
// the immediate HAVING owner is left alone rather than wrongly claimed, and a
// T with its OWN FROM supplying the column stays genuinely local.
func TestAggHoistAlreadyAggregateOwnerNeverWrong(t *testing.T) {
	differAllowingDeclines(t, "min/max through this path still declines, never wrongly computes", append(append([]string{}, invoiceSetup...), []string{
		`SELECT sum(amount), name from invoice group by name having (select v > 6 from (select max(amount) v) t)`,
	}...))
	differAllowingDeclines(t, "an escape two levels out (non-immediate owner) is not wrongly claimed", []string{
		`CREATE TABLE t0(val INTEGER)`,
		`CREATE TABLE t1(other INTEGER)`,
		`INSERT INTO t0 VALUES (100)`,
		`INSERT INTO t1 VALUES (1),(2)`,
		`SELECT * FROM t0 WHERE EXISTS (SELECT 1 FROM t1 GROUP BY other HAVING (SELECT v > 0 FROM (SELECT sum(val) v)))`,
	})
	differ(t, "T with its own FROM supplying the column stays local, unaffected", []string{
		`CREATE TABLE invoice2 (id INTEGER PRIMARY KEY, amount DOUBLE, name TEXT)`,
		`CREATE TABLE other(amount DOUBLE)`,
		`INSERT INTO invoice2 (amount, name) VALUES (4.0,'Michael'),(15.0,'Bara')`,
		`INSERT INTO other VALUES (1000)`,
		`SELECT sum(amount), name from invoice2 group by name having (select v > 6 from (select sum(amount) v from other) t)`,
	})
}
