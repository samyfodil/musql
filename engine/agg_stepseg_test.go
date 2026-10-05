package engine

// Tests aggregate step segment structure. Aggregates are segmented when argument
// evaluation must be ordered around raising step bodies. Each shape here produces
// the same value but verifies the correct segment boundaries and opcodes.

import (
	"testing"
)

// segs renders each accumulator template's STEP SEGMENT in plan (== runtime
// step) order, the companion of stamps().
func segs(plan *aggPlan) []int {
	tmpls := aggPlanTemplates(plan)
	out := make([]int, len(tmpls))
	for i, it := range tmpls {
		out[i] = it.stepSeg
	}
	return out
}

// stepSegsEmitted returns the segment number (P2) of every step opcode the
// program emits, in program order. It is the half segs() cannot see: a plan
// could stamp segment 1 on an accumulator the body never emits a second opcode
// for, and that accumulator would then never step at all.
func stepSegsEmitted(prog *Program) []int {
	var out []int
	for _, in := range prog.Insns {
		switch in.Op {
		case OpAggStep, OpHashAggStep:
			out = append(out, in.P2)
		}
	}
	return out
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAggStepSegments verifies segment boundaries and opcode emission for various
// aggregate combinations.
func TestAggStepSegments(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql       string
		wantSegs  []int
		wantSteps []int
		why       string
	}{
		{`SELECT sum(v), count(v) FROM t`, []int{0, 0}, []int{0},
			"nothing can raise between them, so C's boundary is unobservable and the run coalesces into one opcode"},
		{`SELECT sum(v), group_concat(s) FROM t`, []int{0, 0}, []int{0},
			"the raising body is LAST, so there is nothing after it to separate"},
		{`SELECT group_concat(s), sum(v) FROM t`, []int{0, 1}, []int{0, 1},
			"sum's argument must be coded after group_concat's body, not before it"},
		{`SELECT group_concat(s), group_concat(s, '-'), count(*) FROM t`, []int{0, 1, 2}, []int{0, 1, 2},
			"two raising bodies in a row make three segments"},
		{`SELECT json_group_array(v), json_group_object(s, v) FROM t`, []int{0, 1}, []int{0, 1},
			"both JSON aggregates raise through jsonValueArg"},

		// The min()/max() census block cannot be split by a segment boundary.
		{`SELECT max(v), group_concat(s), sum(v) FROM t`, []int{0, 0, 0, 1}, []int{0, 1},
			"the census site, then max's own accumulator, then group_concat -- all segment 0 -- and sum below the cut"},

		// The three other row sources: the hash GROUP BY, the sorted drain, and
		// the FROM-less shape (which has its own entry point, below).
		{`SELECT k, group_concat(s), sum(v) FROM t GROUP BY k`, []int{0, 1}, []int{0, 1},
			"OpHashAggStep segments exactly like OpAggStep"},
		{`SELECT k, group_concat(s), sum(v) FROM t GROUP BY k ORDER BY k`, []int{0, 1}, []int{0, 1},
			"...and so does the sorted drain's record-mode OpAggStep"},

		// An UNLOWERED expression used to be a raising point too -- it ran
		// inside its own step body and so ended the segment -- and that arm is
		// GONE with aggItem.rowValue's own fallback (sql_agg.go): an expression
		// this plan cannot lower is now the STATEMENT's error (RULE #1), so it
		// never reaches a step body to raise in. The three spellings that used to
		// prove the cut are kept as the CONTROLS they became: each lowers
		// everything, on a different route, and so makes NO cut.
		//
		// The history is worth keeping because the assertion did not move, only
		// the mechanism under it. The first case was once spelled on the HASH
		// path (json() generating a subtype a grouped plan refused, closed by
		// taking the loss at the LEAF -- OpClearSubtype), then on the drain (a
		// SUBQUERY the record could not serve, closed by aggDrainRow.lowerable
		// carrying a sub-program). WHICH error wins is still pinned, by
		// TestAggArgErrorOrder and by the oracle in
		// compat-harness/agg_step_segment_test.go.
		{`SELECT k, sum(json((SELECT s))), sum(abs(v)) FROM t GROUP BY k ORDER BY k`, []int{0, 0}, []int{0},
			"the drain lowers a SUBQUERY argument now (aggDrainRow.lowerable), so there is no un-lowered raising point and no cut"},
		{`SELECT k, sum(json(s)), sum(abs(v)) FROM t, json_each('[1]') GROUP BY k`, []int{0, 0}, []int{0},
			"...and the hash path over a subtype-carrying row lowers BOTH, so there is no cut to make"},
		{`SELECT k, sum((SELECT 1)), sum(abs(v)) FROM t GROUP BY k ORDER BY k`, []int{0, 0}, []int{0},
			"...on the drain with no json() in sight either"},
		{`SELECT sum(json(s)), sum(abs(v)) FROM t`, []int{0, 0}, []int{0},
			"a whole-table aggregate keeps the subtype and lowers both, so there is no raising point and no cut"},

		// The census block over a SUBQUERY census site, which the drain also
		// lowers now. A bare column in the select list puts this plan on the
		// sorted drain (usesGroupBare excludes the hash path), and magnetWalk is
		// ONE unit: whatever happens inside it, no cut may land between the two
		// census sites. Mutating that guard is a WRONG ANSWER, not a lost
		// lowering -- magnetWalk steps every site in segment 0, so a site
		// stamped for segment 1 would read a register this row has not filled
		// yet. See TestAggCensusBlockIsOneSegment.
		{`SELECT k, max((SELECT 1)), min(v), v FROM t GROUP BY k`, []int{0, 0, 0, 0}, []int{0},
			"two census sites, both lowered off the drain's record: one block, one segment"},
	} {
		prog, plan := compiledAgg(t, p, tc.sql)
		if got := segs(plan); !intsEqual(got, tc.wantSegs) {
			t.Errorf("%s: segments %v, want %v (%s)", tc.sql, got, tc.wantSegs, tc.why)
		}
		if got := stepSegsEmitted(prog); !intsEqual(got, tc.wantSteps) {
			t.Errorf("%s: emitted step segments %v, want %v (%s)", tc.sql, got, tc.wantSteps, tc.why)
		}
	}
}

