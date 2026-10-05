package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// BEGIN, END, savepoints, DDL, and deferred foreign key checks in transactions.
func TestTxnSavepointSweep(t *testing.T) {
	scripts := map[string][]string{
		"begin-commit":        {`BEGIN`, `INSERT INTO t VALUES(9)`, `COMMIT`},
		"begin-rollback":      {`BEGIN`, `INSERT INTO t VALUES(9)`, `ROLLBACK`},
		"deferred":            {`BEGIN DEFERRED`, `INSERT INTO t VALUES(9)`, `COMMIT`},
		"immediate":           {`BEGIN IMMEDIATE`, `INSERT INTO t VALUES(9)`, `COMMIT`},
		"exclusive":           {`BEGIN EXCLUSIVE`, `INSERT INTO t VALUES(9)`, `COMMIT`},
		"begin-transaction":   {`BEGIN TRANSACTION`, `INSERT INTO t VALUES(9)`, `END TRANSACTION`},
		"end":                 {`BEGIN`, `INSERT INTO t VALUES(9)`, `END`},
		"nested-begin":        {`BEGIN`, `BEGIN`, `INSERT INTO t VALUES(9)`, `COMMIT`},
		"commit-no-txn":       {`COMMIT`},
		"rollback-no-txn":     {`ROLLBACK`},
		"sp-release":          {`SAVEPOINT s`, `INSERT INTO t VALUES(9)`, `RELEASE s`},
		"sp-rollback-to":      {`SAVEPOINT s`, `INSERT INTO t VALUES(9)`, `ROLLBACK TO s`, `COMMIT`},
		"sp-rollback-to-keep": {`SAVEPOINT s`, `INSERT INTO t VALUES(9)`, `ROLLBACK TO s`, `INSERT INTO t VALUES(8)`, `RELEASE s`},
		"sp-nested":           {`SAVEPOINT a`, `INSERT INTO t VALUES(9)`, `SAVEPOINT b`, `INSERT INTO t VALUES(8)`, `ROLLBACK TO b`, `RELEASE a`},
		"sp-release-outer":    {`SAVEPOINT a`, `SAVEPOINT b`, `INSERT INTO t VALUES(9)`, `RELEASE a`},
		"sp-dup-names":        {`SAVEPOINT s`, `INSERT INTO t VALUES(9)`, `SAVEPOINT s`, `INSERT INTO t VALUES(8)`, `ROLLBACK TO s`, `RELEASE s`},
		"sp-in-txn":           {`BEGIN`, `SAVEPOINT s`, `INSERT INTO t VALUES(9)`, `RELEASE s`, `COMMIT`},
		"sp-in-txn-rollback":  {`BEGIN`, `SAVEPOINT s`, `INSERT INTO t VALUES(9)`, `ROLLBACK`, `COMMIT`},
		"sp-release-missing":  {`SAVEPOINT s`, `RELEASE nosuch`, `RELEASE s`},
		"sp-rollback-missing": {`SAVEPOINT s`, `ROLLBACK TO nosuch`, `RELEASE s`},
		"rollback-to-savepoint-kw": {`SAVEPOINT s`, `INSERT INTO t VALUES(9)`, `ROLLBACK TRANSACTION TO SAVEPOINT s`, `RELEASE SAVEPOINT s`},
		"ddl-in-txn":          {`BEGIN`, `CREATE TABLE q(x)`, `INSERT INTO q VALUES(1)`, `ROLLBACK`},
		"ddl-in-txn-commit":   {`BEGIN`, `CREATE TABLE q(x)`, `INSERT INTO q VALUES(1)`, `COMMIT`},
		"drop-in-txn":         {`BEGIN`, `DROP TABLE t`, `ROLLBACK`},
		"err-then-commit":     {`BEGIN`, `INSERT INTO t VALUES(9)`, `INSERT INTO nosuch VALUES(1)`, `COMMIT`},
		"vacuum-in-txn":       {`BEGIN`, `VACUUM`, `COMMIT`},
		"fk-deferred-commit":  {`BEGIN`, `INSERT INTO ch VALUES(99)`, `COMMIT`},
		"fk-deferred-rollback": {`BEGIN`, `INSERT INTO ch VALUES(99)`, `COMMIT`, `ROLLBACK`},
		"fk-deferred-resolved": {`BEGIN`, `INSERT INTO ch VALUES(99)`, `INSERT INTO p VALUES(99)`, `COMMIT`},
		"fk-deferred-sp":      {`SAVEPOINT s`, `INSERT INTO ch VALUES(99)`, `RELEASE s`},
		"fk-deferred-sp-undo": {`SAVEPOINT s`, `INSERT INTO ch VALUES(99)`, `ROLLBACK TO s`, `RELEASE s`},
	}
	probes := []string{
		`SELECT count(*) FROM t`, `SELECT count(*) FROM ch`,
		`SELECT name FROM sqlite_schema ORDER BY name`, `PRAGMA integrity_check`,
	}
	for name, script := range scripts {
		name, script := name, script
		t.Run(name, func(t *testing.T) {
			var out [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range []string{
					`PRAGMA foreign_keys=ON`,
					`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1),(2)`,
					`CREATE TABLE p(x INTEGER PRIMARY KEY)`,
					`CREATE TABLE ch(y REFERENCES p(x) DEFERRABLE INITIALLY DEFERRED)`,
				} {
					if _, err := db.Exec(s); err != nil {
						t.Fatalf("setup %s: %v", s, err)
					}
				}
				for _, s := range script {
					_, err := db.Exec(s)
					out[i] += fmt.Sprintf("%v|", err != nil)
				}
				out[i] += " || "
				for _, q := range probes {
					out[i] += renderQuery(db, q) + " | "
				}
				db.Close()
			}
			if out[0] != out[1] {
				t.Errorf("%v\n  cgo: %s\n  mus: %s", script, out[0], out[1])
			}
		})
	}
}
