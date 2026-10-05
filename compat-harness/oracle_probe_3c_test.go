// Oracle probes A and B: corruption handling via hand-patched files.
// Tests that C SQLite accepts or refuses specific b-tree corruption shapes.
package compat

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Returns file byte offset of rowid's cell's overflow pointer on root leaf.
func probeCellOverflowPtrOffset(t *testing.T, path string, pageSize int, rootPgno int, rowid int64) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("probeCellOverflowPtrOffset: ReadFile: %v", err)
	}
	pageOff := (rootPgno - 1) * pageSize
	page := data[pageOff : pageOff+pageSize]
	hdrOff := 0
	if rootPgno == 1 {
		hdrOff = 100 // the 100-byte file header precedes page 1's own b-tree header
	}
	if page[hdrOff] != 0x0d {
		t.Fatalf("probeCellOverflowPtrOffset: page %d is not a table leaf (type 0x%02x)", rootPgno, page[hdrOff])
	}
	numCells := int(binary.BigEndian.Uint16(page[hdrOff+3 : hdrOff+5]))
	for i := 0; i < numCells; i++ {
		ptrOff := hdrOff + 8 + 2*i
		cellOff := int(binary.BigEndian.Uint16(page[ptrOff : ptrOff+2]))
		b := page[cellOff:]
		payloadLen, n1 := getVarintProbe(b)
		b = b[n1:]
		rid, n2 := getVarintProbe(b)
		if int64(rid) != rowid {
			continue
		}
		usable := pageSize // no reserved bytes in a plain cgo-created file
		local := localPayloadSizeProbe(payloadLen, usable)
		if local == payloadLen {
			t.Fatalf("probeCellOverflowPtrOffset: rowid %d's cell does not use overflow (payloadLen=%d fits locally)", rowid, payloadLen)
		}
		return int64(pageOff + cellOff + n1 + n2 + int(local))
	}
	t.Fatalf("probeCellOverflowPtrOffset: rowid %d not found on page %d", rowid, rootPgno)
	return 0
}

// Decodes a SQLite varint independently of musql's parser.
func getVarintProbe(b []byte) (v uint64, n int) {
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			v = (v << 8) | uint64(b[i])
			return v, 9
		}
		v = (v << 7) | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v, len(b)
}

// Computes local/overflow split size independently.
func localPayloadSizeProbe(payloadLen uint64, usable int) uint64 {
	maxLocal := uint64(usable - 35)
	if payloadLen <= maxLocal {
		return payloadLen
	}
	minLocal := (uint64(usable)-12)*32/255 - 23
	k := minLocal + (payloadLen-minLocal)%(uint64(usable)-4)
	if k <= maxLocal {
		return k
	}
	return minLocal
}

// probePragma runs a single PRAGMA/query against db and returns its first
// row's first column as a string (or "" if no rows) -- probes only ever
// read one scalar at a time.
func probePragma(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("probePragma(%q): %v", q, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ""
	}
	var v sql.NullString
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("probePragma(%q): scan: %v", q, err)
	}
	return v.String
}

