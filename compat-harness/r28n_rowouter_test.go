package compat

// This file gates enclosing row references in subqueries, verifying that
// scope resolution correctly prioritizes outer rows in COMPOUND and FROM-less
// aggregate subqueries.
//
//	(1) an arm's OWN FROM wins over the enclosing row. Structural in this
//	    engine: compileColumn resolves through resolveInScopes (this compile's
//	    scopes) and then the enclosing *compiler* chain BEFORE ever consulting
//	    c.outerRowCtx(), so wrapping the row as a one-level enclosing compiler
//	    (rowOuterCompile) cannot preempt anything. Swept below anyway, over
//	    fixtures that vary which side declares the shared name and in which
//	    position -- a 540-statement sweep in this repo once missed a wrong answer
//	    because every table in it declared the shared column first.
//
//	(2) a same-named EXPLICIT alias at an intervening level wins over the
//	    enclosing row -- lookupName's arm 4, reached before the level is left.
//	    NC_UEList is set only AFTER the select list has been resolved
//	    (resolveSelectStep, resolve.c ~1997), so a subquery in WHERE/GROUP BY/
//	    HAVING sees those aliases and one in the SELECT LIST does not. Verified
//	    against the oracle, over w(x INTEGER) holding 1:
//
//	      SELECT x,(SELECT 7 AS x WHERE (SELECT x UNION SELECT 99
//	                ORDER BY 1 LIMIT 1)=7) FROM w   -> 1|7     (alias wins)
//	      ...the same with "=1"                     -> 1|NULL
//
//	    THIS ENGINE ANSWERS BOTH THE OTHER WAY ROUND, and did so at round 27's
//	    baseline too: it is a PRE-EXISTING wrong answer, not one R1 introduced,
//	    and it does not go through any of R1's three call sites. Its cause is
//	    engine/sql_alias.go: substituteAliasesIntoSub -- the pass that implements
//	    "an alias beats an outer-scope column" for a nested FROM-less subquery --
//	    returns early on "len(sub.Compound) != 0", so the alias never reaches a
//	    compound arm and the arm's own compile then resolves the bare name
//	    outward through the ordinary compiler chain. The FROM-less-AGGREGATE
//	    spelling of the identical shape is RIGHT for exactly that reason (that
//	    pass does descend into it), which is the control below.
//	    TestR28NAliasBeatsOuterRowKnownWrong pins it self-verifyingly.

