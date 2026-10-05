// Tests index key order (collation and DESC flag) for correctness against
// the oracle, verified by integrity_check.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// sortOrderCases are index shapes to verify for correct key order.
var sortOrderCases = []struct{ name, table, ddl, query string }{
	{"plain-desc", "t(a INTEGER, b INTEGER, c TEXT)",
		`CREATE INDEX i1 ON t(a DESC)`, `SELECT count(*) FROM t WHERE a > 10`},
	{"leading-desc-multi", "t(a INTEGER, b INTEGER, c TEXT)",
		`CREATE INDEX i1 ON t(a DESC, b ASC)`, `SELECT count(*) FROM t WHERE a > 10`},
	{"trailing-desc", "t(a INTEGER, b INTEGER, c TEXT)",
		`CREATE INDEX i1 ON t(b ASC, a DESC)`, `SELECT count(*) FROM t WHERE b = 3`},
	{"expr-desc", "t(a INTEGER, b INTEGER, c TEXT)",
		`CREATE INDEX i1 ON t(a+b DESC)`, `SELECT count(*) FROM t WHERE a+b > 10`},
	{"partial-desc", "t(a INTEGER, b INTEGER, c TEXT)",
		`CREATE INDEX i1 ON t(a DESC) WHERE b > 2`, `SELECT count(*) FROM t WHERE b > 2 AND a > 10`},
	{"collate-desc", "t(a INTEGER, b INTEGER, c TEXT)",
		`CREATE INDEX i1 ON t(c COLLATE NOCASE DESC)`, `SELECT count(*) FROM t WHERE c > 'v5'`},
	{"nocase-asc", "t(a INTEGER, b INTEGER, c TEXT)",
		`CREATE INDEX i1 ON t(c COLLATE NOCASE)`, `SELECT count(*) FROM t WHERE c = 'V4' COLLATE NOCASE`},
	{"auto-unique-desc", "t(a INTEGER, b INTEGER, c TEXT, UNIQUE(a DESC))",
		``, `SELECT count(*) FROM t WHERE a > 10`},
	{"auto-pk-desc", "t(a INTEGER, b INTEGER, c TEXT, PRIMARY KEY(a DESC))",
		``, `SELECT count(*) FROM t WHERE a > 10`},
}

// sortOrderRows returns the 60-row fixture for test cases.
func sortOrderRows() []string {
	out := make([]string, 0, 60)
	for i := 1; i <= 60; i++ {
		s := fmt.Sprintf("v%d", i)
		if i%2 == 0 {
			s = fmt.Sprintf("V%d", i)
		}
		out = append(out, fmt.Sprintf("INSERT INTO t VALUES(%d,%d,%s)", i, i%7, wvString(s)))
	}
	return out
}

// TestIndexSortOrderInterchange builds each shape three ways -- written fresh,
// rebuilt after a reopen, and appended to after a reopen (the incremental
// commit path) -- and at every stage requires C SQLite to accept the file
// (integrity_check) and to answer the query exactly as a NATIVE C SQLite
// database built from the identical statements does. Deriving the expected
// answer from cgo rather than by hand is the point: it is the same oracle the
// rest of this package uses, and it cannot drift as the fixture changes.
func TestIndexSortOrderInterchange(t *testing.T) {
	for _, tc := range sortOrderCases {
		t.Run(tc.name, func(t *testing.T) {
			setup := append([]string{"CREATE TABLE " + tc.table}, sortOrderRows()...)
			if tc.ddl != "" {
				setup = append(setup, tc.ddl)
			}
			const (
				rebuild = `DELETE FROM t WHERE a = 1`        // full rebuild
				append_ = `INSERT INTO t VALUES(61,5,'v61')` // append
			)

			path := filepath.Join(t.TempDir(), "order.sqlite")
			edb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			for _, q := range setup {
				wvExec(t, edb, q)
			}
			if err := edb.Close(); err != nil {
				t.Fatalf("engine writer Close: %v", err)
			}
			requireOrderOK(t, path, tc.query, cgoAnswer(t, setup, tc.query), "fresh")

			for _, stage := range []struct{ name, stmt string }{{"rebuilt", rebuild}, {"appended", append_}} {
				edb, err := engine.OpenWrite(path)
				if err != nil {
					t.Fatalf("[%s] engine.OpenWrite: %v", stage.name, err)
				}
				wvExec(t, edb, stage.stmt)
				if err := edb.Close(); err != nil {
					t.Fatalf("[%s] engine writer Close: %v", stage.name, err)
				}
				setup = append(setup, stage.stmt)
				requireOrderOK(t, path, tc.query, cgoAnswer(t, setup, tc.query), stage.name)
			}
		})
	}
}

// cgoAnswer builds a NATIVE real-SQLite database from steps and returns its
// answer to query -- the expected value the engine-written file must match.
func cgoAnswer(t *testing.T, steps []string, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "native.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range steps {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("cgo setup %s: %v", q, err)
		}
	}
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("cgo %s: %v", query, err)
	}
	return n
}

// requireOrderOK hands path to C SQLite and requires integrity_check "ok"
// plus want for query.
func requireOrderOK(t *testing.T, path, query string, want int, stage string) {
	t.Helper()
	db := openCgo(t, path)
	var ic string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&ic); err != nil {
		t.Fatalf("[%s] integrity_check: %v", stage, err)
	}
	if ic != "ok" {
		t.Errorf("[%s] C SQLite integrity_check = %q, want \"ok\" -- the index b-tree is not in the order C SQLite expects", stage, ic)
	}
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("[%s] %s: %v", stage, query, err)
	}
	if got != want {
		t.Errorf("[%s] %s = %d, want %d (C SQLite answered through the index this engine wrote)", stage, query, got, want)
	}
}

// TestIndexSortOrderPragmaParity pins that PRAGMA index_xinfo reports each key
// column's DESC flag the way C SQLite does -- including for the automatic
// index of a DESC-ordered UNIQUE/PRIMARY KEY constraint, which has no
// sqlite_schema SQL text of its own to read it from.
func TestIndexSortOrderPragmaParity(t *testing.T) {
	driverParity(t, "index-desc-pragma", []string{
		`CREATE TABLE t(a, b, c, UNIQUE(a DESC, b))`,
		`CREATE TABLE u(a, b, PRIMARY KEY(a DESC))`,
		`CREATE INDEX i1 ON t(c DESC)`,
		`CREATE INDEX i2 ON t(b ASC, c DESC)`,
		`PRAGMA index_xinfo(sqlite_autoindex_t_1)`,
		`PRAGMA index_xinfo(sqlite_autoindex_u_1)`,
		`PRAGMA index_xinfo(i1)`,
		`PRAGMA index_xinfo(i2)`,
		`PRAGMA index_info(i2)`,
		// A constraint whose index would duplicate an earlier one collapses
		// into it, and the SURVIVOR keeps the FIRST constraint's sort order --
		// verified directly: "UNIQUE(a,b), PRIMARY KEY(a DESC,b)" reports
		// desc=0 for a while "PRIMARY KEY(a DESC,b), UNIQUE(a,b)" reports 1
		// (origin is 'pk' either way).
		`CREATE TABLE c1(a, b, UNIQUE(a,b), PRIMARY KEY(a DESC,b))`,
		`CREATE TABLE c2(a, b, PRIMARY KEY(a DESC,b), UNIQUE(a,b))`,
		`PRAGMA index_list(c1)`,
		`PRAGMA index_xinfo(sqlite_autoindex_c1_1)`,
		`PRAGMA index_list(c2)`,
		`PRAGMA index_xinfo(sqlite_autoindex_c2_1)`,
	})
}
