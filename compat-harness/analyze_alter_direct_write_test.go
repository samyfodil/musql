package compat

import (
	"encoding/json"
	"testing"
)

// Tests ANALYZE and ALTER TABLE RENAME direct writes to sqlite_stat1 and sqlite_sequence.
func TestAnalyzeAndAlterDirectWriteAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"whole-schema analyze", []string{
			"CREATE TABLE t1(a,b)", "CREATE INDEX t1a ON t1(a)",
			"CREATE TABLE t2(c)", "CREATE INDEX t2c ON t2(c)",
			"INSERT INTO t1 VALUES(1,1),(2,2),(3,3)",
			"INSERT INTO t2 VALUES(1),(2)",
			"ANALYZE",
			"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx",
		}},
		{"analyze one index leaves the rest", []string{
			"CREATE TABLE t1(a,b)", "CREATE INDEX t1a ON t1(a)", "CREATE INDEX t1b ON t1(b)",
			"CREATE TABLE t2(c)", "CREATE INDEX t2c ON t2(c)",
			"INSERT INTO t1 VALUES(1,1),(2,2),(3,3)", "INSERT INTO t2 VALUES(1),(2)",
			"ANALYZE",
			"INSERT INTO t1 VALUES(4,4),(5,5)",
			"ANALYZE t1a",
			"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx",
		}},
		{"analyze one table leaves the other", []string{
			"CREATE TABLE t1(a)", "CREATE INDEX t1a ON t1(a)",
			"CREATE TABLE t2(c)", "CREATE INDEX t2c ON t2(c)",
			"INSERT INTO t1 VALUES(1),(2),(3)", "INSERT INTO t2 VALUES(1),(2)",
			"ANALYZE",
			"INSERT INTO t2 VALUES(3),(4),(5)",
			"ANALYZE t2",
			"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx",
		}},
		{"re-analyze replaces rather than duplicates", []string{
			"CREATE TABLE t1(a)", "CREATE INDEX t1a ON t1(a)",
			"INSERT INTO t1 VALUES(1),(2)",
			"ANALYZE", "ANALYZE", "ANALYZE",
			"SELECT count(*) FROM sqlite_stat1",
			"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl,idx",
		}},
		// ANALYZE must NOT move last_insert_rowid(); the direct write never
		// sets it, where the old InsertArgs path did and had to undo it.
		{"analyze does not move last_insert_rowid", []string{
			"CREATE TABLE t1(a)", "CREATE INDEX t1a ON t1(a)",
			"INSERT INTO t1 VALUES(8)",
			"ANALYZE",
			"SELECT last_insert_rowid()",
		}},
		{"alter rename rewrites sqlite_sequence", []string{
			"CREATE TABLE s(id INTEGER PRIMARY KEY AUTOINCREMENT, v)",
			"INSERT INTO s(v) VALUES('a'),('b')",
			"SELECT name,seq FROM sqlite_sequence ORDER BY name",
			"ALTER TABLE s RENAME TO s2",
			"SELECT name,seq FROM sqlite_sequence ORDER BY name",
			"INSERT INTO s2(v) VALUES('c')",
			"SELECT id,v FROM s2 ORDER BY id",
			"SELECT name,seq FROM sqlite_sequence ORDER BY name",
		}},
		{"rename leaves another table's sequence alone", []string{
			"CREATE TABLE p(id INTEGER PRIMARY KEY AUTOINCREMENT, v)",
			"CREATE TABLE q(id INTEGER PRIMARY KEY AUTOINCREMENT, v)",
			"INSERT INTO p(v) VALUES('a')", "INSERT INTO q(v) VALUES('x'),('y')",
			"ALTER TABLE p RENAME TO p2",
			"SELECT name,seq FROM sqlite_sequence ORDER BY name",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := run(t, "musql", tc.stmts)
			c := run(t, "cgo", tc.stmts)
			for i := range tc.stmts {
				mb, _ := json.Marshal(m[i])
				cb, _ := json.Marshal(c[i])
				if string(mb) != string(cb) {
					t.Errorf("STMT %s\n  cgo:    %s\n  musql: %s", tc.stmts[i], cb, mb)
				}
			}
		})
	}
}
