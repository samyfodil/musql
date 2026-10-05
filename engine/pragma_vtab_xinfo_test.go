package engine_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestPragmaTableXInfoOnAVirtualTable: table_xinfo on virtual tables.
func TestPragmaTableXInfoOnAVirtualTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
		`CREATE VIRTUAL TABLE f USING fts4(x)`,
		`CREATE VIRTUAL TABLE fl USING fts4(a,b,languageid=l)`,
		`CREATE VIRTUAL TABLE g USING fts5(x,y)`,
		`CREATE VIRTUAL TABLE fa USING fts4aux('f')`,
		`CREATE VIRTUAL TABLE ftk USING fts3tokenize('simple')`,
	} {
		mustExec(t, db, s)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, tc := range []struct {
		tbl  string
		want []string // "cid|name|type|hidden"
	}{
		{"r", []string{"0|id|INT|0", "1|x0|REAL|0", "2|x1|REAL|0"}},
		{"f", []string{"0|x||0", "1|f||1", "2|docid||1", "3|__langid||1"}},
		{"fl", []string{"0|a||0", "1|b||0", "2|fl||1", "3|docid||1", "4|l||1"}},
		{"g", []string{"0|x||0", "1|y||0", "2|g||1", "3|rank||1"}},
		{"fa", []string{"0|term||0", "1|col||0", "2|documents||0", "3|occurrences||0", "4|languageid||1"}},
		{"ftk", []string{"0|input||0", "1|token||0", "2|start||0", "3|end||0", "4|position||0"}},
	} {
		cols, rows, err := p.Query(`PRAGMA table_xinfo(` + tc.tbl + `)`)
		if err != nil {
			t.Errorf("table_xinfo(%s): %v", tc.tbl, err)
			continue
		}
		if got := strings.Join(cols, ","); got != "cid,name,type,notnull,dflt_value,pk,hidden" {
			t.Errorf("table_xinfo(%s): columns = %s", tc.tbl, got)
		}
		got := make([]string, len(rows))
		for i, r := range rows {
			if len(r) != 7 {
				t.Fatalf("table_xinfo(%s): row %d has %d columns", tc.tbl, i, len(r))
			}
			// notnull/dflt_value/pk are 0/NULL/0 for every virtual-table
			// column, rtree's rowid-alias id included -- verified.
			if r[3].I != 0 || r[4].Typ != engine.Null || r[5].I != 0 {
				t.Errorf("table_xinfo(%s) row %d: notnull/dflt/pk = %v/%v/%v, want 0/NULL/0",
					tc.tbl, i, r[3], r[4], r[5])
			}
			got[i] = strings.Join([]string{
				strconv.FormatInt(r[0].I, 10), string(r[1].S), string(r[2].S), strconv.FormatInt(r[6].I, 10),
			}, "|")
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("table_xinfo(%s):\n got %v\nwant %v", tc.tbl, got, tc.want)
		}
	}

	// The eponymous TVF answers identically, by construction (it routes
	// through the same queryPragmaStmt) -- pinned because "by construction"
	// has a way of becoming "by two implementations agreeing".
	_, trows, err := p.Query(`SELECT cid,name,hidden FROM pragma_table_xinfo('f')`)
	if err != nil {
		t.Fatalf("pragma_table_xinfo('f'): %v", err)
	}
	var tgot []string
	for _, r := range trows {
		tgot = append(tgot, strconv.FormatInt(r[0].I, 10)+"|"+string(r[1].S)+"|"+strconv.FormatInt(r[2].I, 10))
	}
	if strings.Join(tgot, " ") != "0|x|0 1|f|1 2|docid|1 3|__langid|1" {
		t.Errorf("pragma_table_xinfo('f') = %v", tgot)
	}

	// table_INFO still reports the visible columns alone, unchanged.
	_, rows, err := p.Query(`PRAGMA table_info(f)`)
	if err != nil {
		t.Fatalf("table_info(f): %v", err)
	}
	if len(rows) != 1 || string(rows[0][1].S) != "x" {
		t.Errorf("table_info(f) = %v, want the one visible column x", rows)
	}
}
