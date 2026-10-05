package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

var ddlSweepSchemas = map[string][]string{
	"plain":     {`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
	"pk":        {`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
	"norowid":   {`CREATE TABLE t(a TEXT PRIMARY KEY, b) WITHOUT ROWID`, `INSERT INTO t VALUES('k','x')`},
	"gencol":    {`CREATE TABLE t(a,b, c AS (a+1), d AS (b||'!') STORED)`, `INSERT INTO t(a,b) VALUES(1,'x')`},
	"strict":    {`CREATE TABLE t(a INT, b TEXT) STRICT`, `INSERT INTO t VALUES(1,'x')`},
	"collate":   {`CREATE TABLE t(a COLLATE NOCASE, b)`, `INSERT INTO t VALUES('A','x')`},
	"view":      {`CREATE TABLE t(a,b)`, `CREATE VIEW vv AS SELECT a,b FROM t`, `INSERT INTO t VALUES(1,'x')`},
	"vtab":      {`CREATE VIRTUAL TABLE t USING fts4(a,b)`, `INSERT INTO t VALUES('one two','x')`},
	"temptable": {`CREATE TEMP TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x')`},
}

var ddlSweepStatements = []string{
	`CREATE INDEX i ON t(a)`,
	`CREATE INDEX i ON t(a DESC)`,
	`CREATE INDEX i ON t(a COLLATE NOCASE)`,
	`CREATE INDEX i ON t(a,b)`,
	`CREATE INDEX i ON t(a+1)`,
	`CREATE INDEX i ON t(abs(a))`,
	`CREATE INDEX i ON t(a) WHERE b IS NOT NULL`,
	`CREATE UNIQUE INDEX i ON t(a)`,
	`CREATE INDEX i ON t(rowid)`,
	`CREATE INDEX i ON t(oid)`,
	`CREATE INDEX i ON t(nosuch)`,
	`CREATE INDEX i ON t(random())`,
	`CREATE INDEX i ON t(a) WHERE a IN ()`,
	`CREATE INDEX IF NOT EXISTS i ON t(a)`,
	`CREATE INDEX main.i ON t(a)`,
	`CREATE INDEX temp.i ON t(a)`,
	`CREATE VIEW v AS SELECT * FROM t`,
	`CREATE VIEW v(x,y) AS SELECT * FROM t`,
	`CREATE VIEW v(x) AS SELECT * FROM t`,
	`CREATE TEMP VIEW v AS SELECT * FROM t`,
	`CREATE VIEW v AS SELECT a FROM t WHERE b=(SELECT max(b) FROM t)`,
	`CREATE VIEW v AS WITH q AS (SELECT a FROM t) SELECT * FROM q`,
	`CREATE TRIGGER tg AFTER INSERT ON t BEGIN SELECT 1; END`,
	`CREATE TRIGGER tg BEFORE DELETE ON t BEGIN SELECT RAISE(ABORT,'no'); END`,
	`CREATE TRIGGER tg AFTER UPDATE OF a ON t BEGIN SELECT 1; END`,
	`CREATE TRIGGER tg AFTER UPDATE OF nosuch ON t BEGIN SELECT 1; END`,
	`CREATE TRIGGER tg INSTEAD OF INSERT ON t BEGIN SELECT 1; END`,
	`CREATE TEMP TRIGGER tg AFTER INSERT ON t BEGIN SELECT 1; END`,
	`CREATE TRIGGER tg AFTER INSERT ON t WHEN new.a>0 BEGIN SELECT 1; END`,
	`DROP TABLE t`,
	`DROP TABLE IF EXISTS t`,
	`DROP VIEW t`,
	`DROP INDEX nosuch`,
	`REINDEX t`,
	`REINDEX`,
	`ANALYZE t`,
	`ANALYZE`,
	`VACUUM`,
}

// TestDDLSurfaceSweep tests DDL statements against various schema shapes.
func TestDDLSurfaceSweep(t *testing.T) {
	probes := []string{
		`SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`,
		`SELECT * FROM t ORDER BY 1`, `SELECT * FROM v`,
		`PRAGMA integrity_check`, `PRAGMA index_list(t)`,
	}
	for sname, setup := range ddlSweepSchemas {
		for di, d := range ddlSweepStatements {
			sname, setup, d := sname, setup, d
			t.Run(fmt.Sprintf("%s/%d", sname, di), func(t *testing.T) {
				var out [2]string
				for i, drv := range []string{"sqlite3", "sqlite"} {
					p := filepath.Join(t.TempDir(), "x.db")
					db, err := sql.Open(drv, p)
					if err != nil {
						t.Fatal(err)
					}
					db.SetMaxOpenConns(1)
					for _, s := range setup {
						if _, err := db.Exec(s); err != nil {
							out[i] += "SETUP-ERR "
						}
					}
					_, derr := db.Exec(d)
					out[i] += fmt.Sprintf("ddl=%v | ", derr != nil)
					for _, q := range probes {
						out[i] += renderQuery(db, q) + " | "
					}
					db.Close()
				}
				if out[0] != out[1] {
					t.Errorf("[%s] %s\n  cgo: %s\n  mus: %s", sname, d, out[0], out[1])
				}
			})
		}
	}
}
