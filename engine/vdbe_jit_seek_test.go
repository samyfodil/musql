package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestVMJITSeekDifferential: joins and lookups whose inner level is a rowid or
// index seek on a segment table, which native code walks through a copy of the
// seek's rows (seekSnap). Lists shorter and longer than jitSeekRows, LIMITs
// that stop a list part way, rows narrower than the table after an ADD COLUMN,
// REAL affinity and NULLs; with the JIT and without, every row must match.
func TestVMJITSeekDifferential(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(11))
	val := func() string {
		switch rng.Intn(8) {
		case 0:
			return "NULL"
		case 1:
			return fmt.Sprintf("'s%d'", rng.Intn(5))
		default:
			return fmt.Sprint(rng.Intn(9))
		}
	}
	var setup []string
	setup = append(setup,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, a, b REAL, c TEXT)",
		"CREATE INDEX t_a ON t(a)",
		"CREATE TABLE u(id INTEGER PRIMARY KEY, k, w)")
	for i := 1; i <= 2400; i++ {
		a := rng.Intn(40)
		if i%3 == 0 {
			a = rng.Intn(3) // lists past jitSeekRows
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %d, %s, %s)", i, a, val(), val()))
	}
	for i := 1; i <= 60; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO u VALUES(%d, %d, %d)", i, rng.Intn(42), rng.Intn(2600)))
	}
	// The rows go into segments (VACUUM), which the seeks read; e is added
	// after, so every row is narrower than the table.
	setup = append(setup, "ALTER TABLE t ADD COLUMN d DEFAULT 7", "VACUUM", "ALTER TABLE t ADD COLUMN e DEFAULT 'z'")
	queries := []string{
		"SELECT u.id, t.id, t.a, t.b, t.c, t.d, t.e FROM u JOIN t ON t.a = u.k ORDER BY 1, 2",
		"SELECT u.id, t.id, t.b FROM u JOIN t ON t.a = u.k WHERE u.id < 20 LIMIT 37",
		"SELECT u.id, t.id FROM u JOIN t ON t.a = u.k WHERE t.b > 3 ORDER BY 1, 2",
		"SELECT count(*), sum(t.b), max(t.c) FROM u JOIN t ON t.a = u.k",
		"SELECT u.id, t.id, t.c, t.d, t.e FROM u JOIN t ON t.id = u.w ORDER BY 1",
		"SELECT u.id, t.a FROM u LEFT JOIN t ON t.id = u.w ORDER BY 1",
		"SELECT u.id, (SELECT max(t.b) FROM t WHERE t.a = u.k) FROM u ORDER BY 1",
		"SELECT u.id, (SELECT t.c FROM t WHERE t.id = u.w) FROM u ORDER BY 1",
		"SELECT u.id, t.id FROM u, t WHERE t.a = u.k AND t.id % 7 = 0 ORDER BY 1, 2",
	}
	for i := 1; i < 2600; i += 97 {
		queries = append(queries, fmt.Sprintf("SELECT b, c, d, e FROM t WHERE id = %d", i))
	}
	// served is how many rows the seeks handed out: native code walking one
	// must account for the rows it moved over as advance() would.
	run := func(jitOn bool) (out []string, served int64) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		n, err := OpenOrCreate(filepath.Join(t.TempDir(), "j.musq"))
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
		SegRowsServedForTest()
		out = make([]string, len(queries))
		for i, q := range queries {
			cols, rows, err := n.Query(q, nil)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			out[i] = fmt.Sprint(cols, rows)
		}
		return out, SegRowsServedForTest()
	}
	want, wantServed := run(false)
	before := vmJITSeekSnaps.Load()
	got, gotServed := run(true)
	snaps := vmJITSeekSnaps.Load() - before
	for i := range queries {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges on %s\nvdbe %s\njit  %s", queries[i], want[i], got[i])
		}
	}
	if gotServed != wantServed {
		t.Fatalf("the seeks served %d rows with the JIT, %d without", gotServed, wantServed)
	}
	if snaps == 0 {
		t.Fatalf("native code walked no seek: the test checks nothing")
	}
	t.Logf("%d queries agree; native code walked a seek in %d entries", len(queries), snaps)
}
