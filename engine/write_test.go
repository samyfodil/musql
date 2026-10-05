// Tests the write path through various table and row configurations.
package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

// execer is any writable handle these tests hold: an *engine.DB, or the
// *engine.Session a segment database hands back. Widened from *engine.DB when the
// fixtures moved to this format -- the helpers only ever wanted Exec.
type execer interface {
	Exec(string) error
}

// snapshotter is a writable handle a test also READS through -- an *engine.DB or
// the *engine.Session a segment database hands back, both of which answer
// SnapshotPager with a reader over what the session can see right now.
type snapshotter interface {
	Exec(string) error
	SnapshotPager() (*engine.ReadOnlyPager, error)
}

func mustExec(t *testing.T, db execer, sqlText string) {
	t.Helper()
	if err := db.Exec(sqlText); err != nil {
		t.Fatalf("Exec(%s): %v", sqlText, err)
	}
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sqlBlob(b []byte) string {
	return "x'" + fmt.Sprintf("%x", b) + "'"
}

// floatLiteral renders f as SQL text that parses back to the exact same
// float64 bit pattern, including signed zero (which needs an explicit "-0.0"
// rather than Go's default "%v" rendering of negative zero as just "0").
func floatLiteral(f float64) string {
	if math.Signbit(f) && f == 0 {
		return "-0.0"
	}
	s := fmt.Sprintf("%.17g", f)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0" // force real: "4185" alone would parse back as an INTEGER literal
	}
	return s
}

func TestInsertDuplicateRowidErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,'a')")
	if err := db.Exec("INSERT INTO t(id,v) VALUES(1,'b')"); err == nil {
		t.Error("expected an error inserting a duplicate rowid, got none")
	}
}

// TestCreateTableWithoutRowidRequiresPrimaryKey confirms this write path's
// WITHOUT ROWID support (schema_write.go's finalizeWithoutRowidPK) still
// declines the one shape C SQLite itself rejects outright -- a WITHOUT
// ROWID table with no PRIMARY KEY at all ("PRIMARY KEY missing on table t",
// verified directly against mattn/go-sqlite3) -- while a single-column
// PRIMARY KEY WITHOUT ROWID table (formerly rejected wholesale by this write
// path) is now accepted; see compat-harness/vdbe_without_rowid_test.go for
// the full byte-exact-against-real-SQLite coverage of what IS supported.
func TestCreateTableWithoutRowidRequiresPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wr.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec("CREATE TABLE t(id INTEGER, v TEXT) WITHOUT ROWID"); err == nil {
		t.Error("expected a WITHOUT ROWID table with no PRIMARY KEY to be rejected, got none")
	}
	mustExec(t, db, "CREATE TABLE t2(id INTEGER PRIMARY KEY, v TEXT) WITHOUT ROWID")
}

func TestExecRejectsUnsupportedStatements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsupp.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO t(id,v) VALUES(1,'a'),(2,'b')")
	mustExec(t, db, "CREATE INDEX tv ON t(v)")
	for _, stmt := range []string{
		// The two UNIQUE expression/partial forms that used to be here are
		// implemented now (validateUniqueIndex); see
		// compat-harness/unique_expr_index_test.go.
		"CREATE INDEX idx3 ON t((SELECT v FROM t))", // subquery in index expression: declined
		"DELETE FROM t USING other WHERE t.id = other.id",
		// "UPDATE t SET v=(SELECT v FROM t t2 WHERE t2.id<=t.id) WHERE v>'a'"
		// was here, refused because an index could drive update.c's one-pass
		// loop in an order this write path could not promise. It is served now:
		// the loop takes the order the ported planner gives it, or rowid order
		// where C falls back to two passes -- as it does for this SET, which
		// updates the one index. See compat-harness/write_loop_subqueries_test.go.
	} {
		if err := db.Exec(stmt); err == nil {
			t.Errorf("Exec(%q): expected an error, got none", stmt)
		}
	}
}
