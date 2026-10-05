package engine

// rtreeTree is an r-tree maintained exactly as in ext/rtree/rtree.c.
// The tree shape is observable via shadow tables, so the algorithm must match
// exactly. Each node is kept as C's zData buffer and edited by ported algorithms:
// ChooseLeaf, AdjustTree, R*-tree SplitNode, and LIFO reinsertion of removed nodes.

import (
	"encoding/binary"
	"fmt"
	"math"
)

type rtreeTree struct {
	nodeSize      int
	nDim2         int
	nBytesPerCell int
	i32           bool
	depth         int

	nodes  map[int64]*rtreeTNode // %_node
	parent map[int64]int64       // %_parent: child node -> parent node
	rowid  map[int64]int64       // %_rowid: rowid -> leaf node

	deleted []*rtreeTNode // Rtree.pDeleted, most recent LAST
}

type rtreeTNode struct {
	no     int64 // 0 until SplitNode writes a new node
	data   []byte
	parent *rtreeTNode
	cached bool
	height int // removeNode's iHeight, once the node is on the deleted list
}

// rtreeTCell is RtreeCell: the rowid or child node number and the raw
// coordinate words (RtreeCoord is a union; i32 picks the reading).
type rtreeTCell struct {
	rowid int64
	c     [2 * rtreeMaxDimensions]uint32
}

func newRtreeTree(nodeSize, nDim2 int, i32 bool) *rtreeTree {
	t := &rtreeTree{nodeSize: nodeSize, nDim2: nDim2, nBytesPerCell: rtreeBytesPerCell(nDim2), i32: i32,
		nodes: map[int64]*rtreeTNode{}, parent: map[int64]int64{}, rowid: map[int64]int64{}}
	// rtreeSqlInit's "INSERT INTO %_node VALUES(1, zeroblob(iNodeSize))"
	// (rtree.c:3455-3457): the root exists even for an empty tree.
	t.nodes[1] = &rtreeTNode{no: 1, data: make([]byte, nodeSize)}
	return t
}

func (t *rtreeTree) clone() *rtreeTree {
	cp := *t
	cp.nodes = make(map[int64]*rtreeTNode, len(t.nodes))
	for no, n := range t.nodes {
		cp.nodes[no] = &rtreeTNode{no: n.no, data: append([]byte(nil), n.data...)}
	}
	cp.parent = make(map[int64]int64, len(t.parent))
	for k, v := range t.parent {
		cp.parent[k] = v
	}
	cp.rowid = make(map[int64]int64, len(t.rowid))
	for k, v := range t.rowid {
		cp.rowid[k] = v
	}
	cp.deleted = nil
	return &cp
}

var errRtreeCorrupt = fmt.Errorf("database disk image is malformed")

// ---- node primitives (rtree.c:604-994) ----

func (t *rtreeTree) maxCells() int { return (t.nodeSize - 4) / t.nBytesPerCell }

// minCells is RTREE_MINCELLS.
func (t *rtreeTree) minCells() int { return t.maxCells() / 3 }

func rtreeNCell(n *rtreeTNode) int { return int(binary.BigEndian.Uint16(n.data[2:4])) }

func (t *rtreeTree) cellOff(i int) int { return 4 + t.nBytesPerCell*i }

func (t *rtreeTree) nodeGetRowid(n *rtreeTNode, i int) int64 {
	return int64(binary.BigEndian.Uint64(n.data[t.cellOff(i):]))
}

func (t *rtreeTree) nodeGetCell(n *rtreeTNode, i int) rtreeTCell {
	var c rtreeTCell
	off := t.cellOff(i)
	c.rowid = int64(binary.BigEndian.Uint64(n.data[off:]))
	for k := 0; k < t.nDim2; k++ {
		c.c[k] = binary.BigEndian.Uint32(n.data[off+8+4*k:])
	}
	return c
}

