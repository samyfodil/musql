// Tests CREATE TABLE's per-constraint ON CONFLICT clauses against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// TestCreateTableOnConflictMatchesCSQLite is the per-constraint declared
// "ON CONFLICT <action>" conformance gate: for each page size, and for each
// VDBEMode (on/off -- a table with any declared conflict default is out of
// the VDBE write compiler's own scope, see vdbe_write.go's
// compileInsertWrite/compileUpdateWrite, so this also exercises that
// fallback is reached correctly whether MUSQL_VDBE is on or off), it drives
// the pure-Go engine writer and a live real-SQLite oracle connection through
// a script covering: a column-level declared PRIMARY KEY (REPLACE) and
// UNIQUE (IGNORE) on the SAME table, a table-level UNIQUE(...) (FAIL), a
// table-level PRIMARY KEY(...) (ROLLBACK), a declared NOT NULL (IGNORE), an
// explicit statement-level OR-clause overriding each of those declared
// defaults, and a row conflicting on TWO differently-declared constraints at
// once -- then verifies the resulting file's exact row content and PRAGMA
// integrity_check='ok' against C SQLite, and that a Close->OpenWrite
// reopen still applies the declared defaults correctly.
func TestCreateTableOnConflictMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					testCreateTableOnConflictScenario(t, pageSize)
				})
			}
		})
	}
}

func testCreateTableOnConflictScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("onconflict_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection, so DDL/DML/schema queries all see the SAME in-memory schema (see execConflictBoth's doc comment)

	// ---- t1: a column-level declared PRIMARY KEY ON CONFLICT REPLACE (the
	// rowid alias -- columnInfo.RowidConflict) alongside a column-level
	// declared UNIQUE ON CONFLICT IGNORE, on the SAME table. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t1(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b TEXT UNIQUE ON CONFLICT IGNORE)`)
	for _, s := range []string{
		`INSERT INTO t1 VALUES(1,'x'),(2,'y')`,

		// Declared PK default REPLACE, no OR-clause on the statement: a
		// rowid collision (a=1) deletes the existing row and stores the new
		// one under the SAME rowid (unlike a unique-COLUMN conflict --
		// verified directly, see vdbe_conflict_test.go's t3 for the
		// identical rowid-vs-unique-column distinction with an EXPLICIT
		// OR REPLACE).
		`INSERT INTO t1 VALUES(1,'replaced-one')`,

		// Declared UNIQUE(b) default IGNORE, no OR-clause: a duplicate b
		// value is silently skipped, RowsAffected=0, no error.
		`INSERT INTO t1 VALUES(3,'y')`,

		// An explicit statement-level "OR ABORT" overrides the declared PK
		// REPLACE default uniformly: the SAME rowid collision (a=1) now
		// errors instead of replacing.
		`INSERT OR ABORT INTO t1 VALUES(1,'never')`,

		// An explicit statement-level "OR FAIL" overrides the declared
		// UNIQUE(b) IGNORE default: the SAME duplicate b value now errors
		// instead of being silently skipped.
		`INSERT OR FAIL INTO t1 VALUES(4,'y')`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- t2: a table-level UNIQUE(...) ON CONFLICT FAIL -- FAIL leaves
	// every row the statement already applied in place, unlike ABORT. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t2(a, b, UNIQUE(a,b) ON CONFLICT FAIL)`)
	for _, s := range []string{
		`INSERT INTO t2 VALUES(1,10),(2,20)`,
		// No OR-clause at all: the declared FAIL default governs. The THIRD
		// row (1,10) conflicts with the pre-existing row; the two rows
		// before it in this SAME statement (3,30),(4,40) stay applied
		// (FAIL's own semantics), and the row after it (5,50) is never even
		// attempted.
		`INSERT INTO t2 VALUES(3,30),(4,40),(1,10),(5,50)`,
		// An explicit "OR ROLLBACK" overrides the declared FAIL default:
		// with no explicit transaction active, ROLLBACK collapses to
		// exactly what ABORT does -- unlike FAIL, undoes EVERY row this
		// statement itself applied (6,60 rolled back too).
		`INSERT OR ROLLBACK INTO t2 VALUES(6,60),(1,10)`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- t3: a table-level PRIMARY KEY(...) ON CONFLICT ROLLBACK -- with
	// an explicit transaction active, ROLLBACK undoes the WHOLE enclosing
	// transaction (including an earlier statement's own already-applied
	// change) and ends it outright. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t3(a, b, PRIMARY KEY(a) ON CONFLICT ROLLBACK)`)
	execPlainBoth(t, db, sdb, `BEGIN`)
	for _, s := range []string{
		`INSERT INTO t3 VALUES(1,'first')`,
		// No OR-clause: the declared ROLLBACK default governs a duplicate a.
		`INSERT INTO t3 VALUES(1,'dup')`,
	} {
		execConflictBoth(t, db, sdb, s)
	}
	// The transaction was already ended by ROLLBACK above -- COMMIT errors,
	// matching C SQLite exactly.
	execPlainBoth(t, db, sdb, `COMMIT`)

	// ---- t4: a declared NOT NULL ON CONFLICT IGNORE (no DEFAULT clause --
	// see columnInfo.NotNullConflict's own doc comment for why a DEFAULT
	// clause is a separate, declined case), and UPDATE consulting the SAME
	// declared default. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t4(a INTEGER UNIQUE, b NOT NULL ON CONFLICT IGNORE)`)
	for _, s := range []string{
		`INSERT INTO t4 VALUES(1,10)`,
		// No OR-clause: the declared IGNORE default silently skips this row.
		`INSERT INTO t4 VALUES(2,NULL)`,
		// An explicit "OR FAIL" overrides the declared IGNORE default: the
		// SAME NOT NULL violation now errors instead of being skipped.
		`INSERT OR FAIL INTO t4 VALUES(3,NULL)`,
		// UPDATE against the same declared default: no OR-clause, so
		// setting b to NULL is silently skipped (the row is left
		// unchanged), RowsAffected=0.
		`UPDATE t4 SET b=NULL WHERE a=1`,
		// UPDATE's own explicit "OR ABORT" overrides the declared IGNORE
		// default: the same SET now errors instead of being skipped.
		`UPDATE OR ABORT t4 SET b=NULL WHERE a=1`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- nnr1: a declared NOT NULL ON CONFLICT REPLACE *with* a DEFAULT --
	// the case t4's comment above calls out as separate. REPLACE on a NOT
	// NULL constraint SUBSTITUTES the column's DEFAULT for the NULL and then
	// re-runs the check with REPLACE demoted to ABORT, so a DEFAULT that is
	// itself NULL (or absent) still fails. Verified directly against the
	// oracle; the substitution is applied identically by all three write
	// paths (vdbe_write.go's compiler, insert_write.go's row builder and
	// write_update_delete.go's), which is why this runs under both VDBEModes.
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr1(a INTEGER, c INTEGER NOT NULL ON CONFLICT REPLACE DEFAULT 7, d TEXT NOT NULL ON CONFLICT REPLACE DEFAULT 'dd')`)
	for _, s := range []string{
		// An explicitly-supplied NULL takes the DEFAULT, in either column.
		`INSERT INTO nnr1 VALUES(1,NULL,NULL)`,
		`INSERT INTO nnr1 VALUES(2,NULL,'x')`,
		`INSERT INTO nnr1 VALUES(3,9,NULL)`,
		// An OMITTED column takes the same DEFAULT by the ordinary path.
		`INSERT INTO nnr1(a) VALUES(4)`,
		// A statement-level OR-clause overrides the declared REPLACE, so
		// these do NOT substitute: IGNORE skips the row, ABORT errors.
		`INSERT OR IGNORE INTO nnr1 VALUES(5,NULL,NULL)`,
		`INSERT OR ABORT INTO nnr1 VALUES(6,NULL,NULL)`,
		// UPDATE consults the same declared default and substitutes too.
		`UPDATE nnr1 SET c=NULL, d=NULL WHERE a=1`,
		`UPDATE OR IGNORE nnr1 SET c=NULL WHERE a=2`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- nnr2/nnr3: the same declared REPLACE with a DEFAULT of NULL, and with
	// no DEFAULT at all -- both must still fail the NOT NULL check (SQLite's
	// documented "if the column has no default value, the ABORT algorithm is
	// used", and a NULL default substitutes a NULL). ----
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr2(x INTEGER NOT NULL ON CONFLICT REPLACE DEFAULT NULL, y INTEGER)`)
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr3(x INTEGER NOT NULL ON CONFLICT REPLACE, y INTEGER)`)
	for _, s := range []string{
		`INSERT INTO nnr2 VALUES(NULL,1)`,
		`INSERT INTO nnr3 VALUES(NULL,1)`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	// ---- nnr4: statement-level REPLACE (no column list) on a table with a
	// GENERATED column. This exact shape is gencol1.test's, and it is the one
	// that regressed while the substitution was first written: with no column
	// list the tuple maps positionally onto the NON-generated columns INSIDE
	// buildFullRow, so a substitution keyed off the INSERT's column list never
	// fired -- and because the decline had already been narrowed, the failure
	// surfaced as a CONSTRAINT error, which the corpus scores as wrong rather
	// than as an honest decline. See rowValSlotForColumn (insert_write.go).
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr4(a NOT NULL DEFAULT 123, b AS(a) UNIQUE)`)
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr5(a NOT NULL DEFAULT 123, b AS(a+1111) UNIQUE)`)
	// A NOT NULL GENERATED column derived from a column that is ITSELF being
	// substituted: the substitution has to happen BEFORE the derivation, or b
	// is computed from the NULL and fails a constraint SQLite satisfies. Both
	// the VIRTUAL and STORED spellings, and both orders of dependency.
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr6(c0 NOT NULL DEFAULT 'xyz', c1 AS(c0) STORED NOT NULL)`)
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr7(a NOT NULL DEFAULT 'aaa', b AS(c) NOT NULL, c NOT NULL DEFAULT 'ccc')`)
	execPlainBoth(t, db, sdb, `CREATE TABLE nnr8(a NOT NULL DEFAULT 'aaa', b AS(c) STORED NOT NULL, c NOT NULL DEFAULT 'ccc')`)
	for _, s := range []string{
		`REPLACE INTO nnr4 VALUES(NULL)`,
		`REPLACE INTO nnr5 VALUES(NULL)`,
		// The same table reached WITH a column list, and with the column
		// omitted entirely -- the other two of buildFullRow's three mappings.
		`REPLACE INTO nnr4(a) VALUES(NULL)`,
		`INSERT OR REPLACE INTO nnr5(a) VALUES(NULL)`,
		`REPLACE INTO nnr6(c0) VALUES(NULL)`,
		`REPLACE INTO nnr7(a,c) VALUES(NULL,NULL)`,
		`REPLACE INTO nnr8(a,c) VALUES(NULL,NULL)`,
	} {
		execConflictBoth(t, db, sdb, s)
	}
	check2 := func(q string) {
		t.Helper()
		pager, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		assertPragmaEqual(t, pager, sdb, q)
	}
	check2(`SELECT a, b FROM nnr4 ORDER BY a`)
	check2(`SELECT a, b FROM nnr5 ORDER BY a`)
	check2(`SELECT c0, c1 FROM nnr6`)
	check2(`SELECT a, b, c FROM nnr7`)
	check2(`SELECT a, b, c FROM nnr8`)

	// ---- t5: a single candidate row conflicting on TWO differently-
	// declared UNIQUE constraints at once (one IGNORE, one REPLACE) -- real
	// SQLite processes every non-REPLACE-declared constraint before any
	// REPLACE-declared one (verified directly, both orderings), so the
	// IGNORE constraint's own resolution governs the whole row regardless
	// of which column was declared first in the CREATE TABLE text. ----
	execPlainBoth(t, db, sdb, `CREATE TABLE t5(a INTEGER UNIQUE ON CONFLICT IGNORE, b INTEGER UNIQUE ON CONFLICT REPLACE)`)
	for _, s := range []string{
		`INSERT INTO t5 VALUES(1,10),(2,20)`,
		// Conflicts on BOTH a=1 (IGNORE) and b=20 (REPLACE) at once: the
		// IGNORE constraint's own resolution wins -- the whole candidate
		// row is skipped, neither existing row is touched.
		`INSERT INTO t5 VALUES(1,20)`,
	} {
		execConflictBoth(t, db, sdb, s)
	}
	execPlainBoth(t, db, sdb, `CREATE TABLE t6(a INTEGER UNIQUE ON CONFLICT REPLACE, b INTEGER UNIQUE ON CONFLICT IGNORE)`)
	for _, s := range []string{
		`INSERT INTO t6 VALUES(1,10),(2,20)`,
		// Same shape, declared in the OPPOSITE order (REPLACE first, IGNORE
		// second): the result must be identical -- IGNORE still wins,
		// regardless of declaration order.
		`INSERT INTO t6 VALUES(1,20)`,
	} {
		execConflictBoth(t, db, sdb, s)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	// THE ORACLE IS ASKED ABOUT THE EXPORT: the file this engine wrote is a segment
	// file and C cannot read one, so the interchange claim runs through
	// ExportSQLite -- which tests the conversion too. See
	// convert_for_oracle_test.go.
	exported := filepath.Join(t.TempDir(), "exported-for-oracle.db")
	if xerr := sqliteconv.Export(path, exported, 0); xerr != nil {
		t.Fatalf("ExportSQLite: %v", xerr)
	}
	fdb, err := sql.Open("sqlite3", exported)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s): %v", exported, err)
	}
	defer fdb.Close()

	var integrity string
	if err := fdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
	}

	verifyTableViaCSQLite(t, fdb, wvTable{name: "t1", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "replaced-one"}},
		{rowid: 2, cols: []any{int64(2), "y"}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t2", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), int64(10)}},
		{rowid: 2, cols: []any{int64(2), int64(20)}},
		{rowid: 3, cols: []any{int64(3), int64(30)}},
		{rowid: 4, cols: []any{int64(4), int64(40)}},
	}})
	// t3's ROLLBACK ran inside an explicit transaction, so it undid the
	// WHOLE enclosing transaction -- including the EARLIER, same-
	// transaction insert of row a=1 -- leaving the table empty (verified
	// directly: execConflictBoth above already required the engine and real
	// SQLite to agree on this exact outcome).
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t3", colList: "a,b", rows: nil})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t4", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), int64(10)}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t5", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), int64(10)}},
		{rowid: 2, cols: []any{int64(2), int64(20)}},
	}})
	verifyTableViaCSQLite(t, fdb, wvTable{name: "t6", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), int64(10)}},
		{rowid: 2, cols: []any{int64(2), int64(20)}},
	}})
	if err := fdb.Close(); err != nil {
		t.Fatalf("close fdb: %v", err)
	}

	// ---- Close -> reopen: a declared conflict default still applies
	// correctly against a table recovered from an existing file, not just
	// one built fresh this session (its own declared action is re-derived
	// from the table's stored CREATE TABLE sql text by
	// parseCreateTableColumnsAndAutoIndexes/buildAutoIndexes, exactly like
	// every other UNIQUE/PRIMARY KEY constraint -- see writer_open.go's
	// OpenWrite). ----
	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	for _, s := range []string{
		// t1's declared PK REPLACE default, still applied after reopen.
		`INSERT INTO t1 VALUES(2,'reopened-replace')`,
		// t4's declared NOT NULL IGNORE default, still applied after reopen.
		`INSERT INTO t4 VALUES(5,NULL)`,
	} {
		execConflictBoth(t, db2, sdb, s)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("engine writer Close (reopened): %v", err)
	}

	// ...and the ORACLE reads the EXPORT again, for the reason above: a second
	// export, because the engine has written to the file since the first one.
	exported2 := filepath.Join(t.TempDir(), "exported-after-reopen.db")
	if xerr := sqliteconv.Export(path, exported2, 0); xerr != nil {
		t.Fatalf("ExportSQLite (after reopen): %v", xerr)
	}
	fdb2, err := sql.Open("sqlite3", exported2)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s) after reopen: %v", exported2, err)
	}
	defer fdb2.Close()
	if err := fdb2.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check (after reopen): %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check (after reopen) = %q, want \"ok\"", integrity)
	}
	verifyTableViaCSQLite(t, fdb2, wvTable{name: "t1", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "replaced-one"}},
		{rowid: 2, cols: []any{int64(2), "reopened-replace"}},
	}})
	verifyTableViaCSQLite(t, fdb2, wvTable{name: "t4", colList: "a,b", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), int64(10)}},
	}})
}
