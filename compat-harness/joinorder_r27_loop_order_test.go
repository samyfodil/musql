package compat

// Tests nested-loop join order, which is determined by cost modeling and
// restriction hoisting. Tests use group_concat or LIMIT to observe the binding
// order without relying on index selection.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestR27JoinLoopOrder(t *testing.T) {
	two := []string{
		"CREATE TABLE a(x)",
		"INSERT INTO a VALUES(1),(2)",
		"CREATE TABLE b(y)",
		"INSERT INTO b VALUES(10),(20),(30)",
	}
	three := []string{
		"CREATE TABLE t1(p)",
		"INSERT INTO t1 VALUES('A'),('B')",
		"CREATE TABLE t2(q)",
		"INSERT INTO t2 VALUES('Q'),('R')",
		"CREATE TABLE t3(e)",
		"INSERT INTO t3 VALUES(1),(2)",
	}
	for _, c := range []struct {
		name  string
		setup []string
		query string
	}{
		// A single-table restriction hoists ITS table into the outer loop.
		{"hoistOuter", two,
			"SELECT group_concat(a.x||'-'||b.y) FROM a,b WHERE b.y>5"},
		{"hoistOuterMirror", two,
			"SELECT group_concat(a.x||'-'||b.y) FROM a,b WHERE a.x>0"},
		{"hoistReversedFrom", two,
			"SELECT group_concat(a.x||'-'||b.y) FROM b,a WHERE b.y>5"},
		// Same hoist, read through a LIMIT: this one changes the ROW SET, not
		// merely the order.
		{"limitRowSet", two,
			"SELECT a.x, b.y FROM a,b WHERE b.y>5 LIMIT 3"},
		// Control: with nothing to hoist, SQLite keeps FROM order. The
		// four-way cost tie falls through to whereLoopIsNoBetter, which cannot
		// tell the two paths apart and so keeps the incumbent.
		{"noFilter", two,
			"SELECT group_concat(a.x||'-'||b.y) FROM a,b"},
		// Control: CROSS is a reorder barrier, so the same restriction that
		// hoists b above a with a comma may not do it here. musql's AST used
		// to be unable to tell the two apart at all (JoinCross is JoinKind's
		// zero value); FromItem.CrossKeyword is what makes this case possible.
		{"crossBarrier", two,
			"SELECT group_concat(a.x||'-'||b.y) FROM a CROSS JOIN b WHERE b.y>5"},
		{"crossBarrier3", three,
			"SELECT group_concat(t1.p||t2.q||t3.e) FROM t1 CROSS JOIN t2 CROSS JOIN t3 WHERE t3.e>0"},
		// The prerequisite accumulator is a SNAPSHOT, not a prefix cut: t3
		// inherits the mask taken AT the CROSS -- t1 only -- so it may be
		// bound above the CROSS's own right operand. SQLite runs t1, t3, t2.
		{"maskSnapshot", three,
			"SELECT group_concat(t1.p||t2.q||t3.e) FROM t1 CROSS JOIN t2, t3 WHERE t3.e>0"},
		{"plain3", three,
			"SELECT group_concat(t1.p||t2.q||t3.e) FROM t1,t2,t3 WHERE t3.e>0"},
		{"plain3Middle", three,
			"SELECT group_concat(t1.p||t2.q||t3.e) FROM t1,t2,t3 WHERE t2.q>''"},
		// A LEFT JOIN pins its own target after everything to its left, but
		// pins nothing about an item to its RIGHT: t3 carries the restriction
		// and is free to drive the outermost loop.
		{"leftJoinHoist", three,
			"SELECT group_concat(t1.p||coalesce(t2.q,'-')||t3.e) FROM t1 LEFT JOIN t2 ON t2.q>'' , t3 WHERE t3.e>0"},
		{"leftJoinPinned", three,
			"SELECT group_concat(t1.p||coalesce(t2.q,'-')) FROM t1 LEFT JOIN t2 ON t2.q>'' WHERE t1.p>''"},
	} {
		t.Run(c.name, func(t *testing.T) {
			differ(t, "r27-"+c.name, append(append([]string{}, c.setup...), c.query))
		})
	}
}
