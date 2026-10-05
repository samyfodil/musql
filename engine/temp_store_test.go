package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The TEMP database is the connection's own (db->aDb[1], opened by
// sqlite3OpenTempDatabase, build.c:2073), so nothing a connection creates with
// the TEMP keyword reaches main: main's segment file and delta are byte-identical
// whether or not temp objects exist.
func TestTempDatabaseLeavesMainFileUntouched(t *testing.T) {
	build := func(temp bool) []byte {
		path := filepath.Join(t.TempDir(), "m.musq")
		n, err := Create(path)
		if err != nil {
			t.Fatal(err)
		}
		stmts := []string{`CREATE TABLE m(a)`, `INSERT INTO m VALUES(1)`}
		if temp {
			stmts = append(stmts,
				`CREATE TEMP TABLE tt(z)`,
				`INSERT INTO tt VALUES('x')`,
				`CREATE TEMP TABLE gone(y)`,
				`DROP TABLE gone`)
		}
		for _, s := range stmts {
			if err := n.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
		var all []byte
		for _, p := range []string{path, segDeltaPath(path)} {
			b, err := os.ReadFile(p)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			all = append(all, b...)
		}
		return all
	}
	if plain, withTemp := build(false), build(true); !bytes.Equal(plain, withTemp) {
		t.Fatalf("main's files differ when temp objects exist: %d bytes vs %d", len(plain), len(withTemp))
	}
}

// A driver may run a connection's statements on more than one session, so the
// TEMP database is handed between them: what one session COMMITTED is what the
// next one installs (TempDatabase / SetTempDatabase).
func TestTempObjectsSurviveTheSessionThatCreatedThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`CREATE TABLE m(a)`, `CREATE TEMP TABLE tt(z)`, `INSERT INTO tt VALUES(7)`} {
		if err := n.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	temp := n.TempDatabase()
	if temp == nil {
		t.Fatal("the committed session published no temp database")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	n2, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Discard()
	n2.SetTempDatabase(temp)
	tt := n2.findTableMetaIn(scopeTemp, "tt")
	if tt == nil || !tt.isTemp {
		t.Fatal("temp table tt did not come back as a temp table")
	}
	cols, rows, err := n2.Query(`SELECT z FROM tt`, nil)
	if err != nil {
		t.Fatalf("SELECT z FROM tt: %v", err)
	}
	if len(cols) != 1 || len(rows) != 1 || rows[0][0].I != 7 {
		t.Fatalf("SELECT z FROM tt: cols=%v rows=%v", cols, rows)
	}
}

// A statement whose commit fails must not hand its temp changes to anyone: the
// handle is published only by a successful commit.
func TestTempDatabaseNotPublishedBeforeCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	if err := n.Exec(`CREATE TEMP TABLE tt(z)`); err != nil {
		t.Fatal(err)
	}
	if h := n.TempDatabase(); h != nil && len(h.tables) != 0 {
		t.Fatalf("an open transaction's temp table was published: %d tables", len(h.tables))
	}
}

// TestTempTableIsNotHeldInMemory: a TEMP table's committed rows are in its file,
// read on demand like main's -- C keeps a large temp table on disk by default
// (SQLITE_TEMP_STORE=1), and so does this engine.
func TestTempTableIsNotHeldInMemory(t *testing.T) {
	skipUnlessMapped(t)
	if testing.Short() {
		t.Skip("builds a ~40MB temp table")
	}
	tempStoreDirForTest = t.TempDir()
	defer func() { tempStoreDirForTest = "" }()
	n, err := Create(filepath.Join(t.TempDir(), "m.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	heap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	baseline := heap()
	if err := n.Exec(`CREATE TEMP TABLE big(id INTEGER PRIMARY KEY, p TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := n.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 200000)
		INSERT INTO big SELECT i, printf('%0180d', i) FROM c`); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	data := uint64(200000 * 196)
	// RESIDENCY, not growth: once committed, the rows are the file's, and what
	// the session holds is back near where it started.
	if held := heap(); held > baseline && held-baseline > data/10 {
		t.Fatalf("after its commit the temp table still holds %d of its %d bytes in memory", held-baseline, data)
	}
	before := heap()
	_, rows, err := n.Query(`SELECT count(*), sum(length(p)), max(id) FROM big WHERE id % 2 = 0`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].I != 100000 || rows[0][1].I != 100000*180 || rows[0][2].I != 200000 {
		t.Fatalf("temp scan answered %v", rows[0])
	}
	after := heap()
	if after > before && after-before > data/10 {
		t.Fatalf("scanning the temp table grew the heap by %d of its %d bytes: it is held in memory", after-before, data)
	}
	// ...and the committed state is the file's: a write after it, rolled back,
	// leaves exactly the committed rows.
	for _, q := range []string{`BEGIN`, `DELETE FROM big WHERE id > 100`, `ROLLBACK`} {
		if err := n.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, r, err := n.Query(`SELECT count(*) FROM big`, nil); err != nil || r[0][0].I != 200000 {
		t.Fatalf("after a rolled-back delete: %v %v", r, err)
	}
}

// TestTempFts4auxReachesMain: a TEMP fts4aux table over a MAIN fts4 table
// (amatch1.test's "CREATE VIRTUAL TABLE temp.t1aux USING fts4aux(main, t1)").
// The temp pager must know it is temp; read as main, it looked for t1 in the
// temp database and answered "no such table".
func TestTempFts4auxReachesMain(t *testing.T) {
	tempStoreDirForTest = t.TempDir()
	defer func() { tempStoreDirForTest = "" }()
	n, err := Create(filepath.Join(t.TempDir(), "x.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for _, q := range []string{
		`CREATE VIRTUAL TABLE t1 USING fts4(words)`,
		`INSERT INTO t1 VALUES('apple banana'), ('cherry')`,
		`CREATE VIRTUAL TABLE temp.t1aux USING fts4aux(main, t1)`,
	} {
		if err := n.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_, rows, err := n.Query(`SELECT term FROM t1aux WHERE col=0 ORDER BY 1`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, string(r[0].S))
	}
	if fmt.Sprint(got) != "[apple banana cherry]" {
		t.Fatalf("terms = %v", got)
	}
}

// TestTempCatalogStoresTextWithoutTemp: C writes a temp object's CREATE text into
// sqlite_temp_schema with the TEMP keyword taken out (alterqf.test, fkey2.test).
func TestTempCatalogStoresTextWithoutTemp(t *testing.T) {
	tempStoreDirForTest = t.TempDir()
	defer func() { tempStoreDirForTest = "" }()
	n, err := Create(filepath.Join(t.TempDir(), "x.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for _, q := range []string{`CREATE TEMP TABLE t1(a PRIMARY KEY)`, `CREATE TEMPORARY TRIGGER tr AFTER INSERT ON t1 BEGIN SELECT 1; END`} {
		if err := n.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_, rows, err := n.Query(`SELECT sql FROM sqlite_temp_schema WHERE sql IS NOT NULL ORDER BY name`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, string(r[0].S))
	}
	want := "[CREATE TABLE t1(a PRIMARY KEY) CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN SELECT 1; END]"
	if fmt.Sprint(got) != want {
		t.Fatalf("sqlite_temp_schema.sql = %q\nwant %s", got, want)
	}
}
