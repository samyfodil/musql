package engine

// Tests that aggregate expressions (FILTER, ARGUMENT, SEPARATOR) are lowered
// into registers correctly so the step function reads pre-computed values instead
// of re-evaluating them.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// compiledAgg compiles sql and returns the program together with the *aggPlan
// its aggregate opcode carries.
func compiledAgg(t *testing.T, p *ReadOnlyPager, sql string) (*Program, *aggPlan) {
	t.Helper()
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	prog, err := compileSelectScanRow(p, stmt, nil, nil)
	if err != nil {
		t.Fatalf("compile %q: %v", sql, err)
	}
	for _, in := range prog.Insns {
		switch in.Op {
		case OpAggStep, OpHashAggStep, OpAggReset:
			if pl, ok := in.P4.(*aggPlan); ok {
				return prog, pl
			}
		}
	}
	t.Fatalf("compile %q: no aggregate opcode carrying an *aggPlan", sql)
	return nil, nil
}

func aggPlanOf(t *testing.T, p *ReadOnlyPager, sql string) *aggPlan {
	t.Helper()
	_, pl := compiledAgg(t, p, sql)
	return pl
}

// stamps renders which of the three per-row expressions (Filter, Argument,
// Separator) was lowered, as "FAS" with '-' for each unlowered slot.
func stamps(plan *aggPlan) []string {
	tmpls := aggPlanTemplates(plan)
	out := make([]string, len(tmpls))
	for i, it := range tmpls {
		s := []byte("---")
		for j, ch := range []byte("FAS") {
			if it.rowRegs[j] > 0 {
				s[j] = ch
			}
		}
		out[i] = string(s)
	}
	return out
}

// anyStamped reports whether the plan lowered anything at all.
func anyStamped(plan *aggPlan) bool {
	for _, s := range stamps(plan) {
		if s != "---" {
			return true
		}
	}
	return false
}

// unstampedOperands names every per-row expression not lowered into a register,
// as "<call>.<F|A|S>" per unlowered slot, empty when all are lowered.
func unstampedOperands(plan *aggPlan) []string {
	var out []string
	for _, it := range aggPlanTemplates(plan) {
		for j, ch := range []byte("FAS") {
			if it.rowExpr(j) != nil && it.rowRegs[j] == 0 {
				out = append(out, fmt.Sprintf("%s.%c", it.srcCall.Name, ch))
			}
		}
	}
	return out
}

// runStamped executes sql and asserts that all per-row expressions are lowered.
// Values are checked against the oracle in compat-harness tests.
func runStamped(t *testing.T, p *ReadOnlyPager, sql string) ([][]Value, error) {
	t.Helper()
	prog, plan := compiledAgg(t, p, sql)
	if un := unstampedOperands(plan); len(un) != 0 {
		t.Errorf("%s: left %v unlowered -- aggItem.rowValue has nothing to fall back to, so this is a run-time error waiting to happen", sql, un)
	}
	return prog.exec(p, nil)
}

