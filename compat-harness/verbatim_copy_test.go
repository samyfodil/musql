// Verify that untouched tables remain consistent during writes.
// Tests compare rows, page count, freelist count, and integrity across engines.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestVerbatimCopyPreservesUntouchedPages tests that untouched tables stay consistent during write.
func TestVerbatimCopyPreservesUntouchedPages(t *testing.T) {
	const nTables = 12
	var create, reads []string
	for i := 0; i < nTables; i++ {
		create = append(create,
			fmt.Sprintf(`CREATE TABLE t%d(id INTEGER PRIMARY KEY, v TEXT)`, i),
			fmt.Sprintf(`INSERT INTO t%d VALUES (1,'a'),(2,'b'),(3,'c')`, i),
		)
		reads = append(reads, fmt.Sprintf(`SELECT id, v FROM t%d ORDER BY id`, i))
	}
	reads = append(reads, `PRAGMA integrity_check`)
	differ(t, "verbatim-copy/wide-schema-one-touch",
		append(append([]string{}, create...), append([]string{`UPDATE t0 SET v = 'z' WHERE id = 2`}, reads...)...))
}

// TestVerbatimCopyLowerRootpageCollision tests page allocation when untouched tables have lower rootpages.
func TestVerbatimCopyLowerRootpageCollision(t *testing.T) {
	create := []string{
		`CREATE TABLE untouched(id INTEGER PRIMARY KEY, v TEXT)`, // claims the LOWER rootpage
		`INSERT INTO untouched VALUES (1,'a'),(2,'b')`,
		`CREATE TABLE touched(id INTEGER PRIMARY KEY, v TEXT)`, // claims a HIGHER rootpage
		`INSERT INTO touched VALUES (1,'x'),(2,'y')`,
	}
	differ(t, "verbatim-copy/lower-rootpage-collision", append(append([]string{}, create...), []string{
		`UPDATE touched SET v = 'z' WHERE id = 1`,
		`SELECT id, v FROM untouched ORDER BY id`,
		`SELECT id, v FROM touched ORDER BY id`,
		`PRAGMA integrity_check`,
	}...))
}

// TestVerbatimCopySchemaOnlySessionKeepsAllPages tests that schema-only DDL preserves untouched tables.
func TestVerbatimCopySchemaOnlySessionKeepsAllPages(t *testing.T) {
	create := []string{
		`CREATE TABLE a(x)`,
		`INSERT INTO a VALUES(1)`,
		`CREATE TABLE l(v)`,
	}
	differ(t, "verbatim-copy/schema-only-session", append(append([]string{}, create...), []string{
		`CREATE TRIGGER t AFTER INSERT ON a BEGIN SELECT 1; END`,
		`SELECT x FROM a`,
		`SELECT v FROM l`,
		`INSERT INTO a VALUES(2)`,
		`PRAGMA integrity_check`,
	}...))
}

// TestVerbatimCopySameSessionDropThenCreateFreesNothingExtra tests DROP and CREATE in one transaction.
func TestVerbatimCopySameSessionDropThenCreateFreesNothingExtra(t *testing.T) {
	differ(t, "verbatim-copy/same-session-drop-then-create", []string{
		`CREATE TABLE t(a)`, `CREATE TABLE u(b)`,
		`BEGIN`,
		`DROP TABLE t`,
		`CREATE TABLE w(c)`,
		`COMMIT`,
		`PRAGMA page_count`,
		`PRAGMA freelist_count`,
		`PRAGMA integrity_check`,
		`SELECT type, name FROM sqlite_master ORDER BY name`,
	})
}

// TestVerbatimCopyRebuiltTableReclaimsOwnPage tests that a rebuilt table can reuse its own pages.
func TestVerbatimCopyRebuiltTableReclaimsOwnPage(t *testing.T) {
	differ(t, "verbatim-copy/rebuilt-table-reclaims-own-page", []string{
		`CREATE TABLE a(x)`,
		`CREATE TABLE b(y)`,
		`INSERT INTO b VALUES(1)`,
		`ALTER TABLE b ADD COLUMN z`,
		`PRAGMA page_count`,
		`PRAGMA freelist_count`,
		`PRAGMA integrity_check`,
		`SELECT * FROM b`,
	})
}

// TestVerbatimCopyRebuiltTableReclaimsOwnPageViaOutOfOrderInsert tests page reuse with out-of-order inserts.
func TestVerbatimCopyRebuiltTableReclaimsOwnPageViaOutOfOrderInsert(t *testing.T) {
	differ(t, "verbatim-copy/rebuilt-table-reclaims-own-page-out-of-order-insert", []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t1 VALUES (10,'a'),(20,'b')`,
		`CREATE TABLE t2(id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t2 VALUES (1,'a'),(2,'b')`,
		`INSERT INTO t1 VALUES (5,'mid')`,
		`PRAGMA page_count`,
		`PRAGMA freelist_count`,
		`PRAGMA integrity_check`,
		`SELECT id, v FROM t1 ORDER BY id`,
		`SELECT id, v FROM t2 ORDER BY id`,
	})
}
