// Test window function operand compilation and buffering.
// Verify FILTER buffering and SUBTYPE argument evaluation order match C SQLite.
package compat

import "testing"

var w1Fixture = []string{
	`CREATE TABLE tj(id INTEGER, x TEXT, j TEXT)`,
	`INSERT INTO tj VALUES(1,'a','[1]'),(2,'b','[2]'),(3,'c','[3]'),(4,'a','[4]'),(5,NULL,'null')`,
	`CREATE TABLE t1(a, b)`,
	`INSERT INTO t1 VALUES(1,10),(2,20),(3,30)`,
	`CREATE TABLE t3(x, y)`,
	`INSERT INTO t3 VALUES(1,'one'),(2,'two'),(3,'three')`,
	// Table with overflow value that tests error frame boundaries.
	`CREATE TABLE ov(k INTEGER, v INTEGER)`,
	`INSERT INTO ov VALUES(1,1),(2,-9223372036854775808),(3,3),(4,4)`,
	// Overflow value positioned at end to test backward-looking frame handling.
	`CREATE TABLE ovl(k INTEGER, v INTEGER)`,
	`INSERT INTO ovl VALUES(1,1),(2,2),(3,3),(4,-9223372036854775808)`,
}

func w1Parity(t *testing.T, name string, probes []string) {
	t.Helper()
	driverParity(t, name, append(append([]string(nil), w1Fixture...), probes...))
}

// TestW1ProjectionCorrelatedSubquery tests projection subqueries with window function correlation.
func TestW1ProjectionCorrelatedSubquery(t *testing.T) {
	w1Parity(t, "projection-correlated-subquery", []string{
		`SELECT a, (SELECT y FROM t3 WHERE x=a), sum(a) OVER (ORDER BY a) FROM t1 ORDER BY a`,
		`SELECT a, (SELECT y FROM t3 WHERE x=t1.a), sum(a) OVER (ORDER BY a) FROM t1 ORDER BY a`,
		`SELECT a, EXISTS(SELECT 1 FROM t3 WHERE x=a), sum(b) OVER () FROM t1 ORDER BY a`,
		`SELECT a, NOT EXISTS(SELECT 1 FROM t3 WHERE x=a+9), sum(b) OVER () FROM t1 ORDER BY a`,
		`SELECT a, a IN (SELECT x FROM t3 WHERE y<>'two'), count(*) OVER () FROM t1 ORDER BY a`,
		// The correlated reference inside an ORDER BY term, which joins the
		// same projection list (buildWindowProjList).
		`SELECT a, sum(a) OVER (ORDER BY a) FROM t1 ORDER BY (SELECT y FROM t3 WHERE x=a)`,
		// A row the subquery finds nothing for, and a NULL-valued correlation.
		`SELECT a, (SELECT y FROM t3 WHERE x=a*10), sum(a) OVER () FROM t1 ORDER BY a`,
		// Nested one level deeper.
		`SELECT a, (SELECT (SELECT y FROM t3 WHERE x=a)), sum(a) OVER () FROM t1 ORDER BY a`,
		// The UNCORRELATED spellings, which the projection serves through its
		// run-once subquery cache (Program.NSubCache) rather than per row; they
		// must answer identically either way.
		`SELECT a, (SELECT max(y) FROM t3), sum(a) OVER () FROM t1 ORDER BY a`,
		`SELECT a, a IN (SELECT x FROM t3), sum(a) OVER () FROM t1 ORDER BY a`,
	})
}

// TestW1FilterBuffered drives the FILTER widening over the aggregates whose
// arguments are NOT buffered, which is where the change bites.
func TestW1FilterBuffered(t *testing.T) {
	w1Parity(t, "window-filter-buffered", []string{
		`SELECT id, count() FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, count(*) FILTER (WHERE id<>2) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM tj ORDER BY id`,
		`SELECT id, count(*) FILTER (WHERE x IS NOT NULL) OVER (PARTITION BY x ORDER BY id) FROM tj ORDER BY id`,
		// A FILTER that is NULL rather than false on some rows.
		`SELECT id, count(*) FILTER (WHERE x) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(json(j)) FILTER (WHERE id<>2) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, json_group_object(x, json(j)) FILTER (WHERE x IS NOT NULL) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, sum(id) FILTER (WHERE id%2=1) OVER (ORDER BY id) FROM tj ORDER BY id`,
		// A FILTER referring to a column no frame reads, evaluated per buffered
		// row exactly as C evaluates it.
		`SELECT id, count(*) FILTER (WHERE length(j)>3) OVER () FROM tj ORDER BY id`,
	})
}

