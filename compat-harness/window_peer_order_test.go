// This file tests that window functions report correct peer ordering in the presence of indexes.
package compat

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

// TestWindowPeerOrderWrongAnswer tests the original wrong answer case that drove the guard.
func TestWindowPeerOrderWrongAnswer(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a,b,c)",
		"INSERT INTO t VALUES(3,'z',1),(1,'x',2),(2,'y',3),(1,'w',4),(3,'v',5)",
	}
	q := "SELECT lead(a) OVER (ORDER BY EXISTS(SELECT 1 FROM (SELECT 1))) FROM t ORDER BY 1"
	differ(t, "peer-order no index", append(append([]string{}, base...), q))
	differAllowingDeclines(t, "peer-order covering index",
		append(append([]string{}, base...), "CREATE INDEX ta ON t(a)", q))
}

// TestWindowPeerOrderServed tests indexes that cannot reorder peer groups.
func TestWindowPeerOrderServed(t *testing.T) {
	// so the only order it can still impose is its trailing rowid's, which is
	// this engine's own.
	differ(t, "index on the partition key", []string{
		"CREATE TABLE t1(id INTEGER PRIMARY KEY, grp_id)",
		"CREATE INDEX i1 ON t1(grp_id)",
		"CREATE VIEW lll AS SELECT row_number() OVER (PARTITION BY grp_id), grp_id, id FROM t1",
		"INSERT INTO t1 VALUES(1,2),(2,3),(3,3),(4,1),(5,1),(6,1),(7,1),(8,1),(9,3),(10,3)," +
			"(11,2),(12,3),(13,3),(14,2),(15,1),(16,2),(17,1),(18,2),(19,3),(20,2)",
		"SELECT * FROM lll",
		"SELECT * FROM lll WHERE grp_id=2",
	})

	pushd := []string{
		"CREATE TABLE t1(a, b, c, d)",
		"INSERT INTO t1 VALUES('A','C',1,0.1),('A','D',2,0.2),('A','E',3,0.3),('A','C',4,0.4)," +
			"('B','D',5,0.5),('B','E',6,0.6),('B','C',7,0.7),('B','D',8,0.8)," +
			"('C','E',9,0.9),('C','C',10,1.0),('C','D',11,1.1),('C','E',12,1.2)",
		"CREATE INDEX i1 ON t1(a)",
		"CREATE INDEX i2 ON t1(b)",
		"CREATE VIEW v2 AS SELECT a, c, max(c) OVER (PARTITION BY a), row_number() OVER () FROM t1",
		"CREATE VIEW v3 AS SELECT b, d, max(d) OVER (PARTITION BY b), row_number() OVER (PARTITION BY b) FROM t1",
		"CREATE TABLE t2(x,y,z)",
		"INSERT INTO t2 VALUES('W',3,1),('W',2,2),('X',1,4),('X',5,7),('Y',1,9),('Y',4,2),('Z',3,3),('Z',3,4)",
	}
	// v2: two INCOMPATIBLE partitions ("PARTITION BY a" and "OVER ()"), which
	// sets SF_MultiPart and blocks push-down outright (select.c:5146), so
	// neither index can be driven by a term this compile never sees, and
	// neither is covering -- the base scan is the plain rowid one in both
	// engines.
	// v3: one index inert (i1 on a, a column the query neither names nor
	// orders by), the other keyed on the partition column.
	// The last two: a ROWS frame spanning UNBOUNDED PRECEDING to UNBOUNDED
	// FOLLOWING is the whole partition however the rows are ordered.
	differ(t, "windowpushd views", append(append([]string{}, pushd...),
		"SELECT * FROM v2",
		"SELECT * FROM v2 WHERE a = 'C'",
		"SELECT * FROM v3",
		"SELECT * FROM v3 WHERE b<'E'",
		"SELECT * FROM v3 WHERE d<0.55",
		"SELECT * FROM ( SELECT x, sum(y) AS s, max(z) AS m, max( max(z) ) OVER "+
			"(PARTITION BY sum(y) ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING ) FROM t2 GROUP BY x )",
		"SELECT * FROM ( SELECT x, sum(y) AS s, max(z) AS m, max( max(z) ) OVER "+
			"(PARTITION BY sum(y) ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING ) FROM t2 GROUP BY x ) WHERE s=6",
	))

	// The SF_MultiPart reasoning, on data deliberately NOT stored in a-order so
	// an index walk and a rowid walk cannot coincide. Verified against 3.53.3:
	// "SELECT * FROM w3 WHERE a<'C'" reports row_number 2,3,4 -- the positions
	// among ALL FIVE rows, in rowid order -- which a pushed-down filter could
	// not produce.
	w3 := []string{
		"CREATE TABLE t3(a, c)",
		"INSERT INTO t3 VALUES('C',1),('A',2),('B',3),('A',4),('C',5)",
		"CREATE INDEX j1 ON t3(a)",
		"CREATE VIEW w3 AS SELECT a, c, max(c) OVER (PARTITION BY a), row_number() OVER () FROM t3",
	}
	differ(t, "SF_MultiPart blocks push-down", append(append([]string{}, w3...),
		"SELECT * FROM w3 ORDER BY 2",
		"SELECT * FROM w3 WHERE a<'C' ORDER BY 2",
	))
}