import (
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r28nOwnFromSchema deliberately varies where the shared name sits and how many
// columns each table has: sh1 declares the shared name FIRST, sh2 SECOND, sh3 in
// the MIDDLE of three, and emp is sh1's EMPTY twin. Nothing here declares a
// column the enclosing table does not also declare, which is the whole point --
// the name is ambiguous between an arm's own FROM and the enclosing row, and the
// arm's own FROM must win.
var r28nOwnFromSchema = []string{
	"CREATE TABLE outr(v INTEGER, w TEXT)",
	"INSERT INTO outr VALUES(1,'o1'),(2,'o2')",
	"CREATE TABLE sh1(v INTEGER, e TEXT)",
	"INSERT INTO sh1 VALUES(70,'a')",
	"CREATE TABLE sh2(e TEXT, v INTEGER)",
	"INSERT INTO sh2 VALUES('b',71)",
	"CREATE TABLE sh3(p TEXT, v INTEGER, q TEXT)",
	"INSERT INTO sh3 VALUES('p',72,'q')",
	"CREATE TABLE emp(v INTEGER, e TEXT)",
	// outr2 has the shared name SECOND, so the enclosing side varies too.
	"CREATE TABLE outr2(w TEXT, v INTEGER)",
	"INSERT INTO outr2 VALUES('z1',1),('z2',2)",
}

// TestR28NArmOwnFromWins is WRONG-IF (1). Every statement pairs an arm (or a
// FROM-less aggregate body) that HAS its own FROM declaring the shared name with
// an enclosing query that declares it too; the answer must be the arm's.
func TestR28NArmOwnFromWins(t *testing.T) {
	for _, q := range []string{
		// The compound route: the arm's own FROM supplies v.
		"SELECT v, (SELECT v FROM sh1 UNION SELECT 999 ORDER BY 1 LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT v FROM sh2 UNION SELECT 999 ORDER BY 1 LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT v FROM sh3 UNION SELECT 999 ORDER BY 1 LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT v FROM sh1 UNION SELECT 999 ORDER BY 1 LIMIT 1) FROM outr2 GROUP BY v ORDER BY v",
		"SELECT v, (SELECT v FROM sh2 UNION SELECT 999 ORDER BY 1 LIMIT 1) FROM outr2 GROUP BY v ORDER BY v",
		// ...and the EMPTY one: no rows means the arm contributes nothing, which
		// must not silently fall back to the enclosing row's value.
		"SELECT v, (SELECT v FROM emp UNION SELECT 999 ORDER BY 1 LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, count(*) FROM outr GROUP BY v HAVING EXISTS (SELECT v FROM sh1 INTERSECT SELECT 70) ORDER BY v",
		"SELECT v, count(*) FROM outr GROUP BY v HAVING EXISTS (SELECT v FROM sh1 INTERSECT SELECT 1) ORDER BY v",
		"SELECT v, count(*) FROM outr GROUP BY v HAVING EXISTS (SELECT v FROM emp INTERSECT SELECT 1) ORDER BY v",
		// The arm's own FROM on the SECOND arm only -- the first arm's bare v
		// still binds outward, so one statement exercises both bindings.
		"SELECT v, (SELECT v UNION SELECT v FROM sh1 ORDER BY 1 LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT v UNION SELECT v FROM sh1 ORDER BY 1 DESC LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		// The FROM-less-aggregate route has no FROM of its own by construction,
		// so the analogue is an aggregate body WITH one (which is a different
		// compiler) beside a bare correlated column: both must bind their own way.
		"SELECT v, (SELECT count(*)+v FROM sh1) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT count(*)+v FROM sh2) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT count(*)+v FROM emp) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT count(v) FROM sh1) FROM outr GROUP BY v ORDER BY v",
		// A nested-local reference inside the arm (the arm's OWN correlated
		// subquery) must keep binding to the arm's FROM, not to the outer row.
		"SELECT v, (SELECT (SELECT count(*) FROM sh2 WHERE sh2.v=v) FROM sh1 UNION SELECT 999 ORDER BY 1 LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		// A WHOLE-TABLE (no GROUP BY) enclosing query whose FROM-less aggregate
		// subquery names only its column: C SQLite RE-ASSOCIATES the average
		// outward into a one-row query, and this engine reproduces that through
		// the hoist (engine/vdbe_agg_hoist.go), not through the row threading --
		// which is why it belongs here and not in TestR28NStillDeclined.
		"SELECT (SELECT avg(v)) FROM outr",
		"SELECT (SELECT sum(v)) FROM outr",
	} {
		if !differ(t, "r28n-armfrom", append(append([]string(nil), r28nOwnFromSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestR28NArmOwnFromWritesOwnFrom is the same rule reaching a WRITE, where losing
// it is a silently mis-set column rather than a visibly wrong cell. Each script is
// run ONCE per engine through differ() -- safe because no statement in any of
// them errors in either engine, so the worker's "Query, then Exec if that
// failed" retry never runs a write twice.
func TestR28NArmOwnFromWritesOwnFrom(t *testing.T) {
	for _, tc := range [][]string{
		{"UPDATE outr SET w='HIT' WHERE EXISTS (SELECT v FROM sh1 INTERSECT SELECT 70)",
			"SELECT v,w FROM outr ORDER BY v"},
		{"UPDATE outr SET w='HIT' WHERE EXISTS (SELECT v FROM sh1 INTERSECT SELECT 1)",
			"SELECT v,w FROM outr ORDER BY v"},
		{"UPDATE outr SET w=(SELECT v FROM sh2 UNION SELECT 999 ORDER BY 1 LIMIT 1)",
			"SELECT v,w FROM outr ORDER BY v"},
		{"UPDATE outr SET w=(SELECT count(*)+v FROM sh1)",
			"SELECT v,w FROM outr ORDER BY v"},
		{"DELETE FROM outr WHERE EXISTS (SELECT v FROM sh1 INTERSECT SELECT 70)",
			"SELECT v,w FROM outr ORDER BY v"},
	} {
		if !differ(t, "r28n-armfrom-write", append(append([]string(nil), r28nOwnFromSchema...), tc...)) {
			t.Errorf("diverged on: %v", tc)
		}
	}
}

// r28nAliasSchema. The enclosing table's column name is what an intervening
// level's explicit alias collides with, so it has to be spelled the same.
var r28nAliasSchema = []string{
	"CREATE TABLE w(x INTEGER, mark TEXT)",
	"INSERT INTO w VALUES(1,'m')",
}

// TestR28NAliasBeatsOuterRow is WRONG-IF (2)'s SERVED half: every intervening-
// alias shape this engine already agrees with the oracle on. The FROM-less
// aggregate spelling is here rather than in the known-wrong list below because
// substituteAliasesIntoSub (engine/sql_alias.go) DOES descend into a FROM-less
// non-compound body, so the alias reaches the reference and wins exactly as
// lookupName's arm 4 says it must.
func TestR28NAliasBeatsOuterRow(t *testing.T) {
	for _, q := range []string{
		// The FROM-less-aggregate body: alias 7 wins, so count(*)+7 = 8.
		"SELECT x, (SELECT 7 AS x WHERE (SELECT count(*)+x)=8) FROM w",
		"SELECT x, (SELECT 7 AS x WHERE (SELECT count(*)+x)=2) FROM w",
		"SELECT x, (SELECT 7 AS x WHERE (SELECT count(*)+x)=7) FROM w",
		// A NON-colliding alias: the bare name still binds outward, which is the
		// control saying the alias rule is not simply swallowing every name.
		"SELECT x, (SELECT 7 AS q WHERE (SELECT x UNION SELECT 99 ORDER BY 1 LIMIT 1)=1) FROM w",
		"SELECT x, (SELECT 7 AS q WHERE (SELECT count(*)+x)=2) FROM w",
		// The alias written in the intervening level's SELECT LIST is NOT visible
		// (NC_UEList is set only after the select list is resolved), so the name
		// binds outward in both engines.
		"SELECT x, (SELECT (SELECT count(*)+x) FROM (SELECT 7 AS x)) FROM w",
	} {
		if !differ(t, "r28n-alias-served", append(append([]string(nil), r28nAliasSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestR28NAliasBeatsOuterRowKnownWrong pins WRONG-IF (2)'s open half: an
// intervening explicit alias whose name collides with an enclosing table's
// column, referenced from inside a COMPOUND. This engine answers the outer row's
// value where the oracle answers the alias's -- a live WRONG ANSWER, PRE-EXISTING
// (reproduced at round 27's baseline 28b5fb3, before R1 threaded anything) and
// caused by engine/sql_alias.go's substituteAliasesIntoSub bailing out on
// "len(sub.Compound) != 0". Fixing it means letting that pass descend into a
// compound whose EVERY arm is FROM-less/CTE-less, which is a change to a file
// round 28's stream N does not own.
//
// SELF-VERIFYING in both directions: the oracle's own answer is re-derived and
// asserted to be the ALIAS's, so a gate failure says which side moved -- and the
// day this engine agrees, the test says so and asks to be deleted rather than
// silently passing on a stale expectation.
func TestR28NAliasBeatsOuterRowKnownWrong(t *testing.T) {
	for _, tc := range []struct {
		q string
		// wantOracle is the oracle's answer with the ALIAS winning; wantEngine
		// is what this engine answers today, with the outer row winning.
		wantOracle, wantEngine string
	}{
		{"SELECT x, (SELECT 7 AS x WHERE (SELECT x UNION SELECT 99 ORDER BY 1 LIMIT 1)=7) FROM w",
			`"I:1","I:7"`, `"I:1","N"`},
		{"SELECT x, (SELECT 7 AS x WHERE (SELECT x UNION SELECT 99 ORDER BY 1 LIMIT 1)=1) FROM w",
			`"I:1","N"`, `"I:1","I:7"`},
	} {
		stmts := append(append([]string(nil), r28nAliasSchema...), tc.q)
		oracle, _ := json.Marshal(run(t, "cgo", stmts)[len(stmts)-1])
		got, _ := json.Marshal(run(t, "musql", stmts)[len(stmts)-1])
		if string(oracle) == string(got) {
			t.Errorf("%q now AGREES (%s) -- substituteAliasesIntoSub must have learned compounds: delete this entry and move the statement into TestR28NAliasBeatsOuterRow", tc.q, oracle)
			continue
		}
		if !strings.Contains(string(oracle), tc.wantOracle) {
			t.Errorf("%q: the ORACLE changed -- expected the alias to win (%s), got %s", tc.q, tc.wantOracle, oracle)
		}
		if !strings.Contains(string(got), tc.wantEngine) {
			t.Errorf("%q: this engine's WRONG answer changed shape -- expected the outer row to win (%s), got %s\n  re-measure before assuming it is still the same bug", tc.q, tc.wantEngine, got)
		}
	}
}

// r28nPropSchema varies EVERY affinity and both collation ranks, because the
// enclosing row now reaches a compound arm and a FROM-less aggregate through a
// route round 27 measured only for tryVDBEScan/tryVDBENoFrom: the compound's own
// dedup collation is resolved by a SEPARATE walk (outerSchemaCtx,
// engine/vdbe_compound_codegen.go) from the one that resolves a comparison's,
// so agreeing on "=" proves nothing about INTERSECT/EXCEPT/UNION.
var r28nPropSchema = []string{
	"CREATE TABLE o6(i INTEGER, t TEXT, n, r REAL, m NUMERIC, b BLOB)",
	"INSERT INTO o6 VALUES(1,'1.0',1,1.0,1,'1')",
	"CREATE TABLE o2(t TEXT, i INTEGER)",
	"INSERT INTO o2 VALUES('1.0',1)",
	"CREATE TABLE inn(t TEXT, i INTEGER, n)",
	"INSERT INTO inn VALUES('1.0',1,'1.0')",
	"CREATE TABLE ie(t TEXT, i INTEGER)",
	"CREATE TABLE cc(a TEXT COLLATE NOCASE, b TEXT, c TEXT COLLATE RTRIM)",
	"INSERT INTO cc VALUES('abc','ABC','xy ')",
	"CREATE TABLE cd(p TEXT, q TEXT COLLATE NOCASE)",
	"INSERT INTO cd VALUES('ABC','ABC')",
}

// TestR28NUnqualifiedKeepsColumnProperties is outerval_r27_test.go's sweep run
// through the two NEW routes, in the UNQUALIFIED spelling -- the one
// materializeOuterRefs deliberately never substitutes, so
// the property has to survive the COMPILE rather than ride on a LiteralExpr.
func TestR28NUnqualifiedKeepsColumnProperties(t *testing.T) {
	for _, q := range []string{
		// --- AFFINITY, compound route. The comparison lives in an arm, so its
		// affinity comes from compiler.affCtx's rowOuter tail.
		"SELECT i, (SELECT i='1.0' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY i",
		"SELECT t, (SELECT t=1 UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY t",
		"SELECT n, (SELECT n='1.0' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY n",
		"SELECT r, (SELECT r='1.0' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY r",
		"SELECT m, (SELECT m='1.0' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY m",
		"SELECT b, (SELECT b=1 UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY b",
		// Reversed operand order, and the two-column table with the REVERSED
		// declaration order holding the same row.
		"SELECT i, (SELECT '1.0'=i UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY i",
		"SELECT i, (SELECT i='1.0' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o2 GROUP BY i",
		"SELECT t, (SELECT t=1 UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o2 GROUP BY t",
		// --- AFFINITY, FROM-less-aggregate route (the WHERE clause is what the
		// compiler emits a comparison for there).
		"SELECT i, (SELECT count(*) WHERE i='1.0') FROM o6 GROUP BY i",
		"SELECT t, (SELECT count(*) WHERE t=1) FROM o6 GROUP BY t",
		"SELECT n, (SELECT count(*) WHERE n='1.0') FROM o6 GROUP BY n",
		"SELECT r, (SELECT count(*) WHERE r='1.0') FROM o6 GROUP BY r",
		"SELECT b, (SELECT count(*) WHERE b=1) FROM o6 GROUP BY b",
		"SELECT i, (SELECT count(*) WHERE '1.0'=i) FROM o6 GROUP BY i",
		// --- AFFINITY against an INNER COLUMN (sqlite3CompareAffinity's other
		// branch), and the EMPTY inner table.
		"SELECT i, (SELECT count(*) FROM inn WHERE inn.t=i UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY i",
		"SELECT t, (SELECT count(*) FROM inn WHERE inn.i=t UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY t",
		"SELECT n, (SELECT count(*) FROM inn WHERE inn.t=n UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY n",
		"SELECT i, (SELECT count(*) FROM ie WHERE ie.t=i UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY i",
		// --- The ROWID pseudo-column: INTEGER affinity, no declared collation.
		"SELECT i, (SELECT rowid='1.0' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM o6 GROUP BY i",
		"SELECT i, (SELECT count(*) WHERE rowid=1) FROM o6 GROUP BY i",
		// --- COLLATION through the arm's own comparison...
		"SELECT a, (SELECT a='ABC' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY a",
		"SELECT b, (SELECT b='abc' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY b",
		"SELECT c, (SELECT c='xy' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY c",
		"SELECT a, (SELECT count(*) WHERE a='ABC') FROM cc GROUP BY a",
		"SELECT b, (SELECT count(*) WHERE b='abc') FROM cc GROUP BY b",
		"SELECT c, (SELECT count(*) WHERE c='xy') FROM cc GROUP BY c",
		// ...and through the COMPOUND's OWN DEDUP row-equality, which is the
		// separate walk outerSchemaCtx feeds. cc.a declares NOCASE, cc.b none:
		// the NOCASE one matches 'ABC' and the BINARY one does not.
		"SELECT a, (SELECT a INTERSECT SELECT 'ABC') FROM cc GROUP BY a",
		"SELECT b, (SELECT b INTERSECT SELECT 'ABC') FROM cc GROUP BY b",
		"SELECT c, (SELECT c INTERSECT SELECT 'xy') FROM cc GROUP BY c",
		"SELECT a, (SELECT a EXCEPT SELECT 'ABC') FROM cc GROUP BY a",
		"SELECT b, (SELECT b EXCEPT SELECT 'ABC') FROM cc GROUP BY b",
		"SELECT a, count(*) FROM cc GROUP BY a HAVING EXISTS (SELECT a INTERSECT SELECT 'ABC')",
		"SELECT b, count(*) FROM cc GROUP BY b HAVING EXISTS (SELECT b INTERSECT SELECT 'ABC')",
		// An EXPLICIT collation on the other operand must outrank the correlated
		// column's DECLARED one; one written ON the reference still wins.
		"SELECT a, (SELECT a='ABC ' COLLATE RTRIM UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY a",
		"SELECT a, (SELECT 'ABC ' COLLATE RTRIM = a UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY a",
		"SELECT a, (SELECT a COLLATE BINARY = 'ABC' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY a",
		"SELECT a, (SELECT count(*) WHERE a='ABC ' COLLATE RTRIM) FROM cc GROUP BY a",
		"SELECT a, (SELECT count(*) WHERE a COLLATE BINARY = 'ABC') FROM cc GROUP BY a",
		// The LEFT operand's declared collation wins (sqlite3BinaryCompareCollSeq
		// consults pLeft first): cc.b is BINARY and cd.q NOCASE, so the two
		// orders answer DIFFERENTLY -- a fix that forced the correlated side's
		// collation would pass one and fail the other.
		"SELECT b, (SELECT count(*) FROM cd WHERE b=cd.q UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY b",
		"SELECT b, (SELECT count(*) FROM cd WHERE cd.q=b UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY b",
		"SELECT a, (SELECT count(*) FROM cd WHERE a=cd.p UNION SELECT -1 ORDER BY 1 DESC LIMIT 1) FROM cc GROUP BY a",
	} {
		if !differ(t, "r28n-props", append(append([]string(nil), r28nPropSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestR28NUnqualifiedWrites is the same properties reaching a WRITE, scored on
// the SIDE EFFECT: this whole class's failure mode is a write that silently does
// nothing, which "accepted" looks exactly like. Each script runs once per engine.
func TestR28NUnqualifiedWrites(t *testing.T) {
	for _, tc := range [][]string{
		// AFFINITY: the outer column's INTEGER affinity must survive into the
		// arm's comparison against a TEXT column, and its absence must too.
		{"CREATE TABLE uu(i INTEGER, mark TEXT)", "INSERT INTO uu VALUES(1,'x')",
			"CREATE TABLE tt(t TEXT)", "INSERT INTO tt VALUES('1.0')",
			"UPDATE uu SET mark='HIT' WHERE EXISTS (SELECT 1 FROM tt WHERE tt.t=i INTERSECT SELECT 1)",
			"SELECT i,mark FROM uu"},
		{"CREATE TABLE u2(i INTEGER, mark TEXT)", "INSERT INTO u2 VALUES(1,'x')",
			"UPDATE u2 SET mark='HIT' WHERE (SELECT i='1.0' UNION SELECT 0 ORDER BY 1 DESC LIMIT 1)",
			"SELECT i,mark FROM u2"},
		{"CREATE TABLE u3(i INTEGER, mark TEXT)", "INSERT INTO u3 VALUES(1,'x')",
			"UPDATE u3 SET mark='HIT' WHERE (SELECT count(*) WHERE i='1.0')",
			"SELECT i,mark FROM u3"},
		{"CREATE TABLE u4(t TEXT, mark TEXT)", "INSERT INTO u4 VALUES('1.0','x')",
			"UPDATE u4 SET mark='HIT' WHERE (SELECT t=1 UNION SELECT 0 ORDER BY 1 DESC LIMIT 1)",
			"SELECT t,mark FROM u4"},
		// COLLATION, through the compound's own dedup: c1.a declares NOCASE and
		// c3.a nothing, so the first UPDATE marks its row and the second must not.
		{"CREATE TABLE c1(a TEXT COLLATE NOCASE, b TEXT)", "INSERT INTO c1 VALUES('abc','x')",
			"UPDATE c1 SET b='HIT' WHERE EXISTS (SELECT a INTERSECT SELECT 'ABC')",
			"SELECT a,b FROM c1"},
		{"CREATE TABLE c3(a TEXT, b TEXT)", "INSERT INTO c3 VALUES('abc','x')",
			"UPDATE c3 SET b='HIT' WHERE EXISTS (SELECT a INTERSECT SELECT 'ABC')",
			"SELECT a,b FROM c3"},
		{"CREATE TABLE c4(a TEXT COLLATE NOCASE, b TEXT)", "INSERT INTO c4 VALUES('abc','x')",
			"UPDATE c4 SET b='HIT' WHERE (SELECT a='ABC' EXCEPT SELECT 0)",
			"SELECT a,b FROM c4"},
		{"CREATE TABLE c5(a TEXT COLLATE NOCASE, b TEXT)", "INSERT INTO c5 VALUES('abc','x')",
			"UPDATE c5 SET b='HIT' WHERE (SELECT count(*) WHERE a='ABC')",
			"SELECT a,b FROM c5"},
		// An EXPLICIT collation on the other operand still outranks the declared
		// one through the new route.
		{"CREATE TABLE c6(a TEXT COLLATE NOCASE, b TEXT)", "INSERT INTO c6 VALUES('abc','x')",
			"UPDATE c6 SET b='HIT' WHERE (SELECT 'ABC ' COLLATE RTRIM = a UNION SELECT 0 ORDER BY 1 DESC LIMIT 1)",
			"SELECT a,b FROM c6"},
		// DELETE, and a SET whose value comes out of the compound.
		{"CREATE TABLE k1(a TEXT COLLATE NOCASE, b TEXT)", "INSERT INTO k1 VALUES('abc','x'),('def','y')",
			"DELETE FROM k1 WHERE EXISTS (SELECT a INTERSECT SELECT 'ABC')",
			"SELECT a,b FROM k1 ORDER BY a"},
		{"CREATE TABLE m1(i INTEGER, mark TEXT)", "INSERT INTO m1 VALUES(1,'x')",
			"CREATE TABLE m2(t TEXT)", "INSERT INTO m2 VALUES('1.0')",
			"UPDATE m1 SET mark=(SELECT group_concat(t) FROM m2 WHERE m2.t=i UNION SELECT 'none' ORDER BY 1 LIMIT 1)",
			"SELECT i,mark FROM m1"},
		{"CREATE TABLE m3(i INTEGER, mark TEXT)", "INSERT INTO m3 VALUES(1,'x')",
			"UPDATE m3 SET mark=(SELECT count(*)+i)",
			"SELECT i,mark FROM m3"},
	} {
		if !differ(t, "r28n-props-write", tc) {
			t.Errorf("diverged on: %v", tc)
		}
	}
}

// TestR28NPseudoRowsThroughNewRoutes: the two outer scopes that are NOT SrcList
// tables. resolve.c gives a trigger's NEW/OLD TK_TRIGGER and an upsert's
// "excluded" TK_REGISTER, so exprRefToSrcList counts neither and
// sqlite3ReferencesSrcList answers -1 for "sum(new.a)" -- the aggregate STAYS in
// the subquery and the single-row value computes it exactly. Both now reach the
// compound and FROM-less-aggregate routes, so both are swept here.
func TestR28NPseudoRowsThroughNewRoutes(t *testing.T) {
	for _, tc := range [][]string{
		{"CREATE TABLE src(a INTEGER, b TEXT)", "CREATE TABLE tlog(v)",
			"CREATE TRIGGER tr AFTER INSERT ON src BEGIN INSERT INTO tlog VALUES((SELECT new.a UNION SELECT 99 ORDER BY 1 LIMIT 1)); END",
			"INSERT INTO src VALUES(3,'x')", "SELECT v FROM tlog"},
		{"CREATE TABLE src2(a INTEGER, b TEXT)", "CREATE TABLE tlog2(v)",
			"CREATE TRIGGER tr2 AFTER INSERT ON src2 BEGIN INSERT INTO tlog2 VALUES((SELECT sum(new.a))); END",
			"INSERT INTO src2 VALUES(3,'x')", "SELECT v FROM tlog2"},
		{"CREATE TABLE s3(a TEXT COLLATE NOCASE)", "CREATE TABLE tlog3(v)",
			"CREATE TRIGGER tr3 AFTER INSERT ON s3 BEGIN INSERT INTO tlog3 VALUES((SELECT new.a INTERSECT SELECT 'ABC')); END",
			"INSERT INTO s3 VALUES('abc')", "SELECT v FROM tlog3"},
		{"CREATE TABLE d(a INTEGER, b INTEGER)", "INSERT INTO d VALUES(1,1),(2,2)",
			"CREATE TABLE dlog(v)",
			"CREATE TRIGGER dtr AFTER DELETE ON d BEGIN INSERT INTO dlog VALUES((SELECT old.a UNION SELECT 99 ORDER BY 1 LIMIT 1)); END",
			"DELETE FROM d WHERE a=1", "SELECT v FROM dlog"},
		{"CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)", "INSERT INTO u VALUES(1,10)",
			"INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v UNION SELECT 99 ORDER BY 1 LIMIT 1)",
			"SELECT k,v FROM u"},
		{"CREATE TABLE u2(k INTEGER PRIMARY KEY, v INTEGER)", "INSERT INTO u2 VALUES(1,10)",
			"INSERT INTO u2 VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT sum(excluded.v))",
			"SELECT k,v FROM u2"},
	} {
		if !differ(t, "r28n-pseudorow", tc) {
			t.Errorf("diverged on: %v", tc)
		}
	}
}

// TestR28NStillDeclined used to pin "reassoc-in-compound-arm" and
// "reassoc-in-nofrom-agg" here as still-declined; both are now served and
// AGREE with the oracle (r35dNoFromAnywhere's per-arm compound fix,
// sql_agg.go, closes the exact "sum(v) UNION SELECT 0" / bare "sum(v)"
// re-association these needed) -- moved to TestR28NReassociationNowServed
// below per this file's own convention (TestR28NPseudoRowsThroughNewRoutes's
// doc comment).
func TestR28NReassociationNowServed(t *testing.T) {
	for _, q := range []string{
		"SELECT v, (SELECT sum(v) UNION SELECT 0 ORDER BY 1 DESC LIMIT 1) FROM outr GROUP BY v ORDER BY v",
		"SELECT v, (SELECT sum(v)) FROM outr GROUP BY v ORDER BY v",
	} {
		if !differ(t, "r28n-reassoc", append(append([]string(nil), r28nOwnFromSchema...), q)) {
			t.Errorf("diverged on: %v", q)
		}
	}
}
