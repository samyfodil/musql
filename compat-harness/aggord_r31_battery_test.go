package compat

// Tests GROUP BY output order with varying schemas and indexes.
// Verifies that GROUP BY results match C SQLite when index order differs from result order.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

type r31Case struct {
	shape string // "<schema>/<index>/<query>"
	qlab  string
	ilab  string
	setup []string
	query string
}

var r31Schemas = []struct{ name, ddl, ins string }{
	{"plain", "CREATE TABLE %T(a, b, o)",
		"INSERT INTO %T VALUES(2,'y','r0'),(1,'z','r1'),(2,'x','r2'),(3,'w','r3'),(1,'v','r4'),(2,'u','r5')"},
	// o FIRST: the column order that hid a wrong answer from a 540-statement
	// sweep, because every table in it declared the shared column first.
	{"o-first", "CREATE TABLE %T(o, a, b)",
		"INSERT INTO %T VALUES('r0',2,'y'),('r1',1,'z'),('r2',2,'x'),('r3',3,'w'),('r4',1,'v'),('r5',2,'u')"},
	{"ipk", "CREATE TABLE %T(k INTEGER PRIMARY KEY, a INT, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(10,2,'y','r0'),(20,1,'z','r1'),(30,2,'x','r2'),(40,3,'w','r3'),(50,1,'v','r4'),(60,2,'u','r5')"},
	{"nocase", "CREATE TABLE %T(a TEXT COLLATE NOCASE, b INT NOT NULL, o TEXT)",
		"INSERT INTO %T VALUES('B',5,'r0'),('a',9,'r1'),('b',2,'r2'),('C',7,'r3'),('A',1,'r4'),('b',4,'r5')"},
	{"nulls", "CREATE TABLE %T(a INT, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(2,'y','r0'),(NULL,'z','r1'),(2,'x','r2'),(3,NULL,'r3'),(NULL,'v','r4'),(2,'u','r5')"},
	// A BYTE-DISTINCT TIE in the grouped/aggregated column: 1 and 1.0 compare
	// equal, group together and tie every min()/max(), but typeof() tells them
	// apart -- so WHICH of them the answer reports is observable.
	{"numtie", "CREATE TABLE %T(a, b, o)",
		"INSERT INTO %T VALUES(1,'y','r0'),(1.0,'z','r1'),(2,'x','r2'),(2.0,'w','r3'),(1.0,'v','r4'),(0,'u','r5')"},
}

var r31Indexes = []struct{ name, ddl string }{
	{"none", ""},
	{"a", "CREATE INDEX %I ON %T(a)"},
	{"a-desc", "CREATE INDEX %I ON %T(a DESC)"},
	{"ab", "CREATE INDEX %I ON %T(a,b)"},
	{"a-desc-b", "CREATE INDEX %I ON %T(a DESC,b)"},
	{"ab-desc", "CREATE INDEX %I ON %T(a,b DESC)"},
	{"ba", "CREATE INDEX %I ON %T(b,a)"},
	{"uniq-o", "CREATE UNIQUE INDEX %I ON %T(o)"},
	{"a-nocase", "CREATE INDEX %I ON %T(a COLLATE NOCASE)"},
	{"cover-desc", "CREATE INDEX %I ON %T(a DESC,b DESC,o)"},
	{"o-a-desc", "CREATE INDEX %I ON %T(o,a DESC)"},
}

