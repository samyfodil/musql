// This file is the structural half of integrity_check over a C SQLite file: a
// page-reachability walk confirming every page in [1, page_count] belongs to
// exactly one of {a table/index b-tree, the freelist, a pointer-map page}, and
// that the freelist's walked length matches the header's count. It is the
// first part of Import's check of its source (import.go); the rest assumes
// every page parses.
//
// The walk reuses the reader's page and cell helpers (parseBtreePageHeader,
// tableLeafCellSpan, parseIdxPageHeader, idxCellSpan, ...) and records every
// page in one map shared by all roots, the freelist and the pointer map
// (integrityWalker.owner). So a page claimed twice -- a tree looping on itself,
// two trees colliding, a pointer into the freelist -- is one "double
// reference" finding. A malformed page is recorded and its subtree skipped, so
// it does not hide other findings. A page is claimed at most once, so the walk
// terminates whatever the pointers do.
package sqlite

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/samyfodil/musql/engine"
)

// IntegrityProblem is one structural finding: a page-reachability or
// page-format defect CheckStructuralIntegrity discovered. Page is 0 when the
// finding is not about one specific page (currently only the freelist
// page-count mismatch).
type IntegrityProblem struct {
	Page    uint32
	Message string
}

// StructuralIntegrityResult is CheckStructuralIntegrity's return value: OK is
// true iff Problems is empty. A structured result rather than a formatted
// string, so a test can assert on individual findings (which page, what kind
// of defect) instead of parsing PRAGMA row text.
type StructuralIntegrityResult struct {
	OK       bool
	Problems []IntegrityProblem
}

func (p *pager) checkStructuralIntegrity() (*StructuralIntegrityResult, error) {
	if p != nil && p.nPages == 0 {
		// A zero-length file is an empty database with nothing to walk -- no
		// page 1, no freelist, no pointer map. C SQLite answers "ok" for it
		// (verified: "PRAGMA integrity_check" on a freshly opened, never-written
		// file). See newPager.
		return &StructuralIntegrityResult{OK: true}, nil
	}
	rows, err := p.schema()
	if err != nil {
		return nil, err
	}
	roots := structuralRoots(rows)
	checkFreelist, checkCoverage := true, true

	w := &integrityWalker{
		pages:      p.page,
		pageCount:  p.PageCount(),
		usable:     p.hdr.UsablePageSize(),
		autoVacuum: autoVacuumModeOfHeader(p.hdr.LargestRootPage, p.hdr.IncrementalVacuum) != 0,
		owner:      make(map[uint32]string),
	}
	// Pointer-map pages are claimed FIRST, purely from page-number arithmetic
	// (isPtrmapPage), before any b-tree/freelist walk runs -- so a b-tree or
	// freelist pointer that lands on one (real corruption: a ptrmap page is
	// never legitimately part of either) is caught as an ordinary "referenced
	// by both pointer-map page and X" double-reference finding, the same
	// mechanism every other collision uses. Ptrmap CONTENT (whether each
	// entry names its true parent) is deliberately not validated: only "does a
	// ptrmap page exist where auto-vacuum says one must, and is it excluded
	// from the orphan/double-reference check" is.
	if w.autoVacuum {
		for pgno := uint32(2); pgno <= w.pageCount; pgno++ {
			if isPtrmapPage(pgno, w.usable) {
				w.claim(pgno, "pointer-map page")
			}
		}
	}
	// The pending-byte page is claimed too, as sqlite3BtreeIntegrityCheck
	// does unconditionally before its freelist check (btree.c:11203):
	//
	//	i = PENDING_BYTE_PAGE(pBt);
	//	if( i<=sCheck.nCkPage ) setPageReferenced(&sCheck, i);
	//
	// It only matters for files over about 1GiB (see pendingBytePage).
	if pb := pendingBytePage(p.hdr.PageSize); pb <= w.pageCount {
		w.claim(pb, "pending-byte page")
	}
	for _, r := range roots {
		if r.index {
			w.walkIndexTree(r.pgno, r.label, true)
		} else {
			// A fresh ordering cursor per root: rowids only have to ascend
			// WITHIN one table's b-tree, and two tables' rowid ranges are
			// unrelated.
			w.walkTableTree(r.pgno, r.label, true, &rowidOrder{})
		}
	}
	if checkFreelist {
		walked := w.walkFreelist(p.hdr.FreelistTrunk)
		if walked != p.hdr.FreelistPages {
			w.problem(0, fmt.Sprintf("freelist: header claims %d pages, the chain actually holds %d", p.hdr.FreelistPages, walked))
		}
	}
	if checkCoverage {
		for pgno := uint32(1); pgno <= w.pageCount; pgno++ {
			if _, ok := w.owner[pgno]; !ok {
				w.problem(pgno, fmt.Sprintf("page %d: never used (not reachable from any table/index b-tree, the freelist chain, or a pointer-map page)", pgno))
			}
		}
	}
	return &StructuralIntegrityResult{OK: len(w.problems) == 0, Problems: w.problems}, nil
}