// TestOracleProbeA is musql-stage3c-freelist-push-design.md section 6.2:
// does C SQLite silently free an overflow page that a SECOND,
// uncorrupted-otherwise cell still (corruptly) references, or does
// clearCellOverflow's own refcount check (btree.c:7020-7033) refuse it --
// and does the answer depend on whether that page happens to already be in
// the pager cache?
//
// Findings (recorded here per RULE ZERO, not assumed): see the two
// sub-tests' own t.Logf output for the exact page/freelist/integrity_check
// numbers observed. Both runs share the identical corrupted file (a fresh
// copy is made before each DELETE) -- only whether row 2's overflow page is
// pulled into cache FIRST differs.
func TestOracleProbeA(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.db")
	const pageSize = 4096

	db, err := sql.Open("sqlite3", base)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`PRAGMA page_size=%d`, pageSize),
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b BLOB)`,
		`INSERT INTO t VALUES(1, zeroblob(5000))`, // > maxLocal (usable-35=4061 at this page size): genuinely needs overflow, with a SMALL local portion, so both cells still fit on one leaf
		`INSERT INTO t VALUES(2, zeroblob(5000))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var root int
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 't'`).Scan(&root); err != nil {
		t.Fatalf("rootpage: %v", err)
	}
	db.Close()

	// Recover both rows' own overflow page numbers straight out of the
	// file, then hand-patch row 2's trailing pointer to name row 1's own
	// overflow page instead of its real second page.
	ptrOff1 := probeCellOverflowPtrOffset(t, base, pageSize, root, 1)
	ptrOff2 := probeCellOverflowPtrOffset(t, base, pageSize, root, 2)
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	ovfl1 := binary.BigEndian.Uint32(data[ptrOff1 : ptrOff1+4])
	ovfl2 := binary.BigEndian.Uint32(data[ptrOff2 : ptrOff2+4])
	t.Logf("row1 overflow page=%d, row2 overflow page=%d", ovfl1, ovfl2)

	patch := func(path string) {
		f, err := os.OpenFile(path, os.O_WRONLY, 0644)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, ovfl1)
		if _, err := f.WriteAt(buf, ptrOff2); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	}

	run := func(name string, touchRow2First bool) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".db")
			raw, err := os.ReadFile(base)
			if err != nil {
				t.Fatalf("ReadFile(base): %v", err)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			patch(path)

			db, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close()
			// database/sql pools connections; without pinning to exactly
			// ONE, the SELECT below and the DELETE that follows could run
			// on two DIFFERENT underlying sqlite3 handles with separate
			// pager caches each, silently defeating the whole point of
			// "pre-touch, then check whether it was still cached" --
			// exactly the gotcha this probe exists to control for.
			db.SetMaxOpenConns(1)
			if touchRow2First {
				var n int
				if err := db.QueryRow(`SELECT length(b) FROM t WHERE a = 2`).Scan(&n); err != nil {
					t.Fatalf("SELECT row2: %v", err)
				}
				t.Logf("pre-touched row2's blob length = %d", n)
			}

			before := probePragma(t, db, `PRAGMA freelist_count`)
			_, delErr := db.Exec(`DELETE FROM t WHERE a = 1`)
			after := probePragma(t, db, `PRAGMA freelist_count`)
			check := probePragma(t, db, `PRAGMA integrity_check`)
			t.Logf("touchRow2First=%v: DELETE row1 err=%v, freelist_count %s -> %s, integrity_check=%q",
				touchRow2First, delErr, before, after, check)
		})
	}

	run("untouched", false)
	run("precached", true)

	// A THIRD variant, added after the first two both showed IDENTICAL
	// "no protection" behavior (see this test's own findings comment at
	// the top of the file): a plain, already-COMPLETED SELECT releases its
	// page reference when the statement finishes, so btreePageLookup's
	// cache hit alone never finds refcount!=1 -- sqlite3PagerLookup
	// (pager.c:5805-5814) fetches from cache regardless of whether
	// anything else currently holds it, and ITS OWN fetch is the only
	// reference by the time clearCellOverflow's check runs. To exercise
	// the refcount mechanism for real, the reference must be held OPEN
	// concurrently with the DELETE: an UNDRAINED Rows cursor over row 2,
	// pinned to the same *sql.Conn the DELETE then runs on.
	t.Run("open-cursor", func(t *testing.T) {
		path := filepath.Join(dir, "open-cursor.db")
		raw, err := os.ReadFile(base)
		if err != nil {
			t.Fatalf("ReadFile(base): %v", err)
		}
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		patch(path)

		db, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		defer conn.Close()

		rows, err := conn.QueryContext(ctx, `SELECT b FROM t WHERE a = 2`)
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		if !rows.Next() {
			t.Fatalf("no row")
		}
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		t.Logf("held-open cursor's own row2 blob length = %d (NOT closing rows before the DELETE)", len(blob))
		// rows deliberately left open (no rows.Close()) -- its own
		// reference to page 3 (via the corrupted pointer) is still live
		// when the DELETE below runs on the SAME connection. Every read
		// from here on MUST go through this SAME conn (not db, whose own
		// pool -- capped to 1 -- would otherwise deadlock waiting for the
		// connection this cursor is still holding).
		before := probePragmaConn(t, ctx, conn, `PRAGMA freelist_count`)
		_, delErr := conn.ExecContext(ctx, `DELETE FROM t WHERE a = 1`)
		rows.Close()
		after := probePragmaConn(t, ctx, conn, `PRAGMA freelist_count`)
		check := probePragmaConn(t, ctx, conn, `PRAGMA integrity_check`)
		t.Logf("open-cursor: DELETE row1 err=%v, freelist_count %s -> %s, integrity_check=%q", delErr, before, after, check)
	})
}

// probePragmaConn is probePragma's own *sql.Conn-pinned twin -- see
// TestOracleProbeA's own "open-cursor" sub-test for why using db (its own
// connection pool) instead of the SAME conn an earlier cursor is still
// holding would deadlock rather than exercise anything.
func probePragmaConn(t *testing.T, ctx context.Context, conn *sql.Conn, q string) string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("probePragmaConn(%q): %v", q, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ""
	}
	var v sql.NullString
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("probePragmaConn(%q): scan: %v", q, err)
	}
	return v.String
}

// TestOracleProbeB is section 6.3: does C SQLite catch a double-free
// (btree.c:6826's own doc comment says freePage2 ASSUMES a page is not
// already on the free-list, and never checks) via the refcount mechanism
// firing on the SECOND pass over the same page (the page is now in cache
// from the FIRST getOverflowPage call), or does it corrupt the freelist
// (a trunk/count implying the same page twice)?
//
// Fixture: ONE row whose payload needs exactly 2 overflow pages (a
// genuine 2-page chain, built by cgo itself), then the FIRST overflow
// page's own "next" field is hand-patched to point at ITSELF -- a 1-page
// self-cycle within what the cell's own payload length still claims is a
// 2-page chain, so clearCellOverflow's count-bounded loop visits the same
// physical page twice.
func TestOracleProbeB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probeb.db")
	const pageSize = 4096

	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`PRAGMA page_size=%d`, pageSize),
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b BLOB)`,
		`INSERT INTO t VALUES(1, zeroblob(12000))`, // needs >= 2 overflow pages at this page size
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var root int
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 't'`).Scan(&root); err != nil {
		t.Fatalf("rootpage: %v", err)
	}
	db.Close()

	ptrOff := probeCellOverflowPtrOffset(t, path, pageSize, root, 1)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	first := binary.BigEndian.Uint32(data[ptrOff : ptrOff+4])
	firstPageOff := int64(first-1) * int64(pageSize)
	realNext := binary.BigEndian.Uint32(data[firstPageOff : firstPageOff+4])
	t.Logf("chain: first overflow page=%d, real second page=%d", first, realNext)

	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, first) // self-cycle: page `first`'s own "next" now points at itself
	if _, err := f.WriteAt(buf, firstPageOff); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	f.Close()

	db2, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	before := probePragma(t, db2, `PRAGMA freelist_count`)
	_, delErr := db2.Exec(`DELETE FROM t WHERE a = 1`)
	after := probePragma(t, db2, `PRAGMA freelist_count`)
	check := probePragma(t, db2, `PRAGMA integrity_check`)
	t.Logf("DELETE row1 (self-cycled chain) err=%v, freelist_count %s -> %s, integrity_check=%q", delErr, before, after, check)
}

// TestOracleProbeSelfTrunkCollision is the round-2 companion to Oracle Probe
// A/B above: does C SQLite also write a self-referential freelist
// trunk/leaf when a LATER delete's overflow chain is hand-patched to name
// the file's CURRENT freelist trunk page -- a page that became the trunk
// from an EARLIER, already-committed DELETE in the SAME file -- and that
// colliding page is then freed a second time?
//
// Traced by hand against btree.c's freePage2 (:6836-6969) for this exact
// input: row 2's own overflow page T is looked up via a plain
// `btreeGetPage(pBt, iTrunk, &pTrunk, 0)` (no refcount check, unlike
// allocateBtreePage's own trunk fetch via btreeGetUnusedPage) once T is
// established as db.freelistTrunk's C-side equivalent (page 1 offset 32).
// iTrunk == iPage == T here, so pTrunk and the page being freed are the
// SAME physical MemPage: the leaf-append branch's own
// `put4byte(&pTrunk->aData[8+nLeaf*4], iPage)` writes T's own page number
// into T's own leaf array -- exactly musql's own
// TestFreePageIncrementalSelfTrunkCollisionAppendBranch construction
// (engine/freelist_push_test.go), reproduced here against the real C
// engine rather than musql's own port, per RULE ZERO. This is what
// freelist_push.go's own iTrunk==pgno guard comment cites as confirming
// C SQLite has the identical blind spot -- and, per that same comment,
// why parity with C is NOT by itself a reason to skip the guard: this
// codebase's own precedent (freelist_pop.go's round-1 `iPage == iTrunk`
// check) already treats a trunk colliding with itself as a distinct
// structural fact to police regardless of what C does.
func TestOracleProbeSelfTrunkCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selftrunk.db")
	const pageSize = 4096

	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`PRAGMA page_size=%d`, pageSize),
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b BLOB)`,
		`INSERT INTO t VALUES(1, zeroblob(5000))`, // > maxLocal at this page size: genuinely needs a 1-page overflow chain
		`INSERT INTO t VALUES(2, zeroblob(5000))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var root int
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 't'`).Scan(&root); err != nil {
		t.Fatalf("rootpage: %v", err)
	}

	// Step 1: delete row1 in ITS OWN transaction/close, creating the file's
	// first (single-page, zero-leaf) freelist trunk T = row1's own former
	// overflow page.
	if _, err := db.Exec(`DELETE FROM t WHERE a = 1`); err != nil {
		t.Fatalf("delete 1: %v", err)
	}
	freelistAfterFirstDelete := probePragma(t, db, `PRAGMA freelist_count`)
	t.Logf("after deleting row1: freelist_count=%s", freelistAfterFirstDelete)
	db.Close()

	// Recover T (the freelist trunk page number) directly from the file
	// header (offset 32, big-endian, in page 1, after the 100-byte file
	// header prologue) -- the same field db.freelistTrunk mirrors.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	trunkT := binary.BigEndian.Uint32(data[32:36])
	t.Logf("freelist trunk T=%d", trunkT)
	if trunkT == 0 {
		t.Fatalf("setup: expected a nonzero freelist trunk after deleting row1")
	}

	// Step 2: hand-patch row2's own trailing overflow pointer to name T
	// instead of its real (distinct) overflow page -- only the pointer,
	// never the pointed-to content, so the payload-length-driven decode
	// that determines how many pages to walk is unaffected by the patch
	// (the same technique Oracle Probe A/B above already use).
	ptrOff2 := probeCellOverflowPtrOffset(t, path, pageSize, root, 2)
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, trunkT)
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteAt(buf, ptrOff2); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	f.Close()

	// Step 3: reopen (a fresh connection/pager cache -- T has never been
	// touched by this handle) and delete row2, which frees T: the SAME page
	// number as the file's CURRENT freelist trunk.
	db2, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	before := probePragma(t, db2, `PRAGMA freelist_count`)
	_, delErr := db2.Exec(`DELETE FROM t WHERE a = 2`)
	after := probePragma(t, db2, `PRAGMA freelist_count`)
	check := probePragma(t, db2, `PRAGMA integrity_check`)
	t.Logf("DELETE row2 (self-trunk-collision, T=%d) err=%v, freelist_count %s -> %s, integrity_check=%q",
		trunkT, delErr, before, after, check)
}