func (t *rtreeTree) nodeOverwriteCell(n *rtreeTNode, c *rtreeTCell, i int) {
	off := t.cellOff(i)
	binary.BigEndian.PutUint64(n.data[off:], uint64(c.rowid))
	for k := 0; k < t.nDim2; k++ {
		binary.BigEndian.PutUint32(n.data[off+8+4*k:], c.c[k])
	}
}

// nodeDeleteCell (rtree.c:851-859): the memmove leaves the old last cell's
// bytes where they were.
func (t *rtreeTree) nodeDeleteCell(n *rtreeTNode, i int) {
	nc := rtreeNCell(n)
	dst := t.cellOff(i)
	src := dst + t.nBytesPerCell
	copy(n.data[dst:], n.data[src:src+(nc-i-1)*t.nBytesPerCell])
	binary.BigEndian.PutUint16(n.data[2:4], uint16(nc-1))
}

// nodeInsertCell (rtree.c:866-888) reports true when the node was FULL, in
// which case nothing was written.
func (t *rtreeTree) nodeInsertCell(n *rtreeTNode, c *rtreeTCell) bool {
	nc := rtreeNCell(n)
	if nc < t.maxCells() {
		t.nodeOverwriteCell(n, c, nc)
		binary.BigEndian.PutUint16(n.data[2:4], uint16(nc+1))
	}
	return nc == t.maxCells()
}

// nodeZero (rtree.c:639-642) clears everything but the depth bytes.
func (t *rtreeTree) nodeZero(n *rtreeTNode) {
	clear(n.data[2:])
}

func (t *rtreeTree) resetCache() {
	for _, n := range t.nodes {
		n.cached, n.parent = false, nil
	}
}

// nodeAcquire, over an all-in-memory node set. Acquiring the root reads the
// depth out of its first two bytes, as rtree.c:790-797 does.
func (t *rtreeTree) nodeAcquire(no int64, parent *rtreeTNode) (*rtreeTNode, error) {
	n := t.nodes[no]
	if n == nil {
		return nil, errRtreeCorrupt
	}
	if !n.cached {
		n.cached = true
		n.parent = parent
	}
	if no == 1 {
		t.depth = int(binary.BigEndian.Uint16(n.data[0:2]))
	}
	return n, nil
}

func (t *rtreeTree) nodeHashLookup(no int64) *rtreeTNode {
	if n := t.nodes[no]; n != nil && n.cached {
		return n
	}
	return nil
}

// nodeWrite for a NEW node: "INSERT INTO %_node VALUES(NULL, ...)", whose
// rowid is one past the largest nodeno now in the table.
func (t *rtreeTree) nodeWriteNew(n *rtreeTNode) {
	var max int64
	for no := range t.nodes {
		if no > max {
			max = no
		}
	}
	n.no = max + 1
	n.cached = true
	t.nodes[n.no] = n
}

// nodeRowidIndex (rtree.c:1400-1416).
func (t *rtreeTree) nodeRowidIndex(n *rtreeTNode, rowid int64) (int, error) {
	nc := rtreeNCell(n)
	for i := 0; i < nc; i++ {
		if t.nodeGetRowid(n, i) == rowid {
			return i, nil
		}
	}
	return 0, errRtreeCorrupt
}

// nodeParentIndex (rtree.c:1423-1431): the root is its own "index -1".
func (t *rtreeTree) nodeParentIndex(n *rtreeTNode) (int, error) {
	if n.parent == nil {
		return -1, nil
	}
	return t.nodeRowidIndex(n.parent, n.no)
}

// ---- cell geometry (rtree.c:2146-2250) ----

func (t *rtreeTree) f(c *rtreeTCell, i int) float32 { return math.Float32frombits(c.c[i]) }
func (t *rtreeTree) i(c *rtreeTCell, i int) int32   { return int32(c.c[i]) }

