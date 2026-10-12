package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/internal/mmapfile"
)

// TestRowSpillAnswersAsMemory runs one statement sequence on two sessions, one
// spilling its written rows every couple of KB (rowSpillThreshold) and one never,
// and compares the whole table after every statement and again after a reopen.
// The sequence goes through every way a row store's rows are read and replaced:
// puts over spilled rows, rowid moves, deletes, UNIQUE conflict resolution, a
// statement that fails partway, savepoints rolled back to and released, a
// transaction rolled back, commits.
func TestRowSpillAnswersAsMemory(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, u TEXT UNIQUE, v INTEGER, b BLOB)`,
		`CREATE INDEX t_v ON t(v)`,
		`BEGIN`,
	}
	for i := 1; i <= 300; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'u%d', %d, zeroblob(%d))`, i, i, i%13, 200+i))
	}
	stmts = append(stmts,
		`UPDATE t SET v = v + 100 WHERE id % 7 = 0`,
		`UPDATE t SET id = id + 1000 WHERE id % 11 = 0`, // the rowid moves
		`DELETE FROM t WHERE id % 5 = 0`,
		`INSERT OR REPLACE INTO t VALUES(2000, 'u3', 1, x'01')`, // replaces id 3 through u
		`INSERT INTO t VALUES(2001, 'u4', 1, x'02')`,            // fails: u4 exists
		`SAVEPOINT a`,
		`UPDATE t SET b = zeroblob(5000) WHERE v = 3`,
		`DELETE FROM t WHERE v = 4`,
		`ROLLBACK TO a`,
		`UPDATE t SET u = u || '!' WHERE v = 6`,
		`RELEASE a`,
		`COMMIT`,
		`BEGIN`,
		`DELETE FROM t WHERE v < 5`,
		`INSERT INTO t SELECT id + 5000, u || 'x', v, b FROM t WHERE v = 7`,
		`ROLLBACK`,
		`INSERT INTO t SELECT id + 5000, u || 'y', v, b FROM t WHERE v > 8`,
		`UPDATE t SET v = -v WHERE id > 5000`,
		// A catalog change makes the commit rewrite the file, and the rows the
		// INSERT spilled in order are then copied as the segments they are.
		`BEGIN`,
		`CREATE TABLE t3(x)`,
		`INSERT INTO t SELECT id + 9000, u || 'z', v, b FROM t`,
		`COMMIT`,
	)
	dump := func(t *testing.T, n *Session) string {
		t.Helper()
		_, rows, err := n.Query(`SELECT id, u, v, length(b), hex(substr(b, 1, 4)) FROM t ORDER BY id`, nil)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, r := range rows {
			for _, v := range r {
				fmt.Fprintf(&b, "%d:%d:%s|", v.Typ, v.I, v.S)
			}
			b.WriteByte('\n')
		}
		// The equality index has to answer the same rows as the scan.
		_, seek, err := n.Query(`SELECT count(*) FROM t WHERE v = 3`, nil)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "v=3: %d", seek[0][0].I)
		return b.String()
	}
	run := func(t *testing.T, threshold int64) (each []string, reopened string) {
		saved := rowSpillThreshold
		rowSpillThreshold = threshold
		defer func() { rowSpillThreshold = saved }()
		path := filepath.Join(t.TempDir(), "s.musq")
		n, err := Create(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			err := n.Exec(s)
			if _, cerr := n.Commit(); cerr != nil {
				t.Fatal(cerr)
			}
			each = append(each, fmt.Sprint(err != nil))
			if !strings.HasPrefix(s, "CREATE") {
				each = append(each, dump(t, n))
			}
		}
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Discard()
		return each, dump(t, r)
	}
	memEach, memReopen := run(t, 1<<62)
	// Both spill formats: records, and segments a commit can copy as they are
	// (spillRun), which must answer exactly as the rows they hold.
	for _, segs := range []bool{false, true} {
		if segs && !mmapfile.CanMapAt {
			continue // segments spill only where the platform maps (js does not)
		}
		saved := spillSegments
		spillSegments = segs
		SpilledRowsForTest()
		spillSegsCopied, spillsAdopted = 0, 0
		spillEach, spillReopen := run(t, 16<<10)
		spillSegments = saved
		if SpilledRowsForTest() == 0 {
			t.Fatal("nothing spilled -- this test proves nothing")
		}
		if segs && spillSegments && spillSegsCopied == 0 {
			t.Fatal("no spilled segment was copied -- the copy went untested")
		}
		if segs && spillSegments && spillsAdopted == 0 {
			t.Fatal("no spill file became the database file -- adopting one went untested")
		}
		for i := range memEach {
			if memEach[i] != spillEach[i] {
				t.Fatalf("segments %v: step %d differs\n  memory: %.300s\n  spill:  %.300s", segs, i, memEach[i], spillEach[i])
			}
		}
		if memReopen != spillReopen {
			t.Fatalf("segments %v: after the reopen\n  memory: %.300s\n  spill:  %.300s", segs, memReopen, spillReopen)
		}
	}
}
