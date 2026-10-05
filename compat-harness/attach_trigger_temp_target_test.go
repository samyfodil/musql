// Differential gate for TEMP triggers targeting ATTACHed database tables.
// Tests agree with the oracle, never acceptance alone.
package compat

import (
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestEngineAttachTempTriggerQualifiedTarget covers explicitly-qualified TEMP triggers on ATTACH tables.
func TestEngineAttachTempTriggerQualifiedTarget(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TEMP TRIGGER tst_trigger BEFORE INSERT ON aux.t4 BEGIN SELECT 'hello world'; END")
	p.agreeQuery("SELECT count(*) FROM temp.sqlite_master")
	p.agreeQuery("SELECT type, name, tbl_name FROM temp.sqlite_master")

	// Firing needs write to delegated session; must decline rather than silently miss.
	p.declineExec("INSERT INTO aux.t4 VALUES(1,2,3)")
	p.agreeQuery("SELECT * FROM aux.t4")

	// DROP TABLE cascade-drops the trigger too.
	p.agreeExec("DROP TABLE aux.t4")
	p.agreeQuery("SELECT count(*) FROM temp.sqlite_master")
}

// TestEngineAttachTempTriggerBareUnqualifiedTarget covers bare unqualified TEMP triggers on ATTACH tables.
func TestEngineAttachTempTriggerBareUnqualifiedTarget(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t1(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TEMPORARY TRIGGER tr1 DELETE ON t1 BEGIN SELECT 1; END")
	p.agreeQuery("SELECT count(*) FROM temp.sqlite_master")

	// New local t1: tr1 must not rebind.
	p.agreeExec("CREATE TABLE t1(x)")
	p.agreeExec("INSERT INTO t1 VALUES(9)")
	p.agreeExec("DELETE FROM t1") // fires nothing: local t1 has no trigger
	p.agreeQuery("SELECT * FROM t1")
}

// TestEngineAttachTempTriggerSameNameAcrossCatalogs tests same-name tables
// across catalogs each with their own TEMP trigger.
func TestEngineAttachTempTriggerSameNameAcrossCatalogs(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t4(a, b, c)")
	p.agreeExec("CREATE TABLE temp.t4(a, b, c)")
	p.agreeExec("CREATE TABLE insert_log(db, a, b, c)")

	p.agreeExec("CREATE TEMP TRIGGER trig1 AFTER INSERT ON main.t4 BEGIN INSERT INTO insert_log VALUES('main', new.a, new.b, new.c); END")
	p.agreeExec("CREATE TEMP TRIGGER trig2 AFTER INSERT ON temp.t4 BEGIN INSERT INTO insert_log VALUES('temp', new.a, new.b, new.c); END")
	p.agreeExec("CREATE TEMP TRIGGER trig3 AFTER INSERT ON aux.t4 BEGIN INSERT INTO insert_log VALUES('aux', new.a, new.b, new.c); END")

	// main.t4 insert fires only trig1.
	p.agreeExec("INSERT INTO main.t4 VALUES(1,2,3)")
	p.agreeQuery("SELECT * FROM insert_log ORDER BY db, a")

	// temp.t4 insert fires only trig2.
	p.agreeExec("INSERT INTO temp.t4 VALUES(4,5,6)")
	p.agreeQuery("SELECT * FROM insert_log ORDER BY db, a")

	// aux.t4 insert fires trig3.
	p.agreeExec("INSERT INTO aux.t4 VALUES(7,8,9)")
	p.agreeQuery("SELECT * FROM insert_log ORDER BY db, a")
}

// TestEngineAttachTempTriggerFiresOnceBodyStaysInAttachment is Phase 2 of
// the same feature (attach_write.go's routeAttachedStatement, trigger.go's
// attachedTriggerMirrorSafe): trigger1.test's OWN 10.9/10.10 sequence, where
// "the reference within trig3's program is re-resolved at statement compile
// time, not trigger installation time" (that file's own comment) flips an
// "INSERT INTO aux.t4" from firing back into the ORIGINATING session to
// firing entirely within the attachment, mid-session, with no CREATE
// TRIGGER of its own in between.
//
// trig3's body ("INSERT INTO insert_log VALUES('aux', new.a, new.b,
// new.c)") starts by writing this session's own main.insert_log (10.1-10.8:
// attachedTriggerOriginatingSafe fires it back into the originating
// session, confirmed against a live oracle probe -- C SQLite fires trig3
// unconditionally on any insert into its host table, and an unqualified
// name in its body resolves via the ordinary TEMP/MAIN/ATTACHED search
// order regardless of which database owns the table that triggered it).
// Once insert_log is DROPped from main and re-CREATEd inside aux (10.9),
// the identical trigger's body now resolves entirely within aux --
// mirror-safe, so the very next "INSERT INTO aux.t4" (10.10) fires it via
// the OTHER classification (attachedTriggerMirrorSafe) instead, verified
// against the real oracle by agreeExec/agreeQuery rather than merely
// accepted.
func TestEngineAttachTempTriggerFiresOnceBodyStaysInAttachment(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t4(a, b, c)")
	p.agreeExec("CREATE TABLE temp.t4(a, b, c)")
	p.agreeExec("CREATE TABLE insert_log(db, a, b, c)")
	p.agreeExec("CREATE TEMP TRIGGER trig1 AFTER INSERT ON main.t4 BEGIN INSERT INTO insert_log VALUES('main', new.a, new.b, new.c); END")
	p.agreeExec("CREATE TEMP TRIGGER trig2 AFTER INSERT ON temp.t4 BEGIN INSERT INTO insert_log VALUES('temp', new.a, new.b, new.c); END")
	p.agreeExec("CREATE TEMP TRIGGER trig3 AFTER INSERT ON aux.t4 BEGIN INSERT INTO insert_log VALUES('aux', new.a, new.b, new.c); END")

	// 10.1-10.8 (abbreviated): insert_log is still THIS session's own local
	// table, so trig3 fires back into the originating session -- all three
	// rows land in main.insert_log.
	p.agreeExec("INSERT INTO main.t4 VALUES(1,2,3)")
	p.agreeExec("INSERT INTO temp.t4 VALUES(4,5,6)")
	p.agreeExec("INSERT INTO aux.t4 VALUES(7,8,9)")
	p.agreeQuery("SELECT * FROM insert_log ORDER BY db, a")

	// 10.9: move insert_log into aux. C SQLite's own point: nothing about
	// trig3 itself changed, only what its body's UNQUALIFIED name now means.
	p.agreeExec("DROP TABLE insert_log")
	p.agreeExec("CREATE TABLE aux.insert_log(db, d, e, f)")

	// Insert into aux fires trig3 (mirror-safe).
	p.agreeExec("INSERT INTO aux.t4 VALUES(27,28,29)")
	p.agreeQuery("SELECT * FROM aux.insert_log")

	// main.t4 fires trig1, which writes to aux.insert_log.
	p.agreeExec("INSERT INTO main.t4 VALUES(31,32,33)")
	p.agreeQuery("SELECT * FROM aux.insert_log")

	// temp.t4 fires trig2, which writes to aux.insert_log.
	p.agreeExec("INSERT INTO temp.t4 VALUES(34,35,36)")
	p.agreeQuery("SELECT * FROM aux.insert_log ORDER BY d")
}

// TestEngineAttachTempTriggerBeforeStaysDeclinedEvenWhenBodyIsLocal ensures BEFORE triggers stay declined.
func TestEngineAttachTempTriggerBeforeStaysDeclinedEvenWhenBodyIsLocal(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)", "CREATE TABLE insert_log(db, a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	// BEFORE trigger must decline even though body stays in aux.
	p.agreeExec("CREATE TEMP TRIGGER trig3 BEFORE INSERT ON aux.t4 BEGIN INSERT INTO insert_log VALUES('aux', new.a, new.b, new.c); END")
	p.declineExec("INSERT INTO aux.t4 VALUES(7,8,9)")
	p.agreeQuery("SELECT * FROM aux.t4")
	p.agreeQuery("SELECT * FROM aux.insert_log")
}

// TestEngineAttachTempTriggerSurvivesLocalRename ensures renaming a local table
// doesn't affect an attachment-bound trigger.
func TestEngineAttachTempTriggerSurvivesLocalRename(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t4(a, b, c)")
	p.agreeExec("CREATE TEMP TRIGGER trig3 AFTER INSERT ON aux.t4 BEGIN SELECT 1; END")

	// Rename local t4; trig3 must be untouched.
	p.agreeExec("ALTER TABLE main.t4 RENAME TO t9")
	p.agreeQuery("SELECT sql FROM temp.sqlite_master WHERE name='trig3'")

	// aux.t4 is still trig3's target; write still declines.
	p.declineExec("INSERT INTO aux.t4 VALUES(1,2,3)")
}

// TestEngineAttachTempTriggerRenameCascade ensures renaming an ATTACH table
// cascades to rewrite dependent TEMP triggers.
func TestEngineAttachTempTriggerRenameCascade(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t1(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a, b, c)")
	// Trigger body has no table references.
	p.agreeExec("CREATE TEMP TRIGGER tr AFTER INSERT ON aux.t1 BEGIN SELECT length(new.a || new.b || new.c); END")
	p.agreeQuery("SELECT name, tbl_name FROM sqlite_temp_master")

	// Renaming local main.t1 must not touch tr.
	p.agreeExec("ALTER TABLE main.t1 RENAME TO t2")
	p.agreeQuery("SELECT name, tbl_name FROM sqlite_temp_master")

	// Renaming aux.t1 must cascade to tr.
	p.agreeExec("ALTER TABLE aux.t1 RENAME TO t2")
	p.agreeQuery("SELECT name, tbl_name FROM sqlite_temp_master")
	p.agreeQuery("SELECT sql FROM sqlite_temp_master WHERE name='tr'")

	// Write into renamed aux.t2 still resolves tr.
	p.declineExec("INSERT INTO aux.t2 VALUES(7, 8, 9)")
}

// TestEngineAttachTempTriggerRenameCascadeLegacy tests cascade under PRAGMA legacy_alter_table=1.
func TestEngineAttachTempTriggerRenameCascadeLegacy(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t1(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("PRAGMA legacy_alter_table = 1")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a, b, c)")
	p.agreeExec("CREATE TEMP TRIGGER tr AFTER INSERT ON aux.t1 BEGIN SELECT length(new.a || new.b || new.c); END")
	p.agreeQuery("SELECT name, tbl_name FROM sqlite_temp_master")

	p.agreeExec("ALTER TABLE main.t1 RENAME TO t2")
	p.agreeQuery("SELECT name, tbl_name FROM sqlite_temp_master")

	p.agreeExec("ALTER TABLE aux.t1 RENAME TO t2")
	p.agreeQuery("SELECT name, tbl_name FROM sqlite_temp_master")
	p.agreeQuery("SELECT sql FROM sqlite_temp_master WHERE name='tr'")
}

// TestEngineAttachTempTriggerRenameCascadeAmbiguousBodyDeclines tests
// conservative rejection when trigger body also references the renamed table.
func TestEngineAttachTempTriggerRenameCascadeAmbiguousBodyDeclines(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t1(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	// Body also mentions the renamed table; conservative check declines.
	p.agreeExec("CREATE TEMP TRIGGER tr AFTER INSERT ON aux.t1 BEGIN INSERT INTO t1 VALUES(new.a); END")

	p.declineExec("ALTER TABLE aux.t1 RENAME TO t9")
	// Decline leaves both sides untouched.
	p.agreeQuery("SELECT name, tbl_name FROM sqlite_temp_master")
}

// TestEngineAttachTempTriggerForeignAttachmentStillRefused ensures non-TEMP triggers
// cannot reference attachment objects.
func TestEngineAttachTempTriggerForeignAttachmentStillRefused(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TRIGGER trig AFTER INSERT ON aux.t4 BEGIN SELECT 1; END")
	p.agreeQuery("SELECT count(*) FROM aux.sqlite_master WHERE type='trigger'")
	p.agreeQuery("SELECT count(*) FROM sqlite_master WHERE type='trigger'")
}

// TestEngineAttachTempTriggerSurvivesDetach ensures TEMP triggers are not dropped on DETACH.
func TestEngineAttachTempTriggerSurvivesDetach(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE aux.t20_2(y)")
	p.agreeExec("CREATE TABLE aux.t20_3(z)")
	p.agreeExec("CREATE TEMP TRIGGER r20_3 AFTER INSERT ON t20_2 BEGIN UPDATE t20_3 SET z=z+1; END")
	p.agreeQuery("SELECT count(*) FROM temp.sqlite_master")

	p.agreeExec("DETACH aux")
	// The orphaned trigger still exists: DETACH does not touch db.triggers.
	p.agreeQuery("SELECT count(*) FROM temp.sqlite_master")

	p.agreeExec("DROP TRIGGER r20_3")
	p.agreeQuery("SELECT count(*) FROM temp.sqlite_master")
}

// TestEngineAttachTempTriggerDiesWithItsSession ensures TEMP triggers are session-local
// and don't persist across sessions.
func TestEngineAttachTempTriggerDiesWithItsSession(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")

	db, err := engine.Create(mainPath)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	if _, _, err := db.ExecArgs("ATTACH '"+auxPath+"' AS aux", nil); err != nil {
		t.Fatalf("ATTACH: %v", err)
	}
	if _, _, err := db.ExecArgs("CREATE TABLE aux.t4(a, b, c)", nil); err != nil {
		t.Fatalf("CREATE TABLE aux.t4: %v", err)
	}
	if _, _, err := db.ExecArgs("CREATE TEMP TRIGGER tst_trigger BEFORE INSERT ON aux.t4 BEGIN SELECT 1; END", nil); err != nil {
		t.Fatalf("CREATE TEMP TRIGGER: %v", err)
	}
	// Write that would fire the trigger is refused.
	if _, _, err := db.ExecArgs("INSERT INTO aux.t4 VALUES(1,2,3)", nil); err == nil {
		t.Errorf("INSERT INTO aux.t4: engine ACCEPTED a write that would need to fire the temp trigger")
	}
	if !db.TempDatabaseOpened() {
		t.Fatal("the TEMP TRIGGER did not open a temp database")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := engine.OpenWrite(mainPath)
	if err != nil {
		t.Fatalf("OpenWrite (reload): %v", err)
	}
	defer db2.Discard()

	// New session finds no trace of the trigger.
	countRows := func(sql string) int64 {
		t.Helper()
		pager, perr := db2.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		defer pager.Close()
		cols, rows, qerr := pager.QueryArgs(sql, nil)
		if qerr != nil {
			t.Fatalf("query %q: %v", sql, qerr)
		}
		if len(rows) != 1 || len(cols) != 1 {
			t.Fatalf("query %q: unexpected shape cols=%v rows=%v", sql, cols, rows)
		}
		return rows[0][0].I
	}
	if got := countRows("SELECT count(*) FROM temp.sqlite_master"); got != 0 {
		t.Fatalf("temp.sqlite_master count in a session that only opened the main file: got %d, want 0 -- a TEMP trigger must not persist", got)
	}
	// Main file never carried it either.
	if got := countRows("SELECT count(*) FROM sqlite_master WHERE type='trigger'"); got != 0 {
		t.Fatalf("sqlite_master trigger count: got %d, want 0 -- a TEMP trigger must not be written into main's file", got)
	}

	// Without the trigger, the write is an ordinary INSERT.
	if _, _, err := db2.ExecArgs("ATTACH '"+auxPath+"' AS aux", nil); err != nil {
		t.Fatalf("re-ATTACH: %v", err)
	}
	if _, _, err := db2.ExecArgs("INSERT INTO aux.t4 VALUES(1,2,3)", nil); err != nil {
		t.Fatalf("INSERT INTO aux.t4 after reopen: %v", err)
	}
}
