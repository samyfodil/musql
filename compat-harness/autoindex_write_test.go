// This file gates automatic-index support in CREATE TABLE: inline UNIQUE/PRIMARY
// KEY and table-level constraints must produce sqlite_autoindex_<table>_<N>
// b-trees, reject duplicates at INSERT time, and write NULL sql to sqlite_schema.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// autoIdxSchemaRow is one sqlite_schema row of type='index' for a given
// table, as read back through C SQLite: name plus whether sql is NULL
// (Valid == false) or holds actual CREATE INDEX text (Valid == true, an
// explicit index).
type autoIdxSchemaRow struct {
	name string
	sql  sql.NullString
}

// fetchIndexSchemaRows reads every sqlite_schema row of type='index' for
// tblName, ordered by name, through C SQLite.
func fetchIndexSchemaRows(t *testing.T, db *sql.DB, tblName string) []autoIdxSchemaRow {
	t.Helper()
	rows, err := db.Query("SELECT name, sql FROM sqlite_schema WHERE type='index' AND tbl_name=? ORDER BY name", tblName)
	if err != nil {
		t.Fatalf("SELECT sqlite_schema index rows for %s: %v", tblName, err)
	}
	defer rows.Close()
	var out []autoIdxSchemaRow
	for rows.Next() {
		var r autoIdxSchemaRow
		if err := rows.Scan(&r.name, &r.sql); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// requireAutoIndexRow fails the test unless idxRows contains exactly one
// row named name with sql IS NULL.
func requireAutoIndexRow(t *testing.T, idxRows []autoIdxSchemaRow, name string) {
	t.Helper()
	for _, r := range idxRows {
		if r.name == name {
			if r.sql.Valid {
				t.Errorf("index %s: sql = %q, want NULL (automatic indexes must have NULL sql)", name, r.sql.String)
			}
			return
		}
	}
	t.Errorf("no sqlite_schema index row named %s found (rows: %+v)", name, idxRows)
}

// TestAutoIndexUniqueColumn covers a single inline "col ... UNIQUE" column
// constraint: C SQLite creates sqlite_autoindex_t_1 with NULL sql, and
// rejects a duplicate non-NULL value while allowing duplicate NULLs.
func TestAutoIndexUniqueColumn(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "autoidx_unique.sqlite")
			edb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT UNIQUE)")
			wvExec(t, edb, "INSERT INTO t(id,email) VALUES(1,'a@x.com')")
			if err := edb.Exec("INSERT INTO t(id,email) VALUES(2,'a@x.com')"); err == nil {
				t.Fatal("expected the pure-Go engine to reject a duplicate UNIQUE-column value at INSERT time, got none")
			}
			wvExec(t, edb, "INSERT INTO t(id,email) VALUES(3,NULL)")
			wvExec(t, edb, "INSERT INTO t(id,email) VALUES(4,NULL)")
			if err := edb.Close(); err != nil {
				t.Fatalf("engine writer Close: %v", err)
			}

			db := openCgo(t, path)
			requireIntegrityOK(t, db)

			idxRows := fetchIndexSchemaRows(t, db, "t")
			if len(idxRows) != 1 {
				t.Fatalf("sqlite_schema index rows for t = %+v, want exactly 1", idxRows)
			}
			requireAutoIndexRow(t, idxRows, "sqlite_autoindex_t_1")

			var id int
			if err := db.QueryRow("SELECT id FROM t WHERE email = ?", "a@x.com").Scan(&id); err != nil {
				t.Fatalf("SELECT WHERE email=...: %v", err)
			}
			if id != 1 {
				t.Errorf("id = %d, want 1", id)
			}
			requireUsesIndex(t, db, "SELECT id FROM t WHERE email = 'a@x.com'", "sqlite_autoindex_t_1")

			var count int
			if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 3 {
				t.Fatalf("row count = %d, want 3 (the duplicate-email INSERT must have been rejected)", count)
			}
		})
	}
}

