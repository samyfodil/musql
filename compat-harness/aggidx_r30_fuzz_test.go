package compat

// Package compat tests indexed aggregate ordering with randomized schemas.

import (
	"encoding/json"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r30FuzzCases is how many random (schema x index x query) draws to make.
// Overridable so a bisect can widen the search without editing the file.
func r30FuzzCases() int {
	if s := os.Getenv("R30_FUZZ_N"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 400
}

type r30Col struct {
	name string
	decl string // "" | "INT" | "TEXT" | "REAL"
	coll string // "" | "NOCASE" | "RTRIM"
	nn   bool
	ipk  bool
}

// r30Schema draws one table: 3..5 columns in a random order, one of which is
// always the per-row-unique observer "o" whose values are deliberately NOT
// ascending with rowid, so index order and rowid order are distinguishable.
func r30Schema(rng *rand.Rand, tab string) ([]r30Col, []string) {
	n := 3 + rng.Intn(3)
	cols := make([]r30Col, n)
	names := []string{"a", "b", "c", "d", "e"}
	for i := 0; i < n; i++ {
		cols[i] = r30Col{name: names[i]}
		switch rng.Intn(4) {
		case 1:
			cols[i].decl = "INT"
		case 2:
			cols[i].decl = "TEXT"
		case 3:
			cols[i].decl = "REAL"
		}
		if cols[i].decl == "TEXT" || cols[i].decl == "" {
			switch rng.Intn(4) {
			case 1:
				cols[i].coll = "NOCASE"
			case 2:
				cols[i].coll = "RTRIM"
			}
		}
		cols[i].nn = rng.Intn(4) == 0
	}
	// The observer goes at a RANDOM position, which is the fixture rule a
	// previous round learned the hard way: a sweep whose tables all declare the
	// interesting column first measures one shape.
	oAt := rng.Intn(n)
	cols[oAt] = r30Col{name: "o", decl: "TEXT", nn: true}
	// An INTEGER PRIMARY KEY on one of the others, sometimes.
	if rng.Intn(3) == 0 {
		for i := range cols {
			if i != oAt {
				cols[i] = r30Col{name: cols[i].name, decl: "INTEGER", ipk: true, nn: true}
				break
			}
		}
	}

	var decls []string
	for _, c := range cols {
		d := c.name
		if c.decl != "" {
			d += " " + c.decl
		}
		if c.ipk {
			d += " PRIMARY KEY"
		}
		if c.coll != "" {
			d += " COLLATE " + c.coll
		}
		if c.nn && !c.ipk {
			d += " NOT NULL"
		}
		decls = append(decls, d)
	}
	setup := []string{"CREATE TABLE " + tab + "(" + strings.Join(decls, ",") + ")"}

	// Six rows with heavy duplication in the non-observer columns (so groups and
	// ORDER BY ties actually exist), mixed case (so NOCASE bites) and NULLs
	// wherever the column allows them.
	vals := []string{"2", "1", "2", "3", "1", "2"}
	txt := []string{"'Y'", "'z '", "'x'", "'W'", "'z'", "'u'"}
	var rows []string
	for r := 0; r < 6; r++ {
		var cells []string
		for _, c := range cols {
			switch {
			case c.name == "o":
				cells = append(cells, "'r"+strconv.Itoa(r)+"'")
			case c.ipk:
				cells = append(cells, strconv.Itoa((r+1)*10))
			case c.decl == "TEXT" || c.decl == "":
				if !c.nn && r == 3 {
					cells = append(cells, "NULL")
				} else {
					cells = append(cells, txt[r])
				}
			default:
				if !c.nn && r == 1 {
					cells = append(cells, "NULL")
				} else {
					cells = append(cells, vals[r])
				}
			}
		}
		rows = append(rows, "("+strings.Join(cells, ",")+")")
	}
	setup = append(setup, "INSERT INTO "+tab+" VALUES"+strings.Join(rows, ","))
	return cols, setup
}

// r30Index draws 0..2 indexes over the drawn columns.
func r30Index(rng *rand.Rand, tab, idxBase string, cols []r30Col) ([]string, string) {
	n := rng.Intn(3)
	var out []string
	first := ""
	for k := 0; k < n; k++ {
		width := 1 + rng.Intn(2)
		perm := rng.Perm(len(cols))
		var parts []string
		for w := 0; w < width && w < len(perm); w++ {
			c := cols[perm[w]]
			p := c.name
			if (c.decl == "TEXT" || c.decl == "") && rng.Intn(3) == 0 {
				p += " COLLATE " + []string{"NOCASE", "RTRIM", "BINARY"}[rng.Intn(3)]
			}
			if rng.Intn(2) == 0 {
				p += " DESC"
			}
			parts = append(parts, p)
		}
		uniq := ""
		if rng.Intn(4) == 0 && width == 1 {
			uniq = "UNIQUE "
		}
		name := idxBase + strconv.Itoa(k)
		if first == "" {
			first = name
		}
		out = append(out, "CREATE "+uniq+"INDEX "+name+" ON "+tab+"("+strings.Join(parts, ",")+")")
	}
	return out, first
}

// r30Query draws one statement whose answer reports the arrival order.
func r30Query(rng *rand.Rand, tab, firstIdx string, cols []r30Col) string {
	var plain []string
	for _, c := range cols {
		if c.name != "o" {
			plain = append(plain, c.name)
		}
	}
	pick := func() string { return plain[rng.Intn(len(plain))] }
	where := ""
	switch rng.Intn(5) {
	case 1:
		where = " WHERE " + pick() + " IS NOT NULL"
	case 2:
		where = " WHERE " + pick() + "=2"
	case 3:
		where = " WHERE " + pick() + ">1"
	case 4:
		where = " WHERE " + pick() + " IS NULL"
	}
	hint := ""
	switch {
	case rng.Intn(8) == 0:
		hint = " NOT INDEXED"
	case firstIdx != "" && rng.Intn(8) == 0:
		hint = " INDEXED BY " + firstIdx
	}
	dir := func() string {
		switch rng.Intn(4) {
		case 1:
			return " DESC"
		case 2:
			return " DESC NULLS LAST"
		case 3:
			return " ASC NULLS LAST"
		}
		return ""
	}
	g := pick()
	src := tab + hint + where
	switch rng.Intn(8) {
	case 0:
		return "SELECT group_concat(o) FROM " + src + " GROUP BY " + g + " ORDER BY 1"
	case 1:
		return "SELECT " + g + ", group_concat(o) FROM " + src + " GROUP BY " + g + " ORDER BY 1" + dir()
	case 2:
		return "SELECT o FROM " + src + " ORDER BY " + g + dir()
	case 3:
		return "SELECT o FROM " + src + " ORDER BY " + g + dir() + ", " + pick() + dir()
	case 4:
		return "SELECT DISTINCT " + g + " FROM " + src
	case 5:
		return "SELECT DISTINCT " + g + ", o FROM " + src
	case 6:
		return "SELECT o FROM " + src + " ORDER BY " + g + dir() + " LIMIT 3"
	default:
		return "SELECT group_concat(o) FROM " + src
	}
}

func TestR30IndexedAggregateFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(0x30a99))
	n := r30FuzzCases()
	type fcase struct {
		setup []string
		query string
	}
	cases := make([]fcase, 0, n)
	for i := 0; i < n; i++ {
		tab := "f" + strconv.Itoa(i)
		cols, setup := r30Schema(rng, tab)
		idx, first := r30Index(rng, tab, "fi"+strconv.Itoa(i)+"_", cols)
		setup = append(setup, idx...)
		cases = append(cases, fcase{setup: setup, query: r30Query(rng, tab, first, cols)})
	}

	agree, declined, wrong, mutual := 0, 0, 0, 0
	byKind := map[string]int{}
	// One worker process per batch, per engine: see the battery's own note.
	const batch = 25
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
			cErr, mErr := cr["kind"] == "error", mr["kind"] == "error"
			switch {
			case cErr && mErr:
				mutual++
			case cErr != mErr:
				declined++
				byKind[r30QueryKind(c.query)]++
			case string(cb) != string(mb):
				wrong++
				t.Errorf("R30 fuzz DIVERGES\n  setup:  %s\n  query:  %s\n  cgo:    %s\n  musql: %s",
					strings.Join(c.setup, ";\n          "), c.query, cb, mb)
			default:
				agree++
			}
		}
	}
	keys := make([]string, 0, len(byKind))
	for k := range byKind {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("R30 fuzz declined %-14s %d", k, byKind[k])
	}
	t.Logf("R30 INDEXED-ORDER FUZZ: cases=%d agree=%d declined=%d wrong=%d mutualReject=%d",
		len(cases), agree, declined, wrong, mutual)
	if wrong != 0 {
		t.Errorf("R30 fuzz: %d of %d served answers differ from the oracle", wrong, len(cases))
	}
}

// r30QueryKind labels a drawn query for the decline histogram.
func r30QueryKind(q string) string {
	switch {
	case strings.Contains(q, "GROUP BY"):
		return "grouped"
	case strings.Contains(q, "DISTINCT"):
		return "distinct"
	case strings.Contains(q, "LIMIT"):
		return "limit"
	case strings.Contains(q, "ORDER BY"):
		return "orderby"
	}
	return "whole"
}
