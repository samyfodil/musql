// Package compat gates generated-column INSERT...SELECT xfer optimization.
package compat

import "testing"

// TestGencol1XferCorpusRepro tests generated columns with different names.
func TestGencol1XferCorpusRepro(t *testing.T) {
	differ(t, "gencol1.test#0 xfer-eligible INSERT...SELECT", []string{
		`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
		`INSERT INTO t0(c1) VALUES(0)`,
		`CREATE TABLE t1(x AS (typeof(y)), y)`,
		`INSERT INTO t1 SELECT * FROM t0`,
		`SELECT * FROM t1`,
	})
}

// TestGencol1XferMultiRowMiddleColumn tests multiple rows with generated column in middle.
func TestGencol1XferMultiRowMiddleColumn(t *testing.T) {
	differ(t, "gencol1-xfer multi-row, middle generated column", []string{
		`CREATE TABLE t0(a INTEGER, g0 AS (a+1), b INTEGER)`,
		`INSERT INTO t0(a,b) VALUES(1,10),(2,20),(3,30)`,
		`CREATE TABLE t1(a INTEGER, g1 AS (a+1), b INTEGER)`,
		`INSERT INTO t1 SELECT * FROM t0`,
		`SELECT * FROM t1 ORDER BY a`,
	})
}

// TestGencol1XferTriggerBodyPath forces the shape through a TRIGGER BODY,
// which is the harder way in: a body's own "INSERT ... SELECT" must read the
// database as it stands at FIRE time, not through a pager frozen at COMPILE
// time (programReadsFrozenSnapshot, engine/vdbe_trigger.go), so it is served
// by a source re-lowered at fire time (Program.LiveSource,
// engine/vdbe_live_read.go).
//
// It was written when that spelling reached a SECOND, separate insert
// implementation that needed xferOptimizationEligible wired in independently.
// There is one implementation now (compileInsertSelectWrite, vdbe_write.go
// is the single call site), so the case no longer covers a second wiring -- it
// covers the CROSSING, which is worth as much: the same statement, reached the
// harder way, must still answer what 3.53.3 answers.
func TestGencol1XferTriggerBodyPath(t *testing.T) {
	differ(t, "gencol1-xfer through a trigger body", []string{
		`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
		`INSERT INTO t0(c1) VALUES(0)`,
		`CREATE TABLE t1(x AS (typeof(y)), y)`,
		`CREATE TABLE trg_log(msg TEXT)`,
		`CREATE TRIGGER fire_it AFTER INSERT ON trg_log BEGIN INSERT INTO t1 SELECT * FROM t0; END`,
		`INSERT INTO trg_log VALUES('go')`,
		`SELECT * FROM t1`,
	})
}

// TestGencol1XferReturningDeclines pins a gap found via direct oracle probing
// (not in the original investigation): build.c:1439's sqlite3AddReturning
// documents that a RETURNING clause is implemented as a synthetic TEMP AFTER
// trigger on the target table, which trigger.c:64-68's sqlite3TriggerList
// splices into the SAME list sqlite3TriggersExist walks -- making
// insert.c:979's pTrigger non-nil exactly like a real user trigger would,
// which disables xferOptimization at insert.c:1032. Verified directly
// against 3.53.3: appending RETURNING to the mined statement's own shape
// turns the oracle's own success into "table t1 has 1 columns but 2 values
// were supplied" -- a parse-time ERROR. xferOptimizationEligible's
// hasReturning parameter closes exactly this: without it, this engine would
// have wrongly SUCCEEDED where the oracle errors.
func TestGencol1XferReturningDeclines(t *testing.T) {
	differ(t, "gencol1-xfer RETURNING disables the optimization", []string{
		`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
		`INSERT INTO t0(c1) VALUES(0)`,
		`CREATE TABLE t1(x AS (typeof(y)), y)`,
		`INSERT INTO t1 SELECT * FROM t0 RETURNING x, y`,
	})
}

// TestGencol1XferTriggeredDestDeclines pins insert.c:1029-1034's own gate: a
// destination table with an ordinary user INSERT trigger of its OWN also
// makes pTrigger non-nil, so xferOptimization never even runs -- the general
// path's arity check applies and errors, on both engines identically.
func TestGencol1XferTriggeredDestDeclines(t *testing.T) {
	differ(t, "gencol1-xfer dest has its own INSERT trigger", []string{
		`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
		`INSERT INTO t0(c1) VALUES(0)`,
		`CREATE TABLE t1(x AS (typeof(y)), y)`,
		`CREATE TABLE log2(msg TEXT)`,
		`CREATE TRIGGER t1_trig AFTER INSERT ON t1 BEGIN INSERT INTO log2 VALUES('fired'); END`,
		`INSERT INTO t1 SELECT * FROM t0`,
	})
}

