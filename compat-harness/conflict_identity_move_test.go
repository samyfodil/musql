// Tests for conflict clauses and UPSERT statements.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

type conflictScript struct {
	name  string
	setup []string
	acts  []string
	dumps []string
}

// runConflictScript executes one script and returns a flat transcript of its results.
func runConflictScript(t *testing.T, driver, dsn string, sc conflictScript) []string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s: open: %v", driver, err)
	}
	defer db.Close()
	conn, cerr := db.Conn(t.Context())
	if cerr != nil {
		t.Fatalf("%s: conn: %v", driver, cerr)
	}
	defer conn.Close()

	for _, s := range sc.setup {
		if _, err := conn.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("%s: setup %q: %v", driver, s, err)
		}
	}
	var out []string
	for _, s := range sc.acts {
		res, err := conn.ExecContext(t.Context(), s)
		if err != nil {
			out = append(out, "ERR")
			continue
		}
		n, _ := res.RowsAffected()
		out = append(out, fmt.Sprintf("changes=%d", n))
	}
	for _, q := range sc.dumps {
		rows, err := conn.QueryContext(t.Context(), q)
		if err != nil {
			out = append(out, "ERR")
			continue
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				out = append(out, "SCANERR")
				continue
			}
			cells := make([]string, len(cols))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					cells[i] = string(b)
				} else {
					cells[i] = fmt.Sprint(v)
				}
			}
			out = append(out, "("+strings.Join(cells, ",")+")")
		}
		if rows.Err() != nil {
			out = append(out, "ERR")
		}
		rows.Close()
		out = append(out, "|")
	}
	return out
}

func TestConflictAndUpsertVsOracle(t *testing.T) {
	dir := t.TempDir()
	for i, sc := range conflictScripts {
		t.Run(sc.name, func(t *testing.T) {
			want := runConflictScript(t, "sqlite3", filepath.Join(dir, fmt.Sprintf("c%d.db", i)), sc)
			got := runConflictScript(t, "sqlite", filepath.Join(dir, fmt.Sprintf("m%d.db", i)), sc)
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("musql %v\noracle %v", got, want)
			}
		})
	}
}

