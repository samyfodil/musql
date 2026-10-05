// Tests PRAGMA legacy_alter_table behavior: flag persistence and rewrite rules.
package compat

import "testing"

// TestPragmaR25LegacyAlterTableValue tests the flag's value and persistence.
func TestPragmaR25LegacyAlterTableValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		// One row of one INTEGER column, 0 on a fresh connection; the setter
		// answers zero rows.
		{"getter-default-and-round-trip", []string{
			`PRAGMA legacy_alter_table`,
			`PRAGMA legacy_alter_table=ON`,
			`PRAGMA legacy_alter_table`,
			`PRAGMA legacy_alter_table=OFF`,
			`PRAGMA legacy_alter_table`,
		}},

		// The lifetime that used to be the whole blocker: the value has to
		// survive the statements AFTER the setter, which on this driver each run
		// on their own throwaway session.
		{"survives-later-statements", []string{
			`PRAGMA legacy_alter_table=ON`,
			`CREATE TABLE q(a)`,
			`INSERT INTO q VALUES(1)`,
			`PRAGMA legacy_alter_table`,
			`SELECT a FROM q`,
			`PRAGMA legacy_alter_table`,
		}},

		// ...and a transaction, which does NOT ignore the setter and does not
		// roll it back.
		{"survives-a-transaction", []string{
			`PRAGMA legacy_alter_table=ON`,
			`BEGIN`,
			`PRAGMA legacy_alter_table`,
			`ROLLBACK`,
			`PRAGMA legacy_alter_table`,
		}},
		{"a-setter-inside-a-transaction-is-not-ignored", []string{
			`BEGIN`,
			`PRAGMA legacy_alter_table=ON`,
			`PRAGMA legacy_alter_table`,
			`COMMIT`,
			`PRAGMA legacy_alter_table`,
		}},

		// A qualifier is ignored: one connection-wide flag, whichever database
		// is named.
		{"the-qualifier-is-ignored", []string{
			`PRAGMA main.legacy_alter_table=ON`,
			`PRAGMA legacy_alter_table`,
			`PRAGMA main.legacy_alter_table`,
			`PRAGMA temp.legacy_alter_table`,
		}},

		// The three ALTER forms the flag does NOT change: byte-identical
		// sqlite_master rows with it on, including RENAME COLUMN's rewrite of a
		// view body.
		{"add-rename-and-drop-column-are-unaffected", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE VIEW v1 AS SELECT a FROM t1`,
			`PRAGMA legacy_alter_table=ON`,
			`ALTER TABLE t1 ADD COLUMN c`,
			`ALTER TABLE t1 RENAME COLUMN a TO a2`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, tc.name, tc.stmts) })
	}
}

// TestPragmaR25LegacyAlterTableRenameCascade is alterlegacy.test 1.0/1.1's own
// fixture (t1/t2/v1/tr/i1), replayed through differ so it exercises the SAME
// path the mined corpus does (driver, not engine-direct): the table's own
// row, the explicit index's ON clause and the trigger's ON clause all move to
// the new name, while t2's FK, v1's body and tr's own body stay pointing at
// the OLD one -- and a later INSERT that fires tr breaks on that stale body
// reference exactly as it does on the oracle, not merely at ALTER time.
func TestPragmaR25LegacyAlterTableRenameCascade(t *testing.T) {
	schema := []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TABLE t2(x REFERENCES t1)`,
		`CREATE VIEW v1 AS SELECT a FROM t1`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO t1(a) SELECT 1; END`,
		`CREATE INDEX i1 ON t1(a)`,
	}
	differ(t, "rename-cascade", append(append([]string{}, schema...),
		`PRAGMA legacy_alter_table=ON`,
		`ALTER TABLE t1 RENAME TO t1x`,
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		// tr's ON clause now targets t1x and still fires, but its own body
		// ("INSERT INTO t1...") names a table that no longer exists.
		`INSERT INTO t1x(a) VALUES(1)`,
		// v1's body was never rewritten either.
		`SELECT * FROM v1`,
	))
}

// TestPragmaR25LegacyAlterTableCheckSelfQualifierErrors is alterlegacy.test
// 1.2's own case: a CHECK that qualifies a column with the table's OWN name
// survives the rename unrewritten (C SQLite's isLegacy==0 gate on
// sqlite3WalkExprList(pTab->pCheck), alter.c:1828), which then makes the
// WHOLE ALTER fail once the table is renamed out from under it -- the
// qualifier no longer names anything. Both engines must reject the ALTER and
// leave the schema exactly as it was (differ compares the whole sequence, so
// a partially-applied rename on either side would show up in the readback).
func TestPragmaR25LegacyAlterTableCheckSelfQualifierErrors(t *testing.T) {
	differ(t, "check-self-qualifier", []string{
		`PRAGMA legacy_alter_table=ON`,
		`CREATE TABLE t1(a, b, CHECK(t1.a != t1.b))`,
		`SELECT sql FROM sqlite_master`,
		`ALTER TABLE t1 RENAME TO t1new`,
		`SELECT sql FROM sqlite_master`,
	})
}

// TestPragmaR25LegacyAlterTablePartialIndexWhereErrors is the WHERE-clause
// mirror of the CHECK case (alterlegacy.test 1.3): a partial index's WHERE
// qualified with the indexed table's own name is C SQLite's OTHER
// isLegacy==0-gated walk (alter.c:1844's sqlite3WalkExpr(pPartIdxWhere)), left
// stale the same way and failing the ALTER for the same reason.
func TestPragmaR25LegacyAlterTablePartialIndexWhereErrors(t *testing.T) {
	differ(t, "partial-index-where-self-qualifier", []string{
		`PRAGMA legacy_alter_table=ON`,
		`CREATE TABLE t2(a, b)`,
		`CREATE INDEX t2expr ON t2(a) WHERE t2.b>0`,
		`SELECT sql FROM sqlite_master`,
		`ALTER TABLE t2 RENAME TO t2new`,
		`SELECT sql FROM sqlite_master`,
	})
}

// TestPragmaR25LegacyAlterTableForeignKeysGate is alter.c:1817's
// `if( isLegacy==0 || (db->flags & SQLITE_ForeignKeys) )`: under legacy a
// child's REFERENCES clause is rewritten only when THIS connection's own
// "PRAGMA foreign_keys" is separately ON, never on the default OFF.
func TestPragmaR25LegacyAlterTableForeignKeysGate(t *testing.T) {
	schema := []string{
		`CREATE TABLE p1(a PRIMARY KEY)`,
		`CREATE TABLE c1(x INTEGER PRIMARY KEY, y REFERENCES p1(a))`,
	}
	t.Run("foreign-keys-off-leaves-the-reference-stale", func(t *testing.T) {
		differ(t, "fk-off", append(append([]string{}, schema...),
			`PRAGMA legacy_alter_table=ON`,
			`ALTER TABLE p1 RENAME TO ppp`,
			`SELECT sql FROM sqlite_master WHERE name='c1'`,
		))
	})
	t.Run("foreign-keys-on-still-rewrites-it", func(t *testing.T) {
		differ(t, "fk-on", append(append([]string{}, schema...),
			`PRAGMA legacy_alter_table=ON`,
			`PRAGMA foreign_keys=ON`,
			`ALTER TABLE p1 RENAME TO ppp`,
			`SELECT sql FROM sqlite_master WHERE name='c1'`,
		))
	})
}

// TestPragmaR25LegacyAlterTableAttachForeignKeys is alterlegacy.test 8.x's own
// shape: the FK rewrite reaches ACROSS an ATTACHed database too, since
// "ALTER TABLE aux.p1 RENAME TO ppp" is delegated whole to aux's own write
// session (attach_write.go), which now carries LegacyAlterTable (and
// ForeignKeys) onto that session -- see attachedWriteSession's own comment.
//
// ATTACHes ':memory:' rather than a shared on-disk path: differ() replays the
// SAME statement list against every engine in turn (cgo, then musql), each
// its own worker PROCESS with its own COMPAT_DSN, but a literal file path
// embedded in an ATTACH statement is not templated per engine the way
// newAttachPair's "{0}" placeholder is -- a real path would have the SECOND
// worker (musql) open the FIRST worker's (cgo's) already-populated,
// already-renamed aux file, "CREATE TABLE aux.c1" failing with "already
// exists" for a reason that has nothing to do with this PRAGMA. See
// crossdb_write_test.go for the same ":memory:" convention.
func TestPragmaR25LegacyAlterTableAttachForeignKeys(t *testing.T) {
	differ(t, "attach-fk", []string{
		`PRAGMA legacy_alter_table=1`,
		`ATTACH ':memory:' AS aux`,
		`PRAGMA foreign_keys=on`,
		`CREATE TABLE aux.p1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE aux.c1(x INTEGER PRIMARY KEY, y REFERENCES p1(a))`,
		`INSERT INTO aux.p1 VALUES(1, 1)`,
		`INSERT INTO aux.p1 VALUES(2, 2)`,
		`INSERT INTO aux.c1 VALUES(NULL, 2)`,
		`ALTER TABLE aux.p1 RENAME TO ppp`,
		`INSERT INTO aux.c1 VALUES(NULL, 1)`,
		`SELECT sql FROM aux.sqlite_master WHERE name = 'c1'`,
	})
}

// TestPragmaR25LegacyAlterTableTurnedOff confirms the flag is a genuine
// TOGGLE, not a one-way switch: with it back OFF, the FULL (non-legacy)
// cascade this engine already had runs exactly as before -- the CHECK
// qualifier, the FK reference and the view/trigger body all follow the
// rename, none of the legacy-only carve-outs above apply.
func TestPragmaR25LegacyAlterTableTurnedOff(t *testing.T) {
	differ(t, "legacy-off-again", []string{
		`PRAGMA legacy_alter_table=ON`,
		`PRAGMA legacy_alter_table=OFF`,
		`CREATE TABLE t1(a, b, CHECK(t1.a != t1.b))`,
		`CREATE TABLE t2(x REFERENCES t1)`,
		`ALTER TABLE t1 RENAME TO t1x`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	})
}

// TestPragmaR25LegacyAlterTableCrossCatalogUnaffected is alterlegacy.test
// 11.x's own shape: a schema-qualified rename that does NOT target the table
// a cross-database TEMP trigger is bound to must leave that trigger
// completely untouched, legacy or not -- alter.c:1854's own table-name
// comparison (`0==sqlite3_stricmp(sParse.pNewTrigger->table, zOld)`) simply
// never matches, the same as a same-catalog rename skips a trigger on an
// unrelated table. tr's ON clause explicitly says "aux.t1"; renaming
// main.t1 must never touch it.
//
// engine-direct (flLockstep) rather than differ(): a fresh PRAGMA setter aside,
// this is exactly attach_trigger_temp_target_test.go's shape, and that file's
// own tests are engine-direct for the reason its package comment gives --
// driver's fresh-session-per-autocommit-statement model cannot carry an
// ATTACHed database's LIVE binding (as opposed to its on-disk file, which a
// fresh session reopens by path) from the CREATE TEMP TRIGGER statement that
// resolved it to the next one, so a driver-routed replay of this exact
// sequence errors "unknown database aux" for a reason that has nothing to do
// with this PRAGMA.
func TestPragmaR25LegacyAlterTableCrossCatalogUnaffected(t *testing.T) {
	flLockstep(t, "cross-catalog rename unaffected by an attach-bound trigger",
		[]string{
			`PRAGMA legacy_alter_table=1`,
			`ATTACH ':memory:' AS aux`,
			`CREATE TABLE aux.t1(a, b, c)`,
			`CREATE TABLE main.t1(a, b, c)`,
			`CREATE TEMP TRIGGER tr AFTER INSERT ON aux.t1 BEGIN SELECT 1; END`,
			`ALTER TABLE main.t1 RENAME TO t2`,
		},
		`SELECT name, tbl_name FROM sqlite_temp_master`,
		`SELECT sql FROM sqlite_master ORDER BY name`,
	)
}

// The vtab half of this gate -- both the fts5 rename the mined corpus itself
// exercises (alterlegacy.test 10.x) and the excluded diverging-cascade class
// renameVtabTo declines rather than risk -- lives in
// pragma_r25_legacy_alter_vtab_fts5_test.go, gated `sqlite_fts5` like every
// other fts5-dependent file here (see fts5_enabled_test.go's doc comment for
// why: the oracle itself only has the module under that tag).
