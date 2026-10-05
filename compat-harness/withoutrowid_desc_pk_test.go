// Tests WITHOUT ROWID tables with DESC-ordered PRIMARY KEY columns.
package compat

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestWithoutRowidDescPKAgrees tests DESC primary keys across various operations.
func TestWithoutRowidDescPKAgrees(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"select4-16.1", []string{
			"DROP TABLE IF EXISTS t1",
			`CREATE TABLE t1(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u,v,w,x,y,z,
			 PRIMARY KEY(a,b DESC)) WITHOUT ROWID`,
			`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<100)
			 INSERT INTO t1(a,b,c,d)
			   SELECT x%10, x/10, x, printf('xyz%dabc',x) FROM c`,
			`SELECT t3.c FROM
			   (SELECT a,max(b) AS m FROM t1 WHERE a>=5 GROUP BY a) AS t2
			   JOIN t1 AS t3
			 WHERE t2.a=t3.a AND t2.m=t3.b
			 ORDER BY t3.a`,
		}},
		{"without_rowid1-12.1", []string{
			"DROP TABLE IF EXISTS t0",
			"CREATE TABLE t0 (c0 INTEGER PRIMARY KEY DESC, c1 UNIQUE DEFAULT NULL) WITHOUT ROWID",
			"INSERT INTO t0(c0) VALUES (1), (2), (3), (4), (5)",
			"REINDEX",
			"PRAGMA integrity_check",
			"SELECT * FROM t0",
		}},
		{"single-column-desc-null-rejected", []string{
			"CREATE TABLE t0d(c0 INTEGER PRIMARY KEY DESC, c1) WITHOUT ROWID",
			"INSERT INTO t0d VALUES(NULL, 'x')",
		}},
		{"multi-column-partial-desc-update-delete", []string{
			"CREATE TABLE t7(a,b,c, PRIMARY KEY(a DESC, b)) WITHOUT ROWID",
			"INSERT INTO t7 VALUES(1,10,100),(1,20,200),(2,5,500),(3,1,300)",
			"SELECT * FROM t7",
			"UPDATE t7 SET c=c+1 WHERE a=1",
			"DELETE FROM t7 WHERE a=2",
			"SELECT * FROM t7 ORDER BY a DESC, b",
			"PRAGMA integrity_check",
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestWithoutRowidDescPKPersists tests DESC ordering persists after Close/reopen.
func TestWithoutRowidDescPKPersists(t *testing.T) {
	dir := t.TempDir()

	write := func(engine, dsn string) []map[string]any {
		return runWithDSN(t, engine, dsn, []string{
			"CREATE TABLE t0 (c0 INTEGER PRIMARY KEY DESC, c1 UNIQUE DEFAULT NULL) WITHOUT ROWID",
			"INSERT INTO t0(c0) VALUES (1), (2), (3), (4), (5)",
		})
	}
	reread := []string{
		"PRAGMA integrity_check",
		"SELECT * FROM t0",
	}

	cgoDSN := filepath.Join(dir, "cgo.db")
	write("cgo", cgoDSN)
	cgoGot := runWithDSN(t, "cgo", cgoDSN, reread)
	cgoJSON, _ := json.Marshal(cgoGot)

	mushDSN := filepath.Join(dir, "musql.db")
	write("musql", mushDSN)
	mushGot := runWithDSN(t, "musql", mushDSN, reread)
	mushJSON, _ := json.Marshal(mushGot)

	if string(cgoJSON) != string(mushJSON) {
		t.Errorf("post-reopen DESC-ordered WITHOUT ROWID read DIVERGES from C SQLite\n  cgo:    %s\n  musql: %s", cgoJSON, mushJSON)
	}
}
