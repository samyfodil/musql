// This file gates two rules that were overly broad declines:
// DROP TABLE on a foreign key parent with DELETE triggers, and GROUP BY with
// non-BINARY collations.
package compat

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestDropFKParentWithDeleteTrigger verifies that DROP TABLE with DELETE
// triggers on foreign key parents is accepted.
func TestDropFKParentWithDeleteTrigger(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("PRAGMA foreign_keys = ON")

	// e_fkey-57.1's fixture, verbatim in shape.
	p.agreeExec("CREATE TABLE p(a, b, PRIMARY KEY(a, b))")
	p.agreeExec("CREATE TABLE c1(c, d, FOREIGN KEY(c, d) REFERENCES p ON DELETE SET NULL)")
	p.agreeExec("CREATE TABLE c2(c, d DEFAULT 'dflt', FOREIGN KEY(c, d) REFERENCES p ON DELETE SET DEFAULT)")
	p.agreeExec("CREATE TABLE c3(c, d, FOREIGN KEY(c, d) REFERENCES p ON DELETE CASCADE)")
	p.agreeExec("CREATE TABLE log(msg)")
	p.agreeExec("CREATE TRIGGER tt AFTER DELETE ON p BEGIN INSERT INTO log VALUES('delete ' || old.rowid); END")

	p.agreeExec("INSERT INTO p VALUES('a', 'b')")
	p.agreeExec("INSERT INTO c1 VALUES('a', 'b')")
	p.agreeExec("INSERT INTO c2 VALUES('a', 'b')")
	p.agreeExec("INSERT INTO c3 VALUES('a', 'b')")

	// The DROP itself must be ACCEPTED, not declined: the trigger compiles.
	p.agreeExec("DROP TABLE p")
	p.agreeQuery("SELECT c, d FROM c1")     // SET NULL ran
	p.agreeQuery("SELECT c, d FROM c2")     // SET DEFAULT ran
	p.agreeQuery("SELECT count(*) FROM c3") // CASCADE ran
	p.agreeQuery("SELECT count(*) FROM log")

	// A BEFORE DELETE trigger is the same: accepted, silent.
	p.agreeExec("CREATE TABLE p2(x PRIMARY KEY)")
	p.agreeExec("CREATE TABLE k2(y REFERENCES p2 ON DELETE CASCADE)")
	p.agreeExec("CREATE TRIGGER tt2 BEFORE DELETE ON p2 BEGIN INSERT INTO log VALUES('b' || old.x); END")
	p.agreeExec("INSERT INTO p2 VALUES(7)")
	p.agreeExec("INSERT INTO k2 VALUES(7)")
	p.agreeExec("DROP TABLE p2")
	p.agreeQuery("SELECT count(*) FROM k2")
	p.agreeQuery("SELECT count(*) FROM log")

	// ...and an ordinary DELETE still fires it, so accepting the DROP has not
	// disabled the trigger itself.
	p.agreeExec("CREATE TABLE p3(x PRIMARY KEY)")
	p.agreeExec("CREATE TABLE k3(y REFERENCES p3 ON DELETE CASCADE)")
	p.agreeExec("CREATE TRIGGER tt3 AFTER DELETE ON p3 BEGIN INSERT INTO log VALUES('d' || old.x); END")
	p.agreeExec("INSERT INTO p3 VALUES(9)")
	p.agreeExec("INSERT INTO k3 VALUES(9)")
	p.agreeExec("DELETE FROM p3")
	p.agreeQuery("SELECT msg FROM log ORDER BY 1")
}

// TestDropFKParentStillDeclinesABrokenDeleteTrigger verifies that DROP TABLE
// still declines on foreign key parents with broken DELETE triggers.
func TestDropFKParentStillDeclinesABrokenDeleteTrigger(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("PRAGMA foreign_keys = ON")
	p.agreeExec("CREATE TABLE log(msg)")
	p.agreeExec("CREATE TABLE q(x PRIMARY KEY)")
	p.agreeExec("CREATE TRIGGER qt AFTER DELETE ON q BEGIN INSERT INTO log VALUES(old.nope); END")
	p.agreeExec("CREATE TABLE qc(y REFERENCES q ON DELETE CASCADE)")

	// The trigger DOES fail an ordinary delete, on both sides -- that is what
	// "does not compile" means here, and it is checked so the fixture cannot
	// rot into a healthy trigger without this test noticing.
	goErr, cgoErr := p.exec("DELETE FROM q")
	if goErr == nil || cgoErr == nil {
		t.Fatalf("fixture no longer has a broken trigger: DELETE FROM q go=%v cgo=%v", goErr, cgoErr)
	}

	// ...and the DROP is where the two part company: the engine declines, real
	// SQLite goes ahead.
	goErr, cgoErr = p.exec("DROP TABLE q")
	if goErr == nil {
		t.Errorf("engine ACCEPTED DROP TABLE q -- if this is deliberate, re-check " +
			"without_rowid3.test and fkey2.test with a full sweep and delete " +
			"fkBeforeDropTable's trigger check")
	}
	if cgoErr != nil {
		t.Errorf("C SQLite rejected DROP TABLE q (%v) -- the premise this decline "+
			"was built on may be reproducible after all; re-derive it", cgoErr)
	}
}

