package compat

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
)

// rtreeSwapInt32 and rtreeSetInt32 are rtreecheck.test's TCL helpers
// swap_int32/set_int32 in SQL: the blob's big-endian int32 slots i0 and i1
// exchanged, or slot idx overwritten, by splicing its hex text.
func rtreeSwapInt32(i0, i1 int) string {
	h := "hex(data)"
	at := func(i int) int { return i*8 + 1 }
	return "unhex(substr(" + h + ",1," + strconv.Itoa(at(i0)-1) + ")||substr(" + h + "," + strconv.Itoa(at(i1)) + ",8)||substr(" + h + "," + strconv.Itoa(at(i0)+8) + "," + strconv.Itoa(at(i1)-at(i0)-8) + ")||substr(" + h + "," + strconv.Itoa(at(i0)) + ",8)||substr(" + h + "," + strconv.Itoa(at(i1)+8) + "))"
}

func rtreeSetInt32(idx int, hex8 string) string {
	return "unhex(substr(hex(data),1," + strconv.Itoa(idx*8) + ")||'" + hex8 + "'||substr(hex(data)," + strconv.Itoa(idx*8+9) + "))"
}

// TestRtreecheckMatchesC pins rtreecheck() (engine/rtree_check.go, a port of
// rtree.c:3834-4300) and its PRAGMA integrity_check half against 3.53.3:
// rtreecheck.test's own cases, with its TCL blob helpers spelled in SQL, and
// the argument and object shapes the corpus never reaches.
func TestRtreecheckMatchesC(t *testing.T) {
	simple := func(module string) []string {
		return []string{
			"CREATE VIRTUAL TABLE r1 USING " + module + "(id, x1, x2, y1, y2)",
			"INSERT INTO r1 VALUES(1, 5, 5, 5, 5)",
			"INSERT INTO r1 VALUES(2, 6, 6, 6, 6)",
			"INSERT INTO r1 VALUES(3, 7, 7, 7, 7)",
			"INSERT INTO r1 VALUES(4, 8, 8, 8, 8)",
			"INSERT INTO r1 VALUES(5, 9, 9, 9, 9)",
		}
	}
	with := func(base []string, more ...string) []string { return append(append([]string{}, base...), more...) }
	thousand := []string{
		"CREATE VIRTUAL TABLE r3 USING rtree_i32(id, x1, x2, y1, y2)",
		"WITH x(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM x WHERE i<1000) INSERT INTO r3 SELECT i, i, i, i, i FROM x",
	}
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"arity", []string{"SELECT rtreecheck()", "SELECT rtreecheck(0,0,0)"}},
		{"sound", with(simple("rtree"), "SELECT rtreecheck('r1')", "SELECT rtreecheck('main', 'r1')", "PRAGMA integrity_check", "PRAGMA quick_check")},
		{"sound i32", with(simple("rtree_i32"), "SELECT rtreecheck('r1')", "PRAGMA integrity_check")},
		{"swapped coordinates", with(simple("rtree"),
			"UPDATE r1_node SET data = "+rtreeSwapInt32(3, 9),
			"UPDATE r1_node SET data = "+rtreeSwapInt32(23, 29),
			"SELECT rtreecheck('r1')", "PRAGMA integrity_check", "PRAGMA integrity_check(1)", "PRAGMA quick_check")},
		{"missing rowid mapping", with(simple("rtree"), "DELETE FROM r1_rowid WHERE rowid = 3", "SELECT rtreecheck('r1')", "PRAGMA integrity_check")},
		{"wrong rowid mapping", with(simple("rtree"), "UPDATE r1_rowid SET nodeno=2 WHERE rowid=3", "SELECT rtreecheck('r1')", "PRAGMA integrity_check")},
		{"i32 extremes", []string{
			"CREATE VIRTUAL TABLE r1 USING rtree_i32(id, x1, x2)",
			"INSERT INTO r1 VALUES(1, 0x7FFFFFFF*-1, 0x7FFFFFFF)",
			"INSERT INTO r1 VALUES(2, 0x7FFFFFFF*-1, 5)",
			"INSERT INTO r1 VALUES(3, -5, 5)",
			"INSERT INTO r1 VALUES(4, 5, 0x11111111)",
			"INSERT INTO r1 VALUES(5, 5, 0x00800000)",
			"INSERT INTO r1 VALUES(6, 5, 0x00008000)",
			"INSERT INTO r1 VALUES(7, 5, 0x00000080)",
			"INSERT INTO r1 VALUES(8, 5, 0x40490fdb)",
			"INSERT INTO r1 VALUES(9, 0x7f800000, 0x7f900000)",
			"SELECT rtreecheck('r1')", "PRAGMA integrity_check",
			"CREATE VIRTUAL TABLE r2 USING rtree_i32(id, x1, x2)",
			"INSERT INTO r2 VALUES(2, -1*(1<<31), -1*(1<<31)+5)",
			"SELECT rtreecheck('r2')", "PRAGMA integrity_check",
		}},
		{"short root inside a transaction", []string{
			"CREATE VIRTUAL TABLE r2 USING rtree_i32(id, x1, x2)",
			"INSERT INTO r2 VALUES(2, -1*(1<<31), -1*(1<<31)+5)",
			"BEGIN",
			"UPDATE r2_node SET data = X'123456'",
			"SELECT rtreecheck('r2')",
			"ROLLBACK",
			"UPDATE r2_node SET data = X'00001234'",
			"SELECT rtreecheck('r2')",
			"PRAGMA integrity_check",
		}},
		{"not an rtree", []string{"CREATE TABLE notanrtree(i)", "SELECT rtreecheck('notanrtree')", "CREATE TABLE three(a, b, c)", "SELECT rtreecheck('three')"}},
		{"no such table", []string{"SELECT rtreecheck('nosuch')", "SELECT rtreecheck('nosuchdb', 'nosuch')"}},
		{"null and integer arguments", with(simple("rtree"), "SELECT rtreecheck(NULL)", "SELECT rtreecheck('main', NULL)", "SELECT rtreecheck(NULL, 'r1')", "SELECT rtreecheck(1)")},
		{"quoted name", []string{
			`CREATE VIRTUAL TABLE "it's" USING rtree(id, a, b)`,
			`INSERT INTO "it's" VALUES(1, 2, 3)`,
			`SELECT rtreecheck('it''s')`,
		}},
		{"temp", []string{
			"CREATE VIRTUAL TABLE temp.rt USING rtree(id, a, b)",
			"INSERT INTO temp.rt VALUES(1, 2, 3)",
			"SELECT rtreecheck('temp', 'rt')",
			"SELECT rtreecheck('rt')",
		}},
		{"node blob types", with(simple("rtree"),
			"UPDATE r1_node SET data = 12345",
			"SELECT rtreecheck('r1')",
			"UPDATE r1_node SET data = NULL",
			"SELECT rtreecheck('r1')",
			"UPDATE r1_node SET data = X''",
			"SELECT rtreecheck('r1')",
		)},
		{"missing root", with(simple("rtree"), "DELETE FROM r1_node", "SELECT rtreecheck('r1')", "PRAGMA integrity_check")},
		{"a thousand rows", with(thousand, "SELECT rtreecheck('r3')", "PRAGMA integrity_check",
			"DELETE FROM r3 WHERE id % 3 = 0", "SELECT rtreecheck('r3')")},
		{"a thousand rows, damaged", with(thousand,
			"BEGIN",
			"UPDATE r3_node SET data = "+rtreeSetInt32(3, "00001388"),
			"UPDATE r3_node SET data = "+rtreeSetInt32(4, "00001388"),
			"SELECT rtreecheck('r3')=='ok'",
			"SELECT rtreecheck('r3')",
			"ROLLBACK",
			"BEGIN",
			"UPDATE r3_node SET data = "+rtreeSetInt32(3, "00000000"),
			"UPDATE r3_node SET data = "+rtreeSetInt32(4, "00000000"),
			"SELECT rtreecheck('r3')=='ok'",
			"SELECT rtreecheck('r3')",
			"PRAGMA integrity_check",
			"ROLLBACK",
		)},
		{"a write reads its own image", with(simple("rtree"),
			"CREATE TABLE log(x)",
			"INSERT INTO log SELECT rtreecheck('r1')",
			"DELETE FROM r1_rowid WHERE rowid = 2",
			"INSERT INTO log VALUES(rtreecheck('r1'))",
			"SELECT * FROM log",
		)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "rtreecheck: "+tc.name, tc.stmts) })
	}
}

