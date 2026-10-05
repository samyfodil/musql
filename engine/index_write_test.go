// This file tests CREATE/DROP INDEX: that schema rows are written correctly,
// UNIQUE constraints are enforced with NULL exemption, unsupported forms are
// rejected, and indexes survive a reopen.
package engine_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestCreateIndexSchemaRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_schema.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b INTEGER)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,a,b) VALUES(%d,%s,%d)", i, sqlString(fmt.Sprintf("v%d", i)), i*10))
	}
	mustExec(t, db, "CREATE INDEX idx_a ON t(a)")
	mustExec(t, db, "CREATE UNIQUE INDEX idx_b ON t(b)")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	schema, err := p.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	var idxRows []engine.SchemaRow
	for _, r := range schema {
		if r.Type == "index" {
			idxRows = append(idxRows, r)
		}
	}
	if len(idxRows) != 2 {
		t.Fatalf("got %d index schema rows, want 2 (rows: %+v)", len(idxRows), schema)
	}
	for _, r := range idxRows {
		if r.TblName != "t" {
			t.Errorf("index %s: TblName = %q, want t", r.Name, r.TblName)
		}
		// Index RootPage is zero because the format stores SQL, not index data.
		if r.RootPage != 0 {
			t.Errorf("index %s: RootPage = %d, want 0 on a format with no index data", r.Name, r.RootPage)
		}
		if !strings.Contains(strings.ToUpper(r.SQL), "CREATE") || !strings.Contains(strings.ToUpper(r.SQL), "INDEX") {
			t.Errorf("index %s: SQL %q doesn't look like CREATE INDEX", r.Name, r.SQL)
		}
	}
}

func TestUniqueIndexRejectsDuplicateAtInsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_unique.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, email TEXT)")
	mustExec(t, db, "INSERT INTO t(id,email) VALUES(1,'a@x.com')")
	mustExec(t, db, "CREATE UNIQUE INDEX idx_email ON t(email)")

	if err := db.Exec("INSERT INTO t(id,email) VALUES(2,'a@x.com')"); err == nil {
		t.Error("expected a UNIQUE violation inserting a duplicate email, got none")
	}
	// Non-conflicting inserts must still work.
	if err := db.Exec("INSERT INTO t(id,email) VALUES(3,'b@x.com')"); err != nil {
		t.Errorf("INSERT of a non-conflicting row failed: %v", err)
	}

	if err := db.Exec("UPDATE t SET email='b@x.com' WHERE id=1"); err == nil {
		t.Error("expected a UNIQUE violation from UPDATE creating a duplicate email, got none")
	}
}

func TestCreateUniqueIndexRejectsPreexistingDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_unique_pre.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,'x')")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(2,'x')")
	if err := db.Exec("CREATE UNIQUE INDEX idx_v ON t(v)"); err == nil {
		t.Error("expected CREATE UNIQUE INDEX over already-duplicate data to fail, got none")
	}
}

func TestUniqueIndexAllowsDuplicateNulls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_unique_null.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "CREATE UNIQUE INDEX idx_v ON t(v)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,NULL)")
	if err := db.Exec("INSERT INTO t(id,v) VALUES(2,NULL)"); err != nil {
		t.Errorf("a UNIQUE index should allow multiple NULLs, got error: %v", err)
	}
	if err := db.Exec("INSERT INTO t(id,v) VALUES(3,NULL)"); err != nil {
		t.Errorf("a UNIQUE index should allow multiple NULLs, got error: %v", err)
	}
}

func TestCreateIndexRejectsUnsupportedForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_unsupported.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, b TEXT)")
	for _, stmt := range []string{
		"CREATE INDEX i3 ON t(a COLLATE BACKWARDS)", // custom/unregistered collation (BINARY/NOCASE/RTRIM are supported)
		"CREATE INDEX i5 ON nosuchtable(a)",         // no such table
		"CREATE INDEX i6 ON t(nosuchcolumn)",        // no such column
		// An expression/partial index that references a column the table does
		// not have is rejected exactly like C SQLite ("no such column").
		"CREATE INDEX i7 ON t(upper(nosuchcolumn))",
		"CREATE INDEX i8 ON t(a) WHERE nosuchcolumn IS NOT NULL",
		// Expression/partial indexes with unsupported forms are rejected.
	} {
		if err := db.Exec(stmt); err == nil {
			t.Errorf("Exec(%q): expected an error, got none", stmt)
		}
	}
	// A UNIQUE expression index and a UNIQUE partial index are accepted and
	// ENFORCED: the key is evaluated per row, a NULL key never conflicts, and
	// a partial index constrains only its WHERE-matching rows.
	mustExec(t, db, "CREATE UNIQUE INDEX u1 ON t(upper(a))")
	mustExec(t, db, "CREATE UNIQUE INDEX u2 ON t(b) WHERE b IS NOT NULL")
	mustExec(t, db, "INSERT INTO t(id,a,b) VALUES(1,'x','p')")
	if err := db.Exec("INSERT INTO t(id,a,b) VALUES(2,'X','q')"); err == nil {
		t.Errorf("upper(a) duplicate: expected a UNIQUE violation, got none")
	}
	if err := db.Exec("INSERT INTO t(id,a,b) VALUES(3,'y',NULL)"); err != nil {
		t.Errorf("a NULL partial-index key must not conflict: %v", err)
	}
	if err := db.Exec("INSERT INTO t(id,a,b) VALUES(4,'z',NULL)"); err != nil {
		t.Errorf("a second NULL partial-index key must not conflict: %v", err)
	}

	// Non-unique expression and partial indexes are accepted and materialized.
	mustExec(t, db, "CREATE INDEX i1 ON t(a) WHERE a IS NOT NULL") // partial index
	mustExec(t, db, "CREATE INDEX i2 ON t(upper(a))")              // expression index
	mustExec(t, db, "CREATE INDEX i9 ON t(a, b || a)")             // mixed plain + expression
	// DESC-ordered columns are accepted like ASC.
	mustExec(t, db, "CREATE INDEX i_desc ON t(a DESC)")
	// A previously-registered index name can't be redefined.
	mustExec(t, db, "CREATE INDEX i_ok ON t(a)")
	if err := db.Exec("CREATE INDEX i_ok ON t(b)"); err == nil {
		t.Error("expected redefining an existing index name to fail, got none")
	}
	mustExec(t, db, "CREATE INDEX IF NOT EXISTS i_ok ON t(b)") // must NOT error
}

func TestDropIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_drop.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT)")
	mustExec(t, db, "INSERT INTO t(id,a) VALUES(1,'x')")
	mustExec(t, db, "CREATE INDEX idx_a ON t(a)")
	mustExec(t, db, "DROP INDEX idx_a")
	if err := db.Exec("DROP INDEX idx_a"); err == nil {
		t.Error("expected dropping an already-dropped index to fail, got none")
	}
	if err := db.Exec("DROP INDEX IF EXISTS idx_a"); err != nil {
		t.Errorf("DROP INDEX IF EXISTS on a missing index should not fail: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	schema, err := p.Schema()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range schema {
		if r.Type == "index" {
			t.Errorf("schema still has an index row after DROP INDEX: %+v", r)
		}
	}
}

// TestOpenWriteRecoversIndexes verifies that indexes survive a close/reopen
// cycle through engine.OpenWrite.
func TestOpenWriteRecoversIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx_openwrite.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t(id,a) VALUES(%d,%s)", i, sqlString(fmt.Sprintf("v%d", i))))
	}
	mustExec(t, db, "CREATE UNIQUE INDEX idx_a ON t(a)")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if names := db2.IndexNamesForTest(); len(names) != 1 || names[0] != "idx_a" {
		t.Fatalf("engine.OpenWrite did not recover the index: indexes = %+v", names)
	}
	// The recovered UNIQUE index must still be enforced.
	if err := db2.Exec("INSERT INTO t(id,a) VALUES(6,'v1')"); err == nil {
		t.Error("expected the recovered UNIQUE index to reject a duplicate, got none")
	}
	mustExec(t, db2, "INSERT INTO t(id,a) VALUES(7,'v7')")
	if err := db2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	schema, err := p.Schema()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range schema {
		if r.Type == "index" && r.Name == "idx_a" {
			found = true
		}
	}
	if !found {
		t.Error("index idx_a did not survive an engine.OpenWrite/Close round trip")
	}
	rowids, _, err := p.Rows("t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rowids) != 6 {
		t.Errorf("got %d rows, want 6 (5 original + 1 new, the duplicate rejected)", len(rowids))
	}
}
