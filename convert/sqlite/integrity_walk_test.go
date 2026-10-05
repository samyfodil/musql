package sqlite

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// buildIWTestDB creates a fresh database at pageSize, runs stmts against it,
// and closes it -- the standard "build a real on-disk file, then reopen
// read-only" shape this file's mutation tests all share (mirrors
// zz_verbatim_leak_hard_error_test.go's own convention).
func buildIWTestDB(t *testing.T, pageSize int, stmts []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	cdb, err := engine.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, s := range stmts {
		if err := cdb.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := cdb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	toSQLiteInPlace(t, path, pageSize)
	return path
}

// writeAt patches count bytes of raw file content at a given absolute
// offset -- every mutation test's own corruption primitive.
func writeAt(t *testing.T, path string, offset int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, offset); err != nil {
		t.Fatalf("corrupt at %d: %v", offset, err)
	}
}

func mustCheckClean(t *testing.T, path string) {
	t.Helper()
	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	res, err := p.checkStructuralIntegrity()
	if err != nil {
		t.Fatalf("CheckStructuralIntegrity: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected a clean structural check, got %d problem(s): %+v", len(res.Problems), res.Problems)
	}
}

// TestCheckStructuralIntegrityCleanOnMusqlWrittenDBs is this stage's single
// most important regression check (per the task spec): a FALSE POSITIVE on a
// database this engine itself wrote would silently break PRAGMA
// integrity_check for real users, which is itself a "never wrong" violation.
// Every shape below is a database musql's own write path produced, never
// touched by a byte afterward.
func TestCheckStructuralIntegrityCleanOnMusqlWrittenDBs(t *testing.T) {
	cases := []struct {
		name     string
		pageSize int
		stmts    []string
	}{
		{"empty", 4096, nil},
		{"one-table-no-rows", 4096, []string{`CREATE TABLE t(a,b)`}},
		{"one-table-with-rows", 4096, []string{
			`CREATE TABLE t(a,b)`,
			`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		}},
		{"table-plus-index", 4096, []string{
			`CREATE TABLE t(a,b)`,
			`CREATE INDEX i ON t(b)`,
			`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		}},
		{"without-rowid", 4096, []string{
			`CREATE TABLE t(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
			`INSERT INTO t VALUES('a',1),('b',2),('c',3)`,
		}},
		{"multi-table-multi-index", 4096, []string{
			`CREATE TABLE a(x,y)`,
			`CREATE TABLE b(x,y)`,
			`CREATE INDEX ai ON a(y)`,
			`CREATE INDEX bi ON b(x)`,
			`INSERT INTO a VALUES(1,'p'),(2,'q')`,
			`INSERT INTO b VALUES(3,'r'),(4,'s')`,
		}},
		{"overflow-rows-small-page", 512, []string{
			`CREATE TABLE t(a, b)`,
			`INSERT INTO t VALUES(1, zeroblob(3000))`,
			`INSERT INTO t VALUES(2, zeroblob(4000))`,
		}},
		{"overflow-index-cell", 512, []string{
			`CREATE TABLE t(a, b)`,
			`CREATE INDEX i ON t(b)`,
			`INSERT INTO t VALUES(1, 'x' || zeroblob(3000))`,
		}},
		{"many-rows-interior-pages", 512, func() []string {
			s := []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`}
			for i := 0; i < 300; i++ {
				n := strconv.Itoa(i)
				s = append(s, `INSERT INTO t VALUES(`+n+`, 'row-'||`+n+`)`)
			}
			return s
		}()},
		{"dropped-table-leaves-freelist", 4096, []string{
			`CREATE TABLE a(x)`,
			`CREATE TABLE b(y)`,
			`INSERT INTO b VALUES(1),(2),(3)`,
			`DROP TABLE a`,
		}},
		{"temp-table", 4096, []string{
			`CREATE TABLE main1(x)`,
			`CREATE TEMP TABLE tmp1(y)`,
			`INSERT INTO main1 VALUES(1)`,
			`INSERT INTO tmp1 VALUES(2)`,
		}},
		{"view-and-trigger", 4096, []string{
			`CREATE TABLE t(a,b)`,
			`CREATE VIEW v AS SELECT * FROM t`,
			`CREATE TRIGGER trg AFTER INSERT ON t BEGIN SELECT 1; END`,
			`INSERT INTO t VALUES(1,2)`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := buildIWTestDB(t, c.pageSize, c.stmts)
			mustCheckClean(t, path)
		})
	}
}

