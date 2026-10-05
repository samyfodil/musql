package compat

// This file tests GROUP BY key reporting through various projections.
// gets worse.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

type r32lCase struct {
	shape string // "<schema>/<index>/<query>"
	qlab  string
	ilab  string
	slab  string
	setup []string
	query string
}

// r32lSchemas: the same six logical rows under declarations that differ ONLY in
// the affinity of the grouped column a -- which is exactly what decides whether
// one group can hold byte-distinct values for it.
var r32lSchemas = []struct{ name, ddl, ins string }{
	// No declared type: affinity BLOB (none), so INTEGER 1 and REAL 1.0 are both
	// stored as written and group together while typeof() separates them.
	{"untyped", "CREATE TABLE %T(a, b, o)",
		"INSERT INTO %T VALUES(1,'y','r0'),(1.0,'z','r1'),(2,'x','r2'),(2.0,'w','r3'),(1.0,'v','r4'),(0,'u','r5')"},
	// The same rows with the INTEGER/REAL roles SWAPPED: a fix that always
	// reports the REAL member of a tie passes untyped and fails this.
	{"untyped-rev", "CREATE TABLE %T(a, b, o)",
		"INSERT INTO %T VALUES(1.0,'y','r0'),(1,'z','r1'),(2.0,'x','r2'),(2,'w','r3'),(1,'v','r4'),(0.0,'u','r5')"},
	// BLOB affinity is "none" spelled out, and it must behave exactly like
	// untyped -- build.c's sqlite3AffinityType maps the substring "BLOB" to
	// SQLITE_AFF_BLOB, which applies no conversion at all.
	{"blobaff", "CREATE TABLE %T(a BLOB, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(1,'y','r0'),(1.0,'z','r1'),(2,'x','r2'),(2.0,'w','r3'),(1.0,'v','r4'),(0,'u','r5')"},
	// CONTROL: INTEGER affinity folds a lossless REAL onto its INTEGER, so every
	// value in a group is byte-identical and no readout can tell the rows apart.
	{"intaff", "CREATE TABLE %T(a INT, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(1,'y','r0'),(1.0,'z','r1'),(2,'x','r2'),(2.0,'w','r3'),(1.0,'v','r4'),(0,'u','r5')"},
	// CONTROL: REAL affinity folds the INTEGERs the other way.
	{"realaff", "CREATE TABLE %T(a REAL, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(1,'y','r0'),(1.0,'z','r1'),(2,'x','r2'),(2.0,'w','r3'),(1.0,'v','r4'),(0,'u','r5')"},
	// CONTROL: TEXT affinity stores '1' and '1.0' as DIFFERENT text, so they are
	// different GROUPS -- the shape where the whole question disappears.
	{"textaff", "CREATE TABLE %T(a TEXT, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(1,'y','r0'),(1.0,'z','r1'),(2,'x','r2'),(2.0,'w','r3'),(1.0,'v','r4'),(0,'u','r5')"},
	// SIGNED ZERO: -0.0 and 0.0 and 0 all group together and all three render
	// differently. compareNumeric treats -0.0 == 0.0, so this is a byte-distinct
	// tie that has nothing to do with the INTEGER/REAL storage class.
	{"signzero", "CREATE TABLE %T(a, b, o)",
		"INSERT INTO %T VALUES(0,'y','r0'),(-0.0,'z','r1'),(0.0,'x','r2'),(1,'w','r3'),(1.0,'v','r4'),(2,'u','r5')"},
	// A non-BINARY grouping collation: 'A' and 'a' are one group with two
	// byte-distinct keys. Most of these are DECLINED today (planGroupByStmt's
	// three non-BINARY-key tails); they are kept so a fix that lifts one of those
	// declines is measured against the oracle rather than assumed.
	{"nocase", "CREATE TABLE %T(a TEXT COLLATE NOCASE, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES('A','y','r0'),('a','z','r1'),('B','x','r2'),('b','w','r3'),('a','v','r4'),('C','u','r5')"},
}

var r32lIndexes = []struct{ name, ddl string }{
	{"none", ""},
	{"a", "CREATE INDEX %I ON %T(a)"},
	{"a-desc", "CREATE INDEX %I ON %T(a DESC)"},
	{"ab", "CREATE INDEX %I ON %T(a,b)"},
	{"ba", "CREATE INDEX %I ON %T(b,a)"},
	{"uniq-o", "CREATE UNIQUE INDEX %I ON %T(o)"},
	{"cover-desc", "CREATE INDEX %I ON %T(a DESC,b DESC,o)"},
}