// dcoord is DCOORD: the coordinate widened to RtreeDValue (double).
func (t *rtreeTree) dcoord(c *rtreeTCell, i int) float64 {
	if t.i32 {
		return float64(t.i(c, i))
	}
	return float64(t.f(c, i))
}

// cellArea keeps C's arithmetic exactly: for "rtree" each side is a FLOAT
// subtraction (RtreeCoord.f), and only the product is double.
func (t *rtreeTree) cellArea(c *rtreeTCell) float64 {
	area := 1.0
	for d := t.nDim2/2 - 1; d >= 0; d-- {
		if t.i32 {
			area *= float64(int64(t.i(c, 2*d+1)) - int64(t.i(c, 2*d)))
		} else {
			area *= float64(t.f(c, 2*d+1) - t.f(c, 2*d))
		}
	}
	return area
}

// cellMargin sums from the LAST dimension down, as C's loop does.
func (t *rtreeTree) cellMargin(c *rtreeTCell) float64 {
	margin := 0.0
	for ii := t.nDim2 - 2; ii >= 0; ii -= 2 {
		margin += t.dcoord(c, ii+1) - t.dcoord(c, ii)
	}
	return margin
}

func (t *rtreeTree) cellUnion(p1, p2 *rtreeTCell) {
	for ii := 0; ii < t.nDim2; ii += 2 {
		if t.i32 {
			p1.c[ii] = uint32(min(t.i(p1, ii), t.i(p2, ii)))
			p1.c[ii+1] = uint32(max(t.i(p1, ii+1), t.i(p2, ii+1)))
		} else {
			// MIN/MAX are "((x)<(y)?(x):(y))" and "((x)>(y)?(x):(y))".
			a, b := t.f(p1, ii), t.f(p2, ii)
			if !(a < b) {
				p1.c[ii] = p2.c[ii]
			}
			a, b = t.f(p1, ii+1), t.f(p2, ii+1)
			if !(a > b) {
				p1.c[ii+1] = p2.c[ii+1]
			}
		}
	}
}

func (t *rtreeTree) cellContains(p1, p2 *rtreeTCell) bool {
	for ii := 0; ii < t.nDim2; ii += 2 {
		if t.i32 {
			if t.i(p2, ii) < t.i(p1, ii) || t.i(p2, ii+1) > t.i(p1, ii+1) {
				return false
			}
		} else if t.f(p2, ii) < t.f(p1, ii) || t.f(p2, ii+1) > t.f(p1, ii+1) {
			return false
		}
	}
	return true
}

func (t *rtreeTree) cellOverlap(p *rtreeTCell, a []rtreeTCell) float64 {
	overlap := 0.0
	for ii := range a {
		o := 1.0
		for jj := 0; jj < t.nDim2; jj += 2 {
			// MAX and MIN are C's ternaries, not math.Max/Min.
			x1, y1 := t.dcoord(p, jj), t.dcoord(&a[ii], jj)
			if !(x1 > y1) {
				x1 = y1
			}
			x2, y2 := t.dcoord(p, jj+1), t.dcoord(&a[ii], jj+1)
			if !(x2 < y2) {
				x2 = y2
			}
			if x2 < x1 {
				o = 0
				break
			}
			o *= x2 - x1
		}
		overlap += o
	}
	return overlap
}

// ---- insertion (rtree.c:2260-2720, 2871-2928) ----