// TestGencol1XferBoundaryDeclines exercises xferOptimizationEligible's
// deliberately-narrowed gates (an index/CHECK/declared-DEFAULT on dest, or a
// mismatched generated expression) -- each one insert.c:3012's own gate
// would ALSO decline for one of these shapes (no matching src index/CHECK,
// or the DEFAULT/expression really do differ), so musql's existing arity
// error is correct and matches the oracle's own error exactly.
func TestGencol1XferBoundaryDeclines(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"index on dest, none on src", []string{
			`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
			`INSERT INTO t0(c1) VALUES(0)`,
			`CREATE TABLE t1(x AS (typeof(y)), y)`,
			`CREATE INDEX i1 ON t1(y)`,
			`INSERT INTO t1 SELECT * FROM t0`,
		}},
		{"CHECK on dest", []string{
			`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
			`INSERT INTO t0(c1) VALUES(0)`,
			`CREATE TABLE t1(x AS (typeof(y)), y, CHECK(y >= 0))`,
			`INSERT INTO t1 SELECT * FROM t0`,
		}},
		{"declared DEFAULT on dest's non-generated column", []string{
			`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
			`INSERT INTO t0(c1) VALUES(0)`,
			`CREATE TABLE t1(x AS (typeof(y)), y DEFAULT 5)`,
			`INSERT INTO t1 SELECT * FROM t0`,
		}},
		{"mismatched generated expression", []string{
			`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
			`INSERT INTO t0(c1) VALUES(0)`,
			`CREATE TABLE t1(x AS (typeof(y) || '!'), y)`,
			`INSERT INTO t1 SELECT * FROM t0`,
		}},
		{"both WITHOUT ROWID", []string{
			`CREATE TABLE t0(k INTEGER PRIMARY KEY, c0 AS(TYPEOF(c1)), c1) WITHOUT ROWID`,
			`INSERT INTO t0(k,c1) VALUES(1,0)`,
			`CREATE TABLE t1(k INTEGER PRIMARY KEY, x AS (typeof(y)), y) WITHOUT ROWID`,
			`INSERT INTO t1 SELECT * FROM t0`,
		}},
	}
	for _, c := range cases {
		differ(t, "gencol1-xfer boundary: "+c.name, c.stmts)
	}
}

// TestGencol1XferStrictBothGap documents a DELIBERATE, ACKNOWLEDGED coverage
// gap rather than a wrong answer: C SQLite's xferOptimization allows a
// STRICT dest fed by an equally STRICT src (insert.c:3113-3115's asymmetry
// rule -- only strict-dest-from-non-strict-src is forbidden), but this
// engine's eligibility check narrows that to "dest is never STRICT" at all
// (see xferOptimizationEligible's own doc comment for why: costs coverage,
// never correctness). Verified directly against 3.53.3 that the oracle DOES
// succeed here ("integer|0", the identical answer as the mined statement) --
// so this is intentionally left as a clean decline (differAllowingDeclines
// tolerates musql erroring where the oracle answered rows), not something
// this fix's scope claims to close.
//
// No trailing SELECT here: once the tolerated INSERT itself has diverged
// (declined on musql, applied on the oracle), t1's own CONTENT has already
// diverged between the two engines, so any later read of it would report a
// real, un-tolerated mismatch of its own -- not a further instance of the
// SAME acknowledged gap.
func TestGencol1XferStrictBothGap(t *testing.T) {
	differAllowingDeclines(t, "gencol1-xfer both-STRICT (declined gap, not wrong)", []string{
		`CREATE TABLE t0(c0 TEXT AS(TYPEOF(c1)), c1 INTEGER) STRICT`,
		`INSERT INTO t0(c1) VALUES(0)`,
		`CREATE TABLE t1(x TEXT AS (typeof(y)), y INTEGER) STRICT`,
		`INSERT INTO t1 SELECT * FROM t0`,
	})
}