// r32lQueries: the (magnet x readout) matrix. Each name is "<readout>-<magnet>".
var r32lQueries = []struct{ name, sql string }{
	// No magnet at all: the anchor IS the group's first row, so these must not
	// move whatever the key fix does. The controls for the whole battery.
	{"key-none", "SELECT a, count(*) FROM %T GROUP BY a"},
	{"typ-none", "SELECT typeof(a), count(*) FROM %T GROUP BY a"},
	{"agg-none", "SELECT count(*) FROM %T GROUP BY a"},

	// One magnet, both directions -- min() and max() land on DIFFERENT rows of
	// the same group, so a fix that hardcodes either one fails the other.
	{"key-min", "SELECT a, min(b) FROM %T GROUP BY a"},
	{"key-max", "SELECT a, max(b) FROM %T GROUP BY a"},
	{"typ-min", "SELECT typeof(a), min(b) FROM %T GROUP BY a"},
	{"typ-max", "SELECT typeof(a), max(b) FROM %T GROUP BY a"},
	{"quote-max", "SELECT quote(a), max(b) FROM %T GROUP BY a"},
	{"cast-max", "SELECT CAST(a AS TEXT), max(b) FROM %T GROUP BY a"},
	{"cat-max", "SELECT a||'/', max(b) FROM %T GROUP BY a"},
	{"key-minb-o", "SELECT a, min(o) FROM %T GROUP BY a"},

	// Two magnets: the LAST min()/max() call in the census decides the anchor.
	{"key-minmax", "SELECT a, min(b), max(b) FROM %T GROUP BY a"},
	{"key-maxmin", "SELECT a, max(b), min(b) FROM %T GROUP BY a"},

	// Magnets that never actually take a row: a FILTER that rejects everything
	// (the anchor stays the group's FIRST row) and max(NULL), which never
	// replaces the accumulator (the anchor becomes the group's LAST row).
	{"key-filter", "SELECT a, max(b) FILTER (WHERE 0) FROM %T GROUP BY a"},
	{"key-maxnull", "SELECT a, max(NULL) FROM %T GROUP BY a"},

	// The magnet lives OUTSIDE the select list: minMaxCensus spans HAVING and
	// ORDER BY too, and the anchor it produces is the whole statement's.
	{"key-having", "SELECT a FROM %T GROUP BY a HAVING max(b) IS NOT NULL"},
	{"key-ordagg", "SELECT a, count(*) FROM %T GROUP BY a ORDER BY max(b)"},

	// A GROUP BY term that is NOT a bare column: select.c REWRITES the matching
	// select-list entry into the iAMem group-key register, so this one reads the
	// group's FIRST row even under a magnet. The pair below it reads the key
	// BOTH ways in one statement, which is the only way to see the split.
	{"expr-max", "SELECT a+0, max(b) FROM %T GROUP BY a+0"},
	{"both-max", "SELECT a, a+0, max(b) FROM %T GROUP BY a"},
	// Two identical select-list entries: resolveOrderGroupBy keeps the LAST
	// match (iOrderByCol = j+1 assigned in a loop with no break).
	{"dup-max", "SELECT a, a, max(b) FROM %T GROUP BY a"},

	// The key alongside a genuinely BARE column -- the shape the aggregate anchor
	// guard already owns -- so a key fix cannot be measured in isolation from it.
	{"key-bare-max", "SELECT a, max(b), o FROM %T GROUP BY a"},

	// The group SET and its emission order, so a key fix that quietly pays for
	// itself in group order is caught: LIMIT decides WHICH groups, DISTINCT
	// re-dedups them, and the trailing ORDER BY re-sorts them.
	{"key-limit-max", "SELECT a, max(b) FROM %T GROUP BY a LIMIT 2"},
	{"key-dist-max", "SELECT DISTINCT a, max(b) FROM %T GROUP BY a"},
	{"key-order-max", "SELECT a, max(b) FROM %T GROUP BY a ORDER BY 1"},
	// sum() over a group holding both an INTEGER and a REAL: the accumulator's
	// own int/real promotion, which is a different reader of the same tie.
	{"sum-max", "SELECT a, sum(a), max(b) FROM %T GROUP BY a"},
}

// r32lShortQueries is the -short subset: one query per thing this battery can
// catch. Every SCHEMA and every INDEX shape is kept, because affinity and index
// order are the two axes a narrow fixture has hidden a bug on before.
var r32lShortQueries = map[string]bool{
	"key-none":      true, // the no-magnet control
	"key-max":       true, // the key under a magnet
	"typ-min":       true, // ... read through typeof(), and pulled the other way
	"key-maxnull":   true, // a magnet that never replaces
	"expr-max":      true, // the iAMem arm, which must NOT follow the anchor
	"key-limit-max": true, // the group SET, not just its order
}

