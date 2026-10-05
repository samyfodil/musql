// Tests aggregate item handling in window function contexts.
// reaching it and answered from compiled code with byte-identical values, and
// all 12 of TestAggItemWindowAggArgPlaceholderMetadata's did -- 0 of those 12
// compiled before.
//
// All 18 statements below agree with 3.53.3. The last to lower was
//
//	SELECT k, (SELECT sum(n) FILTER (WHERE t.v > (SELECT max(n) FROM b b2
//	  WHERE b2.n=b.n)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k
//
// whose FILTER was refused because "max(n)" could have been the window
// query's own aggregate; b2's FROM binds n first (aggShadow,
// engine/vdbe_window.go).
package compat

import "testing"

// TestAggItemWindowAggArgPlaceholder is the value gate. Every statement here
// answers in 3.53.3 and answered here before the window flag was deleted; each
// one puts a reference to the enclosing group in a window aggregate's
// argument, FILTER or separator.
func TestAggItemWindowAggArgPlaceholder(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k INTEGER, v INTEGER, s TEXT)`,
		`INSERT INTO t VALUES(1,10,'a'),(1,20,'b'),(2,30,'c')`,
		`CREATE TABLE b(n INTEGER)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
	}
	flLockstep(t, "placeholder in a window aggregate's argument", setup,
		// The buffered family: these compile today, and they are here because
		// the SAME substitution decision feeds them and the droppable ones.
		`SELECT k, (SELECT sum(t.v) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(n + t.v) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT max(t.v) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT avg(t.v) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT group_concat(n, t.s) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(n) FILTER (WHERE n > t.v - 19) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(t.rowid) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,

		// The SUBTYPE pair, whose arguments are never buffered. These are the
		// ones that answered "no such column (no anchor row available)".
		`SELECT k, (SELECT json_group_array(t.v) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT json_group_object(t.s, t.v) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT json_group_array(t.rowid) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// ...and this one PANICKED: sum(t.v) is HOISTED, so the node in
		// json_group_array's argument is a groupAggExpr, whose reader at the
		// time had no bounds check at all.
		`SELECT k, (SELECT json_group_array(sum(t.v)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT json_group_array(sum(t.v) + n) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT json_group_object(t.s, sum(t.v)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT json_group_array(count(t.v)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT json_group_array(total(t.v)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,

		// The OTHER door, independent of the subtype pair: an argument or
		// FILTER windowOperandLowerable refuses, so add() never claims a batch
		// column for it and rowValue has no register to read -- which is where
		// the last of the three is refused outright today (see DISCLOSURE
		// above).
		`SELECT k, (SELECT sum(t.v + (SELECT max(n) FROM b b2 WHERE b2.n<=b.n)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(t.v + (SELECT count(*) FROM b b2 WHERE b2.n=b.n)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(n) FILTER (WHERE t.v > (SELECT max(n) FROM b b2 WHERE b2.n=b.n)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
	)
}

// TestAggItemWindowAggArgPlaceholderMetadata is the gate for what the
// substitution's WINDOW carve-out closing actually changed: a placeholder in a
// BUFFERED window aggregate's argument / FILTER / separator is now lowered --
// planWindowOperands claims a batch column for it and emitWindowOperands
// compiles it down in the window query's own chain, where the enclosing item's
// register block is reachable -- instead of being left to a per-group
// substitution that spliced it in as a literal.
//
// A lowered read is only equal to that literal if it carries the same
// METADATA, which is where a substitution promotion has been wrong before: the
// literal rewriteExprOuterRefs splices in keeps the column's declared collation
// and its affinity, and a register read that dropped either would rank a
// comparison differently. Each case below puts the placeholder somewhere the
// answer depends on one of them.
//
// The three placeholder KINDS are separated too, because they read three
// different register blocks: the anchor row's column (groupBareColExpr), its
// rowid, the finalized accumulator (groupAggExpr, from a hoisted call) and the
// GROUP BY key tuple (groupKeyExpr, which only a non-column key expression
// reads).
func TestAggItemWindowAggArgPlaceholderMetadata(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k INTEGER, v INTEGER, s TEXT COLLATE NOCASE, n2 TEXT)`,
		`INSERT INTO t VALUES(1,10,'aBc','2'),(1,20,'ABC','3'),(2,30,'xyz','4')`,
		`CREATE TABLE b(n INTEGER)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
	}
	flLockstep(t, "window aggregate argument placeholder metadata", setup,
		// The anchor-row column's DECLARED COLLATION decides the comparison
		// inside the buffered argument.
		`SELECT k, (SELECT sum(t.s = 'abc') OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(n) FILTER (WHERE t.s = 'ABC') OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// ...and its AFFINITY: n2 is TEXT-declared, so "t.n2 = 2" compares as
		// text on one side and applies the column's affinity on the other.
		`SELECT k, (SELECT sum(t.n2 = 2) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(t.n2 > 20) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// The FINALIZED ACCUMULATOR (a hoisted call) inside the argument, and
		// beside the window query's own column.
		`SELECT k, (SELECT sum(sum(t.v) + n) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT max(count(t.v)) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// The GROUP BY KEY tuple: only a non-column key expression reads that
		// block (a bare key column resolves to the anchor row instead).
		`SELECT k+1, (SELECT sum(n + (k+1)) OVER () FROM b LIMIT 1) FROM t GROUP BY k+1 ORDER BY 1`,
		// The SEPARATOR position, which is a third operand of its own.
		`SELECT k, (SELECT group_concat(n, t.s) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT group_concat(t.s, '-') OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// The anchor ROWID block, and a DISTINCT window aggregate over it.
		`SELECT k, (SELECT sum(t.rowid + n) OVER () FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// A window spec whose PARTITION BY / ORDER BY also name the group, so
		// the operand list holds a placeholder in three positions at once.
		`SELECT k, (SELECT sum(t.v) OVER (PARTITION BY t.k ORDER BY n) FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		// ...and a frame whose bounds are ordinary, with the placeholder in the
		// argument, so the buffered column is read once per frame position.
		`SELECT k, (SELECT sum(n * t.v) OVER (ORDER BY n ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM b ORDER BY n DESC LIMIT 1) FROM t GROUP BY k ORDER BY k`,
	)
}
