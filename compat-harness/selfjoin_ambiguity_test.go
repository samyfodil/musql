// Tests unaliased self-joins with USING clauses. These coalesce some columns and
// expose others under the same table name, creating potential ambiguity for
// unqualified references.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

var sjaSetup = []string{
	`CREATE TABLE t1(x,y)`,
	`INSERT INTO t1 VALUES(1,'a'),(2,'b')`,
	`CREATE TABLE t2(x,z)`,
	`INSERT INTO t2 VALUES(1,'p')`,
	`CREATE TABLE t3(x,y,w)`,
	`INSERT INTO t3 VALUES(1,'a','q')`,
}

func TestSelfJoinAmbiguity(t *testing.T) {
	for _, q := range []string{
		// answers: the exposed column is never named
		`SELECT 123 FROM t1 JOIN t1 USING (x)`,
		`SELECT x FROM t1 JOIN t1 USING (x)`,
		`SELECT t1.x FROM t1 JOIN t1 USING (x)`,
		`SELECT count(*) FROM t1 JOIN t1 USING (x)`,
		`SELECT x+1 FROM t1 JOIN t1 USING (x) ORDER BY x`,
		`SELECT (SELECT count(*) FROM t2 WHERE t2.x=1) FROM t1 JOIN t1 USING (x)`,
		`SELECT 1 FROM t1 JOIN t1 USING (x) JOIN t1 USING (x)`,
		`SELECT 456 FROM t1 JOIN t1 USING (x,x)`,
		// errors: the exposed column IS named, in every position
		`SELECT y FROM t1 JOIN t1 USING (x)`,
		`SELECT t1.y FROM t1 JOIN t1 USING (x)`,
		`SELECT * FROM t1 JOIN t1 USING (x)`,
		`SELECT t1.* FROM t1 JOIN t1 USING (x)`,
		`SELECT 1 FROM t1 JOIN t1 USING (x) WHERE y='a'`,
		`SELECT x FROM t1 JOIN t1 USING (x) ORDER BY y`,
		`SELECT x FROM t1 JOIN t1 USING (x) GROUP BY y`,
		`SELECT max(y) FROM t1 JOIN t1 USING (x)`,
		// nothing left exposed -> answers
		`SELECT * FROM t1 NATURAL JOIN t1`,
		`SELECT y FROM t1 NATURAL JOIN t1`,
		`SELECT * FROM t1 JOIN t1 USING (x,y)`,
		`SELECT y FROM t1 JOIN t1 USING (y,y)`,
		`SELECT count(*) FROM t1 NATURAL JOIN t1`,
		// ALIASED: two names, so nothing is shared at all
		`SELECT p.y FROM t1 p JOIN t1 q USING (x)`,
		`SELECT * FROM t1 p JOIN t1 q USING (x)`,
		`SELECT p.* FROM t1 p JOIN t1 q USING (x)`,
		// a self-join alongside a DIFFERENT table. Only the leading spelling
		// is here: "t2 JOIN t1 ON ... JOIN t1 USING (x)" is a SEPARATE,
		// pre-existing decline -- the USING desugar cannot choose a
		// representative for x when two visible items are both named t1
		// ("ambiguous column name in NATURAL/USING join: x") -- and that is
		// its own rule, not this one.
		`SELECT z FROM t1 JOIN t1 USING (x) JOIN t2 ON t2.x=t1.x`,
		// three columns, only two coalesced
		`SELECT w FROM t3 JOIN t3 USING (x,y)`,
		`SELECT * FROM t3 JOIN t3 USING (x,y)`,
		`SELECT * FROM t3 JOIN t3 USING (x,y,w)`,
	} {
		differ(t, q, append(append([]string{}, sjaSetup...), q))
	}
}

