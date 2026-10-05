package compat

// Derived table and parenthesized FROM tests with correctness parity vs C SQLite.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEDerivedDB creates test tables for derived table tests.
func buildVDBEDerivedDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_derived.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	exec := func(sqlText string) {
		t.Helper()
		if err := db.Exec(sqlText); err != nil {
			t.Fatalf("Exec(%s): %v", sqlText, err)
		}
	}

	exec(`CREATE TABLE t1 (id INTEGER PRIMARY KEY, a INTEGER, name TEXT)`)
	exec(`INSERT INTO t1 VALUES (1, 10, 'alice')`)
	exec(`INSERT INTO t1 VALUES (2, 20, 'bob')`)
	exec(`INSERT INTO t1 VALUES (3, 20, 'carol')`)  // duplicate a=20
	exec(`INSERT INTO t1 VALUES (4, NULL, 'dave')`) // NULL key
	exec(`INSERT INTO t1 VALUES (5, 30, 'eve')`)

	exec(`CREATE TABLE t2 (id INTEGER PRIMARY KEY, a INTEGER, val TEXT)`)
	exec(`INSERT INTO t2 VALUES (1, 10, 'x')`)
	exec(`INSERT INTO t2 VALUES (2, 20, 'y')`)
	exec(`INSERT INTO t2 VALUES (3, 40, 'z')`)

	exec(`CREATE TABLE t3 (id INTEGER PRIMARY KEY, a INTEGER, tag TEXT)`)
	exec(`INSERT INTO t3 VALUES (1, 10, 'red')`)
	exec(`INSERT INTO t3 VALUES (2, 20, 'blue')`)

	exec(`CREATE TABLE empty_t (id INTEGER PRIMARY KEY, a INTEGER)`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// derivedCase holds a test statement and its expected behavior.
type derivedCase struct {
	sql     string
	ordered bool
	native  bool
}

var vdbeDerivedCorpus = []derivedCase{
	// --- basic: SELECT * FROM (SELECT ...) x ---
	{"SELECT * FROM (SELECT id, a, name FROM t1) x", false, true},
	{"SELECT * FROM (SELECT id, a, name FROM t1 WHERE a = 20) x ORDER BY id", true, true},

	// --- derived table with WHERE / ORDER BY / LIMIT / aggregate INSIDE ---
	{"SELECT id FROM (SELECT id FROM t1 ORDER BY id LIMIT 3) x", false, true},
	{"SELECT * FROM (SELECT a, count(*) AS n FROM t1 GROUP BY a) x ORDER BY x.a", true, true},
	{"SELECT * FROM (SELECT count(*) AS n, sum(a) AS s FROM t1) x", false, true},

	// --- outer query filtering / ordering / aggregating the derived table ---
	{"SELECT name FROM (SELECT id, a, name FROM t1) x WHERE x.a = 20 ORDER BY name", true, true},
	{"SELECT count(*) FROM (SELECT a FROM t1 WHERE a IS NOT NULL) x", false, true},
	{"SELECT x.a, count(*) FROM (SELECT a FROM t1) x GROUP BY x.a ORDER BY x.a", true, true},
	{"SELECT sum(c) FROM (SELECT a AS c FROM t1) x", false, true},

	// --- derived JOINed to a real table, and to another derived table ---
	{"SELECT x.name, t2.val FROM (SELECT id, a, name FROM t1) x JOIN t2 ON x.a = t2.a ORDER BY x.name, t2.val", true, true},
	{"SELECT x.name, y.val FROM (SELECT a, name FROM t1) x JOIN (SELECT a, val FROM t2) y ON x.a = y.a ORDER BY x.name, y.val", true, true},
	{"SELECT x.name, t2.val FROM (SELECT id, a, name FROM t1 WHERE a IS NOT NULL) x LEFT JOIN t2 ON x.a = t2.a ORDER BY x.name, t2.val", true, true},

	// --- renaming columns inside the subquery (SQLite has no FROM-clause
	//     column-alias list -- "(SELECT ...) x(p,q)" is a syntax error; see
	//     TestVDBEDerivedColumnAliasListRejected) ---
	{"SELECT p, q FROM (SELECT a AS p, name AS q FROM t1) x WHERE p = 20 ORDER BY q", true, true},
	{"SELECT x.p FROM (SELECT a AS p, name AS q FROM t1) x ORDER BY x.p", true, true},

	// --- x.* and x.col and unqualified col resolution ---
	{"SELECT x.* FROM (SELECT id, name FROM t1) x ORDER BY x.id", true, true},
	{"SELECT * FROM (SELECT id, name FROM t1) x WHERE name = 'bob'", false, true},
	{"SELECT x.id, name FROM (SELECT id, name FROM t1) x ORDER BY id", true, true},

	// --- default column naming (computed expression keeps its source text) ---
	{"SELECT * FROM (SELECT a+1 FROM t1 WHERE id = 1) x", false, true},

	// --- nested derived tables ---
	{"SELECT * FROM (SELECT * FROM (SELECT id, a FROM t1) y WHERE y.a = 20) x ORDER BY x.id", true, true},

	// --- parenthesized join (t1 JOIN t2 ON ..) and trivial (t1) ---
	{"SELECT t1.name, t2.val FROM (t1 JOIN t2 ON t1.a = t2.a) ORDER BY t1.name, t2.val", true, true},
	{"SELECT t1.name, t2.val FROM (t1 JOIN t2 ON t1.a = t2.a) WHERE t2.val = 'y'", false, true},
	{"SELECT name FROM (t1) WHERE a = 20 ORDER BY name", true, true},
	{"SELECT t1.name, t3.tag FROM (t1 JOIN t2 ON t1.a = t2.a) JOIN t3 ON t1.a = t3.a ORDER BY t1.name, t3.tag", true, true},

	// --- derived over a compound / aggregate subquery ---
	// A compound-body derived table is declined by the VDBE compiler (compound
	// SELECT), so native=false (see derivedCase's doc comment: with no fallback
	// anywhere, these hard-error on the default path too).
	{"SELECT * FROM (SELECT a FROM t1 UNION SELECT a FROM t2) x ORDER BY x.a", true, false},
	{"SELECT count(*) FROM (SELECT a FROM t1 INTERSECT SELECT a FROM t2) x", false, false},

	// --- empty derived table ---
	{"SELECT * FROM (SELECT id, a FROM empty_t) x", false, true},
	{"SELECT count(*) FROM (SELECT * FROM empty_t) x", false, true},
	{"SELECT x.a FROM (SELECT a FROM empty_t) x JOIN t1 ON x.a = t1.a", false, true},
}

// TestVDBEDerivedResultParity is the derived-table / parenthesized-FROM gate:
// every corpus statement must produce identical results through C SQLite
// and the VDBE (natively via QueryVDBE for native cases, and via the
// default-mode QueryArgs path for every case).
func TestVDBEDerivedResultParity(t *testing.T) {
	path := buildVDBEDerivedDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong := 0
	total := 0
	for _, tc := range vdbeDerivedCorpus {
		total++
		sqlText := tc.sql

		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		// The default engine path: native VDBE execution.
		dCols, dVals, dErr := p.QueryArgs(sqlText, nil)

		if cErr != nil || dErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error\n  cgo=%v\n  default=%v", sqlText, cErr, dErr)
			continue
		}

		dRows := engineRowsToStrings(dVals)

		if ok, reason := queryResultsMatch(dCols, dRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] default engine DIVERGES from C SQLite: %s\n  default: cols=%v rows=%v\n  cgo:     cols=%v rows=%v",
				sqlText, reason, dCols, dRows, cCols, cRows)
		}

		if tc.native {
			vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
			if vErr != nil {
				wrong++
				t.Errorf("[%s] QueryVDBE (expected to compile+run natively): %v", sqlText, vErr)
				continue
			}
			vRows := engineRowsToStrings(vVals)
			if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
				wrong++
				t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
					sqlText, reason, vCols, vRows, cCols, cRows)
			}
		}
	}
	t.Logf("VDBE derived-table result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE derived-table parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEDerivedDualModeIntegration exercises the VDBEMode=dual integration
// path through the ordinary QueryArgs entry point for the whole derived-table
// corpus, confirming every case runs cleanly and restores VDBEMode afterward.
func TestVDBEDerivedDualModeIntegration(t *testing.T) {
	path := buildVDBEDerivedDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeDerivedCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] VDBEMode=dual: %v", tc.sql, err)
		}
	}
}

