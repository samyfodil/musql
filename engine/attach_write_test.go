package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestCrossDatabaseWriteRoutesToTheAttachedSession validates cross-database writes.
func TestCrossDatabaseWriteRoutesToTheAttachedSession(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	aux := filepath.Join(dir, "aux.musq")
	for _, s := range []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`ATTACH '` + aux + `' AS aux`,
		`CREATE TABLE aux.t(a, b)`,
		`INSERT INTO aux.t VALUES(1,'x'),(2,'y')`,
		`UPDATE aux.t SET b='z' WHERE a=1`,
		`DELETE FROM aux.t WHERE a=2`,
		`CREATE INDEX aux.ti ON t(a)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	p.SetAttachedReadersForTest(db)
	// Read-your-writes across the boundary: aux.t must show the UPDATE and not
	// the DELETEd row, and main.t must be untouched.
	for _, tc := range []struct {
		q        string
		wantRows int
	}{
		{`SELECT a,b FROM aux.t ORDER BY a`, 1},
		{`SELECT a FROM t`, 1},
		{`SELECT name FROM aux.sqlite_master ORDER BY name`, 2},
	} {
		_, rows, qerr := p.QueryArgs(tc.q, nil)
		if qerr != nil {
			t.Errorf("%s: %v", tc.q, qerr)
			continue
		}
		if len(rows) != tc.wantRows {
			t.Errorf("%s: got %d rows, want %d", tc.q, len(rows), tc.wantRows)
		}
	}
	if _, rows, _ := p.QueryArgs(`SELECT b FROM aux.t`, nil); len(rows) == 1 && string(rows[0][0].S) != "z" {
		t.Errorf("aux.t.b = %q, want z -- the routed UPDATE did not land", rows[0][0].S)
	}
	p.Close()
	if e := db.Close(); e != nil {
		t.Fatalf("close: %v", e)
	}
	// Reopen the attached file on its own: the writes must be durable.
	//
	// Through OUR door, because an attachment is written in our format now
	// (openAttachedWrite): openSQLiteRead answered "bad magic (not a SQLite 3
	// database)" about a file this engine had just written correctly.
	rp, err := openAttachedRead(aux)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	_, rows, qerr := rp.QueryArgs(`SELECT a,b FROM t ORDER BY a`, nil)
	if qerr != nil || len(rows) != 1 {
		t.Errorf("aux.db did not persist its rows: rows=%v err=%v", rows, qerr)
	}
}

// ATTACH 'test3.db' AS 'ON';
// CREATE TABLE ON.t1(a,b,c)     -> near "ON": syntax error
// CREATE TABLE 'ON'.t1(a,b,c)   -> accepted
func TestCrossDatabaseQualifierRejectsAReservedKeyword(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "m.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	if err := db.Exec(`ATTACH '` + filepath.Join(dir, "on.musq") + `' AS 'ON'`); err != nil {
		t.Fatalf("ATTACH AS 'ON': %v", err)
	}
	if err := db.Exec(`CREATE TABLE ON.t1(a, b, c)`); err == nil {
		t.Error(`CREATE TABLE ON.t1 was accepted; C SQLite reports near "ON": syntax error`)
	}
	// An UNQUOTED name that is not reserved still routes, which is what proves
	// the gate is the keyword rule and not "any qualifier here is refused".
	if err := db.Exec(`ATTACH '` + filepath.Join(dir, "aux.musq") + `' AS aux`); err != nil {
		t.Fatalf("ATTACH AS aux: %v", err)
	}
	if err := db.Exec(`CREATE TABLE aux.t1(a, b, c)`); err != nil {
		t.Errorf(`CREATE TABLE aux.t1: %v -- an ordinary qualifier must route`, err)
	}
}

func TestCrossDatabaseWriteSeesItsOriginatingDatabases(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE m1(a)`,
		`INSERT INTO m1 VALUES(1),(3)`,
		`CREATE TEMP TABLE tmpsrc(a)`,
		`INSERT INTO tmpsrc VALUES(7)`,
		`ATTACH '` + filepath.Join(dir, "aux.musq") + `' AS aux`,
		`ATTACH '` + filepath.Join(dir, "two.musq") + `' AS two`,
		`CREATE TABLE aux.m1(a)`, // SAME NAME as main's, different rows
		`INSERT INTO aux.m1 VALUES(99)`,
		`CREATE TABLE two.only2(a)`,
		`INSERT INTO two.only2 VALUES(5)`,
		`CREATE TABLE aux.dst(a)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("setup %s: %v", s, e)
		}
	}
	for _, tc := range []struct {
		sql  string
		want string
	}{
		// main SHADOWS the target attachment's same-named table, as the
		// originating search order (temp, main, attachments) says it must.
		{`INSERT INTO aux.dst SELECT a FROM m1`, "1,3"},
		{`INSERT INTO aux.dst SELECT a FROM main.m1`, "1,3"},
		// ...and an explicit qualifier still picks the attachment's own.
		{`INSERT INTO aux.dst SELECT a FROM aux.m1`, "99"},
		// A name only a THIRD database has, and one only TEMP has.
		{`INSERT INTO aux.dst SELECT a FROM only2`, "5"},
		{`INSERT INTO aux.dst SELECT a FROM tmpsrc`, "7"},
		// A subquery source, not just an INSERT ... SELECT.
		{`INSERT INTO aux.dst SELECT a FROM aux.m1 WHERE a IN (SELECT 99 FROM m1)`, "99"},
	} {
		if e := db.Exec(tc.sql); e != nil {
			t.Errorf("%s: %v", tc.sql, e)
			continue
		}
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		p.SetAttachedReadersForTest(db)
		_, rows, qerr := p.QueryArgs(`SELECT group_concat(a) FROM aux.dst`, nil)
		if qerr != nil || len(rows) != 1 {
			t.Fatalf("%s: reading aux.dst back: %v", tc.sql, qerr)
		}
		if got := string(rows[0][0].S); got != tc.want {
			t.Errorf("%s: aux.dst = %q, want %q", tc.sql, got, tc.want)
		}
		p.Close()
		if e := db.Exec(`DELETE FROM aux.dst`); e != nil {
			t.Fatal(e)
		}
	}
	// CREATE TABLE ... AS SELECT reads the same way.
	if e := db.Exec(`CREATE TABLE aux.ct AS SELECT a FROM m1`); e != nil {
		t.Fatalf("CREATE TABLE aux.ct AS SELECT: %v", e)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetAttachedReadersForTest(db)
	if _, rows, qerr := p.QueryArgs(`SELECT group_concat(a) FROM aux.ct`, nil); qerr != nil ||
		len(rows) != 1 || string(rows[0][0].S) != "1,3" {
		t.Errorf("aux.ct = %v err=%v, want one row 1,3 -- CTAS read the WRONG m1", rows, qerr)
	}
}

// CREATE VIEW vm AS SELECT a FROM aux.x1  -> "view vm cannot reference
//
//	objects in database aux"
//
// CREATE TRIGGER trm ... INSERT INTO m1 SELECT a FROM aux.x1
//
//	-> "trigger trm cannot reference
//	   objects in database aux"
//
// CREATE VIEW aux.va AS SELECT a FROM m1  -> created, then USING it is
//
//	"no such table: aux.m1"
//
// a TRIGGER in aux whose body reads main's m1 -> fires as "no such table: aux.m1"
func TestCrossDatabaseWriteHidesOriginFromStoredBodies(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE mo(a)`, // MAIN-only: aux has no table of this name
		`INSERT INTO mo VALUES(8)`,
		`ATTACH '` + filepath.Join(dir, "aux.musq") + `' AS aux`,
		`CREATE TABLE aux.dst(a)`,
		`CREATE TABLE aux.trg(a)`,
		`CREATE VIEW aux.vo AS SELECT a FROM mo`,
		`CREATE TRIGGER aux.tro AFTER INSERT ON trg BEGIN INSERT INTO dst SELECT a FROM mo; END`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("setup %s: %v", s, e)
		}
	}
	// The VIEW is created (C SQLite creates it too) but must not resolve.
	if e := db.Exec(`INSERT INTO aux.dst SELECT a FROM vo`); e == nil {
		t.Error(`INSERT INTO aux.dst SELECT a FROM vo was accepted; aux's view body must not reach main's mo`)
	}
	// ...and neither must the TRIGGER's, when the routed write fires it.
	if e := db.Exec(`INSERT INTO aux.trg VALUES(1)`); e == nil {
		t.Error(`INSERT INTO aux.trg was accepted; aux's trigger body must not reach main's mo`)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetAttachedReadersForTest(db)
	if _, rows, qerr := p.QueryArgs(`SELECT count(*) FROM aux.dst`, nil); qerr != nil ||
		len(rows) != 1 || rows[0][0].I != 0 {
		t.Errorf("aux.dst = %v err=%v, want 0 rows -- a stored body reached main", rows, qerr)
	}
	// An unqualified SCHEMA CATALOG name in a routed write means the
	// ORIGINATING database's catalog, never the target attachment's: with three
	// tables in main and two in aux, C SQLite answers 3 for
	// "INSERT INTO aux.dst SELECT count(*) FROM sqlite_master" (verified
	// directly). It is spelled out in itemOwner because a catalog is not a
	// row-store table, so the ordinary name search cannot claim it -- and
	// without that the delegated session silently counted AUX's.
	if e := db.Exec(`INSERT INTO aux.dst SELECT count(*) FROM sqlite_master`); e != nil {
		t.Fatalf("INSERT ... FROM sqlite_master: %v", e)
	}
	p2, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	p2.SetAttachedReadersForTest(db)
	// main holds exactly one object (mo); aux holds four (dst, trg, vo, tro).
	if _, rows, qerr := p2.QueryArgs(`SELECT a FROM aux.dst`, nil); qerr != nil ||
		len(rows) != 1 || rows[0][0].I != 1 {
		t.Errorf("unqualified sqlite_master counted %v (err=%v), want 1 -- the ORIGINATING catalog, not aux's four objects", rows, qerr)
	}
}

