package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestAlterSurfaceSweep tests ALTER TABLE against various schema shapes.
func TestAlterSurfaceSweep(t *testing.T) {
	schemas := map[string][]string{
		"plain":      {`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x')`},
		"pk":         {`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(1,'x')`},
		"norowid":    {`CREATE TABLE t(a TEXT PRIMARY KEY, b) WITHOUT ROWID`, `INSERT INTO t VALUES('k','x')`},
		"strict":     {`CREATE TABLE t(a INT, b TEXT) STRICT`, `INSERT INTO t VALUES(1,'x')`},
		"index":      {`CREATE TABLE t(a,b)`, `CREATE INDEX i ON t(b)`, `INSERT INTO t VALUES(1,'x')`},
		"exprindex":  {`CREATE TABLE t(a,b)`, `CREATE INDEX i ON t(abs(a)) WHERE b IS NOT NULL`, `INSERT INTO t VALUES(1,'x')`},
		"view":       {`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT a,b FROM t`, `INSERT INTO t VALUES(1,'x')`},
		"trigger":    {`CREATE TABLE t(a,b)`, `CREATE TABLE lg(m)`, `CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO lg VALUES(new.a); END`},
		"gencol":     {`CREATE TABLE t(a,b, c AS (a+1), d AS (b||'!') STORED)`, `INSERT INTO t(a,b) VALUES(1,'x')`},
		"fkchild":    {`CREATE TABLE p(x PRIMARY KEY)`, `CREATE TABLE t(a,b REFERENCES p(x))`, `INSERT INTO p VALUES('x')`},
		"fkparent":   {`CREATE TABLE t(a PRIMARY KEY, b)`, `CREATE TABLE ch(y REFERENCES t(a))`},
		"check":      {`CREATE TABLE t(a,b, CHECK(a IS NULL OR a>0))`, `INSERT INTO t VALUES(1,'x')`},
		"defaults":   {`CREATE TABLE t(a DEFAULT 5, b DEFAULT (1+1))`, `INSERT INTO t DEFAULT VALUES`},
		"collate":    {`CREATE TABLE t(a COLLATE NOCASE, b)`, `INSERT INTO t VALUES('A','x')`},
		"autoinc":    {`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `INSERT INTO t(b) VALUES('x')`},
		"quotedname": {`CREATE TABLE t("a b", "select")`, `INSERT INTO t VALUES(1,2)`},
	}
	alters := []string{
		`ALTER TABLE t RENAME TO t2`,
		`ALTER TABLE t RENAME COLUMN a TO z`,
		`ALTER TABLE t RENAME a TO z`,
		`ALTER TABLE t ADD COLUMN z`,
		`ALTER TABLE t ADD COLUMN z INT DEFAULT 7`,
		`ALTER TABLE t ADD COLUMN z INT NOT NULL DEFAULT 7`,
		`ALTER TABLE t ADD COLUMN z INT NOT NULL`,
		`ALTER TABLE t ADD COLUMN z UNIQUE`,
		`ALTER TABLE t ADD COLUMN z PRIMARY KEY`,
		`ALTER TABLE t ADD COLUMN z REFERENCES t`,
		`ALTER TABLE t ADD COLUMN z DEFAULT CURRENT_TIMESTAMP`,
		`ALTER TABLE t ADD COLUMN z AS (1+1)`,
		`ALTER TABLE t ADD COLUMN z AS (1+1) STORED`,
		`ALTER TABLE t ADD COLUMN z COLLATE RTRIM`,
		`ALTER TABLE t ADD COLUMN z CHECK(z>0)`,
		`ALTER TABLE t DROP COLUMN b`,
		`ALTER TABLE t DROP COLUMN a`,
		`ALTER TABLE t DROP b`,
		`ALTER TABLE t RENAME COLUMN nosuch TO z`,
		`ALTER TABLE nosuch RENAME TO z`,
		`ALTER TABLE t RENAME TO t`,
		`ALTER TABLE t RENAME COLUMN a TO b`,
		`ALTER TABLE t ADD COLUMN a`,
		`ALTER TABLE sqlite_schema RENAME TO x`,
		`ALTER TABLE main.t RENAME TO t2`,
		`ALTER TABLE temp.t RENAME TO t2`,
	}
	probes := []string{
		`SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`,
		`SELECT * FROM t`, `SELECT * FROM t2`, `SELECT * FROM v`,
		`PRAGMA integrity_check`,
	}
	for sname, setup := range schemas {
		for ai, a := range alters {
			sname, setup, a := sname, setup, a
			t.Run(fmt.Sprintf("%s/%d", sname, ai), func(t *testing.T) {
				out := map[string]string{}
				for _, drv := range []string{"sqlite3", "sqlite"} {
					p := filepath.Join(t.TempDir(), "x.db")
					db, err := sql.Open(drv, p)
					if err != nil {
						t.Fatal(err)
					}
					db.SetMaxOpenConns(1)
					var sb string
					for _, s := range setup {
						if _, err := db.Exec(s); err != nil {
							sb += "SETUP-ERR "
						}
					}
					_, aerr := db.Exec(a)
					sb += fmt.Sprintf("alter=%v | ", aerr != nil)
					for _, q := range probes {
						sb += renderQuery(db, q) + " | "
					}
					db.Close()
					out[drv] = sb
				}
				if out["sqlite3"] != out["sqlite"] {
					t.Errorf("[%s] %s\n  cgo: %s\n  mus: %s", sname, a, out["sqlite3"], out["sqlite"])
				}
			})
		}
	}
}

// renderQuery renders query results to a comparable string.
func renderQuery(db *sql.DB, q string) string {
	rows, err := db.Query(q)
	if err != nil {
		return "ERR"
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	s := fmt.Sprint(cols)
	n := 0
	for rows.Next() {
		n++
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if rows.Scan(ptrs...) != nil {
			return "SCANERR"
		}
		s += fmt.Sprintf("%v", cells)
	}
	if rows.Err() != nil {
		if n == 0 {
			// The two drivers report a FAILING statement run through Query at
			// different moments -- mattn hands back a Rows whose Err() carries
			// it, this one returns it from Query -- which is a database/sql
			// shape difference, not a SQL one. Both are "this did not run".
			return "ERR"
		}
		return s + "ROWSERR"
	}
	return s
}