// TestGroupByNonBinaryCollation gates the GROUP BY collation rule above, in all
// three places the collation has to reach: the grouping EQUALITY, the KEY VALUE
// each group reports, and the ORDER the groups come out in.
//
// Verified directly against mattn/go-sqlite3 3.53.3 over
// t2(a COLLATE NOCASE, b COLLATE BINARY) = ('aBc','DeF'),('ABC','def'),('abc','DEF'):
//
//	SELECT count(*), a FROM t2 GROUP BY a                 -> ONE group, (3,aBc)
//	SELECT count(*), b FROM t2 GROUP BY b                 -> three groups
//	SELECT count(*), a FROM t2 GROUP BY a COLLATE binary  -> three groups
//	SELECT count(*), b FROM t2 GROUP BY b COLLATE nocase  -> ONE group, (3,DeF)
//
// and over t3(x COLLATE NOCASE) = 'b','A','a','B':
//
//	SELECT x, count(*) FROM t3 GROUP BY x  ->  (A,2) then (b,2)
//
// -- the reported value is the group's FIRST row in scan order, and the groups
// come out in COLLATED key order (LIMIT below is what makes that observable).
func TestGroupByNonBinaryCollation(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("CREATE TABLE t2(a COLLATE NOCASE, b COLLATE BINARY)")
	p.agreeExec("INSERT INTO t2 VALUES('aBc','DeF'),('ABC','def'),('abc','DEF')")
	p.agreeQuery("SELECT count(*), a FROM t2 GROUP BY a")
	p.agreeQuery("SELECT count(*), b FROM t2 GROUP BY b")
	p.agreeQuery("SELECT count(*), a FROM t2 GROUP BY a COLLATE binary")
	p.agreeQuery("SELECT count(*), b FROM t2 GROUP BY b COLLATE nocase")
	p.agreeQuery("SELECT group_concat(b), a FROM t2 GROUP BY a")

	p.agreeExec("CREATE TABLE t3(x COLLATE NOCASE)")
	p.agreeExec("INSERT INTO t3 VALUES('b'),('A'),('a'),('B'),(NULL),(1),(1.0),('  z '),('Z')")
	p.agreeQuery("SELECT x, count(*) FROM t3 GROUP BY x")
	p.agreeQuery("SELECT x||'!', count(*) FROM t3 GROUP BY x")
	// Group ORDER, made observable by LIMIT/OFFSET.
	p.agreeQuery("SELECT x, count(*) FROM t3 GROUP BY x LIMIT 2")
	p.agreeQuery("SELECT x, count(*) FROM t3 GROUP BY x LIMIT 2 OFFSET 2")

	// RTRIM groups too, and a trailing TAB is NOT trimmed.
	p.agreeExec("CREATE TABLE t5(x COLLATE RTRIM)")
	p.agreeExec("INSERT INTO t5 VALUES('z'),('z '),('z  '),(' z'),('z' || char(9)),(NULL)")
	p.agreeQuery("SELECT x, count(*) FROM t5 GROUP BY x")

	// Two keys with different collations.
	p.agreeExec("CREATE TABLE t6(p COLLATE NOCASE, q)")
	p.agreeExec("INSERT INTO t6 VALUES('A','x'),('a','X'),('a','x'),('B','y')")
	p.agreeQuery("SELECT p, q, count(*) FROM t6 GROUP BY p, q")
	p.agreeQuery("SELECT p, count(*) FROM t6 GROUP BY p HAVING count(*) > 1")
}

