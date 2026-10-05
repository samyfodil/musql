package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestUpsertTargetMatrix sweeps ON CONFLICT targets across schema shapes
// including INTEGER PRIMARY KEY, WITHOUT ROWID, partial indexes, and COLLATE
func TestUpsertTargetMatrix(t *testing.T) {
	schemas := map[string][]string{
		"ipk":      {`CREATE TABLE t(id INTEGER PRIMARY KEY, a, b UNIQUE)`, `INSERT INTO t VALUES(1,10,'x')`},
		"rowidonly": {`CREATE TABLE t(a, b UNIQUE)`, `INSERT INTO t VALUES(10,'x')`},
		"wr":       {`CREATE TABLE t(k TEXT PRIMARY KEY, a, b UNIQUE) WITHOUT ROWID`, `INSERT INTO t VALUES('k1',10,'x')`},
		"partial":  {`CREATE TABLE t(a, b)`, `CREATE UNIQUE INDEX i ON t(a) WHERE b>0`, `INSERT INTO t VALUES(1,5)`},
		"multi":    {`CREATE TABLE t(a, b, c, UNIQUE(a,b))`, `INSERT INTO t VALUES(1,2,3)`},
		"collate":  {`CREATE TABLE t(a COLLATE NOCASE UNIQUE, b)`, `INSERT INTO t VALUES('X',1)`},
	}
	stmts := map[string][]string{
		"ipk": {
			`INSERT INTO t VALUES(1,99,'y') ON CONFLICT(id) DO UPDATE SET a=excluded.a`,
			`INSERT INTO t VALUES(1,99,'y') ON CONFLICT(rowid) DO NOTHING`,
			`INSERT INTO t VALUES(1,99,'y') ON CONFLICT(oid) DO NOTHING`,
			`INSERT INTO t VALUES(1,99,'y') ON CONFLICT(_rowid_) DO NOTHING`,
			`INSERT INTO t VALUES(1,99,'y') ON CONFLICT(b) DO UPDATE SET a=a+excluded.a WHERE t.a<100`,
			`INSERT INTO t VALUES(2,99,'x') ON CONFLICT(b) DO UPDATE SET a=excluded.a RETURNING id,a,b`,
			`INSERT INTO t VALUES(1,99,'y') ON CONFLICT DO NOTHING`,
			`INSERT INTO t(id,a,b) VALUES(1,1,'z') ON CONFLICT(id) DO UPDATE SET id=id+100`,
		},
		"rowidonly": {
			`INSERT INTO t VALUES(99,'x') ON CONFLICT(rowid) DO NOTHING`,
			`INSERT INTO t VALUES(99,'x') ON CONFLICT(b) DO UPDATE SET a=excluded.a`,
			`INSERT INTO t VALUES(99,'x') ON CONFLICT(oid) DO UPDATE SET a=1`,
		},
		"wr": {
			`INSERT INTO t VALUES('k1',99,'y') ON CONFLICT(k) DO UPDATE SET a=excluded.a`,
			`INSERT INTO t VALUES('k1',99,'y') ON CONFLICT(rowid) DO NOTHING`,
			`INSERT INTO t VALUES('k2',99,'x') ON CONFLICT(b) DO UPDATE SET a=excluded.a`,
		},
		"partial": {
			`INSERT INTO t VALUES(1,5) ON CONFLICT(a) WHERE b>0 DO UPDATE SET b=99`,
			`INSERT INTO t VALUES(1,5) ON CONFLICT(a) DO UPDATE SET b=99`,
			`INSERT INTO t VALUES(1,-5) ON CONFLICT(a) WHERE b>0 DO NOTHING`,
			`INSERT INTO t VALUES(1,5) ON CONFLICT(a) WHERE b>1 DO NOTHING`,
		},
		"multi": {
			`INSERT INTO t VALUES(1,2,9) ON CONFLICT(a,b) DO UPDATE SET c=excluded.c`,
			`INSERT INTO t VALUES(1,2,9) ON CONFLICT(b,a) DO UPDATE SET c=excluded.c`,
			`INSERT INTO t VALUES(1,2,9) ON CONFLICT(a) DO NOTHING`,
			`INSERT INTO t VALUES(1,2,9) ON CONFLICT(a,b,c) DO NOTHING`,
		},
		"collate": {
			`INSERT INTO t VALUES('x',2) ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
			`INSERT INTO t VALUES('x',2) ON CONFLICT(a COLLATE NOCASE) DO UPDATE SET b=99`,
			`INSERT INTO t VALUES('x',2) ON CONFLICT(a COLLATE BINARY) DO UPDATE SET b=99`,
		},
	}
	for name, setup := range schemas {
		name, setup := name, setup
		for _, q := range stmts[name] {
			q := q
			var out [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				db, _ := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
				db.SetMaxOpenConns(1)
				for _, s := range setup {
					if _, err := db.Exec(s); err != nil {
						t.Fatalf("[%s] %s: %v", name, s, err)
					}
				}
				res := "ok"
				if _, err := db.Exec(q); err != nil {
					res = "ERR:" + err.Error()
				}
				out[i] = res + " || " + renderQuery(db, `SELECT * FROM t ORDER BY 1,2`)
				db.Close()
			}
			if out[0] != out[1] {
				t.Errorf("[%s] %s\n  cgo: %s\n  mus: %s", name, q, out[0], out[1])
			}
		}
	}
}
