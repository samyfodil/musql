package compat

// Tests parenthesized join group handling against the Oracle.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// r26Pair opens engine and oracle databases and returns a compare function.
func r26Pair(t *testing.T, setup []string) func(q string) (declined, agrees bool, oracleErr error, detail string) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	return func(q string) (bool, bool, error, string) {
		cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)
		eCols, eRows, eErr := p.QueryArgs(q, nil)
		if eErr != nil {
			return true, false, cErr, eErr.Error()
		}
		if cErr != nil {
			return false, false, cErr, "the engine ANSWERED a statement the oracle rejects"
		}
		ok, why := queryResultsMatch(eCols, engineRowsToStrings(eRows), cCols, cRows,
			strings.Contains(strings.ToUpper(q), "ORDER BY"))
		return false, ok, nil, why
	}
}

// r26RebuildSchema deliberately puts the shared column SECOND in ta, which is
// what makes SQLite's front-move observable. 389f765's own revert message
// records why that matters: r25GroupSchema and paren_join_group_star_test.go's
// fixture both declare it FIRST, which makes the move a no-op and hid the bug
// from a 540-statement sweep.
var r26RebuildSchema = []string{
	`CREATE TABLE ta(a,b)`, `CREATE TABLE td(b,g)`, `CREATE TABLE te(e,f)`, `CREATE TABLE tc(z)`,
	`CREATE TABLE kf(k,x)`, `CREATE TABLE kg(k,y)`,
	`CREATE TABLE o1(x,y)`, `CREATE TABLE o2(x,p)`, `CREATE TABLE o3(y,r)`, `CREATE TABLE anchor(zz)`,
	`CREATE TABLE zc(m,n)`, `CREATE TABLE zb(q,_ROWID_)`,
	`INSERT INTO ta VALUES(1,2),(3,4)`,
	`INSERT INTO td VALUES(2,'x'),(4,'y')`,
	`INSERT INTO te VALUES(1,200),(3,300)`,
	`INSERT INTO tc VALUES('p')`,
	`INSERT INTO kf VALUES(1,'x1')`,
	`INSERT INTO kg VALUES(1,'y1')`,
	`INSERT INTO o1 VALUES(1,10)`, `INSERT INTO o2 VALUES(1,'P')`, `INSERT INTO o3 VALUES(10,'R')`,
	`INSERT INTO anchor VALUES('A')`,
	`INSERT INTO zc VALUES(5,6)`, `INSERT INTO zb VALUES('Q',77)`,
}

