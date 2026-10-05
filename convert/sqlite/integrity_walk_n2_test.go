// This file tests two structural checks: (1) a reachable b-tree page
// must carry at least one cell (unless it is a leaf root or interior root
// at page 1); (2) every leaf from one root must be at the same depth.
// It adds mutation-tested positive cases and deep-tree false-positive guards.
package sqlite

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// deepTreeRows/deepTreePageSize produce a genuine 3-level table tree.
const (
	deepTreeRows     = 3000
	deepTreePageSize = 512
)

// buildDeepTableTree creates `t(a INTEGER PRIMARY KEY, b)` with deepTreeRows
// rows at deepTreePageSize -- see the constants' own doc comment for why
// this reliably produces a 3-level tree, the minimum needed for a genuine
// NON-ROOT interior page to exist at all.
func buildDeepTableTree(t *testing.T) string {
	t.Helper()
	stmts := make([]string, 0, deepTreeRows+1)
	stmts = append(stmts, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
	for i := 0; i < deepTreeRows; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'row-%d')`, i, i))
	}
	return buildIWTestDB(t, deepTreePageSize, stmts)
}

// childSlot identifies one child pointer of a table interior page: index>=0
// is a cell's own (4-byte, leading) child pointer at that cell index; -1 is
// the page's own rightmost pointer (btreePageHeader.rightmost).
type childSlot struct {
	index int
	pgno  uint32
}

// interiorChildren reads pgno's own table interior page header and returns
// every child pointer it holds, cell 0..numCells-1 in order, rightmost last.
func interiorChildren(t *testing.T, p *pager, pgno uint32) (btreePageHeader, []childSlot) {
	t.Helper()
	page, err := p.page(pgno)
	if err != nil {
		t.Fatalf("read page %d: %v", pgno, err)
	}
	hdr, err := parseBtreePageHeader(page, pgno)
	if err != nil {
		t.Fatalf("parse header page %d: %v", pgno, err)
	}
	if hdr.pageType != pageTypeTableInterior {
		t.Fatalf("page %d is not a table interior page (type 0x%02x)", pgno, hdr.pageType)
	}
	var out []childSlot
	for i := 0; i < int(hdr.numCells); i++ {
		off, err := hdr.cellOffset(page, i)
		if err != nil {
			t.Fatalf("cell offset %d: %v", i, err)
		}
		if off+4 > len(page) {
			t.Fatalf("page %d cell %d truncated before child pointer", pgno, i)
		}
		out = append(out, childSlot{index: i, pgno: binary.BigEndian.Uint32(page[off : off+4])})
	}
	out = append(out, childSlot{index: -1, pgno: hdr.rightmost})
	return hdr, out
}

// tablePageType returns the raw page-type byte of pgno (0x05/0x0d).
func tablePageType(t *testing.T, p *pager, pgno uint32) byte {
	t.Helper()
	page, err := p.page(pgno)
	if err != nil {
		t.Fatalf("read page %d: %v", pgno, err)
	}
	hdr, err := parseBtreePageHeader(page, pgno)
	if err != nil {
		t.Fatalf("parse header page %d: %v", pgno, err)
	}
	return hdr.pageType
}

// descendToTableLeaf follows cell 0's own child pointer down from pgno until
// it reaches a genuine leaf page, returning that leaf's page number.
func descendToTableLeaf(t *testing.T, p *pager, pgno uint32) uint32 {
	t.Helper()
	page, err := p.page(pgno)
	if err != nil {
		t.Fatalf("read page %d: %v", pgno, err)
	}
	hdr, err := parseBtreePageHeader(page, pgno)
	if err != nil {
		t.Fatalf("parse header page %d: %v", pgno, err)
	}
	if hdr.pageType == pageTypeTableLeaf {
		return pgno
	}
	off, err := hdr.cellOffset(page, 0)
	if err != nil {
		t.Fatalf("cell offset 0: %v", err)
	}
	if off+4 > len(page) {
		t.Fatalf("page %d cell 0 truncated before child pointer", pgno)
	}
	return descendToTableLeaf(t, p, binary.BigEndian.Uint32(page[off:off+4]))
}

// tableChildPointerOffset returns the file byte offset of a child pointer.
func tableChildPointerOffset(t *testing.T, path string, parentPgno uint32, slot int) int64 {
	t.Helper()
	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	page, err := p.page(parentPgno)
	if err != nil {
		t.Fatalf("read page %d: %v", parentPgno, err)
	}
	hdr, err := parseBtreePageHeader(page, parentPgno)
	if err != nil {
		t.Fatalf("parse header page %d: %v", parentPgno, err)
	}
	base := int64(parentPgno-1) * int64(deepTreePageSize)
	if slot == -1 {
		return base + int64(hdr.hdrOffset+8)
	}
	off, err := hdr.cellOffset(page, slot)
	if err != nil {
		t.Fatalf("cell offset %d: %v", slot, err)
	}
	return base + int64(off)
}

// TestCheckStructuralIntegrityDetectsZeroCellReachablePage verifies that
// a non-root interior page with zero cells is detected as corrupt.
func TestCheckStructuralIntegrityDetectsZeroCellReachablePage(t *testing.T) {
	path := buildDeepTableTree(t)

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tr := struct{ RootPage uint32 }{rootOf(t, p, "t")}
	_, children := interiorChildren(t, p, tr.RootPage)
	var victim uint32
	for _, c := range children {
		if tablePageType(t, p, c.pgno) == pageTypeTableInterior {
			victim = c.pgno
			break
		}
	}
	if victim == 0 {
		t.Fatalf("test setup did not produce a genuine non-root interior page (need >=3 tree levels); got children: %+v", children)
	}
	p.Close()

	writeAt(t, path, int64(victim-1)*deepTreePageSize+3, []byte{0, 0})

	p, err = openPager(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p.Close()
	res, err := p.checkStructuralIntegrity()
	if err != nil {
		t.Fatalf("CheckStructuralIntegrity: %v", err)
	}
	if res.OK {
		t.Fatalf("expected the zero-cell non-root interior page to be reported, got a clean result")
	}
	found := false
	for _, prob := range res.Problems {
		if prob.Page == victim && strings.Contains(prob.Message, "zero cells") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a zero-cell finding on page %d, got: %+v", victim, res.Problems)
	}
}

// TestCheckStructuralIntegrityDetectsChildDepthMismatch verifies detection
// of children at mismatched depths under the same root.
func TestCheckStructuralIntegrityDetectsChildDepthMismatch(t *testing.T) {
	path := buildDeepTableTree(t)

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tr := struct{ RootPage uint32 }{rootOf(t, p, "t")}
	_, children := interiorChildren(t, p, tr.RootPage)

	var victim childSlot
	haveVictim, haveSibling := false, false
	for _, c := range children {
		if tablePageType(t, p, c.pgno) != pageTypeTableInterior {
			continue
		}
		if !haveVictim {
			victim, haveVictim = c, true
		} else {
			haveSibling = true
		}
	}
	if !haveVictim || !haveSibling {
		t.Fatalf("test setup did not produce a root with >=2 interior children (need a genuine 3-level tree); got children: %+v", children)
	}
	shallowLeaf := descendToTableLeaf(t, p, victim.pgno)
	p.Close()

	off := tableChildPointerOffset(t, path, tr.RootPage, victim.index)
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], shallowLeaf)
	writeAt(t, path, off, buf[:])

	p, err = openPager(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p.Close()
	res, err := p.checkStructuralIntegrity()
	if err != nil {
		t.Fatalf("CheckStructuralIntegrity: %v", err)
	}
	if res.OK {
		t.Fatalf("expected the child-depth mismatch to be reported, got a clean result")
	}
	found := false
	for _, prob := range res.Problems {
		if prob.Page == tr.RootPage && strings.Contains(prob.Message, "depth differs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'child page depth differs' finding on root page %d, got: %+v", tr.RootPage, res.Problems)
	}
}

// TestCheckStructuralIntegrityCleanOnDeepTableTree verifies false-positive
// guards: an unmodified deep tree must report clean.
func TestCheckStructuralIntegrityCleanOnDeepTableTree(t *testing.T) {
	path := buildDeepTableTree(t)
	mustCheckClean(t, path)
}

// TestCheckStructuralIntegrityCleanOnDeepIndexTree verifies the same for a
// multi-level index tree.
func TestCheckStructuralIntegrityCleanOnDeepIndexTree(t *testing.T) {
	const n = 800
	stmts := make([]string, 0, n+2)
	stmts = append(stmts, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE INDEX i ON t(b)`)
	for i := 0; i < n; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'row-value-%06d')`, i, i))
	}
	path := buildIWTestDB(t, 512, stmts)

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ir := struct{ RootPage uint32 }{rootOf(t, p, "i")}
	page, err := p.page(ir.RootPage)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	hdr, err := parseIdxPageHeader(page, ir.RootPage)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if hdr.pageType != pageTypeIndexInterior {
		t.Fatalf("test setup did not produce a multi-level index tree (root type 0x%02x); increase row count", hdr.pageType)
	}
	p.Close()

	mustCheckClean(t, path)
}
