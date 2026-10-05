// Differential tests for ALTER TABLE: RENAME TO, RENAME COLUMN, ADD COLUMN,
// DROP COLUMN against C SQLite. Verifies schema, data, persistence, and
// divergences like declining ALTER on views.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// TestAlterTableMatchesCSQLite is the ALTER TABLE conformance gate.
func TestAlterTableMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testAlterTableScenario(t, pageSize)
		})
	}
}

func testAlterTableScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("alter_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // a single logical connection -- see vdbe_drop_test.go's identical note

	// check compares a PRAGMA/SELECT's result between a fresh snapshot of
	// the engine writer's CURRENT (possibly mid-transaction) state and sdb,
	// requiring an exact match -- see assertPragmaEqual (vdbe_pragma_test.go).
	check := func(q string) {
		t.Helper()
		pager, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		assertPragmaEqual(t, pager, sdb, q)
	}

	// ---- phase 1: RENAME TO ----
	for _, s := range []string{
		`CREATE TABLE rt(a INTEGER, b TEXT)`,
		`CREATE INDEX idx_rt ON rt(b)`,
		`INSERT INTO rt VALUES(1,'x'),(2,'y')`,
	} {
		execDropBoth(t, db, sdb, s)
	}
	execDropBoth(t, db, sdb, `ALTER TABLE rt RENAME TO rt2`)
	check(`PRAGMA table_info(rt2)`)
	check(`PRAGMA table_info(rt)`) // the old name: zero rows now, on both sides
	check(`SELECT * FROM rt2 ORDER BY a`)
	check(`PRAGMA index_list(rt2)`)
	check(`PRAGMA index_info(idx_rt)`)

	execDropBoth(t, db, sdb, `ALTER TABLE rt2 RENAME TO rt2`) // collides with itself
	execDropBoth(t, db, sdb, `CREATE TABLE rt_other(x)`)
	execDropBoth(t, db, sdb, `ALTER TABLE rt2 RENAME TO rt_other`) // collides with an existing table
	execDropBoth(t, db, sdb, `ALTER TABLE rt_nope RENAME TO whatever`)
	execDropBoth(t, db, sdb, `ALTER TABLE sqlite_master RENAME TO x`)

	// ---- phase 2: RENAME COLUMN ----
	for _, s := range []string{
		`CREATE TABLE rc(a INTEGER, b TEXT)`,
		`CREATE INDEX idx_rc ON rc(b)`,
		`INSERT INTO rc VALUES(1,'p'),(2,'q')`,
	} {
		execDropBoth(t, db, sdb, s)
	}
	execDropBoth(t, db, sdb, `ALTER TABLE rc RENAME COLUMN b TO c`)
	check(`PRAGMA table_info(rc)`)
	check(`SELECT a,c FROM rc ORDER BY a`)
	check(`PRAGMA index_info(idx_rc)`)

	execDropBoth(t, db, sdb, `ALTER TABLE rc RENAME COLUMN nope TO z`)
	execDropBoth(t, db, sdb, `ALTER TABLE rc RENAME COLUMN a TO c`) // duplicate
	execDropBoth(t, db, sdb, `ALTER TABLE rc RENAME c TO d`)        // bare form (no COLUMN keyword)
	check(`PRAGMA table_info(rc)`)
	check(`SELECT a,d FROM rc ORDER BY a`)

	// ---- phase 3: ADD COLUMN ----
	for _, s := range []string{
		`CREATE TABLE ac(a INTEGER)`,
		`INSERT INTO ac VALUES(1),(2)`,
	} {
		execDropBoth(t, db, sdb, s)
	}
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN b TEXT DEFAULT 'z'`)
	check(`PRAGMA table_info(ac)`)
	check(`SELECT * FROM ac ORDER BY a`)

	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN c INTEGER`) // no default: backfills NULL
	check(`PRAGMA table_info(ac)`)
	check(`SELECT * FROM ac ORDER BY a`)

	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN a TEXT`)                // duplicate name
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN d INTEGER PRIMARY KEY`) // rejected
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN d TEXT UNIQUE`)         // rejected
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN d TEXT NOT NULL`)       // rejected: rows exist, no default

	// A REFERENCES clause is DECLARATIVE with foreign keys off (C SQLite's
	// default, and the only state this engine has -- it declines
	// "PRAGMA foreign_keys=ON"), so every form of it is accepted: the coldef's
	// verbatim text lands in the stored CREATE TABLE sql and foreign_key_list
	// then reports the key, NEWEST FIRST. All verified against the oracle.
	execDropBoth(t, db, sdb, `CREATE TABLE acp(id INTEGER PRIMARY KEY)`)
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN r REFERENCES acp(id)`)
	check(`PRAGMA table_info(ac)`)
	check(`SELECT sql FROM sqlite_master WHERE name='ac'`)
	check(`PRAGMA foreign_key_list(ac)`)
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN r2 INTEGER DEFAULT 5 REFERENCES acp(id)`)
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN r3 INTEGER NOT NULL DEFAULT 7 REFERENCES acp(id) ON DELETE CASCADE`)
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN r4 REFERENCES acp`) // no column list
	check(`PRAGMA table_info(ac)`)
	check(`SELECT sql FROM sqlite_master WHERE name='ac'`)
	check(`PRAGMA foreign_key_list(ac)`)
	check(`SELECT * FROM ac ORDER BY rowid`)
	execDropBoth(t, db, sdb, `ALTER TABLE ac ADD COLUMN d TEXT NOT NULL DEFAULT 'ok'`)
	check(`PRAGMA table_info(ac)`)
	check(`SELECT * FROM ac ORDER BY a`)

	execDropBoth(t, db, sdb, `ALTER TABLE ac_nope ADD COLUMN x TEXT`)

	// ---- phase 4: DROP COLUMN ----
	for _, s := range []string{
		`CREATE TABLE dc(a INTEGER, b TEXT, c TEXT, d INTEGER UNIQUE)`,
		`INSERT INTO dc VALUES(1,'x','y',10),(2,'p','q',20)`,
	} {
		execDropBoth(t, db, sdb, s)
	}
	execDropBoth(t, db, sdb, `ALTER TABLE dc DROP COLUMN b`)
	check(`PRAGMA table_info(dc)`)
	check(`SELECT * FROM dc ORDER BY a`)

	execDropBoth(t, db, sdb, `ALTER TABLE dc DROP COLUMN nope`)
	execDropBoth(t, db, sdb, `ALTER TABLE dc DROP COLUMN d`) // inline UNIQUE: rejected

	execDropBoth(t, db, sdb, `CREATE TABLE dc2(id INTEGER PRIMARY KEY, x TEXT)`)
	execDropBoth(t, db, sdb, `ALTER TABLE dc2 DROP COLUMN id`) // rowid-alias PK: rejected

	execDropBoth(t, db, sdb, `CREATE TABLE dc3(a TEXT)`)
	execDropBoth(t, db, sdb, `ALTER TABLE dc3 DROP COLUMN a`) // last remaining column: rejected

	execDropBoth(t, db, sdb, `CREATE TABLE dc4(a INTEGER, b INTEGER)`)
	execDropBoth(t, db, sdb, `CREATE INDEX idx_dc4 ON dc4(b)`)
	execDropBoth(t, db, sdb, `ALTER TABLE dc4 DROP COLUMN b`) // referenced by an explicit index: rejected

	execDropBoth(t, db, sdb, `CREATE TABLE dc5(a INTEGER, b INTEGER, UNIQUE(a,b))`)
	execDropBoth(t, db, sdb, `ALTER TABLE dc5 DROP COLUMN a`) // referenced by a table-level UNIQUE(...): rejected

	execDropBoth(t, db, sdb, `ALTER TABLE dc DROP COLUMN c`) // succeeds: dc is left with (a,d)
	check(`PRAGMA table_info(dc)`)
	check(`SELECT * FROM dc ORDER BY a`)

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close (phase 1): %v", err)
	}

	// ---- phase 5: reopen and prove a fresh session can still ALTER ----
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	execDropBoth(t, db2, sdb, `ALTER TABLE rt2 RENAME TO rt3`)
	execDropBoth(t, db2, sdb, `INSERT INTO rt3 VALUES(3,'z')`)
	if err := db2.Close(); err != nil {
		t.Fatalf("engine writer Close (phase 2): %v", err)
	}

	// ---- final verification: the file, via C SQLite, through the EXPORT ----
	//
	// What musql wrote is a segment file, which C cannot read -- so the question
	// "does C SQLite accept what this engine produced" is asked of
	// ExportSQLite's output, which is the only .db this engine makes. That is also
	// where the page size went: it used to be Create's, and this format has no
	// pages, so the two sizes now exercise the EXPORT's own page layout.
	exported := path + ".export.db"
	if xerr := sqliteconv.Export(path, exported, pageSize); xerr != nil {
		t.Fatalf("ExportSQLite(pageSize=%d): %v", pageSize, xerr)
	}
	fdb, err := sql.Open("sqlite3", exported)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s): %v", path, err)
	}
	defer fdb.Close()

	var integrity string
	if err := fdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
	}

	gotSchema := dumpSchema(t, fdb)
	wantSchema := dumpSchema(t, sdb)
	if len(gotSchema) != len(wantSchema) {
		t.Fatalf("final schema: got %d objects %+v, want %d %+v", len(gotSchema), gotSchema, len(wantSchema), wantSchema)
	}
	for i := range wantSchema {
		if gotSchema[i] != wantSchema[i] {
			t.Errorf("final schema object %d: got %+v, want %+v", i, gotSchema[i], wantSchema[i])
		}
	}

	verifyTableViaCSQLite(t, fdb, wvTable{name: "rt3", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "x"}},
		{rowid: 2, cols: []any{int64(2), "y"}},
		{rowid: 3, cols: []any{int64(3), "z"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "dc", colList: "a,d", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), int64(10)}},
		{rowid: 2, cols: []any{int64(2), int64(20)}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "rc", colList: "a,d", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "p"}},
		{rowid: 2, cols: []any{int64(2), "q"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "ac", colList: "a,b,c,d", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "z", nil, "ok"}},
		{rowid: 2, cols: []any{int64(2), "z", nil, "ok"}},
	}})
}

