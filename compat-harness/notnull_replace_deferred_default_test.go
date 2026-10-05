// Tests NOT NULL ... ON CONFLICT REPLACE with deferred DEFAULT expressions.
// FOLDED default (columnInfo.DefaultKnown), and the NOT NULL re-check just
// beneath it declined outright whenever DefaultKnown was false -- without
// ever asking whether DefaultDeferred could stand in for it, even though
// C SQLite's own C never distinguishes "constant" from "must run per row"
// for this purpose: it just re-emits the column's DEFAULT expression, folded
// or not (pCol->iDflt!=0 is the only gate). See the comment on
// emitInsertRowBody's "A NOT NULL column whose effective action is REPLACE"
// loop for the exact fix.
package compat

import "testing"

// TestNotNullReplaceDeferredDefault covers the three mined fuzz4.test
// statements (a fresh table + DEFAULT VALUES insert each, since a NOT NULL
// DEFAULT clock-reading expression can only ever be exercised through the
// omitted-column path unless a value is explicitly given), plus one
// additional shape not in the corpus: an EXPLICIT NULL against the same kind
// of column, which only the "NOT NULL ON CONFLICT REPLACE" pass (not the
// omitted-column fill) ever sees.
func TestNotNullReplaceDeferredDefault(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fuzz4-100", []string{
			"CREATE TABLE Table0 (Col0  NOT NULL DEFAULT (CURRENT_TIME IS 1 > 1))",
			"INSERT OR REPLACE INTO Table0 DEFAULT VALUES",
			"SELECT * FROM Table0",
		}},
		{"fuzz4-200", []string{
			"CREATE TABLE Table2a(Col0  NOT NULL   DEFAULT (CURRENT_TIME IS 1  IS NOT 1  > 1))",
			"INSERT OR REPLACE INTO Table2a DEFAULT VALUES",
			"SELECT * FROM Table2a",
		}},
		{"fuzz4-210", []string{
			"CREATE TABLE Table2b (Col0  NOT NULL  DEFAULT (CURRENT_TIME  IS NOT FALSE))",
			"INSERT OR REPLACE INTO Table2b DEFAULT VALUES",
			"SELECT * FROM Table2b",
		}},
		{"explicit-null-deferred-default", []string{
			"CREATE TABLE t4(Col0 NOT NULL DEFAULT (CURRENT_TIME IS 1 > 1))",
			"INSERT OR REPLACE INTO t4(Col0) VALUES(NULL)",
			"SELECT * FROM t4",
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestNotNullReplaceRandomDefaultStillDeclines pins the excluded class this
// fix's soundness depends on: a NOT NULL ON CONFLICT REPLACE column whose
// DEFAULT genuinely cannot be reproduced at all (random()/randomblob() --
// columnInfo.DefaultDeferred is nil for these, not merely DefaultKnown
// false, per its own doc comment), given an EXPLICIT NULL so the omitted-
// column path (insertRejectOmittedDefaults, a different and unrelated
// decline) never gets a chance to fire first -- this isolates the exact
// branch this change touched.
func TestNotNullReplaceRandomDefaultStillDeclines(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t5(Col0 NOT NULL DEFAULT (random()))",
		"INSERT OR REPLACE INTO t5(Col0) VALUES(NULL)",
	}
	res := run(t, "musql", stmts)
	last := res[len(res)-1]
	if last["kind"] != "error" {
		t.Fatalf("expected musql to still decline a NOT NULL ON CONFLICT REPLACE column whose DEFAULT is random() (unreproducible against an independently-seeded oracle), got: %v", last)
	}
	// The oracle must still ACCEPT and run it (confirms this is a real,
	// reachable shape rather than something both sides already reject).
	oracle := run(t, "cgo", stmts)
	oLast := oracle[len(oracle)-1]
	if oLast["kind"] == "error" {
		t.Fatalf("expected the oracle to accept random() as a NOT NULL ON CONFLICT REPLACE default -- pin is stale: %v", oLast)
	}
}
