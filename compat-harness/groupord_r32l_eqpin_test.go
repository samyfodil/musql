package compat

// Tests GROUP BY order with WHERE clauses that pin index columns with equality conditions.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var r32lEqSchemas = []struct{ name, ddl, ins string }{
	{"plain", "CREATE TABLE %T(a, b, c, o)",
		"INSERT INTO %T VALUES(2,1,'p','r0'),(1,1,'q','r1'),(3,1,'r','r2'),(1,2,'s','r3'),(2,2,'t','r4'),(3,1,'u','r5'),(1,1,'v','r6'),(2,1,'w','r7')"},
	// b NOT NULL: Column.notNull is what tag-20210426-1 reads.
	{"typed", "CREATE TABLE %T(a INT, b INT NOT NULL, c TEXT, o TEXT)",
		"INSERT INTO %T VALUES(2,1,'p','r0'),(1,1,'q','r1'),(3,1,'r','r2'),(1,2,'s','r3'),(2,2,'t','r4'),(3,1,'u','r5'),(1,1,'v','r6'),(2,1,'w','r7')"},
	// NULLs in the pinned column, so "b IS NULL" is a real pin.
	{"nulls", "CREATE TABLE %T(a INT, b INT, c TEXT, o TEXT)",
		"INSERT INTO %T VALUES(2,1,'p','r0'),(1,NULL,'q','r1'),(3,1,'r','r2'),(1,2,'s','r3'),(2,NULL,'t','r4'),(3,1,'u','r5'),(1,NULL,'v','r6'),(2,1,'w','r7')"},
	// An INTEGER PRIMARY KEY present: build.c folds its index column onto
	// XN_ROWID, which both this predicate and the C special-case.
	{"ipk", "CREATE TABLE %T(k INTEGER PRIMARY KEY, a INT, b INT, c TEXT, o TEXT)",
		"INSERT INTO %T VALUES(10,2,1,'p','r0'),(20,1,1,'q','r1'),(30,3,1,'r','r2'),(40,1,2,'s','r3'),(50,2,2,'t','r4'),(60,3,1,'u','r5'),(70,1,1,'v','r6'),(80,2,1,'w','r7')"},
}

var r32lEqIndexes = []struct{ name, ddl string }{
	{"none", ""},
	// The named shape: the pin is index column 0, the GROUP BY key is column 1,
	// DESCENDING.
	{"b-adesc", "CREATE INDEX %I ON %T(b,a DESC)"},
	{"b-a", "CREATE INDEX %I ON %T(b,a)"},
	{"bdesc-adesc", "CREATE INDEX %I ON %T(b DESC,a DESC)"},
	// NEGATIVE control: b is pinned, but column 1 is c, which no GROUP BY term
	// names -- so the C's match loop breaks with obSat incomplete.
	{"b-c-adesc", "CREATE INDEX %I ON %T(b,c,a DESC)"},
	{"b-adesc-c", "CREATE INDEX %I ON %T(b,a DESC,c)"},
	// The pin is NOT leading: nothing to skip, and the GROUP BY key leads.
	{"adesc-b", "CREATE INDEX %I ON %T(a DESC,b)"},
	{"a-bdesc", "CREATE INDEX %I ON %T(a,b DESC)"},
	{"c-b-adesc", "CREATE INDEX %I ON %T(c,b,a DESC)"},
	{"adesc", "CREATE INDEX %I ON %T(a DESC)"},
}

var r32lEqQueries = []struct{ name, sql string }{
	{"eq", "SELECT a, count(*) FROM %T WHERE b=1 GROUP BY a"},
	{"eq-concat", "SELECT a, group_concat(o) FROM %T WHERE b=1 GROUP BY a"},
	// The pinned column is ALSO a GROUP BY term: the pre-loop satisfies it.
	{"eq-two", "SELECT b, a, count(*) FROM %T WHERE b=1 GROUP BY b, a"},
	{"eq-two-rev", "SELECT a, b, count(*) FROM %T WHERE b=1 GROUP BY a, b"},
	// exprCommute's own shape.
	{"eq-commuted", "SELECT a, count(*) FROM %T WHERE 1=b GROUP BY a"},
	// The other two members of eqOpMask.
	{"is", "SELECT a, count(*) FROM %T WHERE b IS 1 GROUP BY a"},
	{"isnull", "SELECT a, count(*) FROM %T WHERE b IS NULL GROUP BY a"},
	// A RANGE pins nothing: nEq stays 0 and the leading index column must match
	// a GROUP BY term the ordinary way.
	{"range", "SELECT a, count(*) FROM %T WHERE b>0 GROUP BY a"},
	{"eq-range", "SELECT a, count(*) FROM %T WHERE b=1 AND c>'a' GROUP BY a"},
	// Two equalities, so nEq can reach 2 on (b,c,a DESC) -- the negative control
	// above turned positive.
	{"eq-eq", "SELECT a, count(*) FROM %T WHERE b=1 AND c='q' GROUP BY a"},
	{"eq-limit", "SELECT a, count(*) FROM %T WHERE b=1 GROUP BY a LIMIT 2"},
	{"eq-having", "SELECT a, count(*) FROM %T WHERE b=1 GROUP BY a HAVING count(*)>0"},
	{"eq-max", "SELECT a, max(c) FROM %T WHERE b=1 GROUP BY a"},
	{"eq-bare", "SELECT a, o FROM %T WHERE b=1 GROUP BY a"},
	{"eq-distinct", "SELECT DISTINCT a, count(*) FROM %T WHERE b=1 GROUP BY a"},
	{"eq-idxby", "SELECT a, count(*) FROM %T INDEXED BY %I WHERE b=1 GROUP BY a"},
	// CONTROLS: an explicit ORDER BY makes the emission order the statement's
	// own, and no WHERE at all is the shape round 31 already ported.
	{"eq-order", "SELECT a, count(*) FROM %T WHERE b=1 GROUP BY a ORDER BY 1"},
	{"noeq", "SELECT a, count(*) FROM %T GROUP BY a"},
}