// pendingByteOffset is the byte offset of SQLite's "pending byte" -- the lock
// byte multi-process readers/writers use for POSIX advisory locking (os.h:
// `#define PENDING_BYTE (0x40000000)`), 1GiB into the file. C SQLite's own
// sqlite3BtreeIntegrityCheck excludes the page holding it from the "page
// never used" coverage check, unconditionally (not gated on auto-vacuum) --
// see checkStructuralIntegrity's own citation, above, for where that runs.
const pendingByteOffset = 0x40000000

// pendingBytePage returns the 1-indexed page holding pendingByteOffset for a
// page size of pageSize (the full size, not usable), from btreeInt.h:609:
//
//	#define PENDING_BYTE_PAGE(pBt)  ((Pgno)((PENDING_BYTE/((pBt)->pageSize))+1))
//
// It is at least 16385 (for 65536-byte pages), so reachable only in files over
// about 1GiB -- which a C-written file can be.
func pendingBytePage(pageSize uint32) uint32 {
	return pendingByteOffset/pageSize + 1
}

// structuralRoot is one b-tree checkStructuralIntegrity must walk: a root
// page number, tagged with which on-disk page-format family it uses (index:
// true selects INDEX b-tree page types 0x02/0x0a -- both an ordinary
// secondary index and a WITHOUT ROWID table's own root, which is
// index-shaped, not table-shaped, per withoutrowid_read.go's own doc
// comment) and a human-readable label used only in diagnostic text.
type structuralRoot struct {
	pgno  uint32
	index bool
	label string
}

// structuralRoots is every b-tree root in the catalog, sqlite_schema's first:
// what sqlite3BtreeIntegrityCheck walks for the whole-database form
// (pragma.c:1762-1772).
func structuralRoots(rows []schemaRow) []structuralRoot {
	roots := []structuralRoot{{pgno: 1, label: "sqlite_schema"}}
	for _, r := range rows {
		if r.RootPage == 0 {
			continue // a view, or a virtual table with no b-tree of its own
		}
		switch r.Type {
		case "table":
			sh, err := engine.TableShapeOf(r.SQL)
			roots = append(roots, structuralRoot{pgno: r.RootPage, index: err == nil && sh.WithoutRowid, label: fmt.Sprintf("table %s", r.Name)})
		case "index":
			roots = append(roots, structuralRoot{pgno: r.RootPage, index: true, label: fmt.Sprintf("index %s", r.Name)})
		}
	}
	return roots
}

// integrityWalker holds the state one checkStructuralIntegrity call
// accumulates: pages reads a page's raw bytes by number (ordinarily
// pager.page, its non-copying/cached form -- ptrmap/freelist/b-tree
// pages are only ever READ here, never mutated, so sharing the pager's own
// cached buffer is safe, exactly as btree.go's tableScanner already relies
// on); owner is the "claimed by" map every visited page number is recorded
// into exactly once (see this file's package doc comment for why this is
// ONE global map rather than a per-tree local one).
type integrityWalker struct {
	pages      pageSource
	pageCount  uint32
	usable     uint32
	autoVacuum bool
	owner      map[uint32]string
	problems   []IntegrityProblem
}

