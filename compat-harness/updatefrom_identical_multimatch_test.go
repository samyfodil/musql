// This file tests UPDATE ... FROM with multi-match ambiguity where the
// SET value is identical across all matches.
// 400 (a BEFORE UPDATE trigger body whose own UPDATE ... FROM sets a column
// to the literal NULL across a self-referential NATURAL LEFT FULL JOIN).
package compat

import "testing"

// TestUpdateFromIdenticalMultiMatchAgrees covers the mined corpus shapes plus
// one constructed case where EVERY target row (not just one) is ambiguous.
func TestUpdateFromIdenticalMultiMatchAgrees(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"upfrom1-5.1", []string{
			"CREATE TABLE t1(a)",
			"INSERT INTO t1(a) VALUES(5)",
			"CREATE VIEW t2 AS SELECT a FROM t1 UNION ALL SELECT a FROM t1",
			"CREATE TABLE t3(b,c)",
			"INSERT INTO t3(b,c) VALUES(1,2)",
			"UPDATE t3 SET (c,b) = (SELECT 3,4) FROM t1, t2",
			"SELECT * FROM t3",
		}},
		{"upfrom4-400", []string{
			"CREATE TABLE t2(x,y,z PRIMARY KEY) WITHOUT ROWID",
			"INSERT INTO t2 VALUES(89,-89,6)",
			"CREATE TABLE t1(a INT,b TEXT,c TEXT,d REAL) STRICT",
			"INSERT INTO t1 VALUES(1,'xyz','def',4.5)",
			`CREATE TRIGGER t1tr BEFORE UPDATE ON t1 BEGIN
			   INSERT INTO t1(a,b) VALUES(1000,'uvw');
			   UPDATE t1 SET b=NULL FROM (SELECT CAST(a AS varchar) FROM t1 ORDER BY b) NATURAL LEFT FULL JOIN t1 AS text;
			 END`,
			"UPDATE t1 SET b=b|100",
			"SELECT * FROM t1 ORDER BY a",
		}},
		{"every-target-row-ambiguous", []string{
			"CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT)",
			"INSERT INTO tgt VALUES(1,0),(2,0)",
			"CREATE TABLE src(k INT)",
			"INSERT INTO src VALUES(100),(100),(100)",
			// No WHERE: an unconditional cross join, so BOTH target rows
			// match all three src rows -- every candidate SET tuple for
			// every target row is the same constant 42.
			"UPDATE tgt SET val = 42 FROM src",
			"SELECT * FROM tgt ORDER BY id",
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestUpdateFromDifferingMultiMatchStillDeclines is a SECOND, independent
// excluded-class pin from TestUpdateFromMultiMatchStaysDeclined
// (update_from_multimatch_test.go): it exercises the WITHOUT ROWID target
// branch of the same ambiguity check (identifyTargetRow's PRIMARY-KEY-tuple
// path, rather than an ordinary table's rowid), with genuinely DIFFERING
// SET values across the ambiguous matches -- confirming setValueTuplesEqual
// correctly still declines when the tuples are NOT identical, on the code
// path the rowid-table pin does not cover.
func TestUpdateFromDifferingMultiMatchStillDeclines(t *testing.T) {
	stmts := []string{
		"CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT) WITHOUT ROWID",
		"CREATE TABLE src(k INT, v INT)",
		"INSERT INTO tgt VALUES(1,0),(2,0)",
		"INSERT INTO src VALUES(1,10),(1,20)",
		"UPDATE tgt SET val = src.v FROM src WHERE src.k = tgt.id",
	}
	res := run(t, "musql", stmts)
	last := res[len(res)-1]
	if last["kind"] != "error" {
		t.Fatalf("expected musql to still decline a WITHOUT ROWID target with a genuinely differing multi-match, got: %v", last)
	}
	oracle := run(t, "cgo", stmts)
	oLast := oracle[len(oracle)-1]
	if oLast["kind"] == "error" {
		t.Fatalf("expected the oracle to accept the differing multi-match (its choice is merely undefined, not rejected) -- pin is stale: %v", oLast)
	}
}
