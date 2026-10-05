// Tests PRAGMA with schema qualifiers (main., aux., temp.) against the oracle
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var pragmaQualifierCases = []string{
	`PRAGMA aux.schema_version = 30`,
	`PRAGMA aux.schema_version`,
	`PRAGMA aux.table_info(t)`,
	`PRAGMA aux.user_version = 1`,
	// Main database qualifies successfully
	`PRAGMA main.schema_version`,
	`PRAGMA main.user_version = 7`,
	`PRAGMA main.user_version`,
	`PRAGMA main.table_info(t)`,
	`PRAGMA user_version`,
}

func TestPragmaSchemaQualifierParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	if err := edb.Exec(`CREATE TABLE t(a,b)`); err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.Exec(`CREATE TABLE t(a,b)`); err != nil {
		t.Fatal(err)
	}

	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range pragmaQualifierCases {
		eErr := edb.Exec(s)
		_, cErr := cdb.Exec(s)
		if (eErr == nil) != (cErr == nil) {
			t.Errorf("[exec %s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
		} else if eErr != nil {
			if got := strings.TrimPrefix(eErr.Error(), "engine: "); got != cErr.Error() {
				t.Errorf("[exec %s] error text mismatch\n  engine: %q\n  cgo:    %q", s, got, cErr.Error())
			}
		}
		// Read path must agree with write path
		_, _, qErr := p.QueryArgs(s, nil)
		if (qErr == nil) != (eErr == nil) {
			t.Errorf("[query %s] read path disagrees with write path\n  query=%v\n  exec=%v", s, qErr, eErr)
		}
	}
}

// TestPragmaTempQualifierAnswersTheTempDatabase verifies temp database pragma
// handling with the temp. qualifier
func TestPragmaTempQualifierAnswersTheTempDatabase(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA temp.user_version`,
		`PRAGMA temp.application_id`,
		`PRAGMA temp.page_count`,
		`PRAGMA temp.freelist_count`,
		`PRAGMA temp.schema_version`,
	} {
		_, rows, qErr := p.QueryArgs(q, nil)
		var want int64
		cErr := cdb.QueryRow(q).Scan(&want)
		if qErr != nil || cErr != nil {
			t.Errorf("[%s] engine=%v cgo=%v; both must answer", q, qErr, cErr)
			continue
		}
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].I != want {
			t.Errorf("[%s] engine=%v, cgo=%d", q, rows, want)
		}
	}
	if err := edb.Exec(`PRAGMA temp.user_version=7`); err != nil {
		t.Fatalf("PRAGMA temp.user_version=7: %v", err)
	}
	p2, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		q    string
		want int64
	}{{`PRAGMA temp.user_version`, 7}, {`PRAGMA main.user_version`, 0}, {`PRAGMA temp.page_count`, 1}} {
		_, rows, qErr := p2.QueryArgs(c.q, nil)
		if qErr != nil || len(rows) != 1 || rows[0][0].I != c.want {
			t.Errorf("after the temp setter, %s = %v (err %v), want %d", c.q, rows, qErr, c.want)
		}
	}
}