// TestAggStepSegmentsNoFrom is the same assertion on the FROM-less plan family,
// which compileSelectScanRow never reaches (compileNoFromAggregate has its own
// entry point) -- without it the segmentation there could be deleted with every
// other test still green.
func TestAggStepSegmentsNoFrom(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql       string
		wantSegs  []int
		wantSteps []int
	}{
		{`SELECT count(*), sum(2)`, []int{0, 0}, []int{0}},
		{`SELECT group_concat('a', '-'), sum(2)`, []int{0, 1}, []int{0, 1}},
	} {
		stmt, err := ParseSelect(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		prog, err := compileNoFromAggregate(p, stmt, nil)
		if err != nil {
			t.Fatalf("compile %q: %v", tc.sql, err)
		}
		var plan *aggPlan
		for _, in := range prog.Insns {
			if pl, ok := in.P4.(*aggPlan); ok {
				plan = pl
				break
			}
		}
		if plan == nil {
			t.Fatalf("compile %q: no aggregate opcode carrying an *aggPlan", tc.sql)
		}
		if got := segs(plan); !intsEqual(got, tc.wantSegs) {
			t.Errorf("%s: segments %v, want %v", tc.sql, got, tc.wantSegs)
		}
		if got := stepSegsEmitted(prog); !intsEqual(got, tc.wantSteps) {
			t.Errorf("%s: emitted step segments %v, want %v", tc.sql, got, tc.wantSteps)
		}
	}
}

