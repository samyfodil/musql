package compat

// Round-26 gate for checkRebuiltJoinGroups observability: SQLite rebuilds a
// parenthesized join group's column list when not leading or aliased; the
// rebuild moves USING/NATURAL-coalesced columns to front. Only reaches a result
// through "*" that expands over the group.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// r26AutoNameRE matches SQLite's ":N" uniquifier for rebuilt group column names.
var r26AutoNameRE = regexp.MustCompile(`:[0-9]+$`)

// r26NamesAgree compares result-column name lists, treating oracle's ":N" suffix as unspecified.
func r26NamesAgree(eCols, cCols []string) (bool, string) {
	if len(eCols) != len(cCols) {
		return false, fmt.Sprintf("column count: engine=%d cgo=%d", len(eCols), len(cCols))
	}
	for i := range cCols {
		if r26AutoNameRE.MatchString(cCols[i]) {
			continue
		}
		if eCols[i] != cCols[i] {
			return false, fmt.Sprintf("column %d name: engine=%q cgo=%q", i, eCols[i], cCols[i])
		}
	}
	return true, ""
}

// r26ObsPair is r26Pair's sibling that hands back both sides raw, so a caller
// can apply r26NamesAgree instead of an exact name comparison.
func r26ObsPair(t *testing.T, setup []string) func(q string) (eCols []string, eRows [][]string, eErr error, cCols []string, cRows [][]string, cErr error) {
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
	return func(q string) ([]string, [][]string, error, []string, [][]string, error) {
		cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)
		eCols, eVals, eErr := p.QueryArgs(q, nil)
		return eCols, engineRowsToStrings(eVals), eErr, cCols, cRows, cErr
	}
}

// r26ObsSchema: the shared column b sits SECOND in ta and FIRST in td, so the
// front-move is real for a group led by ta and a no-op for one led by td --
// both directions are exercised below. tb3 gives a third member; e1/e2 share
// nothing, so their group coalesces nothing at all.
var r26ObsSchema = []string{
	`CREATE TABLE ta(a,b)`, `CREATE TABLE td(b,g)`, `CREATE TABLE tb3(b,h)`,
	`CREATE TABLE tc(z)`,
	`CREATE TABLE wa(p,b,q)`, `CREATE TABLE wd(r,b,s)`,
	`CREATE TABLE e1(m)`, `CREATE TABLE e2(n)`,
	`INSERT INTO ta VALUES(1,2),(3,4),(5,NULL)`,
	`INSERT INTO td VALUES(2,'x'),(4,'y'),(6,'w')`,
	`INSERT INTO tb3 VALUES(2,'h2'),(6,'h6')`,
	`INSERT INTO tc VALUES('p')`,
	`INSERT INTO wa VALUES('p1',2,'q1'),('p2',4,'q2')`,
	`INSERT INTO wd VALUES('r1',2,'s1'),('r2',6,'s2')`,
	`INSERT INTO e1 VALUES('m1')`, `INSERT INTO e2 VALUES('n1')`,
}

