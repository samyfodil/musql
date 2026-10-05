// Tests for instr() matching on character boundaries, not raw bytes, and blob
// handling.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var instrQ = []string{
	`SELECT instr('xä€y', x'a4')`,
	`SELECT instr('xabcy', x'62')`,
	`SELECT instr(x'0102030405', x'0304')`,
	`SELECT instr('xabcy', 'bc')`,
	`SELECT instr('xä€y', 'ä')`,
	`SELECT instr(x'010203', 'x')`,
	`SELECT instr('abc', x'')`,
	`SELECT instr(x'616263', x'62')`,
	`SELECT instr('aäbä', 'ä')`,
	`SELECT instr('abc', 'd')`,
}

func TestInstrCharacterBoundaryParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	p, _ := edb.SnapshotPager()
	for _, q := range instrQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, _ := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			t.Fatalf("[%s] %v", q, eerr)
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch([]string{"n"}, eRows, []string{"n"}, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
	// CREATE INDEX on a non-temp table with temp schema qualifier must be
	// rejected.
	edb2, err := engine.Create(filepath.Join(t.TempDir(), "e2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb2.Close()
	if err := edb2.Exec(`CREATE TABLE t6(c)`); err != nil {
		t.Fatal(err)
	}
	if err := edb2.Exec(`CREATE INDEX temp.i21 ON t6(c)`); err == nil {
		t.Error(`CREATE INDEX temp.i21 was accepted; it must decline`)
	}
}