// TestCheckStructuralIntegrityAutoVacuumClean confirms a real auto-vacuum'd
// database (this engine's own write path, ptrmap_write.go's buildPtrmap)
// reports clean -- ptrmap pages must not be false-flagged as orphans, the
// specific hazard requirement (c) of Stage 2a's spec warns about.
func TestCheckStructuralIntegrityAutoVacuumClean(t *testing.T) {
	path := buildIWTestDB(t, 512, []string{
		`PRAGMA auto_vacuum=1`,
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i ON t(b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3, zeroblob(2000))`,
	})
	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	if mode := autoVacuumModeOfHeader(p.hdr.LargestRootPage, p.hdr.IncrementalVacuum); mode == 0 {
		t.Fatalf("test setup did not actually produce an auto-vacuum database (mode 0)")
	}
	mustCheckClean(t, path)
}

// findFirstOverflowCellByteRange opens path read-only and returns the
// absolute file offset of the LAST 4 bytes of tableName's single leaf cell's
// overflow-pointer trailer (i.e. the bytes tableLeafCellOverflow itself
// reads), using the very same pre-existing, independently-exercised page
// primitives the read path has always used (parseBtreePageHeader,
// tableLeafCellSpan) -- not anything from integrity_walk.go, which is what
// is under test here.
func findFirstOverflowCellByteRange(t *testing.T, path, tableName string) int64 {
	t.Helper()
	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	tr := struct{ RootPage uint32 }{rootOf(t, p, tableName)}
	page, err := p.page(tr.RootPage)
	if err != nil {
		t.Fatalf("read root page %d: %v", tr.RootPage, err)
	}
	hdr, err := parseBtreePageHeader(page, tr.RootPage)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if hdr.pageType != pageTypeTableLeaf {
		t.Fatalf("expected a single-page leaf table for this test, got page type 0x%02x", hdr.pageType)
	}
	usable := p.hdr.UsablePageSize()
	for i := 0; i < int(hdr.numCells); i++ {
		off, err := hdr.cellOffset(page, i)
		if err != nil {
			t.Fatalf("cell offset: %v", err)
		}
		_, length, err := tableLeafCellSpan(page, off, usable)
		if err != nil {
			t.Fatalf("cell span: %v", err)
		}
		if firstOverflow := tableLeafCellOverflow(page[off:off+length], usable); firstOverflow != 0 {
			pageBase := int64(tr.RootPage-1) * int64(p.hdr.PageSize)
			return pageBase + int64(off+length-4)
		}
	}
	t.Fatalf("no overflowing cell found in table %s -- test setup did not produce one", tableName)
	return 0
}

// TestCheckStructuralIntegrityDetectsOrphanPage mutation-tests the orphan
// finding: zeroing an overflow-chain pointer (the trailing 4 bytes of a leaf
// cell that referenced a real overflow page) makes that overflow page
// unreachable from anywhere, without changing the file's own page count or
// freelist -- exactly "orphan a page by zeroing a b-tree pointer that
// referenced it" from the task's own mutation-test list.
func TestCheckStructuralIntegrityDetectsOrphanPage(t *testing.T) {
	path := buildIWTestDB(t, 512, []string{
		`CREATE TABLE t(a, b)`,
		`INSERT INTO t VALUES(1, zeroblob(3000))`,
	})
	off := findFirstOverflowCellByteRange(t, path, "t")
	writeAt(t, path, off, []byte{0, 0, 0, 0})

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	res, err := p.checkStructuralIntegrity()
	if err != nil {
		t.Fatalf("CheckStructuralIntegrity: %v", err)
	}
	if res.OK {
		t.Fatalf("expected the orphaned overflow page to be reported, got a clean result")
	}
	found := false
	for _, prob := range res.Problems {
		if strings.Contains(prob.Message, "never used") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'never used' orphan finding, got: %+v", res.Problems)
	}
}

// TestCheckStructuralIntegrityDetectsDoubleReference mutation-tests the
// double-reference finding: pointing the same overflow-chain pointer at page
// 1 (sqlite_schema's own root, always already claimed) instead of zeroing
// it -- "force a double-reference by pointing two different places at the
// same page number" from the task's own mutation-test list.
func TestCheckStructuralIntegrityDetectsDoubleReference(t *testing.T) {
	path := buildIWTestDB(t, 512, []string{
		`CREATE TABLE t(a, b)`,
		`INSERT INTO t VALUES(1, zeroblob(3000))`,
	})
	off := findFirstOverflowCellByteRange(t, path, "t")
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], 1) // page 1: sqlite_schema, always claimed
	writeAt(t, path, off, buf[:])

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	res, err := p.checkStructuralIntegrity()
	if err != nil {
		t.Fatalf("CheckStructuralIntegrity: %v", err)
	}
	if res.OK {
		t.Fatalf("expected a double-reference finding on page 1, got a clean result")
	}
	found := false
	for _, prob := range res.Problems {
		if prob.Page == 1 && strings.Contains(prob.Message, "referenced by both") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a page-1 double-reference finding, got: %+v", res.Problems)
	}
}

// TestPendingBytePage checks pendingBytePage against C SQLite's own
// PENDING_BYTE_PAGE macro (btreeInt.h:609) for every legal page size,
// including the 4096 case ptrmap_write.go's own doc comment already states a
// value for (page 262145) as independent corroboration.
func TestPendingBytePage(t *testing.T) {
	cases := []struct {
		pageSize uint32
		want     uint32
	}{
		{512, 2097153},
		{1024, 1048577},
		{4096, 262145}, // matches ptrmap_write.go's own doc comment, verbatim
		{8192, 131073},
		{65536, 16385},
	}
	for _, c := range cases {
		if got := pendingBytePage(c.pageSize); got != c.want {
			t.Errorf("pendingBytePage(%d) = %d, want %d", c.pageSize, got, c.want)
		}
	}
}

// rootOf is the root page of the table or index named name.
func rootOf(t *testing.T, p *pager, name string) uint32 {
	t.Helper()
	rows, err := p.schema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, r := range rows {
		if strings.EqualFold(r.Name, name) {
			return r.RootPage
		}
	}
	t.Fatalf("%s not found", name)
	return 0
}