// TestR26RebuiltGroupDeclines pins the shapes checkRebuiltJoinGroups must
// decline. Each was a live wrong answer at 389f765; the comment on each line
// is what the oracle gives, and the test re-derives that rather than trusting
// it -- a shape the oracle no longer answers (or now errors on) fails here
// with a message saying so.
//
// Every front-move case below names a "*": the rebuilt list's ORDER is only
// observable through one (FromItem.GroupRebuildStarred), so that is what the
// decline is now keyed on. "SELECT ta.b FROM tc, (ta JOIN td USING(b))" used
// to be listed here and is now SERVED -- it agrees cell for cell and differs
// only in SQLite's ":N" auto-name for it ("b:1"), the same
// documented-as-unspecified naming this engine already answers plainly for a
// rebuilt group that merely repeats a name. It is asserted, with that one
// relaxation, in joinnarrow_r26_observability_test.go.
func TestR26RebuiltGroupDeclines(t *testing.T) {
	cmp := r26Pair(t, r26RebuildSchema)
	for _, tc := range []struct {
		q          string
		oracleErrs bool
	}{
		// The front-move: a NON-LEADING unaliased group. cols z,b,a,g here,
		// z,a,b,g in FROM order -- wrong cells, not just names.
		{`SELECT * FROM tc, (ta JOIN td USING(b))`, false},
		{`SELECT * FROM tc CROSS JOIN (ta JOIN td USING(b))`, false},
		{`SELECT * FROM tc JOIN (ta JOIN td USING(b)) ON 1`, false},
		{`SELECT * FROM tc, (ta NATURAL JOIN td)`, false},
		{`SELECT * FROM tc, (ta LEFT JOIN td USING(b))`, false},
		{`SELECT * FROM tc, (ta FULL JOIN td USING(b))`, false},
		// The empty-alias hole: SQLite tests the alias TOKEN's LENGTH, so all
		// three empty spellings rebuild exactly as a named alias does.
		{`SELECT * FROM (ta JOIN td USING(b)) AS ""`, false},
		{"SELECT * FROM (ta JOIN td USING(b)) AS ``", false},
		{`SELECT * FROM (ta JOIN td USING(b)) AS []`, false},
		{`SELECT * FROM (ta JOIN td USING(b)) AS g`, false},
		// Two DISTINCT coalesced names: six oracle columns (zz,x,y,y:1,p,r)
		// against this engine's five. A whole column, not a name.
		{`SELECT * FROM anchor, (o1 LEFT JOIN o2 USING(x) FULL JOIN o3 USING(y))`, false},
		// An ALIASED group repeating a column name is an ERROR on the oracle.
		{`SELECT * FROM (ta JOIN td ON ta.b=td.b) AS g`, true},
		// ...and one declaring _ROWID_ collides with the group's own invisible
		// rowid alias, which the oracle then cannot resolve.
		{`SELECT * FROM (zc JOIN zb ON 1) AS g`, true},
		{`SELECT * FROM (zc JOIN zb ON 1) AS ""`, true},
		// "SELECT * FROM (kf JOIN kg USING(k)) AS g" USED to be here as a KNOWN
		// OVER-DECLINE: rule (B)'s narrowing (vdbe_join_codegen.go), landed
		// after round 28's measurement below, now serves it -- see
		// TestR28NNestedFromBareStar and TestR26RebuiltGroupCoalescedAnswers.
		//
		// Round 28's own measurement, kept because the SECOND correction it
		// made (qualified stars) is still what keeps every OTHER line in this
		// list declined: an aliased group whose repeated name is a USING/
		// NATURAL COALESCED one is not ambiguous on the oracle at all when only
		// a BARE "*" (or nothing at all) touches the group -- it answers k,x,y,
		// exactly the order this engine emits, because the coalesced k already
		// leads the group. What stays blocked is a QUALIFIED star naming one of
		// the group's own members or its alias ("f1.*", "gq.*"): unlike a bare
		// one, that BYPASSES select.c's COLFLAG_NOEXPAND suppression and reaches
		// the SUPPRESSED, ":N"-named copy this engine does not reproduce --
		// verified directly, the oracle itself REFUSES "SELECT f1.* FROM (f1
		// JOIN f2 USING(b)) AS gq" ("no such column: b:1"), so declining it here
		// agrees with the oracle's own outcome. FromItem.GroupRebuildQualStarred
		// is what checkOneRebuiltGroup keys this distinction on now.
		// TestR28NNestedFromQualifiedStar pins both directions.
	} {
		declined, _, oracleErr, detail := cmp(tc.q)
		if (oracleErr != nil) != tc.oracleErrs {
			t.Errorf("[%s] the ORACLE changed: oracleErrs=%v, want %v (err=%v) -- re-measure before trusting the decline",
				tc.q, oracleErr != nil, tc.oracleErrs, oracleErr)
			continue
		}
		if !declined {
			t.Errorf("[%s] the engine ANSWERED it; SQLite rebuilds this group's column list and this engine emits FROM order (%s)", tc.q, detail)
		}
	}
}

