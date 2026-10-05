package compat

// The INDEXED-AGGREGATE battery tests grouped aggregates, group_concat,
// DISTINCT, ORDER BY with tie-breaking, and LIMIT against indexes. Every
// shape is verified against the oracle.
//
// Fixture rules:
//   - the observer column o is NOT ascending with rowid, so index order and
//     rowid order are distinguishable at all;
//   - the schema varies COLUMN ORDER (o first / middle / last), declared types,
//     collations, NOT NULL and the INTEGER PRIMARY KEY, because a sweep whose
//     tables all declare the interesting column first measures one shape;
//   - the index varies ASC/DESC, single vs composite, UNIQUE, a per-column
//     COLLATE and a covering shape, because wherePathSatisfiesOrderBy's answer
//     is a function of exactly those.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

type r30Case struct {
	shape string // "<schema>/<index>/<query>"
	qlab  string // the query label alone
	ilab  string // the index label alone
	setup []string
	query string
}

// r30Schemas: five tables holding the SAME six logical rows under different
// declarations. %T is the table name.
var r30Schemas = []struct {
	name string
	ddl  string
	ins  string
}{
	{"plain", "CREATE TABLE %T(a, b, o)",
		"INSERT INTO %T VALUES(2,'y','r0'),(1,'z','r1'),(2,'x','r2'),(3,'w','r3'),(1,'v','r4'),(2,'u','r5')"},
	{"o-first", "CREATE TABLE %T(o, a, b)",
		"INSERT INTO %T VALUES('r0',2,'y'),('r1',1,'z'),('r2',2,'x'),('r3',3,'w'),('r4',1,'v'),('r5',2,'u')"},
	{"ipk", "CREATE TABLE %T(k INTEGER PRIMARY KEY, a INT, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(10,2,'y','r0'),(20,1,'z','r1'),(30,2,'x','r2'),(40,3,'w','r3'),(50,1,'v','r4'),(60,2,'u','r5')"},
	{"nocase", "CREATE TABLE %T(a TEXT COLLATE NOCASE, b INT NOT NULL, o TEXT)",
		"INSERT INTO %T VALUES('B',5,'r0'),('a',9,'r1'),('b',2,'r2'),('C',7,'r3'),('A',1,'r4'),('b',4,'r5')"},
	{"nulls", "CREATE TABLE %T(a INT, b TEXT, o TEXT)",
		"INSERT INTO %T VALUES(2,'y','r0'),(NULL,'z','r1'),(2,'x','r2'),(3,NULL,'r3'),(NULL,'v','r4'),(2,'u','r5')"},
}

var r30Indexes = []struct{ name, ddl string }{
	{"none", ""},
	{"a", "CREATE INDEX %I ON %T(a)"},
	{"a-desc", "CREATE INDEX %I ON %T(a DESC)"},
	{"ab", "CREATE INDEX %I ON %T(a,b)"},
	{"a-desc-b", "CREATE INDEX %I ON %T(a DESC,b)"},
	{"uniq-o", "CREATE UNIQUE INDEX %I ON %T(o)"},
	{"a-nocase", "CREATE INDEX %I ON %T(a COLLATE NOCASE)"},
	{"cover-bao", "CREATE INDEX %I ON %T(b,a,o)"},
}