// TestSelfJoinAmbiguityFuzz randomizes which columns the USING list coalesces
// and which the statement names, because the rule is a per-column
// intersection: exactly the columns NOT coalesced stay ambiguous, and only a
// reference to one of those is an error.
func TestSelfJoinAmbiguityFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260803))
	cols := []string{"a", "b", "c"}
	for i := 0; i < 90; i++ {
		i := i
		t.Run(fmt.Sprintf("j%02d", i), func(t *testing.T) {
			stmts := []string{
				`CREATE TABLE tt(a,b,c)`,
				`INSERT INTO tt VALUES(1,'p','x'),(2,'q','y')`,
			}
			// a random non-empty USING list, sometimes with a repeat
			var using []string
			for _, c := range cols {
				if rng.Intn(2) == 0 {
					using = append(using, c)
				}
			}
			if len(using) == 0 {
				using = []string{cols[rng.Intn(len(cols))]}
			}
			if rng.Intn(4) == 0 {
				using = append(using, using[0]) // USING(a,a)
			}
			join := fmt.Sprintf(`tt JOIN tt USING (%s)`, strings.Join(using, ","))
			// a random projection: a literal, a coalesced column, an exposed
			// one, or a star
			// "tt.*" is excluded: a qualified star whose name matches BOTH
			// items expands both in C SQLite (six columns for a 3-column
			// table) and is declined here -- see expandSelectList. That is a
			// separate rule from this one.
			// "tt.*" is excluded and pinned separately: a qualified star whose
			// name matches BOTH items expands both in C SQLite (six columns
			// for a 3-column table) and is DECLINED here -- see
			// TestSelfJoinQualifiedStarDeclined.
			sel := []string{"1", "count(*)", "*"}
			sel = append(sel, cols...)
			for _, c := range cols {
				sel = append(sel, "tt."+c)
			}
			pick := sel[rng.Intn(len(sel))]
			stmts = append(stmts,
				fmt.Sprintf(`SELECT %s FROM %s`, pick, join),
				fmt.Sprintf(`SELECT %s FROM %s ORDER BY 1`, pick, join),
				fmt.Sprintf(`SELECT count(*) FROM %s`, join))
			differ(t, fmt.Sprintf("j%02d SELECT %s FROM %s", i, pick, join), stmts)
		})
	}
}

// TestSelfJoinQualifiedStarDeclined pins the one shape this rule answers with
// a DECLINE rather than a value: a qualified star whose name matches more than
// one FROM item. C SQLite expands EVERY match --
//
//	CREATE TABLE tt(a,b,c);
//	SELECT tt.* FROM tt JOIN tt USING (a,b,c)   -> a,b,c,a,b,c  (SIX columns)
//
// -- while findTableScope resolves to the first alone, which would answer
// three. Half-expanding is a wrong answer, not a caught one, so it declines.
// The oracle half is asserted too: if SQLite ever stopped answering this, the
// decline would become correct and the case should be deleted, not "fixed".
func TestSelfJoinQualifiedStarDeclined(t *testing.T) {
	for _, q := range []string{
		// Only the shapes where EVERYTHING is coalesced, so nothing is
		// ambiguous and the oracle really does answer. "USING (a)" is NOT one
		// of them -- b and c stay exposed and C SQLite errors on those,
		// which this gate's own oracle assertion caught.
		`SELECT tt.* FROM tt JOIN tt USING (a,b,c)`,
		`SELECT tt.* FROM tt NATURAL JOIN tt`,
	} {
		stmts := []string{
			`CREATE TABLE tt(a,b,c)`,
			`INSERT INTO tt VALUES(1,'p','x')`,
			q,
		}
		if res := run(t, "cgo", stmts); res[len(res)-1]["kind"] != "rows" {
			t.Errorf("oracle no longer answers %q -- this decline may now be correct", q)
		}
		if res := run(t, "musql", stmts); res[len(res)-1]["kind"] != "error" {
			t.Errorf("%q now ANSWERS in musql (%v) -- it must expand EVERY matching FROM item, "+
				"not just the first, before this case is deleted", q, res[len(res)-1])
		}
	}
}
