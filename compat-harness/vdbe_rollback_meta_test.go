// TestRollbackKeepsTableMetadata verifies that ROLLBACK preserves table metadata
// like CHECK constraints, AUTOINCREMENT sequences, and TEMP table status.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestRollbackKeepsTableMetadata(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Discard()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cdb.SetMaxOpenConns(1)
	defer cdb.Close()

	for _, q := range []string{
		`CREATE TABLE c(a CHECK(a > 0))`,
		`CREATE TABLE ai(id INTEGER PRIMARY KEY AUTOINCREMENT, v)`,
		`CREATE TEMP TABLE tt(x)`,
		`INSERT INTO ai(v) VALUES('one')`,
		`INSERT INTO ai(v) VALUES('two')`,
		`DELETE FROM ai`,
		`BEGIN`,
		`INSERT INTO c VALUES(1)`,
		`ROLLBACK`,
		// Verify CHECK, AUTOINCREMENT, and TEMP table status survive rollback.
		`INSERT INTO c VALUES(-1)`,
		`INSERT INTO ai(v) VALUES('three')`,
		`INSERT INTO tt VALUES(9)`,
	} {
		eerr := edb.Exec(q)
		_, cerr := cdb.Exec(q)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine: %v\n  cgo:    %v", q, eerr, cerr)
		}
	}

	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		q    string
		want string
	}{
		{`SELECT count(*) FROM c`, "0"}, // the rolled-back row is gone, the -1 was rejected
		{`SELECT id FROM ai`, "3"},      // the sequence survived the rollback
		{`SELECT count(*) FROM temp.tt`, "1"},
	} {
		_, rows, qerr := p.QueryArgs(tc.q, nil)
		if qerr != nil {
			t.Errorf("[%s]: %v", tc.q, qerr)
			continue
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Errorf("[%s]: got %d row(s), want one 1-column row", tc.q, len(rows))
			continue
		}
		var cgoWant string
		if err := cdb.QueryRow(tc.q).Scan(&cgoWant); err != nil {
			t.Fatalf("[%s] cgo: %v", tc.q, err)
		}
		if cgoWant != tc.want {
			t.Errorf("[%s]: cgo says %q, this test expected %q", tc.q, cgoWant, tc.want)
		}
		if got := fmt.Sprint(rows[0][0].I); got != cgoWant {
			t.Errorf("[%s]\n  engine: %s\n  cgo:    %s", tc.q, got, cgoWant)
		}
	}
}