// chooseLeaf is ChooseLeaf (rtree.c:2260-2327).
func (t *rtreeTree) chooseLeaf(cell *rtreeTCell, iHeight int) (*rtreeTNode, error) {
	node, err := t.nodeAcquire(1, nil)
	if err != nil {
		return nil, err
	}
	for ii := 0; ii < t.depth-iHeight; ii++ {
		var iBest int64
		found := false
		var fMinGrowth, fMinArea float64
		nc := rtreeNCell(node)
		for i := 0; i < nc; i++ {
			c := t.nodeGetCell(node, i)
			if t.cellContains(&c, cell) {
				area := t.cellArea(&c)
				if !found || area < fMinArea {
					iBest, fMinArea, found = c.rowid, area, true
				}
			}
		}
		if !found {
			for i := 0; i < nc; i++ {
				c := t.nodeGetCell(node, i)
				area := t.cellArea(&c)
				t.cellUnion(&c, cell)
				growth := t.cellArea(&c) - area
				if i == 0 || growth < fMinGrowth || (growth == fMinGrowth && area < fMinArea) {
					fMinGrowth, fMinArea, iBest = growth, area, c.rowid
				}
			}
		}
		child, err := t.nodeAcquire(iBest, node)
		if err != nil {
			return nil, err
		}
		node = child
	}
	return node, nil
}

// adjustTree is AdjustTree (rtree.c:2334-2367).
func (t *rtreeTree) adjustTree(node *rtreeTNode, cell *rtreeTCell) error {
	cnt := 0
	for p := node; p.parent != nil; p = p.parent {
		cnt++
		if cnt > 100 {
			return errRtreeCorrupt
		}
		i, err := t.nodeParentIndex(p)
		if err != nil {
			return err
		}
		c := t.nodeGetCell(p.parent, i)
		if !t.cellContains(&c, cell) {
			t.cellUnion(&c, cell)
			t.nodeOverwriteCell(p.parent, &c, i)
		}
	}
	return nil
}

// sortByDimension is SortByDimension (rtree.c:2404-2457), a merge sort whose
// tie rule (min first, then max) and merge order decide the split.
func (t *rtreeTree) sortByDimension(aIdx []int, iDim int, aCell []rtreeTCell, aSpare []int) {
	nIdx := len(aIdx)
	if nIdx <= 1 {
		return
	}
	nLeft := nIdx / 2
	nRight := nIdx - nLeft
	aLeft := aIdx[:nLeft]
	aRight := aIdx[nLeft:]
	t.sortByDimension(aLeft, iDim, aCell, aSpare)
	t.sortByDimension(aRight, iDim, aCell, aSpare)
	copy(aSpare, aLeft)
	aLeft = aSpare[:nLeft]
	iLeft, iRight := 0, 0
	for iLeft < nLeft || iRight < nRight {
		takeLeft := false
		if iLeft != nLeft {
			if iRight == nRight {
				takeLeft = true
			} else {
				xl1 := t.dcoord(&aCell[aLeft[iLeft]], iDim*2)
				xl2 := t.dcoord(&aCell[aLeft[iLeft]], iDim*2+1)
				xr1 := t.dcoord(&aCell[aRight[iRight]], iDim*2)
				xr2 := t.dcoord(&aCell[aRight[iRight]], iDim*2+1)
				takeLeft = xl1 < xr1 || (xl1 == xr1 && xl2 < xr2)
			}
		}
		if takeLeft {
			aIdx[iLeft+iRight] = aLeft[iLeft]
			iLeft++
		} else {
			aIdx[iLeft+iRight] = aRight[iRight]
			iRight++
		}
	}
}

