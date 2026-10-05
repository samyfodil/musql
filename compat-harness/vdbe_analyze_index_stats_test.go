// ANALYZE on expression and partial indexes.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var analyzeIdxCases = []struct {
	name  string
	stmts []string
}{
	{"partial indexes", []string{`CREATE TABLE t1(a,b,c)`, `CREATE INDEX t1a ON t1(a) WHERE a IS NOT NULL`, `CREATE INDEX t1b ON t1(b) WHERE b>10`, `INSERT INTO t1 VALUES(1,11,1),(2,12,2),(NULL,3,3)`, `ANALYZE`}},
	{"plain indexes", []string{`CREATE TABLE t1(a,b,c)`, `CREATE INDEX t1a ON t1(a)`, `CREATE INDEX t1b ON t1(b)`, `INSERT INTO t1 VALUES(1,11,1),(2,12,2),(NULL,3,3)`, `ANALYZE`}},
	{"expression index", []string{`CREATE TABLE t1(a,b,c)`, `CREATE INDEX t1x ON t1(a+b)`, `INSERT INTO t1 VALUES(1,11,1),(2,12,2)`, `ANALYZE`}},
	{"empty partial index", []string{`CREATE TABLE t1(a,b)`, `CREATE INDEX i ON t1(a) WHERE a>100`, `INSERT INTO t1 VALUES(1,1),(2,2)`, `ANALYZE`}},
	{"partial + full index", []string{`CREATE TABLE t1(a,b)`, `CREATE INDEX ip ON t1(a) WHERE a>1`, `CREATE INDEX ifull ON t1(b)`, `INSERT INTO t1 VALUES(1,1),(2,2),(3,3)`, `ANALYZE`}},
	{"multi-col expression index", []string{`CREATE TABLE t1(a,b)`, `CREATE INDEX i ON t1(a+b, b)`, `INSERT INTO t1 VALUES(1,1),(2,2),(1,1)`, `ANALYZE`}},
	// key that is a plain COLUMN takes statColValue, not a compiled program --
	// so none of them could tell the per-key programs apart, and a mutation
	// that used keyProgs[0] for every key passed the whole file.
	{"two expression keys", []string{`CREATE TABLE t1(a,b)`, `CREATE INDEX i ON t1(a+b, a-b)`, `INSERT INTO t1 VALUES(1,1),(1,2),(2,1),(2,2)`, `ANALYZE`}},
	{"two expression keys, partial", []string{`CREATE TABLE t1(a,b)`, `CREATE INDEX i ON t1(a+b, a-b) WHERE a>0`, `INSERT INTO t1 VALUES(1,1),(1,2),(2,1),(2,2),(0,9),(-1,9)`, `ANALYZE`}},
	{"three expression keys", []string{`CREATE TABLE t1(a,b,c)`, `CREATE INDEX i ON t1(a+b, a-b, b*c)`, `INSERT INTO t1 VALUES(1,1,1),(1,2,2),(2,1,3),(2,2,4),(1,1,5)`, `ANALYZE`}},
	{"partial where truthiness", []string{`CREATE TABLE t1(a,b)`, `CREATE INDEX i ON t1(a) WHERE b`, `INSERT INTO t1 VALUES(1,1),(2,0),(3,NULL)`, `ANALYZE`}},
	{"partial on nocase col", []string{`CREATE TABLE t1(a TEXT COLLATE NOCASE,b)`, `CREATE INDEX i ON t1(a) WHERE b>0`, `INSERT INTO t1 VALUES('x',1),('X',1),('y',1)`, `ANALYZE`}},
	// SQLite's own rule: "always record an entry for a partial index, even if
	// it is empty" -- so an EMPTY table still yields a row per PARTIAL index
	// ("0 0"), and nothing at all for a plain/UNIQUE index or the table
	{"empty table, partial indexes", []string{`CREATE TABLE t1(a,b,c)`, `CREATE INDEX t1a ON t1(a) WHERE a IS NOT NULL`, `CREATE INDEX t1b ON t1(b) WHERE b>10`, `ANALYZE`}},
	{"empty table, plain index", []string{`CREATE TABLE t1(a,b,c)`, `CREATE INDEX t1a ON t1(a)`, `ANALYZE`}},
	{"empty table, unique index", []string{`CREATE TABLE t1(a UNIQUE,b)`, `ANALYZE`}},
	{"empty table, no index", []string{`CREATE TABLE t1(a,b,c)`, `ANALYZE`}},
	{"empty table, expression index", []string{`CREATE TABLE t1(a,b)`, `CREATE INDEX i ON t1(a+b)`, `ANALYZE`}},
	// A WITHOUT ROWID table's PRIMARY KEY IS an index: it yields an ordinary
	// stat row -- recorded under the TABLE's own name, since that PK has no
	// sqlite_schema index row to name it -- and, covering every row, means
	// there is never an idx=NULL row for such a table (index7.test).
	{"without rowid, rows, no index", []string{`CREATE TABLE t1(a,b,c PRIMARY KEY) WITHOUT rowid`, `INSERT INTO t1 VALUES(1,2,3),(9,4,5)`, `ANALYZE`}},
	{"without rowid, plain index", []string{`CREATE TABLE t1(a,b,c PRIMARY KEY) WITHOUT rowid`, `CREATE INDEX t1a ON t1(a)`, `INSERT INTO t1 VALUES(1,2,3),(NULL,4,5)`, `ANALYZE`}},
	{"without rowid, partial index", []string{`CREATE TABLE t1(a,b,c PRIMARY KEY) WITHOUT rowid`, `CREATE INDEX t1a ON t1(a) WHERE a IS NOT NULL`, `INSERT INTO t1 VALUES(1,2,3),(NULL,4,5)`, `ANALYZE`}},
	{"without rowid, multi-col pk", []string{`CREATE TABLE t1(a,b,c, PRIMARY KEY(a,b)) WITHOUT rowid`, `INSERT INTO t1 VALUES(1,2,3),(1,4,5)`, `ANALYZE`}},
	{"without rowid, empty, partial", []string{`CREATE TABLE t1(a,b,c PRIMARY KEY) WITHOUT rowid`, `CREATE INDEX t1a ON t1(a) WHERE a IS NOT NULL`, `CREATE INDEX t1b ON t1(b) WHERE b>10`, `ANALYZE`}},
	{"without rowid, empty, no index", []string{`CREATE TABLE t1(a,b,c PRIMARY KEY) WITHOUT rowid`, `ANALYZE`}},
	{"explicit collate key", []string{`CREATE TABLE t1(a TEXT,b)`, `CREATE INDEX i ON t1(a COLLATE NOCASE) WHERE b>0`, `INSERT INTO t1 VALUES('x',1),('X',1),('y',1)`, `ANALYZE`}},
}

