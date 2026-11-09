package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestVMJITAggGatherDifferential: hashed GROUP BYs over segment tables whose
// steps native code gathers and the VDBE replays (gatherAggStep): keys read
// as bare columns, whose group shows its first row's value (1 before 1.0); a
// LEFT JOIN's null row stepped too; groups arriving in an index's order; a
// sorted seek into one segment with tied keys; batches past the gather
// buffer; and an aggregate that raises part way. With the JIT and without,
// every answer must match, and native code must have gathered.
func TestVMJITAggGatherDifferential(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(13))
	keys := []string{"1", "1.0", "2", "'2'", "2.0", "NULL", "'x'", "3", "x'01'"}
	setup := []string{
		"CREATE TABLE c(id INTEGER PRIMARY KEY, k INTEGER, name TEXT)",
		"CREATE TABLE o(id INTEGER PRIMARY KEY, ck INTEGER, amt REAL, note TEXT)",
		"CREATE TABLE g(a, b INTEGER, v INTEGER)",
		"CREATE INDEX g_ba ON g(b, a)",
		"CREATE TABLE big(k INTEGER, v INTEGER)",
	}
	for i := 1; i <= 400; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO c VALUES(%d, %d, 'n%d')", i, rng.Intn(60), rng.Intn(7)))
	}
	for i := 1; i <= 3000; i++ {
		note := "'plain'"
		if rng.Intn(9) == 0 {
			note = "'special requests'"
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO o VALUES(%d, %d, %d.5, %s)", i, rng.Intn(450), rng.Intn(17), note))
	}
	for i := 0; i < 2000; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO g VALUES(%s, %d, %d)", keys[rng.Intn(len(keys))], rng.Intn(5), rng.Intn(1000)))
	}
	setup = append(setup, "INSERT INTO big VALUES(1, 9223372036854775807)")
	for i := 0; i < 600; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO big VALUES(%d, 1)", i%3))
	}
	setup = append(setup, "VACUUM")
	queries := []string{
		"SELECT a, count(*), sum(v) FROM g GROUP BY a",
		"SELECT a, b, count(*), max(v), min(v) FROM g GROUP BY a, b",
		"SELECT a, count(*) FROM g WHERE v > 100 GROUP BY a ORDER BY 2 DESC, 1",
		"SELECT b, a, count(*) FROM g INDEXED BY g_ba GROUP BY b, a",
		"SELECT b, a, total(v) FROM g INDEXED BY g_ba GROUP BY b, a LIMIT 3",
		"SELECT c.id, count(o.id) FROM c LEFT JOIN o ON o.ck = c.id AND o.note NOT LIKE '%special%' GROUP BY c.id",
		"SELECT n, count(*) FROM (SELECT c.id, count(o.id) AS n FROM c LEFT JOIN o ON o.ck = c.id GROUP BY c.id) GROUP BY n ORDER BY 2 DESC, 1 DESC",
		"SELECT c.name, count(o.id), total(o.amt) FROM c LEFT JOIN o ON o.ck = c.k GROUP BY c.name",
		"SELECT c.k, group_concat(o.id) FROM c JOIN o ON o.ck = c.k GROUP BY c.k",
		"SELECT c.id, sum(o.amt) FROM c LEFT JOIN o ON o.ck = c.id GROUP BY c.id HAVING count(o.id) > 3",
		"SELECT o.ck, count(DISTINCT o.amt) FROM o GROUP BY o.ck",
		"SELECT k, sum(v) FROM big GROUP BY k",
	}
	run := func(jitOn bool) []string {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		n, err := OpenOrCreate(filepath.Join(t.TempDir(), "g.musq"))
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
			q := queries[i%len(queries)]
			cols, rows, err := n.Query(q, nil)
			out[i] = fmt.Sprint(cols, rows, err)
		}
		return out
	}
	want := run(false)
	before, seeks := vmJITAggGathered.Load(), segSortedSeeksServed.Load()
	got := run(true)
	gathered, seeks := vmJITAggGathered.Load()-before, segSortedSeeksServed.Load()-seeks
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges on %s\nvdbe %s\njit  %s", queries[i%len(queries)], want[i], got[i])
		}
	}
	if gathered == 0 {
		t.Fatalf("native code gathered no step: the test checks nothing")
	}
	if seeks == 0 {
		t.Fatalf("the segments served no sorted seek")
	}
	t.Logf("%d queries agree; %d steps gathered, %d sorted seeks", len(queries), gathered, seeks)
}