// TestAutoIndexNonIntegerPrimaryKey covers a single non-integer PRIMARY KEY
// column (e.g. "a TEXT PRIMARY KEY"): this does NOT collapse into the rowid
// alias (only a single INTEGER-typed PRIMARY KEY does), so C SQLite
// builds sqlite_autoindex_t_1 for it and enforces uniqueness exactly like a
// UNIQUE column -- including allowing duplicate NULLs.
func TestAutoIndexNonIntegerPrimaryKey(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "autoidx_nonint_pk.sqlite")
			edb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, edb, "CREATE TABLE t(a TEXT PRIMARY KEY, v INTEGER)")
			wvExec(t, edb, "INSERT INTO t(a,v) VALUES('k1',10)")
			if err := edb.Exec("INSERT INTO t(a,v) VALUES('k1',20)"); err == nil {
				t.Fatal("expected the pure-Go engine to reject a duplicate PRIMARY KEY value at INSERT time, got none")
			}
			wvExec(t, edb, "INSERT INTO t(a,v) VALUES(NULL,30)")
			wvExec(t, edb, "INSERT INTO t(a,v) VALUES(NULL,40)")
			if err := edb.Close(); err != nil {
				t.Fatalf("engine writer Close: %v", err)
			}

			db := openCgo(t, path)
			requireIntegrityOK(t, db)

			idxRows := fetchIndexSchemaRows(t, db, "t")
			if len(idxRows) != 1 {
				t.Fatalf("sqlite_schema index rows for t = %+v, want exactly 1", idxRows)
			}
			requireAutoIndexRow(t, idxRows, "sqlite_autoindex_t_1")

			var v int
			if err := db.QueryRow("SELECT v FROM t WHERE a = 'k1'").Scan(&v); err != nil {
				t.Fatalf("SELECT WHERE a='k1': %v", err)
			}
			if v != 10 {
				t.Errorf("v = %d, want 10", v)
			}
			requireUsesIndex(t, db, "SELECT v FROM t WHERE a = 'k1'", "sqlite_autoindex_t_1")

			var count int
			if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 3 {
				t.Fatalf("row count = %d, want 3 (the duplicate-'k1' INSERT must have been rejected)", count)
			}
		})
	}
}

// TestAutoIndexIntegerPKPlusUniqueColumn covers "id INTEGER PRIMARY KEY,
// x TEXT UNIQUE" together: the INTEGER PRIMARY KEY is the rowid alias and
// gets NO automatic index, while the UNIQUE column still gets exactly one --
// C SQLite must see exactly ONE sqlite_autoindex_t_* row total, numbered
// _1 (not _2), since the rowid-alias PK contributes no constraint to count
// against.
func TestAutoIndexIntegerPKPlusUniqueColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoidx_ipk_plus_unique.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, x TEXT UNIQUE, y INTEGER)")
	wvExec(t, edb, "INSERT INTO t(id,x,y) VALUES(1,'a',100)")
	if err := edb.Exec("INSERT INTO t(id,x,y) VALUES(2,'a',200)"); err == nil {
		t.Fatal("expected the pure-Go engine to reject a duplicate UNIQUE-column value at INSERT time, got none")
	}
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	idxRows := fetchIndexSchemaRows(t, db, "t")
	if len(idxRows) != 1 {
		t.Fatalf("sqlite_schema index rows for t = %+v, want exactly 1 (the INTEGER PRIMARY KEY must get none)", idxRows)
	}
	requireAutoIndexRow(t, idxRows, "sqlite_autoindex_t_1")

	var count int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (the duplicate-x INSERT must have been rejected)", count)
	}
}

// TestAutoIndexMultiColumnUnique covers a table-level "UNIQUE(a,b)"
// constraint: the pair must be jointly unique (either column alone may
// repeat), enforced via one two-column automatic index.
func TestAutoIndexMultiColumnUnique(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoidx_multi_unique.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, UNIQUE(a,b))")
	wvExec(t, edb, "INSERT INTO t(id,a,b) VALUES(1,1,1)")
	wvExec(t, edb, "INSERT INTO t(id,a,b) VALUES(2,1,2)") // a repeats, b differs: fine
	wvExec(t, edb, "INSERT INTO t(id,a,b) VALUES(3,2,1)") // b repeats, a differs: fine
	if err := edb.Exec("INSERT INTO t(id,a,b) VALUES(4,1,1)"); err == nil {
		t.Fatal("expected the pure-Go engine to reject a duplicate (a,b) pair at INSERT time, got none")
	}
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	idxRows := fetchIndexSchemaRows(t, db, "t")
	if len(idxRows) != 1 {
		t.Fatalf("sqlite_schema index rows for t = %+v, want exactly 1", idxRows)
	}
	requireAutoIndexRow(t, idxRows, "sqlite_autoindex_t_1")
	requireUsesIndex(t, db, "SELECT id FROM t WHERE a = 1 AND b = 2", "sqlite_autoindex_t_1")

	var count int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("row count = %d, want 3 (the duplicate (1,1) INSERT must have been rejected)", count)
	}
}

