package compat

// This file tests rowid/oid/_rowid_ pseudo-column naming in nested parenthesized joins.

import "testing"

// TestR42NestedRowidNamingCloses tests nested join rowid naming.
func TestR42NestedRowidNamingCloses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{
			"joinH-7.1",
			[]string{
				`CREATE TABLE t1(a, b)`,
				`CREATE TABLE t2(c)`,
				`CREATE TABLE t3(d)`,
				`INSERT INTO t1 VALUES ('a','a')`,
				`INSERT INTO t2 VALUES('ddd')`,
				`INSERT INTO t3 VALUES(1234)`,
				`SELECT t2.rowid FROM t1 JOIN (t2 JOIN t3)`,
			},
		},
		{
			"joinH-8.1",
			[]string{
				`CREATE TABLE x1(a INTEGER PRIMARY KEY, b)`,
				`CREATE TABLE x2(c, d)`,
				`CREATE TABLE x3(rowid, _rowid_)`,
				`INSERT INTO x1 VALUES(1000, 'thousand')`,
				`INSERT INTO x2 VALUES('c', 'd')`,
				`INSERT INTO x3(oid, rowid, _rowid_) VALUES(43, 'hello', 'world')`,
				`SELECT x3.oid FROM x1 JOIN (x2 JOIN x3 ON c='c')`,
			},
		},
		// joinH-8.2: same schema and group, x3's OTHER declared reserved
		// name. x3(rowid, _rowid_) makes "x3.rowid" resolve as an ORDINARY
		// declared column (x3 really has one named that) rather than the
		// pseudo-column path at all -- this doesn't exercise the new code
		// (it passes with or without this round's fix), but pins that an
		// ordinary column read through the same nested group is unaffected
		// by it either.
		{
			"joinH-8.2",
			[]string{
				`CREATE TABLE x1(a INTEGER PRIMARY KEY, b)`,
				`CREATE TABLE x2(c, d)`,
				`CREATE TABLE x3(rowid, _rowid_)`,
				`INSERT INTO x1 VALUES(1000, 'thousand')`,
				`INSERT INTO x2 VALUES('c', 'd')`,
				`INSERT INTO x3(oid, rowid, _rowid_) VALUES(43, 'hello', 'world')`,
				`SELECT x3.rowid FROM x1 JOIN (x2 JOIN x3 ON c='c')`,
			},
		},
		// A three-member single-level group: m1 is OUTSIDE it (an ordinary
		// leading table, whose own rowid naming was already correct before
		// this round), m2 and m3 are both INSIDE it and unaliased-first vs.
		// unaliased-second -- checks the fix over a group wider than two and
		// composes with an ordinary declared-column read in the same
		// statement.
		{
			"three-member-group-plus-value-use",
			[]string{
				`CREATE TABLE m1(a)`,
				`CREATE TABLE m2(b)`,
				`CREATE TABLE m3(c)`,
				`INSERT INTO m1 VALUES(1)`,
				`INSERT INTO m2 VALUES(2)`,
				`INSERT INTO m3 VALUES(3)`,
				`SELECT m1.rowid, m2.rowid, m3.c FROM m1 JOIN (m2 JOIN m3)`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { differ(t, "r42/"+tc.name, tc.stmts) })
	}
}

// TestR42NestedRowidNamingDeclines pins the shapes annotateNestedRowidNames
// deliberately leaves alone -- verifying each one is a SAFE decline (the
// engine errors) rather than either a wrong answer or an accidental match,
// by checking against the oracle's OWN actual behavior first, exactly like
// r26Pair's other consumers.
func TestR42NestedRowidNamingDeclines(t *testing.T) {
	cmp := r26Pair(t, []string{
		`CREATE TABLE q1(a)`,
		`CREATE TABLE q2(b)`,
		`CREATE TABLE q3(c)`,
		`CREATE TABLE q4(d)`,
	})
	for _, tc := range []struct {
		q          string
		oracleErrs bool
	}{
		// Two wrap layers: C SQLite does not resolve this AT ALL.
		{`SELECT q3.rowid FROM q1 JOIN (q2 JOIN (q3 JOIN q4))`, true},
		{`SELECT q4.rowid FROM q1 JOIN (q2 JOIN (q3 JOIN q4))`, true},
		// Leading-splice: q2/q3/q4 all sit in ONE flat group (one wrap
		// layer each), but q3/q4's own candidate collides with q2's --
		// C SQLite ":N"-suffixes them ("_ROWID_:1", "_ROWID_:2").
		{`SELECT q3.rowid FROM q1 JOIN ((q2 JOIN q3) JOIN q4)`, false},
		{`SELECT q4.rowid FROM q1 JOIN ((q2 JOIN q3) JOIN q4)`, false},
	} {
		declined, _, oracleErr, detail := cmp(tc.q)
		if (oracleErr != nil) != tc.oracleErrs {
			t.Errorf("[%s] the ORACLE changed: oracleErrs=%v, want %v (err=%v) -- re-measure before trusting the decline",
				tc.q, oracleErr != nil, tc.oracleErrs, oracleErr)
			continue
		}
		if !declined {
			t.Errorf("[%s] the engine ANSWERED it; this shape needs a SQLite \":N\"-style suffix (or, for a 2-layer nesting, isn't resolvable at all) that this engine does not attempt to reproduce (%s)", tc.q, detail)
		}
	}
}

// TestR42NestedRowidNamingLeadingAliasedUnaffected pins that a LEADING but
// ALIASED group's rowid reference -- which FromItem.NestFromWrapDepth ALSO
// marks (it fires on "hasAlias || !leading", not the narrower
// FromItem.NestedNonLeading's plain "!leading") -- is untouched by this
// round: it already declined, via a wholly separate, earlier
// reference-resolution path (not columnRefNameParts' at all), before this
// round existed, and still does. Verified directly against mattn/go-sqlite3
// 3.53.3 that the oracle DOES answer it ("_ROWID_:1" for the second member,
// colliding with the first's own "_ROWID_") -- so this is a real, intentional
// (if unmeasured-in-the-corpus) over-decline, pinned here so a future change
// to the upstream resolution path doesn't silently start serving it wrong.
func TestR42NestedRowidNamingLeadingAliasedUnaffected(t *testing.T) {
	cmp := r26Pair(t, []string{
		`CREATE TABLE r1(x)`,
		`CREATE TABLE r2(y)`,
	})
	for _, tc := range []struct {
		q          string
		oracleErrs bool
	}{
		{`SELECT r1.rowid FROM (r1 JOIN r2) AS g`, false},
		{`SELECT r2.rowid FROM (r1 JOIN r2) AS g`, false},
	} {
		declined, _, oracleErr, _ := cmp(tc.q)
		if (oracleErr != nil) != tc.oracleErrs {
			t.Errorf("[%s] the ORACLE changed: oracleErrs=%v, want %v -- re-measure", tc.q, oracleErr != nil, tc.oracleErrs)
			continue
		}
		if !declined {
			t.Errorf("[%s] the engine now ANSWERS a leading-but-aliased group's rowid reference -- if that's from a NEW, deliberate fix, this pin needs updating with the verified name; if not, something upstream of columnRefNameParts changed unexpectedly", tc.q)
		}
	}
}
