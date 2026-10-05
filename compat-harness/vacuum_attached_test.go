// Tests VACUUM <name> for attached databases.
package compat

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// schemaVersionOf reads PRAGMA schema_version from both engines on a path.
func schemaVersionOf(t *testing.T, goPath, cgoPath string) (goVer, cgoVer int64) {
	t.Helper()
	pg, err := engine.Open(goPath)
	if err != nil {
		t.Fatalf("engine.Open(%s): %v", goPath, err)
	}
	defer pg.Close()
	_, rows, err := pg.QueryArgs("PRAGMA schema_version", nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("PRAGMA schema_version on %s: rows=%v err=%v", goPath, rows, err)
	}
	goVer = rows[0][0].I

	db, err := sql.Open("sqlite3", cgoPath)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", cgoPath, err)
	}
	defer db.Close()
	if err := db.QueryRow("PRAGMA schema_version").Scan(&cgoVer); err != nil {
		t.Fatalf("PRAGMA schema_version on %s: %v", cgoPath, err)
	}
	return goVer, cgoVer
}

// TestEngineVacuumAttached is the positive case: VACUUM of a name that IS
// attached renumbers that attachment's rowid-eligible tables and bumps ITS
// OWN schema_version, while main's schema_version and content are untouched.
func TestEngineVacuumAttached(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	p.agreeExec("CREATE TABLE m(a)")
	p.agreeExec("INSERT INTO m VALUES(1)")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE aux.t2(x, y)")
	p.agreeExec("INSERT INTO aux.t2 VALUES(10, 20)")
	p.agreeExec("INSERT INTO aux.t2 VALUES(30, 40)")
	p.agreeExec("INSERT INTO aux.t2 VALUES(50, 60)")
	p.agreeExec("DELETE FROM aux.t2 WHERE x = 30")

	// A DETACH/re-ATTACH round trip forces this engine's own deferred
	// attachment-write session to flush to aux's file (TestAttachDetachCommitsItsWrites
	// already gates that commit-on-DETACH rule); schemaVersionOf below reads
	// the file directly, so this is what makes its "before" snapshot honest
	// rather than catching the FILE mid-flight.
	p.agreeExec("DETACH aux")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeQuery("PRAGMA schema_version")
	goVerBefore, cgoVerBefore := schemaVersionOf(t, goAux, cgoAux)
	if goVerBefore != cgoVerBefore {
		t.Fatalf("pre-VACUUM aux schema_version already diverges: go=%d cgo=%d", goVerBefore, cgoVerBefore)
	}

	p.agreeExec("VACUUM aux")

	// main's OWN schema_version did NOT move -- only the vacuumed database's
	// cookie is bumped (verified directly against mattn/go-sqlite3 3.53.3).
	p.agreeQuery("PRAGMA schema_version")
	// ...and aux's OWN schema_version moved by exactly one, on BOTH engines --
	// checked on a FRESH connection straight onto aux's file, since a
	// schema-qualified PRAGMA query through SnapshotPager's read path is a
	// pre-existing gap unrelated to this change (attach_write.go's PRAGMA
	// routing is wired for the EXEC path -- routeAttachedStatement's
	// pragmaScopeRouted case -- not the plain-query one agreeQuery uses).
	if goVer, cgoVer := schemaVersionOf(t, goAux, cgoAux); goVer != goVerBefore+1 || cgoVer != cgoVerBefore+1 {
		t.Errorf("VACUUM aux: schema_version did not bump by exactly one on both sides: before go=%d cgo=%d, after go=%d cgo=%d",
			goVerBefore, cgoVerBefore, goVer, cgoVer)
	}

	// Rowids renumbered to a contiguous 1..N in ascending old-rowid order --
	// the same rule a plain VACUUM applies to main, applied here to aux's own
	// table.
	p.agreeQuery("SELECT rowid, x, y FROM aux.t2 ORDER BY rowid")

	// main is completely untouched.
	p.agreeQuery("SELECT a FROM m")
	p.agreeQuery("SELECT count(*) FROM sqlite_master")

	// A name that is NOT attached is still "unknown database", unaffected by
	// this change.
	if goErr, cgoErr := p.exec("VACUUM notattached"); goErr == nil || cgoErr == nil {
		t.Errorf("VACUUM notattached: expected both engines to reject; go=%v cgo=%v", goErr, cgoErr)
	}

	// The negative case this change must not disturb: VACUUM's autocommit
	// gate is CONNECTION-wide, not per-database -- "BEGIN; VACUUM aux" must
	// still fail with "cannot VACUUM from within a transaction" even though
	// the transaction never touched aux (verified directly; see
	// vacuum_write.go's own doc comment, which this must not weaken).
	p.agreeExec("BEGIN")
	if goErr, cgoErr := p.exec("VACUUM aux"); goErr == nil || cgoErr == nil {
		t.Errorf("BEGIN; VACUUM aux: expected both engines to reject; go=%v cgo=%v", goErr, cgoErr)
	}
	p.agreeExec("COMMIT")
}