// TestWindowPeerOrderLoopImmune is the LOOP-ORDER escape
// (windowPeerLoopImmune, engine/window_peer_order.go): a peer group in which
// every source but one holds the same row is ordered by that one source's own
// scan position under EVERY nesting, so which nesting C SQLite picked cannot
// be observed. windowC.test 2.x is the shape -- two CTEs of VALUES cross-joined,
// which the ported loop-order solver declines outright.
func TestWindowPeerOrderLoopImmune(t *testing.T) {
	for _, enc := range []string{"UTF16le", "UTF16be", "UTF8"} {
		differ(t, "windowC "+enc, []string{
			"PRAGMA encoding=" + enc,
			"WITH separator(x) AS (VALUES(',a,'),(',bc,')), value(y) AS (VALUES(1),(x'5585d09013455178cd11ce4a')) " +
				"SELECT group_concat(y,x) OVER (ORDER BY x ROWS BETWEEN 1 PRECEDING AND 1 PRECEDING) FROM separator, value",
		})
	}
	// The control the escape must not swallow: one peer group, BOTH sources
	// varying inside it, which is exactly where two nestings disagree.
	differAllowingDeclines(t, "two varying sources", []string{
		"CREATE TABLE p(x)", "INSERT INTO p VALUES(1),(2)",
		"CREATE TABLE q(y)", "INSERT INTO q VALUES(10),(20)",
		"SELECT group_concat(x||':'||y) OVER (ORDER BY 1 ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM p, q",
	})
}

// TestWindowPeerOrderFuzz is the measurement: random schemas, random index
// sets, random window shapes. Every query carries a total ORDER BY so the
// comparison is order-sensitive on purpose (the corpus is not, and cannot see
// a peer-order divergence at all). Served or declined, never wrong.
func TestWindowPeerOrderFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260905))
	idxSets := [][]string{
		{},
		{"CREATE INDEX x1 ON t(a)"},
		{"CREATE INDEX x1 ON t(b)"},
		{"CREATE INDEX x1 ON t(a,b)"},
		{"CREATE INDEX x1 ON t(a)", "CREATE INDEX x2 ON t(b)"},
		{"CREATE UNIQUE INDEX x1 ON t(c)"},
		{"CREATE INDEX x1 ON t(b DESC)"},
		{"CREATE INDEX x1 ON t(b COLLATE NOCASE)"},
		{"CREATE INDEX x1 ON t(a,b,c,d)"},
	}
	specs := []string{
		"OVER ()",
		"OVER (PARTITION BY a)",
		"OVER (ORDER BY a)",
		"OVER (PARTITION BY a ORDER BY b)",
		"OVER (ORDER BY a DESC)",
		"OVER (ORDER BY 1)",
		"OVER (PARTITION BY b ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING)",
		"OVER (PARTITION BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)",
		"OVER (ORDER BY b RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)",
	}
	funcs := []string{
		"row_number()", "rank()", "dense_rank()", "ntile(3)",
		"lead(c)", "lag(c)", "first_value(c)", "last_value(c)",
		"sum(c)", "count(*)", "max(c)", "group_concat(d,'-')",
	}
	wheres := []string{"", " WHERE a IS NOT NULL", " WHERE b<'m'", " WHERE c>1"}

	declined := 0
	total := 0
	for i := 0; i < 900; i++ {
		idx := idxSets[rng.Intn(len(idxSets))]
		stmts := []string{
			"CREATE TABLE t(a,b,c,d)",
			// Deliberately NOT stored in a- or b-order: an index walk and a
			// rowid walk must be able to disagree.
			"INSERT INTO t VALUES(3,'z',1,'p'),(1,'x',2,'q'),(2,'Y',3,'r'),(1,'w',4,'s')," +
				"(3,'v',5,'t'),(2,'y',6,'u'),(NULL,'x',7,'v'),(1,'z',8,'w')",
		}
		stmts = append(stmts, idx...)
		fn := funcs[rng.Intn(len(funcs))]
		sp := specs[rng.Intn(len(specs))]
		wh := wheres[rng.Intn(len(wheres))]
		q := fmt.Sprintf("SELECT c, %s %s AS w FROM t%s ORDER BY 1, 2", fn, sp, wh)
		stmts = append(stmts, q)
		total++
		if !windowPeerCase(t, fmt.Sprintf("fuzz-%d", i), stmts) {
			declined++
		}
	}
	t.Logf("window peer-order fuzz: %d cases, %d declined", total, declined)
}

// windowPeerCase runs one case and fails on a musql answer that differs from
// the oracle's. It returns false when musql declined the final statement --
// counted, never asserted, for the reason differAllowingDeclines' own doc
// comment gives.
func windowPeerCase(t *testing.T, name string, stmts []string) bool {
	t.Helper()
	oracle := run(t, "cgo", stmts)
	got := run(t, "musql", stmts)
	if len(oracle) != len(got) {
		t.Fatalf("[%s] statement count %d vs %d", name, len(oracle), len(got))
	}
	served := true
	for i := range oracle {
		ob, _ := json.Marshal(oracle[i])
		gb, _ := json.Marshal(got[i])
		if string(ob) == string(gb) {
			continue
		}
		if got[i]["kind"] == "error" && oracle[i]["kind"] != "error" {
			served = false
			continue
		}
		t.Errorf("[%s] musql DIVERGES from C SQLite\n  sql:     %s\n  cgo:     %s\n  musql:  %s",
			name, stmts[i], ob, gb)
	}
	return served
}
