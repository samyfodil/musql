package compat

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestUpsertChainAcrossTableShapes replays upsert5.test's "foreach {tn sql}"
// block, which the corpus miner cannot expand (each table comes from
// "execsql $sql"): every 1.$tn.* body against each of its six tables. Chained
// ON CONFLICT clauses are checked in C's upsert order -- the targeted indexes
// first, in clause order, the rowid after the last of them unless the first
// clause names it (insert.c:2138-2186, :2253-2266, :2673-2680).
func TestUpsertChainAcrossTableShapes(t *testing.T) {
	src, err := os.ReadFile("testdata/tcl/upsert5.test")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	head := s[strings.Index(s, "foreach {tn sql} {"):]
	block := head[:strings.Index(head, "\n}\n")]
	creates := regexp.MustCompile(`(?m)^  (\d) \{ (CREATE TABLE t1.*?) ?\}$`).FindAllStringSubmatch(block, -1)
	bodies := regexp.MustCompile(`(?s)do_execsql_test 1\.\$tn\.\d+ \{(.*?)\n  \} \{`).FindAllStringSubmatch(block, -1)
	if len(creates) != 6 || len(bodies) < 30 {
		t.Fatalf("upsert5.test's block changed shape: %d tables, %d bodies", len(creates), len(bodies))
	}
	for _, cr := range creates {
		stmts := []string{cr[2]}
		for _, b := range bodies {
			for _, st := range strings.Split(b[1], ";") {
				if st = strings.TrimSpace(st); st != "" {
					stmts = append(stmts, st)
				}
			}
		}
		differ(t, "upsert5 1."+cr[1], stmts)
	}
}

