package compat

// TestR29SingleTableIndexOrder tests index-selection decisions: which index
// the planner chooses for different constraint shapes over a single table.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

type r29Case struct {
	setup []string
	query string
	shape string
}

// r29Probe enumerates single-table indexed shapes: which index is declared, and
// which constraint (if any) the query puts on it. Each case gets its own table.
func r29Probe() []r29Case {
	idxs := []struct{ name, ddl string }{
		{"idx-a", "CREATE INDEX %I ON %T(a)"},
		{"idx-ab", "CREATE INDEX %I ON %T(a,b)"},
		{"idx-b", "CREATE INDEX %I ON %T(b)"},
		{"idx-a-desc", "CREATE INDEX %I ON %T(a DESC)"},
		{"uniq-b", "CREATE UNIQUE INDEX %I ON %T(b)"},
		{"none", ""},
	}
	qs := []struct{ name, sql string }{
		{"eq-a", "SELECT group_concat(o) FROM %T WHERE a=2"},
		{"gt-a", "SELECT group_concat(o) FROM %T WHERE a>1"},
		{"ge-a", "SELECT group_concat(o) FROM %T WHERE a>=1"},
		{"between-a", "SELECT group_concat(o) FROM %T WHERE a BETWEEN 1 AND 2"},
		{"noconstraint", "SELECT group_concat(o) FROM %T"},
		{"covering-a", "SELECT group_concat(a) FROM %T"},
		{"covering-b", "SELECT group_concat(b) FROM %T"},
		{"in-a", "SELECT group_concat(o) FROM %T WHERE a IN (1,2)"},
		{"notnull-a", "SELECT group_concat(o) FROM %T WHERE a IS NOT NULL"},
		{"limit", "SELECT o FROM %T WHERE a>=1 LIMIT 3"},
		{"grouped-bare", "SELECT a, o FROM %T WHERE a>=1 GROUP BY a ORDER BY 1"},
		{"eq-b", "SELECT group_concat(o) FROM %T WHERE b>'u'"},
	}
	var out []r29Case
	n := 0
	for _, ix := range idxs {
		for _, q := range qs {
			tab := "s" + strconv.Itoa(n)
			idx := "i" + strconv.Itoa(n)
			n++
			rep := func(s string) string {
				return strings.ReplaceAll(strings.ReplaceAll(s, "%T", tab), "%I", idx)
			}
			c := r29Case{shape: ix.name + "/" + q.name}
			// o is a per-row-unique observer whose values are deliberately NOT
			// ascending with rowid (r27's hard-won fixture rule), so index order
			// and rowid order are distinguishable.
			c.setup = []string{
				"CREATE TABLE " + tab + "(a,b,o)",
				"INSERT INTO " + tab + " VALUES(2,'y','r0'),(1,'z','r1'),(2,'x','r2')," +
					"(3,'w','r3'),(1,'v','r4'),(2,'u','r5')",
			}
			if ix.ddl != "" {
				c.setup = append(c.setup, rep(ix.ddl))
			}
			c.query = rep(q.sql)
			out = append(out, c)
		}
	}
	return out
}

// r29Declined names query shapes the planner declines to handle.
var r29Declined = map[string]string{}

func TestR29SingleTableIndexOrder(t *testing.T) {
	cases := r29Probe()
	byShape := map[string]int{}
	total := 0
	const batch = 12
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
			cb, _ := json.Marshal(cgo[qAt[k]])
			mb, _ := json.Marshal(mush[qAt[k]])
			qlab := c.shape[strings.IndexByte(c.shape, '/')+1:]
			why, declined := r29Declined[qlab]
			switch {
			case string(cb) != string(mb) && !declined:
				byShape[c.shape]++
				total++
				t.Errorf("R29 single-table index order DIVERGES %s\n  %s\n  cgo:    %s\n  musql: %s",
					c.shape, c.query, cb, mb)
			case string(cb) != string(mb):
				byShape[c.shape]++
				total++
				t.Logf("R29 declined as expected %-24s (%s)", c.shape, why)
			case declined:
				t.Logf("R29 shape %s now AGREES though it is declined (%s) -- an "+
					"UPGRADE if the port now covers it, or a fixture that stopped "+
					"discriminating; re-check r29Declined", c.shape, why)
			}
		}
	}
	keys := make([]string, 0, len(byShape))
	for k := range byShape {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("R29 shape %-24s %d", k, byShape[k])
	}
	t.Logf("R29 SINGLE-TABLE INDEX ORDER TOTAL: %d/%d", total, len(cases))
}
