// Tests that unknown collations can be named in result expressions but not
// consumed in comparisons, ordering, or grouping operations.
package compat

import "testing"

func TestInertUnknownCollation(t *testing.T) {
	// e_expr.test's own four, plus the neighbours that pin the value AND the
	// column name (SQLite echoes the source text, COLLATE clause included).
	differ(t, "an unknown collation on a closed comparison", []string{
		`SELECT ('abcd' < 'bbbb') COLLATE reverse`,
		`SELECT ('abcd' <= 'bbbb') COLLATE reverse`,
		`SELECT ('abcd' > 'bbbb') COLLATE reverse`,
		`SELECT ('abcd' >= 'bbbb') COLLATE reverse`,
		`SELECT ('abcd' IS 'bbbb') COLLATE reverse`,
		`SELECT ('abcd' LIKE 'bbbb') COLLATE reverse`,
		`SELECT ('abcd' IN ('bbbb')) COLLATE reverse`,
	})
	// The collation is equally inert over a value that was never compared.
	differ(t, "an unknown collation on a plain value", []string{
		`SELECT 'abcd' COLLATE reverse`,
		`SELECT 1 COLLATE nosuchcollation`,
		`SELECT ('abcd' || 'x') COLLATE reverse`,
		`SELECT abs(-1) COLLATE reverse`,
		`SELECT (SELECT 1) COLLATE reverse`,
		`SELECT NULL COLLATE reverse`,
	})
	// More than one in the same statement, and one beside an ordinary column.
	differ(t, "several unknown collations in one select list", []string{
		`SELECT ('a'<'b') COLLATE reverse, ('c'<'d') COLLATE reverse`,
		`SELECT 7 AS n, ('a'<'b') COLLATE reverse`,
		`SELECT ('a'<'b') COLLATE reverse AS r`,
	})
	// A KNOWN collation in the same position keeps working, and still
	// participates where it is consumed.
	differ(t, "a known collation is unaffected", []string{
		`CREATE TABLE t(a TEXT)`,
		`INSERT INTO t VALUES('B'),('a')`,
		`SELECT ('abcd' < 'bbbb') COLLATE NOCASE`,
		`SELECT a FROM t ORDER BY a COLLATE NOCASE`,
		`SELECT 'A' = 'a' COLLATE NOCASE AS eq`,
	})
}

// TestConsumedUnknownCollationStillErrors is the wrong-answer boundary: each of
// these CONSUMES the collation, and C SQLite rejects every one. Run through
// differ so the assertion is mutual -- if musql ever started answering one,
// this fails on the difference rather than on a hand-written expectation.
func TestConsumedUnknownCollationStillErrors(t *testing.T) {
	differ(t, "an unknown collation consumed by a comparison", []string{
		`SELECT 'abcd' < 'bbbb' COLLATE reverse`,
		`SELECT ('abcd' COLLATE reverse) < 'bbbb'`,
		`SELECT 1 AS alive`,
	})
	differ(t, "an unknown collation consumed by a sort or a grouping", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`SELECT a FROM t ORDER BY a COLLATE reverse`,
		`SELECT a FROM t GROUP BY a COLLATE reverse`,
		`SELECT DISTINCT a COLLATE reverse FROM t`,
		`SELECT DISTINCT ('a'<'b') COLLATE reverse`,
		`SELECT max(a COLLATE reverse) FROM t`,
		`SELECT ('abcd' < 'bbbb') COLLATE reverse ORDER BY 1`,
		`SELECT 1 AS alive`,
	})
	differ(t, "an unknown collation in a schema object", []string{
		`CREATE TABLE t2(a TEXT COLLATE reverse)`,
		`CREATE TABLE t3(a)`,
		`CREATE INDEX i3 ON t3(a COLLATE reverse)`,
		`SELECT 1 AS alive`,
	})
}

// TestInertCollationRemainingGaps records what the counting argument does NOT
// cover. Both are statements C SQLite runs; musql declines them, because a
// WHERE or a LIMIT puts the statement outside the shape the retry can prove.
// Asserted one-sided so the boundary is visible rather than assumed -- if a
// later change serves them, this fails and gets updated to a differ case.
func TestInertCollationRemainingGaps(t *testing.T) {
	for _, q := range []string{
		`SELECT ('a'<'b') COLLATE reverse WHERE 1`,
		`SELECT ('a'<'b') COLLATE reverse LIMIT 1`,
	} {
		res := run(t, "musql", []string{q})
		if res[0]["kind"] != "error" {
			t.Errorf("expected %q to still decline; if it now answers, widen the gate", q)
		}
	}
}