// TestUpsertChainConflictActions pins what a constraint no clause names does
// under an upsert: it resolves with its own action -- the statement's
// OR-clause or its declared one -- rather than always aborting, and REPLACE
// still deletes the conflicting row (upsert5.test 3.0-3.3).
func TestUpsertChainConflictActions(t *testing.T) {
	differ(t, "upsert chain conflict actions", []string{
		"CREATE TABLE t1(aa INTEGER PRIMARY KEY, bb INT, cc INT)",
		"INSERT INTO t1 VALUES(10,21,32),(11,22,33),(12,23,34)",
		"CREATE UNIQUE INDEX t1bb ON t1(bb)",
		"CREATE UNIQUE INDEX t1cc ON t1(cc)",
		"REPLACE INTO t1 VALUES(11,44,55) ON CONFLICT(bb) DO UPDATE SET aa = 99 ON CONFLICT(cc) DO UPDATE SET aa = 99 ON CONFLICT(bb) DO UPDATE SET aa = 99",
		"SELECT * FROM t1 ORDER BY aa",
		"PRAGMA integrity_check",
		"INSERT INTO t1 VALUES(50,21,77) ON CONFLICT(cc) DO UPDATE SET cc=cc+1000",
		"INSERT OR IGNORE INTO t1 VALUES(51,21,78) ON CONFLICT(cc) DO UPDATE SET cc=cc+1000",
		"INSERT OR FAIL INTO t1 VALUES(52,21,79) ON CONFLICT(cc) DO UPDATE SET cc=cc+1000",
		"INSERT OR REPLACE INTO t1 VALUES(53,21,80) ON CONFLICT(cc) DO UPDATE SET cc=cc+1000",
		"SELECT * FROM t1 ORDER BY aa",
		"INSERT INTO t1 VALUES(60,23,55) ON CONFLICT(bb) DO UPDATE SET bb=-bb ON CONFLICT(cc) DO UPDATE SET cc=-cc",
		"INSERT INTO t1 VALUES(61,21,34) ON CONFLICT(cc) DO UPDATE SET cc=-cc ON CONFLICT(bb) DO UPDATE SET bb=-bb",
		"SELECT * FROM t1 ORDER BY aa",
		"INSERT INTO t1 VALUES(10,-23,1) ON CONFLICT(bb) DO NOTHING ON CONFLICT(aa) DO UPDATE SET cc=0",
		"INSERT INTO t1 VALUES(10,-23,2) ON CONFLICT(aa) DO UPDATE SET cc=0 ON CONFLICT(bb) DO NOTHING",
		"SELECT * FROM t1 ORDER BY aa",
		"INSERT INTO t1 VALUES(70,-23,3) ON CONFLICT(cc) DO UPDATE SET cc=1 ON CONFLICT DO NOTHING",
		"INSERT INTO t1 VALUES(71,999,-34) ON CONFLICT(bb) DO NOTHING ON CONFLICT DO UPDATE SET bb=bb*10",
		"SELECT * FROM t1 ORDER BY aa",
		"INSERT INTO t1 VALUES(72,1,1) ON CONFLICT DO NOTHING ON CONFLICT(bb) DO NOTHING",
		"INSERT INTO t1 VALUES(73,1,1) ON CONFLICT(b) DO NOTHING",
		"INSERT INTO t1 DEFAULT VALUES ON CONFLICT DO NOTHING",
		"CREATE TABLE src(x,y,z)",
		"INSERT INTO src VALUES(80,-230,1),(81,5,-340),(82,6,7)",
		"INSERT INTO t1 SELECT * FROM src WHERE 1 ON CONFLICT(bb) DO UPDATE SET cc=excluded.cc ON CONFLICT(cc) DO UPDATE SET bb=excluded.bb",
		"SELECT * FROM t1 ORDER BY aa",
		"INSERT INTO t1 VALUES(90,6,-3400) ON CONFLICT(bb) DO UPDATE SET cc=1 ON CONFLICT(cc) DO UPDATE SET bb=2 RETURNING *",
		"INSERT INTO t1 VALUES(91,6,-3400) ON CONFLICT(cc) DO UPDATE SET cc=cc WHERE 0 ON CONFLICT(bb) DO UPDATE SET bb=3 RETURNING *",
		"SELECT * FROM t1 ORDER BY aa",
	})
	differ(t, "upsert chain triggers", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE, d)",
		"CREATE TABLE log(x)",
		"CREATE TRIGGER tb AFTER UPDATE OF b ON t BEGIN INSERT INTO log VALUES('b:'||new.b); END",
		"CREATE TRIGGER tc AFTER UPDATE OF c ON t BEGIN INSERT INTO log VALUES('c:'||new.c); END",
		"CREATE TRIGGER td BEFORE UPDATE OF d ON t BEGIN INSERT INTO log VALUES('d:'||new.d); END",
		"CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO log VALUES('i:'||new.a); END",
		"INSERT INTO t VALUES(1,1,1,1),(2,2,2,2)",
		"INSERT INTO t VALUES(3,1,2,3) ON CONFLICT(c) DO UPDATE SET c=c+10 ON CONFLICT(b) DO UPDATE SET b=b+10",
		"INSERT INTO t VALUES(4,2,1,4) ON CONFLICT(d) DO NOTHING",
		"INSERT INTO t VALUES(4,2,1,4) ON CONFLICT(b) DO UPDATE SET d=d+100 ON CONFLICT(c) DO UPDATE SET c=c+10",
		"INSERT INTO t VALUES(5,5,5,5) ON CONFLICT(b) DO UPDATE SET d=0 ON CONFLICT(c) DO UPDATE SET c=0",
		"SELECT * FROM t ORDER BY a",
		"SELECT rowid, x FROM log ORDER BY rowid",
		"PRAGMA recursive_triggers=ON",
		"CREATE TRIGGER tdel AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del:'||old.a); END",
		"REPLACE INTO t VALUES(5,50,50,50) ON CONFLICT(b) DO UPDATE SET d=-1",
		"INSERT OR REPLACE INTO t VALUES(6,50,6,6) ON CONFLICT(c) DO NOTHING",
		"SELECT * FROM t ORDER BY a",
		"SELECT rowid, x FROM log ORDER BY rowid",
	})
}
