// Gates checkReservedObjectName: whether C SQLite allows CREATE or RENAME of
// "sqlite_sequence" or "sqlite_stat1" under writable_schema=ON. C skips the
// entire "sqlite_" prefix check when writable_schema=ON. This engine's
// synthesized versions of these tables follow the same rules.
package compat

import (
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestReservedSqliteNameWritableSchemaGenericLift verifies the general
// writable_schema=ON lift for any "sqlite_"-prefixed name.
func TestReservedSqliteNameWritableSchemaGenericLift(t *testing.T) {
	stmts := []string{
		"PRAGMA writable_schema=ON",
		"CREATE TABLE sqlite_nope(x)",
		"CREATE VIEW sqlite_myview AS SELECT 1",
		"CREATE TABLE t1(a)",
		"ALTER TABLE t1 RENAME TO sqlite_bad",
		"INSERT INTO sqlite_nope VALUES(42)",
		"SELECT * FROM sqlite_nope",
		"SELECT * FROM sqlite_myview",
		"SELECT * FROM sqlite_bad",
	}
	differ(t, "generic-lift", stmts)
}

// TestReservedSqliteNameWritableSchemaOff verifies that writable_schema=OFF
// still refuses every "sqlite_"-prefixed name.
func TestReservedSqliteNameWritableSchemaOff(t *testing.T) {
	cases := []struct{ name, stmt string }{
		{"generic", "CREATE TABLE sqlite_nope(x)"},
		{"stat1", "CREATE TABLE sqlite_stat1(tbl,idx,stat)"},
		{"sequence", "CREATE TABLE sqlite_sequence(name,seq)"},
	}
	for _, c := range cases {
		differ(t, c.name, []string{c.stmt})
	}
}

// TestReservedSqliteNameOffMessageIsGeneric is a direct-engine check (error
// TEXT is deliberately excluded from the differ()/run() comparison -- see
// worker/main.go's stmtResult doc comment -- so this bypasses the harness
// entirely) that checkReservedObjectName's OFF path no longer gives
// sqlite_stat1/sqlite_sequence their own special wording: both now get the
// exact same "object name reserved for internal use" text every other
// "sqlite_"-prefixed name already did, matching build.c:1054's own
// undifferentiated refusal.
func TestReservedSqliteNameOffMessageIsGeneric(t *testing.T) {
	for _, name := range []string{"sqlite_stat1", "sqlite_sequence", "sqlite_nope"} {
		db, err := engine.Create(t.TempDir()+"/reserved-off.sqlite")
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		err = db.Exec("CREATE TABLE " + name + "(a,b,c)")
		db.Close()
		if err == nil {
			t.Fatalf("%s: expected a decline under writable_schema=OFF, got none", name)
		}
		if !strings.Contains(err.Error(), "object name reserved for internal use: "+name) {
			t.Errorf("%s: expected the generic reserved-name message, got: %v", name, err)
		}
		// The OLD message this bucket replaced -- must never reappear.
		if strings.Contains(err.Error(), "synthesizes that table itself") {
			t.Errorf("%s: still carries the old bucket-specific wording: %v", name, err)
		}
	}
}

// TestReservedSqliteSequenceWithoutRowidCorpusShape is without_rowid1.test
// 15.1 verbatim (trailing ";" stripped per statement, matching how
// splitTopLevelStatements -- slt_test.go -- feeds the real corpus replay):
// a user "sqlite_sequence" WITHOUT ROWID table created under
// writable_schema=ON, populated, and then ALTER TABLE RENAME TO's
// alter.c:246 cascade rewrites its "c1" row to "a" -- a coincidental name
// match this session's AUTOINCREMENT bookkeeping never touches (no
// AUTOINCREMENT table exists anywhere in this script). Expected final
// answer (verified against 3.53.3, and matching the .test file's own
// {a c0 c2}): the renamed row moved, the other two untouched.
func TestReservedSqliteSequenceWithoutRowidCorpusShape(t *testing.T) {
	stmts := []string{
		"PRAGMA writable_schema=ON",
		"CREATE TABLE sqlite_sequence (name PRIMARY KEY) WITHOUT ROWID",
		"PRAGMA writable_schema=OFF",
		"CREATE TABLE c1(x)",
		"INSERT INTO sqlite_sequence(name) VALUES('c0'),('c1'),('c2')",
		"ALTER TABLE c1 RENAME TO a",
		"SELECT name FROM sqlite_sequence ORDER BY +name",
	}
	differ(t, "without_rowid1-15.1", stmts)
}

// TestReservedSqliteStat1FkeyCorpusShapesDeclineForUnrelatedReason runs
// fkey1.test 8.1 and 8.2's EXACT mined statement sequences (DESC-ordered
// PRIMARY KEY, verbatim) directly against the engine (not through
// differ()/run(), whose stmtResult intentionally strips error text) and
// confirms BOTH still decline, but now ONLY because of
// finalizeWithoutRowidPK's separate DESC-ordered-PK limitation -- never
// because of the reserved-name check this bucket closed. A regression back
// to the OLD "synthesizes that table itself" wording, or a NEW crash/silent
// accept, would both be caught here.
func TestReservedSqliteStat1FkeyCorpusShapesAgreeWithOracle(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fkey1-8.1", []string{
			"PRAGMA writable_schema=ON",
			"PRAGMA foreign_keys = ON",
			"CREATE TABLE sqlite_stat1 (tbl INTEGER PRIMARY KEY DESC, idx UNIQUE DEFAULT NULL) WITHOUT ROWID",
		}},
		{"fkey1-8.2", []string{
			"CREATE TABLE t1(a REFERENCES sqlite_stat1 ON DELETE CASCADE)",
			"CREATE TABLE t2(a TEXT PRIMARY KEY)",
			"PRAGMA writable_schema=ON",
			"CREATE TABLE sqlite_stat1(tbl INTEGER PRIMARY KEY DESC, idx UNIQUE DEFAULT NULL) WITHOUT ROWID",
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestReservedSqliteStat1FkeyAscVariantsAgreeWithOracle is fkey1.test 8.1 and
// 8.2 with DESC dropped (the corpus's own PRIMARY KEY column, ASC instead) --
// the ONLY textual difference from the real corpus statements. This isolates
// the reserved-name fix from finalizeWithoutRowidPK's unrelated DESC-PK gap
// and proves it is complete and correct: without the DESC blocker in the way,
// both shapes now agree with the oracle end to end.
func TestReservedSqliteStat1FkeyAscVariantsAgreeWithOracle(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fkey1-8.1-asc", []string{
			"PRAGMA writable_schema=ON",
			"PRAGMA foreign_keys = ON",
			"CREATE TABLE sqlite_stat1 (tbl INTEGER PRIMARY KEY, idx UNIQUE DEFAULT NULL) WITHOUT ROWID",
			"PRAGMA writable_schema=OFF",
			"CREATE TABLE sqlsim4(stat PRIMARY KEY)",
			"CREATE TABLE t1(sqlsim7 REFERENCES sqlite_stat1 ON DELETE CASCADE)",
			`DROP table "sqlsim4"`,
		}},
		{"fkey1-8.2-asc", []string{
			"CREATE TABLE t1(a REFERENCES sqlite_stat1 ON DELETE CASCADE)",
			"CREATE TABLE t2(a TEXT PRIMARY KEY)",
			"PRAGMA writable_schema=ON",
			"CREATE TABLE sqlite_stat1(tbl INTEGER PRIMARY KEY, idx UNIQUE DEFAULT NULL) WITHOUT ROWID",
			"UPDATE sqlite_schema SET name='sqlite_autoindex_sqlite_stat1_1' WHERE name='sqlite_autoindex_sqlite_stat1_2'",
			"PRAGMA writable_schema=RESET",
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestReservedSqliteSequenceAutoIncrementAdoptsUserTable covers a REAL user
// "sqlite_sequence" created (under writable_schema) before the catalog's first
// AUTOINCREMENT table. That table is pSeqTab from then on (build.c:2968), so
// the AUTOINCREMENT CREATE makes no second one (build.c:2925) and succeeds;
// each INSERT then uses it if it is an ordinary two-column rowid table and
// fails SQLITE_CORRUPT_SEQUENCE otherwise (insert.c:426-434) -- here, because
// the first one is WITHOUT ROWID. The user's own rows survive either way.
func TestReservedSqliteSequenceAutoIncrementAdoptsUserTable(t *testing.T) {
	differ(t, "user sqlite_sequence WITHOUT ROWID", []string{
		"PRAGMA writable_schema=ON",
		"CREATE TABLE sqlite_sequence(name PRIMARY KEY) WITHOUT ROWID",
		"PRAGMA writable_schema=OFF",
		"INSERT INTO sqlite_sequence(name) VALUES('keepme')",
		"CREATE TABLE t(x INTEGER PRIMARY KEY AUTOINCREMENT, y)",
		"INSERT INTO t(y) VALUES('a')",
		"SELECT name FROM sqlite_sequence",
		"SELECT * FROM t",
		"SELECT type, name FROM sqlite_schema ORDER BY rowid",
	})
	differ(t, "user sqlite_sequence two columns", []string{
		"PRAGMA writable_schema=ON",
		"CREATE TABLE sqlite_sequence(a, b)",
		"PRAGMA writable_schema=OFF",
		"INSERT INTO sqlite_sequence VALUES('keepme', 7)",
		"CREATE TABLE t(x INTEGER PRIMARY KEY AUTOINCREMENT, y)",
		"INSERT INTO t(y) VALUES('a')",
		"INSERT INTO sqlite_sequence VALUES('t', 40)",
		"INSERT INTO t(y) VALUES('b')",
		"SELECT rowid, * FROM sqlite_sequence",
		"SELECT * FROM t",
		"SELECT type, name FROM sqlite_schema ORDER BY rowid",
	})
}

// TestReservedSqliteSequenceMultipleAutoIncrementTablesUnaffected is a
// regression guard for the new AUTOINCREMENT-vs-real-sqlite_sequence check
// (schema_write.go): the ORDINARY case -- several AUTOINCREMENT tables in one
// session, no user-created sqlite_sequence table anywhere -- must keep
// working exactly as before. Only the SECOND+ AUTOINCREMENT table's CREATE
// sees db.buildAutoIncrementSequenceTable() already non-nil, and the new
// check only ever fires when that call is nil AND a real "sqlite_sequence"
// table already exists (neither is true here).
func TestReservedSqliteSequenceMultipleAutoIncrementTablesUnaffected(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t1(x INTEGER PRIMARY KEY AUTOINCREMENT, y)",
		"CREATE TABLE t2(x INTEGER PRIMARY KEY AUTOINCREMENT, y)",
		"INSERT INTO t1(y) VALUES(1)",
		"INSERT INTO t2(y) VALUES(2)",
		"DROP TABLE t1",
		"CREATE TABLE t3(x INTEGER PRIMARY KEY AUTOINCREMENT, y)",
		"INSERT INTO t3(y) VALUES(3)",
		"SELECT name,seq FROM sqlite_sequence ORDER BY name",
	}
	differ(t, "multiple-autoincrement-unaffected", stmts)
}
