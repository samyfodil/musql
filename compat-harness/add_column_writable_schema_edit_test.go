// This file tests ALTER TABLE ADD COLUMN against a table with outstanding
// writable_schema edits.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

var addColumnWritableSchemaEditCases = []wsCase{
	{
		name: "add column splices before a phantom trailing column, which becomes genuinely live",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c DEFAULT 42)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`},
			{sql: `PRAGMA writable_schema=OFF`},
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1'`},
			{sql: `SELECT * FROM t1`},
			{sql: `PRAGMA table_info(t1)`},
			{sql: `PRAGMA integrity_check`},
			{sql: `INSERT INTO t1(a,b,d) VALUES(9,9,9)`},
			{sql: `SELECT * FROM t1 ORDER BY a`},
		},
	},
	{
		// A phantom NOT NULL column: the ALTER itself never validates it
		// (C SQLite's own nested re-validation pass only runs when the
		// ADDED column carries its own CHECK -- see
		// addColumnUnderWritableSchemaEdit's doc comment), but a later
		// INSERT that omits it DOES enforce it.
		name: "phantom NOT NULL column is not validated by ADD COLUMN itself, but IS enforced afterward",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c NOT NULL DEFAULT 5 CHECK(c>100))' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`},
			{sql: `PRAGMA writable_schema=OFF`},
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1'`},
			{sql: `INSERT INTO t1(a,b) VALUES(9,9)`, decline: true},
		},
	},
	{
		// Two ADD COLUMNs under the SAME outstanding edit: the first
		// resolves it (tbl.sql becomes the spliced text for real), so the
		// second is an ORDINARY append at the new end -- not a second
		// offset-splice.
		name: "a second ADD COLUMN after the edit is resolved is an ordinary append",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`},
			{sql: `ALTER TABLE t1 ADD COLUMN e`},
			{sql: `PRAGMA writable_schema=OFF`},
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1'`},
			{sql: `SELECT * FROM t1`},
		},
	},
	{
		// An ordinary (edit-free) ALTER TABLE DROP COLUMN moves tbl.sql for
		// real, so a LATER edit's splice offset must be computed from the
		// table's shape AFTER that drop, not its original CREATE TABLE text.
		name: "splice offset reflects an earlier ordinary DROP COLUMN, not the table's original text",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b,c,e,f)`},
			{sql: `ALTER TABLE t1 DROP COLUMN e`},
			{sql: `INSERT INTO t1 VALUES(1,2,3,4)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c,f,z)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN g`},
			{sql: `PRAGMA writable_schema=OFF`},
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1'`},
			{sql: `SELECT * FROM t1`},
		},
	},
	{
		// An empty table has nothing for a phantom NOT NULL column to
		// violate -- same "back-fill loop never raises over zero rows" rule
		// ordinary ADD COLUMN's own REFERENCES/NOT NULL/non-constant-DEFAULT
		// checks already apply.
		name: "empty table: a phantom NOT NULL column raises nothing",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c NOT NULL)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`},
			{sql: `PRAGMA writable_schema=OFF`},
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1'`},
		},
	},
	{
		// ADD COLUMN on a table with NO edit of its own succeeds normally
		// even while a DIFFERENT table's row carries one elsewhere in the
		// catalog -- the ALTER STATEMENT itself is not blocked. But real
		// SQLite's post-ALTER reload is WHOLE-CATALOG (alter.c:115-116,
		// zWhere=0), so it silently ABSORBS that other table's edit too
		// (its phantom column becomes genuinely live) as a side effect this
		// write path's single-target function does not reproduce -- so this
		// engine declines the ALTER outright rather than leave the other
		// table's edit stale. Verified directly against 3.53.3: with t1
		// untouched by the ALTER but separately edited to a still-parseable
		// "CREATE TABLE t1(a,b,c)", "ALTER TABLE t2 ADD COLUMN y" leaves t1
		// reading back as a genuine 3-column table afterward.
		name: "ADD COLUMN is declined while a DIFFERENT table's row also carries an edit",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `CREATE TABLE t2(x)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t2 ADD COLUMN y`, decline: true},
			{sql: `PRAGMA writable_schema=OFF`},
			// Confirm the decline changed nothing on either table.
			{sql: `SELECT sql FROM sqlite_master WHERE name IN ('t1','t2') ORDER BY name`},
		},
	},
	{
		// The identical shape succeeds once only ONE table's row carries an
		// edit -- t1's own ADD COLUMN resolves it first (deleting its
		// overlay entry, see addColumnUnderWritableSchemaEdit), so the
		// LATER "ALTER TABLE t2 ADD COLUMN" sees a session with no
		// outstanding edit left at all and runs as ordinary DDL.
		name: "a second table's own ADD COLUMN proceeds once the first table's edit is resolved",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `CREATE TABLE t2(x)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`},
			{sql: `ALTER TABLE t2 ADD COLUMN y`},
			{sql: `PRAGMA writable_schema=OFF`},
			{sql: `SELECT sql FROM sqlite_master WHERE name IN ('t1','t2') ORDER BY name`},
		},
	},
	{
		// The ADDED column's own CHECK clause is declined outright: real
		// SQLite's post-ALTER re-validation pass, triggered only because the
		// added column carries a CHECK, validates EVERY column (including
		// phantom ones) for CHECK *and* NOT NULL against every existing row --
		// a materially larger validation this write path does not
		// reproduce. See addColumnUnderWritableSchemaEdit's doc comment.
		name: "ADD COLUMN's own CHECK clause under an outstanding edit is declined",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c NOT NULL)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d CHECK(d IS NULL OR d>0)`, decline: true},
			{sql: `PRAGMA writable_schema=OFF`},
			// Nothing was mutated by the decline -- the row still reads the
			// edited (not the ADD COLUMN's) text.
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1'`},
		},
	},
	{
		// The edit reorders/renames an EXISTING (already-real) column --
		// declined: a stored value's position can never be safely relabeled
		// under a different column here (see this function's doc comment;
		// this is the same "DROP COLUMN needs real row surgery" boundary,
		// just triggered from the opposite direction).
		name: "edit renaming an existing column is declined",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(x,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`, decline: true},
			{sql: `PRAGMA writable_schema=OFF`},
		},
	},
	{
		// A pre-existing PRIMARY KEY/UNIQUE-derived auto-index on the table
		// makes this write path decline outright, rather than risk telling
		// apart "was already there" from "the edit invented it" without the
		// original parse's own autoIndexSpec list.
		name: "a pre-existing UNIQUE auto-index is declined",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a UNIQUE, b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a UNIQUE,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`, decline: true},
			{sql: `PRAGMA writable_schema=OFF`},
		},
	},
	{
		// A phantom column's non-constant DEFAULT (a clock reading, an
		// expression) has no single value this write path can back-fill
		// every existing row with here -- declined, matching this write
		// path's existing (edit-free) non-constant-DEFAULT decline in
		// spirit.
		name: "a phantom column's non-constant DEFAULT is declined",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c DEFAULT CURRENT_TIME)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 ADD COLUMN d`, decline: true},
			{sql: `PRAGMA writable_schema=OFF`},
		},
	},
	{
		// RENAME TO over the table's own edited row reloads the whole catalog
		// and then renames, which is what alter.c does in the other order:
		// validate against the stale schema, rewrite, renameReloadSchema
		// (alter.c:143, :281). Names are all RENAME TO validates, and an
		// sql-only edit cannot change one, so both orders agree. Every later
		// step is ordinary: no edit is outstanding after the reload.
		name: "RENAME TO over the table's own edited row reloads first",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 RENAME TO t1x`},
			{sql: `PRAGMA writable_schema=OFF`},
			{sql: `SELECT type,name,tbl_name,sql FROM sqlite_master`},
			{sql: `SELECT * FROM t1x`},
			{sql: `ALTER TABLE t1x RENAME COLUMN b TO bb`},
			{sql: `ALTER TABLE t1x DROP COLUMN bb`},
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1x'`},
			{sql: `INSERT INTO t1x VALUES(7,8)`},
			{sql: `SELECT * FROM t1x`},
			{sql: `PRAGMA integrity_check`},
		},
	},
	{
		// RENAME COLUMN and DROP COLUMN stay declined over the table's own
		// edited row. alter.c takes the column POSITION from the stale table
		// (alter.c:637, :2272) and rewrites the edited text positionally,
		// so reloading first answers differently -- measured: RENAME COLUMN
		// of a column only the edit has errors on C SQLite and succeeded
		// here, and a failed one left the reloaded columns behind.
		name: "RENAME COLUMN/DROP COLUMN over the table's own edited row stay declined",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 RENAME COLUMN c TO cc`, decline: true},
			{sql: `ALTER TABLE t1 RENAME COLUMN b TO bb`, decline: true},
			{sql: `ALTER TABLE t1 DROP COLUMN b`, decline: true},
			{sql: `PRAGMA writable_schema=OFF`},
			// Confirm none of the declines mutated anything.
			{sql: `SELECT sql FROM sqlite_master WHERE name='t1'`},
		},
	},
	{
		// A RENAME TO that fails after the reload must leave the stale schema,
		// as C SQLite's failed ALTER does: t1 keeps two columns, so a
		// three-value INSERT is refused on both engines.
		name: "a failed RENAME TO over an edited row leaves the stale schema",
		steps: []wsStep{
			{sql: `CREATE TABLE t1(a,b)`},
			{sql: `CREATE TABLE t2(x)`},
			{sql: `INSERT INTO t1 VALUES(1,2)`},
			{sql: `PRAGMA writable_schema=ON`},
			{sql: `UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`},
			{sql: `ALTER TABLE t1 RENAME TO t2`, decline: true},
			{sql: `SELECT * FROM t1`},
		},
	},
}

func TestAddColumnAfterWritableSchemaEdit(t *testing.T) {
	for _, tc := range addColumnWritableSchemaEditCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			godb, err := engine.Create(filepath.Join(dir, "pure.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
			if err != nil {
				t.Fatal(err)
			}
			cgodb.SetMaxOpenConns(1)
			defer cgodb.Close()

			for i, step := range tc.steps {
				goCols, goRows, goErr := wsGoRun(godb, step.sql)
				if step.decline {
					if goErr == nil {
						t.Fatalf("step %d %q: expected this engine to DECLINE, got cols=%v rows=%v", i, step.sql, goCols, goRows)
					}
					wsAssertOracleAlsoRejects(t, cgodb, i, step.sql)
					continue
				}
				if goErr != nil {
					t.Fatalf("step %d %q: this engine errored: %v", i, step.sql, goErr)
				}
				cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, step.sql)
				if cgoErr != nil {
					t.Fatalf("step %d %q: C SQLite errored where this engine accepted: %v", i, step.sql, cgoErr)
				}
				if !wsSameResult(goCols, goRows, cgoCols, cgoRows) {
					t.Fatalf("step %d %q diverges:\n  pure: %v %v\n  real: %v %v", i, step.sql, goCols, goRows, cgoCols, cgoRows)
				}
			}
		})
	}
}
