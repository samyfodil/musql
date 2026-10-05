package compat

import "testing"

// Tests RETURNING lists with mixed subquery lifetimes. Some subqueries are
// evaluated once (frozen) and some per-row. Cache slot resets must be per-slot,
// not per-block, to support both in a single RETURNING list.
func TestReturningMixedSubqueryLifetimes(t *testing.T) {
	vw := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE s(x,y)`,
		`INSERT INTO s VALUES(1,'S1'),(2,'S2')`,
		`CREATE VIEW v AS SELECT a, b FROM t`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(NEW.a, NEW.b); END`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE t SET b = NEW.b WHERE a = OLD.a; END`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM t WHERE a = OLD.a; END`,
	}
	// The view shapes that mix a subquery NAMING THE VIEW (per row, because the
	// view IS the modified table) with one that does not (frozen) are still
	// declined: that classification has to come from the AST -- a subquery over
	// a view never opens the view, it opens the view's BASE tables -- so the
	// insn walk that answers everywhere else cannot see it, and forcing the
	// whole block per-row would be wrong for the frozen half.
	declines := map[string]bool{
		`INSERT INTO v VALUES(5,'m') RETURNING a, (SELECT count(*) FROM v), (SELECT count(*) FROM s)`:      true,
		`UPDATE v SET b='W' WHERE a=2 RETURNING a, (SELECT count(*) FROM v), (SELECT y FROM s WHERE x=2)`:  true,
		`DELETE FROM v WHERE a=1 RETURNING a, (SELECT count(*) FROM v), (SELECT count(*) FROM s)`:          true,
	}
	for _, q := range []string{
		`INSERT INTO v VALUES(3,'z') RETURNING a, (SELECT count(*) FROM t), (SELECT max(a) FROM t WHERE a <= v.a)`,
		`UPDATE v SET b='Q' WHERE a=1 RETURNING a, b, (SELECT count(*) FROM t), (SELECT b FROM t WHERE a = v.a)`,
		// Plain-table forms, multi-row, where per-row vs once genuinely differ.
		`INSERT INTO t VALUES(7,'p'),(8,'q') RETURNING a, (SELECT count(*) FROM s), (SELECT count(*) FROM t WHERE t.a <= 8)`,
		`INSERT INTO t VALUES(9,'r'),(10,'s') RETURNING a, (SELECT y FROM s WHERE x = 1), (SELECT count(*) FROM t)`,
		`UPDATE t SET b='U' WHERE a IN (1,2) RETURNING a, (SELECT count(*) FROM s), (SELECT count(*) FROM t WHERE a <= t.a)`,
		`DELETE FROM t WHERE a IN (7,8) RETURNING a, (SELECT count(*) FROM s), (SELECT count(*) FROM t)`,
		// INSERT ... SELECT, whose RETURNING block is a per-row site over the
		// source scan rather than an unrolled VALUES list.
		`INSERT INTO t SELECT x+20, y FROM s RETURNING a, (SELECT count(*) FROM s), (SELECT count(*) FROM t)`,
		`INSERT INTO t SELECT x+30, y FROM s RETURNING a, (SELECT y FROM s WHERE x=1), (SELECT count(*) FROM t WHERE a <= t.a)`,
		// A view RETURNING mixing a subquery that NAMES the view with one that
		// does not -- vdbe_view_write.go's own mixed decline.
		`INSERT INTO v VALUES(5,'m') RETURNING a, (SELECT count(*) FROM v), (SELECT count(*) FROM s)`,
		`UPDATE v SET b='W' WHERE a=2 RETURNING a, (SELECT count(*) FROM v), (SELECT y FROM s WHERE x=2)`,
		`DELETE FROM v WHERE a=1 RETURNING a, (SELECT count(*) FROM v), (SELECT count(*) FROM s)`,
	} {
		c, m := boundPair(t, vw)
		cv, mv := renderQuery(c, q), renderQuery(m, q)
		if cv == "ERR" {
			t.Errorf("the oracle refused %q -- the case no longer measures anything", q)
			continue
		}
		if declines[q] {
			if mv != "ERR" {
				t.Errorf("%s: served now (%s) -- if the view-naming half is classified from the AST, move it to the served set", q, mv)
			}
			continue
		}
		if cv != mv {
			t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
		}
	}
}
