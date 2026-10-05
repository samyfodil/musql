// This file gates INSERT ... ON CONFLICT ... RETURNING (UPSERT with RETURNING),
// verifying the correct row is returned for each arm (DO UPDATE, DO NOTHING, and no conflict).
//
//   - an UPSERT combined with an INSERT ... SELECT source. It was declined
//     because "the per-row capture above is wired into insertRowsWithConflict's
//     VALUES-sourced caller only" -- a fact about one of this engine's own
//     emitters, not about SQLite, which codes the RETURNING inside its ONE
//     insertion loop whichever template filled the register block
//     (insert.c:1604-1608, above the endOfLoop label at :1613). The compiled
//     tail is shared by both row sources, so it inherited the shape.
//   - a TRIGGERED table, whose guard is still in insertRowsWithConflict and
//     would still fire on the fallback route; nothing reaches it now. The
//     compiled route serves the shape, with the trigger LOG pinned in
//     upsert_returning_codegen_test.go and in returning_trigger_test.go's
//     TestReturningTriggerConflictClause, which is where that decline's own
//     pin (TestReturningTriggerStillDeclined) became a parity case.
//
// An explicit OR-clause (IGNORE/REPLACE/FAIL/ROLLBACK) NO LONGER declines: the
// VDBE write compiler lowers "INSERT ... VALUES ... RETURNING" carrying one
// (engine/vdbe_write.go's insertPlan.skipAddr), and the open question this
// header used to flag -- WHICH rows belong in an IGNORE/REPLACE result set --
// now has C SQLite's own answer: RETURNING is coded as an AFTER trigger
// (codeReturningTrigger, trigger.c:1507) emitted past the OE_Ignore jump
// (insert.c:2369 / :2595), so a conflict-IGNOREd row produces NO row. The
// four clauses are therefore compared against the oracle at the bottom of
// TestUpsertReturning rather than asserted to decline.
package compat

import "testing"

func TestUpsertReturning(t *testing.T) {
	seed := []string{
		`CREATE TABLE t4(a INTEGER PRIMARY KEY, b, c)`,
		`INSERT INTO t4 VALUES(1,2,3)`,
	}
	// DO UPDATE over a real conflict: the UPDATED row, not the incoming
	// VALUES tuple.
	differ(t, "do update over a conflict", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(1,22,33) ON CONFLICT(a) DO UPDATE SET b=44 RETURNING *`,
		`SELECT a,b,c FROM t4`,
	))
	// DO UPDATE with no actual conflict: the freshly inserted row.
	differ(t, "do update with no conflict", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(9,22,33) ON CONFLICT(a) DO UPDATE SET b=44 RETURNING *`,
		`SELECT a,b,c FROM t4`,
	))
	// DO NOTHING over a real conflict: the skipped row does not appear.
	differ(t, "do nothing over a conflict", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(1,55,66) ON CONFLICT(a) DO NOTHING RETURNING *`,
		`SELECT a,b,c FROM t4`,
	))
	// DO NOTHING with no conflict: an ordinary insert.
	differ(t, "do nothing with no conflict", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(2,55,66) ON CONFLICT(a) DO NOTHING RETURNING *`,
		`SELECT a,b,c FROM t4`,
	))
	// A DO UPDATE whose own WHERE is false is a silent no-op, exactly like DO
	// NOTHING -- the row does not appear.
	differ(t, "do update with a false WHERE", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(1,22,33) ON CONFLICT(a) DO UPDATE SET b=44 WHERE b>1000 RETURNING *`,
		`SELECT a,b,c FROM t4`,
	))
	// excluded.col in the SET, and rowid in the RETURNING list -- the IPK
	// column's own name ("a"), not the literal word "rowid".
	differ(t, "excluded and rowid", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(1,77,88) ON CONFLICT(a) DO UPDATE SET b=excluded.b RETURNING a,b,c,rowid`,
		`SELECT a,b,c FROM t4`,
	))
	// A DO UPDATE whose own SET reassigns the IPK column: RETURNING must
	// report the row at its NEW rowid, not the conflicting row's old one --
	// this is exactly why applyUpsert hands insertRowsWithConflict the
	// update's own final rowid rather than assuming it stayed at
	// matched[0].rowid.
	differ(t, "do update reassigns the IPK column", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(1,22,33) ON CONFLICT(a) DO UPDATE SET a=99 RETURNING *`,
		`SELECT a,b,c FROM t4`,
	))
	// A multi-row VALUES upsert where every row targets the SAME conflict
	// key: each row resolves against the LIVE state the row before it just
	// left, not a shared pre-statement snapshot, and RETURNING captures each
	// row's own POST-update image at the moment it was processed.
	differ(t, "multi-row upsert same key sequential", append(append([]string{}, seed...),
		`INSERT INTO t4(a,b,c) VALUES(1,10,10),(1,20,20) ON CONFLICT(a) DO UPDATE SET b=t4.b+excluded.b RETURNING a,b,c`,
		`SELECT a,b,c FROM t4`,
	))

	// NO LONGER declined: an upsert combined with an INSERT ... SELECT source.
	// This asserted "kind == error" until emitUpsertTail learned to emit the
	// RETURNING block, and the decline it was pinning was a property of
	// insertRowsWithConflict's wiring rather than of the shape -- see this
	// file's header. A stale decline-assertion is exactly what hides the answer
	// being wrong, so it is oracle agreement now, on the very statement it used
	// to require to fail.
	differ(t, "upsert over a SELECT source with RETURNING", []string{
		`CREATE TABLE t4(a INTEGER PRIMARY KEY, b, c)`,
		`INSERT INTO t4 VALUES(1,2,3)`,
		`CREATE TABLE src(a,b,c)`,
		`INSERT INTO src VALUES(1,22,33)`,
		`INSERT INTO t4(a,b,c) SELECT a,b,c FROM src WHERE true ON CONFLICT(a) DO UPDATE SET b=44 RETURNING *`,
		`SELECT a,b,c FROM t4 ORDER BY a`,
	})
	// NO LONGER declined: an explicit OR-clause with RETURNING (no upsert).
	// This used to assert a DECLINE; the compiler lowers the shape now, so the
	// assertion is oracle agreement instead -- a stale decline-assertion is
	// exactly what hides the answer being wrong. OR IGNORE yields no row for
	// the conflicting candidate and OR REPLACE yields the replacement (1,9);
	// OR FAIL and OR ROLLBACK error, on a single-row VALUES so that nothing is
	// half-applied for the worker's failing-statement re-run to disagree about.
	for _, or := range []string{"OR IGNORE", "OR REPLACE", "OR FAIL", "OR ROLLBACK"} {
		differ(t, "insert "+or+" returning", []string{
			`CREATE TABLE t4(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t4 VALUES(1,2)`,
			"INSERT " + or + ` INTO t4 VALUES(1,9) RETURNING *`,
			`SELECT a,b FROM t4 ORDER BY a`,
		})
	}
	// The multi-row spelling, where the OE_Ignore jump has to skip exactly the
	// conflicting row's RETURNING and no other.
	differ(t, "insert or ignore returning multi-row", []string{
		`CREATE TABLE t4(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t4 VALUES(1,2)`,
		`INSERT OR IGNORE INTO t4 VALUES(1,9),(2,8),(1,7),(3,6) RETURNING *`,
		`SELECT a,b FROM t4 ORDER BY a`,
	})
}