// splitNodeStartree is rtree.c:2463-2559.
func (t *rtreeTree) splitNodeStartree(aCell []rtreeTCell, left, right *rtreeTNode) (bboxLeft, bboxRight rtreeTCell) {
	nCell := len(aCell)
	nDim := t.nDim2 / 2
	aSorted := make([][]int, nDim)
	aSpare := make([]int, nCell)
	for ii := 0; ii < nDim; ii++ {
		aSorted[ii] = make([]int, nCell)
		for jj := range aSorted[ii] {
			aSorted[ii][jj] = jj
		}
		t.sortByDimension(aSorted[ii], ii, aCell, aSpare)
	}
	iBestDim, iBestSplit := 0, 0
	fBestMargin := 0.0
	minc := t.minCells()
	for ii := 0; ii < nDim; ii++ {
		margin := 0.0
		fBestOverlap, fBestArea := 0.0, 0.0
		iBestLeft := 0
		for nLeft := minc; nLeft <= nCell-minc; nLeft++ {
			l := aCell[aSorted[ii][0]]
			r := aCell[aSorted[ii][nCell-1]]
			for kk := 1; kk < nCell-1; kk++ {
				if kk < nLeft {
					t.cellUnion(&l, &aCell[aSorted[ii][kk]])
				} else {
					t.cellUnion(&r, &aCell[aSorted[ii][kk]])
				}
			}
			margin += t.cellMargin(&l)
			margin += t.cellMargin(&r)
			overlap := t.cellOverlap(&l, []rtreeTCell{r})
			area := t.cellArea(&l) + t.cellArea(&r)
			if nLeft == minc || overlap < fBestOverlap || (overlap == fBestOverlap && area < fBestArea) {
				iBestLeft, fBestOverlap, fBestArea = nLeft, overlap, area
			}
		}
		if ii == 0 || margin < fBestMargin {
			iBestDim, fBestMargin, iBestSplit = ii, margin, iBestLeft
		}
	}
	bboxLeft = aCell[aSorted[iBestDim][0]]
	bboxRight = aCell[aSorted[iBestDim][iBestSplit]]
	for ii := 0; ii < nCell; ii++ {
		c := &aCell[aSorted[iBestDim][ii]]
		if ii < iBestSplit {
			t.nodeInsertCell(left, c)
			t.cellUnion(&bboxLeft, c)
		} else {
			t.nodeInsertCell(right, c)
			t.cellUnion(&bboxRight, c)
		}
	}
	return bboxLeft, bboxRight
}

// updateMapping is rtree.c:2561-2583.
func (t *rtreeTree) updateMapping(rowid int64, node *rtreeTNode, iHeight int) error {
	if iHeight > 0 {
		child := t.nodeHashLookup(rowid)
		for p := node; p != nil; p = p.parent {
			if p == child {
				return errRtreeCorrupt
			}
		}
		if child != nil {
			child.parent = node
		}
		t.parent[rowid] = node.no
		return nil
	}
	t.rowid[rowid] = node.no
	return nil
}

// splitNode is SplitNode (rtree.c:2585-2719).
func (t *rtreeTree) splitNode(node *rtreeTNode, cell *rtreeTCell, iHeight int) error {
	nCell := rtreeNCell(node)
	aCell := make([]rtreeTCell, 0, nCell+1)
	for i := 0; i < nCell; i++ {
		aCell = append(aCell, t.nodeGetCell(node, i))
	}
	t.nodeZero(node)
	aCell = append(aCell, *cell)

	var left, right *rtreeTNode
	if node.no == 1 {
		right = &rtreeTNode{data: make([]byte, t.nodeSize), parent: node, cached: true}
		left = &rtreeTNode{data: make([]byte, t.nodeSize), parent: node, cached: true}
		t.depth++
		binary.BigEndian.PutUint16(node.data[0:2], uint16(t.depth))
	} else {
		left = node
		right = &rtreeTNode{data: make([]byte, t.nodeSize), parent: left.parent, cached: true}
	}
	clear(left.data)
	clear(right.data)

	leftbbox, rightbbox := t.splitNodeStartree(aCell, left, right)

	// "Ensure both child nodes have node numbers assigned" -- right first.
	t.nodeWriteNew(right)
	if left.no == 0 {
		t.nodeWriteNew(left)
	}
	rightbbox.rowid = right.no
	leftbbox.rowid = left.no

	if node.no == 1 {
		if err := t.insertCell(left.parent, &leftbbox, iHeight+1); err != nil {
			return err
		}
	} else {
		par := left.parent
		i, err := t.nodeParentIndex(left)
		if err != nil {
			return err
		}
		t.nodeOverwriteCell(par, &leftbbox, i)
		if err := t.adjustTree(par, &leftbbox); err != nil {
			return err
		}
	}
	if err := t.insertCell(right.parent, &rightbbox, iHeight+1); err != nil {
		return err
	}

	newCellIsRight := false
	for i := 0; i < rtreeNCell(right); i++ {
		rid := t.nodeGetRowid(right, i)
		if err := t.updateMapping(rid, right, iHeight); err != nil {
			return err
		}
		if rid == cell.rowid {
			newCellIsRight = true
		}
	}
	if node.no == 1 {
		for i := 0; i < rtreeNCell(left); i++ {
			if err := t.updateMapping(t.nodeGetRowid(left, i), left, iHeight); err != nil {
				return err
			}
		}
	} else if !newCellIsRight {
		return t.updateMapping(cell.rowid, left, iHeight)
	}
	return nil
}

