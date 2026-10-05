// Package sqlite implements the SQLite file format and b-tree storage layer.
// It is byte-compatible with C SQLite (verified by compat-harness).
package sqlite

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// HeaderSize is the fixed size of the database header at the start of page 1.
const HeaderSize = 100

// headerMagic is the 16-byte magic string, including its trailing NUL.
var headerMagic = []byte("SQLite format 3\x00")

// TextEncoding identifies the database's text encoding (header offset 56).
type TextEncoding uint32

const (
	UTF8    TextEncoding = 1
	UTF16LE TextEncoding = 2
	UTF16BE TextEncoding = 3
)

// Header is the parsed 100-byte database header. All multi-byte fields are
// big-endian on disk.
type Header struct {
	PageSize            uint32 // bytes per page: power of two in [512, 65536]
	WriteVersion        uint8  // 1=rollback journal, 2=WAL
	ReadVersion         uint8  // 1=rollback journal, 2=WAL
	ReservedPerPage     uint8  // unused bytes at the end of each page
	FileChangeCounter   uint32
	DBSizePages         uint32 // in-header database size, in pages (valid iff == FileChangeCounter's era, see IsSizeValid)
	FreelistTrunk       uint32 // page number of the first freelist trunk page, or 0
	FreelistPages       uint32 // total number of freelist pages
	SchemaCookie        uint32
	SchemaFormat        uint32 // 1..4
	DefaultCacheSize    uint32
	LargestRootPage     uint32 // for auto/incremental vacuum, else 0
	TextEncoding        TextEncoding
	UserVersion         uint32
	IncrementalVacuum   uint32
	ApplicationID       uint32
	VersionValidFor     uint32 // FileChangeCounter value when DBSizePages was last valid
	SQLiteVersionNumber uint32

}

// ParseHeader decodes and validates the database header from the first
// HeaderSize bytes of page 1.
func ParseHeader(b []byte) (*Header, error) {
	if len(b) < HeaderSize {
		return nil, fmt.Errorf("engine: header needs %d bytes, got %d", HeaderSize, len(b))
	}
	if !bytesEqual(b[0:16], headerMagic) {
		return nil, errors.New("engine: bad magic (not a SQLite 3 database)")
	}

	h := &Header{
		PageSize:            decodePageSize(binary.BigEndian.Uint16(b[16:18])),
		WriteVersion:        b[18],
		ReadVersion:         b[19],
		ReservedPerPage:     b[20],
		FileChangeCounter:   binary.BigEndian.Uint32(b[24:28]),
		DBSizePages:         binary.BigEndian.Uint32(b[28:32]),
		FreelistTrunk:       binary.BigEndian.Uint32(b[32:36]),
		FreelistPages:       binary.BigEndian.Uint32(b[36:40]),
		SchemaCookie:        binary.BigEndian.Uint32(b[40:44]),
		SchemaFormat:        binary.BigEndian.Uint32(b[44:48]),
		DefaultCacheSize:    binary.BigEndian.Uint32(b[48:52]),
		LargestRootPage:     binary.BigEndian.Uint32(b[52:56]),
		TextEncoding:        TextEncoding(binary.BigEndian.Uint32(b[56:60])),
		UserVersion:         binary.BigEndian.Uint32(b[60:64]),
		IncrementalVacuum:   binary.BigEndian.Uint32(b[64:68]),
		ApplicationID:       binary.BigEndian.Uint32(b[68:72]),
		VersionValidFor:     binary.BigEndian.Uint32(b[92:96]),
		SQLiteVersionNumber: binary.BigEndian.Uint32(b[96:100]),
		// Reserved bytes b[74:92] stay zero for C SQLite compatibility.
	}

	// Validation mirrors SQLite's own header checks.
	if !isPowerOfTwo(h.PageSize) || h.PageSize < 512 || h.PageSize > 65536 {
		return nil, fmt.Errorf("engine: invalid page size %d", h.PageSize)
	}
	if h.WriteVersion > 2 || h.ReadVersion > 2 {
		return nil, fmt.Errorf("engine: unknown file format versions read=%d write=%d", h.ReadVersion, h.WriteVersion)
	}
	if uint32(h.ReservedPerPage) >= h.PageSize {
		return nil, fmt.Errorf("engine: reserved bytes %d >= page size %d", h.ReservedPerPage, h.PageSize)
	}
	// The three payload-fraction bytes are fixed constants in the format.
	if b[21] != 64 || b[22] != 32 || b[23] != 32 {
		return nil, errors.New("engine: bad payload-fraction constants")
	}
	if h.SchemaFormat > 4 {
		return nil, fmt.Errorf("engine: unsupported schema format %d", h.SchemaFormat)
	}
	switch h.TextEncoding {
	case 0:
		// Default to UTF8 for uninitialized databases (btree.c:3535).
		h.TextEncoding = UTF8
	case UTF8, UTF16LE, UTF16BE:
	default:
		return nil, fmt.Errorf("engine: invalid text encoding %d", h.TextEncoding)
	}
	return h, nil
}

// UsablePageSize is the page size minus the reserved trailing bytes; this is the
// space b-tree cells may occupy.
func (h *Header) UsablePageSize() uint32 { return h.PageSize - uint32(h.ReservedPerPage) }

// SizeIsValid reports whether the in-header DBSizePages can be trusted: it is
// only authoritative when written by a modern SQLite that kept VersionValidFor
// in step with the change counter.
func (h *Header) SizeIsValid() bool {
	return h.DBSizePages != 0 && h.VersionValidFor == h.FileChangeCounter
}

// decodePageSize maps the on-disk u16 page-size field to bytes: the value 1
// encodes 65536 (which doesn't fit in 16 bits).
func decodePageSize(v uint16) uint32 {
	if v == 1 {
		return 65536
	}
	return uint32(v)
}

func isPowerOfTwo(n uint32) bool { return n != 0 && n&(n-1) == 0 }

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
