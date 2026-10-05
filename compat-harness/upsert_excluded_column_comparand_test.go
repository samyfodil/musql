package compat

// Tests that excluded.<col> carries no column identity: collation and affinity
// come from the comparand when excluded is used in comparisons.

import "testing"

// TestUpsertExcludedYieldsToAColumnComparandCollation pins the collation
// fall-through: excluded.<col> on the LEFT of a comparison whose RIGHT operand
// is a column declared COLLATE NOCASE must be compared NOCASE, because the
// left contributed no collating sequence at all (expr.c:436-438).
//
// Contributing BINARY instead answers "miss"/leaves the row alone, which is a
// different row image on disk, not merely a different report.
func TestUpsertExcludedYieldsToAColumnComparandCollation(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"DO UPDATE ... WHERE", `SET v='hit' WHERE excluded.v=u.v`},
		{"SET, excluded on the left", `SET v=CASE WHEN excluded.v=u.v THEN 'hit' ELSE 'miss' END`},
		// The mirror image is the control: with a real column on the LEFT the
		// left already resolves (NOCASE), so no fall-through is involved and
		// this spelling was never in doubt.
		{"SET, target column on the left", `SET v=CASE WHEN u.v=excluded.v THEN 'hit' ELSE 'miss' END`},
	} {
		differ(t, "excluded yields its collation to a column comparand: "+tc.name, []string{
			`CREATE TABLE u(k INTEGER PRIMARY KEY, v TEXT COLLATE NOCASE)`,
			`INSERT INTO u VALUES(1,'abc')`,
			`INSERT INTO u VALUES(1,'ABC') ON CONFLICT(k) DO UPDATE ` + tc.tail,
			`SELECT k,v FROM u ORDER BY k`,
		})
	}
	// The CONTROL that must stay red-if-broken in the other direction: with a
	// literal on the right there is nothing to fall through TO, so the
	// comparison is BINARY and 'ABC' does not match 'abc'. A "fix" that hands
	// excluded the target column's NOCASE back would break this one.
	differ(t, "excluded yields its collation to a column comparand: literal comparand stays BINARY", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v TEXT COLLATE NOCASE)`,
		`INSERT INTO u VALUES(1,'x')`,
		`INSERT INTO u VALUES(1,'ABC') ON CONFLICT(k) DO UPDATE SET v=CASE WHEN excluded.v='abc' THEN 'hit' ELSE 'miss' END`,
		`SELECT k,v FROM u ORDER BY k`,
	})
	// And the two-excluded control: NEITHER side resolves, so the comparison is
	// BINARY even though both columns are declared NOCASE.
	differ(t, "excluded yields its collation to a column comparand: two excluded operands stay BINARY", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v TEXT COLLATE NOCASE, w TEXT COLLATE NOCASE, r)`,
		`INSERT INTO u VALUES(1,'q','q','x')`,
		`INSERT INTO u VALUES(1,'ABC','abc','y') ON CONFLICT(k) DO UPDATE SET r=CASE WHEN excluded.v=excluded.w THEN 'hit' ELSE 'miss' END`,
		`SELECT k,r FROM u ORDER BY k`,
	})
}

// TestUpsertExcludedYieldsItsAffinityToAColumnComparand verifies that excluded
// contributes no affinity to comparisons with columns.
func TestUpsertExcludedYieldsItsAffinityToAColumnComparand(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"SET, excluded on the left", `SET r=CASE WHEN excluded.n=u.t THEN 'hit' ELSE 'miss' END`},
		{"SET, TEXT column on the left", `SET r=CASE WHEN u.t=excluded.n THEN 'hit' ELSE 'miss' END`},
		{"DO UPDATE ... WHERE", `SET r='hit' WHERE excluded.n=u.t`},
	} {
		differ(t, "excluded yields its affinity to a column comparand: "+tc.name, []string{
			`CREATE TABLE u(k INTEGER PRIMARY KEY, n INTEGER, t TEXT, r)`,
			`INSERT INTO u VALUES(1,0,'5','x')`,
			`INSERT INTO u VALUES(1,5,'zz','y') ON CONFLICT(k) DO UPDATE ` + tc.tail,
			`SELECT k,r FROM u ORDER BY k`,
		})
	}
	// A TYPELESS excluded column is the same story: C reads its affinity off
	// the rewritten register (0), not off the declaration, so the TEXT column
	// on the right still governs.
	differ(t, "excluded yields its affinity to a column comparand: typeless excluded column", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, z, t TEXT, r)`,
		`INSERT INTO u VALUES(1,0,'5','x')`,
		`INSERT INTO u VALUES(1,5,'zz','y') ON CONFLICT(k) DO UPDATE SET r=CASE WHEN excluded.z=u.t THEN 'hit' ELSE 'miss' END`,
		`SELECT k,r FROM u ORDER BY k`,
	})
}