// TestRtreecheckErrorText pins the wording of the errors rtreecheck() and a
// schema object calling it raise.
func TestRtreecheckErrorText(t *testing.T) {
	setup := []string{
		"CREATE VIRTUAL TABLE r1 USING rtree(id, x1, x2)",
		"INSERT INTO r1 VALUES(1, 2, 3)",
		"CREATE TABLE three(a, b, c)",
	}
	for _, q := range []string{
		"SELECT rtreecheck()",
		"SELECT rtreecheck('nosuch')",
		"SELECT rtreecheck('three')",
	} {
		var msg [2]string
		for i, drv := range []string{"sqlite3", "sqlite"} {
			db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			for _, s := range setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("%s: %s: %v", drv, s, err)
				}
			}
			var s string
			if err := db.QueryRow(q).Scan(&s); err != nil {
				msg[i] = err.Error()
			}
			db.Close()
		}
		if msg[0] == "" {
			t.Fatalf("the oracle did not reject %q", q)
		}
		if msg[0] != msg[1] {
			t.Errorf("%s\n  cgo: %q\n  mus: %q", q, msg[0], msg[1])
		}
	}
}

// TestRtreecheckDamagedRootByConnection pins the one place rtreecheck()'s
// answer depends on the CONNECTION (engine/rtree_conn.go): with node 1 cut
// short, the connection that created the table still holds it and gets the
// report, while a fresh connection's xConnect refuses the undersized root
// ("undersize RTree blobs", rtree.c:3595) and the call fails.
func TestRtreecheckDamagedRootByConnection(t *testing.T) {
	ctx := t.Context()
	for _, drv := range []string{"sqlite3", "sqlite"} {
		db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(2)
		creator, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			"CREATE VIRTUAL TABLE r2 USING rtree_i32(id, x1, x2)",
			"INSERT INTO r2 VALUES(2, -1*(1<<31), -1*(1<<31)+5)",
			"UPDATE r2_node SET data = X'123456'",
		} {
			if _, err := creator.ExecContext(ctx, s); err != nil {
				t.Fatalf("%s: %s: %v", drv, s, err)
			}
		}
		var rep string
		if err := creator.QueryRowContext(ctx, "SELECT rtreecheck('r2')").Scan(&rep); err != nil {
			t.Errorf("%s: creating connection: %v", drv, err)
		} else if want := "Node 1 is too small (3 bytes)\nWrong number of entries in %_rowid table - expected 0, actual 1"; rep != want {
			t.Errorf("%s: creating connection: %q, want %q", drv, rep, want)
		}
		fresh, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.QueryRowContext(ctx, "SELECT rtreecheck('r2')").Scan(&rep); err == nil {
			t.Errorf("%s: a fresh connection answered %q; C's xConnect refuses the undersized root", drv, rep)
		}
		fresh.Close()
		creator.Close()
		db.Close()
	}
}