// TestR26RebuiltListNeedsAStar is the narrowing's own gate. Everything in
// starred must DECLINE; everything in unstarred must ANSWER and AGREE. At
// 15ce537 every unstarred line here declined, which is what this gate was
// written to move.
func TestR26RebuiltListNeedsAStar(t *testing.T) {
	cmp := r26ObsPair(t, r26ObsSchema)

	starred := []string{
		// The front-move, seen through a bare "*".
		`SELECT * FROM tc, (ta JOIN td USING(b))`,
		`SELECT * FROM tc JOIN (ta JOIN td USING(b)) ON 1`,
		`SELECT * FROM tc, (ta NATURAL LEFT JOIN td)`,
		`SELECT * FROM (ta JOIN td USING(b)) AS g`,
		`SELECT * FROM tc, (wa JOIN wd USING(b))`,
		// ...and through a "*" qualified by one of the group's own members,
		// which reaches the same rebuilt list.
		`SELECT ta.* FROM tc, (ta JOIN td USING(b))`,
		`SELECT tc.z, td.* FROM tc, (ta JOIN td USING(b))`,
		// A bare "*" anywhere in the list counts, aliased or not.
		`SELECT tc.z, * FROM tc, (ta JOIN td USING(b))`,
	}
	unstarred := []string{
		// The same groups, with an explicit select list: the rebuilt order is
		// unobservable, so these must answer.
		`SELECT a, b, g FROM tc, (ta JOIN td USING(b))`,
		`SELECT z, a, b, g FROM tc, (ta JOIN td USING(b))`,
		`SELECT b FROM tc, (ta JOIN td USING(b))`,
		`SELECT coalesce(ta.b, td.b) FROM tc, (ta LEFT JOIN td USING(b))`,
		`SELECT ta.a, td.g FROM tc JOIN (ta JOIN td USING(b)) ON 1`,
		`SELECT p, b, q, r, s FROM tc, (wa JOIN wd USING(b))`,
		// A member-qualified read of the coalesced column: agrees cell for
		// cell, and differs only in the ":N" auto-name r26NamesAgree relaxes.
		`SELECT ta.b FROM tc, (ta JOIN td USING(b))`,
		`SELECT td.b FROM tc, (ta JOIN td USING(b))`,
		`SELECT ta.b AS bb FROM tc, (ta JOIN td USING(b))`,
		// A "*" qualified by a table OUTSIDE the group never reaches the
		// rebuilt list.
		`SELECT tc.* FROM tc, (ta JOIN td USING(b))`,
		// The other ways the list could leak, all of which read the SELECT
		// list's own positions and are therefore already covered by it.
		`SELECT a, b, g FROM tc, (ta JOIN td USING(b)) ORDER BY 2`,
		`SELECT a, b FROM tc, (ta JOIN td USING(b)) UNION ALL SELECT 9, 9 FROM tc`,
		`SELECT * FROM (SELECT a, b FROM tc, (ta JOIN td USING(b)))`,
		`SELECT b FROM tb3 NATURAL JOIN (ta JOIN td USING(b))`,
		// Rebuilt but coalescing nothing: served before this change and after.
		`SELECT * FROM tc, (e1 JOIN e2 ON 1)`,
		`SELECT * FROM tc, (ta JOIN e2 ON 1)`,
		// Coalescing exactly one name that ALREADY leads the group (td(b,g)):
		// the front-move is a no-op, so even a "*" is served. Declining these
		// is what cost 321 mined statements on this round's first attempt.
		`SELECT * FROM tc, (td JOIN ta USING(b))`,
		`SELECT * FROM tc, (td JOIN tb3 USING(b))`,
	}

	for _, q := range starred {
		eCols, _, eErr, cCols, _, cErr := cmp(q)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure before trusting the decline", q, cErr)
			continue
		}
		if eErr == nil {
			t.Errorf("[%s] the engine ANSWERED it; SQLite rebuilds this group's column list (oracle cols=%v, engine cols=%v)", q, cCols, eCols)
		}
	}
	for _, q := range unstarred {
		eCols, eRows, eErr, cCols, cRows, cErr := cmp(q)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure", q, cErr)
			continue
		}
		if eErr != nil {
			t.Errorf("[%s] the engine DECLINED a shape whose rebuilt column list nothing can observe: %v", q, eErr)
			continue
		}
		if ok, why := r26NamesAgree(eCols, cCols); !ok {
			t.Errorf("[%s] answered but diverged: %s\n  engine cols=%v\n  cgo    cols=%v", q, why, eCols, cCols)
			continue
		}
		blank := make([]string, len(cCols))
		for i := range blank {
			blank[i] = "c"
		}
		if ok, why := queryResultsMatch(blank, eRows, blank, cRows, strings.Contains(strings.ToUpper(q), "ORDER BY")); !ok {
			t.Errorf("[%s] answered but diverged: %s\n  engine: %v\n  cgo:    %v", q, why, eRows, cRows)
		}
	}
}