func argRegsFixture(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ar.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, s TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows := [][3]int64{
		{0, 5, 0}, {0, -3, 1}, {1, 7, 2}, {1, 7, 3}, {2, 0, 4},
		{2, -9223372036854775808, 5}, {0, 11, 6}, {1, -1, 7},
	}
	for i, r := range rows {
		if _, _, err := db.ExecArgs(`INSERT INTO t VALUES(?,?,?,?)`, []Value{
			{Typ: Int, I: int64(i + 1)}, {Typ: Int, I: r[0]}, {Typ: Int, I: r[1]},
			{Typ: Text, S: []byte(fmt.Sprintf("s%d", r[2]))},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Add rows with NULL and REAL values to exercise all storage classes.
	if _, _, err := db.ExecArgs(`INSERT INTO t VALUES(9,0,NULL,NULL)`, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ExecArgs(`INSERT INTO t VALUES(10,1,2.5,'x')`, nil); err != nil {
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
	return p
}

// TestAggArgRegsLowered verifies which aggregate expressions are lowered in
// each plan type.
func TestAggArgRegsLowered(t *testing.T) {
	p := argRegsFixture(t)
	cases := []struct {
		sql  string
		want []string
		why  string
	}{
		{`SELECT k, count(*), sum(v) FROM t GROUP BY k`, []string{"---", "-A-"},
			"count(*) has no argument; sum(v) is lowered"},
		{`SELECT sum(v), count(v) FROM t`, []string{"-A-", "-A-"},
			"the whole-table aggregate steps from live cursors too"},
		{`SELECT sum(v * 2 + length(s)) FROM t`, []string{"-A-"},
			"a whole-table aggregate keeps the JSON subtype, so any expression may lower"},
		{`SELECT k, sum(v * 2 + length(s)) FROM t GROUP BY k`, []string{"-A-"},
			"a GROUPED plan lowers ANY expression: the subtype loss is applied to the LEAVES (OpClearSubtype), which is where C's serialized sorter record applies it"},
		{`SELECT k, max(v) FROM t GROUP BY k`, []string{"-A-", "-A-"},
			"a bare column does lower under GROUP BY -- the census site too -- with its subtype stripped at the read"},

		// FILTER lowers with its arguments or not at all.
		{`SELECT sum(v) FILTER (WHERE v > 0) FROM t`, []string{"FA-"},
			"a whole-table plan lowers the FILTER and emits the jump, so the argument may lower beside it"},
		{`SELECT max(v) FILTER (WHERE v > 0), v FROM t`, []string{"FA-", "FA-"},
			"...the min/max CENSUS site reads the same filter register (magnetStep)"},
		{`SELECT sum(v) FILTER (WHERE v > 0), count(v) FROM t`, []string{"FA-", "-A-"},
			"an accumulator with no FILTER of its own is unaffected by another's"},
		{`SELECT k, sum(v) FILTER (WHERE v > 0) FROM t GROUP BY k`, []string{"FA-"},
			"a comparison cannot return a subtype (expr.c:4726's prune), so the FILTER lowers and the argument lowers beside it -- behind the FILTER's own jump, which is what keeps abs() off a rejected row (pinned in TestAggFilterArgNotEvaluatedOnFilteredRows)"},
		{`SELECT k, sum(v), max(v) FILTER (WHERE v > 0) FROM t GROUP BY k`, []string{"FA-", "-A-", "FA-"},
			"...and now the filtered accumulator lowers too, census site included, for the same reason"},

		// group_concat and JSON aggregates with separators.
		{`SELECT k, group_concat(s) FROM t GROUP BY k`, []string{"-A-"},
			"group_concat lowers its own argument: its arguments precede its own step body in C too"},
		{`SELECT group_concat(s, '-') FROM t`, []string{"-AS"},
			"the SEPARATOR is argv[1] and is coded with the rest of the list (select.c:6902-6906)"},
		{`SELECT k, group_concat(s, '-') FROM t GROUP BY k`, []string{"-AS"},
			"a subtype-free LITERAL separator lowers under GROUP BY too -- it reads no row value, so the grouped clear has nothing to undo"},
		{`SELECT k, group_concat(s, s || 'x') FROM t GROUP BY k`, []string{"-AS"},
			"...and a wider separator lowers too: '||' is a BinaryExpr, which prunes"},
		{`SELECT json_group_object(s, v) FROM t`, []string{"-AS"},
			"json_group_object's VALUE rides the same sepExpr slot"},
		{`SELECT k, json_group_array(v) FROM t GROUP BY k`, []string{"-A-"},
			"a JSON aggregate's bare-column argument lowers with the grouped subtype strip"},
		{`SELECT sum(v), group_concat(s) FROM t`, []string{"-A-", "-A-"},
			"an accumulator BEFORE a raising step body still lowers"},
		{`SELECT group_concat(s), sum(v) FROM t`, []string{"-A-", "-A-"},
			"...and so does one AFTER it, in the NEXT step segment: hoisting sum's argument ahead of group_concat's body would report the wrong error, so it is coded for a second OpAggStep instead -- C's own boundary (select.c:6824). Gated by TestAggStepSegments and TestAggStepBodyErrorOrder"},

		// SORTED drain: registers stamped in the drain loop, not the scan body.
		{`SELECT k, sum(v) FROM t GROUP BY k ORDER BY k`, []string{"-A-"},
			"the sorted drain lowers too, reading the column out of the sorter record (expr.c:4995-4997)"},
		{`SELECT k, max(v), v FROM t GROUP BY k ORDER BY k`, []string{"-A-", "-A-"},
			"...census sites included"},
		{`SELECT k, sum(v * 2 + length(s)) FROM t GROUP BY k ORDER BY k`, []string{"-A-"},
			"a WIDER drain expression lowers as well: only the LEAF comes from the record, the rest is coded normally"},
		{`SELECT k, sum(v) FILTER (WHERE v > 0) FROM t GROUP BY k ORDER BY k`, []string{"FA-"},
			"...FILTER included, jump and all"},
		{`SELECT k, group_concat(s, '-') FROM t GROUP BY k ORDER BY k`, []string{"-AS"},
			"...separator included"},
		{`SELECT k, sum(rowid) FROM t GROUP BY k ORDER BY k`, []string{"-A-"},
			"the record's rowid block backs the rowid pseudo-column, exactly as aggRowSplit lays it out"},
		{`SELECT k, group_concat(s), sum(v) FROM t GROUP BY k ORDER BY k`, []string{"-A-", "-A-"},
			"the drain segments the same way, into two OpAggStep P3==1 opcodes"},
		{`SELECT k, sum((SELECT 1)) FROM t GROUP BY k ORDER BY k`, []string{"-A-"},
			"a SUBQUERY lowers here now: C walks INTO a sub-select when it analyzes aggregates (expr.c:7570-7572), so a leaf naming the aggregate query's FROM reads the SORTER RECORD (expr.c:4996-4999) rather than a closed cursor -- ported as compileColumn's OpOuterAggReg over the drain's row block (aggDrainRow.outerSlotReg)"},
		{`SELECT k, sum((SELECT k)) FROM t GROUP BY k ORDER BY k`, []string{"-A-"},
			"...including the CORRELATED spelling, which is the one the port is actually about"},

		// Subtype consumer on all three grouped routes. Subtype loss is taken at
		// the leaf where columns are read.
		{`SELECT k, max(subtype(v)) FROM t, json_each('[1]') GROUP BY k`, []string{"-A-", "-A-"},
			"the grouped hash path lowers a subtype CONSUMER: its leaves lost the subtype before it reads them"},
		{`SELECT k, max(subtype(v)) FROM t GROUP BY k`, []string{"-A-", "-A-"},
			"...as does the same expression over a row that could not carry one anyway (aggRowCanCarrySubtype), which emits no clear at all"},
		{`SELECT k, max(subtype(v) + 1) FROM t, json_each('[1]') GROUP BY k`, []string{"-A-", "-A-"},
			"...however deeply the read is buried under operators that cannot propagate a subtype outward"},
		{`SELECT k, max(subtype(v)) FROM t, json_each('[1]') GROUP BY k ORDER BY k`, []string{"-A-", "-A-"},
			"the SORTED DRAIN takes the same loss a whole record at a time (OpSorterData's P3), which is C SQLite's serialized sorter exactly, so the leaves it reads carry no subtype and any expression over them lowers"},
		{`SELECT k, max(subtype(v)) FROM t GROUP BY k ORDER BY k`, []string{"-A-", "-A-"},
			"...as it is over a plain table, for the other reason"},
		{`SELECT max(subtype(v)) FROM t`, []string{"-A-", "-A-"},
			"a whole-table aggregate has no grouping record and KEEPS the subtype: nothing is stripped there, and it still lowers"},

		// Other ways a row block can hold a subtype.
		{`SELECT k, sum(json(s)) FROM (SELECT k, s FROM t) GROUP BY k`, []string{"-A-"},
			"a DERIVED table's rows ARE its select list's computed values, so one of them can carry a subtype -- and the leaf clear is what lets a subtype GENERATOR lower over it"},
		{`SELECT k, sum(json(s)) FROM t GROUP BY k`, []string{"-A-"},
			"...as it does over the base table, which has nothing to clear"},
	}
	for _, tc := range cases {
		got := stamps(aggPlanOf(t, p, tc.sql))
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d accumulator templates %v, want %d (%s)", tc.sql, len(got), got, len(tc.want), tc.why)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: template %d lowered %q, want %q (%s)", tc.sql, i, got[i], tc.want[i], tc.why)
			}
		}
	}
}

// TestAggFilterArgNotEvaluatedOnFilteredRows verifies that filtered-out rows
// do not evaluate their arguments, even if evaluation would error.
func TestAggFilterArgNotEvaluatedOnFilteredRows(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql      string
		wantRows int
		wantJump bool
	}{
		{`SELECT sum(abs(v)) FILTER (WHERE v > -9223372036854775808) FROM t`, 1, true},
		// Grouped plan now lowers FILTER with wider expressions.
		{`SELECT k, sum(abs(v)) FILTER (WHERE v > -9223372036854775808) FROM t GROUP BY k`, 3, true},
	} {
		if got := anyStamped(aggPlanOf(t, p, tc.sql)); got != tc.wantJump {
			t.Errorf("%s: lowered=%v, want %v -- the case is only teeth in the state it claims", tc.sql, got, tc.wantJump)
		}
		_, rows, err := p.QueryArgs(tc.sql, nil)
		if err != nil {
			t.Fatalf("%s: %v (the FILTER's rejected row must never reach abs())", tc.sql, err)
		}
		if len(rows) != tc.wantRows {
			t.Fatalf("%s: got %d rows, want %d", tc.sql, len(rows), tc.wantRows)
		}
	}
}

// TestAggSepEvaluatedWithTheArgument verifies that separators are evaluated
// together with arguments, even for rows that contribute nothing to the result.
func TestAggSepEvaluatedWithTheArgument(t *testing.T) {
	p := argRegsFixture(t)
	// Row with value that causes abs() overflow also contributes nothing to
	// aggregate; must still evaluate the second argument.
	for _, tc := range []struct {
		sql        string
		wantStamps []string
	}{
		{`SELECT group_concat(NULL, abs(v)) FROM t WHERE v = -9223372036854775808`, []string{"-AS"}},
		{`SELECT json_group_object(NULL, abs(v)) FROM t WHERE v = -9223372036854775808`, []string{"-AS"}},
		// Separator is coded with the argument, before the step body.
		{`SELECT k, group_concat(NULL, abs(v)) FROM t WHERE v = -9223372036854775808 GROUP BY k`, []string{"-AS"}},
		{`SELECT k, json_group_object(NULL, abs(v)) FROM t WHERE v = -9223372036854775808 GROUP BY k`, []string{"-AS"}},
		{`SELECT k, group_concat(NULL, json_quote(abs(v))) FROM t, json_each('[1]') WHERE v = -9223372036854775808 GROUP BY k`, []string{"-AS"}},
		{`SELECT k, json_group_object(NULL, json_quote(abs(v))) FROM t, json_each('[1]') WHERE v = -9223372036854775808 GROUP BY k`, []string{"-AS"}},
	} {
		got := stamps(aggPlanOf(t, p, tc.sql))
		if len(got) != len(tc.wantStamps) || got[0] != tc.wantStamps[0] {
			t.Errorf("%s: lowered %v, want %v -- the case is only teeth in the state it claims", tc.sql, got, tc.wantStamps)
		}
		if _, _, err := p.QueryArgs(tc.sql, nil); err == nil {
			t.Errorf("%s: answered, want an evaluation error -- the second argument must be evaluated even for a row that contributes nothing", tc.sql)
		}
	}
}

// TestJSONAggArgKeepsGroupedSubtypeRule verifies that grouped queries lose the
// JSON subtype from row values, affecting how containers are embedded.
func TestJSONAggArgKeepsGroupedSubtypeRule(t *testing.T) {
	p := argRegsFixture(t)
	const q = `SELECT json_group_array(value) FROM json_each('[[7]]') GROUP BY key`
	_, rows, err := p.QueryArgs(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: got %d rows", q, len(rows))
	}
	if got, want := string(rows[0][0].S), `["[7]"]`; got != want {
		t.Fatalf("%s: got %s, want %s -- the grouped row's JSON subtype must be lost", q, got, want)
	}
}

// TestAggArgRegsLeftJoinBothBodyEmissions verifies that aggregate arguments are
// correctly lowered in both emission paths of a LEFT JOIN scan body.
func TestAggArgRegsLeftJoinBothBodyEmissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lj.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE t2(c, d)`,
		`CREATE TABLE t3(e, f)`, // stays empty: forces the NULL-extension body
		`INSERT INTO t1 VALUES(123,456)`,
		`INSERT INTO t1 VALUES(124,457)`,
		`INSERT INTO t2 VALUES(123,456)`,
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

	cases := []struct {
		sql  string
		want []int64 // one expected integer per row, column 0 last
	}{
		// The matched body: c is 123 on every combination.
		{`SELECT count(DISTINCT c) FROM t1 LEFT JOIN t2`, []int64{1}},
		{`SELECT sum(c) FROM t1 LEFT JOIN t2`, []int64{246}},
		// The NULL-extension body: t3 is empty, so c/e are NULL everywhere.
		{`SELECT count(DISTINCT e) FROM t1 LEFT JOIN t3`, []int64{0}},
		{`SELECT count(*) FROM t1 LEFT JOIN t3`, []int64{2}},
		// ...and the grouped (hash) path over both.
		{`SELECT count(DISTINCT c) FROM t1 LEFT JOIN t2 GROUP BY a`, []int64{1, 1}},
		{`SELECT count(DISTINCT e) FROM t1 LEFT JOIN t3 GROUP BY a`, []int64{0, 0}},
		// A LEFT JOIN whose right side matches only SOME left rows, so both
		// body emissions run in the same statement.
		{`SELECT count(DISTINCT c) FROM t1 LEFT JOIN t2 ON t2.c = t1.a`, []int64{1}},
		{`SELECT count(DISTINCT c) FROM t1 LEFT JOIN t2 ON t2.c = t1.a GROUP BY a`, []int64{1, 0}},
	}
	for _, tc := range cases {
		_, rows, err := p.QueryArgs(tc.sql, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if len(rows) != len(tc.want) {
			t.Fatalf("%s: got %d rows, want %d", tc.sql, len(rows), len(tc.want))
		}
		for i, w := range tc.want {
			got := rows[i][len(rows[i])-1]
			if got.Typ != Int || got.I != w {
				t.Errorf("%s: row %d = %v, want INTEGER %d", tc.sql, i, got, w)
			}
		}
	}
}

// TestAggArgRegsLowerEveryOperand verifies that all per-row aggregate
// expressions are lowered across all plan types.
func TestAggArgRegsLowerEveryOperand(t *testing.T) {
	p := argRegsFixture(t)
	calls := []string{
		"count(v)", "sum(v)", "total(v)", "avg(v)", "min(v)", "max(v)", "max(s)",
		"sum(DISTINCT v)", "count(DISTINCT v)",
		"min(v * 2)", "max(abs(v + 1))", "sum(CASE WHEN v > 0 THEN v ELSE 0 END)",
		"min(s || 'z')", "sum(DISTINCT v * 2)",
		// The FILTER, accepting and rejecting, and over a column that is NULL
		// on one row so the JUMPIFNULL half is exercised too.
		"sum(v) FILTER (WHERE 1)", "sum(v) FILTER (WHERE 0)",
		"sum(v) FILTER (WHERE v > 0)", "sum(v) FILTER (WHERE v)",
		"count(*) FILTER (WHERE k = 1)", "max(s) FILTER (WHERE v > 0)",
		"count(DISTINCT v) FILTER (WHERE v > 0)",
		// The separator, literal and row-sourced, and json_group_object's
		// VALUE riding the same slot.
		"group_concat(s)", "group_concat(s, '-')", "group_concat(s, s)",
		"group_concat(DISTINCT s)", "group_concat(s, '-') FILTER (WHERE v > 0)",
		"json_group_array(v)", "json_group_object(s, v)",
		// Two accumulators, so the "nothing after a raising body" rule is
		// exercised in both orders.
		"sum(v), group_concat(s)", "group_concat(s), sum(v)",
		// The rowid pseudo-column, which the sorted drain reads out of the
		// record's ROWID block rather than its columns block -- a different
		// slot mapping, and the only reader of aggDrainRow's rowidReg.
		"sum(rowid)", "min(rowid)", "max(t.rowid)", "count(DISTINCT rowid)",
	}
	check := func(sql string) {
		t.Helper()
		if _, err := runStamped(t, p, sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
	for _, call := range calls {
		// All three plan spellings: whole-table, hash GROUP BY, and sorted drain.
		check(fmt.Sprintf("SELECT %s FROM t", call))
		check(fmt.Sprintf("SELECT k, %s FROM t GROUP BY k", call))
		check(fmt.Sprintf("SELECT k, %s FROM t GROUP BY k ORDER BY k", call))
	}
}

// TestAggStepBodyErrorOrder verifies that step-body errors precede argument
// errors in the correct order.
func TestAggStepBodyErrorOrder(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql       string
		wantSegs  []int
		wantSteps []int
		wantErr   string
	}{
		{`SELECT json_group_array(x'ff'), sum(abs(v)) FROM t WHERE v = -9223372036854775808`,
			[]int{0, 1}, []int{0, 1}, "JSON cannot hold BLOB values"},
		{`SELECT sum(abs(v)), json_group_array(x'ff') FROM t WHERE v = -9223372036854775808`,
			[]int{0, 0}, []int{0}, "integer overflow"},
		// The same pair through the two GROUP BY row sources, whose registers
		// are filled in the scan body (hash) and in the drain loop (sorted).
		{`SELECT k, json_group_array(x'ff'), sum(abs(v)) FROM t WHERE v = -9223372036854775808 GROUP BY k`,
			[]int{0, 1}, []int{0, 1}, "JSON cannot hold BLOB values"},
		{`SELECT k, json_group_array(x'ff'), sum(abs(v)) FROM t WHERE v = -9223372036854775808 GROUP BY k ORDER BY k`,
			[]int{0, 1}, []int{0, 1}, "JSON cannot hold BLOB values"},
	} {
		prog, plan := compiledAgg(t, p, tc.sql)
		for i, it := range aggPlanTemplates(plan) {
			if it.rowRegs[aggExprArg] == 0 {
				t.Errorf("%s: accumulator %d did not lower its argument, so this case no longer tests the ordering CUT -- it tests a refusal", tc.sql, i)
			}
		}
		if got := segs(plan); !intsEqual(got, tc.wantSegs) {
			t.Errorf("%s: segments %v, want %v", tc.sql, got, tc.wantSegs)
		}
		if got := stepSegsEmitted(prog); !intsEqual(got, tc.wantSteps) {
			t.Errorf("%s: emitted step segments %v, want %v", tc.sql, got, tc.wantSteps)
		}
		_, _, err := p.QueryArgs(tc.sql, nil)
		if err == nil {
			t.Fatalf("%s: answered, want an error", tc.sql)
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: %v, want %q -- a later accumulator's argument must not be evaluated ahead of an earlier body", tc.sql, err, tc.wantErr)
		}
	}
}

// TestAggArgErrorOrder verifies the correct error is reported when multiple
// arguments could raise, across all plan types.
func TestAggArgErrorOrder(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql        string
		wantStamps []string
		wantSegs   []int
	}{
		{`SELECT k, sum(json((SELECT s))), sum(abs(v)) FROM t GROUP BY k ORDER BY k`,
			[]string{"-A-", "-A-"}, []int{0, 0}},
		{`SELECT k, sum(json(s)), sum(abs(v)) FROM t, json_each('[1]') GROUP BY k`,
			[]string{"-A-", "-A-"}, []int{0, 0}},
		{`SELECT sum(json(s)), sum(abs(v)) FROM t`,
			[]string{"-A-", "-A-"}, []int{0, 0}},
	} {
		plan := aggPlanOf(t, p, tc.sql)
		got := stamps(plan)
		if len(got) != len(tc.wantStamps) {
			t.Fatalf("%s: %d templates %v, want %d", tc.sql, len(got), got, len(tc.wantStamps))
		}
		for i := range got {
			if got[i] != tc.wantStamps[i] {
				t.Errorf("%s: template %d lowered %q, want %q", tc.sql, i, got[i], tc.wantStamps[i])
			}
		}
		if gotSegs := segs(plan); !intsEqual(gotSegs, tc.wantSegs) {
			t.Errorf("%s: segments %v, want %v", tc.sql, gotSegs, tc.wantSegs)
		}
		_, _, err := p.QueryArgs(tc.sql, nil)
		if err == nil {
			t.Fatalf("%s: answered, want an error", tc.sql)
		}
		if !strings.Contains(err.Error(), "malformed JSON") {
			t.Errorf("%s: %v, want json()'s own error -- the LATER abs() must not be evaluated ahead of it", tc.sql, err)
		}
	}
}

// TestNoFromAggArgRegs verifies lowering in FROM-less aggregate queries, where
// a single synthetic row is stepped.
func TestNoFromAggArgRegs(t *testing.T) {
	p := argRegsFixture(t)
	compile := func(sql string) (*Program, *aggPlan) {
		t.Helper()
		stmt, err := ParseSelect(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		prog, err := compileNoFromAggregate(p, stmt, nil)
		if err != nil {
			t.Fatalf("compile %q: %v", sql, err)
		}
		for _, in := range prog.Insns {
			if pl, ok := in.P4.(*aggPlan); ok {
				return prog, pl
			}
		}
		t.Fatalf("compile %q: no aggregate opcode carrying an *aggPlan", sql)
		return nil, nil
	}
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT sum(1)`, []string{"-A-"}},
		{`SELECT count(*), sum(2)`, []string{"---", "-A-"}},
		{`SELECT sum(3) FILTER (WHERE 1)`, []string{"FA-"}},
		{`SELECT sum(abs(-9223372036854775807 - 1)) FILTER (WHERE 0)`, []string{"FA-"}},
		{`SELECT group_concat('a', '-')`, []string{"-AS"}},
	} {
		prog, plan := compile(tc.sql)
		got := stamps(plan)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d accumulator templates %v, want %d", tc.sql, len(got), got, len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: template %d lowered %q, want %q", tc.sql, i, got[i], tc.want[i])
			}
		}
		// Must answer, including filtered-out rows that would error if evaluated.
		if _, err := prog.exec(p, nil); err != nil {
			t.Errorf("%s: %v (a FROM-less aggregate must answer)", tc.sql, err)
		}
	}
}

