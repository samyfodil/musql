package engine

import (
	"strings"
	"testing"
)

// schemaVersionReloadSQL reads a catalog object's text from a fresh snapshot.
func schemaVersionReloadSQL(t *testing.T, db *Session, name string) string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, qerr := p.Query("SELECT sql FROM sqlite_master WHERE name='" + name + "'")
	if qerr != nil || len(rows) != 1 {
		t.Fatalf("read %s's sql: rows=%d err=%v", name, len(rows), qerr)
	}
	return string(rows[0][0].S)
}

// TestSchemaVersionWriteReloadsTheSchema gates that PRAGMA schema_version=N
// causes a full schema reload, preventing stale catalog data from being used.
func TestSchemaVersionWriteReloadsTheSchema(t *testing.T) {
	db, err := Create(t.TempDir() + "/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a INT, b TEXT NOT NULL)",
		"INSERT INTO t1 VALUES(1,2),('a','b')",
		"BEGIN",
		"PRAGMA writable_schema=ON",
		"UPDATE sqlite_master SET sql='CREATE TABLE t1(a INT, b TEXT)' WHERE name LIKE 't1'",
		"PRAGMA schema_version=1234",
		"COMMIT",
		"ALTER TABLE t1 ADD COLUMN c INT DEFAULT 78",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got, want := schemaVersionReloadSQL(t, db, "t1"), "CREATE TABLE t1(a INT, b TEXT, c INT DEFAULT 78)"; got != want {
		t.Errorf("t1's sql after the ADD COLUMN =\n  %q\nwant (the oracle's)\n  %q", got, want)
	}
}

// TestSchemaVersionReloadIsUndoneByRollback pins the half that makes an
// IN-TRANSACTION reload safe here at all. A plain direct sqlite_master write
// never moves the cookie, so C's ROLLBACK does not reload and leaves the
// reverted file under the reloaded schema -- which is why
// wsReloadFromImagePlan otherwise refuses inside a transaction. A
// schema_version write DOES move it, so C's ROLLBACK reloads from the
// reverted catalog. Measured against 3.53.3: both the table and its catalog
// row come back as they were before BEGIN.
func TestSchemaVersionReloadIsUndoneByRollback(t *testing.T) {
	db, err := Create(t.TempDir() + "/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t(a,b)",
		"INSERT INTO t VALUES(1,2)",
		"BEGIN",
		"PRAGMA writable_schema=ON",
		"UPDATE sqlite_master SET sql='CREATE TABLE t(a,b,c)' WHERE name='t'",
		"PRAGMA schema_version=1234",
		"ROLLBACK",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got, want := schemaVersionReloadSQL(t, db, "t"), "CREATE TABLE t(a,b)"; got != want {
		t.Errorf("t's sql after ROLLBACK = %q, want %q", got, want)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	cols, rows, qerr := p.Query("SELECT * FROM t")
	if qerr != nil || len(cols) != 2 || len(rows) != 1 {
		t.Errorf("SELECT * FROM t after ROLLBACK: cols=%v rows=%d err=%v; want [a b] and one row", cols, len(rows), qerr)
	}
}

// TestSchemaVersionWriteWithoutAnEditChangesNothing: with no direct write
// outstanding the catalog already IS the live schema, so the reload does not
// run at all -- pragma.test's own bumps take this path.
func TestSchemaVersionWriteWithoutAnEditChangesNothing(t *testing.T) {
	db, err := Create(t.TempDir() + "/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{"CREATE TABLE t(a,b)", "PRAGMA schema_version=105", "ALTER TABLE t ADD COLUMN c"} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := schemaVersionReloadSQL(t, db, "t"); !strings.Contains(got, "c") {
		t.Errorf("t's sql = %q, want the ordinary ADD COLUMN result", got)
	}
}
