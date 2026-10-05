// This file reads SQLite WAL (write-ahead log) files for Import.
// It reads committed frames only, applying them to override pages from the base file.
// See sqlite.org/fileformat2.html#walformat for the WAL format specification.
package sqlite

import (
	"encoding/binary"
)

// walHeaderSize is the WAL header size in bytes.
const walHeaderSize = 32

// walFrameHeaderSize is the header size before each frame's page image.
const walFrameHeaderSize = 24

// walMagicMask selects the byte-order bit from the magic number.
const walMagicMask = 0xFFFFFFFE

// walMagicBase is the base WAL magic number.
const walMagicBase = 0x377f0682

// walMaxFileFormat is the supported WAL file format version.
const walMaxFileFormat = 3007000

// walSnapshot is the result of scanning a WAL file: committed frame offsets and db size.
type walSnapshot struct {
	size uint32 // db size in pages as of the WAL's last commit frame

	// frameOffset maps a page number to the byte offset, WITHIN THE WAL
	// FILE, of that page's most-recently-committed image (i.e. the start
	// of the page data immediately following that frame's 24-byte header).
	// A page number present in this map is served from the WAL; any other
	// page number <= size is served from the base file.
	frameOffset map[uint32]int64
}

// walBE32 reads a big-endian uint32, matching every multi-byte field in the
// WAL header and frame headers (see the file's doc comment: this is
// independent of the checksum byte-order bit).
func walBE32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// walChecksumStep advances the running SQLite WAL checksum (s0, s1) over
// data, which must be a whole number of 8-byte (two 32-bit word) groups --
// true for every range this package feeds it (a header's first 24 bytes, a
// frame's first 8 bytes, or a whole page whose size is itself a power of
// two >= 512). bigEndian selects how each 4-byte word is decoded from the
// raw bytes; see the byte-order bit discussion above.
func walChecksumStep(bigEndian bool, data []byte, s0, s1 uint32) (uint32, uint32) {
	get32 := binary.LittleEndian.Uint32
	if bigEndian {
		get32 = binary.BigEndian.Uint32
	}
	for i := 0; i+8 <= len(data); i += 8 {
		x0 := get32(data[i : i+4])
		x1 := get32(data[i+4 : i+8])
		s0 += x0 + s1
		s1 += x1 + s0
	}
	return s0, s1
}

// buildWALSnapshot walks a WAL's bytes and returns the snapshot as of its last
// COMMIT frame, or nil when there is none.
//
// A frame that validates but is never followed by a commit marker -- what an
// aborted transaction's spilled pages, or a writer killed mid-append, leave
// behind -- is not part of the snapshot: C SQLite's recovery advances mxFrame
// only on a commit record (wal.c:1516-1524, walIndexRecover), so it never sees
// those pages either.
//
// unreadable is a header that validates but names a different page size than
// the database it logs for.
func buildWALSnapshot(data []byte, dbPageSize uint32) (snap *walSnapshot, unreadable bool) {
	if len(data) < walHeaderSize {
		return nil, false
	}
	hdr := data[:walHeaderSize]
	magic := walBE32(hdr[0:4])
	if magic&walMagicMask != walMagicBase || walBE32(hdr[4:8]) != walMaxFileFormat {
		return nil, false
	}
	walPageSize := walBE32(hdr[8:12])
	if !isPowerOfTwo(walPageSize) || walPageSize < 512 || walPageSize > 65536 {
		return nil, false
	}
	bigEndian := magic&1 == 1
	s0, s1 := walChecksumStep(bigEndian, hdr[0:24], 0, 0)
	if s0 != walBE32(hdr[24:28]) || s1 != walBE32(hdr[28:32]) {
		return nil, false // header checksum fails: torn/corrupt
	}
	if walPageSize != dbPageSize {
		return nil, true
	}
	salt1, salt2 := walBE32(hdr[16:20]), walBE32(hdr[20:24])
	provisional := map[uint32]int64{}
	frameLen := int64(walFrameHeaderSize) + int64(dbPageSize)
	for offset := int64(walHeaderSize); offset+frameLen <= int64(len(data)); offset += frameLen {
		frame := data[offset : offset+frameLen]
		fh := frame[:walFrameHeaderSize]
		if walBE32(fh[8:12]) != salt1 || walBE32(fh[12:16]) != salt2 {
			break
		}
		cs0, cs1 := walChecksumStep(bigEndian, fh[0:8], s0, s1)
		cs0, cs1 = walChecksumStep(bigEndian, frame[walFrameHeaderSize:], cs0, cs1)
		if cs0 != walBE32(fh[16:20]) || cs1 != walBE32(fh[20:24]) {
			break
		}
		s0, s1 = cs0, cs1
		if pgno := walBE32(fh[0:4]); pgno != 0 {
			provisional[pgno] = offset + walFrameHeaderSize
		}
		if dbSizeAfter := walBE32(fh[4:8]); dbSizeAfter != 0 {
			committed := make(map[uint32]int64, len(provisional))
			for k, v := range provisional {
				committed[k] = v
			}
			snap = &walSnapshot{size: dbSizeAfter, frameOffset: committed}
		}
	}
	return snap, false
}