// TestAutoIndexMultiColumnPrimaryKey covers a table-level "PRIMARY KEY(a,b)"
// constraint: never the rowid alias (only a SINGLE-column INTEGER PRIMARY
// KEY collapses into that), so it always gets its own automatic index.
func TestAutoIndexMultiColumnPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoidx_multi_pk.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(a INTEGER, b INTEGER, v TEXT, PRIMARY KEY(a,b))")
	wvExec(t, edb, "INSERT INTO t(a,b,v) VALUES(1,1,'x')")
	wvExec(t, edb, "INSERT INTO t(a,b,v) VALUES(1,2,'y')")
	if err := edb.Exec("INSERT INTO t(a,b,v) VALUES(1,1,'z')"); err == nil {
		t.Fatal("expected the pure-Go engine to reject a duplicate (a,b) PRIMARY KEY pair at INSERT time, got none")
	}
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	idxRows := fetchIndexSchemaRows(t, db, "t")
	if len(idxRows) != 1 {
		t.Fatalf("sqlite_schema index rows for t = %+v, want exactly 1", idxRows)
	}
	requireAutoIndexRow(t, idxRows, "sqlite_autoindex_t_1")

	var v string
	if err := db.QueryRow("SELECT v FROM t WHERE a = 1 AND b = 2").Scan(&v); err != nil {
		t.Fatalf("SELECT WHERE a=1 AND b=2: %v", err)
	}
	if v != "y" {
		t.Errorf("v = %q, want %q", v, "y")
	}
	requireUsesIndex(t, db, "SELECT v FROM t WHERE a = 1 AND b = 2", "sqlite_autoindex_t_1")

	var count int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("row count = %d, want 2 (the duplicate (1,1) INSERT must have been rejected)", count)
	}
}

// TestAutoIndexIntegerPrimaryKeyAloneNoAutoIndex confirms the base case: a
// lone "id INTEGER PRIMARY KEY" column -- the rowid alias -- produces NO
// sqlite_autoindex row at all (it IS the rowid, not a separately indexed
// column), matching C SQLite exactly.
func TestAutoIndexIntegerPrimaryKeyAloneNoAutoIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoidx_ipk_alone.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	wvExec(t, edb, "INSERT INTO t(id,v) VALUES(1,'a')")
	wvExec(t, edb, "INSERT INTO t(id,v) VALUES(2,'a')") // v is NOT constrained: duplicates fine
	if err := edb.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	db := openCgo(t, path)
	requireIntegrityOK(t, db)

	idxRows := fetchIndexSchemaRows(t, db, "t")
	if len(idxRows) != 0 {
		t.Fatalf("sqlite_schema index rows for t = %+v, want none (INTEGER PRIMARY KEY is the rowid alias)", idxRows)
	}

	var count int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("row count = %d, want 2", count)
	}
}

// TestAutoIndexSurvivesReopen covers the round-trip that matters most for a
// writer whose Close model fully rebuilds the file every time (see
// writer.go's package doc comment): CREATE TABLE with a UNIQUE column,
// Close, engine.OpenWrite, INSERT more rows (including one duplicate that
// must still be rejected) plus an UPDATE, Close again -- integrity_check
// must stay green, the automatic index must still be present with NULL
// sql, and enforcement must still work after the reopen.
func TestAutoIndexSurvivesReopen(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "autoidx_reopen.sqlite")
			edb, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, edb, "CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT UNIQUE, v INTEGER)")
			for i := 1; i <= 20; i++ {
				wvExec(t, edb, fmt.Sprintf("INSERT INTO t(id,email,v) VALUES(%d,%s,%d)", i, wvString(fmt.Sprintf("u%d@x.com", i)), i))
			}
			if err := edb.Close(); err != nil {
				t.Fatalf("engine writer Close (initial): %v", err)
			}

			edb2, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("engine.OpenWrite: %v", err)
			}
			wvExec(t, edb2, "INSERT INTO t(id,email,v) VALUES(21,'u21@x.com',21)")
			if err := edb2.Exec("INSERT INTO t(id,email,v) VALUES(22,'u1@x.com',22)"); err == nil {
				t.Fatal("expected the pure-Go engine to reject a duplicate UNIQUE-column value after OpenWrite, got none")
			}
			if _, err := edb2.Update("UPDATE t SET v = v + 1000 WHERE id % 2 = 0"); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if err := edb2.Close(); err != nil {
				t.Fatalf("engine writer Close (after OpenWrite): %v", err)
			}

			db := openCgo(t, path)
			requireIntegrityOK(t, db)

			idxRows := fetchIndexSchemaRows(t, db, "t")
			if len(idxRows) != 1 {
				t.Fatalf("sqlite_schema index rows for t = %+v, want exactly 1", idxRows)
			}
			requireAutoIndexRow(t, idxRows, "sqlite_autoindex_t_1")
			requireUsesIndex(t, db, "SELECT id FROM t WHERE email = 'u21@x.com'", "sqlite_autoindex_t_1")

			var count int
			if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 21 {
				t.Fatalf("row count = %d, want 21 (20 original + 1 new; the duplicate-email INSERT must have been rejected)", count)
			}

			var v int
			if err := db.QueryRow("SELECT v FROM t WHERE id = 2").Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v != 1002 {
				t.Errorf("v for id=2 after UPDATE = %d, want 1002", v)
			}
		})
	}
}
