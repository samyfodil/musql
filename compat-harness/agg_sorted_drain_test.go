package compat

import "testing"

// Tests sorted GROUP BY aggregate accumulator lowering.
func TestAggSortedDrainLowering(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k,v,s)`,
		`INSERT INTO t VALUES(1,-9223372036854775808,'a'),(1,5,'b'),(2,7,'c'),(2,NULL,NULL),(3,-1,'d')`,
		`CREATE TABLE u(k,w)`,
		`INSERT INTO u VALUES(1,10),(3,30)`,
	}
	for _, c := range []struct{ name, sql string }{
		// The plain shapes, one per accumulator kind, all on the drain (a
		// trailing ORDER BY is what takes the sorter path rather than the hash
		// one).
		{"sum", `SELECT k, sum(v) FROM t GROUP BY k ORDER BY k`},
		{"count", `SELECT k, count(v), count(*) FROM t GROUP BY k ORDER BY k`},
		{"avg-total", `SELECT k, avg(v), total(v) FROM t GROUP BY k ORDER BY k`},
		{"minmax", `SELECT k, min(v), max(v), min(s), max(s) FROM t GROUP BY k ORDER BY k`},
		{"distinct", `SELECT k, count(DISTINCT v), sum(DISTINCT v) FROM t GROUP BY k ORDER BY k`},
		{"gc", `SELECT k, group_concat(s), group_concat(s,'-') FROM t GROUP BY k ORDER BY k`},
		{"json", `SELECT k, json_group_array(v), json_group_object(s,v) FROM t GROUP BY k ORDER BY k`},

		// A WIDER argument: only the leaf comes from the record, the rest is
		// coded normally, so this is where a leaf mapped to the wrong record
		// slot would show up as a wrong number rather than an error.
		{"wider-arg", `SELECT k, sum(v * 2 + length(s)) FROM t GROUP BY k ORDER BY k`},
		{"wider-sep", `SELECT k, group_concat(s, s || 'x') FROM t GROUP BY k ORDER BY k`},
		{"case-arg", `SELECT k, sum(CASE WHEN v > 0 THEN v ELSE 0 END) FROM t GROUP BY k ORDER BY k`},
		{"collate-arg", `SELECT k, max(s COLLATE NOCASE) FROM t GROUP BY k ORDER BY k`},

		// The rowid pseudo-column, which lives in the record's own rowid block
		// rather than its columns block -- a different slot mapping.
		{"rowid-arg", `SELECT k, sum(rowid), max(t.rowid) FROM t GROUP BY k ORDER BY k`},

		// The FILTER, and the ORDERING hazard it guards. abs() of the minimum
		// int64 is an "integer overflow" error, and exactly one row carries it:
		// a row this FILTER rejects. The argument must be computed BEHIND the
		// filter's jump (sqlite3ExprIfFalse, select.c:6855, ahead of the
		// argument list at :6902-6906) or the query stops answering.
		{"filter-excludes-overflow", `SELECT k, sum(abs(v)) FILTER (WHERE v > -9223372036854775808) FROM t GROUP BY k ORDER BY k`},
		{"filter-null-row", `SELECT k, count(*) FILTER (WHERE v IS NOT NULL) FROM t GROUP BY k ORDER BY k`},
		{"filter-jumpifnull", `SELECT k, sum(v) FILTER (WHERE v) FROM t GROUP BY k ORDER BY k`},
		{"filter-plus-sep", `SELECT k, group_concat(s,'-') FILTER (WHERE v > 0) FROM t GROUP BY k ORDER BY k`},
		{"filter-two-accs", `SELECT k, sum(v), max(v) FILTER (WHERE v > 0) FROM t GROUP BY k ORDER BY k`},

		// The raising-body ordering rule on the drain: group_concat's own body
		// can raise, so nothing coded after it may be hoisted ahead of it.
		{"raising-body-order", `SELECT k, group_concat(s), sum(abs(v)) FROM t GROUP BY k ORDER BY k`},

		// The group's IDENTITY and ORDER, which the drain also owns: a bare
		// column anchors to a particular row of the group, HAVING filters after
		// finalize, and DISTINCT re-resolves the whole batch.
		{"bare-anchor", `SELECT k, max(v), s FROM t GROUP BY k ORDER BY k`},
		{"having", `SELECT k, sum(v) FROM t GROUP BY k HAVING count(*) > 1 ORDER BY k`},
		{"distinct-batch", `SELECT DISTINCT k, count(*) FROM t GROUP BY k ORDER BY k`},
		{"order-by-agg", `SELECT k, sum(v) FROM t GROUP BY k ORDER BY sum(v)`},
		{"limit-offset", `SELECT k, sum(v) FROM t GROUP BY k ORDER BY k LIMIT 2 OFFSET 1`},

		// Over a JOIN, where the record's columns block spans two tables and a
		// LEFT JOIN's unmatched row contributes NULLs -- the case that would
		// catch a per-source offset applied to the wrong table.
		{"join", `SELECT t.k, sum(u.w), sum(t.v) FROM t JOIN u ON u.k=t.k GROUP BY t.k ORDER BY t.k`},
		{"left-join", `SELECT t.k, sum(u.w), count(u.w), group_concat(u.w) FROM t LEFT JOIN u ON u.k=t.k GROUP BY t.k ORDER BY t.k`},
		{"left-join-rowid", `SELECT t.k, count(u.rowid) FROM t LEFT JOIN u ON u.k=t.k GROUP BY t.k ORDER BY t.k`},

		// A correlated subquery in the aggregate's argument, which the drain
		// refuses (its body would reach back into cursors that closed with the
		// scan) -- kept so the refusal is an ANSWER and not a decline.
		{"subquery-arg", `SELECT k, sum((SELECT max(w) FROM u WHERE u.k=t.k)) FROM t GROUP BY k ORDER BY k`},
	} {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, append(append([]string{}, setup...), c.sql)) })
	}
}

// TestAggSortedDrainJoinUsing is the drain's SHARED-COLUMN battery, and every
// case in it was a wrong answer before aggDrainRow.slotFor required its two
// resolvers to agree.
//
// An unqualified "a" over "t1 JOIN t2 USING(a)" is one reference with two
// candidate slots in the record, and the two resolvers picked different ones:
// resolveInScopes -- the authority, and what the cursor-driven compile reads --
// takes the FIRST matching FROM item, while resolveRowReg walks the scope stack
// innermost-first and took the LAST. On a LEFT JOIN whose right side does not
// match, that is t2's NULL-extended a instead of t1's a:
//
//	SELECT b, sum(a) FROM t1 LEFT JOIN t2 USING(a) GROUP BY b ORDER BY b
//	    3.53.3: (10,1),(20,2)     musql was: (10,NULL),(20,2)
//
// A RIGHT/FULL JOIN's shared column is worse than merely mis-picked: it is a
// COALESCE over both sides (emitColumnReadCoalesce), which no single register
// can be, so slotFor refuses it outright and the drain emits the OpNotNull
// chain over its own row block instead (aggDrainRow.compileCoalesce).
//
// The no-ORDER-BY twins are here deliberately. They take the hash path rather
// than the drain, they were always right, and they are what makes this file's
// claim falsifiable -- a fix that broke the shared column everywhere would pass
// a battery made only of drain spellings.
func TestAggSortedDrainJoinUsing(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE t2(a, c)`,
		`INSERT INTO t1 VALUES(1,10),(2,20)`,
		`INSERT INTO t2 VALUES(2,200),(3,300)`,
	}
	for _, c := range []struct{ name, sql string }{
		{"left-using", `SELECT b, sum(a) FROM t1 LEFT JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		{"full-using", `SELECT b, sum(a) FROM t1 FULL JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		{"right-using", `SELECT b, sum(a) FROM t1 RIGHT JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		{"natural-left", `SELECT b, sum(a) FROM t1 NATURAL LEFT JOIN t2 GROUP BY b ORDER BY b`},
		{"inner-using", `SELECT b, sum(a) FROM t1 JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		{"full-count", `SELECT b, count(a) FROM t1 FULL JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		{"left-gc", `SELECT b, group_concat(a) FROM t1 LEFT JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		{"full-minmax", `SELECT b, max(a), min(a) FROM t1 FULL JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		{"left-filter", `SELECT b, sum(c) FILTER (WHERE a > 1) FROM t1 LEFT JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		// Qualified, so there is no shared-name question at all -- the control.
		{"left-qualified", `SELECT b, sum(t1.a), sum(t2.a) FROM t1 LEFT JOIN t2 USING(a) GROUP BY b ORDER BY b`},
		// The hash-path twins: same shared column, never through the drain.
		{"left-using-hash", `SELECT b, sum(a) FROM t1 LEFT JOIN t2 USING(a) GROUP BY b`},
		{"full-using-hash", `SELECT b, sum(a) FROM t1 FULL JOIN t2 USING(a) GROUP BY b`},
	} {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, append(append([]string{}, setup...), c.sql)) })
	}
}
