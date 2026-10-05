// Window functions are tested directly as VDBE output.
package compat

import (
	"fmt"
	"testing"
)

var windowSetup = []string{
	`CREATE TABLE w(g TEXT, v INTEGER)`,
	`INSERT INTO w VALUES('a',1),('a',2),('a',3),('b',10),('b',20)`,
	`CREATE TABLE tie(g TEXT, v INTEGER, n TEXT)`,
	`INSERT INTO tie VALUES('a',1,'p'),('a',1,'q'),('a',2,'r'),('b',5,'s')`,
	`CREATE TABLE nul(g TEXT, v INTEGER)`,
	`INSERT INTO nul VALUES('a',NULL),('a',2),(NULL,3),(NULL,4)`,
}

var windowCorpus = []string{
	`SELECT g,v,row_number() OVER (PARTITION BY g ORDER BY v), rank() OVER (PARTITION BY g ORDER BY v), sum(v) OVER (PARTITION BY g ORDER BY v) FROM w ORDER BY g,v`,
	`SELECT row_number() OVER () FROM w`,
	`SELECT g, count(*) OVER (PARTITION BY g) FROM w ORDER BY g`,
	`SELECT g, sum(v) OVER (PARTITION BY g) FROM w ORDER BY g`,
	`SELECT g,v,n,row_number() OVER (PARTITION BY g ORDER BY v), rank() OVER (PARTITION BY g ORDER BY v), dense_rank() OVER (PARTITION BY g ORDER BY v) FROM tie ORDER BY g,v,n`,
	`SELECT v, sum(v) OVER (ORDER BY v) FROM tie ORDER BY v`,
	`SELECT v, count(*) OVER (ORDER BY v) FROM tie ORDER BY v`,
	`SELECT v, cume_dist() OVER (ORDER BY v) FROM tie ORDER BY v`,
	`SELECT v, percent_rank() OVER (ORDER BY v) FROM tie ORDER BY v`,
	`SELECT g,v,row_number() OVER (PARTITION BY g ORDER BY v) FROM nul ORDER BY g,v`,
	`SELECT g,v,sum(v) OVER (PARTITION BY g ORDER BY v) FROM nul ORDER BY g,v`,

	`SELECT v, avg(v) OVER (ORDER BY v) FROM w ORDER BY v`,
	`SELECT v, min(v) OVER (ORDER BY v), max(v) OVER (ORDER BY v) FROM w ORDER BY v`,
	`SELECT v, row_number() OVER (ORDER BY v DESC) FROM w ORDER BY v`,
	`SELECT v, sum(v*2) OVER (ORDER BY v) FROM w ORDER BY v`,

	// DISTINCT / ORDER BY / LIMIT all apply after the window values exist.
	`SELECT g, row_number() OVER (PARTITION BY g ORDER BY v) FROM w ORDER BY g LIMIT 3`,
	`SELECT DISTINCT g, count(*) OVER (PARTITION BY g) FROM w ORDER BY g`,
	`SELECT v FROM w ORDER BY row_number() OVER (ORDER BY v DESC)`,

	// DISTINCT INSIDE a window function is rejected by C SQLite for EVERY
	// aggregate, with or without an ORDER BY -- while the same aggregates
	// WITHOUT OVER accept DISTINCT normally (the last two here). This engine
	// used to compute an answer for all five, which is a wrong answer rather
	// than a gap: the statement is one C SQLite refuses to run.
	`SELECT sum(DISTINCT v) OVER () FROM w`,
	`SELECT count(DISTINCT v) OVER () FROM w`,
	`SELECT avg(DISTINCT v) OVER () FROM w`,
	`SELECT group_concat(DISTINCT v) OVER () FROM w`,
	`SELECT max(DISTINCT v) OVER () FROM w`,
	`SELECT sum(DISTINCT v) OVER (ORDER BY v) FROM w`,
	`SELECT sum(DISTINCT v) FROM w`,
	`SELECT count(DISTINCT v) FROM w`,
}

func TestWindowFunctionsMatchCSQLite(t *testing.T) {
	wrong := 0
	for _, q := range windowCorpus {
		stmts := append(append([]string{}, windowSetup...), q)
		c := run(t, "cgo", stmts)
		m := run(t, "musql", stmts)
		ci, mi := c[len(c)-1], m[len(m)-1]
		if fmt.Sprint(ci) != fmt.Sprint(mi) {
			wrong++
			t.Errorf("[%s] DIVERGES\n  cgo:    %v\n  musql: %v", q, ci, mi)
		}
	}
	t.Logf("window-function parity: %d statements, wrong=%d", len(windowCorpus), wrong)
	if wrong != 0 {
		t.Fatalf("window-function parity FAILED: wrong=%d (must be 0)", wrong)
	}
}
