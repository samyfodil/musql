// TestOracleProbeStage3dD2 tests PRAGMA integrity_check against a specific
// corruption shape: a non-root interior page with zero cells.
package compat

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// probeD2Rows/probeD2PageSize: empirically verified to force a genuine
// THREE-level table tree under real cgo SQLite -- root (page 2, interior)
// with exactly two children, both themselves interior pages, each holding
// only leaves -- the identical shape, at the identical row count,
// engine/integrity_walk_n2_test.go independently verified for musql's own
// writer. Deterministic: ascending rowids, one INSERT per row inside a
// single transaction.
const (
	probeD2Rows     = 3000
	probeD2PageSize = 512
)

// probeD2PageHeader is a standalone (no musql engine dependency, matching
// this file's sibling oracle_probe_*_test.go convention) parse of just the
// b-tree page header fields Probe D2 needs: type byte, numCells (offset
// 3:5), and -- for an interior page -- every child pointer (each interior
// cell's own leading 4 bytes, plus the rightmost field at offset 8:12).
type probeD2PageHeader struct {
	pageType  byte
	numCells  int
	rightmost uint32
	children  []uint32 // cell 0..numCells-1's own child pointers, in order
}

func probeD2ParsePage(data []byte, pgno, pageSize int) probeD2PageHeader {
	pageOff := (pgno - 1) * pageSize
	page := data[pageOff : pageOff+pageSize]
	hdrOff := 0
	if pgno == 1 {
		hdrOff = 100
	}
	h := probeD2PageHeader{
		pageType: page[hdrOff],
		numCells: int(binary.BigEndian.Uint16(page[hdrOff+3 : hdrOff+5])),
	}
	if h.pageType != 0x05 { // table interior
		return h
	}
	h.rightmost = binary.BigEndian.Uint32(page[hdrOff+8 : hdrOff+12])
	for i := 0; i < h.numCells; i++ {
		ptrOff := hdrOff + 12 + 2*i
		cellOff := int(binary.BigEndian.Uint16(page[ptrOff : ptrOff+2]))
		h.children = append(h.children, binary.BigEndian.Uint32(page[cellOff:cellOff+4]))
	}
	return h
}

// probeD2Loose runs q and returns every row's first column joined by "\n",
// or the query-open error's own text if q fails before producing any rows --
// unlike probePragma (this file's sibling, oracle_probe_3c_test.go), which
// t.Fatalf's on a query error, this tolerates SQLITE_CORRUPT surfacing at
// EITHER db.Query or during rows.Next()/rows.Err(), since which one it is is
// exactly what this probe is trying to observe.
func probeD2Loose(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return err.Error()
		}
		lines = append(lines, v.String)
	}
	if err := rows.Err(); err != nil {
		return err.Error()
	}
	return strings.Join(lines, "\n")
}

