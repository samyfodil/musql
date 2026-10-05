package compat

// This file tests USING and NATURAL join planning against ON-join equivalents.
// back through channels that observe scan order (a bare column with no ORDER BY,
// group_concat, LIMIT). The ON arm is the control -- it agreed before the fix
// and must keep agreeing -- and any divergence between the two arms over the
// same data is a plan the coalescing changed and should not have.
//
// R36B_DUMP=1 prints every generated statement.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r36bData is chosen so rowid order, ascending-a order and index-(b,a) order are
// three DIFFERENT permutations of t, and so a and b disagree about which row is
// first -- a fixture where they coincide agrees for the wrong reason. The
// INTEGER/REAL pair (1 and 1.0) is what makes an index-order scan visible at all:
// the two compare EQUAL, so only the arrival order distinguishes them, and only
// a readout that shows storage class or the row's other columns reports it.
// z carries a duplicate and a NULL key so the inner loop is not one-to-one.
const r36bData = `
CREATE TABLE t(a,b,c);
INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60);
CREATE TABLE z(a,d);
INSERT INTO z VALUES(1,'p'),(2,'q'),(3,'r'),(2,'s'),(NULL,'u');
CREATE TABLE w(a,e);
INSERT INTO w VALUES(1,'k'),(2,'m'),(3,'n');
`

// r36bSchemas are index placements. "dead" is an index no query mentions: it adds
// no WhereLoop in whereLoopAddBtree, so the oracle's plan is byte-identical to
// the no-index one -- but it is still a REAL index, which is what the declined
// covering test was keyed on.
var r36bSchemas = []struct{ name, ddl string }{
	{"noidx", ``},
	{"t-a", `CREATE INDEX i1 ON t(a);`},
	{"t-a-desc", `CREATE INDEX i1 ON t(a DESC);`},
	{"t-ab", `CREATE INDEX i1 ON t(a,b);`},
	{"t-ba", `CREATE INDEX i1 ON t(b,a);`},
	{"t-cover", `CREATE INDEX i1 ON t(a,b,c);`},
	{"t-dead", `CREATE INDEX i1 ON t(c);`},
	{"t-uniq-b", `CREATE UNIQUE INDEX i1 ON t(b);`},
	{"t-two", `CREATE INDEX i1 ON t(a); CREATE INDEX i2 ON t(b);`},
	{"z-a", `CREATE INDEX j1 ON z(a);`},
	{"both", `CREATE INDEX i1 ON t(a,b); CREATE INDEX j1 ON z(a,d);`},
}

// r36bFroms pairs each coalescing spelling with the ON spelling of the SAME
// join. selfJoin marks the ones whose second item is another copy of t, which
// have no z/w column to read.
var r36bFroms = []struct {
	name     string
	from     string
	selfJoin bool
}{
	{"on", `t JOIN z ON t.a = z.a`, false},
	{"on-left", `t LEFT JOIN z ON t.a = z.a`, false},
	{"using", `t JOIN z USING (a)`, false},
	{"using-left", `t LEFT JOIN z USING (a)`, false},
	{"natural", `t NATURAL JOIN z`, false},
	{"natural-left", `t NATURAL LEFT JOIN z`, false},
	{"using-cross", `t CROSS JOIN z USING (a)`, false},
	{"using-chain", `t JOIN z USING (a) JOIN w USING (a)`, false},
	{"using-self", `t JOIN t AS u USING (a)`, true},
	// An UNALIASED self-join: the scope name reaches BOTH items, so
	// desugarJoinItem addresses the right-hand side by FROM-item index
	// (ColumnExpr.UsingPinned) and leaves the representative qualified. Read as
	// an ordinary name, both sides collapse onto item 0.
	{"using-self-bare", `t JOIN t USING (a)`, true},
	{"natural-self", `t NATURAL JOIN t AS u`, true},
}

// r36bReads observe SCAN ORDER, which is the whole point: a plan difference that
// no readout reports is not a wrong answer. Every entry names only t's columns
// and the coalesced "a", so it is legal under a self-join too.
var r36bReads = []struct{ name, sel string }{
	{"leftcol", `t.b`},
	{"coalesced", `a`},
	{"coalesced-typed", `a, typeof(a)`},
	{"pair", `t.b, t.c`},
	{"star", `*`},
	{"gc", `group_concat(t.b)`},
	{"gc-coal", `group_concat(quote(a), '/')`},
	{"count-gc", `count(*), group_concat(t.c)`},
	{"magnet", `t.b, max(t.c)`},
}