// TestAggStepSegmentsCoverEveryAccumulator is the structural invariant behind
// the whole scheme: every accumulator the runtime will step must belong to a
// segment the body actually emits an opcode for. Stamp one with a segment
// number no opcode carries and that aggregate silently never steps -- a WRONG
// ANSWER (a sum that comes back 0), and one no differential built from
// hand-written shapes is likely to name.
//
// It also asserts the opcodes are 0..n-1 in program order with no gap and no
// repeat, which is what makes "segment k" a position in the body rather than a
// label.
func TestAggStepSegmentsCoverEveryAccumulator(t *testing.T) {
	p := argRegsFixture(t)
	for _, sql := range []string{
		`SELECT sum(v) FROM t`,
		`SELECT count(*), sum(v), group_concat(s), max(v), min(v), v FROM t`,
		`SELECT group_concat(s), sum(v) FILTER (WHERE v > 0), json_group_array(v), count(*) FROM t`,
		`SELECT k, group_concat(s), sum(v), max(v) FROM t GROUP BY k`,
		`SELECT k, group_concat(s), sum(v), max(v) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, group_concat(s), sum(v) FROM t GROUP BY k HAVING count(*) > 0 ORDER BY sum(v)`,
		`SELECT k, sum(json(s)), group_concat(s), sum(v) FROM t GROUP BY k`,
	} {
		prog, plan := compiledAgg(t, p, sql)
		emitted := stepSegsEmitted(prog)
		if len(emitted) == 0 {
			t.Errorf("%s: no step opcode at all", sql)
			continue
		}
		// The body of a LEFT/FULL JOIN is emitted more than once; these are all
		// single-table, so the opcodes are exactly one pass of 0..n-1.
		for i, seg := range emitted {
			if seg != i {
				t.Errorf("%s: emitted step segments %v are not 0..n-1 in order", sql, emitted)
				break
			}
		}
		for i, it := range aggPlanTemplates(plan) {
			if it.stepSeg < 0 || it.stepSeg >= len(emitted) {
				t.Errorf("%s: accumulator %d is in segment %d but the body emits %d step opcode(s) -- it would never step",
					sql, i, it.stepSeg, len(emitted))
			}
		}
	}
}

// TestAggCensusBlockIsOneSegment states the invariant aggPlanStepOrder exists
// for, directly rather than through a stamp vector: every min()/max() CENSUS
// site is in the SAME step segment.
//
// aggAccumulators.magnetWalk (vdbe_agg.go) steps every site and then reads the
// verdict they left in M to decide the group's anchor row -- one unit, run by
// segment 0 alone -- so a site stamped for a LATER segment would be stepped in
// segment 0 off a register that segment has not filled. Measured by mutation
// (planAggArgRegs' "i >= nMagnet" replaced by an always-true test) over
// t(k,v,s) = (1,5,'[1]'),(1,9,'[2]'),(1,2,'[3]'),(2,7,'[4]'),(2,1,'[5]'),(2,8,'[6]'):
//
//	SELECT k, max(json(s)), min(v), v FROM t GROUP BY k
//	    correct: 1|[3]|2|2      mutated: 1|[3]|2|9
//
// -- the bare column reads the row min(v) last accepted, and the mutant reads
// row 2 instead, because min(v)'s stamped register still held the previous
// row's value when magnetWalk stepped it. The differential twin is
// compat-harness/agg_step_segment_test.go's TestAggCensusSegmentAnswers.
func TestAggCensusBlockIsOneSegment(t *testing.T) {
	p := argRegsFixture(t)
	for _, sql := range []string{
		`SELECT k, max(subtype(v)), min(v), v FROM t GROUP BY k`,
		`SELECT k, min(subtype(v)), max(v), v FROM t GROUP BY k`,
		`SELECT k, max(subtype(v)), min(v), max(v), v FROM t GROUP BY k`,
		`SELECT k, max(subtype(v)), min(v), v FROM t GROUP BY k ORDER BY k`,
		`SELECT k, max(subtype(v)), min(v), group_concat(s), v FROM t GROUP BY k`,
		`SELECT max(subtype(v)), min(v), v FROM t`,
		`SELECT k, max(v), min(v), group_concat(s), v FROM t GROUP BY k`,
	} {
		_, plan := compiledAgg(t, p, sql)
		tmpls, nMagnet := aggPlanStepOrder(plan)
		if nMagnet < 2 {
			t.Errorf("%s: %d census site(s) -- fewer than two, so this case cannot see a cut inside the block", sql, nMagnet)
			continue
		}
		for i := 1; i < nMagnet; i++ {
			if tmpls[i].stepSeg != tmpls[0].stepSeg {
				t.Errorf("%s: census site %d is in segment %d, site 0 in %d -- magnetWalk steps them all in one opcode, so a later segment's register is not filled yet",
					sql, i, tmpls[i].stepSeg, tmpls[0].stepSeg)
			}
		}
	}
}
