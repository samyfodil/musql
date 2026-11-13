package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestVMJITPlainAggGatherDifferential: aggregates with no GROUP BY, whose
// steps native code gathers (gatherAggStep), many of them in correlated
// subqueries whose loops read the enclosing query's columns natively
// (jitOuterCols): outer keys of every type and NULL, two levels up, a bare
// column that keeps the step in the VDBE, and a sum that overflows part way.
// With the JIT and without, every answer must match, and native code must have
// gathered.
func TestVMJITPlainAggGatherDifferential(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(29))
	keys := []string{"1", "1.0", "2", "'2'", "NULL", "'x'", "3", "x'01'"}
	setup := []string{
		"CREATE TABLE c(id INTEGER PRIMARY KEY, k INTEGER, name TEXT)",
		"CREATE TABLE o(id INTEGER PRIMARY KEY, ck INTEGER, amt REAL)",
		"CREATE TABLE g(a, v INTEGER)",
		"CREATE TABLE big(k INTEGER, v INTEGER)",
	}
	for i := 1; i <= 200; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO c VALUES(%d, %d, 'n%d')", i, rng.Intn(40), rng.Intn(7)))
	}
	for i := 1; i <= 2000; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO o VALUES(%d, %d, %d.25)", i, rng.Intn(45), rng.Intn(17)))
	}
	for i := 0; i < 600; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO g VALUES(%s, %d)", keys[rng.Intn(len(keys))], rng.Intn(1000)))
	}
	for i := 0; i < 600; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO big VALUES(%d, 1)", i%3))
	}
	setup = append(setup, "INSERT INTO big VALUES(1, 9223372036854775807)", "VACUUM")
	queries := []string{
		"SELECT count(*), sum(amt), avg(amt), min(amt), max(amt), total(ck) FROM o",
		"SELECT count(*), avg(v) FROM g WHERE v > 300",
		"SELECT c.id, (SELECT avg(amt) FROM o WHERE o.ck = c.k) FROM c ORDER BY 1",
		"SELECT c.id FROM c WHERE c.id > (SELECT avg(o.ck) * 0.9 FROM o WHERE o.ck = c.k OR o.ck = c.id) ORDER BY 1",
		"SELECT a, (SELECT count(*) || ',' || sum(v) FROM g g2 WHERE g2.a = g.a) FROM g ORDER BY rowid LIMIT 60",
		"SELECT a, (SELECT count(*) FROM g g2 WHERE g2.a IS g.a AND g2.v < g.v) FROM g ORDER BY rowid LIMIT 60",
		"SELECT c.id, (SELECT (SELECT count(*) FROM o WHERE o.ck = c.k AND o.amt > x.amt) FROM o x WHERE x.id = c.id) FROM c ORDER BY 1",
		"SELECT c.id, (SELECT max(amt) || ':' || id FROM o WHERE o.ck = c.k) FROM c ORDER BY 1",
		"WITH t AS (SELECT ck, sum(amt) s FROM o GROUP BY ck) SELECT t1.ck FROM t t1 WHERE t1.s > (SELECT avg(s) * 1.2 FROM t t2 WHERE t2.ck % 3 = t1.ck % 3) ORDER BY 1",
		"SELECT k, (SELECT sum(v) FROM big b2 WHERE b2.k = big.k) FROM big GROUP BY k",
		"SELECT sum(v) FROM big",
	}
	run := func(jitOn bool) []string {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		n, err := OpenOrCreate(filepath.Join(t.TempDir(), "p.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer n.Close()
		for _, s := range setup {
			if _, _, err := n.ExecArgs(s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			n.CommitIfAutocommit()
		}
		out := make([]string, 2*len(queries))
		for i := range out {
			cols, rows, err := n.Query(queries[i%len(queries)], nil)
			out[i] = fmt.Sprint(cols, rows, err)
		}
		return out
	}
	want := run(false)
	before := vmJITAggGathered.Load()
	got := run(true)
	gathered := vmJITAggGathered.Load() - before
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges on %s\nvdbe %s\njit  %s", queries[i%len(queries)], want[i], got[i])
		}
	}
	if gathered == 0 {
		t.Fatalf("native code gathered no step: the test checks nothing")
	}
	t.Logf("%d queries agree; %d steps gathered", len(queries), gathered)
}