var r36bTails = []struct{ name, tail string }{
	{"none", ``},
	{"limit", `LIMIT 3`},
	{"where-notnull", `WHERE a IS NOT NULL`},
	{"where-eq", `WHERE a = 2`},
	{"group", `GROUP BY a`},
	{"order-b", `ORDER BY t.b`},
}

// r36bOpenTail names tails whose divergences are NOT about coalescing at all:
// each reproduces byte-for-byte in an ON-spelled join, which is why it is
// logged rather than failed here -- this battery is about what USING and NATURAL
// change, and that is not it. Such a tail is only VISIBLE through a coalesced
// column because its ON spelling of the same statement is rejected as
// ambiguous, which is exactly why no earlier battery found it.
//
// It is EMPTY: both tails it held are fixed, and all 1089 cells are asserted.
//
//   - "where-eq": WHERE-clause CONSTANT PROPAGATION (select.c tag-select-0330),
//     which rewrites the term list before the planner runs. It is ported
//     (where_plan_constprop.go).
//   - "group": GROUP BY emission order over a JOIN whose outer table is walked
//     through a DESC index -- "SELECT group_concat(t.b) FROM t JOIN z ON
//     t.a=z.a GROUP BY t.a" over "CREATE INDEX i1 ON t(a DESC)" emitted its
//     groups ascending where the oracle emits them descending (81 cells). The
//     multi-table plan now carries select.c:8545's groupBySort==0 verdict; see
//     groupord_join_desc_test.go.
//
// A new entry is for a tail that is somebody else's bug; when it is fixed,
// DELETE it -- the test logs the cells that started agreeing so the entry
// cannot rot unnoticed.
var r36bOpenTail = map[string]string{}

// TestR36bJoinUsingBattery asserts the ORACLE's answer for every cell, never
// "this declines": the day a shape starts being served, the gate must check what
// it serves rather than turn red for having been fixed.
func TestR36bJoinUsingBattery(t *testing.T) {
	if testing.Short() {
		t.Skip("USING/NATURAL join battery: full run")
	}
	dump := os.Getenv("R36B_DUMP") != ""
	var cells, wrong int
	open := map[string]int{}
	for _, sc := range r36bSchemas {
		for _, fr := range r36bFroms {
			setup := []string{}
			for _, s := range strings.Split(r36bData+sc.ddl, ";") {
				if s = strings.TrimSpace(s); s != "" {
					setup = append(setup, s)
				}
			}
			var queries []string
			var labels []string
			var tails []string
			for _, rd := range r36bReads {
				for _, tl := range r36bTails {
					q := "SELECT " + rd.sel + " FROM " + fr.from
					if tl.tail != "" {
						q += " " + tl.tail
					}
					queries = append(queries, q)
					tails = append(tails, tl.name)
					labels = append(labels, sc.name+"/"+fr.name+"/"+rd.name+"/"+tl.name)
				}
			}
			stmts := append(append([]string{}, setup...), queries...)
			if dump {
				for _, q := range queries {
					t.Log(q)
				}
			}
			m := run(t, "musql", stmts)
			cg := run(t, "cgo", stmts)
			for i := range queries {
				j := len(setup) + i
				cells++
				mb, _ := json.Marshal(m[j])
				cb, _ := json.Marshal(cg[j])
				reason, isOpen := r36bOpenTail[tails[i]]
				if string(mb) == string(cb) {
					if isOpen {
						open[tails[i]+" (AGREES)"]++
					}
					continue
				}
				if isOpen {
					open[tails[i]]++
					continue
				}
				wrong++
				t.Errorf("R36B [%s] DIVERGES\n  sql:    %s\n  cgo:    %s\n  musql: %s\n  %s",
					labels[i], queries[i], cb, mb, reason)
			}
		}
	}
	t.Logf("R36B USING/NATURAL battery: cells=%d wrong=%d", cells, wrong)
	for _, tl := range r36bTails {
		if _, isOpen := r36bOpenTail[tl.name]; !isOpen {
			continue
		}
		still, agrees := open[tl.name], open[tl.name+" (AGREES)"]
		t.Logf("  open family %-9s diverging=%d agreeing=%d -- %s",
			tl.name, still, agrees, r36bOpenTail[tl.name])
		if still == 0 {
			t.Errorf("open family %q no longer diverges anywhere: DELETE its r36bOpenTail entry so these %d cells are asserted again",
				tl.name, agrees)
		}
	}
}