func TestOracleProbeStage3dD2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe_d2.db")

	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA page_size=%d`, probeD2PageSize)); err != nil {
		t.Fatalf("PRAGMA page_size: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < probeD2Rows; i++ {
		if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d, 'row-%d')`, i, i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var root int
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name='t'`).Scan(&root); err != nil {
		t.Fatalf("rootpage: %v", err)
	}
	// Sanity check the file is genuinely clean BEFORE any patch -- if this
	// ever fails, the probe's own premise (a real, uncorrupted cgo file) is
	// broken and everything below is meaningless.
	if check := probePragma(t, db, `PRAGMA integrity_check`); check != "ok" {
		t.Fatalf("setup: fixture is not clean before patching: integrity_check=%q", check)
	}
	db.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	rootHdr := probeD2ParsePage(data, root, probeD2PageSize)
	if rootHdr.pageType != 0x05 {
		t.Fatalf("setup: table root (page %d) is not interior (type 0x%02x); did probeD2Rows regress below the 3-level threshold?", root, rootHdr.pageType)
	}
	var victim uint32
	for _, child := range append(append([]uint32{}, rootHdr.children...), rootHdr.rightmost) {
		if probeD2ParsePage(data, int(child), probeD2PageSize).pageType == 0x05 {
			victim = child
			break
		}
	}
	if victim == 0 {
		t.Fatalf("setup: no genuine non-root interior page found under root %d; did probeD2Rows regress below the 3-level threshold?", root)
	}
	victimHdr := probeD2ParsePage(data, int(victim), probeD2PageSize)
	t.Logf("root=%d, victim=%d (numCells=%d, rightmost=%d, children=%v)",
		root, victim, victimHdr.numCells, victimHdr.rightmost, victimHdr.children)
	if len(victimHdr.children) == 0 {
		t.Fatalf("setup: victim page %d has no cells to begin with", victim)
	}

	// The patch has TWO parts. Both are needed to isolate checkTreePage's
	// OWN blind spot from an unrelated one (a plain orphan-page finding from
	// the coverage check) -- neither part is what makes PRAGMA
	// integrity_check fail overall; see this file's own top-of-file finding
	// for what actually does (pragma.c's independent row-scan).
	//
	// (1) victim's own header, rewritten to look like a genuinely fresh,
	// empty interior page -- numCells=0 (offset 3:5), first freeblock=0
	// (offset 1:3), cell-content-area-start=pageSize (offset 5:7, i.e. "no
	// content stored"), fragmented-free-bytes=0 (offset 7) -- matching
	// checkTreePage's own coverage-check arithmetic for a 0-cell page
	// (btree.c:11031-11107: an empty heap plus contentOffset==usableSize
	// implies nFrag==0, which must equal the stored byte or "Fragmentation
	// of N bytes reported as M" fires -- verified by hitting exactly that
	// finding when ONLY numCells was patched, in this test's own first
	// draft). victim's own rightmost pointer (offset 8:12) is left
	// completely untouched, so it still names a real, valid leaf subtree --
	// the "1-child non-root interior page" shape btree.c:5472/:6251 reject
	// on every descent.
	//
	// (2) victim's own FORMER cell children (victimHdr.children) are moved
	// onto the file's freelist chain -- reusing the FIRST one as a trunk
	// page (SQLite's own freePage2 does the identical reuse-in-place,
	// btree.c:6836-6969) holding the rest as leaves -- so the coverage
	// check ("Make sure every page in the file is referenced",
	// btree.c:11255) does not ALSO flag them as orphans.
	trunk := victimHdr.children[0]
	leaves := victimHdr.children[1:]
	trunkBuf := make([]byte, 8+4*len(leaves))
	binary.BigEndian.PutUint32(trunkBuf[0:4], 0) // next trunk: end of chain
	binary.BigEndian.PutUint32(trunkBuf[4:8], uint32(len(leaves)))
	for i, leaf := range leaves {
		binary.BigEndian.PutUint32(trunkBuf[8+4*i:12+4*i], leaf)
	}

	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	victimBase := int64((int(victim) - 1) * probeD2PageSize)
	writeField := func(off int64, b []byte) {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatalf("WriteAt(%d): %v", off, err)
		}
	}
	var contentStart [2]byte
	binary.BigEndian.PutUint16(contentStart[:], uint16(probeD2PageSize))
	writeField(victimBase+1, []byte{0, 0})                      // first freeblock = 0
	writeField(victimBase+3, []byte{0, 0})                      // numCells = 0
	writeField(victimBase+5, contentStart[:])                   // content start = pageSize
	writeField(victimBase+7, []byte{0})                         // fragmented free bytes = 0
	writeField(int64((int(trunk)-1)*probeD2PageSize), trunkBuf) // trunk's own content
	var hdrBuf [4]byte
	binary.BigEndian.PutUint32(hdrBuf[:], trunk)
	writeField(32, hdrBuf[:]) // file header offset 32: FreelistTrunk
	binary.BigEndian.PutUint32(hdrBuf[:], uint32(1+len(leaves)))
	writeField(36, hdrBuf[:]) // file header offset 36: FreelistPages
	f.Close()

	db2, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	// (1) PRAGMA integrity_check does NOT report "ok" -- contra the design
	// doc's own original hypothesis (see this file's top-of-file finding).
	// It catches the corruption, but via its own row-scan, not checkTreePage.
	check := probeD2Loose(t, db2, `PRAGMA integrity_check`)
	t.Logf("PRAGMA integrity_check -> %q", check)
	if !strings.Contains(check, "malformed") {
		t.Fatalf(`expected PRAGMA integrity_check to report the corruption via its own row-scan (pragma.c:1845-1856's OP_Rewind/OP_Next, independent of checkTreePage), got %q`, check)
	}

	// (2) Same result for quick_check and the single-object partial form --
	// both share the identical unconditional row-scan (pragma.c's own
	// isQuick flag only gates the WITHOUT-ROWID key-order sub-check, not the
	// scan itself; tableSkipIntegrityCheck only skips OTHER tables, never
	// the one actually named).
	if got := probeD2Loose(t, db2, `PRAGMA quick_check`); !strings.Contains(got, "malformed") {
		t.Fatalf(`expected PRAGMA quick_check to also report the corruption, got %q`, got)
	}
	if got := probeD2Loose(t, db2, `PRAGMA integrity_check(t)`); !strings.Contains(got, "malformed") {
		t.Fatalf(`expected PRAGMA integrity_check(t) (the single-object/partial form, still scanning t's own rows) to also report the corruption, got %q`, got)
	}

	// (3) A plain SELECT through the corrupted subtree fails identically --
	// the SAME underlying moveToChild nCell<1 check (btree.c:5472/:6251),
	// reached this time via the ordinary query path rather than PRAGMA's own
	// internal one.
	selectErr := probeD2Loose(t, db2, `SELECT a, b FROM t`)
	t.Logf("SELECT a, b FROM t -> %q", selectErr)
	if !strings.Contains(selectErr, "malformed") {
		t.Fatalf(`expected a "database disk image is malformed" error from the SELECT, got %q`, selectErr)
	}
}