// TestGroupByKeyCollationInComparison verifies that GROUP BY key collations
// are preserved in comparisons.
func TestGroupByKeyCollationInComparison(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("CREATE TABLE t2(a COLLATE NOCASE, b COLLATE BINARY)")
	p.agreeExec("INSERT INTO t2 VALUES('aBc','DeF')")
	p.agreeQuery("SELECT a='abc' FROM t2 GROUP BY a")
	p.agreeQuery("SELECT a>b FROM t2 GROUP BY a, b")
	p.agreeQuery("SELECT a>b COLLATE BINARY FROM t2 GROUP BY a, b")
	p.agreeQuery("SELECT b>a FROM t2 GROUP BY a, b")
	p.agreeQuery("SELECT b>a COLLATE NOCASE FROM t2 GROUP BY a, b")

	p.agreeExec("CREATE TABLE t(c0 COLLATE NOCASE, c1 COLLATE RTRIM)")
	p.agreeExec("INSERT INTO t VALUES('a','b '),('A','B'),('aB','ab')")
	p.agreeQuery("SELECT c1>c0, count(*) FROM t GROUP BY c0")
	p.agreeQuery("SELECT c0>c1, count(*) FROM t GROUP BY c0")
	p.agreeQuery("SELECT c1>c0, count(*) FROM t GROUP BY c0 COLLATE binary")
	p.agreeQuery("SELECT c1, count(*) FROM t GROUP BY c0")
}

// TestGroupByNonBinaryCollationAnchorAndDistinct verifies that min()/max()
// aggregate functions follow the correct anchor row with non-BINARY collations.
func TestGroupByNonBinaryCollationAnchorAndDistinct(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("CREATE TABLE m(k COLLATE NOCASE, v)")
	p.agreeExec("INSERT INTO m VALUES('A',1),('a',2),('B',3),('b',4)")
	// SERVED now, and byte-identical to the oracle: these four are exactly the
	// min()/max() magnet shapes, and the key reference under a magnet now
	// follows the ANCHOR ROW as select.c does -- a bare GROUP BY key reference
	// is TK_AGG_COLUMN, so it reads AggInfoColumnReg, which updateAccumulator's
	// trailing loop reloads under the magnet (rewriteGroupExpr,
	// engine/sql_group.go). The decline that stood here assumed the key follows
	// the key TUPLE, which is what the port changed.
	p.agreeQuery("SELECT k, max(v) FROM m GROUP BY k")
	p.agreeQuery("SELECT k, min(v) FROM m GROUP BY k")
	p.agreeQuery("SELECT k, max(NULL) FROM m GROUP BY k")
	p.agreeQuery("SELECT k FROM m GROUP BY k HAVING max(v) > 0")

	// The trailing ORDER BY is SERVED now too. It was declined because the
	// group ORDER's own DESC tie-break was wrong -- a tied DESC sort came out
	// reversed from the oracle's -- and a non-BINARY key MANUFACTURES ties for
	// that bug to get wrong. The tie-break is fixed (see
	// TestGroupBySortOrderFollowsOrderBy: the ORDER BY sort's tiebreak is the
	// group EMISSION ordinal, and the GROUP BY sort takes the ORDER BY's DESC
	// bits via sqlite3CopySortOrder), so the shape answers.
	p.agreeQuery("SELECT k, count(*) FROM m GROUP BY k ORDER BY k")
	p.agreeQuery("SELECT k, count(*) FROM m GROUP BY k ORDER BY 2")
	p.agreeQuery("SELECT k, count(*) FROM m GROUP BY k ORDER BY k DESC")
	p.agreeQuery("SELECT k, count(*) FROM m GROUP BY k ORDER BY 2 DESC")
	p.agreeQuery("SELECT k, max(v) FROM m GROUP BY k ORDER BY k COLLATE nocase DESC")

	// SELECT DISTINCT over a non-BINARY GROUP BY key is SERVED now, and this
	// was the last decline this test pinned. It stood on the belief that the
	// cross-group dedup would have to honour the GROUP BY KEY's collation --
	// but sqlite3Select builds the distinct index's KeyInfo from the RESULT
	// expression list (sqlite3KeyInfoFromExprList(pParse, pEList, 0, 0)), so
	// the key's collation never reaches that dedup and the result column's
	// always does. See TestDistinctGroupByCollation, which pins both halves.
	p.agreeQuery("SELECT DISTINCT k FROM m GROUP BY k")
	p.agreeQuery("SELECT DISTINCT k COLLATE BINARY FROM m GROUP BY k")
	p.agreeQuery("SELECT DISTINCT v FROM m GROUP BY k")

	// ...and the same statements WITHOUT a non-BINARY key still answer, which
	// is what shows the two collations are being told apart rather than both
	// being ignored.
	p.agreeExec("CREATE TABLE mb(k, v)")
	p.agreeExec("INSERT INTO mb VALUES('A',1),('a',2),('B',3),('b',4)")
	p.agreeQuery("SELECT DISTINCT k FROM mb GROUP BY k")
	p.agreeQuery("SELECT k, count(*) FROM mb GROUP BY k ORDER BY k")
	p.agreeQuery("SELECT k, max(v) FROM mb GROUP BY k")
	p.agreeQuery("SELECT k FROM mb GROUP BY k HAVING max(v) > 0")
}

