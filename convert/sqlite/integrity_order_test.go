// Tests that structural integrity checks detect out-of-order cells.
package sqlite

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Small page size for multi-level test trees.
const gappyPageSize = 512

// buildSingleLeafDB creates a single-leaf table for testing.
func buildSingleLeafDB(t *testing.T) (string, uint32) {
	t.Helper()
	stmts := []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`}
	for i := 1; i <= 5; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'row-%d')`, i, i))
	}
	path := buildIWTestDB(t, gappyPageSize, stmts)

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	root := rootOf(t, p, "t")
	if got := tablePageType(t, p, root); got != pageTypeTableLeaf {
		t.Fatalf("fixture root page %d is type 0x%02x, expected a single leaf", root, got)
	}
	return path, root
}

// TestIntegrityCheckDetectsOutOfOrderLeafCells is the leaf half of the new
// ordering check. It swaps two entries of a leaf's CELL POINTER ARRAY, which
// leaves every cell's bytes, the page's free-space accounting, and the set of
// rows the tree holds all completely untouched -- the only thing it breaks is
// the ascending-key ordering the binary search relies on. Before this check
// existed, PRAGMA integrity_check called such a file clean.
//
// It also asserts the hazard directly: on the patched file the point lookup
// and the full scan DISAGREE about a row that is still physically present.
// That disagreement is a wrong answer, which is worse than an error
// (invariant 2), and is the whole reason the check has to exist.
func TestIntegrityCheckDetectsOutOfOrderLeafCells(t *testing.T) {
	path, root := buildSingleLeafDB(t)
	base := int64(root-1) * gappyPageSize

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	page, err := p.page(root)
	if err != nil {
		t.Fatalf("read page %d: %v", root, err)
	}
	hdr, err := parseBtreePageHeader(page, root)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if hdr.numCells != 5 {
		t.Fatalf("fixture leaf holds %d cells, expected 5", hdr.numCells)
	}
	if res, err := p.checkStructuralIntegrity(); err != nil || !res.OK {
		t.Fatalf("fixture is not clean before patching: %+v (err %v)", res, err)
	}
	p.Close()

	// Swap cell pointers 1 and 3.
	a, b := int64(hdr.cellPtrBase+2*1), int64(hdr.cellPtrBase+2*3)
	pa := slices.Clone(page[a : a+2])
	pb := slices.Clone(page[b : b+2])
	writeAt(t, path, base+a, pb)
	writeAt(t, path, base+b, pa)

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
		t.Fatalf("expected the out-of-order leaf cells to be reported, got a clean result")
	}
	found := false
	for _, prob := range res.Problems {
		if prob.Page == root && strings.Contains(prob.Message, "out of order") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an out-of-order finding on page %d, got: %+v", root, res.Problems)
	}
}

// TestIntegrityCheckDetectsInteriorKeyBelowSubtree is the interior half: an
// interior cell's key must BOUND the subtree its left-child pointer addresses
// (it is a copy of that subtree's largest rowid, btree_write.go; this engine's
// incremental delete may leave it stale-HIGH, which stays a valid upper bound
// -- see incremental_write.go's "why no rebalancing" note -- but never low).
// Patching one separator DOWN, in place and to a same-length varint so no
// other byte on the page moves, is the shape that makes the descent skip past
// rows that are really there.
func TestIntegrityCheckDetectsInteriorKeyBelowSubtree(t *testing.T) {
	path := buildDeepTableTree(t)

	p, err := openPager(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	root := rootOf(t, p, "t")
	if res, err := p.checkStructuralIntegrity(); err != nil || !res.OK {
		t.Fatalf("fixture is not clean before patching: %+v (err %v)", res, err)
	}
	page, err := p.page(root)
	if err != nil {
		t.Fatalf("read page %d: %v", root, err)
	}
	hdr, err := parseBtreePageHeader(page, root)
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if hdr.pageType != pageTypeTableInterior || hdr.numCells == 0 {
		t.Fatalf("fixture root page %d is not a table interior page with cells (type 0x%02x, %d cells)", root, hdr.pageType, hdr.numCells)
	}
	off, err := hdr.cellOffset(page, 0)
	if err != nil {
		t.Fatalf("cell offset 0: %v", err)
	}
	key, n := getVarint(page[off+4:])
	if n == 0 {
		t.Fatalf("fixture root cell 0 has no readable key")
	}
	p.Close()

	// The smallest value that still encodes in exactly n bytes, so the patch
	// is byte-for-byte in place: everything after the key stays where it is.
	var small uint64 = 1
	if n > 1 {
		small = 1 << (7 * uint(n-1))
	}
	if small >= key {
		t.Fatalf("fixture root separator %d is already minimal for its %d-byte varint", key, n)
	}
	buf := make([]byte, 9)
	if got := putVarint(buf, small); got != n {
		t.Fatalf("re-encoding %d took %d bytes, expected %d", small, got, n)
	}
	writeAt(t, path, int64(root-1)*deepTreePageSize+int64(off+4), buf[:n])

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
		t.Fatalf("expected the under-bounding interior separator to be reported, got a clean result")
	}
	found := false
	for _, prob := range res.Problems {
		if prob.Page == root && strings.Contains(prob.Message, "out of order") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an out-of-order finding on page %d, got: %+v", root, res.Problems)
	}
}
