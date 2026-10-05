// Tests partial-index WHERE clauses with table qualifiers, especially during
// ALTER TABLE RENAME. Table names in qualifiers must be rewritten, database
// parts left as-is, column names unchanged.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestFtsR25PartialIndexOwnTableQualifier is the mined shape itself, plus the
// forms around it, run through a write history so a wrongly-built index shows
// up as a divergent READ and not only as an accepted CREATE.
func TestFtsR25PartialIndexOwnTableQualifier(t *testing.T) {
	for _, c := range []struct {
		name  string
		where string
	}{
		{"mined-3part-bogus-db", `xyzzy.t3.b BETWEEN 5 AND 10`},
		{"2part", `t3.b BETWEEN 5 AND 10`},
		{"3part-main", `main.t3.b BETWEEN 5 AND 10`},
		{"quoted-qualifier", `"t3".b BETWEEN 5 AND 10`},
		{"case-folded-qualifier", `T3.B BETWEEN 5 AND 10`},
		{"mixed-with-unqualified", `t3.b > 4 AND c < 9`},
		{"two-qualifiers", `t3.b > 4 AND main.t3.c < 9`},
		{"qualified-no-such-column", `t3.nope > 1`},
		{"qualified-other-real-table", `t4.b > 1`},
	} {
		flLockstep(t, "partial-where-own-qualifier-"+c.name, []string{
			`CREATE TABLE t3(a,b,c)`,
			`CREATE TABLE t4(a,b,c)`,
			`INSERT INTO t3 VALUES(1,5,1),(2,6,2),(3,7,3),(4,20,4)`,
			`CREATE INDEX t3b ON t3(b) WHERE ` + c.where,
			`INSERT INTO t3 VALUES(5,8,5),(6,90,6)`,
			`UPDATE t3 SET b=9 WHERE a=1`,
			`DELETE FROM t3 WHERE a=2`,
		}, `SELECT * FROM t3 ORDER BY a`,
			`SELECT count(*) FROM t3 WHERE t3.b BETWEEN 5 AND 10`,
			`SELECT name, sql FROM sqlite_master WHERE type='index' ORDER BY name`)
	}
}

// TestFtsR25PartialIndexQualifierRename is the cascade: the stored SQL after
// ALTER TABLE RENAME TO must match the oracle's TEXT, not merely parse, and
// the index must still admit the right rows afterwards.
func TestFtsR25PartialIndexQualifierRename(t *testing.T) {
	flLockstep(t, "partial-where-qualifier-rename", []string{
		`CREATE TABLE t3(a,b,c)`,
		`INSERT INTO t3 VALUES(1,5,1),(2,6,2),(3,7,3),(4,20,4)`,
		`CREATE INDEX q3 ON t3(b) WHERE T3.B>1`,
		`CREATE INDEX q4 ON t3(b) WHERE "t3".b>1 AND main.t3.c<9`,
		`CREATE INDEX q7 ON t3(c) WHERE t3.b>4`,
		`ALTER TABLE t3 RENAME TO t9`,
		// Writes AFTER the rename: the index has to still materialize, which
		// it cannot if its parsed WHERE kept the old qualifier.
		`INSERT INTO t9 VALUES(7,6,7),(8,90,8)`,
		`UPDATE t9 SET b=9 WHERE a=1`,
		`DELETE FROM t9 WHERE a=2`,
		`ALTER TABLE t9 RENAME TO t3`,
		`INSERT INTO t3 VALUES(9,6,9)`,
	}, `SELECT * FROM t3 ORDER BY a`,
		`SELECT count(*) FROM t3 WHERE t3.b>4`,
		`SELECT name, sql FROM sqlite_master WHERE type='index' ORDER BY name`)
}