// TestRtreeAuxColumnsMatchC pins r-tree AUXILIARY columns ("+name"), which
// rtree.c stores in %_rowid's a0, a1, ... outside the tree (rtree.c:3440-3444,
// 3236-3245): declared untyped whatever follows the name, any value kept as
// bound, carried through UPDATE's delete-and-reinsert, gone with DELETE, and
// only ever after every coordinate.
func TestRtreeAuxColumnsMatchC(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"store and read back", []string{
			"CREATE VIRTUAL TABLE rt USING rtree(id, x1, x2, +label, +c3 BLOB, +n)",
			"PRAGMA table_info(rt)",
			"SELECT sql FROM sqlite_schema WHERE name='rt_rowid'",
			"INSERT INTO rt VALUES(1, 0, 10, 'one', x'0102', 1.5)",
			"INSERT INTO rt VALUES(2, 5, 6, NULL, 22, '7')",
			"INSERT INTO rt(id, x1, x2) VALUES(3, 1, 2)",
			"INSERT INTO rt(x1, x2, label) VALUES(8, 9, 'auto')",
			"SELECT *, typeof(label), typeof(c3), typeof(n) FROM rt",
			"SELECT * FROM rt_rowid ORDER BY rowid",
			"SELECT label FROM rt WHERE x1 >= 5",
			"UPDATE rt SET label = 'uno' WHERE id = 1",
			"UPDATE rt SET x2 = 20 WHERE id = 2",
			"UPDATE rt SET id = 10, n = NULL WHERE id = 3",
			"DELETE FROM rt WHERE label = 'auto'",
			"SELECT * FROM rt",
			"SELECT * FROM rt_rowid ORDER BY rowid",
			"SELECT rtreecheck('rt')",
			"PRAGMA integrity_check",
		}},
		{"declarations", []string{
			"CREATE VIRTUAL TABLE a1 USING rtree(id, x1, x2, +a, y1)",
			"CREATE VIRTUAL TABLE a2 USING rtree(id, +a, x1, x2)",
			"CREATE VIRTUAL TABLE a3 USING rtree(id, +a, +b)",
			"CREATE VIRTUAL TABLE a4 USING rtree_i32(id, x1, x2, y1, y2, +\"q u\")",
			"INSERT INTO a4 VALUES(1, 1, 2, 3, 4, 'v')",
			"SELECT * FROM a4",
			"PRAGMA table_info(a4)",
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "rtree aux: "+tc.name, tc.stmts) })
	}
}
