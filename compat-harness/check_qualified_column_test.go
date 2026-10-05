// Gate for table-qualified column references in CHECK constraints.
// enough: "xyzzy" resolves to nothing and SQLite still accepts it.
package compat

import "testing"

// TestCheckQualifiedColumn pins the two-part form, in both directions.
func TestCheckQualifiedColumn(t *testing.T) {
	differ(t, "check qualified by its own table", []string{
		`CREATE TABLE t3(x,y,z, CHECK( t3.x<25 ))`,
		`INSERT INTO t3 VALUES(1,2,3)`,
		`INSERT INTO t3 VALUES(99,2,3)`,
		`SELECT x,y,z FROM t3 ORDER BY rowid`,
		`SELECT sql FROM sqlite_master WHERE name='t3'`,
	})
	// The qualifier is matched case-insensitively, and the CHECK still reports
	// its violation with the verbatim source text.
	differ(t, "check qualifier case", []string{
		`CREATE TABLE MixedCase(a, CHECK( mixedcase.a > 0 ))`,
		`INSERT INTO MixedCase VALUES(5)`,
		`INSERT INTO MixedCase VALUES(-5)`,
		`SELECT a FROM MixedCase`,
	})
	// A qualifier naming ANOTHER table is rejected at CREATE.
	differ(t, "check qualified by a foreign table", []string{
		`CREATE TABLE t812(c, CHECK( other.c>0 ))`,
		`INSERT INTO t812 VALUES(1)`,
	})
	// The three-part forms: the schema part is ignored, whether or not it
	// names a database that exists.
	differ(t, "check qualified with main", []string{
		`CREATE TABLE t810(a, CHECK( main.t810.a>0 ))`,
		`INSERT INTO t810 VALUES(5)`,
		`INSERT INTO t810 VALUES(-5)`,
		`SELECT a FROM t810`,
		`SELECT sql FROM sqlite_master WHERE name='t810'`,
	})
	differ(t, "check qualified with a nonexistent schema", []string{
		`CREATE TABLE t811(b, CHECK( xyzzy.t811.b BETWEEN 5 AND 10 ))`,
		`INSERT INTO t811 VALUES(7)`,
		`INSERT INTO t811 VALUES(1)`,
		`SELECT b FROM t811`,
	})
	// The shape the corpus actually mined, where the table and its columns are
	// spelled with reserved words.
	differ(t, "check qualified with keyword names", []string{
		`CREATE TABLE f(true INT, false INT, x INT CHECK (5 IN (f.false)))`,
		`INSERT INTO f VALUES(1,5,3)`,
		`INSERT INTO f VALUES(1,9,3)`,
		`SELECT true,false,x FROM f ORDER BY rowid`,
	})
	// A qualified reference inside every expression shape the traversal walks,
	// so the threading of the table name cannot be missed in one branch.
	differ(t, "check qualified in nested expressions", []string{
		`CREATE TABLE n(a,b, CHECK( abs(n.a) BETWEEN 1 AND 10 AND n.b LIKE 'x%' AND (CASE WHEN n.a>0 THEN n.b ELSE 'x' END) IS NOT NULL ))`,
		`INSERT INTO n VALUES(5,'xy')`,
		`INSERT INTO n VALUES(99,'xy')`,
		`INSERT INTO n VALUES(5,'zz')`,
		`SELECT a,b FROM n ORDER BY rowid`,
	})
}

// TestCheckQualifiedColumnSurvivesRename pins what accepting a qualified CHECK
// made newly reachable, and what it broke.
//
// C SQLite REWRITES the qualifier when the table is renamed -- "t1" with
// "CHECK( t1.a != t1.b )" becomes
// CREATE TABLE "t1new"(a, b, CHECK("t1new".a != "t1new".b)), the new name
// QUOTED exactly as the CREATE's own name token is. This engine rewrote only
// the name token, leaving a CHECK naming a table that no longer existed: a
// WRONG stored schema, and the very first thing altertab.test checks once
// qualified references are accepted at all.
//
// It was caught by the fts5-tagged harness build and NOT by the default one --
// which is why both builds are the rule, not one.
func TestCheckQualifiedColumnSurvivesRename(t *testing.T) {
	differ(t, "rename rewrites a CHECK qualifier", []string{
		`CREATE TABLE t1(a, b, CHECK(t1.a != t1.b))`,
		`ALTER TABLE t1 RENAME TO t1new`,
		`SELECT sql FROM sqlite_master WHERE name='t1new'`,
		// ...and the constraint is still ENFORCED under the new name, which
		// needs the PARSED expression re-derived from the rewritten text.
		`INSERT INTO t1new VALUES(1,2)`,
		`INSERT INTO t1new VALUES(1,1)`,
		`SELECT a,b FROM t1new`,
	})
	differ(t, "rename with a three-part CHECK qualifier", []string{
		`CREATE TABLE t2(a, b, CHECK(main.t2.a != main.t2.b))`,
		`ALTER TABLE t2 RENAME TO t2new`,
		`SELECT sql FROM sqlite_master WHERE name='t2new'`,
		`INSERT INTO t2new VALUES(1,2)`,
		`INSERT INTO t2new VALUES(3,3)`,
		`SELECT a,b FROM t2new`,
	})
	// A CHECK that does NOT name the table is left alone by the rename.
	differ(t, "rename leaves an unqualified CHECK alone", []string{
		`CREATE TABLE t3(a, b, CHECK(a != b))`,
		`ALTER TABLE t3 RENAME TO t3new`,
		`SELECT sql FROM sqlite_master WHERE name='t3new'`,
		`INSERT INTO t3new VALUES(1,1)`,
	})
	// RENAME COLUMN must still rewrite the column inside a qualified CHECK.
	differ(t, "rename column inside a qualified CHECK", []string{
		`CREATE TABLE t4(a, b, CHECK(t4.a != t4.b))`,
		`ALTER TABLE t4 RENAME COLUMN a TO z`,
		`SELECT sql FROM sqlite_master WHERE name='t4'`,
		`INSERT INTO t4 VALUES(1,1)`,
		`INSERT INTO t4 VALUES(1,2)`,
		`SELECT z,b FROM t4`,
	})
}
