package replication

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// lowImageMin makes an empty node take an image however little its peers hold.
func lowImageMin(t *testing.T) {
	old := imageMinOps
	imageMinOps = 1
	t.Cleanup(func() { imageMinOps = old })
}

func syncerOf(t *testing.T, db *sql.DB) *Syncer {
	t.Helper()
	v, ok := openDBs.Load(db)
	if !ok {
		t.Fatal("not a database Open returned")
	}
	return v.(*Syncer)
}

func imagesOf(t *testing.T, db *sql.DB) int32 {
	t.Helper()
	return syncerOf(t, db).images.Load()
}

// waitImage waits until db's node has installed an image: its rows are visible
// a moment before the install is counted.
func waitImage(t *testing.T, db *sql.DB) {
	t.Helper()
	eventually(t, "an image installed", func() (bool, string) {
		n := imagesOf(t, db)
		return n == 1, fmt.Sprint(n, " images")
	})
}

// The replicated state beyond the visible rows: every clock cell, and the rows
// held out of their tables.
const (
	clockQ = `SELECT tbl, hex(pk), col, hlc, site, hex(val) FROM _repl_clock ORDER BY tbl, pk, col`
	heldQ  = `SELECT tbl, hex(pk) FROM _repl_withheld ORDER BY tbl, pk`
)

// leftovers is what an install left beside the database at path.
func leftovers(t *testing.T, path string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, filepath.Base(path)+".boot") || strings.HasPrefix(n, filepath.Base(path)+imageDirInfix) {
			out = append(out, n)
		}
	}
	return out
}

var imageDDL = []string{
	`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`,
	`CREATE TABLE k(v TEXT)`,
	`CREATE TABLE u(id TEXT PRIMARY KEY, e TEXT UNIQUE)`,
	`CREATE INDEX t_v ON t(v)`,
}

const (
	imageRowsT = `SELECT id, v FROM t ORDER BY id`
	imageRowsK = `SELECT rowid, v FROM k ORDER BY rowid`
	imageRowsU = `SELECT id, e FROM u ORDER BY id`
)

// imageHistory gives a and b a history of every kind: inserts, updates and
// deletes on keyed and keyless tables, and a row each wrote while apart that
// the UNIQUE constraint holds out.
func imageHistory(c *cluster) {
	for i := range 30 {
		c.exec("a", fmt.Sprintf(`INSERT INTO t VALUES(%d, 'a%d')`, i, i), fmt.Sprintf(`INSERT INTO k VALUES('k%d')`, i))
	}
	c.exec("a", `UPDATE t SET v = 'changed' WHERE id < 5`, `DELETE FROM t WHERE id BETWEEN 10 AND 14`, `DELETE FROM k WHERE v = 'k3'`)
	c.exec("b", `INSERT INTO t VALUES(100, 'from b')`, `UPDATE t SET v = 'b won' WHERE id = 20`)
	c.converge("", imageRowsT, imageRowsK)
	c.partition(true)
	c.exec("a", `INSERT INTO u VALUES('x', 'dup')`)
	c.exec("b", `INSERT INTO u VALUES('y', 'dup')`)
	c.partition(false)
	c.converge("", imageRowsT, imageRowsK, imageRowsU, clockQ, heldQ)
}