// Queries that test different GROUP BY variations and aggregations.
var r31Queries = []struct{ name, sql string }{
	{"cnt", "SELECT a, count(*) FROM %T GROUP BY a"},
	{"cnt-only", "SELECT count(*) FROM %T GROUP BY a"},
	{"concat", "SELECT a, group_concat(o) FROM %T GROUP BY a"},
	{"limit", "SELECT a, count(*) FROM %T GROUP BY a LIMIT 2"},
	{"limoff", "SELECT a, count(*) FROM %T GROUP BY a LIMIT 2 OFFSET 1"},
	{"having", "SELECT a, count(*) FROM %T GROUP BY a HAVING count(*)>=1"},
	{"where", "SELECT a, count(*) FROM %T WHERE b IS NOT NULL GROUP BY a"},
	{"two", "SELECT a, b, count(*) FROM %T GROUP BY a, b"},
	{"two-rev", "SELECT b, a, count(*) FROM %T GROUP BY b, a"},
	{"grp-min", "SELECT a, min(b), typeof(a) FROM %T GROUP BY a"},
	{"grp-max", "SELECT a, max(b), typeof(a) FROM %T GROUP BY a"},
	{"bare", "SELECT a, o FROM %T GROUP BY a"},
	{"distinct", "SELECT DISTINCT a, count(*) FROM %T GROUP BY a"},
	{"order", "SELECT a, count(*) FROM %T GROUP BY a ORDER BY count(*)"},
	{"idxby", "SELECT a, count(*) FROM %T INDEXED BY %I GROUP BY a"},
	{"notidx", "SELECT a, count(*) FROM %T NOT INDEXED GROUP BY a"},
	{"rowid", "SELECT count(*) FROM %T GROUP BY rowid"},
	// Min/max early-out: SQLite reads one row, so ties are decided by index order.
	{"mm-max", "SELECT max(a), typeof(max(a)) FROM %T"},
	{"mm-min", "SELECT min(a), typeof(min(a)) FROM %T"},
	{"mm-max1", "SELECT typeof(max(a)) FROM %T"},
	{"mm-min-w", "SELECT min(a), typeof(min(a)) FROM %T WHERE o IS NOT NULL"},
}

// r31ShortQueries is a subset of queries for faster testing, one per category.
var r31ShortQueries = map[string]bool{
	"cnt":     true, // the group ORDER itself
	"limit":   true, // ... which decides the row SET, not just its order
	"two-rev": true, // a key the index delivers in a PERMUTED order
	"bare":    true, // the aggregate anchor guard's own shape
	"grp-max": true, // which of a tie the group KEY reports
	"mm-max":  true, // a lone min()/max()'s own arrival order
	"mm-max1": true, // ... written ONCE, the control that isolates the nFunc count
}

