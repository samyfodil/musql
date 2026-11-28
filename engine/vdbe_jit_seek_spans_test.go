package engine

import (
	"fmt"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// jitDifferential builds a database from setup, answers queries read-only
// with the JIT off and on, applies writes and answers them again, failing on
// any answer that differs. It returns, with the JIT on and before the
// writes, each query's entries into native code that hand it cursors, and
// the entries that went on with the cursors it kept.
func jitDifferential(t *testing.T, setup, queries, writes []string) (enters []int64, resumes int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.musq")
	buildDB(t, path, setup...)
	execDB(t, path, "VACUUM")
	answer := func(p *ReadOnlyPager, jitOn bool) (out []string, enters []int64) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		for _, q := range queries {
			e := vmJITCursorEnters.Load()
			cols, rows, err := p.QueryArgs(fmt.Sprintf("%s -- %v", q, jitOn), nil)
			enters = append(enters, vmJITCursorEnters.Load()-e)
			out = append(out, fmt.Sprint(q, cols, rows, err))
		}
		return out, enters
	}
	vmJITCountCursorEnters = true
	defer func() { vmJITCountCursorEnters = false }()
	for phase := range 2 {
		if phase == 1 {
			execDB(t, path, writes...)
		}
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := answer(p, false)
		// With no GC cycle running: its write barrier sends every seek back
		// to the VDBE while it lasts.
		gc := debug.SetGCPercent(-1)
		runtime.GC()
		res := vmJITResumes.Load()
		got, ent := answer(p, true)
		debug.SetGCPercent(gc)
		resumes += vmJITResumes.Load() - res
		if phase == 0 {
			enters = ent
		}
		p.Close()
		for i := range want {
			if want[i] != got[i] {
				t.Fatalf("JIT diverges:\nvdbe %.600s\njit  %.600s", want[i], got[i])
			}
		}
	}
	return enters, resumes
}

// TestVMJITIdxSeekAcrossSegments: an index seek of a table of three
// segments, each holding rows of nearly every key. Native code took every such
// key back to the VDBE; one index over the table serves them, each key's rows
// segment by segment, which is rowid order. Row order shows in a first row, a
// group_concat and an unordered join; an order the index does not keep (a
// further column) stays the VDBE's. Then the same after writes.
func TestVMJITIdxSeekAcrossSegments(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INT, a INT)",
		"CREATE INDEX tk ON t(k)",
		"CREATE TABLE t2(id INTEGER PRIMARY KEY, k INT, a INT)",
		"CREATE INDEX t2ka ON t2(k, a)",
		"CREATE TABLE u(id INTEGER PRIMARY KEY, w)",
		"INSERT INTO t SELECT value, (value * 7) % 3001, value % 17 FROM generate_series(1, 140000)",
		"INSERT INTO t2 SELECT id, k, (id * 31) % 101 FROM t",
		"INSERT INTO u SELECT value, (value * 13) % 3100 FROM generate_series(1, 400)",
		"INSERT INTO u VALUES(401, NULL), (402, 7.0), (403, '11'), (404, -1)",
	}
	queries := []string{
		"SELECT count(*), sum(x), max(x) FROM (SELECT (SELECT t.id FROM t WHERE t.k = u.w) AS x FROM u)",
		"SELECT u.id, (SELECT t.id FROM t WHERE t.k = u.w) FROM u",
		"SELECT u.id, (SELECT group_concat(t.id) FROM t WHERE t.k = u.w) FROM u WHERE u.id % 9 = 0",
		"SELECT u.id, t.id FROM u JOIN t ON t.k = u.w WHERE u.id % 7 = 0",
		"SELECT u.id, (SELECT t2.id FROM t2 WHERE t2.k = u.w) FROM u",
		"SELECT count(*) FROM u WHERE EXISTS (SELECT 1 FROM t WHERE t.k = u.w AND t.a = 3)",
	}
	writes := []string{
		"UPDATE t SET a = a + 1 WHERE id % 50 = 0",
		"DELETE FROM t WHERE id % 61 = 0",
		"INSERT INTO t VALUES(150000, 5, 3)",
	}
	enters, _ := jitDifferential(t, setup, queries, writes)
	// The first row of each of 404 seeks: native code makes the seek and
	// leaves at the row.
	if segCursorsHere() && enters[1] > 100 {
		t.Errorf("%d entries into native code for %d seeks: a key the segments share leaves it at each seek", enters[1], 404)
	}
	// At most a few per row of u: none walks t for its NULL.
	for i, n := range enters {
		if segCursorsHere() && n > 1000 {
			t.Errorf("%s: %d entries into native code for 404 seeks", queries[i], n)
		}
	}
	t.Logf("entries per query %v", enters)
}