// TestR26FrontMoveUnobservableExemption pins the SECOND narrowing of rule
// (A)'s front-move decline: when the group's own OUTWARD USING/NATURAL
// connector coalesces on the SAME column its internal SF_NestedFrom rebuild
// would front, that column is elided from a flat "*" either way (the
// ordinary, non-nested using-omission, select.c:6260), so the front-move
// itself is unobservable and the group is now SERVED instead of declined --
// but ONLY when the coalescing connector is the group's own FIRST internal
// one (members[0]-members[1]); a later connector's insertion point is
// mid-list, not the group's absolute front, and stays declined. See
// frontMoveUnobservable's own doc comment (vdbe_join_codegen.go) for the
// full citation.
//
// join2.test's own join2-1.7 is the served case; the "coalescing connector
// not first" 3/5-member shapes must still decline exactly as rule (A) always
// has, rather than risk a silently wrong column order (this was a genuine
// wrong answer in an earlier, rejected attempt at this exact narrowing,
// which checked only the group's flat-first column instead of the actually-
// displaced one).
//
// A group SERVED by this exemption that ALSO happens to have an accidental
// (non-coalescing) duplicate name is exercised too -- and, per
// checkRebuiltJoinGroups' own top-of-file doc comment (its "NOT declined,
// deliberately" paragraph) and TestR26RebuiltListNeedsAStar's own precedent
// above, this is NOT declined either: an unaliased rebuilt group's plain
// repeated name already answers with SQLite's ":N" copy relaxed by
// r26NamesAgree, the identical documented-as-unspecified family, regardless
// of whether the group's own front-move exemption is also in play.
func TestR26FrontMoveUnobservableExemption(t *testing.T) {
	cmp := r26ObsPair(t, []string{
		`CREATE TABLE j1(a,b)`, `CREATE TABLE j2(b,c)`, `CREATE TABLE j3(c,d)`,
		`INSERT INTO j1 VALUES(1,11)`,
		`INSERT INTO j2 VALUES(11,111)`,
		`INSERT INTO j3 VALUES(111,1111)`,
	})
	served := []string{
		// join2-1.7 and its RIGHT-JOIN mirror: the group's own outward NATURAL
		// connector (j1-group) coalesces "b", the SAME column the internal
		// rebuild would front ahead of "c" -- but "b" is elided from the flat
		// "*" by the ordinary using-omission regardless of its internal
		// position, so the front-move is invisible. No accidental duplicate
		// here, so names must match EXACTLY, not just via r26NamesAgree.
		`SELECT * FROM j1 NATURAL LEFT OUTER JOIN (j2 NATURAL JOIN j3)`,
		`SELECT * FROM j1 NATURAL RIGHT OUTER JOIN (j2 NATURAL JOIN j3)`,
	}
	for _, q := range served {
		eCols, eRows, eErr, cCols, cRows, cErr := cmp(q)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure", q, cErr)
			continue
		}
		if eErr != nil {
			t.Errorf("[%s] the engine DECLINED a shape whose front-move is unobservable: %v", q, eErr)
			continue
		}
		if len(eCols) != len(cCols) {
			t.Errorf("[%s] column count: engine=%d cgo=%d", q, len(eCols), len(cCols))
			continue
		}
		for i := range cCols {
			if eCols[i] != cCols[i] {
				t.Errorf("[%s] column %d name: engine=%q cgo=%q (no accidental duplicate here, names must match exactly)", q, i, eCols[i], cCols[i])
			}
		}
		blank := make([]string, len(cCols))
		for i := range blank {
			blank[i] = "c"
		}
		if ok, why := queryResultsMatch(blank, eRows, blank, cRows, false); !ok {
			t.Errorf("[%s] answered but diverged: %s\n  engine: %v\n  cgo:    %v", q, why, eRows, cRows)
		}
	}

	// The coalescing connector is NOT the group's first internal one (j2-j3's
	// connector is a plain "ON 1=1"; j3-j4's NATURAL is the one that
	// coalesces "c", which is members[2]'s own connector, not members[1]'s) --
	// stays declined exactly as rule (A) always has.
	stillDeclined := r26ObsPair(t, []string{
		`CREATE TABLE k1(x,z)`, `CREATE TABLE k2(x,y)`, `CREATE TABLE k3(w,c)`, `CREATE TABLE k4(c,d)`,
		`INSERT INTO k1 VALUES(1,2)`, `INSERT INTO k2 VALUES(1,3)`,
		`INSERT INTO k3 VALUES(4,5)`, `INSERT INTO k4 VALUES(5,6)`,
	})
	for _, q := range []string{
		`SELECT * FROM k1 NATURAL LEFT JOIN (k2 JOIN k3 ON 1=1 NATURAL JOIN k4)`,
	} {
		eCols, eRows, eErr, cCols, _, cErr := stillDeclined(q)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure", q, cErr)
			continue
		}
		if eErr == nil {
			t.Errorf("[%s] the engine ANSWERED it; the coalescing connector is not the group's first, so the front-move is NOT provably unobservable (oracle cols=%v, engine cols=%v, engine rows=%v)", q, cCols, eCols, eRows)
		}
	}

	// A group served by the front-move exemption that ALSO has an accidental
	// (non-coalescing) duplicate name: still answers, per checkRebuiltJoinGroups'
	// own established "unaliased repeat is never declined" policy -- the
	// front-move exemption being in play changes nothing about that.
	dup := r26ObsPair(t, []string{
		`CREATE TABLE d1(a,b)`, `CREATE TABLE d2(b,c)`, `CREATE TABLE d3(c,d)`, `CREATE TABLE d4(d,z)`,
		`INSERT INTO d1 VALUES(1,11)`, `INSERT INTO d2 VALUES(11,111)`,
		`INSERT INTO d3 VALUES(111,1111)`, `INSERT INTO d4 VALUES(9999,88888)`,
	})
	for _, q := range []string{
		`SELECT * FROM d1 NATURAL LEFT JOIN (d2 NATURAL JOIN d3 JOIN d4 ON 1=1)`,
	} {
		eCols, eRows, eErr, cCols, cRows, cErr := dup(q)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure", q, cErr)
			continue
		}
		if eErr != nil {
			t.Errorf("[%s] the engine DECLINED an unaliased group's accidental duplicate, which this project's own established policy never declines: %v", q, eErr)
			continue
		}
		if ok, why := r26NamesAgree(eCols, cCols); !ok {
			t.Errorf("[%s] answered but diverged: %s\n  engine cols=%v\n  cgo    cols=%v", q, why, eCols, cCols)
			continue
		}
		blank := make([]string, len(cCols))
		for i := range blank {
			blank[i] = "c"
		}
		if ok, why := queryResultsMatch(blank, eRows, blank, cRows, false); !ok {
			t.Errorf("[%s] answered but diverged: %s\n  engine: %v\n  cgo:    %v", q, why, eRows, cRows)
		}
	}
}

