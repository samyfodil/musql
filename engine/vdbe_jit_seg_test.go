package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
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
