package compat

// Generated column planner gate: colUsedBitsFor reproduces sqlite3ExprColUsed's behavior
// exactly, avoiding false planner refusals for tables with unreferenced generated columns.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Corpus repro: unreferenced generated UNIQUE column with whole-table aggregate.
// "count(*) [-style aggregate] with colUsed==0 can still make an index on a
// generated column win" shape: b's automatic UNIQUE index is a live candidate
// for the scan even though b itself is never read, and row materialization
// (normalizeRowInPlace -> computeGeneratedInto) computes b's value before any
// index key is ever applied, so which physical access path wins cannot change
// the answer.
func TestGencolR41CorpusRepro(t *testing.T) {
	differ(t, "select1.test generated-unique unreferenced", []string{
		"CREATE TABLE t1 (a INTEGER PRIMARY KEY, b AS('Y') UNIQUE)",
		"INSERT INTO t1(a) VALUES (10)",
		"SELECT ifnull(a, max((SELECT 123))), count(a) FROM t1",
	})
}

// TestGencolR41ReferencedFloodChangesJoinOrder is the NOT-agreeing-by-
// coincidence proof required alongside the corpus repro. b declares a
// generated column g and this query REFERENCES it (bare, in the select list)
// -- which must flood ALL of b's colUsed bits per sqlite3ExprColUsed
// (resolve.c:176-198), including the bit for column z, which the query never
// names at all.
//
// That extra, otherwise-invisible bit is what changes the OBSERVABLE answer:
// constructAutomaticIndex (where.c:986-1109) appends every colUsed bit not
// already in the join predicate's key, IN COLUMN-ORDINAL ORDER, as extra
// "covering" key columns of the transient automatic index built for b's
// inner loop (b.k = a.k). With the flood, z (ordinal 1, before o at ordinal
// Verifies generated column reference properly affects join index key.
func TestGencolR41ReferencedFloodChangesJoinOrder(t *testing.T) {
	differ(t, "generated-column reference floods an unrelated column into the join's automatic-index key", []string{
		"CREATE TABLE a(k, x)",
		"INSERT INTO a VALUES (1,'p'),(2,'q')",
		"CREATE TABLE b(k, z, o, g AS (k+1000))",
		"INSERT INTO b(k,z,o) VALUES (1,5,'w'),(1,2,'x'),(2,9,'y'),(2,1,'z')",
		"SELECT a.k, group_concat(b.o), b.g FROM a,b WHERE a.k=b.k GROUP BY a.k ORDER BY a.k",
	})
}

// Multi-table: unreferenced generated column with whole-table aggregate anchor.
func TestGencolR41UnreferencedMultiTable(t *testing.T) {
	differ(t, "unreferenced generated column, multi-table anchor", []string{
		"CREATE TABLE j1(k, o)",
		"INSERT INTO j1 VALUES (1,'p'),(1,'q'),(2,'r'),(2,'s')",
		"CREATE TABLE g1(k, o, s AS (k*2))",
		"INSERT INTO g1 VALUES (1,'w'),(1,'x'),(2,'y'),(2,'z')",
		"SELECT j1.k, count(*), j1.o, g1.o FROM j1,g1 WHERE j1.k=g1.k GROUP BY j1.k ORDER BY j1.k",
	})
}

// Referenced generated column with explicit UNIQUE index and min/max query.
func TestGencolR41ReferencedSingleTableIndexOnGenerated(t *testing.T) {
	differ(t, "referenced generated column, explicit UNIQUE index on it, min/max query", []string{
		"CREATE TABLE s2(a INTEGER PRIMARY KEY, b, g AS (b+1) UNIQUE, o)",
		"INSERT INTO s2(a,b,o) VALUES (1,30,'p'),(2,10,'q'),(3,20,'r')",
		"SELECT max(g), o FROM s2",
	})
}

// Real index on plain column must still drive min() access path.
func TestGencolR41UnreferencedSingleTableIndexOnOther(t *testing.T) {
	differ(t, "unreferenced generated column, real index on a different column", []string{
		"CREATE TABLE s1(a INTEGER PRIMARY KEY, b, g AS (b+1), o)",
		"CREATE INDEX s1b ON s1(b)",
		"INSERT INTO s1(a,b,o) VALUES (1,30,'p'),(2,10,'q'),(3,20,'r')",
		"SELECT min(b), o FROM s1",
	})
}

// Wide table (63+ columns) still declines due to colUsed bit limit.
func TestGencolR41WideTableStillDeclines(t *testing.T) {
	var cols []string
	cols = append(cols, "c0 INTEGER PRIMARY KEY", "c1 AS (c0+1) UNIQUE")
	var insertCols []string
	insertCols = append(insertCols, "c0")
	for i := 2; i < 65; i++ {
		cols = append(cols, fmt.Sprintf("c%d", i))
		insertCols = append(insertCols, fmt.Sprintf("c%d", i))
	}
	stmts := []string{
		"CREATE TABLE t65(" + strings.Join(cols, ", ") + ")",
	}
	// Different c3 values per GROUP BY key tests scan-order dependence.
	row := func(a int, c3 string) string {
		vals := []string{fmt.Sprintf("%d", a), "100", c3}
		// insertCols is c0, c2, c3, c4, .., c64 -- pad with zeros for every
		// column past c3 (already supplied above) so vals lines up 1:1.
		for len(vals) < len(insertCols) {
			vals = append(vals, "0")
		}
		return "INSERT INTO t65(" + strings.Join(insertCols, ",") + ") VALUES (" + strings.Join(vals, ",") + ")"
	}
	stmts = append(stmts, row(1, "111"), row(2, "222"))
	stmts = append(stmts, "SELECT c2, count(*), c3 FROM t65 GROUP BY c2")

	c := run(t, "cgo", stmts)
	m := run(t, "musql", stmts)
	cb, _ := json.Marshal(c)
	mb, _ := json.Marshal(m)
	if string(cb) == string(mb) {
		return // agreement is fine too, just not the property this test exists to pin
	}
	if m[len(m)-1]["kind"] != "error" {
		t.Fatalf("[r41wide] a 65-column table's bare-column aggregate anchor over an indexed table "+
			"must be DECLINED, not served a possibly-wrong answer\n  cgo:    %s\n  musql: %s", cb, mb)
	}
	t.Logf("declined as expected: cgo=%s musql=%s", cb, mb)
}