// TestR26RebuiltThirdCopyIsAmbiguous pins rule (A2): a coalesced name that a
// member OUTSIDE that USING/NATURAL also declares survives the rebuild TWICE,
// and SQLite then refuses the whole statement whatever the select list names.
// The identical group in the LEADING (spliced) position answers, which is what
// makes this the rebuild's doing and not ordinary ambiguity.
//
// This was a live wrong answer at 15ce537: the engine returned rows for every
// non-star line below.
func TestR26RebuiltThirdCopyIsAmbiguous(t *testing.T) {
	cmp := r26ObsPair(t, []string{
		`CREATE TABLE g1(k,p)`, `CREATE TABLE g2(q,k)`, `CREATE TABLE g3(k,r)`,
		`CREATE TABLE tc(z)`,
		`INSERT INTO g1 VALUES(1,'p1'),(2,'p2')`,
		`INSERT INTO g2 VALUES('q1',1),('q2',3)`,
		`INSERT INTO g3 VALUES(1,'r1'),(4,'r4')`,
		`INSERT INTO tc VALUES('z1')`,
	})
	// Rebuilt: the oracle refuses these outright, so the engine must too.
	for _, q := range []string{
		`SELECT p, r FROM tc, (g1 JOIN g2 USING(k) JOIN g3 ON 1)`,
		`SELECT * FROM tc, (g1 JOIN g2 USING(k) JOIN g3 ON 1)`,
		`SELECT g1.k FROM tc, (g1 JOIN g2 USING(k) JOIN g3 ON 1)`,
		`SELECT p, r FROM tc, (g1 NATURAL JOIN g2 JOIN g3 ON 1)`,
		`SELECT p, r FROM (g1 JOIN g2 USING(k) JOIN g3 ON 1) AS gg`,
	} {
		eCols, eRows, eErr, _, _, cErr := cmp(q)
		if cErr == nil {
			t.Errorf("[%s] the ORACLE now ANSWERS it -- re-measure before trusting the decline", q)
			continue
		}
		if !strings.Contains(cErr.Error(), "ambiguous column name") {
			t.Errorf("[%s] the ORACLE's rejection changed: %v", q, cErr)
			continue
		}
		if eErr == nil {
			t.Errorf("[%s] the engine ANSWERED what the oracle rejects (%v): cols=%v rows=%v", q, cErr, eCols, eRows)
		}
	}
	// The LEADING (spliced) spelling of the very same group, and the flat
	// spelling, both answer on the oracle and must keep answering here.
	for _, q := range []string{
		`SELECT p, r FROM (g1 JOIN g2 USING(k) JOIN g3 ON 1)`,
		`SELECT * FROM (g1 JOIN g2 USING(k) JOIN g3 ON 1)`,
		`SELECT p, r FROM g1 JOIN g2 USING(k) JOIN g3 ON 1`,
		// Stacking USING on the SAME name coalesces every copy away, so the
		// rebuilt list keeps exactly one and nothing is ambiguous.
		`SELECT p, q, r FROM tc, (g1 JOIN g2 USING(k) JOIN g3 USING(k))`,
		`SELECT k FROM tc, (g1 JOIN g2 USING(k) JOIN g3 USING(k))`,
		`SELECT * FROM tc, (g1 JOIN g2 USING(k) JOIN g3 USING(k))`,
	} {
		eCols, eRows, eErr, cCols, cRows, cErr := cmp(q)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure", q, cErr)
			continue
		}
		if eErr != nil {
			t.Errorf("[%s] the engine DECLINED a shape the oracle answers: %v", q, eErr)
			continue
		}
		if ok, why := r26NamesAgree(eCols, cCols); !ok {
			t.Errorf("[%s] answered but diverged: %s\n  engine cols=%v\n  cgo    cols=%v", q, why, eCols, cCols)
			continue
		}
		blank := make([]string, len(cCols))
		for i := range blank {
			blank[i] = "c"
		}
		if ok, why := queryResultsMatch(blank, eRows, blank, cRows, false); !ok {
			t.Errorf("[%s] answered but diverged: %s\n  engine: %v\n  cgo:    %v", q, why, eRows, cRows)
		}
	}
}

