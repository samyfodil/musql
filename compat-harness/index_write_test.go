// This file tests index creation and query planning with indexes against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// openCgo opens path through C SQLite (mattn/go-sqlite3), closing it
// automatically at test cleanup. This is openCSQLite (write_update_
// delete_verify_test.go) under a locally-descriptive name; requireIntegrityOK
// itself is reused from that same file unchanged.
// openCgo is the CGo oracle over a database THIS ENGINE wrote -- through the
// EXPORT, because the file is a segment file (convert_for_oracle_test.go). Every
// caller passes a path it built with engine.Create, so the conversion is part of
// what each of them now tests.
func openCgo(t *testing.T, path string) *sql.DB {
	return openCSQLite(t, exportedForOracle(t, path))
}

// queryPlan runs "EXPLAIN QUERY PLAN <query>" through C SQLite and
// returns every plan row's "detail" text, newline-joined -- the field that
// contains "SEARCH t USING INDEX idx_name (col=?)" when the index is
// actually used, versus "SCAN t" for a table scan.
func queryPlan(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN %s: %v", query, err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan query plan row: %v", err)
		}
		sb.WriteString(detail)
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// requireUsesIndex fails the test unless query's plan mentions USING INDEX
// idxName (or, when every selected column is already in the index -- e.g.
// just the rowid alias -- USING COVERING INDEX idxName, C SQLite's way
// of saying it didn't even need to visit the table b-tree) -- proof real
// SQLite actually walks that index's b-tree rather than falling back to a
// full table scan.
func requireUsesIndex(t *testing.T, db *sql.DB, query, idxName string) {
	t.Helper()
	plan := queryPlan(t, db, query)
	want := "INDEX " + idxName
	if !strings.Contains(plan, want) || !strings.Contains(plan, "USING") {
		t.Errorf("query %q: plan doesn't show USING [COVERING] %q:\n%s", query, want, plan)
	}
}

// TestIndexBasicLookupUsesIndex is the headline gate: a single-column
// index, at both a small (512, so the index b-tree splits quickly) and
// default-ish (4096) page size, must produce a file C SQLite both
// accepts (integrity_check) and actually USES for an equality lookup.
func TestIndexBasicLookupUsesIndex(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "idx_basic.sqlite")
			edb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT, age INTEGER)")
			const n = 200
			for i := 1; i <= n; i++ {
				wvExec(t, edb, fmt.Sprintf("INSERT INTO t(id,email,age) VALUES(%d,%s,%d)",
					i, wvString(fmt.Sprintf("user%d@example.com", i)), 18+i%50))
			}
			wvExec(t, edb, "CREATE INDEX idx_email ON t(email)")
			if err := edb.Close(); err != nil {
				t.Fatalf("engine writer Close: %v", err)
			}

			db := openCgo(t, path)
			requireIntegrityOK(t, db)

			var age int
			if err := db.QueryRow("SELECT age FROM t WHERE email = ?", "user42@example.com").Scan(&age); err != nil {
				t.Fatalf("SELECT WHERE email=...: %v", err)
			}
			if want := 18 + 42%50; age != want {
				t.Errorf("age = %d, want %d", age, want)
			}
			requireUsesIndex(t, db, "SELECT age FROM t WHERE email = 'user42@example.com'", "idx_email")
		})
	}
}

// TestIndexMultiColumn covers CREATE INDEX ON t(a,b): a two-column
// lookup must both return the right row and use the index.
func TestIndexMultiColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_multi.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b TEXT)")
	const n = 100
	for i := 1; i <= n; i++ {
		wvExec(t, edb, fmt.Sprintf("INSERT INTO t(id,a,b) VALUES(%d,%d,%s)", i, i%5, wvString(fmt.Sprintf("v%d", i))))
	}
	wvExec(t, edb, "CREATE INDEX idx_ab ON t(a,b)")
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	var id int
	if err := db.QueryRow("SELECT id FROM t WHERE a = 2 AND b = 'v7'").Scan(&id); err != nil {
		t.Fatalf("SELECT WHERE a=2 AND b='v7': %v", err)
	}
	if id != 7 {
		t.Errorf("id = %d, want 7", id)
	}
	requireUsesIndex(t, db, "SELECT id FROM t WHERE a = 2 AND b = 'v7'", "idx_ab")
}

// TestIndexNullsAndDuplicates covers a NON-unique index over a column with
// both NULLs and duplicate (non-NULL) keys -- SQLite treats NULL as an
// ordinary, repeatable, sortable value for a non-unique index (only UNIQUE
// indexes give NULL special "always distinct" treatment).
func TestIndexNullsAndDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_nulls.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	vals := []*string{sp("a"), sp("a"), sp("b"), nil, nil, sp("c")}
	for i, v := range vals {
		id := i + 1
		if v == nil {
			wvExec(t, edb, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,NULL)", id))
		} else {
			wvExec(t, edb, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,%s)", id, wvString(*v)))
		}
	}
	wvExec(t, edb, "CREATE INDEX idx_v ON t(v)")
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	rows, err := db.Query("SELECT id FROM t WHERE v = 'a' ORDER BY id")
	if err != nil {
		t.Fatalf("SELECT WHERE v='a': %v", err)
	}
	var gotIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		gotIDs = append(gotIDs, id)
	}
	rows.Close()
	if len(gotIDs) != 2 || gotIDs[0] != 1 || gotIDs[1] != 2 {
		t.Errorf("SELECT WHERE v='a' = %v, want [1 2]", gotIDs)
	}

	var nullCount int
	if err := db.QueryRow("SELECT count(*) FROM t WHERE v IS NULL").Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 2 {
		t.Errorf("count WHERE v IS NULL = %d, want 2", nullCount)
	}
}

