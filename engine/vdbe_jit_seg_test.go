package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestVMJITSegmentModeDifferential: loops native code runs over a segment
// table's column blocks in place (segment mode) and the rowid seeks it makes
// itself (VSeekRowid): keys no row has, REAL and NULL keys, loops whose body
// exits to the VDBE (a function) between Nexts, TEXT compared in order and
// under a numeric affinity, constant TEXTs, NULLs and REALs in an INTEGER
// column; then the same after writes the segments no longer answer alone,
// and after an ADD COLUMN that leaves them narrower than the table. With the
// JIT and without, every row and the rows the scans served must match.
func TestVMJITSegmentModeDifferential(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(13))
	texts := []string{"", "a", "ab", "abc", "b", "m", "zz", "12", " 12 ", "1e5", "+1.5", "-.5", "5.", "1x", "0x10",
		"1995-03-15", "1995-03-16", "1996-12-31", "\t7\n", "e5", "9 9"}
	lit := func(s string) string { return "'" + s + "'" }
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, a INT, b REAL, n NUMERIC, c TEXT, d TEXT, k INT)",
		"CREATE TABLE u(id INTEGER PRIMARY KEY, w, s TEXT, f REAL)",
	}
	for i := 1; i <= 5000; i++ {
		a := fmt.Sprint(rng.Intn(20))
		switch i % 17 {
		case 0:
			a = "NULL"
		case 5:
			a = fmt.Sprintf("%d.5", rng.Intn(20))
		}
		n := lit(texts[rng.Intn(len(texts))])
		if i%3 == 0 {
			n = fmt.Sprint(rng.Intn(30))
		}
		c := lit(texts[rng.Intn(len(texts))])
		if i%23 == 0 {
			c = "NULL"
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %s, %d.25, %s, %s, %s, %d)",
			i, a, rng.Intn(9), n, c, lit(texts[rng.Intn(len(texts))]), rng.Intn(5200)))
	}
	for i := 1; i <= 80; i++ {
		w := fmt.Sprint(rng.Intn(5300) - 50)
		switch i % 9 {
		case 0:
			w = "NULL"
		case 4:
			w = fmt.Sprintf("%d.0", rng.Intn(5000))
		case 7:
			w = lit(fmt.Sprint(rng.Intn(5000)))
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO u VALUES(%d, %s, %s, %d.5)", i, w, lit(texts[rng.Intn(len(texts))]), rng.Intn(9)))
	}
	setup = append(setup, "VACUUM")
	queries := []string{
		"SELECT id, a, b, n, c, d FROM t WHERE a > 15",
		"SELECT id, upper(c), a FROM t WHERE a > 12 AND b < 4",
		"SELECT id, length(d) FROM t WHERE c < 'b' LIMIT 333",
		"SELECT count(*), sum(a), sum(b) FROM t WHERE c >= 'ab' AND c <= 'm'",
		"SELECT id FROM t WHERE d BETWEEN '1995-03-15' AND '1996-12-31'",
		"SELECT id, n, c FROM t WHERE n < c",
		"SELECT id, n, d FROM t WHERE n >= d AND a < 5",
		"SELECT count(*) FROM t WHERE n > '5'",
		"SELECT id FROM t WHERE c = 'abc' OR d <> 'zz' AND a = 3",
		"SELECT u.id, t.id, t.a, t.c FROM u JOIN t ON t.id = u.w ORDER BY 1",
		"SELECT u.id, t.id, t.n, t.d FROM u JOIN t ON t.id = u.w WHERE t.c > u.s ORDER BY 1",
		"SELECT u.id, t.b FROM u LEFT JOIN t ON t.id = u.w ORDER BY 1",
		"SELECT u.id, t1.id, t2.id, t2.c FROM u JOIN t t1 ON t1.id = u.w JOIN t t2 ON t2.id = t1.k ORDER BY 1",
		"SELECT t1.id, t2.a, upper(t2.c) FROM t t1 JOIN t t2 ON t2.id = t1.k WHERE t1.a = 7 ORDER BY 1",
		"SELECT count(*), sum(t2.b) FROM t t1 JOIN t t2 ON t2.id = t1.k WHERE t2.c < t1.d",
		"SELECT u.id, (SELECT t.d FROM t WHERE t.id = u.w) FROM u ORDER BY 1",
		"SELECT count(*) FROM u WHERE EXISTS (SELECT 1 FROM t WHERE t.id = u.w AND t.c >= 'b')",
	}
	writes := []string{
		"UPDATE t SET c = 'zzz', a = a + 1 WHERE id % 50 = 0",
		"DELETE FROM t WHERE id % 61 = 0",
		"INSERT INTO t VALUES(6000, 3, 1.25, 'x', 'abc', '1995-03-15', 10)",
	}
	run := func(jitOn bool) (out []string, served int64) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		n, err := OpenOrCreate(filepath.Join(t.TempDir(), "g.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer n.Close()
		exec := func(s string) {
			if _, _, err := n.ExecArgs(s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			n.CommitIfAutocommit()
		}
		for _, s := range setup {
			exec(s)
		}
		SegRowsServedForTest()
		query := func() {
			for _, q := range queries {
				cols, rows, err := n.Query(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				out = append(out, fmt.Sprint(q, cols, rows))
			}
		}
		query()
		for _, s := range writes {
			exec(s)
		}
		query()
		exec("ALTER TABLE t ADD COLUMN e DEFAULT 'q'")
		query()
		return out, SegRowsServedForTest()
	}
	want, wantServed := run(false)
	walks, seeks := vmJITSegWalks.Load(), vmJITTabSeeks.Load()
	got, gotServed := run(true)
	walks, seeks = vmJITSegWalks.Load()-walks, vmJITTabSeeks.Load()-seeks
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges:\nvdbe %s\njit  %s", want[i], got[i])
		}
	}
	if gotServed != wantServed {
		t.Fatalf("the scans served %d rows with the JIT, %d without", gotServed, wantServed)
	}
	if segCursorsHere() && (walks == 0 || seeks == 0) {
		t.Fatalf("segment walks %d, native seeks %d: the test checks nothing", walks, seeks)
	}
	t.Logf("%d queries agree; %d entries walked a segment, %d seeked one", len(want), walks, seeks)
}

// segCursorsHere is whether this build hands cursors over in segment mode.
func segCursorsHere() bool { return jit.VMSegCursor }

// TestVMJITLikeDifferential: LIKE against constant patterns, which native
// code matches (VLike), over TEXTs in both cases, NULLs, non-TEXTs and
// embedded NULs, NOT LIKE, and patterns it leaves to the VDBE ('_', an
// ESCAPE); then the same under case_sensitive_like. With the JIT and without,
// every row must match.
func TestVMJITLikeDifferential(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(14))
	words := []string{"green", "GREEN", "Forest", "brass", "BRASS", "polished", "MEDIUM", "Customer", "Complaints", "x", "%", "_"}
	setup := []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT, n)"}
	for i := 1; i <= 3000; i++ {
		var b strings.Builder
		for range rng.Intn(4) {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(words[rng.Intn(len(words))])
		}
		s := "'" + b.String() + "'"
		switch i % 29 {
		case 0:
			s = "NULL"
		case 7:
			s = "'gr' || char(0) || 'een'"
		case 11:
			s = fmt.Sprint(rng.Intn(100))
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %s, %d)", i, s, rng.Intn(9)))
	}
	setup = append(setup, "VACUUM")
	queries := []string{
		"SELECT id, s FROM t WHERE s LIKE '%green%'",
		"SELECT count(*) FROM t WHERE s NOT LIKE 'MEDIUM%'",
		"SELECT id FROM t WHERE s LIKE '%BRASS'",
		"SELECT id FROM t WHERE s LIKE '%customer%complaints%' AND n < 5",
		"SELECT id, s LIKE 'green' FROM t WHERE n = 3",
		"SELECT id FROM t WHERE s LIKE 'Forest'",
		"SELECT count(*) FROM t WHERE s LIKE '%'",
		"SELECT id FROM t WHERE s LIKE 'g_een%'",
		"SELECT id FROM t WHERE s LIKE '%x\\%%' ESCAPE '\\'",
		"SELECT id FROM t WHERE s LIKE '%G%r%'",
		// The LIKE of a seeked row runs in the VDBE's loop.
		"SELECT t1.id, t2.s FROM t t1 JOIN t t2 ON t2.id = t1.n WHERE t2.s LIKE '%green%'",
		"SELECT t1.id FROM t t1 JOIN t t2 ON t2.id = t1.n WHERE t2.s NOT LIKE 'b%' AND t1.s LIKE '%s'",
	}
	run := func(jitOn bool) (out []string) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		n, err := OpenOrCreate(filepath.Join(t.TempDir(), "l.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer n.Close()
		exec := func(s string) {
			if _, _, err := n.ExecArgs(s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			n.CommitIfAutocommit()
		}
		for _, s := range setup {
			exec(s)
		}
		for _, cs := range []string{"OFF", "ON", "OFF"} {
			exec("PRAGMA case_sensitive_like=" + cs)
			for _, q := range queries {
				cols, rows, err := n.Query(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				out = append(out, fmt.Sprint(cs, q, cols, rows))
			}
		}
		return out
	}
	want, got := run(false), run(true)
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges:\nvdbe %s\njit  %s", want[i], got[i])
		}
	}
}

