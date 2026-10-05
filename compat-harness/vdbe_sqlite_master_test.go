// This file tests sqlite_master queries against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// sqliteMasterSchemaDDL is the schema used for testing sqlite_master queries.
var sqliteMasterSchemaDDL = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)`,
	`CREATE UNIQUE INDEX idx1 ON t1(b)`,
	`CREATE TABLE t2(x TEXT, y REAL)`,
	`CREATE INDEX idx2 ON t2(x)`,
	`CREATE VIEW v1 AS SELECT a, b FROM t1`,
}

// buildSQLiteMasterFixture creates matching pure-Go and real-SQLite databases.
func buildSQLiteMasterFixture(t *testing.T, pageSize int) (godb *engine.Session, sdb *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("sqlite_master_%d.sqlite", pageSize))
	godb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { godb.Close() })

	sdb = openCSQLite(t, ":memory:")

	for _, stmt := range sqliteMasterSchemaDDL {
		if _, _, err := godb.ExecArgs(stmt, nil); err != nil {
			t.Fatalf("engine ExecArgs(%s): %v", stmt, err)
		}
		if _, err := sdb.Exec(stmt); err != nil {
			t.Fatalf("C SQLite Exec(%s): %v", stmt, err)
		}
	}
	return godb, sdb
}

// requireSQLiteMasterQueryMatches runs stmt against both godb (a fresh
// SnapshotPager + QueryArgs, exactly like tclSafeGoQuery/TestTCLCorpus) and
// sdb (C SQLite, via tclRunCGOQuery), requiring identical columns and
// identical rows, IN ORDER (every stmt this file uses carries its own
// ORDER BY, or is order-insensitive by construction, e.g. count(*)).
func requireSQLiteMasterQueryMatches(t *testing.T, label string, godb *engine.Session, sdb *sql.DB, stmt string) {
	t.Helper()
	gotCols, gotRows, err, panicked, panicVal := tclSafeGoQuery(godb, stmt)
	if panicked {
		t.Fatalf("%s: engine panicked on %q: %v", label, stmt, panicVal)
	}
	if err != nil {
		t.Fatalf("%s: engine QueryArgs(%q): %v", label, stmt, err)
	}
	wantCols, wantRows, err := tclRunCGOQuery(sdb, stmt)
	if err != nil {
		t.Fatalf("%s: C SQLite Query(%q): %v", label, stmt, err)
	}
	if len(gotCols) != len(wantCols) {
		t.Fatalf("%s: %q: column count: engine=%d %v, real=%d %v", label, stmt, len(gotCols), gotCols, len(wantCols), wantCols)
	}
	for i := range wantCols {
		if gotCols[i] != wantCols[i] {
			t.Fatalf("%s: %q: column %d name: engine=%q, real=%q", label, stmt, i, gotCols[i], wantCols[i])
		}
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("%s: %q: row count: engine=%d %v, real=%d %v", label, stmt, len(gotRows), gotRows, len(wantRows), wantRows)
	}
	for i := range wantRows {
		if len(gotRows[i]) != len(wantRows[i]) {
			t.Fatalf("%s: %q: row %d width: engine=%v, real=%v", label, stmt, i, gotRows[i], wantRows[i])
		}
		for j := range wantRows[i] {
			if gotRows[i][j] != wantRows[i][j] {
				t.Fatalf("%s: %q: row %d col %d: engine=%q, real=%q\n  engine full row: %v\n  real   full row: %v",
					label, stmt, i, j, gotRows[i][j], wantRows[i][j], gotRows[i], wantRows[i])
			}
		}
	}
}

// TestSQLiteMasterMatchesCSQLite is the primary gate: for both a small
// (512) and large (4096) page size, "SELECT type,name,tbl_name,sql FROM
// sqlite_master" (unfiltered, and again WHERE type='table' ORDER BY name),
// plus "SELECT count(*) FROM sqlite_master" and the "sqlite_schema" alias,
// must come back byte-identical to C SQLite over the shared
// table+index+view schema (sqliteMasterSchemaDDL).
func TestSQLiteMasterMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testSQLiteMasterScenario(t, pageSize)
		})
	}
}

func testSQLiteMasterScenario(t *testing.T, pageSize int) {
	godb, sdb := buildSQLiteMasterFixture(t, pageSize)

	for _, tc := range []struct {
		label string
		stmt  string
	}{
		{
			"unfiltered, ordered by type/name",
			`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type, name`,
		},
		{
			"WHERE type='table', ordered by name",
			`SELECT type,name,tbl_name,sql FROM sqlite_master WHERE type='table' ORDER BY name`,
		},
		{
			"WHERE type='index', ordered by name",
			`SELECT type,name,tbl_name,sql FROM sqlite_master WHERE type='index' ORDER BY name`,
		},
		{
			"WHERE type='view', ordered by name",
			`SELECT type,name,tbl_name,sql FROM sqlite_master WHERE type='view' ORDER BY name`,
		},
		{
			"count(*)",
			`SELECT count(*) FROM sqlite_master`,
		},
		{
			"count(*) WHERE type='table'",
			`SELECT count(*) FROM sqlite_master WHERE type='table'`,
		},
		{
			"sqlite_schema alias, unfiltered",
			`SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type, name`,
		},
		{
			"sqlite_schema alias, count(*)",
			`SELECT count(*) FROM sqlite_schema`,
		},
		{
			"name only, no sql column, ORDER BY name",
			`SELECT name FROM sqlite_master ORDER BY name`,
		},
		{
			"tbl_name grouping sanity: index rows' owning table",
			`SELECT name, tbl_name FROM sqlite_master WHERE type='index' ORDER BY name`,
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			requireSQLiteMasterQueryMatches(t, tc.label, godb, sdb, tc.stmt)
		})
	}
}

// TestSQLiteMasterRootpageMatchesCSQLite pins queries that read
// sqlite_master (or sqlite_schema)'s "rootpage" column, explicitly or through
// "*". They used to be declined outright, because this writer rebuilds its
// page allocation on flush. Root page numbering is now reproduced, and a
// history where it diverges is flagged and declined
// (DB.wsRootpageNumberingDiverged, engine/rootpage_tail.go), so each query here
// may still decline, but an answer must equal C SQLite's.
func TestSQLiteMasterRootpageMatchesCSQLite(t *testing.T) {
	godb, sdb := buildSQLiteMasterFixture(t, 4096)
	for _, stmt := range []string{
		`SELECT rootpage FROM sqlite_master`,
		`SELECT rootpage, tbl_name FROM sqlite_master`,
		`SELECT * FROM sqlite_master`,
		`SELECT * FROM sqlite_schema WHERE type='table'`,
		`SELECT tbl_name FROM sqlite_master ORDER BY rootpage`,
		`SELECT name FROM sqlite_master WHERE rootpage > 0`,
	} {
		_, _, err, panicked, panicVal := tclSafeGoQuery(godb, stmt)
		if panicked {
			t.Fatalf("%q: engine panicked: %v", stmt, panicVal)
		}
		if err != nil {
			t.Logf("%q: declined: %v", stmt, err)
			continue
		}
		requireSQLiteMasterQueryMatches(t, "rootpage", godb, sdb, stmt)
	}
}

// TestSQLiteMasterServesTriggerSQL asserts that a TRIGGER's stored "sql" reads
// back byte-identically to C SQLite's while the trigger is live.
//
// It used to assert the OPPOSITE -- that the read was DECLINED -- because this
// write path's ALTER TABLE RENAME COLUMN/TO had no rewrite-or-decline guard for
// a dependent TRIGGER body the way it does for a dependent VIEW, so a trigger's
// stored SQL could go stale after a rename. Round 34 closed that: the cascade
// rewrites the body, and every position it cannot rewrite exactly declines the
// ALTER instead (engine/alter_write.go's r34vBoundNameToken /
// r34vDerivedRebindsColumn), so schemaCatalogQueryGuard's trigger clause was
// retired. The whole cascade is compared against the oracle by
// compat-harness/alter_r34v_trigger_cascade_test.go.
//
// "SELECT *" stays declined, but for the OTHER, unrelated rule: "*" includes
// ROOTPAGE, which this writer's rebuild-on-flush page allocation cannot
// reproduce once DROP/CREATE churn has happened.
func TestSQLiteMasterServesTriggerSQL(t *testing.T) {
	godb, sdb := buildSQLiteMasterFixture(t, 4096)
	for _, stmt := range []string{
		`CREATE TABLE tlog(what TEXT)`,
		`CREATE TRIGGER trg1 AFTER INSERT ON t1 BEGIN INSERT INTO tlog VALUES('ins'); END`,
	} {
		if _, _, err := godb.ExecArgs(stmt, nil); err != nil {
			t.Fatalf("engine ExecArgs(%s): %v", stmt, err)
		}
		if _, err := sdb.Exec(stmt); err != nil {
			t.Fatalf("C SQLite Exec(%s): %v", stmt, err)
		}
	}

	// sql-column reads: served, and byte-identical to the oracle's.
	for _, stmt := range []string{
		`SELECT sql FROM sqlite_master WHERE name='trg1'`,
		`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name`,
		`SELECT type,name,tbl_name FROM sqlite_master WHERE type='trigger'`,
	} {
		requireSQLiteMasterQueryMatches(t, "trigger present", godb, sdb, stmt)
	}

	// "*" is served too now, rootpage included: the page store allocates root
	// pages through btree.c's own allocator, so the column reads back real
	// SQLite's own number (a trigger's is 0 in both).
	requireSQLiteMasterQueryMatches(t, "trigger present", godb, sdb, `SELECT * FROM sqlite_master WHERE type='trigger'`)
	requireSQLiteMasterQueryMatches(t, "trigger present", godb, sdb, `SELECT * FROM sqlite_master ORDER BY rowid`)
}

// TestSQLiteMasterFiltersTempRows asserts that once the live schema contains
// an object created via "CREATE TEMP|TEMPORARY ...", "FROM sqlite_master"/
// "sqlite_schema" EXCLUDES it -- exactly as C SQLite's genuinely separate
// temp schema does -- and sqlite_temp_master reports it and nothing else.
//
// This write path stores a TEMP object in the very same schema b-tree as an
// ordinary one (schema_write.go/trigger.go, which keep the TEMP keyword as the
// on-disk marker), so neither catalog can be answered by SCANNING that b-tree.
// It used to decline both rather than risk showing a temp row under main's
// name; engine/temp_catalog.go now FILTERS instead -- one rule, two directions
// -- which is what this asserts. Verified directly against mattn/go-sqlite3
// (schema4.test's own mined repro: a real sqlite_master lists ONLY non-temp
// objects).
func TestSQLiteMasterFiltersTempRows(t *testing.T) {
	godb, sdb := buildSQLiteMasterFixture(t, 4096)
	// A temp object belongs to the connection that made it, so the real-SQLite
	// side has to stay on ONE connection for the rest of this test.
	sdb.SetMaxOpenConns(1)
	temp := `CREATE TEMP TABLE tmp1(z)`
	if _, _, err := godb.ExecArgs(temp, nil); err != nil {
		t.Fatalf("engine ExecArgs(%s): %v", temp, err)
	}
	if _, err := sdb.Exec(temp); err != nil {
		t.Fatalf("C SQLite Exec(%s): %v", temp, err)
	}

	for _, stmt := range []string{
		`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY name`,
		`SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY name`,
		`SELECT count(*) FROM sqlite_schema`,
		`SELECT name FROM sqlite_master WHERE name='tmp1'`,
		// The other direction of the same filter.
		`SELECT type,name,tbl_name,sql FROM sqlite_temp_master ORDER BY name`,
		`SELECT count(*) FROM sqlite_temp_master`,
	} {
		requireSQLiteMasterQueryMatches(t, "temp object live", godb, sdb, stmt)
	}
}

// TestCTASFromCatalogSeesItsOwnPlaceholder is SQLite ticket
// acd12990885d9276 (table.test 19.1): "CREATE TABLE t19 AS SELECT * FROM
// sqlite_master" reads a catalog that ALREADY holds t19's own row, populated
// with five NULLs -- sqlite3StartTable reserves it (build.c:1370-1375) before
// the SELECT runs, and sqlite3EndTable's UPDATE fills it in afterwards
// (build.c:2904). So the copy has one more row than the schema had objects,
// and that row is all NULL.
//
// This engine reads main's catalog straight off its b-tree (resolveTableIn,
// query.go) whenever no temp object forces the filtered row source, and the
// reserved row is not in the b-tree yet -- so the in-flight CTAS is what
// selects the filtered source instead (schemaCatalogSourceScope), which is
// where the placeholder is added. Mutation-checked: dropping that one
// condition puts the row count back one short.
//
// The second half is the same read with a TEMP object present, which takes the
// filtered source for its own reason -- the placeholder must appear exactly
// once there too, not twice.
func TestCTASFromCatalogSeesItsOwnPlaceholder(t *testing.T) {
	flLockstep(t, "ctas-from-catalog", []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(b UNIQUE)`,
		`CREATE INDEX t1a ON t1(a)`,
		`CREATE TABLE t19 AS SELECT * FROM sqlite_master`,
	},
		`SELECT count(*) FROM t19`,
		`SELECT type, name, tbl_name, rootpage FROM t19 ORDER BY name, type`,
		`SELECT count(*) FROM t19 WHERE name IS NULL`,
		`SELECT count(*) FROM sqlite_master`,
	)
	// "SELECT *" is not available in this half: rootpage rides along with it and
	// a TEMP object's own rootpage is its temp DATABASE's page number, which
	// this engine declines (schemaCatalogQueryGuard). The columns that ARE
	// servable answer the same question -- is the reserved row there, once.
	flLockstep(t, "ctas-from-catalog-with-temp", []string{
		`CREATE TABLE m1(a)`,
		`CREATE TEMP TABLE tt(z)`,
		`CREATE TABLE m19 AS SELECT type, name FROM sqlite_master`,
	},
		`SELECT count(*) FROM m19`,
		`SELECT count(*) FROM m19 WHERE name IS NULL`,
		`SELECT type, name FROM m19 ORDER BY name`,
	)
}
