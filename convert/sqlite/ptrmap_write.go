// This file makes the materializer write POINTER-MAP pages, which is what an
// AUTO-VACUUM database is: an ordinary SQLite file plus one reverse-lookup page
// every usable/5 pages, recording for each page what kind of page it is and who
// points at it. C SQLite needs them to relocate a page (that is how it
// truncates without rewriting the world), and its PRAGMA integrity_check
// VALIDATES them, so a file this engine writes with the flag set but no map --
// or with a map that disagrees with the b-trees -- is a corrupt database there.
//
// Layout rules:
//
//	entries per ptrmap page      usable/5
//	first ptrmap page            page 2
//	stride                       entries+1
//	one entry                    1 type byte + 4-byte BIG-ENDIAN parent page
//	tail of a ptrmap page        left zero
//
// Entry types (1=root, 2=freepage, 3=overflow1, 4=overflow2, 5=btree).
// Entries past EOF are never read and written as zeros.
//
// C SQLite reserves the "pending byte" page (0x40000000 offset); this writer
// skips it as well since the database is in-memory.
package sqlite

import (
	"encoding/binary"
)

// The five pointer-map entry types, spelled as C SQLite's btreeInt.h names
// them. See this file's doc comment for what integrity_check requires of each.
const (
	ptrmapRootPage  = 1
	ptrmapFreePage  = 2
	ptrmapOverflow1 = 3
	ptrmapOverflow2 = 4
	ptrmapBtree     = 5
)

// ptrmapEntriesPerPage is how many pages one pointer-map page accounts for:
// usable/5, one 5-byte entry each.
func ptrmapEntriesPerPage(usable uint32) uint32 { return usable / 5 }

// ptrmapStride is the distance between consecutive pointer-map pages -- the
// entries one holds, plus the map page itself.
func ptrmapStride(usable uint32) uint32 { return ptrmapEntriesPerPage(usable) + 1 }

// isPtrmapPage reports whether pgno is a pointer-map page rather than a page
// holding data. Page 1 never is; page 2 always is.
func isPtrmapPage(pgno, usable uint32) bool {
	if pgno < 2 {
		return false
	}
	return (pgno-2)%ptrmapStride(usable) == 0
}



// tableLeafCellOverflow returns the first page of the overflow chain a TABLE
// b-tree leaf cell spills into, or 0 if its payload fits on the page. The cell
// is "varint payload length, varint rowid, local payload, [4-byte overflow]",
// so the trailing pointer is present exactly when the local portion is shorter
// than the whole payload.
func tableLeafCellOverflow(b []byte, usable uint32) uint32 {
	payloadLen, _ := getVarint(b)
	if localPayloadSize(payloadLen, usable) == payloadLen {
		return 0
	}
	return binary.BigEndian.Uint32(b[len(b)-4:])
}

// idxCellOverflow is the same for an INDEX b-tree cell, whose payload rules
// differ (localPayloadSizeIndex) and which carries payload on interior pages
// too -- there behind the 4-byte child pointer.
func idxCellOverflow(b []byte, usable uint32, interior bool) uint32 {
	if interior {
		b = b[4:]
	}
	payloadLen, _ := getVarint(b)
	if localPayloadSizeIndex(payloadLen, usable) == payloadLen {
		return 0
	}
	return binary.BigEndian.Uint32(b[len(b)-4:])
}
