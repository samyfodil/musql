package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestDMLSurfaceSweep is a comprehensive sweep of INSERT, UPDATE, and DELETE
// forms across schema shapes, verifying row counts and integrity.
func TestDMLSurfaceSweep(t *testing.T) {
	stmts := []string{
		`INSERT INTO t VALUES(3,'z')`,
		`INSERT INTO t(a) VALUES(3)`,
		`INSERT INTO t DEFAULT VALUES`,
		`INSERT INTO t SELECT * FROM t`,
		`INSERT INTO t VALUES(3,'z'),(4,'w')`,
		`INSERT OR REPLACE INTO t VALUES(1,'z')`,
		`INSERT OR IGNORE INTO t VALUES(1,'z')`,
		`INSERT OR ABORT INTO t VALUES(1,'z')`,
		`INSERT OR FAIL INTO t VALUES(1,'z')`,
		`INSERT OR ROLLBACK INTO t VALUES(1,'z')`,
		`REPLACE INTO t VALUES(1,'z')`,
		`INSERT INTO t VALUES(1,'z') ON CONFLICT DO NOTHING`,
		`INSERT INTO t VALUES(1,'z') ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
		`INSERT INTO t VALUES(1,'z') ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE t.a>0`,
		`INSERT INTO t VALUES(3,'z') RETURNING *`,
		`INSERT INTO t VALUES(3,'z') RETURNING a, b, rowid`,
		`INSERT INTO t VALUES(3,'z') RETURNING a AS q, a+1`,
		`UPDATE t SET b='q'`,
		`UPDATE t SET b='q' WHERE a=1`,
		`UPDATE t SET (a,b)=(9,'q') WHERE a=1`,
		`UPDATE OR REPLACE t SET a=1`,
		`UPDATE OR IGNORE t SET a=1`,
		`UPDATE t SET b=(SELECT max(b) FROM t)`,
		`UPDATE t SET b='q' RETURNING *`,
		`UPDATE t SET b='q' WHERE rowid IN (SELECT rowid FROM t LIMIT 1)`,
		`UPDATE t SET b=x.b FROM (SELECT 1 AS a, 'zz' AS b) AS x WHERE t.a=x.a`,
		`DELETE FROM t`,
		`DELETE FROM t WHERE a=1`,
		`DELETE FROM t RETURNING *`,
		`DELETE FROM t WHERE a IN (SELECT a FROM t)`,
		`WITH q(x) AS (VALUES(1)) DELETE FROM t WHERE a IN (SELECT x FROM q)`,
		`WITH q(x) AS (VALUES(9)) INSERT INTO t(a) SELECT x FROM q`,
		`WITH q(x) AS (VALUES(9)) UPDATE t SET a=(SELECT x FROM q)`,
		`INSERT INTO t(a,b) VALUES(3,'z') ON CONFLICT DO NOTHING RETURNING *`,
		`UPDATE t SET a=a`,
		`DELETE FROM t WHERE 0`,
		`INSERT INTO t(rowid,a,b) VALUES(77,7,'s')`,
		`UPDATE t SET rowid=99 WHERE a=1`,
	}
	probes := []string{
		`SELECT rowid,* FROM t ORDER BY rowid`, `SELECT count(*) FROM t`,
		`SELECT changes(), total_changes()`, `PRAGMA integrity_check`,
	}
	for sname, setup := range ddlSweepSchemas {
		if sname == "vtab" || sname == "temptable" {
			continue // covered by their own suites; the shadow tables make the diff unreadable
		}
		for si, st := range stmts {
			sname, setup, st := sname, setup, st
			t.Run(fmt.Sprintf("%s/%d", sname, si), func(t *testing.T) {
				var out [2]string
				for i, drv := range []string{"sqlite3", "sqlite"} {
					p := filepath.Join(t.TempDir(), "x.db")
					db, _ := sql.Open(drv, p)
					db.SetMaxOpenConns(1)
					for _, s := range setup {
						if _, err := db.Exec(s); err != nil {
							out[i] += "SETUP-ERR "
						}
					}
					out[i] += "q=" + renderQuery(db, st) + " | "
					_, derr := db.Exec(st)
					out[i] += fmt.Sprintf("e=%v | ", derr != nil)
					for _, q := range probes {
						out[i] += renderQuery(db, q) + " | "
					}
					db.Close()
				}
				if out[0] != out[1] {
					t.Errorf("[%s] %s\n  cgo: %s\n  mus: %s", sname, st, out[0], out[1])
				}
			})
		}
	}
}