// r32lEqShortQueries is the -short subset: one query per thing this battery can
// catch. Every SCHEMA and INDEX shape is kept.
var r32lEqShortQueries = map[string]bool{
	"eq":     true, // the pinned leading column itself
	"eq-two": true, // ... with the pin ALSO a GROUP BY term (the pre-loop)
	"isnull": true, // the eqOpMask member that is not "="
	"range":  true, // a term that pins NOTHING -- the negative control
	"eq-eq":  true, // two pins, so nEq can reach 2
	"noeq":   true, // no WHERE at all: round 31's shape, which must not move
}

type r32lEqCase struct {
	shape string
	qlab  string
	ilab  string
	setup []string
	query string
}

func r32lEqCases(short bool) []r32lEqCase {
	var out []r32lEqCase
	n := 0
	for _, sc := range r32lEqSchemas {
		for _, ix := range r32lEqIndexes {
			for _, q := range r32lEqQueries {
				if short && !r32lEqShortQueries[q.name] {
					n++ // keep table names stable across modes
					continue
				}
				tab := "ep" + strconv.Itoa(n)
				idx := "ei" + strconv.Itoa(n)
				n++
				rep := func(s string) string {
					return strings.ReplaceAll(strings.ReplaceAll(s, "%T", tab), "%I", idx)
				}
				c := r32lEqCase{
					shape: sc.name + "/" + ix.name + "/" + q.name,
					qlab:  q.name, ilab: ix.name,
					setup: []string{rep(sc.ddl), rep(sc.ins)},
				}
				if ix.ddl != "" {
					c.setup = append(c.setup, rep(ix.ddl))
				}
				c.query = rep(q.sql)
				out = append(out, c)
			}
		}
	}
	return out
}

type r32lEqTally struct {
	agree, declined, wrong, mutual int
	declineBy, wrongBy             map[string]int
}

func r32lEqRun(t *testing.T, cases []r32lEqCase, report func(c r32lEqCase, cgo, mush []byte)) r32lEqTally {
	t.Helper()
	tal := r32lEqTally{declineBy: map[string]int{}, wrongBy: map[string]int{}}
	const batch = 60
	for start := 0; start < len(cases); start += batch {
		end := start + batch
		if end > len(cases) {
			end = len(cases)
		}
		var stmts []string
		qAt := make([]int, 0, batch)
		for _, c := range cases[start:end] {
			stmts = append(stmts, c.setup...)
			qAt = append(qAt, len(stmts))
			stmts = append(stmts, c.query)
		}
		cgo := run(t, "cgo", stmts)
		mush := run(t, "musql", stmts)
		for k, c := range cases[start:end] {
			cr, mr := cgo[qAt[k]], mush[qAt[k]]
			cb, _ := json.Marshal(cr)
			mb, _ := json.Marshal(mr)
			cErr := cr["kind"] == "error"
			mErr := mr["kind"] == "error"
			switch {
			case cErr && mErr:
				tal.mutual++
			case cErr != mErr:
				tal.declined++
				tal.declineBy[c.qlab+"/"+c.ilab]++
			case string(cb) != string(mb):
				tal.wrong++
				tal.wrongBy[c.qlab+"/"+c.ilab]++
				if report != nil {
					report(c, cb, mb)
				}
			default:
				tal.agree++
			}
		}
	}
	return tal
}

// r32lEqDeclineCeiling / r32lEqShortDeclineCeiling are CEILINGS: the test fails
// only when declines go UP.
const r32lEqDeclineCeiling = 0
const r32lEqShortDeclineCeiling = 0

