// Gate for WITH-prefixed DML with foreign key constraints.
package compat

import (
	"strings"
	"testing"
)

// mismatchSchema is the smallest schema whose every DML statement must fail to
// prepare: c1's foreign key names a column of nokey that does not exist, which
// sqlite3FkLocateIndex reports as "foreign key mismatch" (fkey.c:283) while
// GENERATING code -- so it fires for a statement that matches no rows, and for
// a DELETE from an empty table.
var mismatchSchema = []string{
	`PRAGMA foreign_keys=ON`,
	`CREATE TABLE nokey(z)`,
	`CREATE TABLE c1(y REFERENCES nokey(z))`,
}

// TestWithPrefixedDMLForeignKeyMismatch pairs each PLAIN spelling with a
// WITH-prefixed one that must behave identically -- same acceptance, and when
// rejected, the same error text as both the plain spelling and the oracle.
func TestWithPrefixedDMLForeignKeyMismatch(t *testing.T) {
	for _, tc := range []struct{ name, plain, with string }{
		{"delete", `DELETE FROM c1`,
			`WITH x AS (SELECT 1) DELETE FROM c1`},
		{"update", `UPDATE c1 SET y=2`,
			`WITH x AS (SELECT 1) UPDATE c1 SET y=2`},
		{"insert-select", `INSERT INTO c1 SELECT 1`,
			`WITH x AS (SELECT 1) INSERT INTO c1 SELECT 1`},
		{"insert-values", `INSERT INTO c1 VALUES(1)`,
			`WITH x AS (SELECT 1) INSERT INTO c1 VALUES(1)`},
		// RECURSIVE, whose extra keyword shifts the verb one token further.
		{"recursive-delete", `DELETE FROM c1`,
			`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n<3) DELETE FROM c1`},
		// Several CTEs: the commas separating them are exactly what a
		// fixed-token skip cannot count past, and each body carries its own
		// parens.
		{"multi-cte-update", `UPDATE c1 SET y=2`,
			`WITH a AS (SELECT 1), b AS (SELECT 2), c AS (SELECT 3) UPDATE c1 SET y=2`},
		// A CTE whose BODY references the target table -- the name the walk is
		// looking for appears before the verb as well as after it.
		{"cte-body-references-target", `DELETE FROM c1`,
			`WITH a AS (SELECT y FROM c1 WHERE y IN (SELECT y FROM c1)) DELETE FROM c1`},
		// ...and one NAMED for the target, so a walk that stopped at the first
		// matching identifier would find the CTE instead of the table.
		{"cte-named-for-target", `DELETE FROM c1`,
			`WITH c1(y) AS (VALUES(1)) DELETE FROM c1`},
		// The schema qualifier is read relative to the verb too
		// (fkDMLTargetName's "<db>." half).
		{"schema-qualified-delete", `DELETE FROM main.c1`,
			`WITH a AS (SELECT 1) DELETE FROM main.c1`},
		// "OR <action>" sits between the verb and the target, and is skipped
		// relative to the VERB, not to token 0.
		{"or-action-update", `UPDATE OR ROLLBACK c1 SET y=2`,
			`WITH a AS (SELECT 1) UPDATE OR ROLLBACK c1 SET y=2`},
		{"or-action-insert", `INSERT OR REPLACE INTO c1 VALUES(1)`,
			`WITH a AS (SELECT 1) INSERT OR REPLACE INTO c1 VALUES(1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newAttachPair(t, nil, nil)
			for _, s := range mismatchSchema {
				p.agreeExec(s)
			}
			plainGo, plainCgo := p.exec(tc.plain)
			withGo, withCgo := p.exec(tc.with)
			// The oracle is the arbiter: it must reject BOTH spellings, which
			// is what makes this a test of the engine and not of the schema.
			if plainCgo == nil || withCgo == nil {
				t.Fatalf("oracle accepted an unresolvable foreign key: plain=%v with=%v", plainCgo, withCgo)
			}
			if withGo == nil {
				t.Fatalf("%q: engine ACCEPTED, C SQLite rejected: %v", tc.with, withCgo)
			}
			if plainGo == nil {
				t.Fatalf("%q: engine ACCEPTED, C SQLite rejected: %v", tc.plain, plainCgo)
			}
			// The SAME error, not merely an error: a WITH clause must not
			// change which failure the statement reports.
			if got, want := trimEnginePrefix(withGo), trimEnginePrefix(plainGo); got != want {
				t.Errorf("%q: engine error %q, plain spelling says %q", tc.with, got, want)
			}
			if got, want := trimEnginePrefix(withGo), withCgo.Error(); got != want {
				t.Errorf("%q: engine error %q, C SQLite says %q", tc.with, got, want)
			}
		})
	}
}

// trimEnginePrefix strips the engine's own "engine: " prefix so an error can be
// compared to the oracle's text.
func trimEnginePrefix(err error) string {
	return strings.TrimPrefix(err.Error(), "engine: ")
}

// TestWithPrefixedDMLReach gates the OTHER direction: a WITH clause must not
// make the engine invent an error the oracle does not raise.
//
// fkStatementReach decides how far fkRequireResolvable walks, and for an INSERT
// it decides that by SCANNING the statement's tokens for "OR REPLACE" and
// "DO UPDATE". Scanning from token 1 rather than from the verb puts a leading
// WITH clause's own bodies inside the scan, where an ordinary "a OR
// replace(...)" reads as "INSERT OR REPLACE" and widens the walk -- turning an
// INSERT the oracle accepts into a spurious "foreign key mismatch".
//
// The schema is fkey2.test's 20150416-100 (see vdbe_foreign_keys_test.go's
// action-reach-decides-which-mismatch-fires): t's foreign key is a mismatch,
// and a statement on t1 sees it only if it generates ON DELETE action code.
func TestWithPrefixedDMLReach(t *testing.T) {
	reachSchema := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE t1(x PRIMARY KEY)`,
		`CREATE TABLE t(y REFERENCES t0(x) ON DELETE SET DEFAULT)`,
		`CREATE TABLE t0(y REFERENCES t1 ON DELETE SET NULL)`,
	}
	for _, stmt := range []string{
		// A plain INSERT generates no action code: accepted, mismatch and all.
		`INSERT INTO t1 VALUES(9)`,
		// The same, behind a WITH clause whose body contains an "OR
		// replace(...)" pair. Still an ordinary INSERT.
		`WITH q AS (SELECT 1 WHERE 0 OR replace('a','a','b')='b') INSERT INTO t1 SELECT 10`,
		// ...and one whose body contains "DO UPDATE"-shaped tokens.
		`WITH q(v) AS (SELECT 1 WHERE 'x' IS NOT NULL) INSERT INTO t1 SELECT 11`,
		// The REAL conflict clause still widens the walk, WITH or not: both
		// engines must reject these.
		`INSERT OR REPLACE INTO t1 VALUES(12)`,
		`WITH q AS (SELECT 1) INSERT OR REPLACE INTO t1 SELECT 13`,
		// And a DELETE always reaches ON DELETE: both reject.
		`DELETE FROM t1`,
		`WITH q AS (SELECT 1) DELETE FROM t1`,
	} {
		t.Run(stmt, func(t *testing.T) {
			p := newAttachPair(t, nil, nil)
			for _, s := range reachSchema {
				p.agreeExec(s)
			}
			p.agreeExec(stmt)
			p.agreeQuery(`SELECT x FROM t1 ORDER BY x`)
		})
	}
}

// TestWithPrefixedDMLStillWrites is the third direction: closing the foreign
// key gap must not turn a LEGAL WITH-prefixed DML into an error, and the
// statement must still do its work. A gate that only ever asserts errors would
// pass on an engine that rejected every WITH-prefixed write.
func TestWithPrefixedDMLStillWrites(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec(`PRAGMA foreign_keys=ON`)
	p.agreeExec(`CREATE TABLE parent(id INTEGER PRIMARY KEY)`)
	p.agreeExec(`CREATE TABLE child(y REFERENCES parent(id))`)
	p.agreeExec(`INSERT INTO parent VALUES(1),(2),(3)`)
	p.agreeExec(`WITH c(v) AS (VALUES(1),(2)) INSERT INTO child SELECT v FROM c`)
	p.agreeQuery(`SELECT y FROM child ORDER BY y`)
	p.agreeExec(`WITH c(v) AS (VALUES(2)) UPDATE child SET y=3 WHERE y IN (SELECT v FROM c)`)
	p.agreeQuery(`SELECT y FROM child ORDER BY y`)
	p.agreeExec(`WITH c(v) AS (VALUES(1)) DELETE FROM child WHERE y IN (SELECT v FROM c)`)
	p.agreeQuery(`SELECT y FROM child ORDER BY y`)
	// A real violation must still be rejected, WITH clause and all.
	p.agreeExec(`WITH c(v) AS (VALUES(99)) INSERT INTO child SELECT v FROM c`)
	p.agreeQuery(`SELECT y FROM child ORDER BY y`)
}
