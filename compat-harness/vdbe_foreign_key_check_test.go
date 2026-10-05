// This file tests PRAGMA foreign_key_check conformance against C SQLite,
// verifying affinity-based parent matching, composite keys, error cases, and
// edge cases like virtual tables and views.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// fkCheckDDL is the shared schema+data script, run statement-by-statement
// against both engines.
var fkCheckDDL = []string{
	// Parents, one per way a foreign key can legally resolve.
	`CREATE TABLE p1(id INTEGER PRIMARY KEY, k TEXT)`, // rowid alias: no index at all
	`CREATE TABLE p2(a, b, UNIQUE(a,b))`,              // composite automatic index
	`CREATE TABLE p3(k TEXT COLLATE NOCASE UNIQUE)`,   // non-BINARY column collation
	`CREATE TABLE p4(x, y)`,                           // explicit CREATE UNIQUE INDEX
	`CREATE UNIQUE INDEX p4x ON p4(x)`,
	`CREATE TABLE pnu(z)`,              // no unique key: "foreign key mismatch"
	`CREATE TABLE ptxt(k TEXT UNIQUE)`, // TEXT affinity parent
	`CREATE TABLE pwr(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
	`CREATE VIEW pview AS SELECT 1 AS q`,
	`CREATE VIRTUAL TABLE pvtab USING fts4(a, b)`, // no CREATE TABLE text to parse, and never any foreign keys

	`CREATE TABLE c_rowid(v REFERENCES p1(id))`,
	`CREATE TABLE c_comp(x, y, FOREIGN KEY(x,y) REFERENCES p2(a,b))`,
	`CREATE TABLE c_comprev(x, y, FOREIGN KEY(x,y) REFERENCES p2(b,a))`, // parent cols in the OTHER order than the index
	`CREATE TABLE c_nocase(v TEXT REFERENCES p3(k))`,
	`CREATE TABLE c_uidx(v REFERENCES p4(x))`,
	`CREATE TABLE c_missing(v REFERENCES nosuchparent(z), w)`,
	`CREATE TABLE c_mismatch(v REFERENCES pnu(z))`,
	`CREATE TABLE c_aff(v REFERENCES ptxt(k))`,
	`CREATE TABLE c_self(id INTEGER PRIMARY KEY, par REFERENCES c_self(id))`,
	`CREATE TABLE c_multi(x REFERENCES p1(id), y REFERENCES ptxt(k))`,
	`CREATE TABLE c_wr(k TEXT PRIMARY KEY, f REFERENCES p1(id)) WITHOUT ROWID`,
	`CREATE TABLE c_pkimp(v REFERENCES pwr)`, // no parent column list: the parent's PRIMARY KEY
	`CREATE TABLE c_none(a, b)`,              // no foreign keys at all
	`CREATE TABLE c_view(v REFERENCES pview(q))`,

	`INSERT INTO p1 VALUES(1,'one'),(2,'two')`,
	`INSERT INTO p2 VALUES(1,2),(3,4)`,
	`INSERT INTO p3 VALUES('ABC')`,
	`INSERT INTO p4 VALUES(7,8)`,
	`INSERT INTO pnu VALUES(1)`,
	`INSERT INTO ptxt VALUES('1'),('zz')`,
	`INSERT INTO pwr VALUES('a',1)`,

	`INSERT INTO c_rowid VALUES(1),(9),(NULL),('2'),('9x')`,
	`INSERT INTO c_comp VALUES(1,2),(1,NULL),(NULL,NULL),(3,3)`,
	`INSERT INTO c_comprev VALUES(2,1),(4,3),(1,2)`,
	`INSERT INTO c_nocase VALUES('abc'),('ABC'),('zzz')`,
	`INSERT INTO c_uidx VALUES(7),(8)`,
	`INSERT INTO c_missing VALUES(1,1),(NULL,2),(3,3)`,
	`INSERT INTO c_mismatch VALUES(1)`,
	`INSERT INTO c_aff VALUES(1),('1'),('zz'),(NULL),(2)`,
	`INSERT INTO c_self VALUES(1,NULL),(2,1),(3,99)`,
	`INSERT INTO c_multi VALUES(1,'1'),(9,'1'),(1,'q'),(9,'q'),(NULL,NULL)`,
	`INSERT INTO c_wr VALUES('b',9),('a',1),('c',8)`,
	`INSERT INTO c_pkimp VALUES('a'),('b')`,
	`INSERT INTO c_none VALUES(1,2)`,
	`INSERT INTO c_view VALUES(1)`,
}

// fkCheckMatching are the statements both engines must ANSWER identically.
var fkCheckMatching = []string{
	"PRAGMA foreign_key_check(c_rowid)",
	"PRAGMA foreign_key_check(c_comp)",
	"PRAGMA foreign_key_check(c_comprev)",
	"PRAGMA foreign_key_check(c_nocase)",
	"PRAGMA foreign_key_check(c_uidx)",
	"PRAGMA foreign_key_check(c_missing)",
	"PRAGMA foreign_key_check(c_aff)",
	"PRAGMA foreign_key_check(c_self)",
	"PRAGMA foreign_key_check(c_multi)",
	"PRAGMA foreign_key_check(c_wr)",
	"PRAGMA foreign_key_check(c_pkimp)",
	"PRAGMA foreign_key_check(c_none)", // no foreign keys: zero rows, not an error
	"PRAGMA foreign_key_check(p1)",     // a pure parent: likewise
	"PRAGMA foreign_key_check(pview)",  // a VIEW name: zero rows, not an error
	"PRAGMA foreign_key_check(pvtab)",  // a VIRTUAL table: likewise
	"PRAGMA foreign_key_check(sqlite_master)",
	"PRAGMA main.foreign_key_check(c_rowid)", // schema-qualified
	"PRAGMA foreign_key_check('c_rowid')",    // string-literal argument
	"PRAGMA foreign_key_check=c_rowid",       // the "= value" spelling
	"PRAGMA foreign_key_check(C_ROWID)",      // case-insensitive argument, schema-cased output
	"PRAGMA foreign_key_list(c_multi)",       // fkid above must equal this id column
}

// fkCheckRejected are the statements both engines must reject.
var fkCheckRejected = []string{
	"PRAGMA foreign_key_check(nosuchtable)",
	"PRAGMA foreign_key_check(c_mismatch)",
	"PRAGMA foreign_key_check(c_view)",
}

// TestForeignKeyCheckMatchesCSQLite is the PRAGMA foreign_key_check conformance gate.
func TestForeignKeyCheckMatchesCSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fkcheck.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // one logical connection, so every statement lands on the same in-memory schema

	for _, s := range fkCheckDDL {
		if err := db.Exec(s); err != nil {
			t.Fatalf("engine Exec(%s): %v", s, err)
		}
		if _, err := sdb.Exec(s); err != nil {
			t.Fatalf("C SQLite Exec(%s): %v", s, err)
		}
	}

	// Test EXEC path for accept/decline consistency with the query path.
	for _, s := range fkCheckMatching {
		goErr := db.Exec(s)
		_, cErr := sdb.Exec(s)
		if (goErr != nil) != (cErr != nil) {
			t.Errorf("Exec(%s): engine err=%v, C SQLite err=%v (must agree)", s, goErr, cErr)
		}
	}
	for _, s := range fkCheckRejected {
		if err := db.Exec(s); err == nil {
			t.Errorf("Exec(%s): engine accepted a statement C SQLite rejects", s)
		}
		if _, err := sdb.Exec(s); err == nil {
			t.Fatalf("Exec(%s): C SQLite accepted it -- this fixture no longer tests what it claims to", s)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}
	rp, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer rp.Close()

	for _, q := range fkCheckMatching {
		assertPragmaEqual(t, rp, sdb, q)
	}
	for _, q := range fkCheckRejected {
		if _, _, err := rp.QueryArgs(q, nil); err == nil {
			t.Errorf("%s: engine answered a query C SQLite rejects", q)
		}
		if _, err := sdb.Query(q); err == nil {
			t.Fatalf("%s: C SQLite accepted it -- this fixture no longer tests what it claims to", q)
		}
	}

	// The unqualified form is deliberately declined (table order is non-deterministic).
	_, _, err = rp.QueryArgs("PRAGMA foreign_key_check", nil)
	if err == nil {
		t.Fatalf("PRAGMA foreign_key_check (unqualified): expected a clean decline, got rows")
	}
	if !strings.Contains(err.Error(), "foreign_key_check") {
		t.Fatalf("PRAGMA foreign_key_check (unqualified): decline should name the pragma, got %v", err)
	}
}