// TestR26RebuiltGroupCoalescedAnswers pins the shapes checkOneRebuiltGroup's
// narrowing (vdbe_join_codegen.go, "(1)"/"(2)" in its own doc comment) newly
// serves: an ALIASED, rebuilt group whose only repeated name is its own
// USING/NATURAL-coalesced one, read either through a plain qualified
// reference (no star touches the group at all) or through a BARE "*" whose
// coalesced column already leads the group (so no front-move is needed
// either). Every line was a live decline before that narrowing landed;
// each is checked for exact cell agreement against the real oracle, not just
// "answers something".
func TestR26RebuiltGroupCoalescedAnswers(t *testing.T) {
	cmp := r26Pair(t, r26RebuildSchema)
	for _, q := range []string{
		// No star at all touches the group -- the census is skipped outright.
		// "x" is not even the coalesced name; "g.k" through the coalesced name
		// itself also answers.
		`SELECT g.x FROM (kf JOIN kg USING(k)) AS g`,
		`SELECT g.k FROM (kf JOIN kg USING(k)) AS g`,
		`SELECT g.x, g.k, g.y FROM (kf JOIN kg USING(k)) AS g`,
		// A bare "*", with the coalesced name (k) already leading the group
		// (kf's own first column) -- the front-move is a no-op, so the
		// COLFLAG_NOEXPAND-suppressed raw copies collapsing to one visible
		// "k" is exactly this engine's own flat per-member emission.
		`SELECT * FROM (kf JOIN kg USING(k)) AS g`,
		`SELECT * FROM (kf LEFT JOIN kg USING(k)) AS g`,
		`SELECT * FROM (kf JOIN kg USING(k)) AS ""`,
		// The joinH.test#12/14 shape this narrowing was written for: THREE
		// members chained by two NATURAL joins, all sharing the SAME single
		// column name (select.c:547's tableAndColumnIndex scans every earlier
		// source, so the SECOND NATURAL also coalesces against the group's
		// first member, not just its immediate left neighbor).
		`SELECT * FROM (kf NATURAL JOIN kg) AS g`,
	} {
		declined, agrees, oracleErr, detail := cmp(q)
		if oracleErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure before trusting this gate", q, oracleErr)
			continue
		}
		if declined {
			t.Errorf("[%s] the engine DECLINED a shape the narrowing should now serve", q)
			continue
		}
		if !agrees {
			t.Errorf("[%s] answered but DIVERGED from the oracle: %s", q, detail)
		}
	}
}

// TestR26RebuiltGroupStillServed is the other half of the net: the shapes a
// rebuild does NOT change, which must keep answering. This engine's flat FROM
// order IS SQLite's rebuilt order whenever the group coalesces nothing, or
// coalesces exactly one name that already leads the group.
func TestR26RebuiltGroupStillServed(t *testing.T) {
	cmp := r26Pair(t, r26RebuildSchema)
	for _, q := range []string{
		// Leading and unaliased: SQLite splices these in flat, no rebuild.
		`SELECT * FROM (ta JOIN td USING(b))`,
		`SELECT * FROM (ta NATURAL JOIN td)`,
		`SELECT * FROM (ta JOIN td USING(b)), tc`,
		`SELECT * FROM ((ta JOIN td USING(b)) JOIN tc ON 1)`,
		// Rebuilt, but coalescing nothing.
		`SELECT * FROM tc, (ta JOIN te ON 1)`,
		`SELECT * FROM tc, (ta JOIN te ON 1) AS gg`,
		`SELECT * FROM (ta JOIN te ON 1) AS g`,
		`SELECT * FROM (ta JOIN te ON 1) AS ""`,
		`SELECT ta.a FROM tc, (ta JOIN te ON 1)`,
		// Rebuilt, coalescing ONE name that already leads the group -- the
		// front-move is a no-op, and declining these is what cost 321
		// TestTCLCorpus statements on this round's first attempt.
		`SELECT * FROM tc, (kf JOIN kg USING(k))`,
		`SELECT * FROM tc, (kf LEFT JOIN kg USING(k))`,
		// The group's OWN connector USING coalesces outside the group, which
		// the rebuild never sees.
		`SELECT * FROM ta JOIN (td JOIN te ON 1) USING(b)`,
		`SELECT * FROM ta LEFT JOIN (td JOIN te ON 1) USING(b)`,
	} {
		declined, agrees, oracleErr, detail := cmp(q)
		if oracleErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure", q, oracleErr)
			continue
		}
		if declined {
			t.Errorf("[%s] the engine now DECLINES a shape it answered correctly: %s", q, detail)
			continue
		}
		if !agrees {
			t.Errorf("[%s] answered but diverged: %s", q, detail)
		}
	}
}