// TestMinMaxArgKeepsGroupedSubtypeRule verifies that min()/max() preserve
// subtype behavior in grouped queries, where row values have the subtype cleared.
func TestMinMaxArgKeepsGroupedSubtypeRule(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct{ sql, want string }{
		{`SELECT json_quote(min(value)) FROM json_each('[[7]]') GROUP BY key`, `"[7]"`},
		{`SELECT json_quote(max(value)) FROM json_each('[[7],[8]]')`, `[8]`},
		// ...and the wider-expression case, which a grouped plan used to REFUSE
		// for want of anywhere correct to put the strip. It lowers now, with
		// the clear at the leaf json_quote reads.
		{`SELECT min(json_quote(value)) FROM json_each('[[7]]') GROUP BY key`, `"[7]"`},
	} {
		_, rows, err := p.QueryArgs(tc.sql, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("%s: got %d rows", tc.sql, len(rows))
		}
		if got := string(rows[0][0].S); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.sql, got, tc.want)
		}
	}
}

// canonRows renders a result set as an order-insensitive multiset of rows,
// storage class and float bits included, so only the VALUES are compared and
// never the emission order the two paths choose.
func canonRows(rows [][]Value) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		s := ""
		for _, v := range r {
			s += fmt.Sprintf("|%d:%d:%g:%q:%v", v.Typ, v.I, v.F, string(v.S), v.Subtype)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
