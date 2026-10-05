// Package compat tests sqlite_sequence lifecycle in AUTOINCREMENT tables.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var seqCases = []struct {
	name  string
	stmts []string
}{
	{"create only", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`}},
	{"empty insert-select", []string{`CREATE TABLE s(x)`, `CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `INSERT INTO t SELECT 1,2 FROM s`}},
	{"insert then delete", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `INSERT INTO t VALUES(NULL,1)`, `DELETE FROM t`}},
	{"explicit rowid insert", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `INSERT INTO t VALUES(5,1)`}},
	{"failed insert", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b NOT NULL)`, `INSERT INTO t VALUES(NULL,NULL)`}},
	{"drop the table", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `INSERT INTO t VALUES(NULL,1)`, `DROP TABLE t`}},
	{"drop, other table remains", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `CREATE TABLE u(x)`, `INSERT INTO t VALUES(NULL,1)`, `DROP TABLE t`}},
	{"empty delete", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `DELETE FROM t`}},
}

func TestSqliteSequenceLifecycleParity(t *testing.T) {
	for _, tc := range seqCases {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
		for _, s := range tc.stmts {
			edb.Exec(s)
			cdb.Exec(s)
		}
		p, _ := edb.SnapshotPager()
		for _, q := range []string{`SELECT name, seq FROM sqlite_sequence ORDER BY name`, `SELECT count(*) FROM sqlite_master WHERE name='sqlite_sequence'`} {
			_, ev, eerr := p.QueryArgs(q, nil)
			_, cr, _ := cgoSelect(t, cdb, q, nil)
			if eerr != nil {
				t.Fatalf("[%s] %s: %v", tc.name, q, eerr)
			}
			eRows := engineRowsToStrings(ev)
			if len(eRows) != len(cr) {
				t.Errorf("[%s] %s row count %d vs %d\n  engine: %v\n  cgo:    %v", tc.name, q, len(eRows), len(cr), eRows, cr)
				continue
			}
			if len(eRows) == 0 {
				continue
			}
			cols := make([]string, len(eRows[0]))
			for i := range cols {
				cols[i] = fmt.Sprintf("c%d", i)
			}
			if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
				t.Errorf("[%s] %s DIVERGES: %s\n  engine: %v\n  cgo:    %v", tc.name, q, reason, eRows, cr)
			}
		}
		edb.Close()
		cdb.Close()
	}
}