// TestGencol1XferExplicitOrDeclines pins a regression this fix closes:
// xferOptimizationEligible originally never inspected the calling
// statement's own orAction/explicitOr at all. insert.c:3247-3249, condition
// (3) of xferOptimization's own destination-empty-required test ("onError is
// something other than OE_Abort and OE_Rollback"), forces the routine into a
// RUNTIME-guarded mode whenever an explicit OR IGNORE/REPLACE/FAIL is given:
// insert.c's own top-of-function comment documents that in that mode the
// function's return value is FALSE regardless of the runtime outcome
// (insert.c:3388), so insert.c:1030-1038's caller ALSO compiles the ordinary
// (non-xfer) path -- and for a dest table with a generated column, COMPILING
// that ordinary path is exactly where the arity mismatch is raised as a hard
// PARSE-TIME error (insert.c:1244-1253), before any row is read, regardless
// of whether dest is actually empty at runtime.
//
// Verified directly against 3.53.3 with these exact tables: t1 is freshly
// created (so genuinely empty when the INSERT runs) -- if xferOptimization's
// runtime-empty guard were what mattered, an empty dest would still let the
// xfer path succeed. It does not: the oracle answers the identical
// PARSE-TIME arity error for OR IGNORE, OR REPLACE, and OR FAIL alike,
// proving this is unconditional at compile time, not merely a missed
// optimization on a non-empty table.
func TestGencol1XferExplicitOrDeclines(t *testing.T) {
	cases := []struct {
		name   string
		action string
	}{
		{"OR IGNORE", "OR IGNORE"},
		{"OR REPLACE", "OR REPLACE"},
		{"OR FAIL", "OR FAIL"},
	}
	for _, c := range cases {
		differ(t, "gencol1-xfer explicit "+c.name+" disqualifies (arity error, not xfer)", []string{
			`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
			`INSERT INTO t0(c1) VALUES(0)`,
			`CREATE TABLE t1(x AS (typeof(y)), y)`,
			`INSERT ` + c.action + ` INTO t1 SELECT * FROM t0`,
		})
	}
}

// TestGencol1XferOrAbortRollbackEligible is TestGencol1XferExplicitOrDeclines'
// positive counterpart: insert.c:3247-3249's condition (3) excludes BOTH
// OE_Abort and OE_Rollback from disqualification (only OE_Ignore/OE_Replace/
// OE_Fail force the runtime-guarded fallback) -- so an explicit "OR ABORT" or
// "OR ROLLBACK" must stay xfer-eligible exactly like no OR clause at all.
// Verified directly against 3.53.3: both answer "integer|0", the identical
// success the mined statement (no OR clause) produces. A coarser fix that
// disqualified on "any explicit OR clause" rather than reading condition
// (3)'s own two exceptions would have wrongly declined these.
func TestGencol1XferOrAbortRollbackEligible(t *testing.T) {
	cases := []string{"OR ABORT", "OR ROLLBACK"}
	for _, action := range cases {
		differ(t, "gencol1-xfer explicit "+action+" stays eligible", []string{
			`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
			`INSERT INTO t0(c1) VALUES(0)`,
			`CREATE TABLE t1(x AS (typeof(y)), y)`,
			`INSERT ` + action + ` INTO t1 SELECT * FROM t0`,
			`SELECT * FROM t1`,
		})
	}
}

// TestGencol1XferExplicitOrTriggerBodyPath is
// TestGencol1XferExplicitOrDeclines' trigger-body counterpart (see
// TestGencol1XferTriggerBodyPath's own doc comment for what a trigger body's
// own INSERT...SELECT has to do differently): the two spellings once reached
// two insert implementations that each needed the same orAction/explicitOr
// gate wired in, and this pins the one adversarial review actually caught (a
// bare INSERT OR IGNORE, no trigger body, reaches compileInsertSelectWrite
// directly since t1 itself has no INSERT trigger of its own -- confirming the
// trigger-body spelling independently is what kept the two from silently
// drifting apart).
func TestGencol1XferExplicitOrTriggerBodyPath(t *testing.T) {
	differ(t, "gencol1-xfer explicit OR IGNORE through a trigger body", []string{
		`CREATE TABLE t0(c0 AS(TYPEOF(c1)), c1)`,
		`INSERT INTO t0(c1) VALUES(0)`,
		`CREATE TABLE t1(x AS (typeof(y)), y)`,
		`CREATE TABLE trg_log(msg TEXT)`,
		`CREATE TRIGGER fire_it AFTER INSERT ON trg_log BEGIN INSERT OR IGNORE INTO t1 SELECT * FROM t0; END`,
		`INSERT INTO trg_log VALUES('go')`,
		`SELECT * FROM t1`,
	})
}

// TestGencol1XferDeclaredIpkConflictDeclines pins the SAME condition-(3) gate
// reached a different way: insert.c:3046-3049 resolves an unspecified onError
// (OE_Default -- no OR clause on the statement itself) to pDest->keyConf, the
// destination's own declared "INTEGER PRIMARY KEY ... ON CONFLICT <x>", NOT
// automatically OE_Abort. So a dest table that declares its IPK's own
// non-Abort/non-Rollback conflict action is disqualified by condition (3)
// even with NO explicit OR-clause on the INSERT itself -- xferOptimizationEligible
// must resolve stmt.orAction the same way C SQLite resolves OE_Default,
// not just check stmt.orAction/explicitOr in isolation. Verified directly
// against 3.53.3: replacing the mined statement's plain "y" column with "k
// INTEGER PRIMARY KEY ON CONFLICT IGNORE" and dropping the OR-clause still
// answers the identical parse-time arity error.
func TestGencol1XferDeclaredIpkConflictDeclines(t *testing.T) {
	differ(t, "gencol1-xfer dest's own declared IPK ON CONFLICT disqualifies (no explicit OR clause)", []string{
		`CREATE TABLE t0(k INTEGER PRIMARY KEY, c0 AS(TYPEOF(c1)), c1)`,
		`INSERT INTO t0(k,c1) VALUES(1,0)`,
		`CREATE TABLE t1(k INTEGER PRIMARY KEY ON CONFLICT IGNORE, x AS (typeof(y)), y)`,
		`INSERT INTO t1 SELECT * FROM t0`,
		`SELECT * FROM t1`,
	})
}
