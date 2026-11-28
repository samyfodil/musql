package compat

import "testing"

// TestWindowIntRealColumnParity holds a window query's reads of a VIRTUAL
// generated column of REAL affinity, whose integral values are MEM_IntReal, to
// C's answers. The projection reads a column through the window sub-select's
// result column, with that column's affinity, so a REAL column is a REAL there
// (b/4 is 0.25, not 0), while a derived table's column that is not REAL (a
// coalesce() or "+" of it) reads back as the integer a record holds. A window
// function's operand holding such a value declines
// (engine.TestWindowIntRealOperandDeclines).
func TestWindowIntRealColumnParity(t *testing.T) {
	driverParity(t, "window-intreal", []string{
		`CREATE TABLE g(k INTEGER PRIMARY KEY, a INT, b REAL GENERATED ALWAYS AS (a) VIRTUAL, p INT)`,
		`INSERT INTO g(k,a,p) VALUES (1,1,1),(2,2,1),(3,3,2),(4,NULL,2),(5,2.5,2)`,
		`CREATE VIEW v AS SELECT k, coalesce(b, 0) x, b y FROM g`,
		`SELECT b, b/4, typeof(b), count(*) OVER () FROM g ORDER BY k`,
		`SELECT k, typeof(b), quote(b), row_number() OVER (ORDER BY k) FROM g ORDER BY k`,
		`SELECT quote(coalesce(b, 0)), quote(+b), quote(b + 0), count(*) OVER () FROM g ORDER BY k`,
		`SELECT quote(b), row_number() OVER (ORDER BY k) FROM g ORDER BY b DESC`,
		`SELECT quote(b), quote(sum(a) OVER (PARTITION BY p ORDER BY k)) FROM g ORDER BY k`,
		`SELECT quote(b), (SELECT quote(b)), count(*) OVER () FROM g ORDER BY k`,
		`SELECT quote(x) FROM (SELECT k, b x, count(*) OVER () FROM g) ORDER BY k`,
		`SELECT quote(x), count(*) OVER () FROM (SELECT k, coalesce(b, 0) x FROM g) ORDER BY k`,
		`SELECT quote(x), count(*) OVER () FROM (SELECT k, b x FROM g) ORDER BY k`,
		`SELECT quote(x), count(*) OVER () FROM (SELECT k, +b x FROM g) ORDER BY k`,
		`SELECT quote(x), quote(y), count(*) OVER () FROM v ORDER BY k`,
		`WITH c AS (SELECT k, coalesce(b, 0) x FROM g) SELECT quote(x), count(*) OVER () FROM c ORDER BY k`,
		`WITH c AS MATERIALIZED (SELECT k, coalesce(b, 0) x FROM g) SELECT quote(x), count(*) OVER () FROM c ORDER BY k`,
		`SELECT quote(b), count(*) OVER () FROM g UNION ALL SELECT 9, 9`,
		`SELECT quote(sum(b)), count(*) OVER () FROM g`,
	})
}