// r26ChainSchema is joinC.test's own fixture. The table ORDER the sweeps below
// use (t2 outermost, then t1, then t4/t5) is deliberately NOT joinC's: with
// joinC's order every one of these combinations happens to match, which is why
// 186 of its statements score pass today while the same shapes are wrong on a
// permutation of the identical rows.
var r26ChainSchema = []string{
	`CREATE TABLE t1(a INT, b INT, c INT)`,
	`CREATE TABLE t2(a INT, b INT, d INT)`,
	`CREATE TABLE t4(a INT, b INT, f INT)`,
	`CREATE TABLE t5(a INT, b INT, g INT)`,
	`INSERT INTO t1 VALUES(11,21,31),(12,22,32),(15,25,35),(17,27,37)`,
	`INSERT INTO t2 VALUES(12,22,32),(13,23,33),(15,25,35),(18,28,38)`,
	`INSERT INTO t4 VALUES(11,21,31),(13,23,33),(15,25,35),(19,29,39)`,
	`INSERT INTO t5 SELECT * FROM t4 WHERE a>=15`,
}

var r26JoinOps = []string{"INNER JOIN", "LEFT JOIN", "RIGHT JOIN", "FULL JOIN"}

// TestR26CoalesceChainBoundary sweeps every operator combination of the three
// shapes round 26's checkGroupCoalesceChain guarded. That guard is GONE (round
// 27 ports the coalesce chain itself -- see joink_r27_coalesce_chain_test.go
// for the rule and the select.c/resolve.c citation), so all 96 must now be
// ANSWERED and must MATCH: a decline here is a regression in the other
// direction and fails just as loudly as a divergence.
//
// What each shape used to do, measured at 389f765 before the guard existed:
//
//	group on the LEFT:  "(t4 o1 t5 USING(a)) o2 t1 USING(a)"
//	    6 of 16 diverged -- o1 in {LEFT,RIGHT,FULL} AND o2 in {RIGHT,FULL}
//	group on the RIGHT: "t2 o2 (t4 o1 t5 USING(a)) USING(a)"
//	    0 of 16 diverged -- one level down was already correct
//	group on the RIGHT, two levels:
//	    "t2 o1 (t1 o2 (t4 o3 t5 USING(a)) USING(a)) USING(a)"
//	    12 of 64 diverged
func TestR26CoalesceChainBoundary(t *testing.T) {
	cmp := r26Pair(t, r26ChainSchema)
	sweep := func(name string, _ int, gen func(emit func(string))) {
		var qs []string
		gen(func(q string) { qs = append(qs, q) })
		for _, q := range qs {
			isDeclined, agrees, oracleErr, detail := cmp(q)
			if oracleErr != nil {
				t.Errorf("[%s] the ORACLE now rejects %q: %v", name, q, oracleErr)
				continue
			}
			if isDeclined {
				t.Errorf("[%s] DECLINED a shape this engine serves: %s\n  %s", name, q, detail)
				continue
			}
			if !agrees {
				t.Errorf("[%s] WRONG (answered and diverged): %s\n  %s", name, q, detail)
			}
		}
	}

	sweep("group on the LEFT", 6, func(emit func(string)) {
		for _, o1 := range r26JoinOps {
			for _, o2 := range r26JoinOps {
				emit(fmt.Sprintf("SELECT a FROM (t4 %s t5 USING(a)) %s t1 USING(a) ORDER BY 1", o1, o2))
			}
		}
	})
	sweep("group on the RIGHT, one level", 0, func(emit func(string)) {
		for _, o1 := range r26JoinOps {
			for _, o2 := range r26JoinOps {
				emit(fmt.Sprintf("SELECT a FROM t2 %s (t4 %s t5 USING(a)) USING(a) ORDER BY 1", o2, o1))
			}
		}
	})
	sweep("group on the RIGHT, two levels", 12, func(emit func(string)) {
		for _, o1 := range r26JoinOps {
			for _, o2 := range r26JoinOps {
				for _, o3 := range r26JoinOps {
					emit(fmt.Sprintf("SELECT a FROM t2 %s (t1 %s (t4 %s t5 USING(a)) USING(a)) USING(a) ORDER BY 1", o1, o2, o3))
				}
			}
		}
	})
}