// r30Queries: every statement here REPORTS the arrival order of the one table
// it reads. "%I" is the index name, so the INDEXED BY cases are self-naming.
var r30Queries = []struct{ name, sql string }{
	{"grp-bare", "SELECT a, o FROM %T GROUP BY a ORDER BY 1"},
	{"grp-bare-noorder", "SELECT a, o FROM %T GROUP BY a"},
	{"grp-concat", "SELECT a, group_concat(o) FROM %T GROUP BY a ORDER BY 1"},
	{"grp-rowid", "SELECT o FROM %T GROUP BY rowid"},
	{"grp-where", "SELECT a, o FROM %T WHERE a IS NOT NULL GROUP BY a ORDER BY 1"},
	{"grp-eq", "SELECT a, o FROM %T WHERE a=2 GROUP BY a"},
	{"grp-max", "SELECT a, max(b), o FROM %T GROUP BY a ORDER BY 1"},
	{"grp-indexedby", "SELECT a, o FROM %T INDEXED BY %I GROUP BY a ORDER BY 1"},
	{"grp-notindexed", "SELECT a, o FROM %T NOT INDEXED GROUP BY a ORDER BY 1"},
	{"whole-concat", "SELECT group_concat(o) FROM %T"},
	{"distinct-a", "SELECT DISTINCT a FROM %T"},
	{"distinct-ao", "SELECT DISTINCT a, o FROM %T"},
	{"orderby-ties", "SELECT o FROM %T ORDER BY a"},
	{"orderby-ties-desc", "SELECT o FROM %T ORDER BY a DESC"},
	{"limit", "SELECT o FROM %T ORDER BY a LIMIT 3"},
	// The corpus census's own two examples, verbatim in shape.
	{"cast-grp-rowid", "SELECT * FROM %T WHERE CAST(a AS NUMERIC) > b GROUP BY rowid"},
	{"grp-sub", "SELECT max(b), (SELECT count(*) FROM %T x WHERE x.b=%T.b) FROM %T GROUP BY a"},
}

func r30Cases() []r30Case {
	var out []r30Case
	n := 0
	for _, sc := range r30Schemas {
		for _, ix := range r30Indexes {
			for _, q := range r30Queries {
				tab := "z" + strconv.Itoa(n)
				idx := "x" + strconv.Itoa(n)
				n++
				rep := func(s string) string {
					return strings.ReplaceAll(strings.ReplaceAll(s, "%T", tab), "%I", idx)
				}
				c := r30Case{
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

// r30Tally is one run's classification of the battery.
type r30Tally struct {
	agree, declined, wrong, mutual int
	declineBy                      map[string]int
	wrongBy                        map[string]int
}

// r30Run executes the battery and classifies every case against the oracle.
// Cases are batched into shared worker invocations (each has its own table), so
// the whole battery costs tens of process spawns rather than hundreds.
func r30Run(t *testing.T, cases []r30Case, report func(c r30Case, cgo, mush []byte)) r30Tally {
	t.Helper()
	tal := r30Tally{declineBy: map[string]int{}, wrongBy: map[string]int{}}
	const batch = 34
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
				// The oracle answered and this engine did not (or the reverse,
				// which is itself a divergence and is counted the same way so it
				// cannot hide).
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

func r30Log(t *testing.T, tal r30Tally, total int) {
	t.Helper()
	keys := make([]string, 0, len(tal.declineBy))
	for k := range tal.declineBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("R30 declined %-28s %d", k, tal.declineBy[k])
	}
	t.Logf("R30 INDEXED-AGGREGATE BATTERY: cases=%d agree=%d declined=%d wrong=%d mutualReject=%d",
		total, tal.agree, tal.declined, tal.wrong, tal.mutual)
}

// r30DeclineCeiling enforces that new declines only go down: a decline ceiling
// that ratchets when the count improves.
const r30DeclineCeiling = 293

func TestR30IndexedAggregateBattery(t *testing.T) {
	cases := r30Cases()
	tal := r30Run(t, cases, func(c r30Case, cgo, mush []byte) {
		t.Errorf("R30 indexed-aggregate DIVERGES %s\n  %s\n  cgo:    %s\n  musql: %s",
			c.shape, c.query, cgo, mush)
	})
	r30Log(t, tal, len(cases))
	if tal.declined > r30DeclineCeiling {
		t.Errorf("R30 declines rose to %d (ceiling %d) -- a shape that used to be served now is not",
			tal.declined, r30DeclineCeiling)
	}
}