func pragmaIntRow(t *testing.T, p *ReadOnlyPager, sqlText string) (cols []string, got int64) {
	t.Helper()
	cols, rows, err := p.QueryArgs(sqlText, nil)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: got %d rows / %d columns, want exactly one of each", sqlText, len(rows), len(cols))
	}
	return cols, rows[0][0].I
}

// PRAGMA aux.user_version=5     -> aux reads 5, main still reads 0
// PRAGMA aux.integrity_check(q) -> "no such table: aux.q" even when main has q
// PRAGMA aux.foreign_keys=ON    -> main, aux AND the unqualified form all read 1
func TestAttachedPragmaRoutesToTheNamedDatabase(t *testing.T) {
	dir := t.TempDir()
	mainPath, auxPath := filepath.Join(dir, "main.musq"), filepath.Join(dir, "aux.musq")
	db, err := Create(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE m1(a)`,
		`ATTACH '` + auxPath + `' AS aux`,
		`CREATE TABLE aux.x1(a)`,
		`PRAGMA aux.user_version = 5`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	// The setter landed on AUX's own session, not on main's.
	w := db.attachedNamed("aux").wdb
	if w == nil {
		t.Fatal("PRAGMA aux.user_version=5 did not open aux's own write session")
	}
	wp, err := w.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	if cols, got := pragmaIntRow(t, wp, `PRAGMA user_version`); got != 5 || len(cols) != 1 || cols[0] != "user_version" {
		t.Errorf("aux user_version = %d cols=%v, want 5 [user_version]", got, cols)
	}
	wp.Close()
	mp, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	if _, got := pragmaIntRow(t, mp, `PRAGMA user_version`); got != 0 {
		t.Errorf("main user_version = %d, want 0 -- the routed setter leaked into main", got)
	}
	mp.Close()
	// A CONNECTION-scoped pragma is the other half of the rule: its qualifier is
	// IGNORED, so setting it through the attachment must change the CONNECTION.
	if e := db.Exec(`PRAGMA aux.foreign_keys = ON`); e != nil {
		t.Fatalf("PRAGMA aux.foreign_keys=ON: %v", e)
	}
	if mp, err = db.SnapshotPager(); err != nil {
		t.Fatal(err)
	}
	if cols, got := pragmaIntRow(t, mp, `PRAGMA foreign_keys`); got != 1 || len(cols) != 1 {
		t.Errorf("foreign_keys = %d cols=%v after PRAGMA aux.foreign_keys=ON, want 1 [foreign_keys] -- it is connection-wide", got, cols)
	}
	mp.Close()
	if e := db.Close(); e != nil {
		t.Fatalf("close: %v", e)
	}
	// ...and it is DURABLE in aux's own file, with main's header untouched.
	for _, tc := range []struct {
		path string
		want int64
	}{{auxPath, 5}, {mainPath, 0}} {
		rp, oerr := openAttachedRead(tc.path)
		if oerr != nil {
			t.Fatal(oerr)
		}
		if cols, got := pragmaIntRow(t, rp, `PRAGMA user_version`); got != tc.want || len(cols) != 1 {
			t.Errorf("%s: user_version = %d cols=%v, want %d", tc.path, got, cols, tc.want)
		}
		rp.Close()
	}
}

// TestAttachedPragmaAcceptDeclineSplit pins WHICH qualified pragmas are
// accepted and which are declined -- the part that must come from the oracle
// rather than from what is convenient, because accepting one C SQLite
// answers differently desynchronizes the shared-oracle corpus replay for the
// rest of the script (see execPragma's own doc comment).
func TestAttachedPragmaAcceptDeclineSplit(t *testing.T) {
	for _, tc := range []struct {
		sql     string
		decline bool
		why     string
	}{
		// Routed: answered by AUX, so a table only MAIN has is not found.
		{sql: `PRAGMA aux.integrity_check(x1)`},
		{sql: `PRAGMA aux.integrity_check(m1)`, decline: true, why: `m1 is main's table; C SQLite says "no such table: aux.m1"`},
		{sql: `PRAGMA aux.foreign_key_check(nope)`, decline: true, why: "no such table in aux"},
		{sql: `PRAGMA aux.schema_version = 77`},
		{sql: `PRAGMA aux.journal_mode`},
		{sql: `PRAGMA aux.journal_mode = delete`},
		// Per-DATABASE and really tracked: the qualifier selects which stored
		// value moves, and the getter reports it back (pragma_tuning.go).
		{sql: `PRAGMA aux.cache_size = 100`},
		{sql: `PRAGMA aux.synchronous = 0`},
		{sql: `PRAGMA aux.journal_size_limit = 4096`},
		{sql: `PRAGMA aux.mmap_size = 65536`},
		// Connection-scoped / tracked-nowhere: the qualifier changes nothing.
		{sql: `PRAGMA aux.page_count`},
		{sql: `PRAGMA aux.table_info(x1)`},
		{sql: `PRAGMA aux.case_sensitive_like = ON`},
		// Connection-scoped and REALLY tracked: C SQLite ignores the
		// qualifier and the flag then reads back 1 for main, aux and
		// unqualified alike (attachedPragmaScope's own verified list).
		{sql: `PRAGMA aux.writable_schema = 1`},
		// Declined UNqualified too, so declining here changes nothing.
		{sql: `PRAGMA aux.locking_mode = exclusive`, decline: true, why: "this engine never holds a cross-statement exclusive lock"},
		// cache_spill's SETTER is per-database for the spill threshold and
		// per-CONNECTION for the flag, and "= 0" moves only the second -- so
		// the qualifier changes nothing here and accepting it is exact. Its
		// GETTER is the half that can still be declined, and only while that
		// database's cache_size is non-positive (see pragmaCacheSpill).
		{sql: `PRAGMA aux.cache_spill = 0`},
		// Accepted UNqualified, declined here: a qualified encoding assignment is
		// a silent no-op in C SQLite even on a still-EMPTY attached database
		// (and even for an unrecognized name), which neither routing it nor
		// answering it for main reproduces -- see pragmaScopeDeclined.
		{sql: `PRAGMA aux.encoding = 'UTF-16'`, decline: true, why: "C SQLite ignores it; this engine's unqualified form does not"},
		// An unattached qualifier stays "unknown database", not a routing miss.
		{sql: `PRAGMA nope.user_version`, decline: true, why: "unknown database"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			dir := t.TempDir()
			db, err := Create(filepath.Join(dir, "main.musq"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range []string{
				`CREATE TABLE m1(a)`,
				`ATTACH '` + filepath.Join(dir, "aux.musq") + `' AS aux`,
				`CREATE TABLE aux.x1(a)`,
			} {
				if e := db.Exec(s); e != nil {
					t.Fatalf("setup %s: %v", s, e)
				}
			}
			switch e := db.Exec(tc.sql); {
			case tc.decline && e == nil:
				t.Errorf("%s was accepted; it must be declined (%s)", tc.sql, tc.why)
			case !tc.decline && e != nil:
				t.Errorf("%s: %v", tc.sql, e)
			}
		})
	}
}

func TestAttachedPragmaJournalModeHonoursMemoryBacking(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	if e := db.Exec(`ATTACH ':memory:' AS aux`); e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{`PRAGMA aux.journal_mode = wal`, `PRAGMA aux.journal_mode = truncate`} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v -- C SQLite silently ignores it on a memory-backed database", s, e)
		}
	}
	w := db.attachedNamed("aux").wdb
	if w == nil {
		t.Fatal("no write session was opened for the routed pragma")
	}
	p, err := w.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cols, rows, qerr := p.QueryArgs(`PRAGMA journal_mode`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(cols) != 1 || len(rows) != 1 || string(rows[0][0].S) != journalModeMemory {
		t.Errorf("aux journal_mode = cols=%v rows=%v, want one row [memory]", cols, rows)
	}
}