// TestIndexSeekOfNullReadsNothing: a correlated equality whose probe is NULL
// matches no row, so its seek reads none, where it was left to a scan of the
// table that tested every row.
func TestIndexSeekOfNullReadsNothing(t *testing.T) {
	old := vmJITEnabled
	vmJITEnabled = false
	defer func() { vmJITEnabled = old }()
	path := filepath.Join(t.TempDir(), "n.musq")
	buildDB(t, path,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INT, s TEXT)",
		"CREATE INDEX tk ON t(k)",
		"CREATE INDEX ts ON t(s)",
		"CREATE TABLE u(id INTEGER PRIMARY KEY, w)",
		"CREATE TABLE v(id INTEGER PRIMARY KEY, w TEXT)",
		"INSERT INTO t SELECT value, value % 50, 's' || (value % 50) FROM generate_series(1, 20000)",
		"INSERT INTO u VALUES(1, NULL), (2, 7), (3, NULL), (4, NULL), (5, NULL)",
		"INSERT INTO v SELECT id, 's' || w FROM u")
	execDB(t, path, "VACUUM")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var rc RowCounter
	p.SetRowCounter(&rc)
	want := fmt.Sprint(typedRows([][]Value{{{Typ: Int, I: 1}, {}}, {{Typ: Int, I: 2}, {Typ: Int, I: 19957}}, {{Typ: Int, I: 3}, {}}, {{Typ: Int, I: 4}, {}}, {{Typ: Int, I: 5}, {}}}))
	for _, q := range []string{
		"SELECT u.id, (SELECT max(t.id) FROM t WHERE t.k = u.w) FROM u",
		"SELECT v.id, (SELECT max(t.id) FROM t WHERE t.s = v.w) FROM v",
		"SELECT u.id, (SELECT max(t.id) FROM t WHERE t.k = u.w AND t.s = 's7') FROM u",
	} {
		read := rc.Read
		_, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := fmt.Sprint(typedRows(rows)); got != want {
			t.Errorf("%s: %s", q, got)
		}
		// The outer rows and the 400 of key 7, not the table's 20,000.
		if n := rc.Read - read; n > 1000 {
			t.Errorf("%s: %d rows read", q, n)
		}
	}
}

// TestVMJITCoroutineKeepsCursors: a FROM-clause subquery run as a co-routine
// hands its consumer one row per yield. Where nothing outside the statement
// runs before it resumes, native code keeps its cursors across the yield
// instead of handing them back and taking them again. Consumers: an
// aggregate, a filter and a nested co-routine, a join, a LIMIT, a recursive
// CTE; every answer the VDBE's.
func TestVMJITCoroutineKeepsCursors(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INT, a INT)",
		"CREATE INDEX tk ON t(k)",
		"CREATE TABLE u(id INTEGER PRIMARY KEY, w INT)",
		"CREATE TABLE v(id INTEGER PRIMARY KEY, z INT)",
		"INSERT INTO t SELECT value, value % 500, value % 17 FROM generate_series(1, 20000)",
		"INSERT INTO u SELECT value, (value * 13) % 520 FROM generate_series(1, 3000)",
		"INSERT INTO v SELECT value, value * 3 FROM generate_series(0, 9)",
	}
	queries := []string{
		"SELECT count(*), sum(x), max(x) FROM (SELECT (SELECT t.a FROM t WHERE t.k = u.w) AS x FROM u)",
		"SELECT group_concat(x) FROM (SELECT u.id * 1000 + (SELECT t.id FROM t WHERE t.k = u.w) AS x FROM u WHERE u.id < 200)",
		"SELECT sum(y), count(*) FROM (SELECT y FROM (SELECT (SELECT t.a FROM t WHERE t.k = u.w) AS y FROM u) WHERE y > 3)",
		"SELECT count(*), sum(d.y * v.z) FROM (SELECT u.id AS i, (SELECT t.a FROM t WHERE t.k = u.w) AS y FROM u) d JOIN v ON v.id = d.i % 10",
		"SELECT sum(x) FROM (SELECT (SELECT t.a FROM t WHERE t.k = u.w) AS x FROM u LIMIT 77)",
		"SELECT * FROM (SELECT u.id, (SELECT t.id FROM t WHERE t.k = u.w) AS x FROM u) LIMIT 5",
		"WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 700) SELECT sum((SELECT t.a FROM t WHERE t.k = c.i)), count(*) FROM c",
	}
	writes := []string{
		"UPDATE t SET a = a + 1 WHERE id % 50 = 0",
		"INSERT INTO t VALUES(30000, 5, 3)",
	}
	_, resumes := jitDifferential(t, setup, queries, writes)
	if segCursorsHere() && resumes < 3000 {
		t.Errorf("%d entries went on with kept cursors: a co-routine's yield hands them back", resumes)
	}
	t.Logf("%d entries resumed", resumes)
}