// insertCell is rtreeInsertCell (rtree.c:2871-2899).
func (t *rtreeTree) insertCell(node *rtreeTNode, cell *rtreeTCell, iHeight int) error {
	if iHeight > 0 {
		if child := t.nodeHashLookup(cell.rowid); child != nil {
			child.parent = node
		}
	}
	if t.nodeInsertCell(node, cell) {
		return t.splitNode(node, cell, iHeight)
	}
	if err := t.adjustTree(node, cell); err != nil {
		return err
	}
	if iHeight == 0 {
		t.rowid[cell.rowid] = node.no
	} else {
		t.parent[cell.rowid] = node.no
	}
	return nil
}

// ---- deletion (rtree.c:2724-2869, 2901-2928, 2943-3032) ----

// fixLeafParent is rtree.c:2724-2755.
func (t *rtreeTree) fixLeafParent(leaf *rtreeTNode) error {
	child := leaf
	for child.no != 1 && child.parent == nil {
		par, ok := t.parent[child.no]
		if ok {
			var test *rtreeTNode
			for test = leaf; test != nil && test.no != par; test = test.parent {
			}
			if test == nil {
				p, err := t.nodeAcquire(par, nil)
				if err != nil {
					return err
				}
				child.parent = p
			}
		}
		if child.parent == nil {
			return errRtreeCorrupt
		}
		child = child.parent
	}
	return nil
}

// removeNode is rtree.c:2759-2807.
func (t *rtreeTree) removeNode(node *rtreeTNode, iHeight int) error {
	i, err := t.nodeParentIndex(node)
	if err != nil {
		return err
	}
	par := node.parent
	node.parent = nil
	if err := t.deleteCell(par, i, iHeight+1); err != nil {
		return err
	}
	delete(t.nodes, node.no)
	delete(t.parent, node.no)
	node.cached = false
	node.height = iHeight
	t.deleted = append(t.deleted, node)
	return nil
}

// fixBoundingBox is rtree.c:2809-2831.
func (t *rtreeTree) fixBoundingBox(node *rtreeTNode) error {
	par := node.parent
	if par == nil {
		return nil
	}
	nc := rtreeNCell(node)
	box := t.nodeGetCell(node, 0)
	for ii := 1; ii < nc; ii++ {
		c := t.nodeGetCell(node, ii)
		t.cellUnion(&box, &c)
	}
	box.rowid = node.no
	i, err := t.nodeParentIndex(node)
	if err != nil {
		return err
	}
	t.nodeOverwriteCell(par, &box, i)
	return t.fixBoundingBox(par)
}

// deleteCell is rtree.c:2836-2869.
func (t *rtreeTree) deleteCell(node *rtreeTNode, iCell, iHeight int) error {
	if err := t.fixLeafParent(node); err != nil {
		return err
	}
	t.nodeDeleteCell(node, iCell)
	if node.parent != nil {
		if rtreeNCell(node) < t.minCells() {
			return t.removeNode(node, iHeight)
		}
		return t.fixBoundingBox(node)
	}
	return nil
}