// TestVMJITInSubDifferential: uncorrelated IN subqueries, which native code
// probes in the set the VDBE builds (VInSub): INTEGER sets, one holding a
// NULL, an empty one, NOT IN, and sets it leaves to the VDBE (TEXT and REAL
// members, a TEXT affinity), probed with INTEGERs, NULLs, REALs and TEXTs.
// With the JIT and without, every row must match.
func TestVMJITInSubDifferential(t *testing.T) {
	lowEntryThreshold(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	rng := rand.New(rand.NewSource(15))
	setup := []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, a INT, b, c TEXT)", "CREATE TABLE u(k INT, r REAL, s TEXT)"}
	for i := 1; i <= 3000; i++ {
		b := fmt.Sprint(rng.Intn(60))
		switch i % 13 {
		case 0:
			b = "NULL"
		case 5:
			b = fmt.Sprintf("%d.5", rng.Intn(60))
		case 9:
			b = fmt.Sprintf("'%d'", rng.Intn(60))
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %d, %s, '%d')", i, rng.Intn(80), b, rng.Intn(60)))
	}
	for i := range 40 {
		setup = append(setup, fmt.Sprintf("INSERT INTO u VALUES(%d, %d.0, '%d')", rng.Intn(70), rng.Intn(70), i))
	}
	setup = append(setup, "INSERT INTO u VALUES(NULL, NULL, NULL)", "VACUUM")
	queries := []string{
		"SELECT id FROM t WHERE a IN (SELECT k FROM u WHERE k IS NOT NULL)",
		"SELECT id FROM t WHERE a NOT IN (SELECT k FROM u WHERE k IS NOT NULL)",
		"SELECT id, a IN (SELECT k FROM u) FROM t WHERE id % 7 = 0",
		"SELECT count(*) FROM t WHERE a NOT IN (SELECT k FROM u)",
		"SELECT id FROM t WHERE b IN (SELECT k FROM u WHERE k < 30)",
		"SELECT id FROM t WHERE a IN (SELECT k FROM u WHERE k > 1000)",
		"SELECT id FROM t WHERE a NOT IN (SELECT k FROM u WHERE k > 1000)",
		"SELECT id FROM t WHERE a IN (SELECT r FROM u)",
		"SELECT id FROM t WHERE a IN (SELECT s FROM u)",
		"SELECT id FROM t WHERE c IN (SELECT k FROM u WHERE k IS NOT NULL)",
		"SELECT t1.id FROM t t1 JOIN t t2 ON t2.id = t1.a WHERE t2.a IN (SELECT k FROM u WHERE k IS NOT NULL)",
	}
	run := func(jitOn bool) (out []string) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		n, err := OpenOrCreate(filepath.Join(t.TempDir(), "i.musq"))
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
		for _, q := range queries {
			cols, rows, err := n.Query(q, nil)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			out = append(out, fmt.Sprint(q, cols, rows))
		}
		return out
	}
	want, got := run(false), run(true)
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges:\nvdbe %s\njit  %s", want[i], got[i])
		}
	}
}