func TestAnalyzeIndexStatsParity(t *testing.T) {
	for _, tc := range analyzeIdxCases {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
		for _, s := range tc.stmts {
			eerr := edb.Exec(s)
			_, cerr := cdb.Exec(s)
			if (eerr == nil) != (cerr == nil) {
				t.Logf("  !! %-52s eng=%v | cgo=%v", s, eerr, cerr)
			}
		}
		p, _ := edb.SnapshotPager()
		q := `SELECT tbl, ifnull(idx,'<NULL>'), stat FROM sqlite_stat1 ORDER BY idx`
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, _ := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			t.Fatalf("[%s] %v", tc.name, eerr)
		}
		eRows := engineRowsToStrings(ev)
		cols := []string{"tbl", "idx", "stat"}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] sqlite_stat1 DIVERGES: %s\n  engine: %v\n  cgo:    %v", tc.name, reason, eRows, cr)
		}
		edb.Close()
		cdb.Close()
	}
}

// TestIndexOnInternalTableRejected: SQLite's own internal tables may not be
// indexed, whatever they are and whether or not they currently exist --
// "table sqlite_stat1 may not be indexed" (analyze.test), same wording for
// sqlite_master and sqlite_sequence.
func TestIndexOnInternalTableRejected(t *testing.T) {
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
	for _, s := range []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`CREATE INDEX ti ON t1(b)`, `INSERT INTO t1 VALUES(1,2)`, `ANALYZE`,
	} {
		if err := edb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	for _, s := range []string{
		`CREATE INDEX i1 ON sqlite_stat1(idx)`,
		`CREATE INDEX i2 ON sqlite_master(name)`,
		`CREATE INDEX i3 ON sqlite_sequence(name)`,
		`CREATE INDEX i4 ON t1(a)`, // an ordinary table is unaffected
	} {
		eErr := edb.Exec(s)
		_, cErr := cdb.Exec(s)
		if (eErr == nil) != (cErr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
		} else if eErr != nil {
			if got := strings.TrimPrefix(eErr.Error(), "engine: "); got != cErr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", s, got, cErr.Error())
			}
		}
	}
}
