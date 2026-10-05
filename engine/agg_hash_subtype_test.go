package engine

import (
	"strconv"
	"testing"
)

// A hash-grouped query loses the JSON subtype on its aggregate arguments, as
// C SQLite's do (they are read back from a serialized sorter record). This
// engine clears it at each column read in the argument block
// (compiler.stripRowSubtype). Each case asserts the clear opcode, that the
// expression compiled, and the oracle's answer; the differential twin is
// compat-harness/agg_subtype_narrowing_test.go.
func TestHashAggLeavesDropSubtype(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql        string
		wantClears bool
		wantStamps []string
		wantAgg    string // the first group's aggregate cell
		why        string
	}{
		{`SELECT key, max(subtype(value)) FROM json_each('[[7],[8]]') GROUP BY key`,
			true, []string{"-A-", "-A-"}, "0",
			"a subtype CONSUMER: the leaf lost the subtype before subtype() read it, so it reports 0 -- 74 is what an uncleared register answers, and it was a live wrong answer"},
		{`SELECT key, group_concat(json_quote(value)) FROM json_each('[[7]]') GROUP BY key`,
			true, []string{"-A-"}, `"[7]"`,
			"a subtype GENERATOR over a subtyped leaf: json_quote of a value that LOST its subtype quotes the text, where an uncleared one embeds the array verbatim as [7]"},
		{`SELECT key, json_group_array(value) FROM json_each('[[7]]') GROUP BY key`,
			true, []string{"-A-"}, `["[7]"]`,
			"...and the same loss reaches json_group_array's own embedding decision"},

		// Controls that keep the clear from being global, also pinned on the
		// emitted code.
		{`SELECT k, group_concat(json_quote(s)) FROM t WHERE id = 1 GROUP BY k`,
			false, []string{"-A-"}, `"s0"`,
			"a plain b-tree row has nowhere to put a subtype (aggRowCanCarrySubtype), so no clear is emitted at all"},
		{`SELECT json_quote(max(value)) FROM json_each('[[7],[8]]')`,
			false, []string{"-A-", "-A-"}, `[8]`,
			"a WHOLE-TABLE aggregate has no grouping record, so it KEEPS the subtype -- max() returns its argument subtype and all, and json_quote then passes the array through"},
	} {
		prog, plan := compiledAgg(t, p, tc.sql)

		clears := 0
		for _, in := range prog.Insns {
			if in.Op == OpClearSubtype {
				clears++
			}
		}
		if (clears > 0) != tc.wantClears {
			t.Errorf("%s: %d OpClearSubtype, want any=%v (%s)", tc.sql, clears, tc.wantClears, tc.why)
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

// TestHashAggWhereKeepsSubtype checks the boundary: a WHERE term runs before
// the row reaches the sorter, so it still sees the subtype and
// json_quote(value)='[7]' matches one row. A strip that leaked out of the
// argument block would answer zero rows.
func TestHashAggWhereKeepsSubtype(t *testing.T) {
	p := argRegsFixture(t)
	const q = `SELECT key, count(*) FROM json_each('[[7]]') WHERE json_quote(value) = '[7]' GROUP BY key`
	_, rows, err := p.QueryArgs(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(rows) != 1 {
		t.Fatalf("%s: %d rows, want 1 -- the WHERE clause is coded before the grouping record and must still see the subtype", q, len(rows))
	}
}