func sp(s string) *string { return &s }

// TestIndexUniqueEnforcement covers the UNIQUE-index gate: a duplicate
// non-NULL key is rejected by the Go engine at INSERT time (never even
// reaches the file C SQLite validates), while duplicate NULLs in the
// SAME unique-indexed column are allowed -- matching SQLite's documented
// "NULLs are always distinct" UNIQUE-index rule.
func TestIndexUniqueEnforcement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_unique.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT)")
	wvExec(t, edb, "INSERT INTO t(id,email) VALUES(1,'a@x.com')")
	wvExec(t, edb, "CREATE UNIQUE INDEX idx_email ON t(email)")

	if err := edb.Exec("INSERT INTO t(id,email) VALUES(2,'a@x.com')"); err == nil {
		t.Fatal("expected the pure-Go engine to reject a duplicate UNIQUE-indexed value at INSERT time, got none")
	}
	// Duplicate NULLs in the same UNIQUE-indexed column must be allowed.
	wvExec(t, edb, "INSERT INTO t(id,email) VALUES(3,NULL)")
	wvExec(t, edb, "INSERT INTO t(id,email) VALUES(4,NULL)")
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	var count int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("row count = %d, want 3 (the duplicate-email INSERT must have been rejected)", count)
	}
	requireUsesIndex(t, db, "SELECT id FROM t WHERE email = 'a@x.com'", "idx_email")
}

// TestIndexSurvivesDeleteAndUpdate checks that DELETE/UPDATE against an
// indexed table -- which, per writer.go's rebuild-at-Close model, rebuilds
// BOTH the table's b-tree and every index's b-tree from scratch -- still
// produces a file C SQLite accepts and can still use the index against.
func TestIndexSurvivesDeleteAndUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_delete_update.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)")
	const n = 150
	for i := 1; i <= n; i++ {
		wvExec(t, edb, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,%d)", i, i))
	}
	wvExec(t, edb, "CREATE INDEX idx_v ON t(v)")

	if _, err := edb.Delete("DELETE FROM t WHERE id % 3 = 0"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := edb.Update("UPDATE t SET v = v + 1000 WHERE id % 2 = 0"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	wantCount := 0
	wantUpdatedVal := 0
	for i := 1; i <= n; i++ {
		if i%3 == 0 {
			continue
		}
		wantCount++
		if i%2 == 0 {
			wantUpdatedVal++
		}
	}
	var gotCount int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&gotCount); err != nil {
		t.Fatal(err)
	}
	if gotCount != wantCount {
		t.Errorf("row count after DELETE = %d, want %d", gotCount, wantCount)
	}

	// id=8 survives the delete (8%3 != 0) and is updated (8%2==0) to v=1008.
	var v int
	if err := db.QueryRow("SELECT v FROM t WHERE id = 8").Scan(&v); err != nil {
		t.Fatalf("SELECT id=8: %v", err)
	}
	if v != 1008 {
		t.Errorf("v for id=8 = %d, want 1008", v)
	}
	requireUsesIndex(t, db, "SELECT id FROM t WHERE v = 1008", "idx_v")
}

// TestIndexLargeForcesSplits is the multi-page gate: enough rows, at both a
// small (512) and default-ish (4096) page size, to force the index b-tree
// through leaf splits and grow interior levels (not just a single-leaf
// index) -- integrity_check must still report "ok" and the index must
// still be usable.
func TestIndexLargeForcesSplits(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "idx_large.sqlite")
			edb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, grp INTEGER, name TEXT)")
			const n = 4000
			const nGroups = 37
			for i := 1; i <= n; i++ {
				wvExec(t, edb, fmt.Sprintf("INSERT INTO t(id,grp,name) VALUES(%d,%d,%s)",
					i, i%nGroups, wvString(fmt.Sprintf("row-with-some-padding-text-%d", i))))
			}
			wvExec(t, edb, "CREATE INDEX idx_grp ON t(grp)")
			if err := edb.Close(); err != nil {
				t.Fatalf("engine writer Close: %v", err)
			}

			db := openCgo(t, path)
			requireIntegrityOK(t, db)

			wantCount := 0
			for i := 1; i <= n; i++ {
				if i%nGroups == 5 {
					wantCount++
				}
			}
			var gotCount int
			if err := db.QueryRow("SELECT count(*) FROM t WHERE grp = 5").Scan(&gotCount); err != nil {
				t.Fatal(err)
			}
			if gotCount != wantCount {
				t.Errorf("count WHERE grp=5 = %d, want %d", gotCount, wantCount)
			}
			requireUsesIndex(t, db, "SELECT id FROM t WHERE grp = 5", "idx_grp")

			// Sanity: the index root really did need to grow past a single
			// page for this to be a meaningful multi-page-split gate.
			var idxRootPage int
			if err := db.QueryRow("SELECT rootpage FROM sqlite_schema WHERE name='idx_grp'").Scan(&idxRootPage); err != nil {
				t.Fatal(err)
			}
			var pageCount int
			if err := db.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
				t.Fatal(err)
			}
			if pageCount < 20 {
				t.Errorf("page_count = %d, want a genuinely multi-page database for this scenario", pageCount)
			}
		})
	}
}