// TestFtsR25PartialIndexQualifierReopens is the half flLockstep cannot see:
// the renamed index's stored text has to be re-derivable by a FRESH OpenWrite,
// and C SQLite has to agree the file is sound.
func TestFtsR25PartialIndexQualifierReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t3(a INTEGER PRIMARY KEY, b INT, c INT)`,
		`INSERT INTO t3 VALUES(1,5,1),(2,6,2),(3,20,3)`,
		`CREATE INDEX q1 ON t3(b) WHERE xyzzy.t3.b BETWEEN 5 AND 10`,
		`CREATE INDEX q2 ON t3(c) WHERE "t3".b>4 AND main.t3.c<9`,
		`ALTER TABLE t3 RENAME TO t9`,
		`INSERT INTO t9 VALUES(4,7,4)`,
	} {
		if e := edb.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	if e := edb.Close(); e != nil {
		t.Fatalf("Close: %v", e)
	}

	edb2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite after a renamed qualified partial index: %v", err)
	}
	if e := edb2.Exec(`INSERT INTO t9 VALUES(5,8,5)`); e != nil {
		t.Fatalf("insert after reopen: %v", e)
	}
	if e := edb2.Close(); e != nil {
		t.Fatalf("Close 2: %v", e)
	}

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	var ic string
	if e := cdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); e != nil {
		t.Fatalf("integrity_check: %v", e)
	}
	if ic != "ok" {
		t.Errorf("C SQLite integrity_check on the reopened file = %q, want ok", ic)
	}
	var got string
	if e := cdb.QueryRow(`SELECT sql FROM sqlite_master WHERE name='q1'`).Scan(&got); e != nil {
		t.Fatalf("sql: %v", e)
	}
	const want = `CREATE INDEX q1 ON "t9"(b) WHERE xyzzy."t9".b BETWEEN 5 AND 10`
	if got != want {
		t.Errorf("stored index SQL after rename = %q, want %q", got, want)
	}
	var n int
	if e := cdb.QueryRow(`SELECT count(*) FROM t9 WHERE b BETWEEN 5 AND 10`).Scan(&n); e != nil {
		t.Fatalf("count: %v", e)
	}
	if n != 4 {
		t.Errorf("C SQLite reads %d rows with b BETWEEN 5 AND 10, want 4", n)
	}
}

// TestFtsR25PartialIndexRenameColumn gates the SECOND bug this work uncovered,
// which predates the qualifier entirely and is the dangerous one: for an
// expression/partial index, indexMeta.cols is the DISTINCT SET of referenced
// columns in TABLE-declaration order, not the key list -- so renameColumn's
// "locateIndexColumnToken(ix.sql, k)" splice, which indexes by KEY POSITION,
// rewrote the WRONG token. Over t(a,b) with "CREATE INDEX ip ON t(b) WHERE
// a>1", renaming a -> aa hit k=0 (a's slot in cols) and spliced the FIRST KEY
// token, which is b: the stored SQL became "ON t(aa) WHERE a>1" and the index
// could no longer be materialized at all ("index ip: WHERE: no such column:
// a") -- a database this engine wrote and could not even CLOSE.
//
// Both halves are now token-rewritten (indexColumnRefTokens), matching the
// oracle: "CREATE INDEX ip ON t(a) WHERE b>1" becomes "... WHERE bb>1" after
// "ALTER TABLE t RENAME COLUMN b TO bb".
func TestFtsR25PartialIndexRenameColumn(t *testing.T) {
	for _, c := range []struct {
		name  string
		index string
		alter string
	}{
		{"where-only-reference", `CREATE INDEX ip ON t(a) WHERE b>1`, `ALTER TABLE t RENAME COLUMN b TO bb`},
		{"key-splice-hazard", `CREATE INDEX ip ON t(b) WHERE a>1`, `ALTER TABLE t RENAME COLUMN a TO aa`},
		{"qualified-where", `CREATE INDEX ip ON t(b) WHERE t.a>1`, `ALTER TABLE t RENAME COLUMN a TO aa`},
		{"key-expression", `CREATE INDEX ip ON t(a+b) WHERE b>1`, `ALTER TABLE t RENAME COLUMN b TO bb`},
		{"both-sides", `CREATE INDEX ip ON t(b) WHERE b>1`, `ALTER TABLE t RENAME COLUMN b TO bb`},
		{"untouched-column", `CREATE INDEX ip ON t(b) WHERE a>1`, `ALTER TABLE t RENAME COLUMN b TO bb`},
	} {
		flLockstep(t, "partial-index-rename-column-"+c.name, []string{
			`CREATE TABLE t(a,b)`,
			`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
			c.index,
			c.alter,
			`INSERT INTO t VALUES(4,40)`,
			`DELETE FROM t WHERE a=1 OR aa=1`,
		}, `SELECT * FROM t ORDER BY 1`,
			`SELECT name, sql FROM sqlite_master ORDER BY name`)
	}
}

// TestFtsR25PartialIndexRenameColumnReopens is the same claim through a real
// file: after the rename the database must still CLOSE (which materializes
// every index) and REOPEN, and C SQLite must agree it is sound.
func TestFtsR25PartialIndexRenameColumnReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc.sqlite")
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b INT)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`CREATE INDEX ip ON t(b) WHERE t.a>1`,
		`ALTER TABLE t RENAME COLUMN a TO aa`,
		`INSERT INTO t VALUES(4,40)`,
	} {
		if e := edb.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	if e := edb.Close(); e != nil {
		t.Fatalf("Close after RENAME COLUMN over a partial index: %v", e)
	}
	edb2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if e := edb2.Exec(`INSERT INTO t VALUES(5,50)`); e != nil {
		t.Fatalf("insert after reopen: %v", e)
	}
	if e := edb2.Close(); e != nil {
		t.Fatalf("Close 2: %v", e)
	}
	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	var ic string
	if e := cdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); e != nil {
		t.Fatalf("integrity_check: %v", e)
	}
	if ic != "ok" {
		t.Errorf("C SQLite integrity_check = %q, want ok", ic)
	}
	var got string
	if e := cdb.QueryRow(`SELECT sql FROM sqlite_master WHERE name='ip'`).Scan(&got); e != nil {
		t.Fatalf("sql: %v", e)
	}
	const want = `CREATE INDEX ip ON t(b) WHERE t.aa>1`
	if got != want {
		t.Errorf("stored index SQL after RENAME COLUMN = %q, want %q", got, want)
	}
}