// r32lEqKnownWrong is the TRACKED BACKLOG: the exact (query/index) cells that
// still diverge, with the exact count each contributes. Every one of them is the
// same single cause -- groupsArriveOutOfKeyOrder never learns that the winning
// loop pinned the index's leading column, so the groups are emitted ascending
// where the index walk delivers them descending. A divergence in ANY other cell,
// or MORE of them in one of these, is a hard failure.
//
// Measured, over the 720 cases here: 143 wrong today. With the cross-file patch
// this stream reports -- an nEq/groupsSorted pair on autoIndexKey
// (engine/where_plan.go), filled in by wherePlanSingleIndexKey
// (engine/where_plan_index.go), and the groupsArriveOutOfKeyOrder rewrite that
// reads it (engine/vdbe_agg_codegen.go) -- 143 -> 0, declines still 0, and the
// round-30, round-31 and group-key batteries all byte-identical.
var r32lEqKnownWrong = map[string]int{
	"eq/b-adesc": 4, "eq/b-adesc-c": 4, "eq/bdesc-adesc": 4,
	"eq-bare/b-adesc": 4, "eq-bare/b-adesc-c": 4, "eq-bare/bdesc-adesc": 4,
	"eq-commuted/b-adesc": 4, "eq-commuted/b-adesc-c": 4, "eq-commuted/bdesc-adesc": 4,
	"eq-concat/b-adesc": 4, "eq-concat/b-adesc-c": 4, "eq-concat/bdesc-adesc": 4,
	"eq-distinct/b-adesc": 4, "eq-distinct/b-adesc-c": 4, "eq-distinct/bdesc-adesc": 4,
	"eq-having/b-adesc": 4, "eq-having/b-adesc-c": 4, "eq-having/bdesc-adesc": 4,
	"eq-idxby/b-adesc": 4, "eq-idxby/b-adesc-c": 4, "eq-idxby/bdesc-adesc": 4,
	"eq-limit/b-adesc": 4, "eq-limit/b-adesc-c": 4, "eq-limit/bdesc-adesc": 4,
	"eq-max/b-adesc": 4, "eq-max/b-adesc-c": 4, "eq-max/bdesc-adesc": 4,
	"eq-range/b-adesc": 4, "eq-range/b-adesc-c": 4, "eq-range/bdesc-adesc": 4,
	"is/b-adesc": 4, "is/b-adesc-c": 4, "is/bdesc-adesc": 4,
	// Only the nulls schema has a row with b NULL, so only it can see this.
	"isnull/b-adesc": 1, "isnull/b-adesc-c": 1, "isnull/bdesc-adesc": 1,
	// The PRE-LOOP's own shape: b is pinned by the WHERE and never appears in
	// the index at all, so the single index column a DESC satisfies what is
	// left of the GROUP BY and the groups arrive descending.
	"eq-two/adesc": 4, "eq-two-rev/adesc": 4,
}

func TestR32LEqPinnedGroupOrder(t *testing.T) {
	short := testing.Short()
	cases := r32lEqCases(short)
	tal := r32lEqRun(t, cases, func(c r32lEqCase, cgo, mush []byte) {
		if r32lEqKnownWrong[c.qlab+"/"+c.ilab] > 0 {
			t.Logf("R32L-EQ known-wrong %s\n  %s\n  cgo:    %s\n  musql: %s",
				c.shape, c.query, cgo, mush)
			return
		}
		t.Errorf("R32L-EQ pinned-group-order DIVERGES %s\n  %s\n  cgo:    %s\n  musql: %s",
			c.shape, c.query, cgo, mush)
	})
	keys := make([]string, 0, len(tal.wrongBy))
	for k := range tal.wrongBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("R32L-EQ WRONG %-28s %d", k, tal.wrongBy[k])
	}
	dkeys := make([]string, 0, len(tal.declineBy))
	for k := range tal.declineBy {
		dkeys = append(dkeys, k)
	}
	sort.Strings(dkeys)
	for _, k := range dkeys {
		t.Logf("R32L-EQ declined %-28s %d", k, tal.declineBy[k])
	}
	t.Logf("R32L-EQ PINNED-GROUP-ORDER BATTERY: cases=%d agree=%d declined=%d wrong=%d mutualReject=%d",
		len(cases), tal.agree, tal.declined, tal.wrong, tal.mutual)
	for k, n := range tal.wrongBy {
		if n > r32lEqKnownWrong[k] {
			t.Errorf("R32L-EQ %s now diverges on %d shapes, tracked at %d", k, n, r32lEqKnownWrong[k])
		}
	}
	ceiling := r32lEqDeclineCeiling
	if short {
		ceiling = r32lEqShortDeclineCeiling
	}
	if tal.declined > ceiling {
		t.Errorf("R32L-EQ declines rose to %d (ceiling %d) -- a shape that used to be served now is not",
			tal.declined, ceiling)
	}
}