// pageSource reads page pgno's (1-indexed) raw bytes -- the same shape
// *pager.page already has, so a method value (p.page) is a
// pageSource with no adapter needed.
type pageSource func(pgno uint32) ([]byte, error)

func (w *integrityWalker) problem(pgno uint32, msg string) {
	w.problems = append(w.problems, IntegrityProblem{Page: pgno, Message: msg})
}

// claim records pgno as reachable, owned by owner (a diagnostic label such
// as "table t1" or "freelist trunk"). It returns false -- and records a
// finding, never panics or aborts the whole check -- for an out-of-range
// page number or a page some earlier claim already owns (which covers both a
// cross-tree/cross-category collision and a single tree looping back on
// itself: either way, re-descending would either duplicate the same findings
// forever or recurse without bound, so the caller must stop there). Because
// every page number can be claimed at most once, no walk can visit more than
// pageCount pages in total, however its pointers are wired.
func (w *integrityWalker) claim(pgno uint32, owner string) bool {
	if pgno == 0 || pgno > w.pageCount {
		w.problem(pgno, fmt.Sprintf("%s: page number %d is out of range [1,%d]", owner, pgno, w.pageCount))
		return false
	}
	if prev, dup := w.owner[pgno]; dup {
		w.problem(pgno, fmt.Sprintf("page %d is referenced by both %s and %s", pgno, prev, owner))
		return false
	}
	w.owner[pgno] = owner
	return true
}

// zeroCellPageAllowed reports whether a b-tree page may have zero cells: only a
// leaf root (an empty table, moveToRoot's SQLITE_EMPTY, btree.c:5626-5628) or
// an interior root on page 1 (sqlite_schema's virtual root, btree.c:5622).
//
// C enforces this on descent (moveToChild's `pCur->pPage->nCell<1`,
// btree.c:5472; the seek path at 6251), not in checkTreePage
// (btree.c:10855-11118), which never compares nCell to 0. C's integrity_check
// still catches it because pragma.c also scans every table row by row
// (pragma.c:1845, 1856), hitting moveToChild. This walk ports checkTreePage
// and this reader has no such row scan (it would skip a 0-cell page), so it
// checks here.
func zeroCellPageAllowed(root, interior bool, pgno uint32) bool {
	if !root {
		return false
	}
	return !interior || pgno == 1
}

// noteChildDepth is N2 part (2): every child reached from the same parent
// page must return the same DEPTH -- ported from checkTreePage's own
// mismatch test (btree.c:11015-11020, "Child page depth differs"), which
// primes `depth` from the first child it recurses into (the right-child,
// btree.c:10950) and flags every later child whose own returned depth
// disagrees. have reports whether depth has already been primed by an
// earlier sibling; the first child to call this always "wins" (sets depth,
// no comparison), matching the C's own priming step -- which child goes
// first does not matter for what gets FOUND, only for which sibling's depth
// is reported as ground truth in the finding.
func (w *integrityWalker) noteChildDepth(pgno uint32, owner string, depth *int, have *bool, childDepth int) {
	if !*have {
		*have = true
		*depth = childDepth
		return
	}
	if childDepth != *depth {
		w.problem(pgno, fmt.Sprintf("%s: page %d: child page depth differs (%d vs %d)", owner, pgno, childDepth, *depth))
		*depth = childDepth
	}
}

// rowidOrder is the ascending-rowid cursor threaded through one table b-tree's
// in-order walk: the key sequence (interior separators between their subtrees)
// must be non-decreasing, repeating only where a subtree's largest key meets
// the separator bounding it.
//
// That is checkTreePage's rowid check (btree.c:10983-10990) run forwards. C
// walks the same sequence backwards, carrying maxKey down and each subtree's
// minimum up, and flags `keyCanBeEqual ? (info.nKey > maxKey) : (info.nKey >=
// maxKey)`, where keyCanBeEqual is 1 only for a page's first key
// (btree.c:10871, 10989) and forced to 0 on an interior page before its cells
// (10951, 11016) -- i.e. the one allowed equality is a subtree's largest key
// against its separator (allowEqual). This walks forwards because that is the
// order walkTableTree visits, so claim collisions are reported against the
// same page.
//
// The equality is allowed because a separator is a copy of a rowid, normally
// equal to its left subtree's largest; a delete can also leave it greater.
// A key going backwards is never valid: a rowid seek would answer wrongly.
type rowidOrder struct {
	prev       int64
	have       bool
	allowEqual bool
}

