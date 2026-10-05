// Tests ATTACH with file: URI paths, including mode and parameter handling.
package compat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEngineAttachURIPaths(t *testing.T) {
	goDir, cgoDir := t.TempDir(), t.TempDir()
	p := newAttachPair(t, []string{goDir}, []string{cgoDir})

	// The wrong-answer input an independent audit named: ATTACH of a "file:"
	// URI with mode=rw for a path that does not exist must FAIL on both
	// sides WITHOUT CREATING THE FILE. mode=rw (unlike the default rwc) never
	// creates -- verified directly against mattn/go-sqlite3 3.53.3, and the
	// naive fix of merely stripping the scheme+query would instead make
	// ensureDatabaseFile create it and succeed.
	if goErr, cgoErr := p.exec("ATTACH 'file:{0}/definitely-missing.db?mode=rw' AS aux"); goErr == nil || cgoErr == nil {
		t.Errorf("ATTACH file:.../definitely-missing.db?mode=rw: expected both engines to reject; go=%v cgo=%v", goErr, cgoErr)
	}
	if _, statErr := os.Stat(filepath.Join(goDir, "definitely-missing.db")); !os.IsNotExist(statErr) {
		t.Errorf("mode=rw ATTACH of a missing file must not create it; os.Stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cgoDir, "definitely-missing.db")); !os.IsNotExist(statErr) {
		t.Errorf("mode=rw ATTACH of a missing file must not create it on the oracle side either; os.Stat err=%v", statErr)
	}

	// A plain "file:" path with no query parameters resolves to the bare
	// filename, exactly like a non-URI ATTACH -- the shape
	// TestEngineAttachDeclinedForms used to assert was declined outright.
	p.agreeExec("ATTACH 'file:{0}/plain.db' AS u")
	p.agreeQuery("SELECT count(*) FROM u.sqlite_master")
	p.agreeExec("CREATE TABLE u.t(x)")
	p.agreeExec("INSERT INTO u.t VALUES(1)")
	p.agreeQuery("SELECT x FROM u.t")
	p.agreeExec("DETACH u")

	// mode=rw on a file that ALREADY exists succeeds and behaves exactly
	// like an ordinary ATTACH of it from there on (reads AND writes -- rw
	// permits both; only ro would need to refuse writes, and ro is declined
	// below rather than approximated).
	p.agreeExec("ATTACH '{0}/existing.db' AS mk")
	p.agreeExec("CREATE TABLE mk.seed(v)")
	p.agreeExec("DETACH mk")
	p.agreeExec("ATTACH 'file:{0}/existing.db?mode=rw' AS ro1")
	p.agreeQuery("SELECT type, name FROM ro1.sqlite_master")
	p.agreeExec("INSERT INTO ro1.seed VALUES(9)")
	p.agreeQuery("SELECT v FROM ro1.seed")
	p.agreeExec("DETACH ro1")

	// mode=rwc, explicit, is the same create-if-missing default a bare
	// ATTACH already gets.
	p.agreeExec("ATTACH 'file:{0}/rwc.db?mode=rwc' AS rwcdb")
	p.agreeQuery("SELECT count(*) FROM rwcdb.sqlite_master")
	p.agreeExec("DETACH rwcdb")

	// 8_3_names is a Windows-only 8.3-filename-mangling toggle this engine
	// (like the oracle's own non-Windows VFS) never consults -- a verified
	// no-op, and 8_3_names.test's/crashM.test's own shape.
	p.agreeExec("ATTACH 'file:{0}/eightthree.db?8_3_names=1' AS e3")
	p.agreeExec("CREATE TABLE e3.t2(y)")
	p.agreeExec("INSERT INTO e3.t2 VALUES(2)")
	p.agreeQuery("SELECT y FROM e3.t2")
	p.agreeExec("DETACH e3")

	// mode=memory is a private database regardless of the path text: two
	// SEPARATE mode=memory ATTACHes of the SAME literal path do not share
	// content, because there is no cache=shared (which is declined below).
	p.agreeExec("ATTACH 'file:{0}/anything.db?mode=memory' AS mem1")
	p.agreeExec("CREATE TABLE mem1.t(x)")
	p.agreeExec("INSERT INTO mem1.t VALUES(1)")
	p.agreeExec("DETACH mem1")
	p.agreeExec("ATTACH 'file:{0}/anything.db?mode=memory' AS mem2")
	p.agreeQuery("SELECT count(*) FROM mem2.sqlite_master")
	p.agreeExec("DETACH mem2")

	// Authority: only "" and "localhost" are legal; anything else is a
	// syntax-level rejection on BOTH sides (this engine's own validation, not
	// a decline), verified directly against 3.53.3.
	p.agreeExec("ATTACH 'file://localhost{0}/abs.db' AS a1")
	p.agreeQuery("SELECT count(*) FROM a1.sqlite_master")
	p.agreeExec("DETACH a1")
	p.agreeExec("ATTACH 'file://baduser{0}/abs2.db' AS a2") // both reject: invalid authority

	// Case sensitivity: only a lowercase "file:" is SQLite's URI scheme
	// (sqlite3ParseUri's own memcmp, not a case-folding compare) --
	// "FILE:upper.db" is a literal filename on BOTH engines, verified
	// directly: the byte-for-byte-named file is what gets created, not
	// "upper.db". (The OLD decline here used a case-INSENSITIVE prefix
	// check, which would have wrongly declined this instead.)
	p.agreeExec("ATTACH '{0}/FILE:upper.db' AS up")
	if _, statErr := os.Stat(filepath.Join(goDir, "FILE:upper.db")); statErr != nil {
		t.Errorf("uppercase FILE: must attach a literal file named \"FILE:upper.db\": os.Stat: %v", statErr)
	}
	p.agreeExec("DETACH up")

	// An unrecognized mode= value is a syntax-level rejection on both sides.
	p.agreeExec("ATTACH 'file:{0}/whatever.db?mode=bogus' AS bad1")

	// An UNKNOWN vfs= name is not a decline at all: C SQLite's own
	// sqlite3_vfs_find lookup (main.c:3321-3325) fails it too -- "no such
	// vfs: tvfs2" -- so this is agreement, the uri.test#274 shape verbatim.
	p.agreeExec("ATTACH 'file:{0}/whatever2.db?vfs=tvfs2' AS vfsbad")

	// -- declined: a parameter (or value) this engine does not honor. --
	// The oracle accepts every one of these; declining is a missing
	// feature, never a wrong answer (declineExec does not run the oracle
	// side at all, for exactly that reason).
	//
	// vfs=unix IS one of the names a bare Linux build always registers (so
	// the oracle accepts it, unlike tvfs2 above) -- but this engine does not
	// model per-VFS locking-strategy differences, so it stays declined.
	p.declineExec("ATTACH 'file:{0}/whatever2b.db?vfs=unix' AS vfsknown")
	p.declineExec("ATTACH 'file:{0}/whatever3.db?mode=ro' AS robad")
	p.declineExec("ATTACH 'file:{0}/whatever4.db?cache=shared' AS cachebad")
	p.declineExec("ATTACH 'file:{0}/whatever5.db?psow=1' AS psowbad")
	p.declineExec("ATTACH 'file:{0}/whatever6.db?immutable=1' AS immbad")
	p.declineExec("ATTACH 'file:{0}/whatever7.db?nolock=1' AS nolockbad")
	p.declineExec("ATTACH 'file:{0}/whatever8.db?%61=1' AS pctbad") // percent-encoded key
}
