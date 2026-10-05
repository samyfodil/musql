// Tests trigger behaviors: cascade visibility, RENAME effects, and DELETE visibility.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

type triggerCascadeCase struct {
	name   string
	script []string
	probe  string
}

func TestTriggerCascadeParity(t *testing.T) {
	cases := []triggerCascadeCase{{
		name: "after-insert-sees-its-own-row",
		script: []string{
			`CREATE TABLE t1(a)`,
			`CREATE TABLE t2(n)`,
			`CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN INSERT INTO t2 SELECT count(*) FROM t1; END`,
			`INSERT INTO t1 VALUES(1)`,
			`INSERT INTO t1 VALUES(2)`,
		},
		probe: `SELECT n FROM t2 ORDER BY 1`,
	}, {
		name: "after-insert-sees-its-own-row-compound",
		script: []string{
			`CREATE TABLE t1(a,b,c,d,e,f)`,
			`INSERT INTO t1 VALUES(1,2,3,4,5,6)`,
			`CREATE TABLE t2(x,y,z)`,
			`CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN
			   INSERT INTO t2 SELECT a,b,c FROM t1 UNION SELECT d,e,f FROM t1 ORDER BY b,c;
			 END`,
			`INSERT INTO t1 VALUES(2,3,4,5,6,7)`,
		},
		probe: `SELECT x,y,z FROM t2 ORDER BY 1,2,3`,
	}, {
		// The trigger names the table in a DIFFERENT case than the CREATE
		// TABLE did, which is exactly the alter.test shape.
		name: "rename-to-keeps-trigger-firing",
		script: []string{
			`CREATE TABLE t6(a,b,c)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER trig1 AFTER INSERT ON T6 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t6 VALUES(1,2,3)`,
			`ALTER TABLE t6 RENAME TO t7`,
			`INSERT INTO t7 VALUES(4,5,6)`,
		},
		probe: `SELECT x FROM log ORDER BY 1`,
	}, {
		name: "rename-to-rewrites-trigger-body-references",
		script: []string{
			`CREATE TABLE t1(a)`,
			`CREATE TABLE n(c)`,
			`CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN INSERT INTO n SELECT count(*) FROM t1; END`,
			`ALTER TABLE t1 RENAME TO t1x`,
			`INSERT INTO t1x VALUES(1)`,
			`INSERT INTO t1x VALUES(2)`,
		},
		probe: `SELECT c FROM n ORDER BY 1`,
	}, {
		name: "rename-column-rewrites-update-of-gate",
		script: []string{
			`CREATE TABLE c(x)`,
			`INSERT INTO c VALUES(0)`,
			`CREATE TABLE t6("col a", "col b", "col c")`,
			`CREATE TRIGGER zzz AFTER UPDATE OF "col a", "col c" ON t6 BEGIN UPDATE c SET x=x+1; END`,
			`INSERT INTO t6 VALUES(0,0,0)`,
			`UPDATE t6 SET "col c" = 1`,
			`ALTER TABLE t6 RENAME "col c" TO "col 3"`,
			`UPDATE t6 SET "col 3" = 0`,
		},
		probe: `SELECT x FROM c`,
	}, {
		name: "update-skips-a-row-a-trigger-deleted",
		script: []string{
			`CREATE TABLE t1(a,b)`,
			`INSERT INTO t1 VALUES(1,'a')`,
			`INSERT INTO t1 VALUES(2,'b')`,
			`INSERT INTO t1 VALUES(3,'c')`,
			`INSERT INTO t1 VALUES(4,'d')`,
			`CREATE TRIGGER r1 AFTER UPDATE ON t1 FOR EACH ROW BEGIN DELETE FROM t1 WHERE a=old.a+2; END`,
			`UPDATE t1 SET b='x-' || b WHERE a=1 OR a=3`,
		},
		probe: `SELECT a,b FROM t1 ORDER BY a`,
	}, {
		// A BEFORE INSERT trigger that inserts into its OWN table claims the
		// rowid the outer row was provisionally given; C SQLite picks the
		// rowid AFTER the BEFORE program, so BOTH rows land.
		name: "before-insert-trigger-takes-the-rowid",
		script: []string{
			`CREATE TABLE tbl(a,b,c)`,
			`CREATE TRIGGER tbl_trig BEFORE INSERT ON tbl BEGIN INSERT INTO tbl VALUES (new.a, new.b, new.c); END`,
			`INSERT INTO tbl VALUES (1,2,3)`,
		},
		probe: `SELECT a,b,c FROM tbl`,
	}, {
		// A MAIN-schema trigger cannot reach a TEMP table: C SQLite keeps
		// them in separate schemas and fails the triggering statement at
		// prepare time. The CREATE TRIGGER itself still succeeds.
		name: "main-trigger-cannot-reach-a-temp-table",
		script: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TEMP TABLE t2(x,y)`,
			`CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN INSERT INTO t2 VALUES(NEW.a,NEW.b); END`,
			`INSERT INTO t1 VALUES(1,2)`,
		},
		probe: `SELECT x,y FROM t2`,
	}, {
		name: "delete-skips-a-row-a-trigger-deleted",
		script: []string{
			`CREATE TABLE t1(a,b)`,
			`INSERT INTO t1 VALUES(1,'a')`,
			`INSERT INTO t1 VALUES(2,'b')`,
			`INSERT INTO t1 VALUES(3,'c')`,
			`INSERT INTO t1 VALUES(4,'d')`,
			`CREATE TABLE log(n)`,
			`CREATE TRIGGER r1 AFTER DELETE ON t1 FOR EACH ROW BEGIN
			   DELETE FROM t1 WHERE a=old.a+2;
			   INSERT INTO log VALUES(old.a);
			 END`,
			`DELETE FROM t1 WHERE a=1 OR a=3`,
		},
		probe: `SELECT (SELECT count(*) FROM t1), (SELECT group_concat(n) FROM (SELECT n FROM log ORDER BY n))`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runTriggerCascade(t, "sqlite", filepath.Join(t.TempDir(), "e.sqlite"), tc)
			want := runTriggerCascade(t, "sqlite3", ":memory:", tc)
			if got != want {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", tc.probe, got, want)
			}
		})
	}
}

// runTriggerCascade runs tc's script then its probe on drv, returning the probe
// output (or the first error) as a single comparable string. An error in the
// script is returned too, so an engine that REJECTS a step still has to agree
// with C SQLite about it rather than silently diverging.
func runTriggerCascade(t *testing.T, drv, dsn string, tc triggerCascadeCase) string {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("%s: open: %v", drv, err)
	}
	defer db.Close()
	for _, q := range tc.script {
		if _, err := db.Exec(q); err != nil {
			// The engine prefixes its errors with "engine: "; the message
			// itself must still match C SQLite's word for word.
			return fmt.Sprintf("ERR at %q: %s", q, strings.TrimPrefix(err.Error(), "engine: "))
		}
	}
	rows, err := db.Query(tc.probe)
	if err != nil {
		return fmt.Sprintf("PROBE ERR: %v", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := ""
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Sprintf("SCAN ERR: %v", err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out += fmt.Sprintf("%v;", vals)
	}
	return out
}