// reinsertNodeContent is rtree.c:2901-2928.
func (t *rtreeTree) reinsertNodeContent(node *rtreeTNode) error {
	nc := rtreeNCell(node)
	for ii := 0; ii < nc; ii++ {
		c := t.nodeGetCell(node, ii)
		ins, err := t.chooseLeaf(&c, node.height)
		if err != nil {
			return err
		}
		if err := t.insertCell(ins, &c, node.height); err != nil {
			return err
		}
	}
	return nil
}

// deleteRowid is rtreeDeleteRowid (rtree.c:2943-3032). A rowid %_rowid does
// not hold deletes nothing, but the root-collapse check still runs, as in C.
func (t *rtreeTree) deleteRowid(iDelete int64) error {
	root, err := t.nodeAcquire(1, nil)
	if err != nil {
		return err
	}
	if leafNo, ok := t.rowid[iDelete]; ok {
		leaf, err := t.nodeAcquire(leafNo, nil)
		if err != nil {
			return err
		}
		i, err := t.nodeRowidIndex(leaf, iDelete)
		if err != nil {
			return err
		}
		if err := t.deleteCell(leaf, i, 0); err != nil {
			return err
		}
	}
	delete(t.rowid, iDelete)
	if t.depth > 0 && rtreeNCell(root) == 1 {
		child, err := t.nodeAcquire(t.nodeGetRowid(root, 0), root)
		if err != nil {
			return err
		}
		if err := t.removeNode(child, t.depth-1); err != nil {
			return err
		}
		t.depth--
		binary.BigEndian.PutUint16(root.data[0:2], uint16(t.depth))
	}
	for len(t.deleted) > 0 {
		n := t.deleted[len(t.deleted)-1]
		err := t.reinsertNodeContent(n)
		t.deleted = t.deleted[:len(t.deleted)-1]
		if err != nil {
			t.deleted = nil
			return err
		}
	}
	return nil
}

// newRowid is rtreeNewRowid (rtree.c:2930-2938): "INSERT INTO %_rowid
// VALUES(NULL, NULL)", i.e. one past the largest rowid %_rowid holds.
func (t *rtreeTree) newRowid() int64 {
	var max int64
	for rid := range t.rowid {
		if rid > max {
			max = rid
		}
	}
	return max + 1
}

// insert is the insertion half of rtreeUpdate (rtree.c:3209-3228).
func (t *rtreeTree) insert(cell *rtreeTCell) error {
	leaf, err := t.chooseLeaf(cell, 0)
	if err != nil {
		return err
	}
	return t.insertCell(leaf, cell, 0)
}

// ---- reading ----

// walk visits the leaf cells in the order rtree.c's cursor returns them for a
// scan: depth first from the root, each node's cells in stored order. Its
// priority queue (rtreeSearchPointNew/Pop, rtree.c:1536-1650) orders search
// points by (rScore, iLevel), and with no geometry callback every score is 0
// and at most one point is pending per level, so the queue IS that depth-first
// order; a coordinate constraint only prunes subtrees, which keeps it.
func (t *rtreeTree) walk(emit func(c rtreeTCell)) error {
	root := t.nodes[1]
	if root == nil {
		return errRtreeCorrupt
	}
	depth := int(binary.BigEndian.Uint16(root.data[0:2]))
	var visit func(n *rtreeTNode, level, guard int) error
	visit = func(n *rtreeTNode, level, guard int) error {
		if guard > 100 {
			return errRtreeCorrupt
		}
		nc := rtreeNCell(n)
		if t.cellOff(nc) > len(n.data) {
			return errRtreeCorrupt
		}
		for i := 0; i < nc; i++ {
			c := t.nodeGetCell(n, i)
			if level == 0 {
				emit(c)
				continue
			}
			child := t.nodes[c.rowid]
			if child == nil {
				return errRtreeCorrupt
			}
			if err := visit(child, level-1, guard+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(root, depth, 0)
}
