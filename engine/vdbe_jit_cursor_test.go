package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// TestVMJITCursorDifferential: random joins, subqueries and derived tables over
// small tables of integers, NULLs, reals and text -- loops of Column, Eq, Gt,
// IfNot and Next over materialized cursors -- run with the JIT and without.
// Every row and every error must match. Native code reads and advances those
// cursors itself, so a position, a row or a rowid it leaves wrong shows up as
// a different answer here.
func TestVMJITCursorDifferential(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(7))
	vals := func() string {
		switch rng.Intn(14) {
		case 0:
			return "NULL"
		case 1:
			return fmt.Sprintf("%g", float64(rng.Intn(9))/2)
		case 2:
			return fmt.Sprintf("'%d'", rng.Intn(6))
		case 3:
			return []string{"'x'", "'y'", "''"}[rng.Intn(3)]
		default:
			return fmt.Sprint(rng.Intn(6))
		}
	}
	var setup []string
	for _, tbl := range []string{"a", "b", "c"} {
		setup = append(setup, fmt.Sprintf("CREATE TABLE %s(id INTEGER PRIMARY KEY, x, y, z)", tbl))
		for i := 1; i <= 12+rng.Intn(14); i++ {
			setup = append(setup, fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, %s, %s)", tbl, i, vals(), vals(), vals()))
		}
	}
	col := func(tbl string) string { return tbl + "." + []string{"x", "y", "id"}[rng.Intn(3)] }
	pred := func(ts ...string) string {
		var ps []string
		for range 1 + rng.Intn(3) {
			l, r := col(ts[rng.Intn(len(ts))]), col(ts[rng.Intn(len(ts))])
			op := []string{"=", "<", ">", "<=", ">=", "<>", "IS", "IS NOT"}[rng.Intn(8)]
			if rng.Intn(3) == 0 {
				r = fmt.Sprint(rng.Intn(6))
			}
			ps = append(ps, fmt.Sprintf("%s %s %s", l, op, r))
		}
		return strings.Join(ps, []string{" AND ", " OR "}[rng.Intn(2)])
	}
	var queries []string
	for range 600 {
		switch rng.Intn(9) {
		case 0:
			queries = append(queries, fmt.Sprintf("SELECT a.id, b.id FROM a, b WHERE %s ORDER BY 1, 2", pred("a", "b")))
		case 1:
			queries = append(queries, fmt.Sprintf("SELECT a.id, b.id, c.id FROM a, b, c WHERE %s ORDER BY 1, 2, 3", pred("a", "b", "c")))
		case 2:
			queries = append(queries, fmt.Sprintf("SELECT a.id, b.id, b.x FROM a LEFT JOIN b ON %s ORDER BY 1, 2", pred("a", "b")))
		case 3:
			queries = append(queries, fmt.Sprintf("SELECT count(*), sum(a.x), max(b.y) FROM a JOIN b ON %s", pred("a", "b")))
		case 4:
			queries = append(queries, fmt.Sprintf("SELECT id FROM a WHERE x IN (SELECT y FROM b WHERE %s) ORDER BY 1", pred("a", "b")))
		case 5:
			queries = append(queries, fmt.Sprintf("SELECT d1.x, d2.y FROM (SELECT x, id FROM a WHERE id > %d) d1, (SELECT y, id FROM b) d2 WHERE d1.x = d2.y ORDER BY 1, 2", rng.Intn(5)))
		case 6:
			queries = append(queries, fmt.Sprintf("SELECT a.id, (SELECT count(*) FROM b WHERE b.x = a.x AND b.id > a.id) FROM a WHERE %s ORDER BY 1", pred("a")))
		case 7:
			queries = append(queries, fmt.Sprintf("SELECT a.x, count(*) FROM a, b WHERE %s GROUP BY a.x ORDER BY 1, 2", pred("a", "b")))
		default:
			queries = append(queries, fmt.Sprintf("SELECT a.id, b.id FROM a JOIN b ON a.x = b.x JOIN c ON c.y = b.y WHERE %s ORDER BY 1, 2", pred("a", "b", "c")))
		}
	}

	run := func(jitOn bool) []string {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		db, err := Create(filepath.Join(t.TempDir(), "j.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		p, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		out := make([]string, len(queries))
		for i, q := range queries {
			cols, rows, err := p.QueryArgs(q, nil)
			if err != nil {
				out[i] = "error: " + err.Error()
				continue
			}
			out[i] = fmt.Sprint(cols, rows)
		}
		return out
	}
	want := run(false)
	before := vmJITCursorSyncs.Load()
	got := run(true)
	syncs := vmJITCursorSyncs.Load() - before
	for i := range queries {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges on %s\nvdbe %s\njit  %s", queries[i], want[i], got[i])
		}
	}
	failed := 0
	for _, g := range got {
		if strings.HasPrefix(g, "error: ") {
			failed++
		}
	}
	if failed > len(queries)/20 {
		t.Fatalf("%d of %d queries failed to run: %s", failed, len(queries), got[0])
	}
	if syncs == 0 {
		t.Fatalf("no query advanced a cursor natively: the test checks nothing")
	}
	t.Logf("%d queries agree; native code advanced cursors in %d entries", len(queries), syncs)
}
