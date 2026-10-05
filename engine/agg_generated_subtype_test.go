package engine

import (
	"path/filepath"
	"testing"
)

// Tests that generated columns in aggregates correctly preserve or clear subtypes.
// Generated columns computed per read (not decoded from storage) can carry
// function subtypes. Both the generated code and the query results are verified.
func TestGeneratedColumnRowCanCarrySubtype(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gen.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE tg(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, g AS (json_array(v)))`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ExecArgs(`INSERT INTO tg(id,k,v) VALUES(1,0,5),(2,0,6),(3,1,7)`, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	// The control: with no grouping there is no record and the subtype SURVIVES,
	// so json_quote embeds the array. This is what makes the row block able to
	// carry one in the first place.
	_, rows, err := p.QueryArgs(`SELECT json_quote(g) FROM tg WHERE id = 1`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rows[0][0].S); got != "[5]" {
		t.Fatalf("ungrouped json_quote(g) = %q, want %q -- a generated column's value must still carry its subtype, or this whole test is vacuous", got, "[5]")
	}

	for _, tc := range []struct {
		sql        string
		wantStamps []string
		wantAgg    string
		why        string
	}{
		{`SELECT k, group_concat(json_quote(g)) FROM tg GROUP BY k`,
			[]string{"-A-"}, `"[5]","[6]"`,
			"the HASH path lowers a subtype generator over a row that can carry one, because the LEAF it reads lost the subtype first (OpClearSubtype) -- so json_quote quotes the text exactly as it does off the cleared row"},
		{`SELECT k, group_concat(json_quote(g)) FROM tg GROUP BY k ORDER BY k`,
			[]string{"-A-"}, `"[5]","[6]"`,
			"the SORTED DRAIN lowers it instead -- its record already dropped the subtype (OpSorterData's P3) -- and must reach the same answer"},
	} {
		_, plan := compiledAgg(t, p, tc.sql)
		got := stamps(plan)
		if len(got) != len(tc.wantStamps) {
			t.Fatalf("%s: %d accumulator templates %v, want %d", tc.sql, len(got), got, len(tc.wantStamps))
		}
		for i := range got {
			if got[i] != tc.wantStamps[i] {
				t.Errorf("%s: template %d lowered %q, want %q (%s)", tc.sql, i, got[i], tc.wantStamps[i], tc.why)
			}
		}
		_, rows, err := p.QueryArgs(tc.sql, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if len(rows) == 0 {
			t.Fatalf("%s: no rows", tc.sql)
		}
		if cell := string(rows[0][len(rows[0])-1].S); cell != tc.wantAgg {
			t.Errorf("%s: the first group's aggregate is %q, want %q (%s)", tc.sql, cell, tc.wantAgg, tc.why)
		}
	}
}