// TestImageBootstrap: a node that joins with nothing takes a peer's image, not
// its history, and holds what every node holds: the rows, the clock, the rows
// held out, and the next writes either way.
func TestImageBootstrap(t *testing.T) {
	lowImageMin(t)
	c := newCluster(t)
	c.open("a", imageDDL...)
	c.open("b")
	imageHistory(c)
	if dump(t, c.dbs["a"], heldQ) == "" {
		t.Fatal("the history holds no row out; the test means less")
	}
	c.open("c")
	c.converge("", imageRowsT, imageRowsK, imageRowsU, clockQ, heldQ)
	waitImage(t, c.dbs["c"])
	if got := dump(t, c.dbs["c"], `SELECT v FROM _repl_meta WHERE k = 'site'`); got != "c\n" {
		t.Fatalf("c's site is %q", got)
	}
	c.exec("c", `UPDATE t SET v = 'from c' WHERE id = 0`, `DELETE FROM u WHERE id = 'x'`)
	c.exec("a", `INSERT INTO t VALUES(200, 'after c')`)
	c.converge("", imageRowsT, imageRowsK, imageRowsU, clockQ, heldQ)
	if got := dump(t, c.dbs["a"], `SELECT v FROM t WHERE id = 0`); got != "from c\n" {
		t.Fatalf("a: %q", got)
	}
	if l := leftovers(t, filepath.Join(c.dir, "c.db")); len(l) > 0 {
		t.Fatalf("left beside c: %v", l)
	}
}

