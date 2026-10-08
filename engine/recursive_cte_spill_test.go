package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestRecursiveCTEQueueSpillMatchesMemory: a UNION ALL recursive CTE whose
// queue outgrows recQueueSpillBytes keeps its tail in a temp file. Forced to
// spill almost at once, every query must answer exactly as it does in memory:
// the same rows, values and order, across every storage class and a queue
// that grows, drains and grows again.
func TestRecursiveCTEQueueSpillMatchesMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	buildDB(t, path, `CREATE TABLE edge(a INTEGER, b INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 400)
		 INSERT INTO edge SELECT i, i * 2 FROM c UNION ALL SELECT i, i * 2 + 1 FROM c`)
	queries := []string{
		// Wide frontier: breadth-first over a binary tree.
		`WITH RECURSIVE r(n, d) AS (SELECT 1, 0 UNION ALL SELECT edge.b, r.d + 1 FROM r JOIN edge ON edge.a = r.n WHERE r.d < 9)
		 SELECT n, d FROM r`,
		// Every storage class through the queue.
		`WITH RECURSIVE c(i, t, f, b, z, e) AS (
		   SELECT 1, 'x''y' || char(0, 10, 233, 128512), 2.0, x'00ff', NULL, ''
		   UNION ALL
		   SELECT i + 1, t || i, f * -1.5, b || x'7f', CASE WHEN i % 3 = 0 THEN NULL ELSE i * 9223372036854 END, e || ''
		   FROM c WHERE i < 300)
		 SELECT i, t, typeof(t), f, typeof(f), hex(b), z, typeof(z), e, typeof(e) FROM c`,
		// A bounded one: LIMIT and OFFSET over a frontier that doubles.
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT edge.b FROM c JOIN edge ON edge.a = c.i LIMIT 600 OFFSET 7) SELECT i FROM c`,
	}
	want := make([]string, len(queries))
	for i, q := range queries {
		var err error
		if want[i], err = queryDB(t, path, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	old := recQueueSpillBytes
	recQueueSpillBytes = 256
	defer func() { recQueueSpillBytes = old }()
	for i, q := range queries {
		recQueueSpilled = 0
		got, err := queryDB(t, path, q)
		if err != nil {
			t.Fatalf("spilled %s: %v", q, err)
		}
		if got != want[i] {
			t.Errorf("spilled answer differs for %s", q)
		}
		if recQueueSpilled == 0 {
			t.Errorf("nothing was spilled, so this compared memory with itself: %s", q)
		}
	}
}

// TestRecQueueDiskKeepsFIFOOrder drives the queue directly through every
// transition SQL reaches only by luck: growing past the threshold, draining the
// file to empty, refilling memory, and spilling again, in random interleavings
// of push and pop against a plain slice as the reference FIFO.
func TestRecQueueDiskKeepsFIFOOrder(t *testing.T) {
	old := recQueueSpillBytes
	recQueueSpillBytes = 256
	defer func() { recQueueSpillBytes = old }()
	rng := rand.New(rand.NewSource(5))
	q := newRecQueue(&recQueueSpec{name: "q", nCol: 2, unionAll: true}, UTF8)
	var ref [][]Value
	next := 0
	spilledBefore := recQueueSpilled
	for step := 0; step < 20000; step++ {
		if len(ref) == 0 || rng.Intn(100) < 52 {
			row := []Value{{Typ: Int, I: int64(next)}, {Typ: Text, S: []byte(fmt.Sprintf("row-%d-%s", next, string(rune('a'+next%26))))}}
			next++
			ref = append(ref, append([]Value(nil), row...))
			q.push(row)
			continue
		}
		got, ok := q.pop()
		if q.disk != nil && q.disk.err != nil {
			t.Fatalf("step %d: %v", step, q.disk.err)
		}
		if !ok {
			t.Fatalf("step %d: queue empty with %d rows expected", step, len(ref))
		}
		want := ref[0]
		ref = ref[1:]
		if got[0].I != want[0].I || string(got[1].S) != string(want[1].S) {
			t.Fatalf("step %d: popped %d %q, want %d %q", step, got[0].I, got[1].S, want[0].I, want[1].S)
		}
	}
	for len(ref) > 0 {
		got, ok := q.pop()
		if !ok || got[0].I != ref[0][0].I {
			t.Fatalf("drain: got %v %v, want %d", got, ok, ref[0][0].I)
		}
		ref = ref[1:]
	}
	if _, ok := q.pop(); ok {
		t.Fatal("queue yields a row after draining")
	}
	if recQueueSpilled == spilledBefore {
		t.Fatal("nothing was spilled")
	}
}
