package engine

import (
	"strconv"
	"testing"
)

// TestSortedDrainRecordDropsSubtype verifies that SORTED GROUP BY drain drops subtype
// from grouped rows to match C SQLite's serialization behavior.
func TestSortedDrainRecordDropsSubtype(t *testing.T) {
	p := argRegsFixture(t)
	// Descending order forces the SORTED drain (HASH does ascending only).
	for _, tc := range []struct {
		sql        string
		wantStamps []string
		wantAgg    string // the first group's aggregate cell
		why        string
	}{
		{`SELECT key, max(subtype(value)) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key DESC`,
			[]string{"-A-", "-A-"}, "0",
			"a subtype CONSUMER: the drained row has lost the subtype, so subtype() reports 0 -- 74 is what an uncleared register answers, and it was a live wrong answer"},
		{`SELECT key, group_concat(json_quote(value)) FROM json_each('[[7]]') GROUP BY key ORDER BY key DESC`,
			[]string{"-A-"}, `"[7]"`,
			"a subtype GENERATOR: json_quote of a value that LOST its subtype quotes the text, where an uncleared one embeds the array verbatim as [7]"},
		{`SELECT key, json_group_array(value) FROM json_each('[[7]]') GROUP BY key ORDER BY key DESC`,
			[]string{"-A-"}, `["[7]"]`,
			"...and the same loss reaches json_group_array's own embedding decision"},
	} {
		prog, plan := compiledAgg(t, p, tc.sql)

		// The opcode field itself, so the assertion survives a change that
		// keeps these answers by some other route.
		found := false
		for _, in := range prog.Insns {
			if in.Op != OpSorterData || in.P3 == 0 {
				continue // P3 == 0: the ORDER BY sorter's own drain, not this one
			}
			found = true
			if in.P3 != plan.nCols {
				t.Errorf("%s: OpSorterData drops the subtype on %d leading values, want the row's %d columns", tc.sql, in.P3, plan.nCols)
			}
		}
		if !found {
			t.Errorf("%s: no OpSorterData asks its record to drop the subtype", tc.sql)
		}

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
		agg := rows[0][len(rows[0])-1]
		cell := string(agg.S)
		if agg.Typ == Int {
			cell = strconv.FormatInt(agg.I, 10)
		}
		if cell != tc.wantAgg {
			t.Errorf("%s: the first group's aggregate is %q, want %q (%s)", tc.sql, cell, tc.wantAgg, tc.why)
		}
	}
}