// TestAlterTableViewDeclineIsUnsupported used to assert that RENAME TO,
// RENAME COLUMN and DROP COLUMN all declined outright against a table a view
// references. TWO of the three now SERVE it -- this write path rewrites the
// dependent view's stored SQL the way C SQLite does -- so this asserts the
// answer instead, byte-for-byte against what the oracle stores. Measured
// against 3.53.3 over t1(a INTEGER, b TEXT) with "CREATE VIEW v_vt AS SELECT
// a, b FROM vt":
//
//	ALTER TABLE vt RENAME TO vt2
//	  vt2  -> CREATE TABLE "vt2"(a INTEGER, b TEXT)
//	  v_vt -> CREATE VIEW v_vt AS SELECT a, b FROM "vt2"
//	ALTER TABLE vt2 RENAME COLUMN a TO z
//	  vt2  -> CREATE TABLE "vt2"(z INTEGER, b TEXT)
//	  v_vt -> CREATE VIEW v_vt AS SELECT z, b FROM "vt2"
//
// Note the quoting asymmetry, which is the oracle's and is why this compares
// TEXT rather than acceptance: a renamed TABLE is force-quoted in the view's
// FROM clause, a renamed COLUMN is rendered bare.
//
// DROP COLUMN still declines, and so does the oracle ("error in view v_vt
// after drop column: no such column: b"), so that half is unchanged.
func TestAlterTableViewDeclineIsUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alter_view_decline.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE vt(a INTEGER, b TEXT)`,
		`INSERT INTO vt VALUES(1,'x')`,
		`CREATE VIEW v_vt AS SELECT a, b FROM vt`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}

	// The two that now SERVE it, each checked on the STORED TEXT of both
	// objects -- the quoting is part of the answer.
	for _, step := range []struct {
		sql      string
		tableSQL string
		viewSQL  string
	}{
		{`ALTER TABLE vt RENAME TO vt2`,
			`CREATE TABLE "vt2"(a INTEGER, b TEXT)`,
			`CREATE VIEW v_vt AS SELECT a, b FROM "vt2"`},
		{`ALTER TABLE vt2 RENAME COLUMN a TO z`,
			`CREATE TABLE "vt2"(z INTEGER, b TEXT)`,
			`CREATE VIEW v_vt AS SELECT z, b FROM "vt2"`},
	} {
		if err := db.Exec(step.sql); err != nil {
			t.Fatalf("Exec(%s): %v", step.sql, err)
		}
		if got := schemaSQLOf(t, db, "vt2"); got != step.tableSQL {
			t.Fatalf("after %s: table sql = %q, want %q", step.sql, got, step.tableSQL)
		}
		if got := schemaSQLOf(t, db, "v_vt"); got != step.viewSQL {
			t.Fatalf("after %s: view sql = %q, want %q", step.sql, got, step.viewSQL)
		}
	}

	// DROP COLUMN is the one that still declines -- and so does the oracle,
	// which reports "error in view v_vt after drop column: no such column: b".
	if err := db.Exec(`ALTER TABLE vt2 DROP COLUMN b`); err == nil {
		t.Fatal("ALTER TABLE vt2 DROP COLUMN b: expected a decline (a view references the column)")
	} else if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("ALTER TABLE vt2 DROP COLUMN b: expected an \"unsupported\" decline, got: %v", err)
	}

	// ADD COLUMN, by contrast, is unaffected by a dependent view (see
	// package doc comment) -- must still succeed.
	if err := db.Exec(`ALTER TABLE vt2 ADD COLUMN c TEXT`); err != nil {
		t.Fatalf("ALTER TABLE vt2 ADD COLUMN c TEXT: %v", err)
	}

	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	cols, rows, qerr := pager.Query(`SELECT z, b, c FROM vt2 ORDER BY z`)
	if qerr != nil {
		t.Fatalf("Query(vt2): %v", qerr)
	}
	if len(cols) != 3 || len(rows) != 1 {
		t.Fatalf("vt2: got cols=%v rows=%v, want 3 cols and 1 row", cols, rows)
	}
	if rows[0][0].I != 1 || string(rows[0][1].S) != "x" || rows[0][2].Typ != engine.Null {
		t.Fatalf("vt2: unexpected row content: %+v", rows[0])
	}

	// The rewritten view must still RESOLVE -- the point of rewriting it -- and
	// answer through the renamed table and column.
	vcols, vrows, verr := pager.Query(`SELECT z, b FROM v_vt ORDER BY z`)
	if verr != nil {
		t.Fatalf("view v_vt must resolve against the renamed table: %v", verr)
	}
	if len(vcols) != 2 || len(vrows) != 1 {
		t.Fatalf("v_vt: got cols=%v rows=%v, want 2 cols and 1 row", vcols, vrows)
	}
}

// schemaSQLOf reads one object's stored CREATE text back out of the live
// schema, which is what makes the rename assertions above about the TEXT --
// quoting included -- rather than about acceptance.
func schemaSQLOf(t *testing.T, db *engine.Session, name string) string {
	t.Helper()
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	rows, serr := pager.Schema()
	if serr != nil {
		t.Fatalf("Schema: %v", serr)
	}
	for i := range rows {
		if strings.EqualFold(rows[i].Name, name) {
			return rows[i].SQL
		}
	}
	t.Fatalf("no schema row named %q", name)
	return ""
}
