// Unqualified write targets in ATTACHed databases; precedence: temp > main > attachments.
// mis-ranked precedence still "works" and simply writes to the wrong database.
package compat

import "testing"

// TestAttachUnqualifiedWriteRoutes is the base case: the object exists ONLY in
// the attachment, so every DML shape must find it there.
func TestAttachUnqualifiedWriteRoutes(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO t VALUES(1,'aux-one')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE m(id INTEGER PRIMARY KEY)")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("INSERT INTO t VALUES(2,'two')")
	p.agreeQuery("SELECT a,b FROM t ORDER BY a")
	p.agreeQuery("SELECT a,b FROM aux.t ORDER BY a")
	p.agreeExec("UPDATE t SET b='updated' WHERE a=2")
	p.agreeQuery("SELECT a,b FROM aux.t ORDER BY a")
	p.agreeExec("REPLACE INTO t VALUES(1,'replaced')")
	p.agreeQuery("SELECT a,b FROM aux.t ORDER BY a")
	p.agreeExec("DELETE FROM t WHERE a=2")
	p.agreeQuery("SELECT a,b FROM aux.t ORDER BY a")
	p.agreeExec("INSERT INTO t(a,b) SELECT 7,'from-select'")
	p.agreeQuery("SELECT a,b FROM aux.t ORDER BY a")
	// main is untouched throughout.
	p.agreeQuery("SELECT count(*) FROM m")
	p.agreeQuery("SELECT count(*) FROM main.m")
}

// TestAttachUnqualifiedWriteMainWins is the precedence case that matters most:
// when BOTH databases have the name, the write belongs to main.
func TestAttachUnqualifiedWriteMainWins(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE both(x INTEGER PRIMARY KEY, tag TEXT)",
		"INSERT INTO both VALUES(1,'aux')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE both(x INTEGER PRIMARY KEY, tag TEXT)")
	p.agreeExec("INSERT INTO both VALUES(1,'main')")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("INSERT INTO both VALUES(2,'written-unqualified')")
	p.agreeExec("UPDATE both SET tag='touched' WHERE x=1")
	p.agreeExec("DELETE FROM both WHERE x=99")
	// The row must be in MAIN and the attachment must be unchanged.
	p.agreeQuery("SELECT x,tag FROM main.both ORDER BY x")
	p.agreeQuery("SELECT x,tag FROM aux.both ORDER BY x")
	p.agreeQuery("SELECT x,tag FROM both ORDER BY x")
}

// TestAttachUnqualifiedWriteTempWins pins the other side of precedence: a TEMP
// object outranks an attachment of the same name.
func TestAttachUnqualifiedWriteTempWins(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE tt(x INTEGER PRIMARY KEY, tag TEXT)",
		"INSERT INTO tt VALUES(1,'aux')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TEMP TABLE tt(x INTEGER PRIMARY KEY, tag TEXT)")
	p.agreeExec("INSERT INTO tt VALUES(1,'temp')")
	p.agreeExec("UPDATE tt SET tag='temp-touched'")
	p.agreeQuery("SELECT x,tag FROM temp.tt ORDER BY x")
	p.agreeQuery("SELECT x,tag FROM aux.tt ORDER BY x")
}

// TestAttachUnqualifiedWriteAttachOrder pins that the FIRST attachment holding
// the name wins when two of them do.
func TestAttachUnqualifiedWriteAttachOrder(t *testing.T) {
	goA, cgoA := buildAuxPair(t, "a1",
		"CREATE TABLE shared(x INTEGER PRIMARY KEY, tag TEXT)",
		"INSERT INTO shared VALUES(1,'first')",
	)
	goB, cgoB := buildAuxPair(t, "a2",
		"CREATE TABLE shared(x INTEGER PRIMARY KEY, tag TEXT)",
		"INSERT INTO shared VALUES(1,'second')",
	)
	p := newAttachPair(t, []string{goA, goB}, []string{cgoA, cgoB})
	p.agreeExec("ATTACH '{0}' AS one")
	p.agreeExec("ATTACH '{1}' AS two")
	p.agreeExec("INSERT INTO shared VALUES(2,'landed')")
	p.agreeQuery("SELECT x,tag FROM one.shared ORDER BY x")
	p.agreeQuery("SELECT x,tag FROM two.shared ORDER BY x")
}

// TestAttachUnqualifiedCreateStaysInMain pins the statement kind that must NOT
// be resolved by search order: an unqualified CREATE creates in main even when
// an attachment already has that name.
func TestAttachUnqualifiedCreateStaysInMain(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE fresh(x INTEGER PRIMARY KEY, tag TEXT)",
		"INSERT INTO fresh VALUES(1,'aux')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE fresh(x INTEGER PRIMARY KEY, tag TEXT)")
	p.agreeExec("INSERT INTO fresh VALUES(9,'main')")
	p.agreeQuery("SELECT x,tag FROM main.fresh ORDER BY x")
	p.agreeQuery("SELECT x,tag FROM aux.fresh ORDER BY x")
}

// TestAttachUnqualifiedDropAndAlter covers the other two statements whose
// target must already exist, including the per-TYPE name space.
func TestAttachUnqualifiedDropAndAlter(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE gone(x)",
		"CREATE TABLE renameme(x)",
		"CREATE TABLE idxhost(x)",
		"CREATE INDEX solo ON idxhost(x)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("ALTER TABLE renameme RENAME TO renamed")
	p.agreeQuery("SELECT name FROM aux.sqlite_master WHERE type='table' ORDER BY name")
	p.agreeExec("DROP TABLE gone")
	p.agreeQuery("SELECT name FROM aux.sqlite_master WHERE type='table' ORDER BY name")
	p.agreeExec("DROP INDEX solo")
	p.agreeQuery("SELECT name FROM aux.sqlite_master ORDER BY name")
}

// TestAttachUnqualifiedWriteInTransaction pins that routing an unqualified
// target obeys the same transaction rules the qualified form already does: the
// write is undone by a ROLLBACK and kept by a COMMIT.
func TestAttachUnqualifiedWriteInTransaction(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO t VALUES(1,'one')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO t VALUES(2,'rolled-back')")
	p.agreeQuery("SELECT a,b FROM t ORDER BY a")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT a,b FROM aux.t ORDER BY a")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO t VALUES(3,'committed')")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT a,b FROM aux.t ORDER BY a")
}

// TestAttachUnqualifiedWriteAfterDetach pins that the routing follows the
// attached SET: once detached, the same statement is "no such table" again.
func TestAttachUnqualifiedWriteAfterDetach(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("INSERT INTO t VALUES(1,'while-attached')")
	p.agreeExec("DETACH aux")
	p.agreeExec("INSERT INTO t VALUES(2,'after-detach')")
}