// step reports whether key continues the sequence, then advances the cursor
// past it. Like the C, it advances even on a violation (btree.c:10988 assigns
// maxKey unconditionally) so one out-of-order key is reported once rather
// than making every later key on the page look wrong too.
func (o *rowidOrder) step(key int64) bool {
	ok := !o.have || key > o.prev || (key == o.prev && o.allowEqual)
	o.prev, o.have, o.allowEqual = key, true, false
	return ok
}

// walkTableTree visits pgno's TABLE b-tree subtree (interior pages, every
// child pointer including the rightmost, every leaf cell, every overflow
// chain a cell spills into), claiming every page it finds. root is true only for
// the initial call from checkStructuralIntegrity (a schema-listed root
// page); every recursive call (a child cell or the rightmost pointer) passes
// false -- see zeroCellPageAllowed's own doc comment for why that
// distinction is load-bearing. ord is this tree's shared key-ordering cursor
// (one per root, see rowidOrder). The return value is this subtree's DEPTH (0
// for a leaf), consumed by noteChildDepth for N2 part (2).
func (w *integrityWalker) walkTableTree(pgno uint32, owner string, root bool, ord *rowidOrder) int {
	if !w.claim(pgno, owner) {
		return 0
	}
	page, err := w.pages(pgno)
	if err != nil {
		w.problem(pgno, fmt.Sprintf("%s: reading page %d: %v", owner, pgno, err))
		return 0
	}
	hdr, err := parseBtreePageHeader(page, pgno)
	if err != nil {
		w.problem(pgno, fmt.Sprintf("%s: %v", owner, err))
		return 0
	}
	if hdr.pageType != pageTypeTableLeaf && hdr.pageType != pageTypeTableInterior {
		w.problem(pgno, fmt.Sprintf("%s: page %d: expected a table b-tree page, got type 0x%02x", owner, pgno, hdr.pageType))
		return 0
	}
	interior := hdr.pageType == pageTypeTableInterior
	if hdr.numCells == 0 && !zeroCellPageAllowed(root, interior, pgno) {
		w.problem(pgno, fmt.Sprintf("%s: page %d has zero cells but is reachable from a parent", owner, pgno))
	}
	var depth int
	var haveDepth bool
	cov := pageCoverage{spans: make([][2]int, 0, hdr.numCells)}
	defer w.checkPageCoverage(page, pgno, hdr.hdrOffset, &cov)
	for i := 0; i < int(hdr.numCells); i++ {
		off, err := hdr.cellOffset(page, i)
		if err != nil {
			w.problem(pgno, fmt.Sprintf("%s: %v", owner, err))
			cov.broken = true
			continue
		}
		if interior {
			if off+4 > len(page) {
				w.problem(pgno, fmt.Sprintf("%s: page %d cell %d: truncated before child pointer", owner, pgno, i))
				cov.broken = true
				continue
			}
			child := binary.BigEndian.Uint32(page[off : off+4])
			d := w.walkTableTree(child, owner, false, ord)
			w.noteChildDepth(pgno, owner, &depth, &haveDepth, d)
			// In-order position: the separator comes right after the subtree
			// it bounds, and is the one key allowed to repeat that subtree's
			// largest (rowidOrder's doc comment).
			key, n := getVarint(page[off+4:])
			if n == 0 {
				w.problem(pgno, fmt.Sprintf("%s: page %d cell %d: truncated interior-cell rowid key", owner, pgno, i))
				cov.broken = true
				continue
			}
			cov.add(off, 4+n)
			ord.allowEqual = true
			if !ord.step(int64(key)) {
				w.problem(pgno, fmt.Sprintf("%s: page %d cell %d: rowid %d out of order", owner, pgno, i, int64(key)))
			}
			continue
		}
		rowid, length, err := tableLeafCellSpan(page, off, w.usable)
		if err != nil {
			w.problem(pgno, fmt.Sprintf("%s: %v", owner, err))
			cov.broken = true
			continue
		}
		if off+length > len(page) {
			w.problem(pgno, fmt.Sprintf("%s: page %d cell %d: truncated cell", owner, pgno, i))
			cov.broken = true
			continue
		}
		cov.add(off, length)
		// After the span check and skipped with it, exactly as checkTreePage
		// orders these: its "Extends off end of page" arm (btree.c:10977-10980)
		// `continue`s past the rowid test rather than folding a second finding
		// onto an already-unreadable cell.
		if !ord.step(int64(rowid)) {
			w.problem(pgno, fmt.Sprintf("%s: page %d cell %d: rowid %d out of order", owner, pgno, i, int64(rowid)))
		}
		if firstOverflow := tableLeafCellOverflow(page[off:off+length], w.usable); firstOverflow != 0 {
			w.walkOverflowChain(firstOverflow, owner)
		}
	}
	if interior {
		d := w.walkTableTree(hdr.rightmost, owner, false, ord)
		w.noteChildDepth(pgno, owner, &depth, &haveDepth, d)
		return depth + 1
	}
	return 0
}