// TestW1SubtypeStepArgs drives the step-time argument program over the frame
// shapes, the ordering the JSON aggregates are sensitive to, and the subtype
// itself -- json_group_array(json(j)) must produce nested JSON, not a string,
// which is the whole point of the SQLITE_SUBTYPE flag.
func TestW1SubtypeStepArgs(t *testing.T) {
	w1Parity(t, "window-subtype-step-args", []string{
		`SELECT id, json_group_array(json(j)) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(j) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(json(j)) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(json(j)) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(json(j)) OVER (ORDER BY id ROWS BETWEEN 1 FOLLOWING AND 2 FOLLOWING) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(json(j)) OVER (ORDER BY id RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE TIES) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(json(j)) OVER (PARTITION BY x ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(DISTINCT x) OVER () FROM tj ORDER BY id`,
		`SELECT id, json_group_object(x, json(j)) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, json_group_object(x, json(j)) OVER (ORDER BY id ROWS BETWEEN 1 FOLLOWING AND 2 FOLLOWING) FROM tj ORDER BY id`,
		// The KEY and the VALUE must not swap: they are argument 0 and argument
		// 1, and the step-arg registers are stamped positionally.
		`SELECT id, json_group_object(x, id) OVER () FROM tj ORDER BY id`,
		`SELECT id, json_group_object(id, x) OVER () FROM tj ORDER BY id`,
		// An expression argument rather than a bare column, so the compiled
		// program has real work to do.
		`SELECT id, json_group_array(json_object('k', x, 'n', id*2)) OVER (ORDER BY id) FROM tj ORDER BY id`,
		`SELECT id, json_group_array(CASE WHEN id>3 THEN NULL ELSE x END) OVER (ORDER BY id) FROM tj ORDER BY id`,
		// Two calls over one named window, which share the spec's key columns.
		`SELECT id, json_group_array(json(j)) OVER w, json_group_array(x) OVER w FROM tj WINDOW w AS (ORDER BY id) ORDER BY id`,
		`SELECT id, group_concat(x) OVER w, json_group_array(x) OVER w FROM tj WINDOW w AS (ORDER BY id) ORDER BY id`,
	})
}

// TestW1SubtypeArgEvaluatedPerStep is the ORDERING gate, and the reason SQLite
// re-codes a SUBTYPE argument at step time instead of buffering it: the
// argument is evaluated for the rows a frame READS, never for every buffered
// row. The middle row's abs() raises "integer overflow", so the two behaviours
// are distinguishable rather than merely different in cost.
//
// The first probe is the shape windowAggArgsLowerable's own doc comment records
// as MEASURED in 3.53.3: json_group_array answers because the offending row is
// in no frame, while sum() over the same expression raises.
//
// Mutation-tested: computing the step-args program for every buffered row
// instead of per step turns the first two probes into "integer overflow" here
// and leaves the oracle answering, which this catches.
func TestW1SubtypeArgEvaluatedPerStep(t *testing.T) {
	w1Parity(t, "window-subtype-arg-per-step", []string{
		// ovl's offending row is LAST, so no frame here reaches it: 3.53.3
		// ANSWERS these, and so must this engine.
		`SELECT k, json_group_array(abs(v)) OVER (ORDER BY k ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM ovl ORDER BY k`,
		`SELECT k, json_group_array(abs(v)) OVER (ORDER BY k ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) FROM ovl ORDER BY k`,
		`SELECT k, json_group_object(k, abs(v)) OVER (ORDER BY k ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM ovl ORDER BY k`,
		// And the same expression where a frame DOES reach the offending row:
		// both engines must raise.
		`SELECT k, json_group_array(abs(v)) OVER (ORDER BY k) FROM ovl ORDER BY k`,
		// sum()'s argument IS buffered (windowAggArgsLowerable), which is
		// SQLite's shape too -- so this raises in both engines even though no
		// frame reads the offending row. The contrast with the first probe is
		// the whole point of the bExprArgs split.
		`SELECT k, sum(abs(v)) OVER (ORDER BY k ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM ovl ORDER BY k`,
	})
}

// TestW1SubtypeArgSkipsFilteredRows is the second ordering gate: the FILTER's
// OP_IfNot jumps PAST the argument coding (window.c:1694-1696 to :1758), so a
// row the FILTER rejects never evaluates the argument at all -- even when it is
// squarely inside the frame.
//
// Mutation-tested: dropping the filter test that guards the step-args run in
// windowAggregate (engine/vdbe_window.go) makes every probe below report
// "integer overflow" where 3.53.3 answers.
func TestW1SubtypeArgSkipsFilteredRows(t *testing.T) {
	w1Parity(t, "window-subtype-arg-filtered", []string{
		`SELECT k, json_group_array(abs(v)) FILTER (WHERE k<>2) OVER (ORDER BY k) FROM ov ORDER BY k`,
		`SELECT k, json_group_array(abs(v)) FILTER (WHERE v>0) OVER () FROM ov ORDER BY k`,
		`SELECT k, json_group_object(k, abs(v)) FILTER (WHERE k<>2) OVER (ORDER BY k) FROM ov ORDER BY k`,
		// A FILTER that is NULL on the offending row rather than false --
		// isTruthy's own rule, and C's sqlite3ExprIfFalse.
		`SELECT k, json_group_array(abs(v)) FILTER (WHERE nullif(k,2)) OVER (ORDER BY k) FROM ov ORDER BY k`,
	})
}

// TestW1UnlowerableOperandDeclines pins what the DELETED fallback used to
// answer. Neither of these can be a musql VALUE any more; the oracle rejects
// them too, so a mutual rejection is the parity outcome and the probe records
// whatever text each side produces.
//
// The nested-window spelling is the one that matters: C SQLite raises
// "abs() may not be used as a window function", while the arm this slice
// removed silently DROPPED the inner OVER and answered. Declining is therefore
// a wrong answer removed, not merely a re-routing.
func TestW1UnlowerableOperandDeclines(t *testing.T) {
	for _, q := range []string{
		`SELECT nth_value(a, abs(a) OVER (ORDER BY a)) OVER (ORDER BY a) FROM t1`,
		`SELECT lead(a, abs(a) OVER (ORDER BY a)) OVER (ORDER BY a) FROM t1`,
	} {
		w1Parity(t, "window-unlowerable-operand", []string{q})
	}
}