func r31Cases(short bool) []r31Case {
	var out []r31Case
	n := 0
	for _, sc := range r31Schemas {
		for _, ix := range r31Indexes {
			for _, q := range r31Queries {
				if short && !r31ShortQueries[q.name] {
					n++ // keep table names stable across modes
					continue
				}
				tab := "g" + strconv.Itoa(n)
				idx := "j" + strconv.Itoa(n)
				n++
				rep := func(s string) string {
					return strings.ReplaceAll(strings.ReplaceAll(s, "%T", tab), "%I", idx)
				}
				c := r31Case{
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

type r31Tally struct {
	agree, declined, wrong, mutual int
	declineBy, wrongBy             map[string]int
}

// r31Run executes the battery and classifies every case against the oracle,
// batching cases into shared worker invocations (each has its own table) so the
// whole battery costs tens of process spawns rather than thousands.
func r31Run(t *testing.T, cases []r31Case, report func(c r31Case, cgo, mush []byte)) r31Tally {
	t.Helper()
	tal := r31Tally{declineBy: map[string]int{}, wrongBy: map[string]int{}}
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

func r31Log(t *testing.T, tal r31Tally, total int) {
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
			t.Logf("R31 %-8s %-24s %d", m.lab, k, m.by[k])
		}
	}
	t.Logf("R31 GROUP-BY-ORDER BATTERY: cases=%d agree=%d declined=%d wrong=%d mutualReject=%d",
		total, tal.agree, tal.declined, tal.wrong, tal.mutual)
}

// r31DeclineCeiling is the number of battery cases this engine declined at the
// commit that last moved it. A CEILING, not an expectation: the test fails only
// when declines go UP, so lifting one is never a red test. It was 74 (a DESC
// index's anchor-guard refusal, and the nocase schema against planGroupByStmt's
// non-BINARY-key declines); both are gone.
const r31DeclineCeiling = 0

// r31ShortDeclineCeiling is the same for the -short subset (r31ShortQueries).
const r31ShortDeclineCeiling = 0

// r31KnownWrong is the TRACKED BACKLOG: the exact (query/index) cells that still
// diverge, with the exact count each contributes, all of them owned by a file
// outside this stream's bucket and all of them itemised in this file's opening
// comment. A divergence in ANY other cell -- or MORE of them in one of these --
// is a hard failure, so this masks a measured, named backlog without masking a
// regression. Delete an entry as its owner fixes it.
var r31KnownWrong = map[string]int{}

func TestR31GroupByOrderBattery(t *testing.T) {
	short := testing.Short()
	cases := r31Cases(short)
	tal := r31Run(t, cases, func(c r31Case, cgo, mush []byte) {
		if r31KnownWrong[c.qlab+"/"+c.ilab] > 0 {
			t.Logf("R31 known-wrong %s\n  %s\n  cgo:    %s\n  musql: %s",
				c.shape, c.query, cgo, mush)
			return
		}
		t.Errorf("R31 group-by-order DIVERGES %s\n  %s\n  cgo:    %s\n  musql: %s",
			c.shape, c.query, cgo, mush)
	})
	r31Log(t, tal, len(cases))
	for k, n := range tal.wrongBy {
		if n > r31KnownWrong[k] {
			t.Errorf("R31 %s now diverges on %d shapes, tracked at %d", k, n, r31KnownWrong[k])
		}
	}
	ceiling := r31DeclineCeiling
	if short {
		ceiling = r31ShortDeclineCeiling
	}
	if tal.declined > ceiling {
		t.Errorf("R31 declines rose to %d (ceiling %d) -- a shape that used to be served now is not",
			tal.declined, ceiling)
	}
}

// TestGroupDistinctEmitsInSorterOrder pins SELECT DISTINCT over a GROUP BY with
// no ORDER BY to the order SQLite emits: the GROUP BY sorter's, under the key's
// own collation (select.c:8501), with the DISTINCT ephemeral index keeping the
// first row EMITTED (select.c:8263-8272). The first case's groups are scanned
// 3, 2, 1, so keeping the first-SCANNED duplicate and then sorting by key --
// what groupBatchFinal used to do -- answers 2, 1 where SQLite answers 1, 2. The
// second is the battery's nocase/none/distinct cell, which it answered in byte
// order. Both run in -short, which the full battery's DISTINCT cells do not.
func TestGroupDistinctEmitsInSorterOrder(t *testing.T) {
	cases := []r31Case{
		{shape: "survivor", setup: []string{"CREATE TABLE gs1(a INT, b)",
			"INSERT INTO gs1 VALUES(3,'p'),(2,'q'),(2,'r'),(1,'s')"},
			query: "SELECT DISTINCT count(*) FROM gs1 GROUP BY a"},
		{shape: "nocase", setup: []string{"CREATE TABLE gs2(a TEXT COLLATE NOCASE, b INT NOT NULL, o TEXT)",
			"INSERT INTO gs2 VALUES('B',5,'r0'),('a',9,'r1'),('b',2,'r2'),('C',7,'r3'),('A',1,'r4'),('b',4,'r5')"},
			query: "SELECT DISTINCT a, count(*) FROM gs2 GROUP BY a"},
	}
	tal := r31Run(t, cases, func(c r31Case, cgo, mush []byte) {
		t.Errorf("DIVERGES %s\n  %s\n  cgo:    %s\n  musql: %s", c.shape, c.query, cgo, mush)
	})
	if tal.agree != len(cases) {
		t.Errorf("want all %d cases served and agreeing, got agree=%d declined=%d wrong=%d mutualReject=%d",
			len(cases), tal.agree, tal.declined, tal.wrong, tal.mutual)
	}
}