// walkIndexTree is walkTableTree's INDEX b-tree counterpart (also used for a
// WITHOUT ROWID table's own root -- see structuralRoot's doc comment), through
// idxCellSpan and idxCellOverflow. root/return value: see
// walkTableTree's own doc comment -- identical contract, N2's two checks
// (zeroCellPageAllowed, noteChildDepth) are shared as-is between both walks.
func (w *integrityWalker) walkIndexTree(pgno uint32, owner string, root bool) int {
	if !w.claim(pgno, owner) {
		return 0
	}
	page, err := w.pages(pgno)
	if err != nil {
		w.problem(pgno, fmt.Sprintf("%s: reading page %d: %v", owner, pgno, err))
		return 0
	}
	hdr, err := parseIdxPageHeader(page, pgno)
	if err != nil {
		w.problem(pgno, fmt.Sprintf("%s: %v", owner, err))
		return 0
	}
	interior := hdr.pageType == pageTypeIndexInterior
	if hdr.numCells == 0 && !zeroCellPageAllowed(root, interior, pgno) {
		w.problem(pgno, fmt.Sprintf("%s: page %d has zero cells but is reachable from a parent", owner, pgno))
	}
	childSize := 0
	if interior {
		childSize = 4
	}
	var depth int
	var haveDepth bool
	cov := pageCoverage{spans: make([][2]int, 0, hdr.numCells)}
	defer w.checkPageCoverage(page, pgno, 0, &cov)
	for i := 0; i < int(hdr.numCells); i++ {
		off, err := hdr.cellOffset(page, i)
		if err != nil {
			w.problem(pgno, fmt.Sprintf("%s: %v", owner, err))
			cov.broken = true
			continue
		}
		if interior {
			if off+4 > len(page) {
				w.problem(pgno, fmt.Sprintf("%s: page %d cell %d: truncated before child pointer", owner, pgno, i))
				cov.broken = true
				continue
			}
			child := binary.BigEndian.Uint32(page[off : off+4])
			d := w.walkIndexTree(child, owner, false)
			w.noteChildDepth(pgno, owner, &depth, &haveDepth, d)
		}
		length, err := idxCellSpan(page, off, childSize, w.usable)
		if err != nil {
			w.problem(pgno, fmt.Sprintf("%s: %v", owner, err))
			cov.broken = true
			continue
		}
		cov.add(off, length)
		if firstOverflow := idxCellOverflow(page[off:off+length], w.usable, interior); firstOverflow != 0 {
			w.walkOverflowChain(firstOverflow, owner)
		}
	}
	if interior {
		d := w.walkIndexTree(hdr.rightmost, owner, false)
		w.noteChildDepth(pgno, owner, &depth, &haveDepth, d)
		return depth + 1
	}
	return 0
}

// pageCoverage collects one b-tree page's cells for checkPageCoverage: each
// cell's first and last byte, and whether any cell was unreadable, which in C
// clears doCoverageCheck (btree.c:10970-10980).
type pageCoverage struct {
	spans  [][2]int
	broken bool
}