// TestR26RebuiltObservabilitySweep is the never-wrong net around the narrowing:
// every combination of join operator, group position, alias spelling and select
// form over four fixtures that vary WHICH column is shared, WHERE it sits and
// HOW MANY columns each table has. Each statement must either be DECLINED or
// AGREE with C SQLite -- values, order, row count and column count exactly,
// names exactly except for SQLite's ":N" uniquifier.
//
// The decline COUNT is not pinned (it is a narrowing's whole point that it
// moves); what is pinned is that nothing answers WRONG, and that the sweep
// actually compares something.
func TestR26RebuiltObservabilitySweep(t *testing.T) {
	fixtures := []struct {
		name   string
		setup  []string
		m1, m2 string // group members, in that order
		anchor string
		shared string
		own1   string
		own2   string
	}{
		{
			name: "shared-second",
			setup: []string{
				`CREATE TABLE s1(a,b)`, `CREATE TABLE s2(b,g)`, `CREATE TABLE sc(z)`,
				`INSERT INTO s1 VALUES(1,2),(3,4),(5,NULL)`,
				`INSERT INTO s2 VALUES(2,'x'),(4,'y'),(6,'w')`,
				`INSERT INTO sc VALUES('p')`,
			},
			m1: "s1", m2: "s2", anchor: "sc", shared: "b", own1: "a", own2: "g",
		},
		{
			name: "shared-first",
			setup: []string{
				`CREATE TABLE f1(b,a)`, `CREATE TABLE f2(b,g)`, `CREATE TABLE fc(z)`,
				`INSERT INTO f1 VALUES(2,1),(4,3),(NULL,5)`,
				`INSERT INTO f2 VALUES(2,'x'),(4,'y'),(6,'w')`,
				`INSERT INTO fc VALUES('p')`,
			},
			m1: "f1", m2: "f2", anchor: "fc", shared: "b", own1: "a", own2: "g",
		},
		{
			name: "shared-middle-wide",
			setup: []string{
				`CREATE TABLE m1t(p,b,q)`, `CREATE TABLE m2t(r,b,s)`, `CREATE TABLE mc(z)`,
				`INSERT INTO m1t VALUES('p1',2,'q1'),('p2',4,'q2')`,
				`INSERT INTO m2t VALUES('r1',2,'s1'),('r2',6,'s2')`,
				`INSERT INTO mc VALUES('p')`,
			},
			m1: "m1t", m2: "m2t", anchor: "mc", shared: "b", own1: "p", own2: "r",
		},
		{
			// The shared column is an INTEGER PRIMARY KEY on both sides, which
			// is what join9.test's own schema does and what makes the rows only
			// one side supplies visible under RIGHT/FULL.
			name: "shared-intpk",
			setup: []string{
				`CREATE TABLE i1(v1 TEXT, b INTEGER PRIMARY KEY)`,
				`CREATE TABLE i2(b INTEGER PRIMARY KEY, v2 TEXT)`,
				`CREATE TABLE ic(z)`,
				`INSERT INTO i1(v1,b) VALUES('a1',2),('a3',4)`,
				`INSERT INTO i2(b,v2) VALUES(2,'x'),(9,'y')`,
				`INSERT INTO ic VALUES('p')`,
			},
			m1: "i1", m2: "i2", anchor: "ic", shared: "b", own1: "v1", own2: "v2",
		},
	}

	joins := []string{"JOIN %s USING(%s)", "LEFT JOIN %s USING(%s)", "RIGHT JOIN %s USING(%s)",
		"FULL JOIN %s USING(%s)", "NATURAL JOIN %s", "NATURAL LEFT JOIN %s", "NATURAL FULL JOIN %s"}

	declined, compared := 0, 0
	for _, f := range fixtures {
		cmp := r26ObsPair(t, f.setup)
		for _, j := range joins {
			var connector string
			if strings.Count(j, "%s") == 2 {
				connector = fmt.Sprintf(j, f.m2, f.shared)
			} else {
				connector = fmt.Sprintf(j, f.m2)
			}
			group := fmt.Sprintf("(%s %s)", f.m1, connector)
			froms := []string{
				group,                           // leading, unaliased: spliced, no rebuild
				f.anchor + ", " + group,         // rebuilt: non-leading
				group + " AS gq",                // rebuilt: aliased
				group + ` AS ""`,                // rebuilt: the empty-alias token
				f.anchor + ", " + group + " g2", // rebuilt: both at once
			}
			sels := []string{
				"*",
				f.m1 + ".*",
				f.m2 + ".*",
				f.shared,
				f.own1 + ", " + f.shared + ", " + f.own2,
				f.m1 + "." + f.shared,
				f.m2 + "." + f.shared,
				fmt.Sprintf("coalesce(%s.%s, %s.%s)", f.m1, f.shared, f.m2, f.shared),
			}
			for _, from := range froms {
				for _, sel := range sels {
					q := fmt.Sprintf("SELECT %s FROM %s ORDER BY 1", sel, from)
					eCols, eRows, eErr, cCols, cRows, cErr := cmp(q)
					if cErr != nil {
						// C SQLite rejects it; the engine must not answer.
						if eErr == nil {
							t.Errorf("[%s/%s] the engine ANSWERED what C SQLite rejects (%v)", f.name, q, cErr)
						}
						continue
					}
					if eErr != nil {
						declined++
						continue
					}
					compared++
					if ok, why := r26NamesAgree(eCols, cCols); !ok {
						t.Errorf("[%s/%s] DIVERGES: %s\n  engine cols=%v\n  cgo    cols=%v", f.name, q, why, eCols, cCols)
						continue
					}
					blank := make([]string, len(cCols))
					for i := range blank {
						blank[i] = "c"
					}
					if ok, why := queryResultsMatch(blank, eRows, blank, cRows, true); !ok {
						t.Errorf("[%s/%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", f.name, q, why, eRows, cRows)
					}
				}
			}
		}
	}
	t.Logf("rebuilt-group observability sweep: %d compared against C SQLite, %d declined", compared, declined)
	if compared == 0 {
		t.Fatal("nothing was actually compared -- the sweep is not exercising anything")
	}
}
