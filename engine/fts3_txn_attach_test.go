package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFts3OptimizeTaintAttachedNameCollisionDeclines verifies FTS3 tainting with attached databases.
func TestFts3OptimizeTaintAttachedNameCollisionDeclines(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	aux := filepath.Join(dir, "aux.musq")
	for _, s := range []string{
		`CREATE VIRTUAL TABLE ft USING fts3(x)`,
		`ATTACH '` + aux + `' AS aux`,
		`CREATE TABLE aux.ft(y UNIQUE)`,
		`BEGIN`,
		`INSERT INTO ft VALUES('one')`,
		`INSERT INTO aux.ft VALUES('a'),('b')`,
		`INSERT INTO ft VALUES('two')`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetAttachedReadersForTest(db)
	_, rows, qerr := p.QueryArgs(`SELECT optimize(ft) FROM ft LIMIT 1`, nil)
	if qerr == nil {
		t.Fatalf("optimize() ANSWERED %v after a write routed to an ATTACHed database's same-named table -- fts3TxnMaybeTaint should never treat a schema-qualified write as the local table's own DML", rows)
	}
	if !strings.Contains(qerr.Error(), "optimize()") {
		t.Errorf("optimize() error = %v, want the usual fts3_optimize.go decline text", qerr)
	}
}
