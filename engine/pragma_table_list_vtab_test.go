package engine_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestPragmaTableListWithVirtualTables tests PRAGMA table_list with virtual tables,
// including column counts and shadow table classification.
func TestPragmaTableListWithVirtualTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tl.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE plain(a, b)`,
		`CREATE VIRTUAL TABLE vr USING rtree(id, x0, x1)`,
		`CREATE VIRTUAL TABLE vf USING fts4(aa, bb)`,
		`CREATE VIRTUAL TABLE vl USING fts4(a, b, languageid=l)`,
		`CREATE VIRTUAL TABLE va USING fts4aux('vf')`,
		`CREATE VIRTUAL TABLE vk USING fts3tokenize('simple')`,
		// A name that merely SHARES the rtree's prefix is NOT a shadow: the
		// suffix has to be one the module's xShadowName accepts. Matching the
		// prefix alone would mistype this.
		`CREATE TABLE vr_wibble(z)`,
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

	_, rows, err := p.Query(`PRAGMA table_list`)
	if err != nil {
		t.Fatalf("PRAGMA table_list: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[string(r[1].S)] = string(r[2].S) + "/" + strconv.FormatInt(r[3].I, 10)
	}
	for _, tc := range []struct{ name, want string }{
		{"plain", "table/2"},
		// ncol is the DECLARED count: rtree declares no hidden columns, fts4
		// declares three (<table>, docid, __langid), fts4aux four visible
		// plus a hidden languageid, fts3tokenize five with none hidden.
		{"vr", "virtual/3"},
		{"vf", "virtual/5"},
		{"vl", "virtual/5"},
		{"va", "virtual/5"},
		{"vk", "virtual/5"},
		// rtree's three shadows exist at all only because it writes them now
		// (rtree_shadow.go); before that the whole listing declined.
		{"vr_node", "shadow/2"},
		{"vr_parent", "shadow/2"},
		{"vr_rowid", "shadow/2"},
		{"vf_content", "shadow/3"},
		{"vf_segdir", "shadow/6"},
		{"vf_stat", "shadow/2"},
		{"vr_wibble", "table/1"},
	} {
		if got[tc.name] != tc.want {
			t.Errorf("table_list row for %s is %q, want %q", tc.name, got[tc.name], tc.want)
		}
	}
	// ...and the single-name form agrees with the full listing.
	_, one, err := p.Query(`PRAGMA table_list(vr)`)
	if err != nil {
		t.Fatalf("PRAGMA table_list(vr): %v", err)
	}
	if len(one) != 1 || string(one[0][2].S) != "virtual" || one[0][3].I != 3 {
		t.Errorf("PRAGMA table_list(vr) = %v, want one virtual row with ncol 3", one)
	}
	if !strings.Contains(strings.Join([]string{got["vr"]}, ""), "virtual") {
		t.Errorf("vr is not reported virtual")
	}
}
