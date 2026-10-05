package sqlite

import (
	"fmt"
	"os"
)

// pager reads the pages of one SQLite database file, through its -wal when it
// has one: what every reader in this package walks. It never writes, and
// recovers a hot journal before its first read, as C SQLite's pager does
// (recovery.go).
type pager struct {
	f      *os.File
	hdr    *Header
	nPages uint32

	walSnap       *walSnapshot
	walBytes      []byte
	walUnreadable bool

	pageCache      map[uint32][]byte
	pageCacheBytes int
}

// openPager opens the database at path.
func openPager(path string) (*pager, error) {
	if err := recoverHotJournal(path); err != nil {
		return nil, fmt.Errorf("sqlite: hot-journal recovery: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	p, err := newPager(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	// WAL overlay: if "<path>-wal" carries committed transactions, apply them.
	if walBytes, werr := os.ReadFile(path + "-wal"); werr == nil {
		snap, unreadable := buildWALSnapshot(walBytes, p.hdr.PageSize)
		p.walSnap, p.walUnreadable = snap, unreadable
		if snap != nil {
			p.walBytes = walBytes
			// Page 1 holds the header, so a WAL carrying a newer page 1 carries
			// a newer header: read it through the overlay, as SQLite does.
			if pg1, perr := p.page(1); perr == nil && len(pg1) >= HeaderSize {
				if h, herr := ParseHeader(pg1[:HeaderSize]); herr == nil && h.PageSize == p.hdr.PageSize {
					p.hdr = h
				}
			}
		}
	}
	return p, nil
}

func newPager(f *os.File) (*pager, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// A zero-length file is an empty database.
	if fi.Size() == 0 {
		return &pager{f: f, hdr: emptyDatabaseHeader()}, nil
	}
	head := make([]byte, HeaderSize)
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("sqlite: reading header: %w", err)
	}
	hdr, err := ParseHeader(head)
	if err != nil {
		return nil, err
	}
	if fi.Size()%int64(hdr.PageSize) != 0 {
		return nil, fmt.Errorf("sqlite: file size %d is not a multiple of page size %d", fi.Size(), hdr.PageSize)
	}
	return &pager{f: f, hdr: hdr, nPages: uint32(fi.Size() / int64(hdr.PageSize))}, nil
}

// Close releases the file.
func (p *pager) Close() error { return p.f.Close() }

// encoding is the database's text encoding.
func (p *pager) encoding() TextEncoding {
	if p.hdr.TextEncoding == UTF16LE || p.hdr.TextEncoding == UTF16BE {
		return p.hdr.TextEncoding
	}
	return UTF8
}

// PageCount is the number of pages in the database: the WAL's committed size
// when one is in effect (it may extend or truncate the base file), else the
// in-header size when valid, else what the file's length gives.
func (p *pager) PageCount() uint32 {
	if p.walSnap != nil {
		return p.walSnap.size
	}
	if p.hdr.SizeIsValid() {
		return p.hdr.DBSizePages
	}
	return p.nPages
}

// pageCacheMaxBytes bounds the page cache.
const pageCacheMaxBytes = 64 << 20

// page is page pgno (1-indexed), cached. The slice is the pager's and is
// read-only to callers.
func (p *pager) page(pgno uint32) ([]byte, error) {
	if b, ok := p.pageCache[pgno]; ok {
		return b, nil
	}
	b, err := p.readPage(pgno)
	if err != nil {
		return nil, err
	}
	if p.pageCacheBytes+len(b) <= pageCacheMaxBytes {
		if p.pageCache == nil {
			p.pageCache = make(map[uint32][]byte)
		}
		p.pageCache[pgno] = b
		p.pageCacheBytes += len(b)
	}
	return b, nil
}

func (p *pager) readPage(pgno uint32) ([]byte, error) {
	limit := p.nPages
	if p.walSnap != nil {
		limit = p.walSnap.size
	}
	if pgno == 0 || pgno > limit {
		return nil, fmt.Errorf("sqlite: page %d out of range [1,%d]", pgno, limit)
	}
	if p.walSnap != nil {
		if off, ok := p.walSnap.frameOffset[pgno]; ok {
			buf := make([]byte, p.hdr.PageSize)
			copy(buf, p.walBytes[off:off+int64(p.hdr.PageSize)])
			return buf, nil
		}
		if pgno > p.nPages {
			return nil, fmt.Errorf("sqlite: page %d present in WAL snapshot (size %d) but not in WAL frames or base file (base has %d pages)", pgno, limit, p.nPages)
		}
	}
	buf := make([]byte, p.hdr.PageSize)
	if _, err := p.f.ReadAt(buf, int64(pgno-1)*int64(p.hdr.PageSize)); err != nil {
		return nil, fmt.Errorf("sqlite: reading page %d: %w", pgno, err)
	}
	return buf, nil
}

// emptyDatabaseHeader is the header a zero-length file reports.
func emptyDatabaseHeader() *Header {
	return &Header{PageSize: 4096, WriteVersion: 1, ReadVersion: 1, TextEncoding: UTF8, SchemaFormat: 4}
}