// TestVMJITIdxSeekDifferential: integer equality seeks native code makes
// through a segment table's index (VSeekIdx) -- in EXISTS bodies, joins and
// correlated subqueries, over a table of several segments, with keys no row
// holds, NULL, REAL and TEXT keys, an index whose order the seek keeps (a
// PRIMARY KEY of two columns, or both its columns held equal), subroutines
// that open and close the cursor per call, and a loop body that exits to the
// VDBE between rows, which takes the cursor back -- then the same after
// writes the segments no longer answer alone. With the JIT and without, every row must
// match.
func TestVMJITIdxSeekDifferential(t *testing.T) {
	lowEntryThreshold(t)
	countSyncs(t)
	if !vmJITEnabled {
		t.Skip("JIT not enabled on this platform")
	}
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INT, a INT, c TEXT)",
		"CREATE INDEX tk ON t(k)",
		"CREATE TABLE p(k INT NOT NULL, j INT NOT NULL, v INT, PRIMARY KEY(k, j))",
		"CREATE TABLE u(id INTEGER PRIMARY KEY, w, s TEXT)",
		"INSERT INTO t SELECT value, value / 7, value % 13, 'c' || (value % 5) FROM generate_series(1, 90000)",
		"INSERT INTO p SELECT value / 4, value % 4, value FROM generate_series(1, 40000)",
	}
	keys := []string{"5", "12856", "12857", "0", "-3", "99999", "NULL", "7.0", "7.5", "'11'", "'x'", "6000", "1"}
	for i, k := range keys {
		setup = append(setup, fmt.Sprintf("INSERT INTO u VALUES(%d, %s, 's%d')", i+1, k, i%3))
	}
	for i := len(keys) + 1; i <= 300; i++ {
		setup = append(setup, fmt.Sprintf("INSERT INTO u VALUES(%d, %d, 's%d')", i, (i*311)%13000, i%3))
	}
	setup = append(setup, "VACUUM")
	queries := []string{
		"SELECT count(*) FROM u WHERE EXISTS (SELECT 1 FROM t WHERE t.k = u.w AND t.a <> 3)",
		"SELECT count(*) FROM u WHERE NOT EXISTS (SELECT 1 FROM t WHERE t.k = u.w AND t.a > 10)",
		"SELECT u.id, t.id, t.a FROM u JOIN t ON t.k = u.w ORDER BY 1, 2",
		"SELECT u.id, (SELECT sum(t.a) FROM t WHERE t.k = u.w) FROM u ORDER BY 1",
		"SELECT u.id, t.id, upper(t.c) FROM u JOIN t ON t.k = u.w WHERE t.a < 9 ORDER BY 1, 2",
		"SELECT count(*) FROM u WHERE EXISTS (SELECT 1 FROM p WHERE p.k = u.w AND p.j <> 2)",
		"SELECT u.id, p.j, p.v FROM u JOIN p ON p.k = u.w ORDER BY 1, 2",
		"SELECT u.id, group_concat(p.v) FROM u JOIN p ON p.k = u.w AND p.j = u.id % 4 GROUP BY u.id",
		"SELECT count(*) FROM u WHERE EXISTS (SELECT 1 FROM p WHERE p.k = u.w AND p.j = 3) AND EXISTS (SELECT 1 FROM t WHERE t.k = u.w)",
	}
	writes := []string{
		"UPDATE t SET a = a + 1 WHERE id % 50 = 0",
		"DELETE FROM t WHERE id % 61 = 0",
		"INSERT INTO t VALUES(95000, 5, 3, 'new')",
	}
	run := func(jitOn bool) (out []string) {
		old := vmJITEnabled
		vmJITEnabled = jitOn
		defer func() { vmJITEnabled = old }()
		n, err := OpenOrCreate(filepath.Join(t.TempDir(), "x.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer n.Close()
		exec := func(s string) {
			if _, _, err := n.ExecArgs(s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			n.CommitIfAutocommit()
		}
		for _, s := range setup {
			exec(s)
		}
		query := func() {
			for _, q := range queries {
				cols, rows, err := n.Query(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				out = append(out, fmt.Sprint(q, cols, rows))
			}
		}
		query()
		for _, s := range writes {
			exec(s)
		}
		query()
		return out
	}
	want := run(false)
	seeks, syncs := vmJITIdxSeeks.Load(), vmJITIdxSyncs.Load()
	got := run(true)
	seeks, syncs = vmJITIdxSeeks.Load()-seeks, vmJITIdxSyncs.Load()-syncs
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("JIT diverges:\nvdbe %.600s\njit  %.600s", want[i], got[i])
		}
	}
	if segCursorsHere() && (seeks == 0 || syncs == 0) {
		t.Fatalf("native index seeks %d, cursors taken back %d: the test checks nothing", seeks, syncs)
	}
	t.Logf("%d queries agree; %d native index seeks, %d taken back", len(want), seeks, syncs)
}