func TestAttachedCreateTriggerSelfQualifiedFires(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	auxPath := filepath.Join(dir, "aux.musq")
	for _, s := range []string{
		`ATTACH '` + auxPath + `' AS aux`,
		`CREATE TABLE aux.t2(a, b)`,
		`CREATE TABLE aux.log2(x)`,
		// A same-named pair in MAIN too, so a wrongly-scoped trigger (or a
		// wrongly-scoped ON-clause table lookup) has somewhere wrong to fire.
		`CREATE TABLE t2(a, b)`,
		`CREATE TABLE log2(x)`,
		`CREATE TRIGGER aux.trig AFTER INSERT ON aux.t2 BEGIN INSERT INTO log2 VALUES(new.a); END`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	readLog := func(q string) []Value {
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		defer p.Close()
		p.SetAttachedReadersForTest(db)
		_, rows, qerr := p.QueryArgs(q, nil)
		if qerr != nil {
			t.Fatalf("%s: %v", q, qerr)
		}
		out := make([]Value, len(rows))
		for i, r := range rows {
			out[i] = r[0]
		}
		return out
	}
	if e := db.Exec(`INSERT INTO aux.t2 VALUES(1,2)`); e != nil {
		t.Fatalf("INSERT INTO aux.t2: %v", e)
	}
	if e := db.Exec(`INSERT INTO t2 VALUES(9,9)`); e != nil {
		t.Fatalf("INSERT INTO t2: %v", e)
	}
	if got := readLog(`SELECT x FROM aux.log2`); len(got) != 1 || got[0].I != 1 {
		t.Errorf("aux.log2 = %v, want one row [1] -- the trigger did not fire on aux.t2", got)
	}
	if got := readLog(`SELECT x FROM log2`); len(got) != 0 {
		t.Errorf("main.log2 = %v, want empty -- the trigger fired against the WRONG database's t2", got)
	}

	// A trigger whose ON-clause names a DIFFERENT attached database than its
	// own name is still declined -- C SQLite refuses it outright ("trigger
	// t cannot reference objects in database three", verified directly), so
	// this is not a routing gap: stripAttachedCreateTriggerOnQualifier only
	// ever strips a qualifier that matches q, the database being routed to.
	if e := db.Exec(`ATTACH '` + filepath.Join(dir, "three.musq") + `' AS three`); e != nil {
		t.Fatal(e)
	}
	if e := db.Exec(`CREATE TABLE three.t3(a)`); e != nil {
		t.Fatal(e)
	}
	if e := db.Exec(`CREATE TRIGGER aux.trig2 AFTER INSERT ON three.t3 BEGIN INSERT INTO log2 VALUES(new.a); END`); e == nil {
		t.Error(`CREATE TRIGGER aux.trig2 ... ON three.t3 was accepted; C SQLite refuses a trigger whose ON-clause names a database other than its own`)
	}

	// A trigger created and then ROLLED BACK must not persist, and must not
	// fire afterward -- exactly like any other routed cross-database write
	// (rollbackTxnAttachedWrites).
	for _, s := range []string{
		`BEGIN`,
		`CREATE TRIGGER aux.trig3 AFTER INSERT ON aux.t2 BEGIN INSERT INTO log2 VALUES(new.b); END`,
		`INSERT INTO aux.t2 VALUES(3,4)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	// The one INSERT fires BOTH triggers now registered on aux.t2 (AFTER INSERT,
	// most-recently-created first): trig3 (new.b=4) and the original trig
	// (new.a=3), on top of the row trig already left from the earlier insert.
	if got := readLog(`SELECT x FROM aux.log2`); len(got) != 3 {
		t.Fatalf("aux.log2 before rollback = %v, want 3 rows (trig's first firing, plus trig3 and trig firing again)", got)
	}
	if e := db.Exec(`ROLLBACK`); e != nil {
		t.Fatalf("ROLLBACK: %v", e)
	}
	if got := readLog(`SELECT x FROM aux.log2`); len(got) != 1 {
		t.Errorf("aux.log2 after rollback = %v, want 1 row -- the transaction's writes must be undone", got)
	}
	if e := db.Exec(`INSERT INTO aux.t2 VALUES(5,6)`); e != nil {
		t.Fatalf("INSERT INTO aux.t2 after rollback: %v", e)
	}
	if got := readLog(`SELECT x FROM aux.log2`); len(got) != 2 || got[1].I != 5 {
		t.Errorf("aux.log2 after post-rollback insert = %v, want [1 5] -- trig3 must not have survived the rollback to fire a second time", got)
	}

	if e := db.Close(); e != nil {
		t.Fatalf("close: %v", e)
	}

	// A completely FRESH session -- a new main database ATTACHing the same
	// aux.db file from scratch -- must reload the trigger from its stored
	// (already self-qualifier-free) CREATE TRIGGER text and still fire it.
	// This is the "schema RECOVERY sequencing" case: attachedWriteSession
	// calls OpenWrite before SetLocalSchema, so a stored trigger whose ON-clause
	// still carried a qualifier would fail the identical way on reload: by
	// stripping the qualifier at CREATE time instead, the stored text never
	// carries one, so this reload path never has to know its own attach name.
	db2, err := Create(filepath.Join(dir, "main2.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Discard()
	if e := db2.Exec(`ATTACH '` + auxPath + `' AS aux`); e != nil {
		t.Fatal(e)
	}
	if e := db2.Exec(`INSERT INTO aux.t2 VALUES(7,8)`); e != nil {
		t.Fatalf("INSERT INTO aux.t2 after reopen: %v", e)
	}
	p, perr := db2.SnapshotPager()
	if perr != nil {
		t.Fatal(perr)
	}
	defer p.Close()
	p.SetAttachedReadersForTest(db2)
	_, rows, qerr := p.QueryArgs(`SELECT x FROM aux.log2 ORDER BY x`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 3 || rows[2][0].I != 7 {
		t.Errorf("aux.log2 after reopen+insert = %v, want 3 rows ending in 7 -- the reloaded trigger did not fire", rows)
	}
}

// TestUnqualifiedWriteIntoAttachedVirtualTableRoutes gates
// unqualifiedAttachedTarget/hasLocalObject (attach_write.go): an UNQUALIFIED
// write whose target only exists in an ATTACHed database must still route
// there, exactly like a qualified one -- C SQLite's sqlite3FindTable
// (build.c:373-393) searches TEMP, then main, then every attachment in
// attachment order for an unqualified name, with no exception for a virtual
// table.
//
// This is the mined fts3aj.test shape: "ATTACH ... AS two; CREATE VIRTUAL
// TABLE two.t2 USING fts3(content); INSERT INTO t2 (rowid, content)
// VALUES(...)" with no "two." on the INSERT. It used to fail with "no such
// table: t2" -- routed only via the DRIVER's own separate routing
// (crossdb_write_test.go's header explains why that hid it here): hasLocalObject
// checked db.findTableMetaIn (ordinary tables, db.tables) but never
// db.findVtabMetaIn (db.vtabs), so an attached write SESSION that had already
// created the virtual table still answered "I don't have it" forever.
func TestUnqualifiedWriteIntoAttachedVirtualTableRoutes(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	aux := filepath.Join(dir, "aux.musq")
	for _, s := range []string{
		`ATTACH '` + aux + `' AS two`,
		`CREATE VIRTUAL TABLE two.t2 USING fts3(content)`,
		`INSERT INTO t2 (rowid, content) VALUES(1, 'hello world')`,
		`INSERT INTO t2 (rowid, content) VALUES(2, 'hello there')`,
		`INSERT INTO t2 (rowid, content) VALUES(3, 'cruel world')`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, qerr := p.QueryArgs(`SELECT rowid FROM t2 WHERE t2 MATCH 'hello'`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 2 || rows[0][0].I != 1 || rows[1][0].I != 2 {
		t.Errorf("SELECT rowid FROM t2 WHERE t2 MATCH 'hello' = %v, want rowid 1,2 -- the unqualified writes never reached the attached vtab", rows)
	}
}

// TestAttachedTriggerFiresAgainstOriginatingSession reproduces
// ~/.cache/musql/sqlite-353/test/trigger1.test 10.0-10.11 verbatim: three
// same-named t4 tables (main, temp, and an attachment) each carry a TEMP
// trigger that logs into insert_log, and insert_log itself starts out as a
// MAIN table -- so trig3 (AFTER INSERT ON aux.t4) is bound to aux but its
// body resolves entirely back to THIS session's own catalog, the
// attachedTriggerOriginatingSafe shape (attach_write.go's
// attachedOriginatingFireInsert), not the pre-existing mirror shape
// (attachedTriggerMirrorSafe) trig3 only becomes once 10.9 moves insert_log
// into aux. Before this bucket, "INSERT INTO aux.t4 ..." at 10.3 declined
// outright; this pins the exact row insert_log must hold at each step,
// across an explicit transaction's ROLLBACK too (10.5-10.6, checking the
// delegated aux.t4 row and this session's own insert_log row are undone
// TOGETHER, not just one of them).
func TestAttachedTriggerFiresAgainstOriginatingSession(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	aux := filepath.Join(dir, "test2.musq")
	for _, s := range []string{
		`ATTACH '` + aux + `' AS aux`,
		`CREATE TABLE main.t4(a, b, c)`,
		`CREATE TABLE temp.t4(a, b, c)`,
		`CREATE TABLE aux.t4(a, b, c)`,
		`CREATE TABLE insert_log(db, a, b, c)`,
		`CREATE TEMP TRIGGER trig1 AFTER INSERT ON main.t4 BEGIN INSERT INTO insert_log VALUES('main', new.a, new.b, new.c); END`,
		`CREATE TEMP TRIGGER trig2 AFTER INSERT ON temp.t4 BEGIN INSERT INTO insert_log VALUES('temp', new.a, new.b, new.c); END`,
		`CREATE TEMP TRIGGER trig3 AFTER INSERT ON aux.t4 BEGIN INSERT INTO insert_log VALUES('aux', new.a, new.b, new.c); END`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	// readLog queries "SELECT *" rather than naming insert_log's own columns:
	// 10.9 below drops and re-creates it with DIFFERENT column names (db, d,
	// e, f instead of db, a, b, c) -- exactly what C SQLite's own
	// trigger1.test comment calls out ("we can change the column names
	// because the trigger programs don't use them explicitly"), and trig3's
	// body never names them either (a bare "INSERT INTO insert_log VALUES(...)"
	// with no column list), so this is the query shape that survives that
	// rename the same way the real trigger program does.
	readLog := func() [][]Value {
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		defer p.Close()
		p.SetAttachedReadersForTest(db)
		_, rows, qerr := p.QueryArgs(`SELECT * FROM insert_log`, nil)
		if qerr != nil {
			t.Fatal(qerr)
		}
		return rows
	}
	checkLog := func(step string, want [][3]int64) {
		t.Helper()
		got := readLog()
		if len(got) != len(want) {
			t.Fatalf("%s: insert_log = %v, want %d rows", step, got, len(want))
		}
		for i, w := range want {
			r := got[i]
			if string(r[0].S) != "aux" && string(r[0].S) != "main" && string(r[0].S) != "temp" {
				t.Fatalf("%s row %d: db=%q unexpected", step, i, r[0].S)
			}
			if r[1].I != w[0] || r[2].I != w[1] || r[3].I != w[2] {
				t.Errorf("%s row %d = (%d,%d,%d), want (%d,%d,%d)", step, i, r[1].I, r[2].I, r[3].I, w[0], w[1], w[2])
			}
		}
	}

	// 10.3: the ONE statement this bucket closes -- "INSERT INTO aux.t4" with
	// insert_log still local to main is trig3's originating-safe shape.
	for _, s := range []string{
		`INSERT INTO main.t4 VALUES(1, 2, 3)`,
		`INSERT INTO temp.t4 VALUES(4, 5, 6)`,
		`INSERT INTO aux.t4 VALUES(7, 8, 9)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("10.3: %s: %v", s, e)
		}
	}
	checkLog("10.4", [][3]int64{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})

	// 10.5-10.6: the same three inserts, inside a transaction that is rolled
	// back -- both the delegated aux.t4 row and this session's own
	// insert_log row from trig3's originating fire must vanish together.
	for _, s := range []string{
		`BEGIN`,
		`INSERT INTO main.t4 VALUES(1, 2, 3)`,
		`INSERT INTO temp.t4 VALUES(4, 5, 6)`,
		`INSERT INTO aux.t4 VALUES(7, 8, 9)`,
		`ROLLBACK`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("10.5: %s: %v", s, e)
		}
	}
	checkLog("10.6", [][3]int64{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	if got := countRowsForTest(t, db, `aux.t4`); got != 1 {
		t.Errorf("aux.t4 after rollback has %d rows, want 1 -- the delegated INSERT must have been rolled back with the rest of the transaction", got)
	}

	// 10.7-10.8
	if e := db.Exec(`DELETE FROM insert_log`); e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{
		`INSERT INTO main.t4 VALUES(11, 12, 13)`,
		`INSERT INTO temp.t4 VALUES(14, 15, 16)`,
		`INSERT INTO aux.t4 VALUES(17, 18, 19)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("10.7: %s: %v", s, e)
		}
	}
	checkLog("10.8", [][3]int64{{11, 12, 13}, {14, 15, 16}, {17, 18, 19}})

	// 10.9: move insert_log into aux -- trig3's body now resolves entirely
	// within aux, the PRE-EXISTING mirror-safe shape, not this bucket's own.
	for _, s := range []string{
		`DROP TABLE insert_log`,
		`CREATE TABLE aux.insert_log(db, d, e, f)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("10.9: %s: %v", s, e)
		}
	}
	// 10.10-10.11
	for _, s := range []string{
		`INSERT INTO main.t4 VALUES(21, 22, 23)`,
		`INSERT INTO temp.t4 VALUES(24, 25, 26)`,
		`INSERT INTO aux.t4 VALUES(27, 28, 29)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("10.10: %s: %v", s, e)
		}
	}
	checkLog("10.11", [][3]int64{{21, 22, 23}, {24, 25, 26}, {27, 28, 29}})
}

// TestAttachedOriginatingTriggerFailureUndoesBothSessions gates the
// atomicity half of attachedOriginatingFireInsert (attach_write.go): trig3's
// body write into THIS session's own insert_log fails a UNIQUE constraint on
// its second firing, which C SQLite's single-connection statement
// atomicity would undo COMPLETELY -- both the trigger's own effect AND the
// delegated base-table row the ATTACHed session (w) already stored, since
// they are one real-SQLite statement. Pins that a delegated aux.t4 row is
// NOT left committed while its trigger's own effect is missing (or vice
// versa): execRoutedToAttached's paired w/db beginStatementSnapshot is what
// this bucket added to make that true, since w's own write and db's own
// trigger fire happen as two separate Go calls rather than one.
func TestAttachedOriginatingTriggerFailureUndoesBothSessions(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	aux := filepath.Join(dir, "aux.musq")
	for _, s := range []string{
		`ATTACH '` + aux + `' AS aux`,
		`CREATE TABLE aux.t4(a, b, c)`,
		// db is UNIQUE, so a second trigger firing collides with the first.
		`CREATE TABLE insert_log(db UNIQUE, a, b, c)`,
		`CREATE TEMP TRIGGER trig3 AFTER INSERT ON aux.t4 BEGIN INSERT INTO insert_log VALUES('aux', new.a, new.b, new.c); END`,
		`INSERT INTO aux.t4 VALUES(7, 8, 9)`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	if got := countRowsForTest(t, db, `aux.t4`); got != 1 {
		t.Fatalf("aux.t4 after the first insert has %d rows, want 1", got)
	}
	if got := countRowsForTest(t, db, `insert_log`); got != 1 {
		t.Fatalf("insert_log after the first insert has %d rows, want 1", got)
	}
	// The second "INSERT INTO aux.t4" stores its row into aux fine, but
	// trig3's own body then collides on insert_log's UNIQUE db column --
	// C SQLite fails the WHOLE statement, undoing the base-table row too.
	if e := db.Exec(`INSERT INTO aux.t4 VALUES(17, 18, 19)`); e == nil {
		t.Fatal(`second "INSERT INTO aux.t4" was accepted; want a UNIQUE constraint failure from trig3's own body`)
	}
	if got := countRowsForTest(t, db, `aux.t4`); got != 1 {
		t.Errorf("aux.t4 after the failed second insert has %d rows, want 1 -- the delegated row must be undone along with the trigger's own failed write", got)
	}
	if got := countRowsForTest(t, db, `insert_log`); got != 1 {
		t.Errorf("insert_log after the failed second insert has %d rows, want 1 -- the trigger's own partial effect must not survive its own statement's failure", got)
	}
	// A THIRD insert, after the failed one, must succeed normally -- the
	// failure must not have left either session's in-memory state corrupted.
	if e := db.Exec(`INSERT INTO aux.t4 VALUES(27, 28, 29)`); e == nil {
		t.Fatal(`third "INSERT INTO aux.t4" was accepted; want it to ALSO collide, since trig3 still tries to write db='aux' again`)
	}
	if got := countRowsForTest(t, db, `aux.t4`); got != 1 {
		t.Errorf("aux.t4 after the third (also failing) insert has %d rows, want 1", got)
	}
}