// add records a cell of size bytes at off. A cell is never shorter than 4
// bytes -- btreeParseCellPtr's "if( pInfo->nSize<4 ) pInfo->nSize = 4;"
// (btree.c:1341, :1380, and cellSizePtrIdxLeaf's :1478).
func (pc *pageCoverage) add(off, size int) {
	pc.spans = append(pc.spans, [2]int{off, off + max(size, 4) - 1})
}

// checkPageCoverage is checkTreePage's coverage test (btree.c:11036-11108),
// run after the page's children as C runs it: every byte from the start of
// the cell content area to the end of the usable area is a cell, a freeblock
// or a fragment, no byte is two of those, and the fragments add up to the
// count the page header keeps at offset 7. A page whose cells or freeblock
// chain C would have refused before getting here -- "Offset out of range",
// "Extends off end of page", btreeComputeFreeSpace's "free space
// corruption" -- is left alone rather than reported under the wrong message.
func (w *integrityWalker) checkPageCoverage(page []byte, pgno uint32, hdrOffset int, pc *pageCoverage) {
	usable := int(w.usable)
	if pc.broken || len(page) < hdrOffset+8 || usable > len(page) {
		return
	}
	content := int(binary.BigEndian.Uint16(page[hdrOffset+5:]))
	if content == 0 {
		content = 65536
	}
	spans := pc.spans
	for _, s := range spans {
		if s[0] < content || s[0] > usable-4 || s[1] >= usable {
			return
		}
	}
	for i := int(binary.BigEndian.Uint16(page[hdrOffset+1:])); i > 0; {
		if i > usable-4 {
			return
		}
		size := int(binary.BigEndian.Uint16(page[i+2:]))
		next := int(binary.BigEndian.Uint16(page[i:]))
		if i+size > usable || (next != 0 && next <= i+size) {
			return
		}
		spans = append(spans, [2]int{i, i + size - 1})
		i = next
	}
	// btreeHeapPull hands entries back smallest first, keyed
	// (start<<16)|end.
	slices.SortFunc(spans, func(a, b [2]int) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})
	nFrag, prev := 0, content-1
	for i, s := range spans {
		if prev >= s[0] {
			w.problem(pgno, fmt.Sprintf("Multiple uses for byte %d of page %d", s[0], pgno))
			// The loop breaks with the overlapping entry already pulled, and
			// the fragment test below runs only if that emptied the heap
			// ("heap[0]==0", btree.c:11104).
			if i < len(spans)-1 {
				return
			}
			break
		}
		nFrag += s[0] - prev - 1
		prev = s[1]
	}
	nFrag += usable - prev - 1
	if frag := int(page[hdrOffset+7]); nFrag != frag {
		w.problem(pgno, fmt.Sprintf("Fragmentation of %d bytes reported as %d on page %d", nFrag, frag, pgno))
	}
}

// walkOverflowChain follows an overflow-page chain rooted at first, claiming
// every page it visits.
func (w *integrityWalker) walkOverflowChain(first uint32, owner string) {
	label := owner + " (overflow chain)"
	pgno := first
	for pgno != 0 {
		if !w.claim(pgno, label) {
			return
		}
		page, err := w.pages(pgno)
		if err != nil {
			w.problem(pgno, fmt.Sprintf("%s: reading page %d: %v", label, pgno, err))
			return
		}
		if len(page) < 4 {
			w.problem(pgno, fmt.Sprintf("%s: page %d too short for its next-overflow-page pointer", label, pgno))
			return
		}
		pgno = binary.BigEndian.Uint32(page[0:4])
	}
}