var conflictScripts = []conflictScript{
	{"ipk-move", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1`}, []string{`SELECT k,b FROM t ORDER BY k`}},
	{"ipk-move-3rows", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1`}, []string{`SELECT k,b FROM t ORDER BY k`}},
	{"ipk-move-descending", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`},
		[]string{`UPDATE OR REPLACE t SET k=k-1 WHERE k>1`}, []string{`SELECT k,b FROM t ORDER BY k`}},
	{"ipk-move-where", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z'),(4,'w')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1 WHERE b<>'z'`}, []string{`SELECT k,b FROM t ORDER BY k`}},
	{"ipk-move-plus-unique", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`, `INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`},
		[]string{`UPDATE OR REPLACE t SET a=a+1`}, []string{`SELECT a,b FROM t ORDER BY a`}},
	{"ipk-move-or-ignore", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`},
		[]string{`UPDATE OR IGNORE t SET k=k+1`}, []string{`SELECT k,b FROM t ORDER BY k`}},
	{"ipk-move-declared-default", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE t SET a=a+1`}, []string{`SELECT a,b FROM t ORDER BY a`}},
	{"ipk-move-nonint", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE OR IGNORE t SET k='abc' WHERE b='y'`}, []string{`SELECT k,b FROM t ORDER BY k`}},

	// WITHOUT ROWID: update.c:868-869 re-seeks by the PRIMARY KEY RECORD.
	{"wor-int-pk-move", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,v) WITHOUT ROWID`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1`}, []string{`SELECT k,v FROM t ORDER BY k`}},
	{"wor-text-pk-move", []string{`CREATE TABLE t(k TEXT PRIMARY KEY,v) WITHOUT ROWID`, `INSERT INTO t VALUES('a','x'),('b','y')`},
		[]string{`UPDATE OR REPLACE t SET k=char(unicode(k)+1)`}, []string{`SELECT k,v FROM t ORDER BY k`}},
	{"wor-declared-default", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY ON CONFLICT REPLACE, v) WITHOUT ROWID`, `INSERT INTO t VALUES(1,'x'),(2,'y')`},
		[]string{`UPDATE t SET k=k+1`}, []string{`SELECT k,v FROM t ORDER BY k`}},
	{"wor-composite-2nd", []string{`CREATE TABLE t(x,y,v, PRIMARY KEY(x,y)) WITHOUT ROWID`, `INSERT INTO t VALUES(1,1,'a'),(1,2,'b')`},
		[]string{`UPDATE OR REPLACE t SET y=y+1`}, []string{`SELECT x,y,v FROM t ORDER BY x,y`}},
	{"wor-composite-1st", []string{`CREATE TABLE t(x,y,v, PRIMARY KEY(x,y)) WITHOUT ROWID`, `INSERT INTO t VALUES(1,1,'a'),(2,1,'b'),(3,1,'c')`},
		[]string{`UPDATE OR REPLACE t SET x=x+1`}, []string{`SELECT x,y,v FROM t ORDER BY x,y`}},
	{"wor-collated-pk", []string{`CREATE TABLE t(k TEXT COLLATE NOCASE PRIMARY KEY, v) WITHOUT ROWID`, `INSERT INTO t VALUES('a',1),('B',2)`},
		[]string{`UPDATE OR REPLACE t SET k='b' WHERE v=1`}, []string{`SELECT k,v FROM t ORDER BY k`}},
	{"wor-desc-pk", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY DESC, v) WITHOUT ROWID`, `INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`},
		[]string{`UPDATE OR REPLACE t SET k=k+1`}, []string{`SELECT k,v FROM t ORDER BY k`}},
	{"wor-triggered-scan", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY, v) WITHOUT ROWID`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.k||'->'||new.k||':'||new.v); END`,
		`INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c')`},
		[]string{`UPDATE t SET v='z' WHERE k<=2`}, []string{`SELECT k,v FROM t ORDER BY k`, `SELECT x FROM log`}},
	{"upsert-after-trigger", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.a||'->'||new.a||':'||coalesce(new.c,'nil')); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-before-and-after", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES('bu:'||old.c||'->'||new.c); END`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.c||'->'||new.c); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-no-conflict-no-fire", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au'); END`},
		[]string{`INSERT INTO t(a,b,c) VALUES(7,2,10) ON CONFLICT(a) DO UPDATE SET c=c+1`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-where-false", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au'); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=99 WHERE c>1000`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-of-column-list", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE OF b ON t BEGIN INSERT INTO log VALUES('au-of-b'); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-ipk-move-trigger", []string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.a||'->'||new.a); END`,
		`INSERT INTO t VALUES(1,'x')`},
		[]string{`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=9`},
		[]string{`SELECT a,b FROM t ORDER BY a`, `SELECT x FROM log`}},
	{"upsert-when-guard", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t WHEN new.c > 50 BEGIN INSERT INTO log VALUES('au:'||new.c); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,20) ON CONFLICT(a) DO UPDATE SET c=excluded.c`,
			`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-multirow-values", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||new.a||':'||new.c); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10),(2,2,20)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,11),(2,2,22),(3,2,33) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t ORDER BY a`, `SELECT x FROM log`}},
	{"upsert-cascade", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE u(x)`, `CREATE TABLE log(y)`,
		`CREATE TRIGGER ua AFTER INSERT ON u BEGIN INSERT INTO log VALUES('cascade:'||new.x); END`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO u VALUES(new.c); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM u`, `SELECT y FROM log`}},
	{"upsert-generated-column", []string{`CREATE TABLE t(a UNIQUE, b, g AS (b*2))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||new.g); END`,
		`INSERT INTO t(a,b) VALUES(1,5)`},
		[]string{`INSERT INTO t(a,b) VALUES(1,7) ON CONFLICT(a) DO UPDATE SET b=excluded.b`},
		[]string{`SELECT a,b,g FROM t`, `SELECT x FROM log`}},
	{"upsert-strict", []string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||new.b); END`,
		`INSERT INTO t VALUES(1,'x')`},
		[]string{`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b=excluded.b`},
		[]string{`SELECT a,b FROM t`, `SELECT x FROM log`}},
	{"upsert-without-rowid", []string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au:'||old.v||'->'||new.v); END`,
		`INSERT INTO t VALUES('a',1)`},
		[]string{`INSERT INTO t VALUES('a',9) ON CONFLICT(k) DO UPDATE SET v=excluded.v`},
		[]string{`SELECT k,v FROM t`, `SELECT x FROM log`}},
	{"upsert-fk-violation", []string{`PRAGMA foreign_keys=ON`, `CREATE TABLE p(k UNIQUE)`,
		`CREATE TABLE c(a UNIQUE, x REFERENCES p(k))`, `CREATE TABLE log(y)`,
		`CREATE TRIGGER cu AFTER UPDATE ON c BEGIN INSERT INTO log VALUES('au'); END`,
		`INSERT INTO p VALUES(1)`, `INSERT INTO c VALUES(7,1)`},
		[]string{`INSERT INTO c VALUES(7,99) ON CONFLICT(a) DO UPDATE SET x=excluded.x`},
		[]string{`SELECT a,x FROM c`, `SELECT y FROM log`}},
	{"upsert-check-violation", []string{`CREATE TABLE t(a UNIQUE,b,c, CHECK(c<50))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au'); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-trigger-raise-abort", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN SELECT RAISE(ABORT,'no upsert'); END`,
		`INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-body-or-ignore", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x UNIQUE)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT OR IGNORE INTO log VALUES(1); END`,
		`INSERT INTO log VALUES(1)`, `INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},
	{"upsert-body-or-replace", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x UNIQUE, y)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT OR REPLACE INTO log VALUES(1,'new'); END`,
		`INSERT INTO log VALUES(1,'old')`, `INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x,y FROM log`}},
	{"plain-update-body-or-ignore", []string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x UNIQUE)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT OR IGNORE INTO log VALUES(1); END`,
		`INSERT INTO log VALUES(1)`, `INSERT INTO t(a,b,c) VALUES(1,2,10)`},
		[]string{`UPDATE t SET c=99`},
		[]string{`SELECT a,b,c FROM t`, `SELECT x FROM log`}},

	{"victim-delete-after", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('ad:'||old.a); END`,
		`INSERT INTO t1 VALUES('a','one'),('b','two')`},
		[]string{`UPDATE OR REPLACE t1 SET a='a'`},
		[]string{`SELECT a,b FROM t1 ORDER BY a,b`, `SELECT x FROM log`}},
	{"victim-delete-before-and-after", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tbd BEFORE DELETE ON t1 BEGIN INSERT INTO log VALUES('bd:'||old.a); END`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('ad:'||old.a); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two'),(3,'three')`},
		[]string{`UPDATE OR REPLACE t1 SET a=a+1 WHERE a>=2`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-delete-pragma-off", []string{`CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('ad:'||old.a); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two'),(3,'three')`},
		[]string{`UPDATE OR REPLACE t1 SET a=a+1 WHERE a>=2`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-before-raise-ignore", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tbd BEFORE DELETE ON t1 BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two')`},
		[]string{`UPDATE OR REPLACE t1 SET a=a+1 WHERE a=1`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-is-a-later-match", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('ad:'||old.a); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two'),(3,'three')`},
		[]string{`UPDATE OR REPLACE t1 SET a=a+1`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-delete-plus-ipk-move", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('ad:'||old.a); END`,
		`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`},
		[]string{`UPDATE OR REPLACE t1 SET a=a+1`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-delete-or-fail", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('ad:'||old.a); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two'),(3,'three')`},
		[]string{`UPDATE OR FAIL t1 SET a=a+1 WHERE a>=2`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-delete-raise-abort", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tbd BEFORE DELETE ON t1 BEGIN SELECT RAISE(ABORT,'nope'); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two')`},
		[]string{`UPDATE OR REPLACE t1 SET a=2 WHERE b='one'`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-delete-writes-the-target", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN DELETE FROM t1 WHERE b='three'; END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two'),(3,'three')`},
		[]string{`UPDATE OR REPLACE t1 SET a=a+1 WHERE b='one'`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
	{"victim-delete-inserts-the-target", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN INSERT INTO t1 VALUES(old.a, 'resurrected'); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two')`},
		[]string{`UPDATE OR REPLACE t1 SET a=1 WHERE b='two'`},
		[]string{`SELECT a,b FROM t1 ORDER BY a,b`, `SELECT x FROM log`}},
	{"victim-delete-writes-target-insert-half", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN DELETE FROM t1 WHERE b='three'; END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two'),(3,'three')`},
		[]string{`INSERT OR REPLACE INTO t1 VALUES(1,'new')`},
		[]string{`SELECT a,b FROM t1 ORDER BY a`}},
	{"victim-delete-returning", []string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t1(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tad AFTER DELETE ON t1 BEGIN INSERT INTO log VALUES('ad:'||old.a); END`,
		`INSERT INTO t1 VALUES(1,'one'),(2,'two')`},
		nil,
		[]string{`UPDATE OR REPLACE t1 SET a=1 WHERE b='two' RETURNING a,b`, `SELECT a,b FROM t1 ORDER BY a`, `SELECT x FROM log`}},
}