// TestVDBEDerivedRowidSuppressed checks that a derived table exposes NO implicit
// rowid/oid/_rowid_ pseudo-column (unlike a base table): a reference to one must
// error consistently across C SQLite and the VDBE path, rather than
// silently resolving to a (meaningless) value.
func TestVDBEDerivedRowidSuppressed(t *testing.T) {
	path := buildVDBEDerivedDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		"SELECT rowid FROM (SELECT id FROM t1) x",
		"SELECT x.rowid FROM (SELECT id FROM t1) x",
	} {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Fatalf("[%s] expected C SQLite to reject a derived-table rowid, got nil", sqlText)
		}
		if _, _, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
			t.Errorf("[%s] expected the VDBE to reject a derived-table rowid, got nil", sqlText)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] expected the default engine to reject a derived-table rowid, got nil", sqlText)
		}
	}
}

// TestVDBEDerivedColumnAliasListRejected pins that a FROM-clause column-alias
// list -- "(SELECT ...) x(c1, c2)" -- is rejected as a syntax error, exactly
// like C SQLite 3.53.3 (which has no such grammar: columns are renamed
// inside the subquery via "SELECT expr AS name" instead). Accepting it would be
// a divergence FROM C SQLite, not a superset -- so both must reject it.
func TestVDBEDerivedColumnAliasListRejected(t *testing.T) {
	path := buildVDBEDerivedDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const sqlText = "SELECT p, q FROM (SELECT a, name FROM t1) x(p, q)"
	if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
		t.Fatalf("[%s] expected C SQLite to reject a FROM-clause column-alias list, got nil", sqlText)
	}
	if _, _, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
		t.Errorf("[%s] expected the VDBE to reject a FROM-clause column-alias list, got nil", sqlText)
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Errorf("[%s] expected the default engine to reject a FROM-clause column-alias list, got nil", sqlText)
	}
}