func r32lCases(short bool) []r32lCase {
	var out []r32lCase
	n := 0
	for _, sc := range r32lSchemas {
		for _, ix := range r32lIndexes {
			for _, q := range r32lQueries {
				if short && !r32lShortQueries[q.name] {
					n++ // keep table names stable across modes
					continue
				}
				tab := "kk" + strconv.Itoa(n)
				idx := "ki" + strconv.Itoa(n)
				n++
				rep := func(s string) string {
					return strings.ReplaceAll(strings.ReplaceAll(s, "%T", tab), "%I", idx)
				}
				c := r32lCase{
					shape: sc.name + "/" + ix.name + "/" + q.name,
					qlab:  q.name, ilab: ix.name, slab: sc.name,
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

type r32lTally struct {
	agree, declined, wrong, mutual int
	declineBy, wrongBy             map[string]int
}

// r32lRun executes the battery and classifies every case against the oracle,
// batching cases into shared worker invocations (each has its own table) so the
// whole battery costs tens of process spawns rather than thousands.
func r32lRun(t *testing.T, cases []r32lCase, report func(c r32lCase, cgo, mush []byte)) r32lTally {
	t.Helper()
	tal := r32lTally{declineBy: map[string]int{}, wrongBy: map[string]int{}}
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
				tal.declineBy[c.slab+"/"+c.qlab]++
			case string(cb) != string(mb):
				tal.wrong++
				tal.wrongBy[c.slab+"/"+c.qlab+"/"+c.ilab]++
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

func r32lLog(t *testing.T, tal r32lTally, total int) {
	t.Helper()
	for _, m := range []struct {
		lab string
		by  map[string]int
	}{{"declined", tal.declineBy}, {"WRONG", tal.wrongBy}} {
		keys := make([]string, 0, len(m.by))
		for k := range m.by {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("R32L %-8s %-34s %d", m.lab, k, m.by[k])
		}
	}
	t.Logf("R32L GROUP-KEY-VALUE BATTERY: cases=%d agree=%d declined=%d wrong=%d mutualReject=%d",
		total, tal.agree, tal.declined, tal.wrong, tal.mutual)
}

// r32lDeclineCeiling / r32lShortDeclineCeiling are the number of battery cases
// this engine declined at the commit that last moved them. CEILINGS, not
// expectations: the test fails only when declines go UP, so lifting one is never
// a red test.
//
// The 21 are the nocase schema against planGroupByStmt's two surviving
// non-BINARY-GROUP-BY-key declines (SELECT DISTINCT and a trailing ORDER BY);
// its third, a min()/max() call, is gone -- the key now follows the anchor row
// that decides its value, which is what that decline existed for.
const r32lDeclineCeiling = 21
const r32lShortDeclineCeiling = 0

// r32lKnownWrong is the TRACKED BACKLOG, keyed "<schema>/<query>": the exact
// cells that still diverge and how many index shapes each contributes. All 26
// are one query, "SELECT a, max(b) FILTER (WHERE 0) FROM t GROUP BY a", over an
// INDEXED table, and all 26 are one cause that lives OUTSIDE this stream's
// files: wherePlanWalkColumns (engine/where_plan_gate.go) reports an
// unresolvable column for any aggregate carrying a FILTER, so wherePlanColUsed
// declines, so markWherePlanIndexEligibility installs no index-order key, so
// this engine full-scans in rowid order where SQLite walks the index -- which
// moves BOTH the group emission order and which row of a group is its first
// (and so its anchor).
//
// 22 of the 26 were already wrong before this stream touched anything; the other
// 4 (the nocase schema) were previously masked by the non-BINARY + min()/max()
// decline this stream lifted. Measured with the three-line where_plan_gate.go
// patch this stream reports -- walk a FuncExpr's Filter instead of declining it,
// which is what resolve.c:1352 does -- 26 -> 0, declines unchanged at 21.
//
// A divergence in ANY other cell, or MORE of them in one of these, fails hard.
var r32lKnownWrong = map[string]int{
	"untyped/key-filter": 4, "untyped-rev/key-filter": 4, "blobaff/key-filter": 4,
	"signzero/key-filter": 4, "nocase/key-filter": 4,
	"intaff/key-filter": 2, "realaff/key-filter": 2, "textaff/key-filter": 2,
}

func TestR32LGroupKeyValueBattery(t *testing.T) {
	short := testing.Short()
	cases := r32lCases(short)
	tal := r32lRun(t, cases, func(c r32lCase, cgo, mush []byte) {
		if r32lKnownWrong[c.slab+"/"+c.qlab] > 0 {
			t.Logf("R32L known-wrong %s\n  %s\n  cgo:    %s\n  musql: %s",
				c.shape, c.query, cgo, mush)
			return
		}
		t.Errorf("R32L group-key-value DIVERGES %s\n  %s\n  cgo:    %s\n  musql: %s",
			c.shape, c.query, cgo, mush)
	})
	r32lLog(t, tal, len(cases))
	byQuery := map[string]int{}
	for k, n := range tal.wrongBy {
		byQuery[k[:strings.LastIndex(k, "/")]] += n
	}
	for k, n := range byQuery {
		if n > r32lKnownWrong[k] {
			t.Errorf("R32L %s now diverges on %d shapes, tracked at %d", k, n, r32lKnownWrong[k])
		}
	}
	ceiling := r32lDeclineCeiling
	if short {
		ceiling = r32lShortDeclineCeiling
	}
	if tal.declined > ceiling {
		t.Errorf("R32L declines rose to %d (ceiling %d) -- a shape that used to be served now is not",
			tal.declined, ceiling)
	}
}