// TestR26Join9NestedFullJoinUsing pins join9.test's own three cases, over
// join9.test's own schema-1 tables. All three answered "id NULL" for the rows
// only one side supplies at 389f765, where SQLite gives 0 and 9; round 26
// declined them; round 27 ANSWERS all three, so this gate now asserts the
// answer instead of the decline (joink_r27_coalesce_chain_test.go carries the
// rule and the source citation).
func TestR26Join9NestedFullJoinUsing(t *testing.T) {
	cmp := r26Pair(t, []string{
		`CREATE TABLE t3(id INTEGER PRIMARY KEY, w TEXT)`,
		`CREATE TABLE t4(id INTEGER PRIMARY KEY, x TEXT)`,
		`CREATE TABLE t5(id INTEGER PRIMARY KEY, y TEXT)`,
		`CREATE TABLE t6(id INTEGER PRIMARY KEY, z INT)`,
		`INSERT INTO t3(id,w) VALUES(2,'two'),(3,'three'),(6,'six'),(7,'seven')`,
		`INSERT INTO t4(id,x) VALUES(2,'alice'),(4,'bob'),(6,'cindy'),(8,'dave')`,
		`INSERT INTO t5(id,y) VALUES(1,'red'),(2,'orange'),(3,'yellow'),(4,'green'),(5,'blue')`,
		`INSERT INTO t6(id,z) VALUES(3,333),(4,444),(5,555),(0,1000),(9,999)`,
	})
	for _, q := range []string{
		// join9-$id.900 / .910 / .920, verbatim modulo whitespace -- the two-
		// level chains that were the whole point of the round-26 decline.
		`SELECT * FROM (t3 NATURAL FULL JOIN t4) NATURAL FULL JOIN (t5 NATURAL FULL JOIN t6) ORDER BY 1`,
		`SELECT * FROM t3 NATURAL FULL JOIN (t4 NATURAL FULL JOIN (t5 NATURAL FULL JOIN t6)) ORDER BY 1`,
		`SELECT * FROM t3 FULL JOIN (t4 FULL JOIN (t5 FULL JOIN t6 USING(id)) USING(id)) USING(id) ORDER BY 1`,
		// ONE level of grouping was already correct and must keep answering.
		`SELECT * FROM t3 FULL JOIN (t4 FULL JOIN t5 USING(id)) USING(id) ORDER BY 1`,
		`SELECT * FROM t3 NATURAL FULL JOIN (t4 NATURAL FULL JOIN t5) ORDER BY 1`,
		`SELECT * FROM t4 INNER JOIN (t5 FULL JOIN t6 USING(id)) USING(id) ORDER BY 1`,
	} {
		declined, agrees, oracleErr, detail := cmp(q)
		if oracleErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v", q, oracleErr)
			continue
		}
		if declined {
			t.Errorf("[%s] the engine now DECLINES a shape it answered correctly: %s", q, detail)
			continue
		}
		if !agrees {
			t.Errorf("[%s] answered but diverged: %s", q, detail)
		}
	}
}
