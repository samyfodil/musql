package engine

// The on-disk format of an r-tree's %_node shadow table, ported from rtree.c
// to keep databases readable by SQLite. Node blobs are big-endian: 2-byte depth
// (root only), 2-byte entry count, then entries (8-byte integer + coordinates).
// Coordinates are 4-byte IEEE-754 floats or signed integers per module type.

import "math"

// rtreeMaxCells is RTREE_MAXCELLS (rtree.c), the cap on entries per node that
// bounds iNodeSize regardless of how large a page is.
const rtreeMaxCells = 51

// rtreeNodeHeader is the 2-byte depth plus the 2-byte entry count every node
// blob begins with.
const rtreeNodeHeader = 4

// rtreeBytesPerCell is one entry's width: the 8-byte integer plus 2*nDim
// 4-byte coordinates. nDim2 is the COORDINATE count (2 per dimension), which
// is what rtree.c itself carries as nDim2.
func rtreeBytesPerCell(nDim2 int) int { return 8 + nDim2*4 }

// rtreeNodeSize ports getNodeSize's xCreate arm (rtree.c:3575-3583):
//
//	pRtree->iNodeSize = iPageSize-64;
//	if( (4+pRtree->nBytesPerCell*RTREE_MAXCELLS)<pRtree->iNodeSize ){
//	  pRtree->iNodeSize = 4+pRtree->nBytesPerCell*RTREE_MAXCELLS;
//	}
//
// The size is not merely an internal choice: xConnect reads it back as
// length(data) of node 1 and rejects anything under 512-64 as corrupt
// (rtree.c:3588-3597), so it is part of what makes the file readable.
// rtreeMinNodeSize is the smallest node size xConnect accepts: getNodeSize
// refuses anything under 512-64 (rtree.c:3595).
const rtreeMinNodeSize = 512 - 64

func rtreeNodeSize(pageSize, nDim2 int) int {
	n := pageSize - 64
	if capped := rtreeNodeHeader + rtreeBytesPerCell(nDim2)*rtreeMaxCells; capped < n {
		n = capped
	}
	return n
}

// rtreeCoordBits renders one coordinate as IEEE-754 or signed integer.
func rtreeCoordBits(v float64, i32 bool) uint32 {
	if i32 {
		return uint32(int32(v))
	}
	return math.Float32bits(float32(v))
}

// rtreeCoordValue is rtreeCoordBits' inverse.
func rtreeCoordValue(bits uint32, i32 bool) float64 {
	if i32 {
		return float64(int32(bits))
	}
	return float64(math.Float32frombits(bits))
}
