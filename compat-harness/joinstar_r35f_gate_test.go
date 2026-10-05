package compat

// This file gates FROM clauses with more than three items. Fuzz testing verifies
// plan ordering against the oracle.

import (
	"encoding/json"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

type r35fStarCase struct {
	setup []string
	query string
	shape string
}

// r35fGen builds one star-ish join of 4..6 tables: a FACT table whose key column
// is equated to a key column of each DIMENSION table, read back through a
// channel that observes ARRIVAL ORDER (group_concat, a bare column under GROUP
// BY, LIMIT with no totally-ordering ORDER BY).
//
// What it varies, because each is an input to a different part of the routine
// being ported: how many dimensions are actually connected (nDep <= 2 is
// constraint (cc), so a 4-table join with only two connected dimensions must
// take the OTHER branch), whether a dimension carries an index (an index-driven
// loop has nLTerm != 0 and so is exempt from the rRun rewrite), whether the fact
// table repeats (the self-join guard), and whether a CROSS or LEFT join sits in
// the middle (constraint (bb) stops the dimension search at the first one).
func r35fGen(rng *rand.Rand, id int) r35fStarCase {
	sfx := strconv.Itoa(id)
	nTab := 4 + rng.Intn(3) // 4..6
	names := make([]string, nTab)
	for i := range names {
		names[i] = "sf" + sfx + "_" + strconv.Itoa(i)
	}
	c := r35fStarCase{}
	// Table 0 is the fact table; the rest are dimensions. Each has a key column
	// k and an observer column o, plus a dead column d so an index can exist
	// that serves nothing.
	for i, n := range names {
		typ := []string{"", " INT", " TEXT"}[rng.Intn(3)]
		c.setup = append(c.setup,
			"CREATE TABLE "+n+"(k"+typ+", o TEXT, d INT)")
		switch rng.Intn(4) {
		case 0:
			c.setup = append(c.setup, "CREATE INDEX x"+n+" ON "+n+"(k)")
		case 1:
			c.setup = append(c.setup, "CREATE INDEX x"+n+" ON "+n+"(d)")
		case 2:
			c.setup = append(c.setup, "CREATE INDEX x"+n+" ON "+n+"(k,o)")
		}
		nRow := 2 + rng.Intn(4)
		tag := rng.Perm(nRow)
		rows := make([]string, nRow)
		for r := 0; r < nRow; r++ {
			key := []string{"1", "2", "3", "NULL", "'2'"}[rng.Intn(5)]
			rows[r] = "(" + key + ",'" + string(rune('a'+i)) + strconv.Itoa(tag[r]) + "'," +
				strconv.Itoa(rng.Intn(4)) + ")"
		}
		c.setup = append(c.setup, "INSERT INTO "+n+" VALUES"+strings.Join(rows, ","))
	}

	// A self-join replaces one dimension with a second alias of the fact table.
	selfJoin := rng.Intn(4) == 0
	from := make([]string, nTab)
	alias := make([]string, nTab)
	for i := range names {
		alias[i] = "t" + strconv.Itoa(i)
		src := names[i]
		if selfJoin && i == nTab-1 {
			src = names[0]
		}
		from[i] = src + " " + alias[i]
	}
	// Constraint (bb): a CROSS or LEFT join stops the dimension search.
	sep := ","
	joinAt := -1
	switch rng.Intn(4) {
	case 0:
		joinAt = 1 + rng.Intn(nTab-1)
		sep = " CROSS JOIN "
	case 1:
		joinAt = 1 + rng.Intn(nTab-1)
		sep = " LEFT JOIN "
	}
	fromClause := from[0]
	for i := 1; i < nTab; i++ {
		if i == joinAt {
			fromClause += sep + from[i]
			if sep == " LEFT JOIN " {
				fromClause += " ON " + alias[0] + ".k=" + alias[i] + ".k"
			}
			continue
		}
		fromClause += "," + from[i]
	}

	// nDep: how many dimensions are actually equated to the fact table.
	nDep := 1 + rng.Intn(nTab-1)
	var conj []string
	for i := 1; i <= nDep && i < nTab; i++ {
		if i == joinAt && sep == " LEFT JOIN " {
			continue // its ON clause already carries the equality
		}
		conj = append(conj, alias[0]+".k="+alias[i]+".k")
	}
	if rng.Intn(3) == 0 {
		conj = append(conj, alias[nTab-1]+".d>=0")
	}
	where := ""
	if len(conj) > 0 {
		where = " WHERE " + strings.Join(conj, " AND ")
	}

	obs := alias[rng.Intn(nTab)] + ".o"
	switch rng.Intn(4) {
	case 0:
		c.query = "SELECT group_concat(" + obs + ") FROM " + fromClause + where
		c.shape = "gc-flat"
	case 1:
		c.query = "SELECT " + alias[0] + ".k, count(*), group_concat(" + obs + ") FROM " +
			fromClause + where + " GROUP BY " + alias[0] + ".k ORDER BY 1"
		c.shape = "gc-grouped"
	case 2:
		c.query = "SELECT " + alias[0] + ".k, count(*), " + obs + " FROM " +
			fromClause + where + " GROUP BY " + alias[0] + ".k ORDER BY 1"
		c.shape = "bare-grouped"
	default:
		c.query = "SELECT " + obs + ", " + alias[0] + ".k FROM " + fromClause + where + " LIMIT 4"
		c.shape = "limit"
	}
	c.shape += "/" + strconv.Itoa(nTab) + "tab/dep" + strconv.Itoa(nDep)
	if selfJoin {
		c.shape += "/self"
	}
	return c
}

func TestR35FStarJoinGate(t *testing.T) {
	nCase := r31Env("R35F_CASES", 300)
	rng := rand.New(rand.NewSource(int64(r31Env("R35F_SEED", 35))))
	cases := make([]r35fStarCase, nCase)
	for i := range cases {
		cases[i] = r35fGen(rng, i)
	}

	const batch = 15
	byShape := map[string]int{}
	total := 0
	var firstBad string
	for start := 0; start < nCase; start += batch {
		end := start + batch
		if end > nCase {
			end = nCase
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
			i := qAt[k]
			cb, _ := json.Marshal(cgo[i])
			mb, _ := json.Marshal(mush[i])
			if string(cb) == string(mb) {
				continue
			}
			total++
			byShape[c.shape]++
			if firstBad == "" {
				firstBad = strings.Join(c.setup, "; ") + "\n      " + c.query +
					"\n      cgo:    " + string(cb) + "\n      musql: " + string(mb)
			}
		}
	}
	if total == 0 {
		t.Logf("R35F star-join gate: %d/%d shapes agree with the oracle", nCase, nCase)
		return
	}
	keys := make([]string, 0, len(byShape))
	for k := range byShape {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("R35F diverge %-28s %d", k, byShape[k])
	}
	t.Fatalf("R35F star-join gate: %d/%d shapes diverge from the oracle.\n"+
		"  A FROM clause of four or more items is planned by the ported\n"+
		"  computeMxChoice (engine/where_plan_index.go). Read that routine\n"+
		"  against where.c:5652 before touching this test -- and if the port is\n"+
		"  RIGHT and this shape is simply outside it, make the port DECLINE the\n"+
		"  shape (which restores this engine's own FROM order); never relax the\n"+
		"  assertion.\n  first: %s", total, nCase, firstBad)
}
