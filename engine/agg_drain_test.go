package engine

// Tests sorted GROUP BY aggregate drain: cursor access and record offsets.

import (
	"path/filepath"
	"testing"
)

// TestAggDrainReadsNoCursor verifies that the aggregate drain does not read cursors.
func TestAggDrainReadsNoCursor(t *testing.T) {
	p := argRegsFixture(t)
	// EVERY case orders DESCENDING, deliberately. Ascending group-key order is
	// now served by the HASH drain -- it is exactly the order hashAggSort
	// already emits, so no sorter is built at all -- and these cases exist to
	// exercise the SORTED drain. Descending is the order the hash path is
	// specifically not allowed to produce, so it still picks the sorter and
	// every case keeps its teeth. Without this the whole test degrades into
	// "no OpSorterSort, so the case asserts nothing", which is how it reported
	// the change rather than silently passing.
	for _, sql := range []string{
		`SELECT k, sum(v) FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, sum(v * 2 + length(s)) FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, sum(v) FILTER (WHERE v > 0) FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, group_concat(s, s || 'x') FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, sum(rowid), max(t.rowid) FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, max(v), s FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, sum(v) FROM t GROUP BY k HAVING count(*) > 1 ORDER BY k DESC`,
		`SELECT DISTINCT k, count(*) FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, sum((SELECT 1)) FROM t GROUP BY k ORDER BY k DESC`,
		`SELECT k, max(subtype(v)) FROM t GROUP BY k ORDER BY k DESC`,
	} {
		prog, _ := compiledAgg(t, p, sql)
		drain := -1
		for i, in := range prog.Insns {
			if in.Op == OpSorterSort {
				drain = i
				break
			}
		}
		if drain < 0 {
			t.Errorf("%s: no OpSorterSort, so this is not the sorted drain and the case asserts nothing", sql)
			continue
		}
		for i := drain; i < len(prog.Insns); i++ {
			in := prog.Insns[i]
			if in.Op == OpAggStep && in.P3 == 1 {
				continue // record mode: reads the sorter payload, not a cursor
			}
			if aggDrainOpForbidden(in.Op) {
				t.Errorf("%s: instruction %d in the drain is %v, which reads a cursor that closed with the scan",
					sql, i, in.Op)
			}
		}
	}
}

// TestAggDrainJoinStamps is the drain's STAMP assertion over a JOIN, which is
// the only shape that can see aggDrainRow's PER-SOURCE offset. The record's
// columns block is every source's columns concatenated at that source's own
// offset (compileScanGroupBy's scan body), so a lowering that ignored the
// offset would read the first table's column where the second's was meant --
// invisible in a single-table fixture, where every offset is zero.
//
// It used to be an A/B against a second evaluator for the same expression.
// That second evaluator is gone, so the answer half is now pinned against the
// 3.53.3 oracle instead (compat-harness/agg_drain_subquery_test.go's join
// fixture), and what stays here is the assertion the A/B could never make: the
// EXACT per-accumulator stamps.
//
// t3 stays empty so a LEFT JOIN's NULL-extension row is drained too: an
// unmatched source contributes NULL columns and a NULL rowid to the record,
// exactly as the join loops' own NULL-extension does (OpNullRow,
// vdbe_join_codegen.go).
func TestAggDrainJoinStamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dj.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE t2(c, d)`,
		`CREATE TABLE t3(e, f)`, // stays empty: forces the NULL-extension row
		`INSERT INTO t1 VALUES(1,10),(1,20),(2,30)`,
		`INSERT INTO t2 VALUES(1,100),(2,200)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// The EXACT stamps, not merely "something lowered". A slot mapping that
	// drops the per-source offset, or points every source's rowid at the first
	// one's, does not produce a wrong answer -- slotFor's resolver-agreement
	// check refuses the mismatch and that accumulator's operand simply goes
	// unlowered. What it produces is a SILENTLY NARROWER lowering, which only
	// a per-accumulator assertion can see: with anyStamped here instead, both
	// mutations passed this test and the whole harness.
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT a, sum(d), sum(b), min(c), max(d) FROM t1 JOIN t2 ON t2.c=t1.a GROUP BY a ORDER BY a`,
			[]string{"-A-", "-A-", "-A-", "-A-", "-A-", "-A-"}},
		// Two raising step bodies in a row: three segments, and the second
		// group_concat's own argument and SEPARATOR lower in the second one.
		{`SELECT a, group_concat(d), group_concat(b, d) FROM t1 JOIN t2 ON t2.c=t1.a GROUP BY a ORDER BY a`,
			[]string{"-A-", "-AS"}},
		{`SELECT a, sum(t1.rowid), sum(t2.rowid) FROM t1 JOIN t2 ON t2.c=t1.a GROUP BY a ORDER BY a`,
			[]string{"-A-", "-A-"}},
		{`SELECT a, sum(f), count(e), count(t3.rowid) FROM t1 LEFT JOIN t3 ON t3.e=t1.a GROUP BY a ORDER BY a`,
			[]string{"-A-", "-A-", "-A-"}},
		{`SELECT a, sum(d) FILTER (WHERE b > 10) FROM t1 JOIN t2 ON t2.c=t1.a GROUP BY a ORDER BY a`,
			[]string{"FA-"}},
		{`SELECT a, sum(b * 2 + d) FROM t1 JOIN t2 ON t2.c=t1.a GROUP BY a ORDER BY a`,
			[]string{"-A-"}},
	} {
		sql := tc.sql
		got := stamps(aggPlanOf(t, p, sql))
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d accumulator templates %v, want %d %v", sql, len(got), got, len(tc.want), tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: template %d lowered %q, want %q", sql, i, got[i], tc.want[i])
			}
		}
		// ...and it must ANSWER. The stamp-cleared twin this used to compare
		// against is gone with the second evaluator it needed; runStamped's
		// own assertion (no operand left unlowered) is what replaced it, and
		// the per-template stamps checked just above are stricter still.
		if _, err := runStamped(t, p, sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
}