// TestGroupByCollationFuzz is the sweep that FOUND both of the above, and the
// reason they are two rules rather than one: random tables whose columns
// declare BINARY/NOCASE/RTRIM, random rows drawn from a literal pool chosen for
// the boundaries that separate the three collations (case pairs, trailing
// spaces, ']' -- which sits between 'B' and 'b' in ASCII, so it is where a
// fold-to-uppercase NOCASE would diverge -- plus numbers, blobs and NULL), and
// random GROUP BY queries over them. A statement the engine DECLINES is skipped
// (a decline is never a wrong answer); everything both engines answer must
// match cell for cell.
//
// It is what showed that honoring the group-key collation ALONE is not enough:
// with the placeholders still carrying no collation of their own, a comparison
// beside the key fell through to the wrong operand's collating sequence.
func TestGroupByCollationFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	colls := []string{"", " COLLATE NOCASE", " COLLATE RTRIM", " COLLATE BINARY"}
	lits := []string{"'a'", "'A'", "'b '", "'B'", "' b'", "''", "NULL", "1", "1.0", "'1'",
		"x'4142'", "'ab'", "'AB'", "'a]b'", "'A]B'", "'z  '", "'Z'", "-1", "'aB'"}

	compared, declined := 0, 0
	for iter := 0; iter < 60; iter++ {
		p := newAttachPair(t, nil, nil)
		nc := 2 + rng.Intn(2)
		create := "CREATE TABLE t("
		for i := 0; i < nc; i++ {
			if i > 0 {
				create += ", "
			}
			create += fmt.Sprintf("c%d%s", i, colls[rng.Intn(len(colls))])
		}
		p.agreeExec(create + ")")
		for r := 0; r < 3+rng.Intn(6); r++ {
			row := "INSERT INTO t VALUES("
			for i := 0; i < nc; i++ {
				if i > 0 {
					row += ","
				}
				row += lits[rng.Intn(len(lits))]
			}
			p.agreeExec(row + ")")
		}

		for q := 0; q < 12; q++ {
			key := fmt.Sprintf("c%d", rng.Intn(nc))
			switch rng.Intn(4) {
			case 1:
				key += " COLLATE nocase"
			case 2:
				key += " COLLATE rtrim"
			case 3:
				key += " COLLATE binary"
			}
			sel := []string{
				fmt.Sprintf("c%d, count(*)", rng.Intn(nc)),
				fmt.Sprintf("count(*), group_concat(c%d)", rng.Intn(nc)),
				fmt.Sprintf("c%d = %s, count(*)", rng.Intn(nc), lits[rng.Intn(len(lits))]),
				fmt.Sprintf("c%d > c%d, count(*)", rng.Intn(nc), rng.Intn(nc)),
				fmt.Sprintf("c%d > c%d COLLATE nocase, count(*)", rng.Intn(nc), rng.Intn(nc)),
				fmt.Sprintf("sum(c%d), c%d", rng.Intn(nc), rng.Intn(nc)),
				fmt.Sprintf("typeof(c%d), c%d", rng.Intn(nc), rng.Intn(nc)),
			}[rng.Intn(7)]
			stmt := fmt.Sprintf("SELECT %s FROM t GROUP BY %s", sel, key)
			if rng.Intn(3) == 0 {
				stmt += fmt.Sprintf(" HAVING count(*) > %d", rng.Intn(2))
			}
			if rng.Intn(4) == 0 {
				stmt += fmt.Sprintf(" LIMIT %d", 1+rng.Intn(3))
			}
			goCols, goRows, goErr, cgoCols, cgoRows, cgoErr := p.query(stmt)
			if goErr != nil {
				declined++
				continue
			}
			if cgoErr != nil {
				t.Errorf("%q: engine ACCEPTED cols=%v rows=%v, C SQLite rejected: %v", stmt, goCols, goRows, cgoErr)
				continue
			}
			compared++
			if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
				t.Errorf("%q: %s\n  engine: %v\n  cgo:    %v", stmt, reason, goRows, cgoRows)
			}
		}
	}
	t.Logf("GROUP BY collation fuzz: %d statements compared, %d declined", compared, declined)
}