// TestAttachedOriginatingTriggerStaysDeclinedOutsideItsNarrowShape checks
// that attachedOriginatingInsertCapturable's refusals (attach_write.go) are
// live: a multi-row VALUES list, an "INSERT ... SELECT" source, and an
// UPDATE/DELETE against the SAME table trig3 is bound to must all still
// decline cleanly (errVDBEUnsupported), never silently skip firing trig3 --
// this bucket only ever added a NEW acceptance for the single-row literal
// INSERT shape, and must not have widened the existing decline for anything
// else that reaches the same tableHasAnyAttachedTrigger gate.
func TestAttachedOriginatingTriggerStaysDeclinedOutsideItsNarrowShape(t *testing.T) {
	newDB := func() (*Session, string) {
		d := t.TempDir()
		db, err := Create(filepath.Join(d, "main.musq"))
		if err != nil {
			t.Fatal(err)
		}
		aux := filepath.Join(d, "aux.musq")
		for _, s := range []string{
			`ATTACH '` + aux + `' AS aux`,
			`CREATE TABLE aux.t4(a, b, c)`,
			`CREATE TABLE insert_log(db, a, b, c)`,
			`CREATE TABLE src(x, y, z)`,
			`INSERT INTO src VALUES(1,2,3)`,
			`CREATE TEMP TRIGGER trig3 AFTER INSERT ON aux.t4 BEGIN INSERT INTO insert_log VALUES('aux', new.a, new.b, new.c); END`,
		} {
			if e := db.Exec(s); e != nil {
				t.Fatalf("%s: %v", s, e)
			}
		}
		return db, aux
	}

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"multi-row VALUES", `INSERT INTO aux.t4 VALUES(1,2,3),(4,5,6)`},
		{"INSERT ... SELECT", `INSERT INTO aux.t4 SELECT x,y,z FROM src`},
		{"OR REPLACE", `INSERT OR REPLACE INTO aux.t4 VALUES(1,2,3)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := newDB()
			defer db.Discard()
			e := db.Exec(tc.sql)
			if e == nil {
				t.Fatalf("%s: accepted; want a clean decline (this shape's row(s) cannot be safely captured back for trig3's own firing)", tc.sql)
			}
			if !errors.Is(e, errVDBEUnsupported) {
				t.Errorf("%s: got %v, want an errVDBEUnsupported decline", tc.sql, e)
			}
		})
	}

	// An UPDATE or a DELETE bound the same way is NOT declined: it fires per
	// row from the OLD/NEW images w's own change capture recorded
	// (attachedOriginatingFireRows), which is what temptrigger.test 8.2.2 and
	// 8.3.2 need.
	db, _ := newDB()
	defer db.Discard()
	for _, s := range []string{
		`INSERT INTO aux.t4 VALUES(1,2,3)`,
		`INSERT INTO aux.t4 VALUES(4,5,6)`,
		`CREATE TEMP TRIGGER trig4 AFTER UPDATE ON aux.t4 BEGIN INSERT INTO insert_log VALUES('upd:'||old.a, new.a, new.b, new.c); END`,
		`CREATE TEMP TRIGGER trig5 AFTER DELETE ON aux.t4 BEGIN INSERT INTO insert_log VALUES('del:'||old.a, old.b, old.c, 0); END`,
		`UPDATE aux.t4 SET a=a+10 WHERE a=4`,
		`DELETE FROM aux.t4`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	p, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatal(perr)
	}
	_, rows, qerr := p.QueryArgs(`SELECT db, a, b, c FROM insert_log ORDER BY rowid`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	var got []string
	for _, r := range rows {
		var cells []string
		for _, v := range r {
			if v.Typ == Int {
				cells = append(cells, fmt.Sprint(v.I))
			} else {
				cells = append(cells, valueText(v))
			}
		}
		got = append(got, strings.Join(cells, " "))
	}
	want := []string{"aux 1 2 3", "aux 4 5 6", "upd:4 14 5 6", "del:1 2 3 0", "del:14 5 6 0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("insert_log after a routed UPDATE and DELETE:\n got %v\nwant %v", got, want)
	}
}

func countRowsForTest(t *testing.T, db *Session, table string) int64 {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetAttachedReadersForTest(db)
	_, rows, qerr := p.QueryArgs(`SELECT count(*) FROM `+table, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	return rows[0][0].I
}