// TestImageBootstrapPruned: the image carries what pruning took from the log.
func TestImageBootstrapPruned(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, n INT)`)
	c.open("b")
	for i := range 40 {
		c.exec("a", fmt.Sprintf(`INSERT OR REPLACE INTO t VALUES('k%d', %d)`, i%5, i))
	}
	const q = `SELECT id, n FROM t ORDER BY id`
	want := "k0|35\nk1|36\nk2|37\nk3|38\nk4|39\n"
	c.converge(want, q)
	for _, site := range []string{"a", "b"} {
		eventually(t, site+" pruned its row ops", func() (bool, string) {
			got := logRowOps(t, c, site)
			var n int
			fmt.Sscan(got, &n)
			return n < 10, got
		})
	}
	lowImageMin(t)
	c.open("c")
	c.converge(want, q, clockQ)
	waitImage(t, c.dbs["c"])
	c.exec("c", `INSERT INTO t VALUES('from-c', 1)`)
	c.exec("a", `UPDATE t SET n = 99 WHERE id = 'k0'`)
	c.converge("from-c|1\nk0|99\nk1|36\nk2|37\nk3|38\nk4|39\n", q, clockQ)
}

// TestImageWritesDuringTransfer: what the donor commits after the image's cut
// reaches the new node all the same.
func TestImageWritesDuringTransfer(t *testing.T) {
	lowImageMin(t)
	c := newCluster(t)
	c.open("a", `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`)
	for i := range 20 {
		c.exec("a", fmt.Sprintf(`INSERT INTO t VALUES(%d, 'v%d')`, i, i))
	}
	var once sync.Once
	var mu sync.Mutex
	var cutErr error
	a := c.dbs["a"]
	imageCutForTest = func() {
		once.Do(func() {
			for _, q := range []string{`INSERT INTO t VALUES(1000, 'after the cut')`, `UPDATE t SET v = 'changed' WHERE id = 1`, `DELETE FROM t WHERE id = 2`} {
				if _, err := a.Exec(q); err != nil {
					mu.Lock()
					cutErr = err
					mu.Unlock()
				}
			}
		})
	}
	t.Cleanup(func() { imageCutForTest = nil })
	c.open("b")
	got := c.converge("", `SELECT id, v FROM t WHERE id IN (1, 2, 1000) ORDER BY id`, clockQ)
	mu.Lock()
	defer mu.Unlock()
	if cutErr != nil {
		t.Fatal(cutErr)
	}
	if got[0] != "1|changed\n1000|after the cut\n" {
		t.Fatalf("b holds %q", got[0])
	}
	waitImage(t, c.dbs["b"])
}

// TestImageInstallRetried: an install that fails leaves the node as it was,
// and the next round installs the image.
func TestImageInstallRetried(t *testing.T) {
	lowImageMin(t)
	c := newCluster(t)
	c.open("a", imageDDL...)
	c.exec("a", `INSERT INTO t VALUES(1, 'one')`, `INSERT INTO k VALUES('kk')`)
	var fails atomic.Int32
	fails.Store(2)
	imageStagedForTest = func() error {
		if fails.Load() == 0 {
			return nil
		}
		fails.Add(-1)
		return errors.New("injected")
	}
	t.Cleanup(func() { imageStagedForTest = nil })
	c.open("b")
	c.converge("", imageRowsT, imageRowsK, clockQ)
	waitImage(t, c.dbs["b"])
	if f := fails.Load(); f != 0 {
		t.Fatalf("the image went in after %d failures, want 2", 2-f)
	}
	if l := leftovers(t, filepath.Join(c.dir, "b.db")); len(l) > 0 {
		t.Fatalf("left beside b: %v", l)
	}
}

// TestImageInstallCrashed: a crash part way through an install -- the image
// staged and made the node's, its marker written, the database's delta gone and
// the image not yet renamed in -- is finished by the next Open, under the
// node's own identity, and the node syncs on from there.
func TestImageInstallCrashed(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	c.open("a", imageDDL...)
	for i := range 20 {
		c.exec("a", fmt.Sprintf(`INSERT INTO t VALUES(%d, 'v%d')`, i, i))
	}
	path := filepath.Join(c.dir, "c.db")
	db, err := Open(ctx, path, CRDT(), WithSite("c"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	plain, err := openApplyDB(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plain.Query(`SELECT site, seq, hlc, tbl, pk, op, cells FROM main._repl_oplog WHERE site = 'c'`)
	if err != nil {
		t.Fatal(err)
	}
	var own []Op
	for rows.Next() {
		op, err := scanOp(rows)
		if err != nil {
			t.Fatal(err)
		}
		own = append(own, op)
	}
	rows.Close()
	plain.Close()
	if len(own) != 1 {
		t.Fatalf("c's own ops: %v", own)
	}
	m, body, err := syncerOf(t, c.dbs["a"]).store.Image(ctx, imageRequest(bookkeepingLayout))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := stageImage(path, m, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := adoptImage(ctx, staged, m, "c", modeCRDT, own); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CompactSegmentFile(staged); err != nil {
		t.Fatal(err)
	}
	if err := writeDurable(path+markerSuffix, []byte(filepath.Base(staged))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".delta"); err != nil {
		t.Fatal(err)
	}

	c.open("c")
	if got := dump(t, c.dbs["c"], `SELECT count(*) FROM t`); got != "20\n" {
		t.Fatalf("c holds %q rows of t before any sync", got)
	}
	if got := dump(t, c.dbs["c"], `SELECT v FROM _repl_meta WHERE k = 'site'`); got != "c\n" {
		t.Fatalf("c's site is %q", got)
	}
	if l := leftovers(t, path); len(l) > 0 {
		t.Fatalf("left beside c: %v", l)
	}
	c.exec("c", `INSERT INTO t VALUES(500, 'from c')`)
	c.exec("a", `INSERT INTO t VALUES(501, 'from a')`)
	c.converge("", `SELECT count(*), sum(id) FROM t`, clockQ)
	if got := dump(t, c.dbs["a"], `SELECT count(*) FROM _repl_clock WHERE site = 'c'`); got == "0\n" {
		t.Fatal("c's write reached a as no cell of c's")
	}
}

// TestImageOwnSiteAndRange: a node that took an image writes as itself: ops of
// its own site, rowids from its own range, none colliding with the donor's.
func TestImageOwnSiteAndRange(t *testing.T) {
	lowImageMin(t)
	c := newCluster(t)
	c.open("a", imageDDL...)
	for i := range 10 {
		c.exec("a", fmt.Sprintf(`INSERT INTO t(v) VALUES('a%d')`, i), fmt.Sprintf(`INSERT INTO k VALUES('a%d')`, i))
	}
	c.open("b")
	c.converge("", imageRowsT, imageRowsK)
	waitImage(t, c.dbs["b"])
	for i := range 10 {
		c.exec("b", fmt.Sprintf(`INSERT INTO t(v) VALUES('b%d')`, i), fmt.Sprintf(`INSERT INTO k VALUES('b%d')`, i))
	}
	c.converge("", imageRowsT, imageRowsK, clockQ)
	if got := dump(t, c.dbs["a"], `SELECT count(*), count(DISTINCT id) FROM t`); got != "20|20\n" {
		t.Fatalf("t: %q", got)
	}
	if got := dump(t, c.dbs["a"], `SELECT count(*) FROM (SELECT DISTINCT tbl, pk FROM _repl_clock WHERE site = 'b')`); got != "20\n" {
		t.Fatalf("rows of b's on a: %q", got)
	}
	lo, hi := RowidRange("b")
	for _, q := range []string{`SELECT id FROM t WHERE v LIKE 'b%'`, `SELECT rowid FROM k WHERE v LIKE 'b%'`} {
		rows, err := c.dbs["a"].Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id uint64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			if id < lo || id >= hi {
				t.Errorf("%s: rowid %d outside b's range [%d, %d)", q, id, lo, hi)
			}
		}
		rows.Close()
	}
}

// TestImageRefusedFallsBack: a donor of another bookkeeping layout refuses the
// image, and the new node takes the history op by op instead.
func TestImageRefusedFallsBack(t *testing.T) {
	lowImageMin(t)
	c := newCluster(t)
	c.open("a", imageDDL...)
	c.exec("a", `INSERT INTO t VALUES(1, 'one')`, `INSERT INTO k VALUES('kk')`)
	if _, err := syncerOf(t, c.dbs["a"]).store.db.Exec(`INSERT INTO main._repl_meta(k, v) VALUES(?, '2')`, metaLayout); err != nil {
		t.Fatal(err)
	}
	c.open("b")
	c.converge("1|one\n", imageRowsT, imageRowsK, clockQ)
	if n := imagesOf(t, c.dbs["b"]); n != 0 {
		t.Fatalf("b installed %d images, want none", n)
	}
}

// TestImageRefusesAnotherFormat: a request for another layout, or file or
// delta format, is refused before anything is written.
func TestImageRefusesAnotherFormat(t *testing.T) {
	c := newCluster(t)
	c.open("a", imageDDL...)
	st := syncerOf(t, c.dbs["a"]).store
	file, minFile, delta := engine.FormatVersions()
	for _, req := range []ImageRequest{
		{Layout: "2", FileVersion: file, MinFileVersion: minFile, DeltaVersion: delta},
		{Layout: bookkeepingLayout, FileVersion: file - 1, MinFileVersion: minFile, DeltaVersion: delta},
		{Layout: bookkeepingLayout, FileVersion: file + 1, MinFileVersion: file + 1, DeltaVersion: delta},
		{Layout: bookkeepingLayout, FileVersion: file, MinFileVersion: minFile, DeltaVersion: delta - 1},
	} {
		if _, _, err := st.Image(context.Background(), req); !errors.Is(err, ErrImageRefused) {
			t.Errorf("%+v: %v, want ErrImageRefused", req, err)
		}
	}
	if l := leftovers(t, filepath.Join(c.dir, "a.db")); len(l) > 0 {
		t.Fatalf("left beside a: %v", l)
	}
}

// TestImageNotOverOwnData: a node with data of its own is never replaced.
func TestImageNotOverOwnData(t *testing.T) {
	lowImageMin(t)
	c := newCluster(t)
	c.open("a", imageDDL...)
	c.exec("a", `INSERT INTO t VALUES(1, 'one')`)
	c.open("b", `CREATE TABLE mine(x)`, `INSERT INTO mine VALUES(42)`)
	c.converge("", imageRowsT, `SELECT x FROM mine`, clockQ)
	if n := imagesOf(t, c.dbs["b"]); n != 0 {
		t.Fatalf("b installed %d images, want none", n)
	}
	if got := dump(t, c.dbs["a"], `SELECT x FROM mine`); got != "42\n" {
		t.Fatalf("b's own row on a: %q", got)
	}
}
