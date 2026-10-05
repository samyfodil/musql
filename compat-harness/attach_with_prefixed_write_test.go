// Tests WITH-prefixed write statements (INSERT, UPDATE, DELETE) targeting
// ATTACHed databases, both qualified and unqualified.
package compat

import "testing"

// TestAttachWithPrefixedInsertUnqualified is crashM.test 1.0's own shape: the
// object exists ONLY in the attachment, referenced with no qualifier at all,
// after a WITH clause.
func TestAttachWithPrefixedInsertUnqualified(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(x, y)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec(`WITH s(a) AS (
		SELECT 1 UNION ALL SELECT a+1 FROM s WHERE a<50
	)
	INSERT INTO t2 SELECT a, a*2 FROM s`)
	p.agreeQuery("SELECT count(*), sum(y) FROM aux.t2")
}

// TestAttachWithPrefixedInsertQualified is the same CTE-prefixed INSERT, this
// time with its target explicitly qualified.
func TestAttachWithPrefixedInsertQualified(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(x, y)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec(`WITH s(a) AS (
		SELECT 1 UNION ALL SELECT a+1 FROM s WHERE a<50
	)
	INSERT INTO aux.t2 SELECT a, a*2 FROM s`)
	p.agreeQuery("SELECT count(*), sum(y) FROM aux.t2")
	// main never got one, since the target was ATTACHed.
	p.agreeQuery("SELECT name FROM main.sqlite_master WHERE type='table'")
}

// TestAttachWithPrefixedInsertRecursiveKeyword pins the explicit "WITH
// RECURSIVE" spelling too, qualified.
func TestAttachWithPrefixedInsertRecursiveKeyword(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(x, y)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec(`WITH RECURSIVE s(a) AS (
		SELECT 1 UNION ALL SELECT a+1 FROM s WHERE a<50
	)
	INSERT INTO aux.t2 SELECT a, a*2 FROM s`)
	p.agreeQuery("SELECT count(*), sum(y) FROM aux.t2")
}

// TestAttachWithPrefixedUpdate pins UPDATE, both qualified and left to
// resolve unqualified, each preceded by a WITH clause whose CTE feeds the
// WHERE clause rather than a source of rows.
func TestAttachWithPrefixedUpdate(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY, y TEXT)",
		"INSERT INTO t2 VALUES(1,'a'),(2,'b'),(3,'c')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec(`WITH bad(x) AS (SELECT 2)
	UPDATE aux.t2 SET y='touched' WHERE x IN (SELECT x FROM bad)`)
	p.agreeQuery("SELECT x,y FROM aux.t2 ORDER BY x")

	p.agreeExec(`WITH bad(x) AS (SELECT 3)
	UPDATE t2 SET y='touched-unqualified' WHERE x IN (SELECT x FROM bad)`)
	p.agreeQuery("SELECT x,y FROM aux.t2 ORDER BY x")
}

// TestAttachWithPrefixedDelete pins DELETE the same way.
func TestAttachWithPrefixedDelete(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY, y TEXT)",
		"INSERT INTO t2 VALUES(1,'a'),(2,'b'),(3,'c')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec(`WITH doomed(x) AS (SELECT 1)
	DELETE FROM aux.t2 WHERE x IN (SELECT x FROM doomed)`)
	p.agreeQuery("SELECT x,y FROM aux.t2 ORDER BY x")

	p.agreeExec(`WITH doomed(x) AS (SELECT 2)
	DELETE FROM t2 WHERE x IN (SELECT x FROM doomed)`)
	p.agreeQuery("SELECT x,y FROM aux.t2 ORDER BY x")
}

// TestAttachWithPrefixedInsertMainWins pins that the unqualified form still
// obeys ordinary precedence (main outranks an attachment) even behind a WITH
// clause -- attach_unqualified_write_test.go's own precedence case, replayed
// with a CTE in front.
func TestAttachWithPrefixedInsertMainWins(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE both(x INTEGER PRIMARY KEY, tag TEXT)",
		"INSERT INTO both VALUES(1,'aux')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE both(x INTEGER PRIMARY KEY, tag TEXT)")
	p.agreeExec("INSERT INTO both VALUES(1,'main')")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec(`WITH s(x,tag) AS (SELECT 2,'written-unqualified')
	INSERT INTO both SELECT x,tag FROM s`)
	p.agreeQuery("SELECT x,tag FROM main.both ORDER BY x")
	p.agreeQuery("SELECT x,tag FROM aux.both ORDER BY x")
}