// walkFreelist decodes the freelist trunk chain from trunk (0 = empty; header
// offset 32), claiming every trunk and listed leaf page, and returns how many
// pages it walked (trunks and leaves, which the header's offset-36 count
// counts).
//
// A trunk page (fileformat2.html, "Freelist Trunk Page Format"): offset 0 the
// next trunk (0 ends), offset 4 the leaf count, then that many leaf numbers,
// all 4-byte big-endian.
//
// The count's corruption bound, maxLeaves, is freePage2's (btree.c:6904-6908):
//
//	nLeaf = get4byte(&pTrunk->aData[4]);
//	if( nLeaf > (u32)pBt->usableSize/4 - 2 ){
//	  rc = SQLITE_CORRUPT_BKPT;
//
// computed as (usable-8)/4, the same integer. It is not C's write-side cap
// (usable/4 - 8, btree.c:6910-6928, R-19920-11576): a fuller trunk is legal to
// read, and checkList enforces the legal maximum (btree.c:10746-10749).
func (w *integrityWalker) walkFreelist(trunk uint32) (walked uint32) {
	maxLeaves := (w.usable - 8) / 4
	for trunk != 0 {
		if !w.claim(trunk, "freelist trunk") {
			return walked
		}
		walked++
		page, err := w.pages(trunk)
		if err != nil {
			w.problem(trunk, fmt.Sprintf("freelist trunk: reading page %d: %v", trunk, err))
			return walked
		}
		if len(page) < 8 {
			w.problem(trunk, fmt.Sprintf("freelist trunk page %d too short for its own header", trunk))
			return walked
		}
		next := binary.BigEndian.Uint32(page[0:4])
		n := binary.BigEndian.Uint32(page[4:8])
		if n > maxLeaves {
			w.problem(trunk, fmt.Sprintf("freelist trunk page %d claims %d leaf entries, more than a page this size can hold (%d)", trunk, n, maxLeaves))
			n = maxLeaves
		}
		for i := uint32(0); i < n; i++ {
			off := 8 + i*4
			if off+4 > w.usable {
				break
			}
			leaf := binary.BigEndian.Uint32(page[off : off+4])
			if w.claim(leaf, "freelist leaf") {
				walked++
			}
		}
		trunk = next
	}
	return walked
}

// idxPageHeader is an index b-tree page's header: its kind, cell count,
// rightmost child (interior pages) and header size.
type idxPageHeader struct {
	pageType   byte
	numCells   uint16
	rightmost  uint32 // interior only
	headerSize int    // 8 (leaf) or 12 (interior)
}

func parseIdxPageHeader(page []byte, pgno uint32) (idxPageHeader, error) {
	if len(page) < 8 {
		return idxPageHeader{}, fmt.Errorf("engine: page %d too short for an index b-tree page header", pgno)
	}
	pageType := page[0]
	headerSize := 8
	switch pageType {
	case pageTypeIndexLeaf:
	case pageTypeIndexInterior:
		headerSize = 12
	default:
		return idxPageHeader{}, fmt.Errorf("engine: page %d: expected an index b-tree page, got type 0x%02x", pgno, pageType)
	}
	if len(page) < headerSize {
		return idxPageHeader{}, fmt.Errorf("engine: page %d too short for its index b-tree page header", pgno)
	}
	h := idxPageHeader{
		pageType:   pageType,
		numCells:   binary.BigEndian.Uint16(page[3:5]),
		headerSize: headerSize,
	}
	if headerSize == 12 {
		h.rightmost = binary.BigEndian.Uint32(page[8:12])
	}
	return h, nil
}

// idxCellSpan is the INDEX b-tree cell-length counterpart of btree.go's
// tableLeafCellSpan: "[4-byte child pointer if interior] varint payload
// length, local payload, [4-byte overflow pointer]".
func idxCellSpan(page []byte, off, childSize int, usable uint32) (length int, err error) {
	if off+childSize > len(page) {
		return 0, fmt.Errorf("engine: index cell at %d: truncated before payload-length varint", off)
	}
	payloadLen, nn := getVarint(page[off+childSize:])
	if nn == 0 {
		return 0, fmt.Errorf("engine: index cell at %d: truncated payload-length varint", off)
	}
	local := localPayloadSizeIndex(payloadLen, usable)
	length = childSize + nn + int(local)
	if uint64(local) < payloadLen {
		length += 4
	}
	if off+length > len(page) {
		return 0, fmt.Errorf("engine: index cell at %d: truncated cell", off)
	}
	return length, nil
}