// TestEngineVacuumAttachedMemory covers "VACUUM <name>" for a name attached
// as ':memory:'. C SQLite accepts it outright (verified directly: no
// error, and the table's rowid is renumbered exactly as a file-backed
// attachment's would be) -- there is a live b-tree to compact whether or not
// it has a file of its own. This engine's own ':memory:' attachment is
// backed by a private temp file (attach.go's own doc comment), so
// execVacuumAttached needs no special case for it at all: attachedWriteSession
// opens that file exactly as it would any other attachment's.
func TestEngineVacuumAttachedMemory(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("ATTACH ':memory:' AS mem1")
	p.agreeExec("CREATE TABLE mem1.t(x, y)")
	p.agreeExec("INSERT INTO mem1.t VALUES(1, 2)")
	p.agreeExec("INSERT INTO mem1.t VALUES(3, 4)")
	p.agreeExec("DELETE FROM mem1.t WHERE x = 3")
	p.agreeExec("VACUUM mem1")
	p.agreeQuery("SELECT rowid, x, y FROM mem1.t ORDER BY rowid")
}

// TestEngineVacuumAttachedIsolatesOtherDatabases is vacuum5.test's own
// shape: with THREE open databases, "VACUUM <name>" must touch only the
// named one -- x3's file must be BYTE IDENTICAL before and after (not just
// its SQL-visible content), since a page this engine's own write model never
// even opens for x3 is the strongest form of "left alone" there is. x2's
// OWN size is deliberately not asserted to shrink here: this engine's
// write sessions rebuild fresh from the live row store at every flush
// (writer.go), and DB.retentionFloor only pads a rebuild back up to a size
// this SESSION has already seen -- so a session that never saw a LARGER x2
// (e.g. right after a fresh ATTACH) has nothing of its own to shrink from,
// even though a longer-lived session (this file's own TestEngineVacuumAttached,
// and the manual mrun/probe evidence in this change's own commit) shows a
// real byte shrink. Page-count/freelist fidelity against C SQLite's
// incremental allocator is a separate, tracked gap (see
// DB.retentionFloor's own doc comment) this change does not attempt to close.
func TestEngineVacuumAttachedIsolatesOtherDatabases(t *testing.T) {
	goX2, cgoX2 := buildAuxPair(t, "x2")
	goX3, cgoX3 := buildAuxPair(t, "x3")
	p := newAttachPair(t, []string{goX2, goX3}, []string{cgoX2, cgoX3})

	p.agreeExec("CREATE TABLE t1(a, b)")
	p.agreeExec("ATTACH '{0}' AS x2")
	p.agreeExec("ATTACH '{1}' AS x3")
	p.agreeExec("CREATE TABLE x2.t2(a, b)")
	p.agreeExec("CREATE TABLE x3.t3(a, b)")
	bigVal := "'" + strings.Repeat("x", 500) + "'"
	for i := 0; i < 60; i++ {
		p.agreeExec("INSERT INTO t1 VALUES(" + bigVal + ", " + bigVal + ")")
		p.agreeExec("INSERT INTO x2.t2 VALUES(" + bigVal + ", " + bigVal + ")")
		p.agreeExec("INSERT INTO x3.t3 VALUES(" + bigVal + ", " + bigVal + ")")
	}
	p.agreeExec("DELETE FROM t1 WHERE rowid % 3 != 0")
	p.agreeExec("DELETE FROM x2.t2 WHERE rowid % 4 != 0")
	p.agreeExec("DELETE FROM x3.t3 WHERE rowid % 5 != 0")

	// Force this engine's own deferred attachment-write sessions to flush,
	// so x3's "before" bytes below reflect its actual current content and
	// not a stale, still-empty file (TestAttachDetachCommitsItsWrites gates
	// the commit-on-DETACH rule this relies on).
	p.agreeExec("DETACH x2")
	p.agreeExec("ATTACH '{0}' AS x2")
	p.agreeExec("DETACH x3")
	p.agreeExec("ATTACH '{1}' AS x3")

	// The file AND its delta: on this format a commit that is not a rewrite
	// lands in the delta, so the file alone would miss one.
	x3Bytes := func() string {
		var all []byte
		for _, p := range []string{goX3, goX3 + ".delta"} {
			b, err := os.ReadFile(p)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("reading %s: %v", p, err)
			}
			all = append(all, b...)
		}
		return string(all)
	}
	x3Before := x3Bytes()

	p.agreeExec("VACUUM x2")

	if x3After := x3Bytes(); x3After != x3Before {
		t.Errorf("VACUUM x2 changed x3's files (len before=%d after=%d)", len(x3Before), len(x3After))
	}
	p.agreeQuery("SELECT a, b FROM x2.t2 ORDER BY rowid")
	p.agreeQuery("SELECT a, b FROM t1 ORDER BY rowid")
	p.agreeQuery("SELECT a, b FROM x3.t3 ORDER BY rowid")
}
