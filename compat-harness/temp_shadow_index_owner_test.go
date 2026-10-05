package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTempShadowIndexOwnerAnswers tests that indexes owned by main tables
// shadowed by temp tables of the same name work correctly.
func TestTempShadowIndexOwnerAnswers(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"temp table created last", `
			CREATE TABLE t0(c0, c1);
			CREATE INDEX i0 ON t0(c0);
			INSERT INTO t0 VALUES(1,2);
			CREATE TEMP TABLE t0(c0, c1);
			SELECT c0 FROM main.t0 WHERE c0=1;
			SELECT c0 FROM main.t0 NOT INDEXED WHERE c0=1;
			SELECT c0 FROM main.t0 WHERE +c0=1;
			SELECT count(*) FROM main.t0;
			SELECT count(*) FROM temp.t0`},
		// The temp table gets rows of its own, so an index rebuilt from the
		// WRONG owner would answer with plausible-looking values rather than
		// nothing -- the shape a count() alone would miss.
		{"temp table created first", `
			CREATE TEMP TABLE tbl(a,b,c);
			INSERT INTO tbl VALUES(1,2,3);
			CREATE TABLE main.tbl(a,b,c);
			INSERT INTO main.tbl VALUES(9,9,9);
			CREATE INDEX main.tbli ON tbl(a,b,c);
			SELECT a FROM main.tbl WHERE a=9;
			SELECT a FROM main.tbl WHERE a=1;
			SELECT a FROM temp.tbl WHERE a=1;
			SELECT count(*) FROM main.tbl;
			SELECT count(*) FROM temp.tbl`},
		{"both sides populated", `
			CREATE TABLE t1(k);
			CREATE INDEX ix ON t1(k);
			INSERT INTO t1 VALUES(10),(20);
			CREATE TEMP TABLE t1(k);
			INSERT INTO temp.t1 VALUES(30),(40);
			SELECT k FROM main.t1 WHERE k=10;
			SELECT k FROM main.t1 WHERE k=30;
			SELECT k FROM temp.t1 WHERE k=30;
			SELECT k FROM main.t1 ORDER BY k;
			SELECT k FROM temp.t1 ORDER BY k`},
		// Writes AFTER the shadow: the index must keep tracking main's table.
		{"insert after the shadow", `
			CREATE TABLE t2(k);
			CREATE INDEX ix2 ON t2(k);
			INSERT INTO t2 VALUES(5);
			CREATE TEMP TABLE t2(k);
			INSERT INTO main.t2 VALUES(6);
			DELETE FROM main.t2 WHERE k=5;
			SELECT k FROM main.t2 WHERE k=6;
			SELECT k FROM main.t2 WHERE k=5;
			SELECT count(*) FROM main.t2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stmts []string
			for _, s := range strings.Split(tc.sql, ";") {
				if s = strings.TrimSpace(s); s != "" {
					stmts = append(stmts, s)
				}
			}
			m := run(t, "musql", stmts)
			c := run(t, "cgo", stmts)
			for i := range stmts {
				mb, _ := json.Marshal(m[i])
				cb, _ := json.Marshal(c[i])
				if string(mb) != string(cb) {
					t.Errorf("STMT %s\n  cgo:    %s\n  musql: %s", stmts[i], cb, mb)
				}
			}
		})
	}
}
